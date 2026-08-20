package builder

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/pkg/config"
)

// TestNewManager tests the creation of a build manager.
func TestNewManager(t *testing.T) {
	cfg := &config.ServerConfig{
		MaxWorkers:    4,
		CloudProvider: "gcp",
	}

	mgr := NewManager(cfg)
	if mgr == nil {
		t.Fatal("NewManager returned nil")
	}

	if mgr.config != cfg {
		t.Error("Config not set correctly")
	}

	if mgr.jobs == nil {
		t.Error("Jobs map is nil")
	}

	if mgr.workQueue == nil {
		t.Error("Work queue is nil")
	}
}

// TestSubmitBuildManager tests submitting a build request.
func TestSubmitBuildManager(t *testing.T) {
	// No workers so the job stays "queued" for the assertion (a worker would
	// immediately claim it).
	cfg := &config.ServerConfig{
		MaxWorkers: 0,
	}

	mgr := NewManager(cfg)

	req := &BuildRequest{
		PackageName: "dev-lang/python",
		Version:     "3.11",
		Arch:        "amd64",
		UseFlags:    []string{"ssl", "threads"},
	}

	jobID, err := mgr.SubmitBuild(req)
	if err != nil {
		t.Fatalf("SubmitBuild failed: %v", err)
	}

	if jobID == "" {
		t.Error("Job ID is empty")
	}

	status, err := mgr.GetStatus(jobID)
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}

	if status.Status != "queued" {
		t.Errorf("Expected status=queued, got %s", status.Status)
	}

	if status.PackageName != "dev-lang/python" {
		t.Errorf("Expected package=dev-lang/python, got %s", status.PackageName)
	}
}

func TestSubmitBuildResolvesServerOwnedCatalogInputs(t *testing.T) {
	mgr := NewManager(&config.ServerConfig{MaxWorkers: 0})
	c := &catalog.Catalog{
		Version: 1,
		Profiles: []catalog.ProfileDefinition{{
			ID: "amd64/base", Arch: "amd64", ProfilePath: "default/linux/amd64/23.0",
			BinhostPath:         "releases/amd64/binpackages/23.0/x86-64",
			ProfileRepositoryID: "gentoo", RepositoryIDs: []string{"gentoo"}, ImageID: "pve/amd64-001",
			MirrorBundleID: "offline/001", DefaultResourceClass: "small",
			RequiredFeatures: []string{"binpkg-multi-instance", "sandbox", "userpriv"},
			EgressPolicyID:   "egress/internal",
			Default:          true, Channel: "stable",
		}},
		Repositories: []catalog.RepositoryDefinition{{
			ID: "gentoo", Name: "gentoo", Location: "/var/db/repos/gentoo",
			SyncType: "git", SyncURI: "https://mirror.internal/gentoo.git",
			Revision: "0123456789abcdef0123456789abcdef01234567", Channel: "stable",
		}},
		Images: []catalog.ImageManifest{{
			ID: "pve/amd64-001", ProfileID: "amd64/base", Generation: "001", Provider: "pve", Arch: "amd64",
			BuildMode: "native-gentoo", Template: "9001",
			Digest:       "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RootfsSource: "catalyst-stage3", Channel: "stable",
		}},
		MirrorBundles: []catalog.MirrorBundle{{
			ID: "offline/001", Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			CreatedAt: time.Now().UTC().Add(-time.Hour), FreshUntil: time.Now().UTC().Add(24 * time.Hour), AdvisoryWatermark: "2026-07-22T00:00:00Z", Channel: "stable",
		}},
		ResourceClasses: []catalog.ResourceClass{{
			ID: "small", MachineSpec: map[string]string{"cores": "2", "memory": "4096", "disk_size": "40"},
			MaxRuntimeMinutes: 60, CloudCostMicrounitsPerMinute: 1000,
		}},
		EgressPolicies: []catalog.EgressPolicy{{
			ID: "egress/internal", Mode: catalog.EgressModeEnforce, Channel: "stable",
			Rules: []catalog.EgressRule{{ID: "git", Hosts: []string{"mirror.internal"}, CIDRs: []string{"10.31.0.2/32"}, Protocol: "tcp", Ports: []int{443}}},
		}},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	mgr.SetBuildCatalog(c)

	bundle := validSecurityTestBundle()
	bundle.Metadata.RequiredFeatures = []string{"test"}
	bundle.Config.Repos[0].RegistryID = "gentoo"
	bundle.Config.Repos[0].SyncURI = "https://client.example/untrusted-but-valid.git"
	req := &BuildRequest{
		PackageName: "dev-lang/python", Version: "3.11.9", ProfileID: "amd64/base",
		RepositoryIDs: []string{"gentoo"}, ResourceClass: "small", ConfigBundle: bundle,
	}
	jobID, err := mgr.SubmitBuild(req)
	if err != nil {
		t.Fatalf("SubmitBuild failed: %v", err)
	}
	if got := req.ConfigBundle.Config.Repos[0].SyncURI; got != "https://mirror.internal/gentoo.git" {
		t.Fatalf("client repository URI was not replaced: %q", got)
	}
	if got := req.ConfigBundle.Config.Repos[0].Revision; got != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("repository revision was not pinned: %q", got)
	}
	if got := strings.Join(req.ConfigBundle.Metadata.RequiredFeatures, " "); got != "binpkg-multi-instance sandbox userpriv" {
		t.Fatalf("client feature policy was not replaced by catalog policy: %q", got)
	}
	status, err := mgr.GetStatus(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ResolvedContext == nil || status.ResolvedContext.Template != "9001" || status.ResolvedContext.ImageID != "pve/amd64-001" {
		t.Fatalf("resolved provenance missing from job: %+v", status.ResolvedContext)
	}
	if req.MachineSpec["cores"] != "2" || req.CloudProvider != "pve" {
		t.Fatalf("catalog resource/provider not applied: %+v provider=%q", req.MachineSpec, req.CloudProvider)
	}

	simpleReq := &BuildRequest{PackageName: "app-misc/hello", Version: "2.12.1", ProfileID: "amd64/base"}
	if _, err := mgr.SubmitBuild(simpleReq); err != nil {
		t.Fatalf("simple request failed: %v", err)
	}
	if simpleReq.ConfigBundle == nil || len(simpleReq.ConfigBundle.Config.Repos) != 1 || simpleReq.ConfigBundle.Config.Repos[0].Revision == "" {
		t.Fatalf("simple request lost resolved repository provenance: %+v", simpleReq.ConfigBundle)
	}

	_, err = mgr.SubmitBuild(&BuildRequest{PackageName: "dev-lang/python", Version: "3.11", ProfileID: "missing"})
	if err == nil || !IsRequestError(err) {
		t.Fatalf("unknown profile should be a request error, got %v", err)
	}
	_, err = mgr.SubmitBuild(&BuildRequest{
		PackageName: "dev-lang/python", Version: "3.11", ProfileID: "amd64/base",
		MachineSpec: map[string]string{"template": "attacker-template"},
	})
	if err == nil || !IsRequestError(err) {
		t.Fatalf("operator-owned machine spec should be rejected, got %v", err)
	}
	_, err = mgr.SubmitBuild(&BuildRequest{
		PackageName: "dev-lang/python", Version: "3.11", ProfileID: "amd64/base",
		MachineSpec: map[string]string{"cores": "128"},
	})
	if err == nil || !IsRequestError(err) {
		t.Fatalf("catalog resource class should not be overridden, got %v", err)
	}
}

func TestValidateResolvedEgressRejectsRuntimeEndpointDrift(t *testing.T) {
	policy := catalog.EgressPolicy{
		ID: "egress/internal", Mode: catalog.EgressModeEnforce, Channel: "stable",
		Rules: []catalog.EgressRule{{
			ID: "services", Hosts: []string{"control.internal", "nas.internal"},
			CIDRs: []string{"10.31.0.2/32"}, Protocol: "tcp", Ports: []int{80, 443},
		}},
	}
	digest, err := policy.Digest()
	if err != nil {
		t.Fatal(err)
	}
	context := &catalog.ResolvedBuildContext{EgressPolicy: policy, EgressPolicyDigest: digest}
	if err := validateResolvedEgress("pve", context, map[string]string{
		"server callback": "http://control.internal:80",
		"binhost":         "https://nas.internal/binpkgs",
	}); err != nil {
		t.Fatalf("approved endpoints were rejected: %v", err)
	}
	if err := validateResolvedEgress("pve", context, map[string]string{
		"public fallback": "https://distfiles.gentoo.org",
	}); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("unapproved runtime endpoint was accepted: %v", err)
	}
	if err := validateResolvedEgress("gcp", context, nil); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("unsupported network enforcement provider was accepted: %v", err)
	}
	context.EgressPolicyDigest = "sha256:" + strings.Repeat("0", 64)
	if err := validateResolvedEgress("pve", context, nil); err == nil || !strings.Contains(err.Error(), "digest changed") {
		t.Fatalf("mutated policy was accepted: %v", err)
	}
}

// TestGetStatusNotFound tests getting status of non-existent job.
func TestGetStatusNotFound(t *testing.T) {
	cfg := &config.ServerConfig{
		MaxWorkers: 2,
	}

	mgr := NewManager(cfg)

	_, err := mgr.GetStatus("non-existent-job")
	if err == nil {
		t.Error("Expected error for non-existent job, got nil")
	}
}

// TestListAllBuilds tests listing all build jobs.
func TestListAllBuilds(t *testing.T) {
	cfg := &config.ServerConfig{
		MaxWorkers: 2,
	}

	mgr := NewManager(cfg)

	// Submit a few builds
	req1 := &BuildRequest{
		PackageName: "dev-lang/python",
		Version:     "3.11",
		Arch:        "amd64",
	}
	_, _ = mgr.SubmitBuild(req1)

	req2 := &BuildRequest{
		PackageName: "sys-devel/gcc",
		Version:     "13.2.0",
		Arch:        "amd64",
	}
	_, _ = mgr.SubmitBuild(req2)

	builds := mgr.ListAllBuilds()
	if len(builds) != 2 {
		t.Errorf("Expected 2 builds, got %d", len(builds))
	}
}

// TestGetClusterStatus tests getting cluster status.
func TestGetClusterStatus(t *testing.T) {
	cfg := &config.ServerConfig{
		MaxWorkers: 2,
	}

	mgr := NewManager(cfg)

	// Submit some builds with different statuses
	req := &BuildRequest{
		PackageName: "dev-lang/python",
		Version:     "3.11",
		Arch:        "amd64",
	}
	jobID, _ := mgr.SubmitBuild(req)

	// Give worker a moment to pick up the job
	time.Sleep(100 * time.Millisecond)

	status := mgr.GetClusterStatus()
	if status == nil {
		t.Fatal("GetClusterStatus returned nil")
	}

	if status.TotalBuilds == 0 {
		t.Error("Expected TotalBuilds > 0")
	}

	// Verify job exists in the system
	_, err := mgr.GetStatus(jobID)
	if err != nil {
		t.Errorf("Job not found: %v", err)
	}
}

// TestGetClusterStatusCalculations tests cluster status calculations.
func TestGetClusterStatusCalculations(t *testing.T) {
	t.Parallel()

	cfg := &config.ServerConfig{
		MaxWorkers: 1,
	}

	mgr := NewManager(cfg)

	// Initially should be all zeros
	status := mgr.GetClusterStatus()
	if status.TotalBuilds != 0 {
		t.Errorf("Expected TotalBuilds=0, got %d", status.TotalBuilds)
	}
	if status.SuccessRate != 0 {
		t.Errorf("Expected SuccessRate=0, got %f", status.SuccessRate)
	}

	// Submit a build
	req := &BuildRequest{
		PackageName: "dev-lang/python",
		Version:     "3.11",
		Arch:        "amd64",
	}
	_, _ = mgr.SubmitBuild(req)

	status = mgr.GetClusterStatus()
	if status.TotalBuilds != 1 {
		t.Errorf("Expected TotalBuilds=1, got %d", status.TotalBuilds)
	}

	// Verify LastUpdated is set
	if status.LastUpdated.IsZero() {
		t.Error("LastUpdated should not be zero")
	}
}

// TestClusterStatusSuccessRate tests success rate calculation.
func TestClusterStatusSuccessRate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		completed   int
		failed      int
		expectedPct float64
	}{
		{"all success", 10, 0, 100.0},
		{"all failed", 0, 10, 0.0},
		{"50-50", 5, 5, 50.0},
		{"no builds", 0, 0, 0.0},
		{"75% success", 3, 1, 75.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completed := tt.completed
			failed := tt.failed
			var rate float64
			if completed+failed > 0 {
				rate = float64(completed) / float64(completed+failed) * 100
			}
			if rate != tt.expectedPct {
				t.Errorf("Expected rate=%f, got %f", tt.expectedPct, rate)
			}
		})
	}
}

// TestBuildRequest tests BuildRequest structure.
func TestBuildRequest(t *testing.T) {
	req := &BuildRequest{
		PackageName:   "dev-lang/python",
		Version:       "3.11",
		Arch:          "amd64",
		UseFlags:      []string{"ssl", "threads"},
		CloudProvider: "gcp",
		MachineSpec: map[string]string{
			"machine_type": "n1-standard-4",
			"zone":         "us-central1-a",
		},
	}

	if req.PackageName != "dev-lang/python" {
		t.Errorf("Expected PackageName=dev-lang/python, got %s", req.PackageName)
	}

	if len(req.UseFlags) != 2 {
		t.Errorf("Expected 2 USE flags, got %d", len(req.UseFlags))
	}

	if req.MachineSpec["machine_type"] != "n1-standard-4" {
		t.Errorf("Expected machine_type=n1-standard-4, got %s", req.MachineSpec["machine_type"])
	}
}

// TestGetBuildLogs tests retrieving build logs.
func TestGetBuildLogs(t *testing.T) {
	cfg := &config.ServerConfig{
		MaxWorkers: 2,
	}

	mgr := NewManager(cfg)

	req := &BuildRequest{
		PackageName: "dev-lang/python",
		Version:     "3.11",
		Arch:        "amd64",
	}

	jobID, err := mgr.SubmitBuild(req)
	if err != nil {
		t.Fatalf("SubmitBuild failed: %v", err)
	}

	logs, err := mgr.GetBuildLogs(jobID)
	if err != nil {
		t.Fatalf("GetBuildLogs failed: %v", err)
	}

	if logs == "" {
		t.Error("Logs should not be empty")
	}

	// Check that logs contain expected information
	expectedStrings := []string{
		"Build Job:",
		"Package: dev-lang/python-3.11",
		"Architecture: amd64",
		"Status:",
	}

	for _, expected := range expectedStrings {
		if !strings.Contains(logs, expected) {
			t.Errorf("Expected logs to contain '%s'", expected)
		}
	}
}

// TestGetBuildLogsNotFound tests retrieving logs for non-existent job.
func TestGetBuildLogsNotFound(t *testing.T) {
	cfg := &config.ServerConfig{
		MaxWorkers: 2,
	}

	mgr := NewManager(cfg)

	_, err := mgr.GetBuildLogs("non-existent-job")
	if err == nil {
		t.Error("Expected error for non-existent job, got nil")
	}
}

// TestGetSchedulerStatus tests retrieving scheduler status.
func TestGetSchedulerStatus(t *testing.T) {
	cfg := &config.ServerConfig{
		MaxWorkers: 0, // Disable automatic processing
	}

	mgr := NewManager(cfg)

	// Submit some builds
	req1 := &BuildRequest{
		PackageName: "dev-lang/python",
		Version:     "3.11",
		Arch:        "amd64",
	}
	jobID1, _ := mgr.SubmitBuild(req1)

	req2 := &BuildRequest{
		PackageName: "dev-lang/ruby",
		Version:     "3.2",
		Arch:        "amd64",
	}
	_, _ = mgr.SubmitBuild(req2)

	// Simulate one job being assigned to a builder
	mgr.jobsMu.Lock()
	if job, exists := mgr.jobs[jobID1]; exists {
		job.Status = "building"
		job.InstanceID = "builder-1"
	}
	mgr.jobsMu.Unlock()

	status := mgr.GetSchedulerStatus()
	if status == nil {
		t.Fatal("GetSchedulerStatus returned nil")
	}

	builders, ok := status["builders"].([]map[string]interface{})
	if !ok {
		t.Fatal("Expected builders to be an array")
	}

	queuedTasks, ok := status["queued_tasks"].(int)
	if !ok {
		t.Fatal("Expected queued_tasks to be an int")
	}

	runningTasks, ok := status["running_tasks"].(int)
	if !ok {
		t.Fatal("Expected running_tasks to be an int")
	}

	if runningTasks != 1 {
		t.Errorf("Expected 1 running task, got %d", runningTasks)
	}

	if queuedTasks != 1 {
		t.Errorf("Expected 1 queued task, got %d", queuedTasks)
	}

	if len(builders) != 1 {
		t.Errorf("Expected 1 builder, got %d", len(builders))
	}

	if len(builders) > 0 {
		builder := builders[0]
		if builder["id"] != "builder-1" {
			t.Errorf("Expected builder id=builder-1, got %s", builder["id"])
		}
		tasks, ok := builder["tasks"].([]string)
		if !ok || len(tasks) != 1 {
			t.Errorf("Expected builder to have 1 task")
		}
		if len(tasks) > 0 && tasks[0] != jobID1 {
			t.Errorf("Expected task %s, got %s", jobID1, tasks[0])
		}
	}
}

// TestUpdateBuilderHeartbeat tests updating builder heartbeat.
func TestUpdateBuilderHeartbeat(t *testing.T) {
	cfg := &config.ServerConfig{
		MaxWorkers: 2,
	}

	mgr := NewManager(cfg)

	tests := []struct {
		name      string
		req       *HeartbeatRequest
		wantError bool
	}{
		{
			name: "valid heartbeat",
			req: &HeartbeatRequest{
				BuilderID:  "builder-1",
				Status:     "healthy",
				Endpoint:   "http://localhost:9090",
				Capacity:   4,
				ActiveJobs: 2,
				Timestamp:  time.Now(),
			},
			wantError: false,
		},
		{
			name: "missing builder_id",
			req: &HeartbeatRequest{
				Status:    "healthy",
				Timestamp: time.Now(),
			},
			wantError: true,
		},
		{
			name: "missing status",
			req: &HeartbeatRequest{
				BuilderID: "builder-1",
				Timestamp: time.Now(),
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mgr.UpdateBuilderHeartbeat(tt.req)
			if (err != nil) != tt.wantError {
				t.Errorf("UpdateBuilderHeartbeat() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

// TestUpdateBuilderHeartbeatConcurrent tests concurrent heartbeat updates.
func TestUpdateBuilderHeartbeatConcurrent(_ *testing.T) {
	cfg := &config.ServerConfig{
		MaxWorkers: 2,
	}

	mgr := NewManager(cfg)

	// Send heartbeats from multiple goroutines
	numGoroutines := 10
	done := make(chan bool, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			req := &HeartbeatRequest{
				BuilderID:  "builder-1",
				Status:     "healthy",
				Capacity:   4,
				ActiveJobs: id,
				Timestamp:  time.Now(),
			}
			_ = mgr.UpdateBuilderHeartbeat(req)
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for i := 0; i < numGoroutines; i++ {
		<-done
	}
}

// TestBuildStatusArch tests that Arch field is properly handled in BuildStatus.
func TestBuildStatusArch(t *testing.T) {
	t.Parallel()

	cfg := &config.ServerConfig{
		MaxWorkers: 2,
	}

	mgr := NewManager(cfg)

	tests := []struct {
		name         string
		arch         string
		expectedArch string
	}{
		{
			name:         "explicit amd64",
			arch:         "amd64",
			expectedArch: "amd64",
		},
		{
			name:         "empty arch",
			arch:         "",
			expectedArch: "amd64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &BuildRequest{
				PackageName: "dev-lang/python",
				Version:     "3.11",
				Arch:        tt.arch,
			}

			jobID, err := mgr.SubmitBuild(req)
			if err != nil {
				t.Fatalf("SubmitBuild failed: %v", err)
			}

			status, err := mgr.GetStatus(jobID)
			if err != nil {
				t.Fatalf("GetStatus failed: %v", err)
			}

			if status.Arch != tt.expectedArch {
				t.Errorf("Expected Arch=%q, got %q", tt.expectedArch, status.Arch)
			}
		})
	}

	_, err := mgr.SubmitBuild(&BuildRequest{
		PackageName: "dev-lang/python",
		Version:     "3.11",
		Arch:        "arm64",
	})
	if err == nil || !IsRequestError(err) {
		t.Fatalf("architecture outside the catalog should be a request error, got %v", err)
	}
}

// TestLocalBuildRequestArch tests LocalBuildRequest Arch field.
func TestLocalBuildRequestArch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  *LocalBuildRequest
		arch string
	}{
		{
			name: "with arch",
			req: &LocalBuildRequest{
				PackageName: "app-misc/neofetch",
				Version:     "7.1.0",
				Arch:        "amd64",
			},
			arch: "amd64",
		},
		{
			name: "without arch",
			req: &LocalBuildRequest{
				PackageName: "app-misc/neofetch",
				Version:     "7.1.0",
			},
			arch: "",
		},
		{
			name: "arm64 arch",
			req: &LocalBuildRequest{
				PackageName: "app-misc/neofetch",
				Arch:        "arm64",
			},
			arch: "arm64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.req.Arch != tt.arch {
				t.Errorf("Expected Arch=%q, got %q", tt.arch, tt.req.Arch)
			}
		})
	}
}

// TestExtractVersionFromArtifact tests extracting version from artifact path.
func TestExtractVersionFromArtifact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		artifactPath string
		expected     string
	}{
		{
			name:         "standard gpkg format",
			artifactPath: "/var/tmp/portage-artifacts/screenfetch-3.9.9-1.gpkg.tar",
			expected:     "3.9.9",
		},
		{
			name:         "version with revision",
			artifactPath: "/var/tmp/portage-artifacts/neofetch-7.1.0-r1-1.gpkg.tar",
			expected:     "7.1.0-r1",
		},
		{
			name:         "two part version",
			artifactPath: "/var/tmp/portage-artifacts/htop-3.3-1.gpkg.tar",
			expected:     "3.3",
		},
		{
			name:         "complex package name",
			artifactPath: "/var/tmp/portage-artifacts/dev-python-setuptools-69.0.3-1.gpkg.tar",
			expected:     "69.0.3",
		},
		{
			name:         "empty path",
			artifactPath: "",
			expected:     "",
		},
		{
			name:         "no version pattern",
			artifactPath: "/var/tmp/portage-artifacts/somefile.tar",
			expected:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractVersionFromArtifact(tt.artifactPath)
			if result != tt.expected {
				t.Errorf("extractVersionFromArtifact(%q) = %q, want %q", tt.artifactPath, result, tt.expected)
			}
		})
	}
}

// TestCloudBuildRefusesWhenUnconfigured verifies the cloud path fails loudly
// instead of fabricating a completed build (the old simulation behavior) when
// no remote builders and no SSH/callback config are present.
func TestCloudBuildRefusesWhenUnconfigured(t *testing.T) {
	cfg := &config.ServerConfig{MaxWorkers: 1, CloudProvider: "gcp"}
	mgr := NewManager(cfg)

	jobID, err := mgr.SubmitBuild(&BuildRequest{
		PackageName: "dev-lang/python", Version: "3.11", Arch: "amd64", CloudProvider: "gcp",
	})
	if err != nil {
		t.Fatalf("SubmitBuild failed: %v", err)
	}

	// Wait for the worker to process and reach a terminal state.
	deadline := time.Now().Add(3 * time.Second)
	var status *BuildStatus
	for time.Now().Before(deadline) {
		status, err = mgr.GetStatus(jobID)
		if err == nil && status.Status == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}
	if status.Status != "failed" {
		t.Fatalf("expected cloud build to FAIL when unconfigured, got %q", status.Status)
	}
	if !strings.Contains(status.Error, "CLOUD_SSH_KEY_PATH") &&
		!strings.Contains(status.Error, "cloud provider") &&
		!strings.Contains(status.Error, "SERVER_CALLBACK_URL") {
		t.Errorf("expected an explanatory config error, got %q", status.Error)
	}
	// Crucially, it must NOT be marked completed with a fake artifact.
	if status.Status == "completed" || status.ArtifactPath != "" {
		t.Errorf("cloud build fabricated success: status=%q artifact=%q", status.Status, status.ArtifactPath)
	}

	mgr.Shutdown()
}

// TestBuildProvisionRequest checks config → ProvisionRequest mapping and the
// guard errors.
func TestBuildProvisionRequest(t *testing.T) {
	// Missing SSH key → error.
	m := &Manager{config: &config.ServerConfig{CloudProvider: "gcp"}}
	if _, err := m.buildProvisionRequest(&BuildRequest{}); err == nil {
		t.Error("expected error when CLOUD_SSH_KEY_PATH is unset")
	}

	// Missing callback → error.
	m = &Manager{config: &config.ServerConfig{CloudProvider: "gcp", CloudSSHKeyPath: "/k"}}
	if _, err := m.buildProvisionRequest(&BuildRequest{}); err == nil {
		t.Error("expected error when SERVER_CALLBACK_URL is unset")
	}

	// Fully configured → valid request with credentials + TTL.
	m = &Manager{config: &config.ServerConfig{
		CloudProvider: "gcp", CloudSSHKeyPath: "/k", CloudSSHUser: "root",
		ServerCallbackURL: "http://srv:8080", CloudInstanceTTL: 30,
		CloudGCPKeyFile: "/gcp.json",
	}}
	pr, err := m.buildProvisionRequest(&BuildRequest{Arch: "amd64"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pr.Provider != "gcp" || pr.SSH.KeyPath != "/k" || pr.ServerCallback != "http://srv:8080" {
		t.Errorf("unexpected provision request: %+v", pr)
	}
	if pr.TTL != 30*time.Minute {
		t.Errorf("expected TTL 30m, got %v", pr.TTL)
	}
	if pr.Credentials == nil || pr.Credentials.GCPKeyFile != "/gcp.json" {
		t.Errorf("credentials not mapped: %+v", pr.Credentials)
	}

	// URL delivery runs the downloaded binary as root, so a digest is
	// mandatory and is propagated all the way to guest bootstrap.
	m = &Manager{config: &config.ServerConfig{
		CloudProvider: "gcp", CloudSSHKeyPath: "/k", ServerCallbackURL: "http://srv:8080",
		CloudBuilderBinaryURL: "http://10.31.0.2/local/portage-engine/portage-builder-linux-amd64",
	}}
	if _, err := m.buildProvisionRequest(&BuildRequest{Arch: "amd64"}); err == nil || !strings.Contains(err.Error(), "CLOUD_BUILDER_BINARY_SHA256") {
		t.Fatalf("expected missing builder digest to be rejected, got %v", err)
	}
	m = &Manager{config: &config.ServerConfig{
		CloudProvider: "gcp", CloudSSHKeyPath: "/k", ServerCallbackURL: "http://srv:8080",
		CloudBuilderBinaryURL:    "http://10.31.0.2/local/portage-engine/portage-builder-linux-amd64",
		CloudBuilderBinarySHA256: strings.Repeat("a", 64),
	}}
	pr, err = m.buildProvisionRequest(&BuildRequest{Arch: "amd64"})
	if err != nil {
		t.Fatalf("valid URL builder delivery rejected: %v", err)
	}
	if pr.BuilderBinaryURL != m.config.CloudBuilderBinaryURL || pr.BuilderBinarySHA256 != strings.Repeat("a", 64) {
		t.Fatalf("builder URL integrity fields not mapped: %+v", pr)
	}

	m = &Manager{config: &config.ServerConfig{
		CloudProvider: "gcp", CloudSSHKeyPath: "/k", ServerCallbackURL: "http://srv:8080",
		CloudBuilderBinaryURL:    "http://user:pass@10.31.0.2/builder?token=secret",
		CloudBuilderBinarySHA256: strings.Repeat("a", 64),
	}}
	if _, err := m.buildProvisionRequest(&BuildRequest{Arch: "amd64"}); err == nil {
		t.Fatal("expected credentialed/query builder URL to be rejected")
	}
}

// TestConcurrentDuplicateSubmissionsClaimedOnce is the regression test for the
// non-atomic job-claim race: many identical submissions with multiple workers
// must each be processed exactly once, and NO job may be stranded in a
// non-terminal state.
func TestConcurrentDuplicateSubmissionsClaimedOnce(t *testing.T) {
	var mu sync.Mutex
	submitsPerRemoteJob := map[string]int{}

	// A fake remote builder: POST /api/v1/build returns a unique job id and
	// counts submissions; GET /api/v1/jobs/<id> reports it completed.
	var counter int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/build" {
			mu.Lock()
			counter++
			id := "r" + strconv.Itoa(counter)
			submitsPerRemoteJob[id] = 1
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"job_id": id, "status": "queued"})
			return
		}
		// status poll
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "status": "completed", "artifact_url": "/binpkgs/x.gpkg.tar",
		})
	}))
	defer srv.Close()

	cfg := &config.ServerConfig{MaxWorkers: 8, RemoteBuilders: []string{srv.URL}}
	mgr := NewManager(cfg)
	defer mgr.Shutdown()

	// Submit many IDENTICAL requests concurrently — the exact trigger condition.
	const n = 40
	var wg sync.WaitGroup
	jobIDs := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := mgr.SubmitBuild(&BuildRequest{PackageName: "dev-lang/python", Version: "3.11", Arch: "amd64"})
			if err == nil {
				jobIDs[i] = id
			}
		}(i)
	}
	wg.Wait()

	// Give workers time to claim + submit every job. The primary race symptoms
	// are jobs stuck in "queued" (never claimed) or "claimed" (claimed but the
	// worker never advanced it) — check for those once the queue has drained.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if claimedOrQueuedCount(mgr, jobIDs) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// No job may be stranded in "queued" (unclaimed) or "claimed" (claimed but
	// abandoned). "forwarding" is a normal in-flight state (the async poll runs
	// on a multi-second ticker), so it is not a strand.
	for _, id := range jobIDs {
		if id == "" {
			continue
		}
		st, err := mgr.GetStatus(id)
		if err != nil {
			t.Errorf("job %s missing: %v", id, err)
			continue
		}
		if st.Status == "queued" || st.Status == "claimed" {
			t.Errorf("job %s stranded in non-terminal state %q", id, st.Status)
		}
	}

	// Each remote job must have been submitted exactly once (no double-process).
	mu.Lock()
	defer mu.Unlock()
	for id, c := range submitsPerRemoteJob {
		if c != 1 {
			t.Errorf("remote job %s submitted %d times (expected 1)", id, c)
		}
	}
}

// claimedOrQueuedCount counts jobs still stuck in queued/claimed — the states
// that indicate the claim race (unclaimed or claimed-but-abandoned).
func claimedOrQueuedCount(m *Manager, ids []string) int {
	c := 0
	for _, id := range ids {
		if id == "" {
			continue
		}
		st, err := m.GetStatus(id)
		if err != nil {
			continue
		}
		if st.Status == "queued" || st.Status == "claimed" {
			c++
		}
	}
	return c
}

// TestSubmitBuildFullQueueLeavesNoOrphan verifies that when the work queue is
// full, SubmitBuild returns an error AND does not leave a permanently "queued"
// orphan job in the map.
func TestSubmitBuildFullQueueLeavesNoOrphan(t *testing.T) {
	// Zero workers so nothing drains the queue; capacity is 100.
	cfg := &config.ServerConfig{MaxWorkers: 0}
	mgr := NewManager(cfg)
	defer mgr.Shutdown()

	// Fill the queue to capacity.
	accepted := 0
	for i := 0; i < 100; i++ {
		if _, err := mgr.SubmitBuild(&BuildRequest{PackageName: "cat/pkg", Version: "1." + strconv.Itoa(i), Arch: "amd64"}); err == nil {
			accepted++
		}
	}

	before := len(mgr.ListAllBuilds())

	// The next submission must fail (queue full) and must NOT add a job.
	_, err := mgr.SubmitBuild(&BuildRequest{PackageName: "cat/overflow", Version: "1", Arch: "amd64"})
	if err == nil {
		t.Fatal("expected an error when the queue is full")
	}

	after := len(mgr.ListAllBuilds())
	if after != before {
		t.Errorf("full-queue submission left an orphan job: before=%d after=%d", before, after)
	}
}

func TestPVESpecWithDefaultsIncludesOperatorNetworkAndKeepsRequestOverrides(t *testing.T) {
	settings := &config.CloudSettings{
		PVEEndpoint:   "https://pve.internal:8006",
		PVETemplate:   "gentoo-template",
		PVEIPConfig:   "10.31.0.105/24",
		PVEGateway:    "10.31.0.1",
		PVENameserver: "10.31.0.252",
	}

	got := pveSpecWithDefaults(settings, map[string]string{
		"ip_config": "10.31.0.106/24",
	}, "native-gentoo")
	if got["ip_config"] != "10.31.0.106/24" {
		t.Fatalf("request override lost: %q", got["ip_config"])
	}
	if got["gateway"] != "10.31.0.1" || got["nameserver"] != "10.31.0.252" {
		t.Fatalf("operator network defaults missing: gateway=%q nameserver=%q", got["gateway"], got["nameserver"])
	}
	if got["bios"] != "ovmf" || got["machine"] != "q35" || got["disk_type"] != "scsi" {
		t.Fatalf("native Gentoo defaults missing: %#v", got)
	}
}
