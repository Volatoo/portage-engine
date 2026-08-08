package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/pkg/config"
)

// readyLedgerServer builds the steady state the ledger reaches on a healthy
// server: a projection that has just loaded and no outstanding write error.
// Everything below varies one fact away from it.
func readyLedgerServer(t *testing.T) *Server {
	t.Helper()
	s := New(&config.ServerConfig{BinpkgPath: t.TempDir(), MaxWorkers: 1})
	t.Cleanup(s.builder.Shutdown)
	s.jobLedger = persistence.NewJobRepository(nil)
	s.ledgerStaleAfter = time.Minute
	s.jobLedger.RecordProjectionSync(nil)
	return s
}

// TestLedgerHealthWithdrawsAVerdictItDoesNotHave is the point of scheduling the
// compare at all.
//
// `ok` used to read `LastError == "" && (CheckedAt.IsZero() || Consistent)`.
// Nothing called the compare, so CheckedAt was always zero and the second half
// was always true: `ok` reported whether the most recent write had succeeded
// while being named, published and consumed as ledger health — /readyz turns it
// into 200, the public status page turns it into "operational", and both
// consoles print it beside "last checked: never". A field that reports one
// thing under the name of another is worse the more people trust it.
func TestLedgerHealthWithdrawsAVerdictItDoesNotHave(t *testing.T) {
	s := readyLedgerServer(t)
	s.ledgerConsistencyEvery = 15 * time.Minute

	ok, status := s.checkLedgerHealth()

	if status["consistency_checked"] != false {
		t.Errorf("consistency_checked = %v, want false so an operator can see "+
			"which of the two answers is missing", status["consistency_checked"])
	}
	if status["consistency_stale"] != true {
		t.Errorf("consistency_stale = %v, want true", status["consistency_stale"])
	}
	// Fatal rather than Error: everything below reads the not-ready path, and a
	// ledger that called itself healthy has already failed the whole claim.
	if ok {
		t.Fatal("a ledger whose durable rows have never been compared with the " +
			"in-memory projection reported itself healthy")
	}

	// /readyz has to say which one it is: a check that never ran and a check
	// that ran and disagreed call for opposite work.
	recorder := httptest.NewRecorder()
	s.handleReadyz(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), "consistency check has not run") {
		t.Fatalf("readyz on an uncompared ledger: %d %s",
			recorder.Code, recorder.Body.String())
	}
}

// TestLedgerHealthDoesNotDemandACheckNobodyScheduled keeps the tightened verdict
// from turning an operator's explicit LEDGER_CONSISTENCY_INTERVAL_SECONDS=0 into
// a permanently unready replica. The ledger is then reported as unchecked, which
// is true, rather than as consistent, which nobody established.
func TestLedgerHealthDoesNotDemandACheckNobodyScheduled(t *testing.T) {
	s := readyLedgerServer(t)
	s.ledgerConsistencyEvery = 0

	ok, status := s.checkLedgerHealth()

	if !ok {
		t.Errorf("a deliberately disabled compare withheld readiness: %#v", status)
	}
	if status["consistency_checked"] != false {
		t.Errorf("consistency_checked = %v, want false", status["consistency_checked"])
	}
}

// TestLedgerConsistencyCheckRunsAndReportsThroughHealth walks the wiring the
// scheduled compare exists for: it runs, its report reaches JobLedgerStatus, and
// health reads it. Before this was wired, `.Reconcile(` had no caller outside an
// integration test and CheckedAt could only ever be zero.
func TestLedgerConsistencyCheckRunsAndReportsThroughHealth(t *testing.T) {
	s := readyLedgerServer(t)
	s.ledgerConsistencyEvery = 15 * time.Minute

	// Not due yet: the schedule holds it back rather than comparing on every
	// projection refresh.
	start := time.Now()
	s.ledgerConsistencyNext = start.Add(time.Minute)
	s.checkLedgerConsistency(start)
	if !s.jobLedger.Status().LastReconcile.CheckedAt.IsZero() {
		t.Fatal("the compare ran before it was due")
	}

	s.ledgerConsistencyNext = start
	s.checkLedgerConsistency(start)

	report := s.jobLedger.Status().LastReconcile
	if report.CheckedAt.IsZero() {
		t.Fatal("the scheduled compare never ran")
	}
	if report.Repaired != 0 {
		t.Errorf("repaired = %d: the scheduled compare reports, it does not write "+
			"the in-memory projection back over the authority it came from",
			report.Repaired)
	}
	_, status := s.checkLedgerHealth()
	if status["consistency_checked"] != true {
		t.Errorf("consistency_checked = %v after a compare ran",
			status["consistency_checked"])
	}
}

// TestLedgerConsistencyCheckIsSkippedWhenDisabled proves the cadence is a real
// switch and not a number the code ignores.
func TestLedgerConsistencyCheckIsSkippedWhenDisabled(t *testing.T) {
	s := readyLedgerServer(t)
	s.ledgerConsistencyEvery = 0

	s.checkLedgerConsistency(time.Now())

	if !s.jobLedger.Status().LastReconcile.CheckedAt.IsZero() {
		t.Error("a disabled compare ran anyway")
	}
}

// TestNextLedgerConsistencyCheckRetriesADisagreement covers both arms of the
// re-arm decision. A clean compare waits the configured interval; one that
// disagreed comes back on the next projection tick, because its verdict holds
// the replica out of rotation and the rows and the snapshot it judged were read
// milliseconds apart — a job created in that gap is a difference the very next
// sync resolves, and must not cost a quarter of an hour of readiness.
func TestNextLedgerConsistencyCheckRetriesADisagreement(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	every := 15 * time.Minute

	for _, test := range []struct {
		name   string
		report persistence.LedgerReconcileReport
		want   time.Time
	}{
		{
			name:   "agreed",
			report: persistence.LedgerReconcileReport{Consistent: true},
			want:   now.Add(every),
		},
		{
			name:   "diverged",
			report: persistence.LedgerReconcileReport{Missing: 1},
			want:   now,
		},
		{
			name: "unreadable",
			report: persistence.LedgerReconcileReport{
				Consistent: true, Error: "connection reset by peer",
			},
			want: now,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nextLedgerConsistencyCheck(now, every, test.report); !got.Equal(test.want) {
				t.Errorf("next check at %s, want %s", got, test.want)
			}
		})
	}
}

// TestLedgerConsistencyIntervalIsConfigurable pins the configuration seam,
// including the disabling value.
func TestLedgerConsistencyIntervalIsConfigurable(t *testing.T) {
	for _, test := range []struct {
		seconds int
		want    time.Duration
	}{
		{seconds: 900, want: 15 * time.Minute},
		{seconds: 30, want: 30 * time.Second},
		{seconds: 0, want: 0},
		{seconds: -1, want: 0},
	} {
		cfg := &config.ServerConfig{}
		cfg.Database.LedgerConsistencyIntervalSeconds = test.seconds
		if got := ledgerConsistencyInterval(cfg); got != test.want {
			t.Errorf("%d seconds -> %s, want %s", test.seconds, got, test.want)
		}
	}
}

// TestRetentionRunsBeforeTheProjectionItWouldContradict pins the order of the
// three ledger boot steps.
//
// The order is the whole of the bug and none of it is observable from a unit
// test: ExpireTerminal hides terminal jobs past the retention window, so a
// projection loaded before it holds exactly the rows it is about to hide, and
// the compare that follows counts every one of them as a job the map has and
// the ledger does not. checkLedgerHealth turns that verdict into a red /readyz
// and a logged warning, both describing a divergence this process manufactured
// between its own two reads. Reproducing it needs a ledger with expired rows
// and a real boot; asserting the order does not.
func TestRetentionRunsBeforeTheProjectionItWouldContradict(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	// Both paths reach the ledger: initPersistence at boot, and the reconciler
	// on every tick — which is the one that fires the sync.Once at all when the
	// boot-time load failed and Initialize downgraded it to a warning.
	ordered := map[string]bool{
		"initPersistence": false, "startLedgerReconciler": false,
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if _, guarded := ordered[fn.Name.Name]; !guarded {
			continue
		}
		ordered[fn.Name.Name] = true
		steps := ledgerBootSteps(fn)
		prune, load := indexOfStep(steps, "pruneLedgerOnce"), indexOfStep(steps, "LoadVisible")
		if prune < 0 || load < 0 {
			t.Fatalf("%s no longer prunes and loads; move this gate with it: %v",
				fn.Name.Name, steps)
		}
		if prune > load {
			t.Fatalf(
				"%s prunes after loading the projection (%v); retention then hides "+
					"rows the loaded map still holds and the compare reads them as a divergence",
				fn.Name.Name, steps,
			)
		}
		if compare := indexOfStep(steps, "checkLedgerConsistency"); compare >= 0 && compare < load {
			t.Fatalf("%s compares before loading the projection (%v)", fn.Name.Name, steps)
		}
	}
	for name, found := range ordered {
		if !found {
			t.Fatalf("%s is no longer in server.go; move this gate with it", name)
		}
	}
}

// ledgerBootSteps lists the ledger calls fn makes, in source order.
func ledgerBootSteps(fn *ast.FuncDecl) []string {
	watched := map[string]bool{
		"pruneLedgerOnce": true, "LoadVisible": true,
		"SyncLedgerJobs": true, "checkLedgerConsistency": true,
	}
	var steps []string
	ast.Inspect(fn, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && watched[selector.Sel.Name] {
			steps = append(steps, selector.Sel.Name)
		}
		return true
	})
	return steps
}

func indexOfStep(steps []string, name string) int {
	for index, step := range steps {
		if step == name {
			return index
		}
	}
	return -1
}
