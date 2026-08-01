package persistence

import (
	"testing"
	"time"
)

func TestMonitorProjectionStatus(t *testing.T) {
	now := time.Date(2026, time.August, 1, 8, 0, 0, 0, time.UTC)
	old := now.Add(-10 * time.Minute)
	recent := now.Add(-3 * time.Minute)

	tests := []struct {
		name              string
		source, projected *time.Time
		wantState         string
		wantValid         bool
		wantPresent       bool
		wantLagSeconds    int64
	}{
		{
			name:      "empty source is valid and has zero lag",
			wantState: "empty", wantValid: true,
		},
		{
			name:   "old but fully projected source is current",
			source: &old, projected: &old,
			wantState: "current", wantValid: true, wantPresent: true,
		},
		{
			name:   "new terminal event outside cached projection is lagging",
			source: &recent, projected: &old,
			wantState: "lagging", wantValid: true, wantPresent: true,
			wantLagSeconds: 420,
		},
		{
			name:      "missing projection uses durable source event age",
			source:    &recent,
			wantState: "lagging", wantValid: true, wantPresent: true,
			wantLagSeconds: 180,
		},
		{
			name:      "projection without a source is invalid",
			projected: &old,
			wantState: "invalid",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := monitorProjectionStatus(now, test.source, test.projected)
			if status.State != test.wantState || status.Valid != test.wantValid ||
				status.SourceWatermarkPresent != test.wantPresent ||
				status.LagSeconds != test.wantLagSeconds {
				t.Fatalf("projection status=%+v", status)
			}
		})
	}
}

func TestMonitorProjectionWatermarkLagGrowsWithNewSourceEvents(t *testing.T) {
	now := time.Date(2026, time.August, 1, 8, 0, 0, 0, time.UTC)
	projected := now.Add(-10 * time.Minute)
	firstSource := now.Add(-3 * time.Minute)
	newerSource := now.Add(-time.Minute)
	first := monitorProjectionStatus(now, &firstSource, &projected)
	newer := monitorProjectionStatus(now, &newerSource, &projected)
	if first.LagSeconds != 420 || newer.LagSeconds != 540 ||
		newer.LagSeconds <= first.LagSeconds {
		t.Fatalf("watermark lag did not grow first=%+v newer=%+v", first, newer)
	}
}

func TestMonitorProjectionFutureEventDoesNotProduceNegativeLag(t *testing.T) {
	now := time.Date(2026, time.August, 1, 8, 0, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	status := monitorProjectionStatus(now, &future, nil)
	if status.State != "lagging" || status.LagSeconds != 0 {
		t.Fatalf("future source watermark produced invalid lag: %+v", status)
	}
}
