package builder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/slchris/portage-engine/internal/metrics"
	"github.com/slchris/portage-engine/pkg/config"
)

// phaseQueueStub accepts every durable phase call so a test can drive one
// active-mode phase claim end to end without PostgreSQL.
type phaseQueueStub struct {
	mu        sync.Mutex
	value     *PhaseExecutionContext
	loadErr   error
	finalized [][2]string
	failed    []string
}

func (*phaseQueueStub) ActivatePhasePlan(context.Context, *BuildStatus) error { return nil }

func (*phaseQueueStub) ClaimPhaseWork(
	context.Context, string, time.Duration, []string, ...string,
) (*PhaseWorkClaim, error) {
	return nil, nil
}

func (*phaseQueueStub) RenewPhaseWork(context.Context, *PhaseWorkClaim, time.Duration) error {
	return nil
}

func (*phaseQueueStub) CompletePhaseWork(context.Context, *PhaseWorkClaim) error { return nil }

func (s *phaseQueueStub) SavePhaseExecutionContext(
	_ context.Context, _ *PhaseWorkClaim, value *PhaseExecutionContext,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copyValue := *value
	s.value = &copyValue
	return nil
}

func (s *phaseQueueStub) LoadPhaseExecutionContext(
	context.Context, *PhaseWorkClaim,
) (*PhaseExecutionContext, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	copyValue := *s.value
	return &copyValue, nil
}

func (s *phaseQueueStub) FinalizePhaseWork(
	_ context.Context, _ *PhaseWorkClaim, previous, completed *BuildStatus,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalized = append(s.finalized, [2]string{previous.Status, completed.Status})
	return nil
}

func (s *phaseQueueStub) FailPhaseWorkAndJob(
	_ context.Context, _ *PhaseWorkClaim, _ *BuildStatus, detail string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = append(s.failed, detail)
	return nil
}

// TestActivePublishPhaseCountsTheCompletedBuild covers the only topology the
// public boundary supports: under PHASE_EXECUTOR_MODE=active a job reaches
// "completed" through FinalizePhaseWork and never through updateStatus, so a
// success counter written only from updateStatus leaves the Grafana success
// rate at a permanent red 0% on a healthy cluster.
func TestActivePublishPhaseCountsTheCompletedBuild(t *testing.T) {
	registry := metrics.New(&metrics.Config{Enabled: true})
	before := registry.GetSnapshot()["builds_succeeded"].(int64)

	manager, claim, queue := newPublishPhaseFixture(t)
	manager.executePhaseClaim(claim)

	queue.mu.Lock()
	finalized, failed := queue.finalized, queue.failed
	queue.mu.Unlock()
	if len(failed) != 0 {
		t.Fatalf("publish phase failed: %v", failed)
	}
	if len(finalized) != 1 || finalized[0] != [2]string{"publishing", "completed"} {
		t.Fatalf("finalized = %v", finalized)
	}
	after := registry.GetSnapshot()["builds_succeeded"].(int64)
	if after-before != 1 {
		t.Fatalf("builds_succeeded moved by %d over an active-mode completion", after-before)
	}

	// A legacy updateStatus arriving after the phase path already committed
	// the same completion must not count the build a second time.
	manager.updateStatus(claim.JobID, "completed", "pve-1", "")
	if repeat := registry.GetSnapshot()["builds_succeeded"].(int64); repeat != after {
		t.Fatalf("one build was counted %d times", repeat-before)
	}
}

// TestActivePhaseFailureCountsTheFailedBuild is the completion counter's
// mirror. Under PHASE_EXECUTOR_MODE=active a job reaches "failed" through
// FailPhaseWorkAndJob and never through updateStatus, and the only other
// writer of the failed counter is the remote-builder HTTP client, a path
// StartWorkers refuses to run in active mode at all. Both "Builds Failed" and
// the failed series of the build-rate chart therefore sat at zero no matter
// how many builds broke.
func TestActivePhaseFailureCountsTheFailedBuild(t *testing.T) {
	registry := metrics.New(&metrics.Config{Enabled: true})
	before := registry.GetSnapshot()["builds_failed"].(int64)

	manager, claim, queue := newPublishPhaseFixture(t)
	queue.mu.Lock()
	queue.loadErr = errors.New("phase context unreadable")
	queue.mu.Unlock()
	manager.executePhaseClaim(claim)

	queue.mu.Lock()
	finalized, failed := queue.finalized, queue.failed
	queue.mu.Unlock()
	if len(finalized) != 0 {
		t.Fatalf("finalized a phase whose context never loaded: %v", finalized)
	}
	if len(failed) != 1 {
		t.Fatalf("failed = %v", failed)
	}
	after := registry.GetSnapshot()["builds_failed"].(int64)
	if after-before != 1 {
		t.Fatalf("builds_failed moved by %d over an active-mode failure", after-before)
	}

	// A legacy updateStatus arriving after the phase path already committed
	// the same failure must not count the build a second time.
	manager.updateStatus(claim.JobID, "failed", "pve-1", "phase context unreadable")
	if repeat := registry.GetSnapshot()["builds_failed"].(int64); repeat != after {
		t.Fatalf("one failure was counted %d times", repeat-before)
	}
}

func newPublishPhaseFixture(t *testing.T) (*Manager, *PhaseWorkClaim, *phaseQueueStub) {
	t.Helper()
	staging := t.TempDir()
	const relative = "app-misc/jq/jq-1.8.2.gpkg.tar"
	local := filepath.Join(staging, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(local), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("package"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(&config.ServerConfig{
		MaxWorkers: 0, BuildMode: "native-gentoo", PhaseExecutorMode: "active",
		BinpkgPath: filepath.Join(t.TempDir(), "binpkgs"),
		DataDir:    filepath.Join(t.TempDir(), "data"),
	})
	t.Cleanup(manager.Shutdown)
	manager.SetArtifactPromotionHook(func(
		_ string, rels []string, _, binhostPath string,
	) ([]string, error) {
		paths := make([]string, 0, len(rels))
		for _, rel := range rels {
			paths = append(paths, filepath.Join(
				manager.config.BinpkgPath, binhostPath, filepath.FromSlash(rel),
			))
		}
		return paths, nil
	})

	jobID, attemptID := uuid.NewString(), uuid.NewString()
	queue := &phaseQueueStub{value: &PhaseExecutionContext{
		Instance: &PhaseInstanceContext{
			ID: "pve-1", Provider: "pve", Status: "running", Arch: "amd64",
			BuilderEndpoint: "pull://" + attemptID,
		},
		WorkerID:        uuid.NewString(),
		StagingRoot:     staging,
		StagedArtifacts: []string{relative},
		StagedPrimary:   relative,
	}}
	manager.phaseQueue = queue

	status := &BuildStatus{
		JobID: jobID, Status: "publishing", Arch: "amd64",
		AttemptID: attemptID, FenceToken: 3,
	}
	return manager, &PhaseWorkClaim{
		ID: uuid.NewString(), JobID: jobID, AttemptID: attemptID,
		AttemptFence: 3, Phase: "publish", Sequence: 4, ClaimFence: 1,
		Request: &BuildRequest{PackageName: "app-misc/jq", Arch: "amd64"},
		Status:  status,
	}, queue
}
