// Package builder provides local and remote build capabilities.
package builder

import (
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SystemInfo represents system resource usage.
type SystemInfo struct {
	CPUUsage    float64 `json:"cpu_usage"`    // percentage (0-100)
	MemoryUsage float64 `json:"memory_usage"` // percentage (0-100)
	DiskUsage   float64 `json:"disk_usage"`   // percentage (0-100)
	CPUCount    int     `json:"cpu_count"`
	MemoryTotal uint64  `json:"memory_total"` // bytes
	MemoryUsed  uint64  `json:"memory_used"`  // bytes
	DiskTotal   uint64  `json:"disk_total"`   // bytes
	DiskUsed    uint64  `json:"disk_used"`    // bytes
}

// ExecutorTelemetry is a bounded, non-authoritative scheduling observation.
// Capability labels remain the hard compatibility boundary; pressure and
// cache keys are only soft-scoring inputs and operational evidence.
type ExecutorTelemetry struct {
	ObservedAt     time.Time `json:"observed_at"`
	Available      bool      `json:"available"`
	CPUPressure    int       `json:"cpu_pressure"`
	MemoryPressure int       `json:"memory_pressure"`
	DiskPressure   int       `json:"disk_pressure"`
	PressureScore  int       `json:"pressure_score"`
	CacheKeys      []string  `json:"cache_keys,omitempty"`
}

// CaptureExecutorTelemetry samples the executor host and derives bounded
// cache-locality keys from already validated capability labels.
func CaptureExecutorTelemetry(labels []string) ExecutorTelemetry {
	return executorTelemetryFromSystemInfo(GetSystemInfo(), labels, time.Now().UTC())
}

func executorTelemetryFromSystemInfo(
	info *SystemInfo,
	labels []string,
	observedAt time.Time,
) ExecutorTelemetry {
	telemetry := ExecutorTelemetry{
		ObservedAt: observedAt.UTC(),
		// /proc memory plus statfs are required to distinguish a genuinely
		// idle Linux worker from a platform on which sampling is unavailable.
		Available: info != nil && info.MemoryTotal > 0 && info.DiskTotal > 0,
	}
	if info != nil {
		telemetry.CPUPressure = pressurePermille(info.CPUUsage)
		telemetry.MemoryPressure = pressurePermille(info.MemoryUsage)
		telemetry.DiskPressure = pressurePermille(info.DiskUsage)
	}
	if telemetry.Available {
		telemetry.PressureScore = max(
			telemetry.CPUPressure,
			telemetry.MemoryPressure,
			telemetry.DiskPressure,
		)
	} else {
		// Unknown is neutral, not idle. This keeps unsupported/misconfigured
		// telemetry from winning soft-scoring decisions.
		telemetry.PressureScore = 500
	}
	for _, label := range labels {
		if strings.HasPrefix(label, "capacity-pool:") ||
			strings.HasPrefix(label, "profile:") ||
			strings.HasPrefix(label, "image:") {
			telemetry.CacheKeys = append(telemetry.CacheKeys, label)
		}
	}
	sort.Strings(telemetry.CacheKeys)
	return telemetry
}

func pressurePermille(value float64) int {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0
	}
	return min(1000, int(math.Round(value*10)))
}

// GetSystemInfo returns current system resource usage.
func GetSystemInfo() *SystemInfo {
	info := &SystemInfo{
		CPUCount: runtime.NumCPU(),
	}

	// Get memory info
	info.MemoryTotal, info.MemoryUsed, info.MemoryUsage = getMemoryInfo()

	// Get disk info for root partition
	info.DiskTotal, info.DiskUsed, info.DiskUsage = getDiskInfo("/")

	// Get CPU usage (load average based approximation)
	info.CPUUsage = getCPUUsage(info.CPUCount)

	return info
}

// getMemoryInfo reads memory information from /proc/meminfo.
func getMemoryInfo() (total, used uint64, percentage float64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, 0
	}

	var memTotal, memFree, memBuffers, memCached uint64

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}

		// Values in /proc/meminfo are in kB
		switch fields[0] {
		case "MemTotal:":
			memTotal = value * 1024
		case "MemFree:":
			memFree = value * 1024
		case "Buffers:":
			memBuffers = value * 1024
		case "Cached:":
			memCached = value * 1024
		}
	}

	if memTotal == 0 {
		return 0, 0, 0
	}

	// Calculate used memory (excluding buffers/cache)
	memUsed := memTotal - memFree - memBuffers - memCached
	memPercentage := float64(memUsed) / float64(memTotal) * 100

	return memTotal, memUsed, memPercentage
}

// getDiskInfo returns disk usage for the specified path.
func getDiskInfo(path string) (total, used uint64, percentage float64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, 0
	}

	total = stat.Blocks * uint64(stat.Bsize) // #nosec G115 -- statfs block size is non-negative.
	free := stat.Bfree * uint64(stat.Bsize)  // #nosec G115 -- statfs block size is non-negative.
	used = total - free

	if total == 0 {
		return 0, 0, 0
	}

	percentage = float64(used) / float64(total) * 100
	return total, used, percentage
}

// getCPUUsage estimates CPU usage from load average.
func getCPUUsage(cpuCount int) float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}

	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}

	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}

	// Convert load average to percentage (rough approximation)
	// Load of 1.0 per CPU core = 100% usage
	percentage := (load1 / float64(cpuCount)) * 100
	if percentage > 100 {
		percentage = 100
	}

	return percentage
}

// detectArchitecture returns the system architecture.
func detectArchitecture() string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "386":
		return "x86"
	case "arm":
		return "arm"
	default:
		return arch
	}
}
