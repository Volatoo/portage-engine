package persistence

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/slchris/portage-engine/internal/workergateway"
)

func gatewayTestIdentity() workergateway.Identity {
	return workergateway.Identity{
		WorkerID:   "worker-1",
		JobID:      "11111111-1111-4111-8111-111111111111",
		AttemptID:  "22222222-2222-4222-8222-222222222222",
		FenceToken: 3,
	}
}

// The destination column is what the gateway ultimately hands to the
// filesystem. A row that spells a relative name or leaves ".." unresolved
// cannot have come from the control plane, so neither writing nor reading one
// is allowed to succeed.
func TestGatewayDestinationMustBeAbsoluteAndNormalized(t *testing.T) {
	for _, destination := range []string{
		"",
		"quarantine/token/pkg.gpkg.tar",
		"../../etc/cron.d/portage-engine",
		"/srv/binpkgs/.portage-engine-quarantine/../../../etc/cron.d/portage-engine",
		"/srv/binpkgs/./quarantine/pkg.gpkg.tar",
		"/srv/binpkgs/quarantine/",
		strings.Repeat("/a", maxGatewayDestinationBytes),
	} {
		if err := checkGatewayDestination(destination); err == nil {
			t.Fatalf("destination %q was accepted", destination)
		}
	}
	for _, destination := range []string{
		"/srv/binpkgs/.portage-engine-quarantine/0f9c/app-misc/jq-1.8-1.gpkg.tar",
		"/tmp/gateway-spool-artifact",
	} {
		if err := checkGatewayDestination(destination); err != nil {
			t.Fatalf("legitimate destination %q was rejected: %v", destination, err)
		}
	}
}

// PrepareWorkerUpload must reach its validation before it reaches the pool, so
// a traversal destination is refused by a repository that has no database at
// all — the refusal is the code path, not a database constraint. A repository
// without a pool that gets as far as querying announces itself by panicking,
// which is reported here as the failure it is.
func TestPrepareWorkerUploadRejectsTraversalDestination(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("traversal destination reached the database layer: %v", recovered)
		}
	}()
	repository := &JobRepository{}
	err := repository.PrepareWorkerUpload(
		context.Background(), gatewayTestIdentity(), uuid.NewString(),
		"/srv/binpkgs/.portage-engine-quarantine/0f9c/../../../../etc/cron.d/pe",
		1024,
	)
	if err == nil {
		t.Fatal("traversal destination was persisted as an upload capability")
	}
}

// A uuid column matches every spelling of the same value, so a worker could
// redeem its own capability under a name of its choosing unless the textual
// form is pinned here as well.
func TestGatewayUploadIDMustBeCanonical(t *testing.T) {
	canonical := uuid.NewString()
	if err := checkGatewayUploadID(canonical); err != nil {
		t.Fatalf("canonical upload id was rejected: %v", err)
	}
	for _, spelling := range []string{
		"",
		"not-a-uuid",
		"{" + canonical + "}",
		"urn:uuid:" + canonical,
		strings.ToUpper(canonical),
		strings.ReplaceAll(canonical, "-", ""),
	} {
		if err := checkGatewayUploadID(spelling); err == nil {
			t.Fatalf("upload id %q was accepted", spelling)
		}
	}
}

func TestClaimWorkerUploadRejectsNonCanonicalID(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("braced upload id reached the database layer: %v", recovered)
		}
	}()
	repository := &JobRepository{}
	canonical := uuid.NewString()
	if _, err := repository.ClaimWorkerUpload(
		context.Background(), gatewayTestIdentity(), "{"+canonical+"}", time.Minute,
	); err == nil {
		t.Fatal("braced upload id was carried into the claim transaction")
	}
}
