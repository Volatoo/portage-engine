package builder

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/slchris/portage-engine/internal/iac"
	"github.com/slchris/portage-engine/pkg/config"
)

type recordingLedger struct {
	mu          sync.Mutex
	create      LedgerCreateResult
	createErr   error
	transition  [][2]string
	transitionE error
	hidden      []string
}

type recordingRuntimeMetadata struct {
	status *BuildStatus
	record InfraRecord
}

type phaseGateScheduler struct {
	recordingLedger
	calls     int
	claimCaps chan []string
}

func (s *phaseGateScheduler) RecordTransition(
	_ context.Context,
	previous, current *BuildStatus,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls == 1 {
		return &PhaseCapacityError{Phase: "verify", Used: 1, Limit: 1}
	}
	s.transition = append(s.transition, [2]string{previous.Status, current.Status})
	return nil
}

func (s *phaseGateScheduler) ClaimNext(
	_ context.Context,
	_ string,
	_ time.Duration,
	capabilities ...[]string,
) (*SchedulerClaim, error) {
	if s.claimCaps != nil && len(capabilities) > 0 {
		select {
		case s.claimCaps <- append([]string(nil), capabilities[0]...):
		default:
		}
	}
	return nil, nil
}
func (*phaseGateScheduler) RenewClaim(context.Context, *BuildStatus, time.Duration) error {
	return nil
}
func (*phaseGateScheduler) CheckClaim(context.Context, *BuildStatus) error { return nil }
func (*phaseGateScheduler) LoadVisible(context.Context) (map[string]*BuildStatus, error) {
	return nil, nil
}
func (*phaseGateScheduler) CancelJob(context.Context, string, string) (*BuildStatus, error) {
	return nil, nil
}
func (*phaseGateScheduler) RetryJob(context.Context, string) (*BuildStatus, error) {
	return nil, nil
}
func (*phaseGateScheduler) RuntimeStatus(context.Context) (SchedulerRuntimeStatus, error) {
	return SchedulerRuntimeStatus{}, nil
}

func (m *recordingRuntimeMetadata) RecordInfra(_ context.Context, status *BuildStatus, record InfraRecord) error {
	copyStatus := *status
	m.status = &copyStatus
	m.record = record
	return nil
}

func (*recordingRuntimeMetadata) RecordArtifacts(context.Context, *BuildStatus, []ArtifactRecord) error {
	return nil
}

func (l *recordingLedger) CreateJob(_ context.Context, _ *BuildRequest, status *BuildStatus) (LedgerCreateResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.createErr != nil {
		return LedgerCreateResult{}, l.createErr
	}
	if l.create.JobID != "" {
		return l.create, nil
	}
	return LedgerCreateResult{JobID: status.JobID, Status: status, Created: true}, nil
}

func (l *recordingLedger) RecordTransition(_ context.Context, previous, current *BuildStatus) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.transitionE != nil {
		return l.transitionE
	}
	l.transition = append(l.transition, [2]string{previous.Status, current.Status})
	return nil
}

func (l *recordingLedger) HideJob(_ context.Context, status *BuildStatus, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hidden = append(l.hidden, status.JobID)
	return nil
}

func newLedgerTestManager(t *testing.T, ledger JobLedger) *Manager {
	t.Helper()
	mgr := NewManager(&config.ServerConfig{
		MaxWorkers: 0,
		BuildMode:  "native-gentoo",
	})
	mgr.SetJobLedger(ledger)
	t.Cleanup(mgr.Shutdown)
	return mgr
}

func TestManagerJobLedgerSubmissionClaimTransitionAndHide(t *testing.T) {
	ledger := &recordingLedger{}
	mgr := newLedgerTestManager(t, ledger)

	jobID, err := mgr.SubmitBuild(&BuildRequest{
		PackageName: "app-misc/hello", Version: "2.12.1", Arch: "amd64",
		IdempotencyKey: "manager-ledger-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := mgr.GetStatus(jobID)
	if err != nil || status.Request == nil || status.Request.IdempotencyKey != "manager-ledger-1" {
		t.Fatalf("status=%+v err=%v", status, err)
	}

	claimed, err := mgr.claimJob(jobID)
	if err != nil || !claimed {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	mgr.updateStatus(jobID, "building", "worker-1", "")
	mgr.updateStatus(jobID, "completed", "worker-1", "")
	if err := mgr.DeleteJob(jobID); err != nil {
		t.Fatal(err)
	}

	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	want := [][2]string{{"queued", "claimed"}, {"claimed", "building"}, {"building", "completed"}}
	if len(ledger.transition) != len(want) {
		t.Fatalf("transitions = %+v", ledger.transition)
	}
	for i := range want {
		if ledger.transition[i] != want[i] {
			t.Fatalf("transition[%d] = %v, want %v", i, ledger.transition[i], want[i])
		}
	}
	if len(ledger.hidden) != 1 || ledger.hidden[0] != jobID {
		t.Fatalf("hidden = %v", ledger.hidden)
	}
}

func TestManagerClaimWaitsForLedger(t *testing.T) {
	ledger := &recordingLedger{transitionE: errors.New("database unavailable")}
	mgr := newLedgerTestManager(t, ledger)
	jobID, err := mgr.SubmitBuild(&BuildRequest{PackageName: "app-misc/hello", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := mgr.claimJob(jobID)
	if err == nil || claimed {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	status, err := mgr.GetStatus(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "queued" {
		t.Fatalf("status after failed ledger claim = %q", status.Status)
	}
}

func TestManagerWaitsForPhaseCapacityWithoutFailingTransition(t *testing.T) {
	scheduler := &phaseGateScheduler{}
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0, BuildMode: "native-gentoo"})
	mgr.scheduler = scheduler
	t.Cleanup(mgr.Shutdown)

	previous := &BuildStatus{
		JobID: "phase-gate-job", Status: "building",
		AttemptID: uuid.NewString(), FenceToken: 1, LeaseOwner: "worker",
	}
	current := *previous
	current.Status = "verifying"
	if err := mgr.recordDurableTransition(previous, &current); err != nil {
		t.Fatal(err)
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.calls != 2 || len(scheduler.transition) != 1 {
		t.Fatalf(
			"phase transition calls=%d transitions=%v",
			scheduler.calls, scheduler.transition,
		)
	}
}

// serializationFailureScheduler rejects the first two transitions the way
// PostgreSQL rejects concurrent phase transitions before committing the third.
type serializationFailureScheduler struct {
	phaseGateScheduler
	failures int
	verdict  error
}

func (s *serializationFailureScheduler) RecordTransition(
	_ context.Context,
	previous, current *BuildStatus,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.verdict != nil {
		return s.verdict
	}
	if s.calls <= s.failures {
		return fmt.Errorf("record transition: %w",
			&pgconn.PgError{Code: "40001", Message: "could not serialize access"})
	}
	s.transition = append(s.transition, [2]string{previous.Status, current.Status})
	return nil
}

func TestManagerRetriesSerializationFailuresBeforeFailingTheJob(t *testing.T) {
	scheduler := &serializationFailureScheduler{failures: 2}
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0, BuildMode: "native-gentoo"})
	mgr.scheduler = scheduler
	t.Cleanup(mgr.Shutdown)

	previous := &BuildStatus{
		JobID: "retry-job", Status: "building",
		AttemptID: uuid.NewString(), FenceToken: 1, LeaseOwner: "worker",
	}
	current := *previous
	current.Status = "verifying"
	if err := mgr.recordDurableTransition(previous, &current); err != nil {
		t.Fatalf("transient serialization failure failed the job: %v", err)
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.calls != 3 || len(scheduler.transition) != 1 {
		t.Fatalf("calls=%d transitions=%v", scheduler.calls, scheduler.transition)
	}
}

func TestManagerDoesNotRetryLedgerVerdicts(t *testing.T) {
	scheduler := &serializationFailureScheduler{
		verdict: errors.New("job ledger state is failed, expected building"),
	}
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0, BuildMode: "native-gentoo"})
	mgr.scheduler = scheduler
	t.Cleanup(mgr.Shutdown)

	previous := &BuildStatus{
		JobID: "fence-job", Status: "building",
		AttemptID: uuid.NewString(), FenceToken: 1, LeaseOwner: "worker",
	}
	current := *previous
	current.Status = "verifying"
	if err := mgr.recordDurableTransition(previous, &current); err == nil {
		t.Fatal("a stale-fence verdict was swallowed")
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.calls != 1 {
		t.Fatalf("a ledger verdict was retried: calls=%d", scheduler.calls)
	}
}

func TestSyncLedgerJobsKeepsTheSignedGenerationOfTheLiveAttempt(t *testing.T) {
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0, BuildMode: "native-gentoo"})
	t.Cleanup(mgr.Shutdown)

	const jobID = "5f4d6ba1-1e0f-4b48-9a51-6f3c0a4f2e77"
	attempt := "0d1f2a3b-4c5d-4e6f-8a9b-0c1d2e3f4a5b"
	mgr.jobs[jobID] = &BuildStatus{
		JobID: jobID, Status: "verifying", AttemptID: attempt, FenceToken: 4,
		Signed: true, SigningKeyID: "AAAA1111AAAA1111",
		ArtifactPath: "/binpkgs/app-misc/jq/jq-1.8.2.gpkg.tar",
		ArtifactURL:  "/binpkgs/app-misc/jq/jq-1.8.2.gpkg.tar",
		Artifacts:    []string{"/binpkgs/app-misc/jq/jq-1.8.2.gpkg.tar"},
	}

	// The reconciler ticks between signing and the signed verify, so the
	// durable snapshot still describes the pre-signing generation.
	mgr.SyncLedgerJobs(map[string]*BuildStatus{jobID: {
		JobID: jobID, Status: "verifying", AttemptID: attempt, FenceToken: 4,
	}})
	live := mgr.jobs[jobID]
	if !live.Signed || live.SigningKeyID != "AAAA1111AAAA1111" {
		t.Fatalf("reconciler cleared the signed generation: %+v", live)
	}
	if live.ArtifactURL == "" || live.ArtifactPath == "" || len(live.Artifacts) != 1 {
		t.Fatalf("reconciler cleared the promoted artifact locations: %+v", live)
	}

	// A requeued attempt is a different generation and must not inherit them.
	mgr.SyncLedgerJobs(map[string]*BuildStatus{jobID: {
		JobID: jobID, Status: "queued",
		AttemptID: "9e8d7c6b-5a49-4382-9170-6f5e4d3c2b1a",
	}})
	retried := mgr.jobs[jobID]
	if retried.Signed || retried.SigningKeyID != "" || retried.ArtifactURL != "" ||
		len(retried.Artifacts) != 0 {
		t.Fatalf("a new attempt inherited the previous generation: %+v", retried)
	}
}

func TestManagerIdempotentLedgerResultDoesNotEnqueueDuplicate(t *testing.T) {
	existing := &BuildStatus{
		JobID: "f18f572e-0b18-43f4-b06a-5aef965d7e33", Status: "completed",
		PackageName: "app-misc/hello", Arch: "amd64",
		CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now(),
	}
	ledger := &recordingLedger{create: LedgerCreateResult{
		JobID: existing.JobID, Status: existing, Created: false,
	}}
	mgr := newLedgerTestManager(t, ledger)

	jobID, err := mgr.SubmitBuild(&BuildRequest{
		PackageName: "app-misc/hello", Arch: "amd64", IdempotencyKey: "same-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if jobID != existing.JobID || len(mgr.workQueue) != 0 {
		t.Fatalf("job=%s queue=%d", jobID, len(mgr.workQueue))
	}
}

func TestManagerClassifiesLedgerAndIdempotencyErrors(t *testing.T) {
	conflict := NewIdempotencyConflictError(errors.New("key belongs to another request"))
	mgr := newLedgerTestManager(t, &recordingLedger{createErr: conflict})

	_, err := mgr.SubmitBuild(&BuildRequest{
		PackageName: "app-misc/hello", Arch: "amd64", IdempotencyKey: "conflict-key",
	})
	if err == nil || !IsLedgerError(err) || !IsIdempotencyConflict(err) {
		t.Fatalf("error classification = %v", err)
	}
	if len(mgr.workQueue) != 0 || len(mgr.GetJobsSnapshot()) != 0 {
		t.Fatal("ledger-rejected submission reached the memory queue")
	}
}

func TestManagerRejectsSubmitAfterShutdown(t *testing.T) {
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0, BuildMode: "native-gentoo"})
	mgr.Shutdown()

	if _, err := mgr.SubmitBuild(&BuildRequest{PackageName: "app-misc/hello", Arch: "amd64"}); err == nil {
		t.Fatal("submission succeeded after shutdown")
	}
}

func TestInfraCleanupUsesCapturedAttemptFenceAfterTerminalProjectionRefresh(t *testing.T) {
	metadata := &recordingRuntimeMetadata{}
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0, BuildMode: "native-gentoo"})
	mgr.metadata = metadata
	t.Cleanup(mgr.Shutdown)

	const jobID = "c4d6131d-cdb8-4aad-867a-a227f8ba8282"
	mgr.jobs[jobID] = &BuildStatus{
		JobID: jobID, Status: "publishing",
		AttemptID:  "69c56195-dfa2-4850-b9e6-e4cdb89ff63a",
		FenceToken: 7, LeaseOwner: "server/slot-0",
	}
	cleanupFence, err := mgr.jobStatusSnapshot(jobID)
	if err != nil {
		t.Fatal(err)
	}

	// A terminal PostgreSQL projection no longer has an active worker lease.
	// Cleanup must nevertheless retain the originating attempt/fence.
	mgr.SyncLedgerJobs(map[string]*BuildStatus{
		jobID: {JobID: jobID, Status: "completed"},
	})
	now := time.Now().UTC()
	if err := mgr.recordInfraWithStatus(cleanupFence, &iac.Instance{
		ID: "pve-workspace-1", Provider: "pve", Arch: "amd64",
	}, "destroyed", "", &now); err != nil {
		t.Fatal(err)
	}
	if metadata.status == nil ||
		metadata.status.AttemptID != cleanupFence.AttemptID ||
		metadata.status.FenceToken != cleanupFence.FenceToken ||
		metadata.status.LeaseOwner != cleanupFence.LeaseOwner {
		t.Fatalf("cleanup fence was not retained: %+v", metadata.status)
	}
	if metadata.record.State != "destroyed" || metadata.record.DeletedAt == nil {
		t.Fatalf("cleanup record = %+v", metadata.record)
	}
}
