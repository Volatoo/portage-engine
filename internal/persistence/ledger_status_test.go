package persistence

import (
	"errors"
	"testing"

	"github.com/slchris/portage-engine/internal/builder"
)

// TestLedgerKeepsTheDiagnosisForAsLongAsTheCount holds the write-error message
// to the same lifetime as the number that reports it.
//
// LastError is deliberately transient: it answers "is the ledger unhealthy
// right now", and internal/server/server.go reads it that way to decide `ok`.
// The count is cumulative. Reporting a cumulative number beside a transient
// message meant an operator reading "19 write errors" off /monitor had no way
// left to learn what any of them were — on this deployment the message had been
// overwritten by the next of seventeen thousand successful writes.
func TestLedgerKeepsTheDiagnosisForAsLongAsTheCount(t *testing.T) {
	repository := &JobRepository{}
	boom := errors.New("insert build_jobs: deadlock detected")

	repository.recordWrite(boom)
	repository.recordWrite(nil)
	repository.recordWrite(nil)

	status := repository.Status()
	if status.WriteErrors != 1 {
		t.Fatalf("write errors = %d, want 1", status.WriteErrors)
	}
	if status.LastError != "" {
		t.Errorf("LastError = %q, want it cleared by the successful writes", status.LastError)
	}
	if status.LastWriteError != boom.Error() {
		t.Errorf("a count of %d write errors reports the message %q; "+
			"an operator who can see that writes failed and not how is being sent "+
			"to look where the answer is not",
			status.WriteErrors, status.LastWriteError)
	}
	if status.LastWriteErrorAt.IsZero() {
		t.Error("the retained write error carries no time, so it cannot be told " +
			"from one that happened days ago")
	}
}

// TestLedgerDoesNotCountAnIdempotencyConflictAsAWriteFailure pins the exemption
// recordWrite already makes, so the retained message cannot start reporting one.
func TestLedgerDoesNotCountAnIdempotencyConflictAsAWriteFailure(t *testing.T) {
	repository := &JobRepository{}
	repository.recordWrite(nil)
	before := repository.Status()

	repository.recordWrite(builder.NewIdempotencyConflictError(
		errors.New("idempotency key reused with a different request")))

	after := repository.Status()
	if after.WriteErrors != before.WriteErrors {
		t.Errorf("write errors %d -> %d: a client-side 409 proves the ledger was "+
			"reachable and compared deterministically", before.WriteErrors, after.WriteErrors)
	}
	if after.LastWriteError != "" {
		t.Errorf("retained %q for a conflict that is not a write failure", after.LastWriteError)
	}
}
