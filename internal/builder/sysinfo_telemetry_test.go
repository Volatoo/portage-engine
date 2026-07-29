package builder

import (
	"reflect"
	"testing"
	"time"
)

func TestExecutorTelemetryUsesWorstPressureAndStableCacheKeys(t *testing.T) {
	observedAt := time.Date(2026, 7, 29, 1, 2, 3, 0, time.UTC)
	telemetry := executorTelemetryFromSystemInfo(&SystemInfo{
		CPUUsage: 17.4, MemoryUsage: 82.6, DiskUsage: 44.5,
		MemoryTotal: 16 << 30, DiskTotal: 100 << 30,
	}, []string{
		"phase:build", "image:base@g7", "capacity-pool:p1",
		"profile:pe/base", "provider:pve",
	}, observedAt)
	if !telemetry.Available || telemetry.PressureScore != 826 ||
		telemetry.CPUPressure != 174 || telemetry.MemoryPressure != 826 ||
		telemetry.DiskPressure != 445 || !telemetry.ObservedAt.Equal(observedAt) {
		t.Fatalf("unexpected telemetry: %+v", telemetry)
	}
	wantKeys := []string{
		"capacity-pool:p1", "image:base@g7", "profile:pe/base",
	}
	if !reflect.DeepEqual(telemetry.CacheKeys, wantKeys) {
		t.Fatalf("cache keys=%v, want %v", telemetry.CacheKeys, wantKeys)
	}
}

func TestExecutorTelemetryTreatsUnavailableSamplingAsNeutral(t *testing.T) {
	telemetry := executorTelemetryFromSystemInfo(
		&SystemInfo{CPUUsage: 0}, nil, time.Now(),
	)
	if telemetry.Available || telemetry.PressureScore != 500 {
		t.Fatalf("unavailable telemetry was treated as idle: %+v", telemetry)
	}
}
