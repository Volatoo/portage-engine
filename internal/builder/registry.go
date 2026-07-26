// Package builder provides builder registration and monitoring.
package builder

import (
	"sync"
	"time"
)

// BuilderInfo represents information about a registered builder.
// nolint:revive // BuilderInfo is intentionally named for clarity
type BuilderInfo struct {
	ID            string    `json:"id"`
	Endpoint      string    `json:"endpoint"`
	Architecture  string    `json:"architecture"`
	Status        string    `json:"status"`       // online, offline, busy, draining
	Capacity      int       `json:"capacity"`     // max concurrent builds
	CurrentLoad   int       `json:"current_load"` // current active builds
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Enabled       bool      `json:"enabled"`
	CPUUsage      float64   `json:"cpu_usage"`      // percentage
	MemoryUsage   float64   `json:"memory_usage"`   // percentage
	DiskUsage     float64   `json:"disk_usage"`     // percentage
	TotalBuilds   int       `json:"total_builds"`   // lifetime total
	SuccessBuilds int       `json:"success_builds"` // lifetime successes
	FailedBuilds  int       `json:"failed_builds"`  // lifetime failures
}

// Registry manages registered builders and their status.
type Registry struct {
	mu               sync.RWMutex
	builders         map[string]*BuilderInfo
	heartbeatTimeout time.Duration
	cleanupInterval  time.Duration
	stopCleanup      chan struct{}
}

// NewRegistry creates a new builder registry.
func NewRegistry(heartbeatTimeout, cleanupInterval time.Duration) *Registry {
	r := &Registry{
		builders:         make(map[string]*BuilderInfo),
		heartbeatTimeout: heartbeatTimeout,
		cleanupInterval:  cleanupInterval,
		stopCleanup:      make(chan struct{}),
	}

	// Start cleanup goroutine
	go r.cleanupStaleBuilders()

	return r
}

// Register registers or updates a builder.
func (r *Registry) Register(info *BuilderInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, exists := r.builders[info.ID]
	if exists {
		// Update existing builder
		existing.Endpoint = info.Endpoint
		existing.Architecture = info.Architecture
		existing.Status = info.Status
		existing.Capacity = info.Capacity
		existing.CurrentLoad = info.CurrentLoad
		existing.LastHeartbeat = time.Now()
		existing.CPUUsage = info.CPUUsage
		existing.MemoryUsage = info.MemoryUsage
		existing.DiskUsage = info.DiskUsage
		if info.TotalBuilds > 0 {
			existing.TotalBuilds = info.TotalBuilds
		}
		if info.SuccessBuilds > 0 {
			existing.SuccessBuilds = info.SuccessBuilds
		}
		if info.FailedBuilds > 0 {
			existing.FailedBuilds = info.FailedBuilds
		}
	} else {
		// Register new builder
		info.LastHeartbeat = time.Now()
		if !info.Enabled {
			info.Enabled = true // Default to enabled
		}
		r.builders[info.ID] = info
	}
}

// Unregister removes a builder from the registry.
func (r *Registry) Unregister(builderID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.builders, builderID)
}

// Get retrieves a builder by ID.
func (r *Registry) Get(builderID string) (*BuilderInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	builder, exists := r.builders[builderID]
	if !exists {
		return nil, false
	}
	// Return a copy to avoid concurrent access issues
	builderCopy := *builder
	return &builderCopy, true
}

// List returns all registered builders.
func (r *Registry) List() []*BuilderInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	builders := make([]*BuilderInfo, 0, len(r.builders))
	for _, builder := range r.builders {
		// Return copies to avoid concurrent access issues
		builderCopy := *builder
		builders = append(builders, &builderCopy)
	}
	return builders
}

// UpdateStatus updates the status of a builder.
func (r *Registry) UpdateStatus(builderID, status string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	builder, exists := r.builders[builderID]
	if !exists {
		return false
	}
	builder.Status = status
	builder.LastHeartbeat = time.Now()
	return true
}

// UpdateLoad updates the current load of a builder.
func (r *Registry) UpdateLoad(builderID string, load int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	builder, exists := r.builders[builderID]
	if !exists {
		return false
	}
	builder.CurrentLoad = load
	builder.LastHeartbeat = time.Now()
	return true
}

// UpdateMetrics updates system metrics for a builder.
func (r *Registry) UpdateMetrics(builderID string, cpu, memory, disk float64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	builder, exists := r.builders[builderID]
	if !exists {
		return false
	}
	builder.CPUUsage = cpu
	builder.MemoryUsage = memory
	builder.DiskUsage = disk
	builder.LastHeartbeat = time.Now()
	return true
}

// IncrementBuilds increments build counters for a builder.
func (r *Registry) IncrementBuilds(builderID string, success bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	builder, exists := r.builders[builderID]
	if !exists {
		return false
	}
	builder.TotalBuilds++
	if success {
		builder.SuccessBuilds++
	} else {
		builder.FailedBuilds++
	}
	return true
}

// Enable enables or disables a builder.
func (r *Registry) Enable(builderID string, enabled bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	builder, exists := r.builders[builderID]
	if !exists {
		return false
	}
	builder.Enabled = enabled
	return true
}

// GetStats returns aggregate statistics for all builders.
func (r *Registry) GetStats() map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()

	totalBuilders := len(r.builders)
	onlineBuilders := 0
	offlineBuilders := 0
	drainingBuilders := 0
	totalCapacity := 0
	totalLoad := 0
	totalBuilds := 0
	totalSuccess := 0
	totalFailed := 0

	for _, builder := range r.builders {
		if builder.Enabled {
			switch builder.Status {
			case "online", "busy":
				onlineBuilders++
			case "draining":
				drainingBuilders++
			default:
				offlineBuilders++
			}
		}
		totalCapacity += builder.Capacity
		totalLoad += builder.CurrentLoad
		totalBuilds += builder.TotalBuilds
		totalSuccess += builder.SuccessBuilds
		totalFailed += builder.FailedBuilds
	}

	successRate := 0.0
	if totalBuilds > 0 {
		successRate = float64(totalSuccess) / float64(totalBuilds) * 100
	}

	return map[string]interface{}{
		"total_builders":    totalBuilders,
		"online_builders":   onlineBuilders,
		"offline_builders":  offlineBuilders,
		"draining_builders": drainingBuilders,
		"total_capacity":    totalCapacity,
		"total_load":        totalLoad,
		"total_builds":      totalBuilds,
		"success_builds":    totalSuccess,
		"failed_builds":     totalFailed,
		"success_rate":      successRate,
	}
}

// cleanupStaleBuilders marks builders as offline if they haven't sent heartbeat.
func (r *Registry) cleanupStaleBuilders() {
	ticker := time.NewTicker(r.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.checkStaleBuilders()
		case <-r.stopCleanup:
			return
		}
	}
}

// checkStaleBuilders checks for builders that haven't sent heartbeat recently.
func (r *Registry) checkStaleBuilders() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	for _, builder := range r.builders {
		if now.Sub(builder.LastHeartbeat) > r.heartbeatTimeout {
			builder.Status = "offline"
		}
	}
}

// Close stops the registry cleanup goroutine.
func (r *Registry) Close() {
	close(r.stopCleanup)
}
