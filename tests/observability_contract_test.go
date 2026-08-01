package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A dashboard panel or an alert whose metric no production path ever writes is
// worse than no panel: Success Rate renders a red 0% on a healthy system and a
// critical alert can never clear. This gate catches the cheapest form of that —
// a charted metric whose recorder is named nowhere but internal/metrics itself,
// which is what a deleted or never-written call site looks like.
//
// It is a textual scan, and the limit is worth stating because the gap has bit
// before: a recorder named in exactly one non-test file passes even when that
// file is a legacy path the shipped topology never enters, or when the call
// sits behind a mode the deployment does not select. recordCompletedBuildMetric
// stayed green here throughout the period its only call site was inside
// updateStatus, which PHASE_EXECUTOR_MODE=active never reaches. Deciding that
// needs the call graph and the runtime role, neither of which a regexp has.
// Reachability under the executor the deployment actually runs is pinned
// behaviourally instead, one metric at a time, next to the code that writes it
// — internal/builder/manager_phase_test.go for the phase-path counters.
func TestChartedAndAlertedMetricsHaveAProductionWriter(t *testing.T) {
	root := repositoryRoot(t)
	exposition := readRepositoryFile(t, "internal/metrics/metrics.go")

	// Only counters and gauges backed by a Metrics field need a recorder call;
	// the scheduler and monitor series are rendered straight from a
	// SchedulerSnapshot the persistence layer produces on every scrape.
	fieldOf := map[string]string{}
	for _, match := range regexp.MustCompile(`"(portage_[a-z0-9_]+) %d\\n", m\.([A-Za-z0-9_]+)\.`).
		FindAllStringSubmatch(exposition, -1) {
		fieldOf[match[1]] = match[2]
	}
	if len(fieldOf) == 0 {
		t.Fatal("no field-backed metrics found in internal/metrics/metrics.go")
	}

	recorderOf := map[string]string{}
	for _, block := range regexp.MustCompile(`(?s)func \(m \*Metrics\) ([A-Za-z0-9_]+)\([^)]*\) \{.*?\n\}`).
		FindAllStringSubmatch(exposition, -1) {
		for _, mutation := range regexp.MustCompile(`m\.([A-Za-z0-9_]+)\.(Add|Set|Store)\(`).
			FindAllStringSubmatch(block[0], -1) {
			recorderOf[mutation[1]] = block[1]
		}
	}

	written := writtenRecorders(t, root)
	// A recorder the scrape path refreshes from the injected scheduler snapshot
	// is reachable too, and its only call site is necessarily inside this
	// package. Counting those as unwritten would condemn a gauge that is
	// correctly computed on every scrape; what is worth checking for them is
	// that some production path still installs the provider at all.
	refreshed := scrapeRefreshedRecorders(exposition)
	if len(refreshed) > 0 && !written["SetSchedulerProvider"] {
		t.Error("no production path installs the scheduler snapshot provider, " +
			"so every scrape-time refreshed gauge reads zero forever")
	}

	for _, metric := range chartedAndAlertedMetrics(t) {
		field, backed := fieldOf[metric]
		if !backed {
			continue
		}
		recorder, found := recorderOf[field]
		if !found {
			t.Errorf("metric %q has no recorder method on Metrics", metric)
			continue
		}
		if !written[recorder] && !refreshed[recorder] {
			t.Errorf("metric %q is charted or alerted but no non-test source "+
				"outside internal/metrics names %s", metric, recorder)
		}
	}
}

// The Infrastructure row charts the executor inventory, and the legacy series
// behind portage_builders_* have exactly two writers — /api/v1/builders/register
// and /api/v1/builders/heartbeat — which no executor arriving over the mTLS
// worker gateway ever calls. Charting them puts a stat at 0 on a fully staffed
// cluster and trips its red <1 threshold, reporting an outage that is not
// happening. The regexp gate above cannot see this: those recorders do have
// production call sites, just not ones the deployed topology reaches.
func TestDashboardChartsGatewayInventoryNotTheLegacyRegistry(t *testing.T) {
	charted := map[string]bool{}
	for _, metric := range chartedAndAlertedMetrics(t) {
		charted[metric] = true
	}
	for _, legacy := range []string{
		"portage_builders_active", "portage_builders_healthy",
		"portage_builder_capacity",
	} {
		if charted[legacy] {
			t.Errorf("%s is charted or alerted, but only the legacy HTTP "+
				"heartbeat registry writes it and it reads 0 under the gateway "+
				"topology", legacy)
		}
	}
	if !charted["portage_gateway_workers_active"] {
		t.Error("nothing charts portage_gateway_workers_active, leaving the " +
			"executor inventory the scheduler actually dispatches against unread")
	}
}

// scrapeRefreshedRecorders reports the Metrics methods the exposition handler
// calls with a field of the snapshot it just pulled from the injected provider.
// Those recorders are written on every scrape without any caller outside this
// package, which is the whole point of a scrape-time gauge.
func scrapeRefreshedRecorders(exposition string) map[string]bool {
	refreshed := map[string]bool{}
	for _, binding := range regexp.MustCompile(`(\w+)\s*:?=\s*schedulerProvider\(\)`).
		FindAllStringSubmatch(exposition, -1) {
		for _, call := range regexp.MustCompile(
			`m\.([A-Za-z0-9_]+)\(\s*`+regexp.QuoteMeta(binding[1])+`\.`,
		).FindAllStringSubmatch(exposition, -1) {
			refreshed[call[1]] = true
		}
	}
	return refreshed
}

// writtenRecorders reports which Metrics methods a non-test production path calls.
func writtenRecorders(t *testing.T, root string) map[string]bool {
	t.Helper()
	called := map[string]bool{}
	pattern := regexp.MustCompile(`\.([A-Z][A-Za-z0-9_]*)\(`)
	for _, tree := range []string{"internal", "cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") ||
				strings.Contains(path, filepath.Join("internal", "metrics")) {
				return err
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for _, match := range pattern.FindAllStringSubmatch(string(data), -1) {
				called[match[1]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return called
}

// chartedAndAlertedMetrics collects every portage_* series the deployed Grafana
// dashboard and Prometheus rules depend on.
func chartedAndAlertedMetrics(t *testing.T) []string {
	t.Helper()
	var dashboard struct {
		Panels []struct {
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(
		[]byte(readRepositoryFile(t, "deploy/grafana/portage-engine-dashboard.json")),
		&dashboard,
	); err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`portage_[a-z0-9_]+`)
	seen := map[string]bool{}
	var metrics []string
	collect := func(text string) {
		for _, name := range pattern.FindAllString(text, -1) {
			if !seen[name] {
				seen[name] = true
				metrics = append(metrics, name)
			}
		}
	}
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			collect(target.Expr)
		}
	}
	collect(readRepositoryFile(t, "deploy/observability/rules/portage-engine.yml"))
	if len(metrics) == 0 {
		t.Fatal("no metrics found in the dashboard or alert rules")
	}
	return metrics
}
