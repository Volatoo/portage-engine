package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/slchris/portage-engine/internal/builder"
)

// handleBuilderRegister handles builder registration requests.
func (s *Server) handleBuilderRegister(w http.ResponseWriter, r *http.Request) {
	s.metrics.IncHTTPRequests()

	if r.Method != http.MethodPost {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var info builder.BuilderInfo
	if err := json.NewDecoder(r.Body).Decode(&info); err != nil {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Register the builder
	s.builderRegistry.Register(&info)

	response := map[string]interface{}{
		"success": true,
		"message": "Builder registered successfully",
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// handleBuildersList returns the list of all registered builders.
func (s *Server) handleBuildersList(w http.ResponseWriter, r *http.Request) {
	s.metrics.IncHTTPRequests()

	if r.Method != http.MethodGet {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	builders := s.builderRegistry.List()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(builders)
}

// handleBuildersStatus returns aggregate status and statistics for all builders.
func (s *Server) handleBuildersStatus(w http.ResponseWriter, r *http.Request) {
	s.metrics.IncHTTPRequests()

	if r.Method != http.MethodGet {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Fetch real-time status from all configured remote builders
	builders := s.fetchAllBuilderStatus()

	// Calculate aggregate statistics
	stats := calculateBuilderStats(builders)

	response := map[string]interface{}{
		"stats":    stats,
		"builders": builders,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// BuilderStatusInfo represents status information from a builder.
type BuilderStatusInfo struct {
	ID              string  `json:"id"`
	Endpoint        string  `json:"endpoint"`
	Architecture    string  `json:"architecture"`
	Status          string  `json:"status"`
	Capacity        int     `json:"capacity"`
	CurrentLoad     int     `json:"current_load"`
	Enabled         bool    `json:"enabled"`
	CPUUsage        float64 `json:"cpu_usage"`
	MemoryUsage     float64 `json:"memory_usage"`
	DiskUsage       float64 `json:"disk_usage"`
	TotalBuilds     int     `json:"total_builds"`
	SuccessBuilds   int     `json:"success_builds"`
	FailedBuilds    int     `json:"failed_builds"`
	AcceptingBuilds bool    `json:"accepting_builds"`
	NativeJobPolicy string  `json:"native_job_policy,omitempty"`
}

// fetchAllBuilderStatus queries all configured remote builders for their status.
func (s *Server) fetchAllBuilderStatus() []BuilderStatusInfo {
	remoteBuilders := s.builder.CloudSettings().RemoteBuilders
	if len(remoteBuilders) == 0 {
		return nil
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		builders []BuilderStatusInfo
	)

	client := &http.Client{Timeout: 5 * time.Second}

	for _, addr := range remoteBuilders {
		wg.Add(1)
		go func(address string) {
			defer wg.Done()

			baseURL := normalizeBuilderURL(address)
			url := fmt.Sprintf("%s/api/v1/status", baseURL)
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err == nil && s.config.BuilderToken != "" {
				req.Header.Set("X-API-Key", s.config.BuilderToken)
			}
			if err != nil {
				log.Printf("Failed to construct builder status request for %s: %v", address, err)
				return
			}
			resp, err := client.Do(req) // nolint:gosec // Internal service URL from operator config.
			if err != nil {
				log.Printf("Failed to query builder %s: %v", address, err)
				// Add offline entry for unreachable builder
				mu.Lock()
				builders = append(builders, BuilderStatusInfo{
					ID:              address,
					Endpoint:        baseURL,
					Status:          "offline",
					Enabled:         false,
					AcceptingBuilds: false,
				})
				mu.Unlock()
				return
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				log.Printf("Builder %s returned status %d", address, resp.StatusCode)
				mu.Lock()
				builders = append(builders, BuilderStatusInfo{
					ID:              address,
					Endpoint:        baseURL,
					Status:          "error",
					Enabled:         false,
					AcceptingBuilds: false,
				})
				mu.Unlock()
				return
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("Failed to read response from builder %s: %v", address, err)
				return
			}

			var status map[string]interface{}
			if err := json.Unmarshal(body, &status); err != nil {
				log.Printf("Failed to parse response from builder %s: %v", address, err)
				return
			}

			info := BuilderStatusInfo{
				ID:            getStringValue(status, "instance_id", address),
				Endpoint:      baseURL,
				Architecture:  getStringValue(status, "architecture", "unknown"),
				Status:        getStringValue(status, "status", "online"),
				Capacity:      getIntValue(status, "capacity", 0),
				CurrentLoad:   getIntValue(status, "current_load", 0),
				Enabled:       getBoolValue(status, "enabled", true),
				CPUUsage:      getFloatValue(status, "cpu_usage", 0),
				MemoryUsage:   getFloatValue(status, "memory_usage", 0),
				DiskUsage:     getFloatValue(status, "disk_usage", 0),
				TotalBuilds:   getIntValue(status, "total_builds", 0),
				SuccessBuilds: getIntValue(status, "success_builds", 0),
				FailedBuilds:  getIntValue(status, "failed_builds", 0),
				AcceptingBuilds: getBoolValue(status, "accepting_builds",
					getStringValue(status, "status", "online") != "draining"),
				NativeJobPolicy: getStringValue(status, "native_job_policy", ""),
			}

			mu.Lock()
			builders = append(builders, info)
			mu.Unlock()
		}(addr)
	}

	wg.Wait()
	return builders
}

// calculateBuilderStats calculates aggregate statistics from builder list.
func calculateBuilderStats(builders []BuilderStatusInfo) map[string]interface{} {
	totalBuilders := len(builders)
	onlineBuilders := 0
	offlineBuilders := 0
	drainingBuilders := 0
	totalCapacity := 0
	totalLoad := 0
	totalBuilds := 0
	totalSuccess := 0
	totalFailed := 0

	for _, b := range builders {
		switch b.Status {
		case "online", "busy":
			onlineBuilders++
		case "draining":
			drainingBuilders++
		default:
			offlineBuilders++
		}
		totalCapacity += b.Capacity
		totalLoad += b.CurrentLoad
		totalBuilds += b.TotalBuilds
		totalSuccess += b.SuccessBuilds
		totalFailed += b.FailedBuilds
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

// handleHeartbeat handles builder heartbeat requests.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	s.metrics.IncHTTPRequests()
	s.metrics.IncHeartbeatsTotal()

	if r.Method != http.MethodPost {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req builder.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.metrics.IncHTTPRequestErrors()
		s.metrics.IncHeartbeatsFailed()
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Update builder registry with heartbeat info
	builderInfo := &builder.BuilderInfo{
		ID:          req.BuilderID,
		Endpoint:    req.Endpoint,
		Status:      req.Status,
		Capacity:    req.Capacity,
		CurrentLoad: req.ActiveJobs,
	}
	s.builderRegistry.Register(builderInfo)

	// Update builder heartbeat in the scheduler
	if err := s.builder.UpdateBuilderHeartbeat(&req); err != nil {
		s.metrics.IncHeartbeatsFailed()
		response := builder.HeartbeatResponse{
			Success: false,
			Message: err.Error(),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(response)
		return
	}

	response := builder.HeartbeatResponse{
		Success: true,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// Helper functions for type conversion from map[string]interface{}

// normalizeBuilderURL ensures the builder address has the correct URL format.
func normalizeBuilderURL(address string) string {
	if strings.HasPrefix(address, "http://") || strings.HasPrefix(address, "https://") {
		return address
	}
	return fmt.Sprintf("http://%s", address)
}

func getStringValue(m map[string]interface{}, key, defaultVal string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return defaultVal
}

func getIntValue(m map[string]interface{}, key string, defaultVal int) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		}
	}
	return defaultVal
}

func getFloatValue(m map[string]interface{}, key string, defaultVal float64) float64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		case int64:
			return float64(n)
		}
	}
	return defaultVal
}

func getBoolValue(m map[string]interface{}, key string, defaultVal bool) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return defaultVal
}
