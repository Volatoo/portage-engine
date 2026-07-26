package server

import (
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/internal/catalog"
)

func TestServerStore(t *testing.T) {
	tmpDir := t.TempDir()

	store, err := NewServerStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create server store: %v", err)
	}

	// Test loading from empty store
	jobs, err := store.LoadJobs()
	if err != nil {
		t.Fatalf("Failed to load jobs from empty store: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("Expected 0 jobs, got %d", len(jobs))
	}

	// Test saving and loading jobs
	testJobs := map[string]*builder.BuildStatus{
		"job-1": {
			JobID:       "job-1",
			Status:      "completed",
			PackageName: "dev-lang/python",
			Version:     "3.11",
			CreatedAt:   time.Now().Add(-time.Hour),
			UpdatedAt:   time.Now(),
			ResolvedContext: &catalog.ResolvedBuildContext{
				CatalogVersion: 1, ProfileID: "pe/amd64/base", ImageID: "pe/base-g1",
				ImageGeneration: "g1", MirrorBundleID: "mirror/g1",
				Repositories: []catalog.ResolvedRepository{{ID: "gentoo", Revision: "0123456789abcdef0123456789abcdef01234567"}},
			},
		},
		"job-2": {
			JobID:       "job-2",
			Status:      "building",
			PackageName: "sys-apps/systemd",
			Version:     "254",
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		},
	}

	if err := store.SaveJobs(testJobs); err != nil {
		t.Fatalf("Failed to save jobs: %v", err)
	}

	// Load and verify
	loaded, err := store.LoadJobs()
	if err != nil {
		t.Fatalf("Failed to load jobs: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("Expected 2 jobs, got %d", len(loaded))
	}
	if loaded["job-1"].PackageName != "dev-lang/python" {
		t.Errorf("Expected package dev-lang/python, got %s", loaded["job-1"].PackageName)
	}
	if loaded["job-2"].Status != "building" {
		t.Errorf("Expected status building, got %s", loaded["job-2"].Status)
	}
	if got := loaded["job-1"].ResolvedContext; got == nil || got.ImageGeneration != "g1" || len(got.Repositories) != 1 {
		t.Fatalf("resolved provenance was not persisted: %+v", got)
	}
}

func TestServerStoreCleanOldJobs(t *testing.T) {
	tmpDir := t.TempDir()

	store, err := NewServerStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create server store: %v", err)
	}

	now := time.Now()
	jobs := map[string]*builder.BuildStatus{
		"old-completed": {
			JobID:     "old-completed",
			Status:    "completed",
			UpdatedAt: now.Add(-48 * time.Hour),
		},
		"recent-completed": {
			JobID:     "recent-completed",
			Status:    "completed",
			UpdatedAt: now.Add(-1 * time.Hour),
		},
		"in-progress": {
			JobID:     "in-progress",
			Status:    "building",
			UpdatedAt: now.Add(-48 * time.Hour), // Old but in-progress
		},
	}

	cleaned, removed := store.CleanOldJobs(jobs, 24*time.Hour)
	if removed != 1 {
		t.Errorf("Expected 1 removed, got %d", removed)
	}
	if len(cleaned) != 2 {
		t.Errorf("Expected 2 remaining, got %d", len(cleaned))
	}
	if _, ok := cleaned["old-completed"]; ok {
		t.Error("Old completed job should have been removed")
	}
	if _, ok := cleaned["in-progress"]; !ok {
		t.Error("In-progress job should have been kept regardless of age")
	}
}

func TestServerPersister(t *testing.T) {
	tmpDir := t.TempDir()

	store, err := NewServerStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create server store: %v", err)
	}

	jobs := map[string]*builder.BuildStatus{
		"test-job": {
			JobID:       "test-job",
			Status:      "queued",
			PackageName: "dev-libs/openssl",
			UpdatedAt:   time.Now(),
		},
	}

	getJobs := func() map[string]*builder.BuildStatus {
		return jobs
	}

	persister := NewServerPersister(store, getJobs, 100*time.Millisecond, 0)
	persister.Start()

	// Wait for at least one save cycle
	time.Sleep(250 * time.Millisecond)

	persister.Stop()

	// Verify the data was saved
	loaded, err := store.LoadJobs()
	if err != nil {
		t.Fatalf("Failed to load jobs after persistence: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("Expected 1 job, got %d", len(loaded))
	}
	if loaded["test-job"].PackageName != "dev-libs/openssl" {
		t.Errorf("Expected dev-libs/openssl, got %s", loaded["test-job"].PackageName)
	}
}
