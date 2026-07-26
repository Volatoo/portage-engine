package builder

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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
