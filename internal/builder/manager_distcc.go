package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/slchris/portage-engine/internal/distcc"
)

func (m *Manager) distCCPackageAllowed(atom string) bool {
	if !m.config.DistCCAlphaEnabled {
		return false
	}
	for _, allowed := range m.config.DistCCPackageAllowlist {
		if strings.TrimSpace(allowed) == atom {
			return true
		}
	}
	return false
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
	if !m.distCCPackageAllowed(req.PackageName) {
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
	m.appendJobLog(jobID, fmt.Sprintf(
		"[distcc] reserved %d compile slot(s) pool=%s fence=%d queue=%s",
		lease.Slots, lease.PoolID, lease.Fence, lease.QueueTime,
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
			case <-ticker.C:
				heartbeatCtx, heartbeatCancel := context.WithTimeout(context.Background(), 5*time.Second)
				heartbeatErr := m.compileQueue.HeartbeatCompileLease(
					heartbeatCtx, *lease, leaseDuration, workerFreshness,
				)
				heartbeatCancel()
				if heartbeatErr != nil {
					m.appendJobLog(jobID, "[distcc] lease heartbeat rejected; network authorization will expire")
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

func (m *Manager) checkCompileOutput(lease *distcc.Lease) error {
	if lease == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.compileQueue.CheckCompileLease(
		ctx, *lease,
		time.Duration(m.config.DistCCWorkerFreshnessSeconds)*time.Second,
	)
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
