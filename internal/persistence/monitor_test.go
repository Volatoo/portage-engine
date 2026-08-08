package persistence

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		snapshotAge       time.Duration
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
			source: &recent, projected: &old, snapshotAge: 12 * time.Second,
			wantState: "lagging", wantValid: true, wantPresent: true,
			wantLagSeconds: 12,
		},
		{
			name:      "missing projection reports cached snapshot age",
			source:    &recent,
			wantState: "lagging", wantValid: true, wantPresent: true,
		},
		{
			name:      "projection without a source is invalid",
			projected: &old,
			wantState: "invalid",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := agedMonitorProjection(
				monitorProjectionStatus(now, test.source, test.projected),
				test.snapshotAge,
			)
			if status.State != test.wantState || status.Valid != test.wantValid ||
				status.SourceWatermarkPresent != test.wantPresent ||
				status.LagSeconds != test.wantLagSeconds {
				t.Fatalf("projection status=%+v", status)
			}
		})
	}
}

// A quiet control plane whose previous terminal event is hours old must not
// report those hours as staleness the moment one build finishes: the lag is
// how long this replica has been serving a snapshot without that build.
func TestMonitorProjectionLagIsSnapshotAgeNotEventGap(t *testing.T) {
	now := time.Date(2026, time.August, 1, 8, 0, 0, 0, time.UTC)
	projected := now.Add(-3 * time.Hour)
	source := now.Add(-time.Second)
	loaded := monitorProjectionStatus(now, &source, &projected)
	stale := agedMonitorProjection(loaded, 20*time.Second)
	fresh := agedMonitorProjection(loaded, 0)
	if stale.State != "lagging" || stale.LagSeconds != 20 ||
		fresh.LagSeconds != 0 {
		t.Fatalf("projection lag stale=%+v fresh=%+v", stale, fresh)
	}
}

// LagSeconds is the age of the snapshot being served, so the bound published
// beside it has to be the cache TTL that decides when that snapshot is thrown
// away: a reader scales the reading against the bound, and a bound inherited
// from the retired watermark-distance semantics scaled it against a number the
// read model can no longer produce. The bound describes the refresh contract,
// it is not a ceiling applied to the reading — monitorProjectionStatus reports
// whatever snapshot age it is handed.
func TestMonitorLagBoundTracksTheCacheTTL(t *testing.T) {
	now := time.Date(2026, time.August, 1, 8, 0, 0, 0, time.UTC)
	source := now.Add(-time.Second)
	projected := now.Add(-time.Hour)
	loaded := monitorProjectionStatus(now, &source, &projected)
	status := agedMonitorProjection(loaded, 29*time.Second)
	if status.AlertThresholdSeconds != int64(monitorCacheTTL/time.Second) {
		t.Fatalf("published lag bound %ds does not match the %s cache TTL that produces lag_seconds",
			status.AlertThresholdSeconds, monitorCacheTTL)
	}
	beyond := agedMonitorProjection(loaded, 90*time.Second)
	if beyond.LagSeconds != 90 {
		t.Fatalf("lag reading was clamped to the published bound: %+v", beyond)
	}
}

// The compose gate asserts the published bound against a literal, and a literal
// left behind by a semantics change is how that gate came to demand a value the
// API had stopped publishing: it failed against a correct server, which reads as
// a broken deployment. Nothing else compares the constant with the script.
func TestComposeGatePinsThePublishedLagBound(t *testing.T) {
	script, err := os.ReadFile(
		filepath.Join("..", "..", "scripts", "verify-compose.sh"),
	)
	if err != nil {
		t.Fatal(err)
	}
	pinned := fmt.Sprintf(
		".target_history.projection.alert_threshold_seconds == %d and",
		int64(monitorCacheTTL/time.Second),
	)
	if !strings.Contains(string(script), pinned) {
		t.Fatalf("verify-compose.sh does not assert %q", pinned)
	}
}

func TestMonitorProjectionLagNeverReportsNegativeStaleness(t *testing.T) {
	now := time.Date(2026, time.August, 1, 8, 0, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	status := agedMonitorProjection(
		monitorProjectionStatus(now, &future, nil), -time.Second,
	)
	if status.State != "lagging" || status.LagSeconds != 0 {
		t.Fatalf("projection status=%+v", status)
	}
}
