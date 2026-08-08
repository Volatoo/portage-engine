package persistence

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// recordingQuerier captures the counter rows a flush touches, in the order it
// touches them. Only Exec is reachable from flushLeaseExpiries.
type recordingQuerier struct {
	updates []leaseExpiryKey
	counts  []int64
}

func (q *recordingQuerier) Exec(
	_ context.Context, _ string, arguments ...any,
) (pgconn.CommandTag, error) {
	kind, _ := arguments[0].(string)
	result, _ := arguments[1].(string)
	count, _ := arguments[2].(int64)
	q.updates = append(q.updates, leaseExpiryKey{leaseKind: kind, result: result})
	q.counts = append(q.counts, count)
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (q *recordingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("flushLeaseExpiries must not query")
}

func (q *recordingQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("flushLeaseExpiries must not query")
}

// TestLeaseExpiryCountersAreWrittenInOneGlobalOrder is the deadlock fix stated
// as a property: whatever a batch recovered, the counter rows are taken in the
// same order every time.
//
// Recovery used to write one counter update per recovered lease, in the order
// loadExpiredLeases returned them — by expires_at. Two replicas recovering
// overlapping lease kinds therefore took the same rows in opposite orders,
// which PostgreSQL resolves with 40P01 by rolling one of them back. The
// transaction it rolls back is the one that requeues expired work, so the cost
// of the cycle is a whole round of recovery, not one increment.
func TestLeaseExpiryCountersAreWrittenInOneGlobalOrder(t *testing.T) {
	// Two batches that recovered the same kinds in opposite arrival orders.
	// Go randomizes map iteration, so an unsorted flush would disagree with
	// itself, never mind with another replica.
	forward := map[leaseExpiryKey]int64{
		{leaseKind: "attempt", result: "requeued"}:   3,
		{leaseKind: "attempt", result: "failed"}:     1,
		{leaseKind: "admission", result: "canceled"}: 2,
		{leaseKind: "phase", result: "reclaimed"}:    5,
	}
	want := []leaseExpiryKey{
		{leaseKind: "admission", result: "canceled"},
		{leaseKind: "attempt", result: "failed"},
		{leaseKind: "attempt", result: "requeued"},
		{leaseKind: "phase", result: "reclaimed"},
	}
	// Repeated because the failure is a map-iteration order, and one run of an
	// unsorted flush can agree with the sorted one by chance.
	for attempt := range 32 {
		querier := &recordingQuerier{}
		if err := flushLeaseExpiries(context.Background(), querier, forward); err != nil {
			t.Fatalf("flush: %v", err)
		}
		if len(querier.updates) != len(want) {
			t.Fatalf("run %d wrote %d counter rows, want %d",
				attempt, len(querier.updates), len(want))
		}
		for index, key := range want {
			if querier.updates[index] != key {
				t.Fatalf("run %d took counter rows in order %v, want %v",
					attempt, querier.updates, want)
			}
		}
	}
}

// TestLeaseExpiryCountersAreFoldedIntoOneUpdatePerRow keeps the ordered section
// short: the point of sorting is lost if a batch of thirty takes and holds
// thirty locks one at a time.
func TestLeaseExpiryCountersAreFoldedIntoOneUpdatePerRow(t *testing.T) {
	querier := &recordingQuerier{}
	err := flushLeaseExpiries(context.Background(), querier, map[leaseExpiryKey]int64{
		{leaseKind: "attempt", result: "requeued"}: 17,
	})
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(querier.updates) != 1 || querier.counts[0] != 17 {
		t.Fatalf("17 recovered leases produced %d updates %v",
			len(querier.updates), querier.counts)
	}
}

// TestOnlyTheFlushWritesLeaseExpiryCounters is the structural half of the fix.
//
// The three cases above prove flushLeaseExpiries orders what it is given; none
// of them can see a second writer that never goes through it. ClaimPhaseWork
// was exactly that writer: it took the phase counter at the top of its
// transaction, before locking any phase_work_items row, while the recovery loop
// locks phase_work_items first and the counters last. Each path was internally
// ordered and the pair still deadlocked. A behavioural test cannot catch the
// next one — the call that reintroduces it compiles, passes, and only fails
// under concurrency in production — so the invariant is asserted over the source
// instead: recordLeaseExpiry has one caller, and it is the flush.
func TestOnlyTheFlushWritesLeaseExpiryCounters(t *testing.T) {
	const writer = "recordLeaseExpiry"
	const allowed = "flushLeaseExpiries"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fileSet := token.NewFileSet()
	inspected := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		inspected++
		// Enclosing function for every call site, so the report names the
		// offender rather than a line number.
		var enclosing string
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.FuncDecl:
				enclosing = typed.Name.Name
			case *ast.CallExpr:
				called, ok := typed.Fun.(*ast.Ident)
				if !ok || called.Name != writer {
					return true
				}
				if enclosing == allowed {
					return true
				}
				t.Errorf(
					"%s:%s calls %s directly; tally the count instead and let "+
						"%s take the counter locks last",
					name, enclosing, writer, allowed,
				)
			}
			return true
		})
	}
	// A glob that stops matching would make this pass over nothing.
	if inspected < 2 {
		t.Fatalf("inspected %d files; the package source was not read", inspected)
	}
}

// A tally that recovered nothing writes nothing, so an idle recovery round
// takes no counter lock at all.
func TestAnEmptyRecoveryBatchTakesNoCounterLock(t *testing.T) {
	querier := &recordingQuerier{}
	if err := flushLeaseExpiries(
		context.Background(), querier, map[leaseExpiryKey]int64{},
	); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(querier.updates) != 0 {
		t.Fatalf("an empty batch wrote %v", querier.updates)
	}
}
