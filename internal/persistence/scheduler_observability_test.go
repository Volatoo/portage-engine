package persistence

import "testing"

func TestSchedulerLeaseKindIsClosedAndStable(t *testing.T) {
	if got := schedulerLeaseKind([]string{"context:legacy"}); got != "attempt" {
		t.Fatalf("default lease kind=%q", got)
	}
	if got := schedulerLeaseKind([]string{
		"phase:provision", "worker-kind:admission",
	}); got != "admission" {
		t.Fatalf("admission lease kind=%q", got)
	}
}

func TestLeaseExpiryResultIsClosed(t *testing.T) {
	for state, expected := range map[string]string{
		"queued": "requeued", "failed": "failed", "canceled": "canceled",
	} {
		if got := leaseExpiryResult(state); got != expected {
			t.Fatalf("state %q expiry result=%q", state, got)
		}
	}
	if got := leaseExpiryResult("building"); got != "" {
		t.Fatalf("unknown expiry result=%q", got)
	}
}
