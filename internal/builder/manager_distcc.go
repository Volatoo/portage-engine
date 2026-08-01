package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/slchris/portage-engine/internal/distcc"
)

// distCCEligibleAtoms reduces the request's own package set to the reviewed
// category/name atoms this build may compile remotely. Eligibility is decided
// here and shipped on the lease: the control plane reserves the slot and runs
// the heartbeat while the builder writes the package.env mapping, so if each
// side derived the set from a different value (request atom vs. bundle atom,
// versioned vs. package-level form) a reserved slot could reach a builder that
// finds nothing eligible and, under blocked policy, fails before emerge runs.
func (m *Manager) distCCEligibleAtoms(req *BuildRequest) []string {
	if !m.config.DistCCAlphaEnabled || req == nil {
		return nil
	}
	allowed := make(map[string]struct{}, len(m.config.DistCCPackageAllowlist))
	for _, atom := range m.config.DistCCPackageAllowlist {
		if reduced, ok := distcc.NormalizeAtom(atom); ok {
			allowed[reduced] = struct{}{}
		}
	}
	// The builder only wires distcc for a config-bundle build, so the bundle's
	// own atoms are authoritative whenever it carries one.
	candidates := []string{req.PackageName}
	if req.ConfigBundle != nil && req.ConfigBundle.Packages != nil &&
		len(req.ConfigBundle.Packages.Packages) > 0 {
		candidates = candidates[:0]
		for _, pkg := range req.ConfigBundle.Packages.Packages {
			candidates = append(candidates, pkg.Atom)
		}
	}
	eligible := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		reduced, ok := distcc.NormalizeAtom(candidate)
		if !ok {
			continue
		}
		if _, reviewed := allowed[reduced]; !reviewed {
			continue
		}
		if _, duplicate := seen[reduced]; duplicate {
			continue
		}
		seen[reduced] = struct{}{}
		eligible = append(eligible, reduced)
	}
	sort.Strings(eligible)
	return eligible
}

func (m *Manager) distCCPool(req *BuildRequest) (distcc.Pool, error) {
	if req == nil || req.ProjectID == "" {
		return distcc.Pool{}, fmt.Errorf("distcc requires a durable project-bound request")
	}
	arch := strings.TrimSpace(req.Arch)
	zone := strings.TrimSpace(m.config.DistCCNetworkZone)
	if req.ResolvedContext != nil {
		arch = strings.TrimSpace(req.ResolvedContext.Arch)
		if resolvedZone := strings.TrimSpace(req.ResolvedContext.ExecutionZone); resolvedZone != "" {
			zone = resolvedZone
		}
	}
	return distcc.Pool{
		Architecture: arch, CHOST: m.config.DistCCCHOST,
		CompilerDigest:           m.config.DistCCCompilerDigest,
		ToolchainImageGeneration: m.config.DistCCToolchainImageGeneration,
		CPUFeatures:              append([]string(nil), m.config.DistCCCPUFeatures...),
		NetworkZone:              zone,
		// Alpha defaults to project isolation. A future reviewed trust-domain
		// mapping may group projects without changing fairness/quotas.
		ProjectTrustDomain: req.ProjectID,
	}.Normalize()
}

// beginCompileLease returns a live lease plus a release closure. The closure
// stops heartbeat before returning capacity. If local fallback is configured,
// an unavailable pool returns a nil lease; blocked policy stops the build
// before any staging output exists.
func (m *Manager) beginCompileLease(
	jobID, builderID, builderNetworkIdentity string,
	req *BuildRequest,
) (*distcc.Lease, func(string), error) {
	noop := func(string) {}
	if req == nil {
		return nil, noop, fmt.Errorf("distcc build request is required")
	}
	eligibleAtoms := m.distCCEligibleAtoms(req)
	if len(eligibleAtoms) == 0 {
		return nil, noop, nil
	}
	if m.compileQueue == nil {
		if m.config.DistCCFallbackPolicy == distcc.FallbackLocal {
			m.appendJobLog(jobID, "[distcc] scheduler unavailable; controlled local fallback")
			return nil, noop, nil
		}
		return nil, noop, fmt.Errorf("distcc pool is required but scheduler is unavailable")
	}
	if builderID == "" || builderNetworkIdentity == "" {
		if m.config.DistCCFallbackPolicy == distcc.FallbackLocal {
			m.appendJobLog(jobID, "[distcc] builder has no isolated-network identity; controlled local fallback")
			return nil, noop, nil
		}
		return nil, noop, fmt.Errorf("distcc pool is required but builder network identity is unavailable")
	}
	status, err := m.jobStatusSnapshot(jobID)
	if err != nil {
		return nil, noop, err
	}
	if status.ProjectID == "" || status.ProjectID != req.ProjectID ||
		status.AttemptID == "" || status.FenceToken <= 0 {
		return nil, noop, fmt.Errorf("distcc request does not match the claimed project/attempt boundary")
	}
	pool, err := m.distCCPool(req)
	if err != nil {
		// A malformed pool dimension is an operator configuration fault, not a
		// fence anomaly, so it belongs with the other unavailable-pool cases:
		// without this branch a single bad compiler digest fails every
		// allowlisted package 100% of the time even under local fallback.
		if m.config.DistCCFallbackPolicy == distcc.FallbackLocal {
			m.appendJobLog(jobID, "[distcc] exact pool cannot be derived; controlled local fallback")
			return nil, noop, nil
		}
		return nil, noop, err
	}
	leaseDuration := time.Duration(m.config.DistCCLeaseSeconds) * time.Second
	workerFreshness := time.Duration(m.config.DistCCWorkerFreshnessSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	lease, err := m.compileQueue.ReserveCompileSlots(ctx, distcc.ReservationRequest{
		ProjectID: status.ProjectID, JobID: status.JobID,
		AttemptID: status.AttemptID, AttemptFence: status.FenceToken,
		BuilderID:              builderID,
		BuilderNetworkIdentity: builderNetworkIdentity,
		Slots:                  m.config.DistCCSlotsPerJob, LeaseDuration: leaseDuration,
		WorkerFreshness: workerFreshness, Pool: pool,
		FallbackPolicy: m.config.DistCCFallbackPolicy,
	})
	cancel()
	if err != nil {
		if m.config.DistCCFallbackPolicy == distcc.FallbackLocal {
			m.appendJobLog(jobID, "[distcc] reservation failed; controlled local fallback")
			return nil, noop, nil
		}
		return nil, noop, err
	}
	if lease == nil {
		if m.config.DistCCFallbackPolicy == distcc.FallbackLocal {
			m.appendJobLog(jobID, "[distcc] exact compile pool has no free slot; controlled local fallback")
			return nil, noop, nil
		}
		return nil, noop, fmt.Errorf("distcc exact compile pool has no free slot")
	}
	if err := validateCompileLeaseReservation(
		lease, status, builderID, builderNetworkIdentity, pool,
		m.config.DistCCSlotsPerJob, m.config.DistCCFallbackPolicy,
	); err != nil {
		_ = m.compileQueue.ReleaseCompileLease(context.Background(), *lease, "policy")
		if m.config.DistCCFallbackPolicy == distcc.FallbackLocal {
			m.appendJobLog(jobID, "[distcc] scheduler lease contract rejected; controlled local fallback")
			return nil, noop, nil
		}
		return nil, noop, err
	}
	if err := validateCompileEndpointNetworks(
		lease.WorkerEndpoint, m.config.DistCCIsolatedNetworkCIDRs,
	); err != nil {
		_ = m.compileQueue.ReleaseCompileLease(context.Background(), *lease, "policy")
		if m.config.DistCCFallbackPolicy == distcc.FallbackLocal {
			m.appendJobLog(jobID, "[distcc] worker endpoint rejected; controlled local fallback")
			return nil, noop, nil
		}
		return nil, noop, err
	}
	// The scheduler contract is validated on the shape it issued; the eligible
	// atom set is the control plane's own decision and is attached afterwards.
	lease.EligibleAtoms = eligibleAtoms
	m.appendJobLog(jobID, fmt.Sprintf(
		"[distcc] reserved %d compile slot(s) pool=%s fence=%d queue=%s atoms=%s",
		lease.Slots, lease.PoolID, lease.Fence, lease.QueueTime,
		strings.Join(eligibleAtoms, ","),
	))
	observationCtx, observationCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := m.compileQueue.RecordCompileObservation(
		observationCtx, *lease, distcc.Observation{
			Outcome: distcc.OutcomeHit, Count: 1,
			QueueMillis: lease.QueueTime.Milliseconds(),
		},
	); err != nil {
		m.appendJobLog(jobID, "[distcc] reservation observation rejected: "+err.Error())
	}
	observationCancel()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		interval := leaseDuration / 3
		if interval < time.Second {
			interval = time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-m.stopCh:
				return
			case <-ticker.C:
				if !m.renewCompileLease(
					*lease, leaseDuration, workerFreshness, stop,
				) {
					m.appendJobLog(jobID, "[distcc] lease heartbeat rejected for a full lease period; network authorization will expire")
					return
				}
			}
		}
	}()
	var once sync.Once
	finish := func(reason string) {
		once.Do(func() {
			close(stop)
			wg.Wait()
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer releaseCancel()
			if err := m.compileQueue.ReleaseCompileLease(releaseCtx, *lease, reason); err != nil {
				m.appendJobLog(jobID, "[distcc] lease release rejected: "+err.Error())
			}
		})
	}
	return lease, finish, nil
}

// renewCompileLease retries a rejected renewal with backoff for a full lease
// period before reporting the lease lost. A single rejection is normally a
// transient ledger or compile-worker freshness blip, while the compile it
// authorizes routinely runs for hours: giving up on the first failure fenced
// otherwise successful multi-hour builds long after the blip had healed.
func (m *Manager) renewCompileLease(
	lease distcc.Lease,
	leaseDuration, workerFreshness time.Duration,
	stop <-chan struct{},
) bool {
	deadline := time.Now().Add(leaseDuration)
	backoff := time.Second
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := m.compileQueue.HeartbeatCompileLease(
			ctx, lease, leaseDuration, workerFreshness,
		)
		cancel()
		if err == nil {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-stop:
			return false
		case <-m.stopCh:
			return false
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func validateCompileLeaseReservation(
	lease *distcc.Lease,
	status *BuildStatus,
	builderID, builderNetworkIdentity string,
	pool distcc.Pool,
	slots int,
	fallbackPolicy string,
) error {
	if lease == nil || status == nil {
		return fmt.Errorf("distcc scheduler returned an incomplete lease")
	}
	if err := lease.Validate(time.Now()); err != nil {
		return err
	}
	expectedPoolID, err := pool.ID()
	if err != nil {
		return err
	}
	if lease.PoolID != expectedPoolID ||
		lease.Pool.ProjectTrustDomain != status.ProjectID ||
		lease.AttemptID != status.AttemptID ||
		lease.AttemptFence != status.FenceToken ||
		lease.BuilderID != builderID ||
		lease.BuilderNetworkIdentity != builderNetworkIdentity ||
		lease.Slots != slots || lease.FallbackPolicy != fallbackPolicy {
		return fmt.Errorf("distcc scheduler lease does not match the claimed project/attempt/builder contract")
	}
	return nil
}

func validateCompileEndpointNetworks(endpoint string, cidrs []string) error {
	// NewBuilderPolicy refuses to enable distcc without a reviewed isolated
	// CIDR. The control plane must reach the same verdict instead of reading an
	// empty operator list as "every network is isolated".
	if len(cidrs) == 0 {
		return fmt.Errorf("distcc requires at least one reviewed isolated build network CIDR")
	}
	networks := make([]*net.IPNet, 0, len(cidrs))
	for _, raw := range cidrs {
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			return fmt.Errorf("invalid isolated distcc CIDR %q", raw)
		}
		networks = append(networks, network)
	}
	return distcc.ValidateEndpoint(endpoint, networks)
}

// checkCompileOutput fences build output on the live compile lease and returns
// the release reason to attribute. A lease lost mid-build is a capacity and
// authorization fact, not evidence that the produced bytes are wrong: under
// the reviewed local-fallback policy the phase-aware wrapper already
// recompiled locally once the workers stopped answering, so discarding a
// finished two-hour build would destroy correct artifacts. Blocked policy has
// no local path and still refuses the output, and a lease loss that turns out
// to be fence supersession is refused under every policy.
func (m *Manager) checkCompileOutput(jobID string, lease *distcc.Lease) (string, error) {
	if lease == nil {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := m.compileQueue.CheckCompileLease(
		ctx, *lease,
		time.Duration(m.config.DistCCWorkerFreshnessSeconds)*time.Second,
	)
	cancel()
	if err == nil {
		return "", nil
	}
	if lease.FallbackPolicy != distcc.FallbackLocal {
		return "lease-fenced", err
	}
	// The authorization read answers "not authorized" without saying why, so
	// the attempt fence is interrogated separately before the policy is
	// allowed to absorb the loss. A live claim means only the compile
	// reservation lapsed while this attempt kept the build, which is the case
	// local fallback was reviewed for. A claim that is gone means another
	// attempt owns the job: the produced bytes are unfenced, no policy covers
	// that, and continuing would make the output fence advisory.
	if claimErr := m.attemptClaimIsLive(jobID); claimErr != nil {
		return "lease-fenced", fmt.Errorf(
			"compile lease loss coincides with a superseded attempt fence (%w): %w",
			claimErr, err,
		)
	}
	m.appendJobLog(jobID,
		"[distcc] compile lease was lost during the build; controlled local fallback keeps the produced artifacts")
	observationCtx, observationCancel := context.WithTimeout(context.Background(), 5*time.Second)
	observationErr := m.compileQueue.RecordCompileObservation(
		observationCtx, *lease, distcc.Observation{
			Outcome: distcc.OutcomeFallback, FailureReason: "lease-expired", Count: 1,
		},
	)
	observationCancel()
	if observationErr != nil {
		m.appendJobLog(jobID, "[distcc] lease-expired observation rejected: "+observationErr.Error())
	}
	return "lease-expired", nil
}

// attemptClaimIsLive reports whether this replica still owns the job's attempt.
// It is deliberately read-only: requireActiveClaim renews the lease first,
// which would answer "yes" to the very question being asked here.
func (m *Manager) attemptClaimIsLive(jobID string) error {
	if m.scheduler == nil {
		return nil
	}
	status, err := m.jobStatusSnapshot(jobID)
	if err != nil {
		return err
	}
	if status.AttemptID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.scheduler.CheckClaim(ctx, status)
}

// recordCompileReport persists the bounded builder report through the same
// active lease/attempt fence used for network authorization. Telemetry failure
// is visible in the job log but never changes artifact correctness.
func (m *Manager) recordCompileReport(jobID string, lease *distcc.Lease, metadata map[string]interface{}) {
	if lease == nil || metadata == nil {
		return
	}
	value, ok := metadata[distcc.MetadataReportKey]
	if !ok {
		m.appendJobLog(jobID, "[distcc] builder returned no compiler observation report")
		return
	}
	payload, err := json.Marshal(value)
	if err != nil {
		m.appendJobLog(jobID, "[distcc] builder observation report was malformed")
		return
	}
	var report distcc.Report
	if err := json.Unmarshal(payload, &report); err != nil || report.Validate() != nil {
		m.appendJobLog(jobID, "[distcc] builder observation report was rejected")
		return
	}
	for _, observation := range report.Observations {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := m.compileQueue.RecordCompileObservation(ctx, *lease, observation)
		cancel()
		if err != nil {
			m.appendJobLog(jobID, "[distcc] compiler observation rejected: "+err.Error())
			return
		}
	}
}
