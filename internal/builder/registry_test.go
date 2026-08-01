package builder

import (
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/metrics"
)

func TestNewRegistry(t *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)
	if r == nil {
		t.Fatal("NewRegistry returned nil")
	}
	defer r.Close()

	if r.builders == nil {
		t.Error("builders map not initialized")
	}
	if r.heartbeatTimeout != 30*time.Second {
		t.Errorf("heartbeatTimeout = %v, want %v", r.heartbeatTimeout, 30*time.Second)
	}
	if r.cleanupInterval != 10*time.Second {
		t.Errorf("cleanupInterval = %v, want %v", r.cleanupInterval, 10*time.Second)
	}
}

func TestRegister(t *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)
	defer r.Close()

	tests := []struct {
		name string
		info *BuilderInfo
	}{
		{
			name: "new builder",
			info: &BuilderInfo{
				ID:           "builder-1",
				Endpoint:     "http://localhost:9090",
				Architecture: "amd64",
				Status:       "online",
				Capacity:     4,
				CurrentLoad:  2,
				Enabled:      true,
			},
		},
		{
			name: "builder with metrics",
			info: &BuilderInfo{
				ID:           "builder-2",
				Endpoint:     "http://localhost:9091",
				Architecture: "arm64",
				Status:       "online",
				Capacity:     2,
				CurrentLoad:  1,
				CPUUsage:     45.5,
				MemoryUsage:  60.2,
				DiskUsage:    70.8,
				Enabled:      true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r.Register(tt.info)

			builder, exists := r.Get(tt.info.ID)
			if !exists {
				t.Fatalf("builder %s not found after registration", tt.info.ID)
			}
			if builder.ID != tt.info.ID {
				t.Errorf("ID = %s, want %s", builder.ID, tt.info.ID)
			}
			if builder.Endpoint != tt.info.Endpoint {
				t.Errorf("Endpoint = %s, want %s", builder.Endpoint, tt.info.Endpoint)
			}
			if builder.Architecture != tt.info.Architecture {
				t.Errorf("Architecture = %s, want %s", builder.Architecture, tt.info.Architecture)
			}
			if builder.LastHeartbeat.IsZero() {
				t.Error("LastHeartbeat not set")
			}
		})
	}
}

func TestRegisterUpdate(t *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)
	defer r.Close()

	// Register initial builder
	info := &BuilderInfo{
		ID:           "builder-1",
		Endpoint:     "http://localhost:9090",
		Architecture: "amd64",
		Status:       "online",
		Capacity:     4,
		CurrentLoad:  2,
		TotalBuilds:  10,
	}
	r.Register(info)

	// Update the same builder
	updatedInfo := &BuilderInfo{
		ID:           "builder-1",
		Endpoint:     "http://localhost:9091",
		Architecture: "amd64",
		Status:       "busy",
		Capacity:     4,
		CurrentLoad:  3,
		CPUUsage:     80.0,
		TotalBuilds:  15,
	}
	r.Register(updatedInfo)

	builder, exists := r.Get("builder-1")
	if !exists {
		t.Fatal("builder not found")
	}
	if builder.Endpoint != "http://localhost:9091" {
		t.Errorf("Endpoint = %s, want http://localhost:9091", builder.Endpoint)
	}
	if builder.Status != "busy" {
		t.Errorf("Status = %s, want busy", builder.Status)
	}
	if builder.CurrentLoad != 3 {
		t.Errorf("CurrentLoad = %d, want 3", builder.CurrentLoad)
	}
	if builder.CPUUsage != 80.0 {
		t.Errorf("CPUUsage = %f, want 80.0", builder.CPUUsage)
	}
	if builder.TotalBuilds != 15 {
		t.Errorf("TotalBuilds = %d, want 15", builder.TotalBuilds)
	}
}

func TestUnregister(t *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)
	defer r.Close()

	info := &BuilderInfo{
		ID:       "builder-1",
		Endpoint: "http://localhost:9090",
	}
	r.Register(info)

	// Verify registered
	if _, exists := r.Get("builder-1"); !exists {
		t.Fatal("builder not registered")
	}

	// Unregister
	r.Unregister("builder-1")

	// Verify unregistered
	if _, exists := r.Get("builder-1"); exists {
		t.Error("builder still exists after unregister")
	}
}

func TestGet(t *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)
	defer r.Close()

	// Test non-existent builder
	if _, exists := r.Get("nonexistent"); exists {
		t.Error("Get returned true for non-existent builder")
	}

	// Register and test
	info := &BuilderInfo{
		ID:           "builder-1",
		Endpoint:     "http://localhost:9090",
		Architecture: "amd64",
	}
	r.Register(info)

	builder, exists := r.Get("builder-1")
	if !exists {
		t.Fatal("Get returned false for registered builder")
	}
	if builder.ID != "builder-1" {
		t.Errorf("ID = %s, want builder-1", builder.ID)
	}
}

func TestList(t *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)
	defer r.Close()

	// Test empty list
	builders := r.List()
	if len(builders) != 0 {
		t.Errorf("List() returned %d builders, want 0", len(builders))
	}

	// Register multiple builders
	for i := 1; i <= 3; i++ {
		info := &BuilderInfo{
			ID:       "builder-" + string(rune('0'+i)),
			Endpoint: "http://localhost:909" + string(rune('0'+i)),
		}
		r.Register(info)
	}

	builders = r.List()
	if len(builders) != 3 {
		t.Errorf("List() returned %d builders, want 3", len(builders))
	}
}

func TestUpdateStatus(t *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)
	defer r.Close()

	info := &BuilderInfo{
		ID:     "builder-1",
		Status: "online",
	}
	r.Register(info)

	// Update status
	if !r.UpdateStatus("builder-1", "busy") {
		t.Error("UpdateStatus returned false for existing builder")
	}

	builder, _ := r.Get("builder-1")
	if builder.Status != "busy" {
		t.Errorf("Status = %s, want busy", builder.Status)
	}

	// Test non-existent builder
	if r.UpdateStatus("nonexistent", "online") {
		t.Error("UpdateStatus returned true for non-existent builder")
	}
}

func TestUpdateLoad(t *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)
	defer r.Close()

	info := &BuilderInfo{
		ID:          "builder-1",
		CurrentLoad: 2,
	}
	r.Register(info)

	// Update load
	if !r.UpdateLoad("builder-1", 5) {
		t.Error("UpdateLoad returned false for existing builder")
	}

	builder, _ := r.Get("builder-1")
	if builder.CurrentLoad != 5 {
		t.Errorf("CurrentLoad = %d, want 5", builder.CurrentLoad)
	}

	// Test non-existent builder
	if r.UpdateLoad("nonexistent", 3) {
		t.Error("UpdateLoad returned true for non-existent builder")
	}
}

func TestUpdateMetrics(t *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)
	defer r.Close()

	info := &BuilderInfo{
		ID: "builder-1",
	}
	r.Register(info)

	// Update metrics
	if !r.UpdateMetrics("builder-1", 75.5, 80.2, 60.8) {
		t.Error("UpdateMetrics returned false for existing builder")
	}

	builder, _ := r.Get("builder-1")
	if builder.CPUUsage != 75.5 {
		t.Errorf("CPUUsage = %f, want 75.5", builder.CPUUsage)
	}
	if builder.MemoryUsage != 80.2 {
		t.Errorf("MemoryUsage = %f, want 80.2", builder.MemoryUsage)
	}
	if builder.DiskUsage != 60.8 {
		t.Errorf("DiskUsage = %f, want 60.8", builder.DiskUsage)
	}

	// Test non-existent builder
	if r.UpdateMetrics("nonexistent", 50.0, 60.0, 70.0) {
		t.Error("UpdateMetrics returned true for non-existent builder")
	}
}

func TestIncrementBuilds(t *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)
	defer r.Close()

	info := &BuilderInfo{
		ID:            "builder-1",
		TotalBuilds:   10,
		SuccessBuilds: 8,
		FailedBuilds:  2,
	}
	r.Register(info)

	// Increment success
	if !r.IncrementBuilds("builder-1", true) {
		t.Error("IncrementBuilds returned false for existing builder")
	}

	builder, _ := r.Get("builder-1")
	if builder.TotalBuilds != 11 {
		t.Errorf("TotalBuilds = %d, want 11", builder.TotalBuilds)
	}
	if builder.SuccessBuilds != 9 {
		t.Errorf("SuccessBuilds = %d, want 9", builder.SuccessBuilds)
	}

	// Increment failure
	r.IncrementBuilds("builder-1", false)

	builder, _ = r.Get("builder-1")
	if builder.TotalBuilds != 12 {
		t.Errorf("TotalBuilds = %d, want 12", builder.TotalBuilds)
	}
	if builder.FailedBuilds != 3 {
		t.Errorf("FailedBuilds = %d, want 3", builder.FailedBuilds)
	}

	// Test non-existent builder
	if r.IncrementBuilds("nonexistent", true) {
		t.Error("IncrementBuilds returned true for non-existent builder")
	}
}

func TestEnable(t *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)
	defer r.Close()

	info := &BuilderInfo{
		ID:      "builder-1",
		Enabled: true,
	}
	r.Register(info)

	// Disable
	if !r.Enable("builder-1", false) {
		t.Error("Enable returned false for existing builder")
	}

	builder, _ := r.Get("builder-1")
	if builder.Enabled {
		t.Error("Enabled = true, want false")
	}

	// Enable
	r.Enable("builder-1", true)

	builder, _ = r.Get("builder-1")
	if !builder.Enabled {
		t.Error("Enabled = false, want true")
	}

	// Test non-existent builder
	if r.Enable("nonexistent", true) {
		t.Error("Enable returned true for non-existent builder")
	}
}

func TestGetStats(t *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)
	defer r.Close()

	// Register multiple builders with different states
	builders := []*BuilderInfo{
		{
			ID:            "builder-1",
			Status:        "online",
			Enabled:       true,
			Capacity:      4,
			CurrentLoad:   2,
			TotalBuilds:   100,
			SuccessBuilds: 90,
			FailedBuilds:  10,
		},
		{
			ID:            "builder-2",
			Status:        "busy",
			Enabled:       true,
			Capacity:      2,
			CurrentLoad:   2,
			TotalBuilds:   50,
			SuccessBuilds: 45,
			FailedBuilds:  5,
		},
		{
			ID:            "builder-3",
			Status:        "offline",
			Enabled:       true,
			Capacity:      4,
			CurrentLoad:   0,
			TotalBuilds:   30,
			SuccessBuilds: 25,
			FailedBuilds:  5,
		},
		{
			ID:            "builder-4",
			Status:        "draining",
			Enabled:       true,
			Capacity:      0,
			CurrentLoad:   0,
			TotalBuilds:   1,
			SuccessBuilds: 1,
		},
	}

	for _, b := range builders {
		r.Register(b)
	}

	stats := r.GetStats()

	if stats["total_builders"] != 4 {
		t.Errorf("total_builders = %v, want 4", stats["total_builders"])
	}
	if stats["online_builders"] != 2 {
		t.Errorf("online_builders = %v, want 2", stats["online_builders"])
	}
	if stats["offline_builders"] != 1 {
		t.Errorf("offline_builders = %v, want 1", stats["offline_builders"])
	}
	if stats["draining_builders"] != 1 {
		t.Errorf("draining_builders = %v, want 1", stats["draining_builders"])
	}
	if stats["total_capacity"] != 10 {
		t.Errorf("total_capacity = %v, want 10", stats["total_capacity"])
	}
	if stats["total_load"] != 4 {
		t.Errorf("total_load = %v, want 4", stats["total_load"])
	}
	if stats["total_builds"] != 181 {
		t.Errorf("total_builds = %v, want 181", stats["total_builds"])
	}
	if stats["success_builds"] != 161 {
		t.Errorf("success_builds = %v, want 161", stats["success_builds"])
	}
	if stats["failed_builds"] != 20 {
		t.Errorf("failed_builds = %v, want 20", stats["failed_builds"])
	}

	// Success rate should include the completed draining builder.
	successRate := stats["success_rate"].(float64)
	expectedSuccessRate := float64(161) / float64(181) * 100
	if successRate != expectedSuccessRate {
		t.Errorf("success_rate = %f, want %f", successRate, expectedSuccessRate)
	}
}

func TestCleanupStaleBuilders(t *testing.T) {
	// Use short timeout for testing
	r := NewRegistry(100*time.Millisecond, 50*time.Millisecond)
	defer r.Close()

	info := &BuilderInfo{
		ID:     "builder-1",
		Status: "online",
	}
	r.Register(info)

	// Verify online
	builder, _ := r.Get("builder-1")
	if builder.Status != "online" {
		t.Errorf("Status = %s, want online", builder.Status)
	}

	// Wait for cleanup to mark as offline
	time.Sleep(200 * time.Millisecond)

	builder, _ = r.Get("builder-1")
	if builder.Status != "offline" {
		t.Errorf("Status = %s, want offline after timeout", builder.Status)
	}
}

// TestRegistryPublishesExecutorInventoryGauges pins the builder gauges to the
// heartbeat inventory. They previously had no writer at all, so the builder
// panels reported an empty cluster while every executor was healthy.
func TestRegistryPublishesExecutorInventoryGauges(t *testing.T) {
	registry := metrics.New(&metrics.Config{Enabled: true})
	r := NewRegistry(100*time.Millisecond, 50*time.Millisecond)
	defer r.Close()

	r.Register(&BuilderInfo{ID: "builder-1", Status: "online", Capacity: 4})
	r.Register(&BuilderInfo{ID: "builder-2", Status: "offline", Capacity: 2})
	// An offline executor takes no work and lends no capacity, so it belongs in
	// none of the three gauges — only in the total the operator listing shows.
	snapshot := registry.GetSnapshot()
	if snapshot["builders_active"].(int64) != 1 ||
		snapshot["builders_healthy"].(int64) != 1 ||
		snapshot["builder_capacity"].(int64) != 4 {
		t.Fatalf("executor inventory gauges = %+v", snapshot)
	}

	r.Unregister("builder-1")
	snapshot = registry.GetSnapshot()
	if snapshot["builders_active"].(int64) != 0 ||
		snapshot["builder_capacity"].(int64) != 0 {
		t.Fatalf("gauges kept an unregistered executor: %+v", snapshot)
	}
}

// TestRegistryInventoryGaugesDropAStaleExecutor covers the transition the
// heartbeat timeout actually performs. checkStaleBuilders marks a silent
// builder offline in place — it neither disables nor removes the entry — so
// gauges keyed only on Enabled never fall, and the "Active Builders" stat stays
// above its red threshold long after the last executor stopped answering.
func TestRegistryInventoryGaugesDropAStaleExecutor(t *testing.T) {
	registry := metrics.New(&metrics.Config{Enabled: true})
	r := NewRegistry(time.Minute, time.Hour)
	defer r.Close()

	r.Register(&BuilderInfo{ID: "builder-1", Status: "online", Capacity: 4})
	snapshot := registry.GetSnapshot()
	if snapshot["builders_active"].(int64) != 1 ||
		snapshot["builder_capacity"].(int64) != 4 {
		t.Fatalf("gauges did not follow a live executor: %+v", snapshot)
	}

	// Age the heartbeat rather than sleeping the cleanup interval away, so the
	// timeout transition is exercised without a timing-dependent test.
	r.mu.Lock()
	r.builders["builder-1"].LastHeartbeat = time.Now().Add(-2 * time.Minute)
	r.mu.Unlock()
	r.checkStaleBuilders()

	stale, exists := r.Get("builder-1")
	if !exists || stale.Status != "offline" || !stale.Enabled {
		t.Fatalf("stale executor was removed or disabled instead of marked offline: %+v", stale)
	}
	snapshot = registry.GetSnapshot()
	if snapshot["builders_active"].(int64) != 0 ||
		snapshot["builders_healthy"].(int64) != 0 ||
		snapshot["builder_capacity"].(int64) != 0 {
		t.Fatalf("gauges still count a timed-out executor: %+v", snapshot)
	}
}

func TestConcurrentAccess(_ *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)
	defer r.Close()

	done := make(chan bool)

	// Concurrent registrations
	go func() {
		for i := 0; i < 100; i++ {
			info := &BuilderInfo{
				ID:       "builder-1",
				Endpoint: "http://localhost:9090",
				Status:   "online",
			}
			r.Register(info)
		}
		done <- true
	}()

	// Concurrent reads
	go func() {
		for i := 0; i < 100; i++ {
			_, _ = r.Get("builder-1")
			_ = r.List()
		}
		done <- true
	}()

	// Concurrent updates
	go func() {
		for i := 0; i < 100; i++ {
			_ = r.UpdateStatus("builder-1", "busy")
			_ = r.UpdateLoad("builder-1", i)
		}
		done <- true
	}()

	// Wait for all goroutines
	<-done
	<-done
	<-done
}

func TestClose(t *testing.T) {
	r := NewRegistry(30*time.Second, 10*time.Second)

	// Register a builder
	info := &BuilderInfo{
		ID:     "builder-1",
		Status: "online",
	}
	r.Register(info)

	// Close should not panic
	r.Close()

	// Further operations should still work (just cleanup goroutine stopped)
	builder, exists := r.Get("builder-1")
	if !exists {
		t.Error("Get failed after Close")
	}
	if builder.ID != "builder-1" {
		t.Errorf("ID = %s, want builder-1", builder.ID)
	}
}
