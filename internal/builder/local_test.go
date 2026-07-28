package builder

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/slchris/portage-engine/pkg/config"
)

// testReusableBuilderConfig keeps tests isolated from the production
// single-use marker. Reuse is safe here because t.TempDir gives every test a
// disposable root and these tests exercise queue/status behavior rather than
// host-root hygiene.
func testReusableBuilderConfig(t *testing.T) *config.BuilderConfig {
	t.Helper()
	root := t.TempDir()
	return &config.BuilderConfig{
		NativeJobPolicy:    "unsafe-reuse",
		DataDir:            filepath.Join(root, "data"),
		WorkDir:            filepath.Join(root, "work"),
		ArtifactDir:        filepath.Join(root, "artifacts"),
		PersistenceEnabled: false,
	}
}

// TestNewLocalBuilder tests creating a new LocalBuilder.
func TestNewLocalBuilder(t *testing.T) {
	builder := NewLocalBuilder(2, testReusableBuilderConfig(t))
	if builder == nil {
		t.Fatal("NewLocalBuilder returned nil")
	}

	if builder.workers != 2 {
		t.Errorf("Expected 2 workers, got %d", builder.workers)
	}

	if builder.executor == nil {
		t.Error("Executor not initialized")
	}

}

func TestNativeSingleUsePersistsDrainAcrossRestart(t *testing.T) {
	root := t.TempDir()
	cfg := &config.BuilderConfig{
		Workers:            0, // keep the accepted job queued; no real emerge
		NativeJobPolicy:    "single-use",
		DataDir:            filepath.Join(root, "data"),
		WorkDir:            filepath.Join(root, "work"),
		ArtifactDir:        filepath.Join(root, "artifacts"),
		PersistenceEnabled: false,
	}
	first := NewLocalBuilder(0, cfg)
	if !first.AcceptingBuilds() || first.Capacity() != 1 {
		t.Fatalf("fresh single-use native builder state: accepting=%v capacity=%d", first.AcceptingBuilds(), first.Capacity())
	}
	jobID, err := first.SubmitBuild(&LocalBuildRequest{PackageName: "app-misc/hello"})
	if err != nil {
		t.Fatalf("first native job was rejected: %v", err)
	}
	if first.AcceptingBuilds() {
		t.Fatal("single-use native builder still accepts jobs after reservation")
	}
	if first.Capacity() != 0 {
		t.Fatalf("consumed single-use native builder capacity = %d, want 0", first.Capacity())
	}
	if _, err := first.SubmitBuild(&LocalBuildRequest{PackageName: "app-misc/jq"}); !errors.Is(err, ErrNativeBuilderDraining) {
		t.Fatalf("second native job error = %v, want ErrNativeBuilderDraining", err)
	}

	marker := filepath.Join(cfg.DataDir, "native-builder-tainted.json")
	info, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("taint marker missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("taint marker mode = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), jobID) {
		t.Fatalf("taint marker does not bind accepted job: %s", data)
	}

	restarted := NewLocalBuilder(0, cfg)
	if restarted.AcceptingBuilds() {
		t.Fatal("process restart cleared native taint without a rootfs reset")
	}
	if _, err := restarted.SubmitBuild(&LocalBuildRequest{PackageName: "app-misc/jq"}); !errors.Is(err, ErrNativeBuilderDraining) {
		t.Fatalf("restarted dirty builder error = %v, want ErrNativeBuilderDraining", err)
	}
	status := restarted.GetStatus()
	if status["status"] != "draining" || status["accepting_builds"] != false || status["capacity"] != 0 {
		t.Fatalf("dirty builder status did not expose drain state: %+v", status)
	}
}

func TestNativeSingleUseCorruptMarkerFailsClosed(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "native-builder-tainted.json"), []byte("{partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.BuilderConfig{
		NativeJobPolicy: "single-use",
		DataDir:         dataDir,
		WorkDir:         filepath.Join(root, "work"),
		ArtifactDir:     filepath.Join(root, "artifacts"),
	}
	b := NewLocalBuilder(0, cfg)
	if b.AcceptingBuilds() {
		t.Fatal("corrupt native taint marker was treated as a clean builder")
	}
	if _, err := b.SubmitBuild(&LocalBuildRequest{PackageName: "app-misc/hello"}); !errors.Is(err, ErrNativeBuilderDraining) {
		t.Fatalf("corrupt-marker builder error = %v, want ErrNativeBuilderDraining", err)
	}
}

// TestLocalBuilderSubmitBuild tests submitting a build job.
func TestLocalBuilderSubmitBuild(t *testing.T) {
	builder := NewLocalBuilder(0, testReusableBuilderConfig(t))

	req := &LocalBuildRequest{
		PackageName: "dev-lang/python",
		Version:     "3.11.0",
		UseFlags:    map[string]string{"ssl": "enabled"},
		Environment: map[string]string{"ARCH": "amd64"},
	}

	jobID, err := builder.SubmitBuild(req)
	if err != nil {
		t.Fatalf("SubmitBuild failed: %v", err)
	}

	if jobID == "" {
		t.Error("Expected non-empty job ID")
	}

	// Wait a moment for the job to be queued
	time.Sleep(100 * time.Millisecond)

	// Verify the job exists
	job, err := builder.GetJobStatus(jobID)
	if err != nil {
		t.Fatalf("GetJobStatus failed: %v", err)
	}

	if job.ID != jobID {
		t.Errorf("Expected job ID %s, got %s", jobID, job.ID)
	}

	if job.Request.PackageName != req.PackageName {
		t.Errorf("Expected package %s, got %s", req.PackageName, job.Request.PackageName)
	}
}

func TestLocalBuilderSubmitBuildIsIdempotentByExecutionID(t *testing.T) {
	local := NewLocalBuilder(0, testReusableBuilderConfig(t))
	t.Cleanup(local.Shutdown)
	executionID := uuid.NewString()
	request := &LocalBuildRequest{
		ExecutionID: executionID,
		PackageName: "app-misc/jq",
	}
	first, err := local.SubmitBuild(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := local.SubmitBuild(&LocalBuildRequest{
		ExecutionID: executionID,
		PackageName: "app-misc/jq",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("duplicate execution created job %q; want original %q", second, first)
	}
	if got := len(local.ListJobs()); got != 1 {
		t.Fatalf("duplicate execution created %d jobs; want 1", got)
	}
	if _, err := local.SubmitBuild(&LocalBuildRequest{
		ExecutionID: executionID,
		PackageName: "app-misc/hello",
	}); err == nil || !strings.Contains(err.Error(), "different build request") {
		t.Fatalf("execution ID request conflict was not rejected: %v", err)
	}
}

func TestLocalBuilderExecutionIDSurvivesRestart(t *testing.T) {
	cfg := testReusableBuilderConfig(t)
	cfg.PersistenceEnabled = true
	executionID := uuid.NewString()
	first := NewLocalBuilder(0, cfg)
	jobID, err := first.SubmitBuild(&LocalBuildRequest{
		ExecutionID: executionID,
		PackageName: "app-misc/jq",
	})
	if err != nil {
		t.Fatal(err)
	}
	first.Shutdown()

	restarted := NewLocalBuilder(0, cfg)
	t.Cleanup(restarted.Shutdown)
	replayedID, err := restarted.SubmitBuild(&LocalBuildRequest{
		ExecutionID: executionID,
		PackageName: "app-misc/jq",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayedID != jobID {
		t.Fatalf("restart replay created job %q; want original %q", replayedID, jobID)
	}
	job, err := restarted.GetJobStatus(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "failed" ||
		!strings.Contains(job.Error, "interrupted by builder restart") {
		t.Fatalf("interrupted durable job was not reconciled: %+v", job)
	}
	if got := len(restarted.ListJobs()); got != 1 {
		t.Fatalf("restart replay created %d jobs; want 1", got)
	}
}

// TestLocalBuilderGetJobStatus tests retrieving job status.
func TestLocalBuilderGetJobStatus(t *testing.T) {
	builder := NewLocalBuilder(0, testReusableBuilderConfig(t))

	req := &LocalBuildRequest{
		PackageName: "app-editors/vim",
		Version:     "9.0",
	}

	jobID, err := builder.SubmitBuild(req)
	if err != nil {
		t.Fatalf("SubmitBuild failed: %v", err)
	}

	// Wait briefly for the job to be picked up by worker
	time.Sleep(50 * time.Millisecond)

	job, err := builder.GetJobStatus(jobID)
	if err != nil {
		t.Fatalf("GetJobStatus failed: %v", err)
	}

	if job == nil {
		t.Fatal("Expected non-nil job")
	}

	// Note: Not checking job.Status directly to avoid race condition
	// The worker goroutine may be modifying it concurrently
}

// TestLocalBuilderGetJobStatusNotFound tests retrieving non-existent job.
func TestLocalBuilderGetJobStatusNotFound(t *testing.T) {
	builder := NewLocalBuilder(0, testReusableBuilderConfig(t))

	_, err := builder.GetJobStatus("non-existent-job-id")
	if err == nil {
		t.Error("Expected error for non-existent job")
	}
}

// TestLocalBuilderListJobs tests listing all jobs.
func TestLocalBuilderListJobs(t *testing.T) {
	builder := NewLocalBuilder(0, testReusableBuilderConfig(t))

	// Submit multiple jobs
	for i := 0; i < 3; i++ {
		req := &LocalBuildRequest{
			PackageName: "dev-lang/python",
			Version:     "1.0",
		}
		_, err := builder.SubmitBuild(req)
		if err != nil {
			t.Fatalf("SubmitBuild failed: %v", err)
		}
	}

	jobs := builder.ListJobs()
	if len(jobs) != 3 {
		t.Errorf("Expected 3 jobs, got %d", len(jobs))
	}
}

// TestLocalBuilderGetStatus tests getting builder status.
func TestLocalBuilderGetStatus(t *testing.T) {
	builder := NewLocalBuilder(2, testReusableBuilderConfig(t))

	status := builder.GetStatus()
	if status == nil {
		t.Fatal("Expected non-nil status")
	}

	if workers, ok := status["workers"].(int); !ok || workers != 2 {
		t.Errorf("Expected 2 workers in status, got %v", status["workers"])
	}

	if _, ok := status["total"]; !ok {
		t.Error("Expected total in status")
	}
}

// TestLocalBuilderGetArtifactPath tests getting artifact path for a job.
func TestLocalBuilderGetArtifactPath(t *testing.T) {
	builder := NewLocalBuilder(0, testReusableBuilderConfig(t))

	// Test non-existent job
	_, err := builder.GetArtifactPath("non-existent-job")
	if err == nil {
		t.Error("Expected error for non-existent job")
	}

	// Submit a job
	req := &LocalBuildRequest{
		PackageName: "dev-lang/python",
		Version:     "1.0",
	}
	jobID, err := builder.SubmitBuild(req)
	if err != nil {
		t.Fatalf("SubmitBuild failed: %v", err)
	}

	// Job is queued, not completed, should error
	_, err = builder.GetArtifactPath(jobID)
	if err == nil {
		t.Error("Expected error for non-completed job")
	}
}

// TestLocalBuilderGetArtifactInfo tests getting artifact info for a job.
func TestLocalBuilderGetArtifactInfo(t *testing.T) {
	builder := NewLocalBuilder(0, testReusableBuilderConfig(t))

	// Test non-existent job
	_, err := builder.GetArtifactInfo("non-existent-job")
	if err == nil {
		t.Error("Expected error for non-existent job")
	}

	// Submit a job
	req := &LocalBuildRequest{
		PackageName: "dev-lang/python",
		Version:     "1.0",
	}
	jobID, err := builder.SubmitBuild(req)
	if err != nil {
		t.Fatalf("SubmitBuild failed: %v", err)
	}

	// Job is queued, not completed, should error
	_, err = builder.GetArtifactInfo(jobID)
	if err == nil {
		t.Error("Expected error for non-completed job")
	}
}

// TestArtifactInfo tests ArtifactInfo struct.
func TestArtifactInfo(t *testing.T) {
	info := &ArtifactInfo{
		JobID:       "job-123",
		FileName:    "test-1.0.tbz2",
		FilePath:    "/tmp/artifacts/test-1.0.tbz2",
		FileSize:    1024,
		PackageName: "dev-lang/python",
		Version:     "1.0",
	}

	if info.JobID != "job-123" {
		t.Errorf("Expected JobID 'job-123', got '%s'", info.JobID)
	}
	if info.FileName != "test-1.0.tbz2" {
		t.Errorf("Expected FileName 'test-1.0.tbz2', got '%s'", info.FileName)
	}
	if info.FileSize != 1024 {
		t.Errorf("Expected FileSize 1024, got %d", info.FileSize)
	}
}

// TestLocalBuilderSubmitBuildQueueFull verifies SubmitBuild does a non-blocking
// send and returns an error (instead of hanging) when the queue is full.
func TestLocalBuilderSubmitBuildQueueFull(t *testing.T) {
	// Build a LocalBuilder with a tiny, unconsumed queue (no workers) so it
	// fills immediately.
	lb := &LocalBuilder{
		workers:  0,
		jobQueue: make(chan *BuildJob, 1),
		jobs:     make(map[string]*BuildJob),
	}

	req := &LocalBuildRequest{PackageName: "dev-lang/python", Version: "3.11.0"}

	// First submit fills the single-slot queue.
	if _, err := lb.SubmitBuild(req); err != nil {
		t.Fatalf("first SubmitBuild should succeed, got error: %v", err)
	}

	// Second submit must fail fast rather than block.
	done := make(chan struct{})
	var submitErr error
	go func() {
		_, submitErr = lb.SubmitBuild(req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SubmitBuild blocked when queue was full; expected non-blocking error")
	}

	if submitErr == nil {
		t.Fatal("expected error when queue is full, got nil")
	}
	if submitErr.Error() != "builder queue full" {
		t.Errorf("expected 'builder queue full' error, got: %v", submitErr)
	}

	// The rejected job must not be left registered in the jobs map.
	if len(lb.jobs) != 1 {
		t.Errorf("expected only the accepted job to remain, got %d jobs", len(lb.jobs))
	}
}

// TestReconcileLoadedJobs verifies interrupted "building"/"queued" jobs are
// marked failed on startup and persisted.
func TestReconcileLoadedJobs(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJobStore(dir)
	if err != nil {
		t.Fatalf("NewJobStore failed: %v", err)
	}

	jobs := map[string]*BuildJob{
		"a": {ID: "a", Status: "building"},
		"b": {ID: "b", Status: "queued"},
		"c": {ID: "c", Status: "success"},
	}

	reconcileLoadedJobs(store, jobs)

	if jobs["a"].Status != "failed" {
		t.Errorf("expected building job to be reconciled to failed, got %s", jobs["a"].Status)
	}
	if jobs["b"].Status != "failed" {
		t.Errorf("expected queued job to be reconciled to failed, got %s", jobs["b"].Status)
	}
	if jobs["c"].Status != "success" {
		t.Errorf("expected success job to be untouched, got %s", jobs["c"].Status)
	}

	// Verify the reconciled state was persisted.
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded["a"].Status != "failed" {
		t.Errorf("expected persisted job a to be failed, got %s", loaded["a"].Status)
	}
}
