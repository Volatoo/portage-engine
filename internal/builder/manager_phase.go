package builder

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slchris/portage-engine/internal/iac"
	"github.com/slchris/portage-engine/internal/workergateway"
)

const (
	phaseWorkLease    = 45 * time.Second
	phaseAttemptLease = 2 * time.Minute
)

// durablePhaseAdmissionLoop is the only shadow→active cutover path. It claims
// a new job attempt, activates its complete phase plan at the claimed boundary,
// and never invokes the legacy whole-pipeline executor.
func (m *Manager) durablePhaseAdmissionLoop() {
	owner := m.schedulerID + "/phase-admission"
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastError := ""
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		claim, err := m.scheduler.ClaimNext(
			ctx, owner, phaseAttemptLease,
			workerKindCapabilities(m.executorCaps, "admission"),
		)
		cancel()
		if err == nil && claim != nil {
			lastError = ""
			m.jobsMu.Lock()
			m.jobs[claim.Status.JobID] = claim.Status
			m.jobsMu.Unlock()
			m.appendRuntimeBudgetReservationLog(
				claim.Status.JobID, claim.Request,
			)
			ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			err = m.phaseQueue.ActivatePhasePlan(ctx, claim.Status)
			cancel()
			if err != nil {
				m.appendJobLog(claim.Status.JobID,
					"[phase] active plan cutover rejected: "+err.Error())
				m.updateStatus(claim.Status.JobID, "failed", "",
					"activate durable phase plan: "+err.Error())
			} else {
				m.appendJobLog(claim.Status.JobID,
					"[phase] durable provision/build/verify/publish plan activated")
			}
			continue
		}
		if err != nil && err.Error() != lastError {
			fmt.Printf("Warning: durable phase admission failed: %v\n", err)
			lastError = err.Error()
		}
		select {
		case <-m.stopCh:
			return
		case <-m.workQueue:
		case <-ticker.C:
		}
	}
}

func (m *Manager) durablePhaseWorker(slot int) {
	owner := fmt.Sprintf("%s/phase-slot-%d", m.schedulerID, slot)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastError := ""
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		claim, err := m.phaseQueue.ClaimPhaseWork(
			ctx, owner, phaseWorkLease,
			workerKindCapabilities(m.executorCaps, "phase"),
		)
		cancel()
		if err == nil && claim != nil {
			lastError = ""
			m.executePhaseClaim(claim)
			continue
		}
		if err != nil && err.Error() != lastError {
			fmt.Printf("Warning: durable phase worker %d claim failed: %v\n",
				slot, err)
			lastError = err.Error()
		}
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) executePhaseClaim(claim *PhaseWorkClaim) {
	if claim == nil || claim.Status == nil || claim.Request == nil {
		return
	}
	m.jobsMu.Lock()
	m.jobs[claim.JobID] = claim.Status
	m.jobsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	phaseContext, err := m.phaseQueue.LoadPhaseExecutionContext(ctx, claim)
	cancel()
	if err != nil {
		m.failActivePhase(claim, fmt.Errorf("load phase context: %w", err))
		return
	}
	m.applyPhaseContext(claim.JobID, phaseContext)

	renewStop := make(chan struct{})
	go m.renewPhaseLoop(claim, renewStop)
	err = m.executePhase(claim, phaseContext)
	close(renewStop)
	if err != nil {
		m.failActivePhase(claim, err)
		return
	}

	if claim.Phase == "publish" {
		previous, snapshotErr := m.jobStatusSnapshot(claim.JobID)
		if snapshotErr != nil {
			m.failActivePhase(claim, snapshotErr)
			return
		}
		completed := *previous
		completed.Status, completed.Error = "completed", ""
		completed.UpdatedAt = time.Now().UTC()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = m.phaseQueue.FinalizePhaseWork(ctx, claim, previous, &completed)
		cancel()
		if err != nil {
			m.failActivePhase(claim, fmt.Errorf("finalize publish phase: %w", err))
			return
		}
		m.jobsMu.Lock()
		m.jobs[claim.JobID] = &completed
		m.jobsMu.Unlock()
		// Active phase mode never reaches completion through updateStatus, so
		// this is the only place the success counter can be written in the
		// topology the public boundary requires. It runs after the atomic
		// finalize so a rejected commit never counts a success, and the
		// previous→current guard keeps a legacy updateStatus that later sees
		// the same job from counting it twice.
		recordCompletedBuildMetric(previous.Status, completed.Status)
		m.emitStatus(&completed)
		m.appendJobLog(claim.JobID,
			"[phase] publish and job completion committed atomically")
		m.removeArtifactQuarantineFiles(claim.JobID)
		return
	}

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	err = m.phaseQueue.CompletePhaseWork(ctx, claim)
	cancel()
	if err != nil {
		m.failActivePhase(claim, fmt.Errorf("complete %s phase: %w", claim.Phase, err))
		return
	}
	m.appendJobLog(claim.JobID,
		fmt.Sprintf("[phase] %s hand-off committed", claim.Phase))
}

func (m *Manager) renewPhaseLoop(claim *PhaseWorkClaim, stop <-chan struct{}) {
	ticker := time.NewTicker(12 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := m.phaseQueue.RenewPhaseWork(ctx, claim, phaseWorkLease)
			cancel()
			if err != nil {
				m.appendJobLog(claim.JobID,
					"[phase] lease renewal rejected: "+err.Error())
				return
			}
		}
	}
}

func (m *Manager) refreshPhaseClaim(claim *PhaseWorkClaim) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.phaseQueue.RenewPhaseWork(ctx, claim, phaseWorkLease)
}

func (m *Manager) executePhase(
	claim *PhaseWorkClaim,
	value *PhaseExecutionContext,
) error {
	switch claim.Phase {
	case "provision":
		return m.executeProvisionPhase(claim, value)
	case "build":
		return m.executeBuildPhase(claim, value)
	case "verify":
		return m.executeVerifyPhase(claim, value)
	case "publish":
		return m.executePublishPhase(claim, value)
	default:
		return fmt.Errorf("unsupported active phase %q", claim.Phase)
	}
}

func (m *Manager) failActivePhase(claim *PhaseWorkClaim, cause error) {
	if claim == nil || cause == nil {
		return
	}
	detail := redactJobLogLine(cause.Error())
	m.appendJobLog(claim.JobID,
		fmt.Sprintf("[%s] active phase failed: %s", claim.Phase, detail))
	if claim.Phase == "build" || claim.Phase == "verify" ||
		claim.Phase == "publish" {
		if err := m.revokeArtifactCapability(claim.JobID); err != nil {
			m.appendJobLog(claim.JobID,
				"[phase] warning: revoke verification capability: "+err.Error())
		}
	}
	previous, err := m.jobStatusSnapshot(claim.JobID)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = m.phaseQueue.FailPhaseWorkAndJob(ctx, claim, previous, detail)
	cancel()
	if err != nil {
		m.appendJobLog(claim.JobID,
			"[phase] atomic failure commit rejected: "+err.Error())
		return
	}
	failed := *previous
	failed.Status, failed.Error, failed.FailedStage = "failed", detail, claim.Phase
	failed.UpdatedAt = time.Now().UTC()
	m.jobsMu.Lock()
	m.jobs[claim.JobID] = &failed
	m.jobsMu.Unlock()
	// The mirror of the publish path's success count: active mode terminates a
	// job here and never through updateStatus, and it runs after the atomic
	// commit so a rejected failure commit counts nothing.
	recordFailedBuildMetric(previous.Status, failed.Status)
	m.emitStatus(&failed)
	m.removeArtifactQuarantineFiles(claim.JobID)
}

func (m *Manager) executeProvisionPhase(
	claim *PhaseWorkClaim,
	value *PhaseExecutionContext,
) error {
	if value.Instance != nil && value.Instance.Status == "running" &&
		value.WorkerID != "" {
		value.Instance.BuilderEndpoint = "pull://" + value.WorkerID
		identity := phaseIdentity(claim, value.WorkerID)
		if err := m.workerBroker.Register(identity); err != nil {
			return fmt.Errorf("restore worker session: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		err := m.workerBroker.WaitConnected(ctx, identity.WorkerID)
		cancel()
		if err != nil {
			return fmt.Errorf("restore provisioned worker: %w", err)
		}
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		err = m.phaseQueue.SavePhaseExecutionContext(ctx, claim, value)
		cancel()
		return err
	}

	provision, err := m.buildProvisionRequest(claim.Request)
	if err != nil {
		return err
	}
	provision.InstanceID = provision.Provider + "-" + claim.AttemptID
	workerID := value.WorkerID
	if workerID == "" {
		workerID = stablePhaseUUID(claim.AttemptID, "worker")
	}
	identity := phaseIdentity(claim, workerID)
	if _, err := m.prepareWorkerPullIdentity(
		claim.JobID, provision, identity,
	); err != nil {
		return fmt.Errorf("prepare outbound worker: %w", err)
	}
	value.WorkerID = workerID

	var deployingOnce sync.Once
	provision.LogSink = func(line string) {
		m.appendJobLog(claim.JobID, line)
		if strings.HasPrefix(line, "[deploy]") {
			deployingOnce.Do(func() {
				m.updateStatus(claim.JobID, "deploying", "", "")
			})
		}
	}
	var infraFence atomic.Pointer[BuildStatus]
	provision.Lifecycle = func(
		instance *iac.Instance,
		state, failure string,
		deletedAt *time.Time,
	) error {
		instance.Status = state
		value.Instance = phaseInstanceFromIAC(instance)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		saveErr := m.phaseQueue.SavePhaseExecutionContext(ctx, claim, value)
		cancel()
		if saveErr != nil {
			return saveErr
		}
		if captured := infraFence.Load(); captured != nil {
			return m.recordInfraWithStatus(
				captured, instance, state, failure, deletedAt,
			)
		}
		return m.recordInfra(claim.JobID, instance, state, failure, deletedAt)
	}

	if !m.updateStatus(claim.JobID, "provisioning", "", "") {
		return fmt.Errorf("durable provision transition was rejected")
	}
	m.appendJobLog(claim.JobID, fmt.Sprintf(
		"[provision] phase=%s provisioning deterministic single-use instance %s",
		claim.ID, provision.InstanceID))
	instance, err := m.iacMgr.Provision(provision)
	if err != nil {
		return fmt.Errorf("provisioning failed: %w", err)
	}
	instance.BuilderEndpoint = "pull://" + workerID
	instance.Status = "running"
	m.iacMgr.SetInstanceActiveTasks(instance.ID, 1)
	if err := m.recordInfra(claim.JobID, instance, "running", "", nil); err != nil {
		return fmt.Errorf("persist infrastructure ownership: %w", err)
	}
	cleanupFence, err := m.jobStatusSnapshot(claim.JobID)
	if err != nil {
		return err
	}
	infraFence.Store(cleanupFence)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	err = m.workerBroker.WaitConnected(ctx, workerID)
	cancel()
	if err != nil {
		return fmt.Errorf("outbound worker did not connect: %w", err)
	}
	if err := m.closePhasePVEInbound(
		claim.JobID, provision, instance, cleanupFence,
	); err != nil {
		return err
	}
	value.Instance = phaseInstanceFromIAC(instance)
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	err = m.phaseQueue.SavePhaseExecutionContext(ctx, claim, value)
	cancel()
	return err
}

func (m *Manager) closePhasePVEInbound(
	jobID string,
	provision *iac.ProvisionRequest,
	instance *iac.Instance,
	status *BuildStatus,
) error {
	if instance.Provider != "pve" ||
		instance.Metadata["pe_egress_enforced"] != "true" {
		return nil
	}
	auth := iac.PVEAuth{Insecure: provision.Spec["insecure"] == "true"}
	if provision.Credentials != nil {
		auth.TokenID = provision.Credentials.PVETokenID
		auth.TokenSecret = provision.Credentials.PVETokenSecret
		auth.Username = provision.Credentials.PVEUsername
		auth.Password = provision.Credentials.PVEPassword
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err := iac.ClosePVEVMInbound(
		ctx, provision.Spec["endpoint"], auth,
		instance.Metadata["node"], instance.Metadata["vmid"],
	)
	cancel()
	if err != nil {
		return fmt.Errorf("close transient PVE inbound access: %w", err)
	}
	instance.Metadata["pe_inbound_closed"] = "true"
	if err := m.recordInfraWithStatus(
		status, instance, "running", "", nil,
	); err != nil {
		return fmt.Errorf("persist closed PVE inbound boundary: %w", err)
	}
	m.appendJobLog(jobID,
		"[policy] transient SSH bootstrap window closed; VM policy_in=DROP readback verified")
	return nil
}

func (m *Manager) executeBuildPhase(
	claim *PhaseWorkClaim,
	value *PhaseExecutionContext,
) error {
	instance, identity, err := phaseRuntime(claim, value)
	if err != nil {
		return err
	}
	if err := m.workerBroker.Register(identity); err != nil {
		return fmt.Errorf("restore worker session: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	err = m.workerBroker.WaitConnected(ctx, identity.WorkerID)
	cancel()
	if err != nil {
		return fmt.Errorf("wait for outbound worker: %w", err)
	}
	if !m.updateStatus(claim.JobID, "building", instance.ID, "") {
		return fmt.Errorf("durable build transition was rejected")
	}
	m.appendJobLog(claim.JobID,
		"[build] dispatching stable phase command; replay reuses its result")
	if err := m.runBuildOnPullWorkerStableFenced(
		claim.JobID, instance, claim.Request, identity, claim.ID,
		func() error { return m.refreshPhaseClaim(claim) },
	); err != nil {
		return err
	}
	m.capturePhaseContext(claim.JobID, value)
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	err = m.phaseQueue.SavePhaseExecutionContext(ctx, claim, value)
	cancel()
	return err
}

func (m *Manager) executeVerifyPhase(
	claim *PhaseWorkClaim,
	value *PhaseExecutionContext,
) error {
	instance, identity, err := phaseRuntime(claim, value)
	if err != nil {
		return err
	}
	if err := m.workerBroker.Register(identity); err != nil {
		return fmt.Errorf("restore worker session: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	err = m.workerBroker.WaitConnected(ctx, identity.WorkerID)
	cancel()
	if err != nil {
		return fmt.Errorf("wait for outbound worker: %w", err)
	}
	verifyBinhost, err := m.prepareVerificationBinhost(
		claim.JobID, claim.Request.Arch,
	)
	if err != nil {
		return fmt.Errorf("prepare unsigned verification binhost: %w", err)
	}
	if err := m.verifyOnBuilderStableFenced(
		claim.JobID, instance.BuilderEndpoint, instance.ID, claim.Request,
		verifyBinhost, stablePhaseUUID(claim.ID, "verify-unsigned"),
		func() error { return m.refreshPhaseClaim(claim) },
	); err != nil {
		return fmt.Errorf("install verification failed: %w", err)
	}
	unsignedRoot := value.StagingRoot
	if m.config.GPGEnabled {
		if err := m.verifyUnsignedRejectedOnBuilderStableFenced(
			claim.JobID, instance.BuilderEndpoint, claim.Request, verifyBinhost,
			stablePhaseUUID(claim.ID, "verify-negative-control"),
			func() error { return m.refreshPhaseClaim(claim) },
		); err != nil {
			return fmt.Errorf("unsigned negative-control verification failed: %w", err)
		}
		if err := m.revokeArtifactCapability(claim.JobID); err != nil {
			return fmt.Errorf("revoke unsigned verification capability: %w", err)
		}
		m.appendJobLog(claim.JobID,
			"[sign] signing remains fenced inside the active verify phase")
		if err := m.refreshPhaseClaim(claim); err != nil {
			return fmt.Errorf("durable phase fence rejected signing: %w", err)
		}
		if err := m.signJobArtifacts(claim.JobID); err != nil {
			return fmt.Errorf("isolated signing failed: %w", err)
		}
		signedBinhost, err := m.prepareVerificationBinhost(
			claim.JobID, claim.Request.Arch,
		)
		if err != nil {
			return fmt.Errorf("prepare signed verification binhost: %w", err)
		}
		if err := m.verifyOnBuilderStableFenced(
			claim.JobID, instance.BuilderEndpoint, instance.ID, claim.Request,
			signedBinhost, stablePhaseUUID(claim.ID, "verify-signed"),
			func() error { return m.refreshPhaseClaim(claim) },
		); err != nil {
			return fmt.Errorf("signed install verification failed: %w", err)
		}
	}
	m.capturePhaseContext(claim.JobID, value)
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	err = m.phaseQueue.SavePhaseExecutionContext(ctx, claim, value)
	cancel()
	if err == nil && unsignedRoot != "" && unsignedRoot != value.StagingRoot {
		_ = os.RemoveAll(unsignedRoot)
	}
	return err
}

func (m *Manager) executePublishPhase(
	claim *PhaseWorkClaim,
	value *PhaseExecutionContext,
) error {
	instance, _, err := phaseRuntime(claim, value)
	if err != nil {
		return err
	}
	if !m.updateStatus(claim.JobID, "publishing", instance.ID, "") {
		return fmt.Errorf("durable publish transition was rejected")
	}
	if err := m.refreshPhaseClaim(claim); err != nil {
		return fmt.Errorf("durable phase fence rejected publication: %w", err)
	}
	if err := m.promoteJobArtifacts(claim.JobID); err != nil {
		return fmt.Errorf("artifact promotion failed: %w", err)
	}
	m.appendJobLog(claim.JobID,
		"[publish] immutable destinations and Packages index committed")
	if up := newMirrorUploader(m.CloudSettings()); up != nil {
		if err := m.uploadJobToMirror(claim.JobID, up); err != nil {
			m.appendJobLog(claim.JobID,
				"[publish] warning: secondary mirror update failed: "+err.Error())
		}
	}
	m.capturePhaseContext(claim.JobID, value)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = m.phaseQueue.SavePhaseExecutionContext(ctx, claim, value)
	cancel()
	return err
}

func phaseIdentity(
	claim *PhaseWorkClaim,
	workerID string,
) workergateway.Identity {
	return workergateway.Identity{
		WorkerID: workerID, JobID: claim.JobID,
		AttemptID: claim.AttemptID, FenceToken: claim.AttemptFence,
	}
}

func phaseRuntime(
	claim *PhaseWorkClaim,
	value *PhaseExecutionContext,
) (*iac.Instance, workergateway.Identity, error) {
	if value == nil || value.Instance == nil || value.WorkerID == "" {
		return nil, workergateway.Identity{},
			fmt.Errorf("phase %s has no provisioned worker context", claim.Phase)
	}
	instance := phaseInstanceToIAC(value.Instance)
	identity := phaseIdentity(claim, value.WorkerID)
	if err := identity.Validate(); err != nil {
		return nil, workergateway.Identity{}, err
	}
	return instance, identity, nil
}

func phaseInstanceFromIAC(instance *iac.Instance) *PhaseInstanceContext {
	if instance == nil {
		return nil
	}
	return &PhaseInstanceContext{
		ID: instance.ID, Provider: instance.Provider, Status: instance.Status,
		IPAddress: instance.IPAddress, PublicIP: instance.PublicIP,
		PrivateIP: instance.PrivateIP, Arch: instance.Arch,
		Metadata:     cloneStringMap(instance.Metadata),
		TerraformDir: instance.TerraformDir, SSHUser: instance.SSHUser,
		BuilderEndpoint: instance.BuilderEndpoint,
		CreatedAt:       instance.CreatedAt, TTL: instance.TTL,
	}
}

func phaseInstanceToIAC(instance *PhaseInstanceContext) *iac.Instance {
	if instance == nil {
		return nil
	}
	return &iac.Instance{
		ID: instance.ID, Provider: instance.Provider, Status: instance.Status,
		IPAddress: instance.IPAddress, PublicIP: instance.PublicIP,
		PrivateIP: instance.PrivateIP, Arch: instance.Arch,
		Metadata:     cloneStringMap(instance.Metadata),
		TerraformDir: instance.TerraformDir, SSHUser: instance.SSHUser,
		BuilderEndpoint: instance.BuilderEndpoint,
		CreatedAt:       instance.CreatedAt, TTL: instance.TTL,
		LastActivity: time.Now().UTC(),
	}
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (m *Manager) applyPhaseContext(
	jobID string,
	value *PhaseExecutionContext,
) {
	if value == nil {
		return
	}
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	job := m.jobs[jobID]
	if job == nil {
		return
	}
	if value.Instance != nil {
		job.InstanceID = value.Instance.ID
	}
	if m.objectQuarantineEnabled() {
		job.StagingRoot = ""
	} else {
		job.StagingRoot = value.StagingRoot
	}
	job.VerificationToken = value.VerificationToken
	job.StagedArtifacts = append([]string(nil), value.StagedArtifacts...)
	job.StagedPrimary = value.StagedPrimary
	job.Signed, job.SigningKeyID = value.Signed, value.SigningKeyID
	job.ArtifactPath, job.ArtifactURL = value.ArtifactPath, value.ArtifactURL
	job.ArtifactPaths = append([]string(nil), value.ArtifactPaths...)
	job.Artifacts = append([]string(nil), value.Artifacts...)
}

func (m *Manager) capturePhaseContext(
	jobID string,
	value *PhaseExecutionContext,
) {
	m.jobsMu.RLock()
	job := m.jobs[jobID]
	if job != nil {
		if m.objectQuarantineEnabled() {
			value.StagingRoot = ""
		} else {
			value.StagingRoot = job.StagingRoot
		}
		value.VerificationToken = job.VerificationToken
		value.StagedArtifacts = append([]string(nil), job.StagedArtifacts...)
		value.StagedPrimary = job.StagedPrimary
		value.Signed, value.SigningKeyID = job.Signed, job.SigningKeyID
		value.ArtifactPath, value.ArtifactURL = job.ArtifactPath, job.ArtifactURL
		value.ArtifactPaths = append([]string(nil), job.ArtifactPaths...)
		value.Artifacts = append([]string(nil), job.Artifacts...)
	}
	m.jobsMu.RUnlock()
}
