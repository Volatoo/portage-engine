package persistence_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/internal/imagefactory"
	"github.com/slchris/portage-engine/internal/migrations"
	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/internal/signing"
	"github.com/slchris/portage-engine/pkg/config"
)

func TestPostgresMigrationTransactionAndCompatibility(t *testing.T) {
	adminDSN := os.Getenv("PORTAGE_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("set PORTAGE_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin database: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	schema := "db0_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	defer func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Logf("drop test schema: %v", err)
		}
	}()

	testDSN, err := withQueryValue(adminDSN, "search_path", schema)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DatabaseConfig{
		Enabled:               true,
		Required:              true,
		URL:                   testDSN,
		MaxConns:              4,
		MinConns:              0,
		ConnectTimeoutSeconds: 10,
		HealthTimeoutSeconds:  2,
	}

	runner, err := migrations.NewRunner(ctx, cfg)
	if err != nil {
		t.Fatalf("create migration runner: %v", err)
	}
	if _, err := runner.Provider().Up(ctx); err != nil {
		_ = runner.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	if err := runner.Close(); err != nil {
		t.Fatalf("close migration runner: %v", err)
	}

	db, err := persistence.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open application pool: %v", err)
	}
	defer db.Close()
	if health := db.Check(ctx); !health.OK || health.SchemaVersion != persistence.MaxSchemaVersion {
		t.Fatalf("health after migration = %+v", health)
	}

	projectID := uuid.New()
	rollbackMarker := errors.New("force rollback")
	err = db.WithTx(ctx, pgx.TxOptions{}, func(q persistence.Querier) error {
		if _, err := q.Exec(ctx, "INSERT INTO projects (id, name) VALUES ($1, $2)", projectID, "rollback-project"); err != nil {
			return err
		}
		return rollbackMarker
	})
	if !errors.Is(err, rollbackMarker) {
		t.Fatalf("transaction error = %v", err)
	}
	var count int
	if err := db.Pool().QueryRow(ctx, "SELECT count(*) FROM projects WHERE id = $1", projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rolled-back project count = %d", count)
	}

	if err := db.WithTx(ctx, pgx.TxOptions{}, func(q persistence.Querier) error {
		_, err := q.Exec(ctx, "INSERT INTO projects (id, name) VALUES ($1, $2)", projectID, "committed-project")
		return err
	}); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}
	if err := db.Pool().QueryRow(ctx, "SELECT count(*) FROM projects WHERE id = $1", projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("committed project count = %d", count)
	}

	if _, err := db.Pool().Exec(ctx, "INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)", persistence.MaxSchemaVersion+1); err != nil {
		t.Fatalf("inject future schema version: %v", err)
	}
	health := db.Check(ctx)
	if health.OK || !strings.Contains(health.Reason, "newer than server maximum") {
		t.Fatalf("future schema must fail closed: %+v", health)
	}
}

func TestSigningQueueDigestFenceConcurrencyAndReclaim(t *testing.T) {
	adminDSN := os.Getenv("PORTAGE_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("set PORTAGE_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin database: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	schema := "db_sign_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	defer func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Logf("drop signing schema: %v", err)
		}
	}()
	testDSN, err := withQueryValue(adminDSN, "search_path", schema)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DatabaseConfig{
		Enabled: true, Required: true, URL: testDSN,
		MaxConns: 12, ConnectTimeoutSeconds: 10, HealthTimeoutSeconds: 2,
	}
	runner, err := migrations.NewRunner(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().Up(ctx); err != nil {
		_ = runner.Close()
		t.Fatal(err)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := persistence.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := persistence.NewJobRepository(db)

	jobID, attemptID, workerID := uuid.New(), uuid.New(), uuid.New()
	sourceToken := strings.Repeat("a", 32)
	digest := strings.Repeat("1", 64)
	relative := "app-misc/hello-2.12.2.gpkg.tar"
	if err := db.WithTx(ctx, pgx.TxOptions{}, func(q persistence.Querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO workers (id, stable_name, max_slots)
			VALUES ($1, 'signing-build-worker', 1)
		`, workerID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO build_jobs (id, package_atom, state, request, request_digest)
			VALUES ($1, 'app-misc/hello', 'signing', '{}'::jsonb, 'fixture')
		`, jobID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO build_attempts (id, job_id, attempt_no, state, worker_id, fence_token)
			VALUES ($1, $2, 1, 'signing', $3, 1)
		`, attemptID, jobID, workerID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO worker_leases (id, worker_id, attempt_id, fence_token, expires_at)
			VALUES ($1, $2, $3, 1, clock_timestamp() + interval '30 seconds')
		`, uuid.New(), workerID, attemptID); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `
			INSERT INTO artifacts (
				id, job_id, attempt_id, kind, state, digest, size_bytes, location
			) VALUES ($1, $2, $3, 'binpkg', 'verified_unsigned', $4, 123,
			          'quarantine:' || $5 || '/' || $6)
		`, uuid.New(), jobID, attemptID, digest, sourceToken, relative)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request := signing.Request{
		JobID: jobID.String(), AttemptID: attemptID.String(), AttemptFence: 1,
		LeaseOwner: "signing-build-worker", SourceToken: sourceToken, Architecture: "amd64",
		Artifacts: []signing.Artifact{{
			RelativePath: relative, InputDigest: digest, InputSize: 123,
		}},
	}
	task, err := repo.EnqueueSigning(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := repo.EnqueueSigning(ctx, request)
	if err != nil || retry.ID != task.ID {
		t.Fatalf("idempotent enqueue task=%+v err=%v", retry, err)
	}
	changed := request
	changed.Architecture = "arm64"
	if _, err := repo.EnqueueSigning(ctx, changed); err == nil {
		t.Fatal("same attempt accepted a different immutable signing request")
	}

	const claimers = 8
	claims := make(chan *signing.Task, claimers)
	errs := make(chan error, claimers)
	var group sync.WaitGroup
	for index := 0; index < claimers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			claim, claimErr := repo.ClaimSigning(ctx, fmt.Sprintf("signer-%d", index), 150*time.Millisecond)
			claims <- claim
			errs <- claimErr
		}(index)
	}
	group.Wait()
	close(claims)
	close(errs)
	for claimErr := range errs {
		if claimErr != nil {
			t.Fatal(claimErr)
		}
	}
	var first *signing.Task
	count := 0
	for claim := range claims {
		if claim != nil {
			first = claim
			count++
		}
	}
	if count != 1 || first == nil || first.ClaimFence != 1 {
		t.Fatalf("concurrent signer claims=%d first=%+v", count, first)
	}
	time.Sleep(350 * time.Millisecond)
	reclaimed, err := repo.ClaimSigning(ctx, "signer-reclaimed", time.Second)
	if err != nil || reclaimed == nil || reclaimed.ID != first.ID || reclaimed.ClaimFence != 2 {
		t.Fatalf("reclaimed=%+v err=%v", reclaimed, err)
	}
	outputs := []signing.Artifact{{
		RelativePath: relative, InputDigest: digest, InputSize: 123,
		OutputDigest: strings.Repeat("2", 64), OutputSize: 456,
	}}
	if err := repo.CompleteSigning(ctx, first, "stale-key", outputs); err == nil {
		t.Fatal("expired signer claim completed the task")
	}
	wrong := append([]signing.Artifact(nil), outputs...)
	wrong[0].RelativePath = "app-misc/other-1.gpkg.tar"
	if err := repo.CompleteSigning(ctx, reclaimed, "test-key", wrong); err == nil {
		t.Fatal("signer changed the immutable artifact set")
	}
	if err := repo.CompleteSigning(ctx, reclaimed, "test-key", outputs); err != nil {
		t.Fatal(err)
	}
	completed, err := repo.GetSigningTask(ctx, task.ID)
	if err != nil || completed.State != "completed" || completed.SigningKeyID != "test-key" ||
		len(completed.Artifacts) != 1 || completed.Artifacts[0].OutputDigest != outputs[0].OutputDigest {
		t.Fatalf("completed task=%+v err=%v", completed, err)
	}

	// A queued task loses authority as soon as its originating build lease
	// expires; signer polling cancels it before it can be claimed.
	staleJobID, staleAttemptID := uuid.New(), uuid.New()
	staleToken := strings.Repeat("b", 32)
	if err := db.WithTx(ctx, pgx.TxOptions{}, func(q persistence.Querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO build_jobs (id, package_atom, state, request, request_digest)
			VALUES ($1, 'app-misc/hello', 'signing', '{}'::jsonb, 'stale-fixture')
		`, staleJobID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO build_attempts (id, job_id, attempt_no, state, worker_id, fence_token)
			VALUES ($1, $2, 1, 'signing', $3, 2)
		`, staleAttemptID, staleJobID, workerID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO worker_leases (id, worker_id, attempt_id, fence_token, expires_at)
			VALUES ($1, $2, $3, 2, clock_timestamp() + interval '30 seconds')
		`, uuid.New(), workerID, staleAttemptID); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `
			INSERT INTO artifacts (
				id, job_id, attempt_id, kind, state, digest, size_bytes, location
			) VALUES ($1, $2, $3, 'binpkg', 'verified_unsigned', $4, 123,
			          'quarantine:' || $5 || '/' || $6)
		`, uuid.New(), staleJobID, staleAttemptID, digest, staleToken, relative)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	staleRequest := request
	staleRequest.JobID = staleJobID.String()
	staleRequest.AttemptID = staleAttemptID.String()
	staleRequest.AttemptFence = 2
	staleRequest.SourceToken = staleToken
	staleTask, err := repo.EnqueueSigning(ctx, staleRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE worker_leases SET expires_at = clock_timestamp() - interval '1 second'
		WHERE attempt_id = $1
	`, staleAttemptID); err != nil {
		t.Fatal(err)
	}
	if claim, err := repo.ClaimSigning(ctx, "signer-after-build-expiry", time.Second); err != nil || claim != nil {
		t.Fatalf("claim after build lease expiry=%+v err=%v", claim, err)
	}
	canceled, err := repo.GetSigningTask(ctx, staleTask.ID)
	if err != nil || canceled.State != "canceled" {
		t.Fatalf("stale task=%+v err=%v", canceled, err)
	}
}

func TestJobLedgerIdempotencyTransitionsOutboxAndReconcile(t *testing.T) {
	adminDSN := os.Getenv("PORTAGE_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("set PORTAGE_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin database: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	schema := "db1_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	defer func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Logf("drop test schema: %v", err)
		}
	}()

	testDSN, err := withQueryValue(adminDSN, "search_path", schema)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DatabaseConfig{
		Enabled: true, Required: true, URL: testDSN,
		MaxConns: 4, MinConns: 0, ConnectTimeoutSeconds: 10, HealthTimeoutSeconds: 2,
	}
	runner, err := migrations.NewRunner(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().Up(ctx); err != nil {
		_ = runner.Close()
		t.Fatal(err)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := persistence.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := persistence.NewJobRepository(db)

	now := time.Now().UTC().Truncate(time.Microsecond)
	jobID := uuid.NewString()
	req := &builder.BuildRequest{
		PackageName: "app-misc/jq", Version: "1.8.1", Arch: "amd64",
		ProfileID: "pe/amd64/base", RepositoryIDs: []string{"gentoo"},
		IdempotencyKey: strings.Join([]string{"db1", "integration", "001"}, "-"),
		ConfigBundle: &builder.ConfigBundle{
			Metadata: builder.BundleMetadata{CreatedAt: now.Format(time.RFC3339)},
		},
		ResolvedContext: &catalog.ResolvedBuildContext{
			ProfileID: "pe/amd64/base", ImageGeneration: "img-42", ResolvedAt: now,
		},
	}
	queued := &builder.BuildStatus{
		JobID: jobID, Status: "queued", PackageName: req.PackageName,
		Version: req.Version, Arch: req.Arch, CreatedAt: now, UpdatedAt: now,
		ResolvedContext: req.ResolvedContext, Request: req,
	}
	created, err := repo.CreateJob(ctx, req, queued)
	if err != nil || !created.Created || created.JobID != jobID {
		t.Fatalf("create result=%+v err=%v", created, err)
	}
	retryReq := *req
	retryContext := *req.ResolvedContext
	retryContext.ResolvedAt = now.Add(time.Minute)
	retryReq.ResolvedContext = &retryContext
	retryBundle := *req.ConfigBundle
	retryBundle.Metadata.CreatedAt = now.Add(time.Minute).Format(time.RFC3339)
	retryReq.ConfigBundle = &retryBundle
	retry, err := repo.CreateJob(ctx, &retryReq, queued)
	if err != nil || retry.Created || retry.JobID != jobID {
		t.Fatalf("idempotent retry result=%+v err=%v", retry, err)
	}
	conflictReq := *req
	conflictReq.PackageName = "app-misc/hello"
	conflictStatus := *queued
	conflictStatus.JobID = uuid.NewString()
	conflictStatus.PackageName = conflictReq.PackageName
	if _, err := repo.CreateJob(ctx, &conflictReq, &conflictStatus); err == nil || !strings.Contains(err.Error(), "different build request") {
		t.Fatalf("idempotency conflict error = %v", err)
	}
	if status := repo.Status(); status.WriteErrors != 0 || status.LastError != "" {
		t.Fatalf("idempotency conflict degraded ledger health: %+v", status)
	}

	claimed := *queued
	claimed.Status = "claimed"
	claimed.UpdatedAt = now.Add(time.Second)
	if err := repo.RecordTransition(ctx, queued, &claimed); err != nil {
		t.Fatal(err)
	}
	building := claimed
	building.Status = "building"
	building.InstanceID = "worker-1"
	building.UpdatedAt = now.Add(2 * time.Second)
	if err := repo.RecordTransition(ctx, &claimed, &building); err != nil {
		t.Fatal(err)
	}
	completed := building
	completed.Status = "completed"
	completed.ArtifactURL = "/binpkgs/app-misc/jq-1.8.1-1.gpkg.tar"
	completed.Artifacts = []string{completed.ArtifactURL}
	completed.UpdatedAt = now.Add(3 * time.Second)
	completed.Request = req
	if err := repo.RecordTransition(ctx, &building, &completed); err != nil {
		t.Fatal(err)
	}

	var state, attemptState, imageGeneration string
	var eventCount, outboxCount int
	if err := db.Pool().QueryRow(ctx, `
		SELECT state, request->'resolved_context'->>'image_generation'
		FROM build_jobs WHERE id = $1
	`, jobID).Scan(&state, &imageGeneration); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool().QueryRow(ctx, "SELECT state FROM build_attempts WHERE job_id = $1", jobID).Scan(&attemptState); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool().QueryRow(ctx, "SELECT count(*) FROM job_events WHERE job_id = $1", jobID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool().QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", jobID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if state != "completed" || attemptState != "completed" || imageGeneration != "img-42" || eventCount != 4 || outboxCount != 4 {
		t.Fatalf("ledger state=%s attempt=%s image=%s events=%d outbox=%d", state, attemptState, imageGeneration, eventCount, outboxCount)
	}

	report := repo.Reconcile(ctx, map[string]*builder.BuildStatus{jobID: &completed})
	if !report.Consistent || report.Extra != 0 || report.Mismatched != 0 {
		t.Fatalf("initial reconcile = %+v", report)
	}
	if _, err := db.Pool().Exec(ctx, "UPDATE build_jobs SET state = 'tampered' WHERE id = $1", jobID); err != nil {
		t.Fatal(err)
	}
	report = repo.Reconcile(ctx, map[string]*builder.BuildStatus{jobID: &completed})
	if !report.Consistent || report.Mismatched != 1 || report.Repaired != 1 {
		t.Fatalf("repair reconcile = %+v", report)
	}
	if err := db.Pool().QueryRow(ctx, "SELECT state FROM build_jobs WHERE id = $1", jobID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "completed" {
		t.Fatalf("state after repair = %q", state)
	}

	if err := repo.HideJob(ctx, &completed, "integration_test"); err != nil {
		t.Fatal(err)
	}
	var visible bool
	if err := db.Pool().QueryRow(ctx, "SELECT legacy_visible FROM build_jobs WHERE id = $1", jobID).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible {
		t.Fatal("hidden job remained legacy-visible")
	}
	if err := db.Pool().QueryRow(ctx, "SELECT count(*) FROM job_events WHERE job_id = $1", jobID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 6 {
		t.Fatalf("event count after reconcile+hide = %d", eventCount)
	}

	testDurableScheduler(t, ctx, db, repo, now.Add(time.Hour))
}

func testDurableScheduler(t *testing.T, ctx context.Context, db *persistence.Database, repo *persistence.JobRepository, now time.Time) {
	t.Helper()
	req := &builder.BuildRequest{
		PackageName: "app-misc/hello", Version: "2.12.1", Arch: "amd64",
		IdempotencyKey: strings.Join([]string{"db2", "scheduler", "001"}, "-"),
	}
	status := &builder.BuildStatus{
		JobID: uuid.NewString(), Status: "queued", PackageName: req.PackageName,
		Version: req.Version, Arch: req.Arch, CreatedAt: now, UpdatedAt: now, Request: req,
	}
	if _, err := repo.CreateJob(ctx, req, status); err != nil {
		t.Fatal(err)
	}

	first, err := repo.ClaimNext(ctx, "db2-worker-a", 100*time.Millisecond)
	if err != nil || first == nil || first.Status.JobID != status.JobID ||
		first.Status.AttemptID == "" || first.Status.FenceToken != 1 {
		t.Fatalf("first claim=%+v err=%v", first, err)
	}
	if second, err := repo.ClaimNext(ctx, "db2-worker-b", time.Second); err != nil || second != nil {
		t.Fatalf("concurrent duplicate claim=%+v err=%v", second, err)
	}
	// Leave enough headroom for PostgreSQL timestamp precision and loaded CI
	// hosts; this is an expiry semantic test, not a timer accuracy test.
	time.Sleep(350 * time.Millisecond)
	reclaimed, err := repo.ClaimNext(ctx, "db2-worker-b", time.Second)
	if err != nil || reclaimed == nil || reclaimed.Status.JobID != status.JobID ||
		reclaimed.Status.FenceToken != 2 || reclaimed.Status.AttemptID == first.Status.AttemptID {
		t.Fatalf("reclaim=%+v err=%v", reclaimed, err)
	}
	if err := repo.CheckClaim(ctx, first.Status); err == nil {
		t.Fatal("expired fence remained valid")
	}
	staleBuilding := *first.Status
	staleBuilding.Status = "building"
	staleBuilding.UpdatedAt = now.Add(time.Minute)
	if err := repo.RecordTransition(ctx, first.Status, &staleBuilding); err == nil {
		t.Fatal("stale executor changed durable job state")
	}
	if err := repo.RenewClaim(ctx, reclaimed.Status, time.Second); err != nil {
		t.Fatalf("renew active claim: %v", err)
	}
	building := *reclaimed.Status
	building.Status = "building"
	building.UpdatedAt = now.Add(2 * time.Minute)
	if err := repo.RecordTransition(ctx, reclaimed.Status, &building); err != nil {
		t.Fatal(err)
	}
	failed := building
	failed.Status = "failed"
	failed.Error = "controlled integration failure"
	failed.UpdatedAt = now.Add(3 * time.Minute)
	if err := repo.RecordTransition(ctx, &building, &failed); err != nil {
		t.Fatal(err)
	}
	if err := repo.CheckClaim(ctx, &failed); err == nil {
		t.Fatal("terminal attempt retained an active lease")
	}

	retried, err := repo.RetryJob(ctx, status.JobID)
	if err != nil || retried.Status != "queued" {
		t.Fatalf("retry=%+v err=%v", retried, err)
	}
	third, err := repo.ClaimNext(ctx, "db2-worker-c", time.Second)
	if err != nil || third == nil || third.Status.FenceToken != 3 {
		t.Fatalf("third claim=%+v err=%v", third, err)
	}
	canceled, err := repo.CancelJob(ctx, status.JobID, "operator test cancellation")
	if err != nil || canceled.Status != "canceled" {
		t.Fatalf("cancel=%+v err=%v", canceled, err)
	}
	if err := repo.CheckClaim(ctx, third.Status); err == nil {
		t.Fatal("canceled attempt retained an active lease")
	}
	if _, err := repo.RetryJob(ctx, status.JobID); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("attempt budget retry error=%v", err)
	}

	var attempts, leases, leaseExpiredEvents int
	if err := db.Pool().QueryRow(ctx, "SELECT count(*) FROM build_attempts WHERE job_id = $1", status.JobID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM worker_leases l
		JOIN build_attempts a ON a.id = l.attempt_id
		WHERE a.job_id = $1
	`, status.JobID).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM job_events WHERE job_id = $1 AND event_type = 'job.lease_expired'
	`, status.JobID).Scan(&leaseExpiredEvents); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || leases != 0 || leaseExpiredEvents != 1 {
		t.Fatalf("attempts=%d leases=%d lease_expired_events=%d", attempts, leases, leaseExpiredEvents)
	}

	testConcurrentClaims(t, ctx, db, repo, now.Add(2*time.Hour))
	testRuntimeMetadata(t, ctx, db, repo, now.Add(3*time.Hour))
	testCrossReplicaInfraCleanup(t, ctx, db, repo, now.Add(4*time.Hour))
}

func testConcurrentClaims(t *testing.T, ctx context.Context, db *persistence.Database, repo *persistence.JobRepository, now time.Time) {
	t.Helper()
	const jobCount = 24
	jobIDs := make(map[string]struct{}, jobCount)
	for i := 0; i < jobCount; i++ {
		req := &builder.BuildRequest{
			PackageName: "app-misc/hello", Version: "2.12.1", Arch: "amd64",
			IdempotencyKey: fmt.Sprintf("db2-contention-%02d-%s", i, uuid.NewString()),
		}
		status := &builder.BuildStatus{
			JobID: uuid.NewString(), Status: "queued", PackageName: req.PackageName,
			Version: req.Version, Arch: req.Arch,
			CreatedAt: now.Add(time.Duration(i) * time.Microsecond),
			UpdatedAt: now.Add(time.Duration(i) * time.Microsecond), Request: req,
		}
		if _, err := repo.CreateJob(ctx, req, status); err != nil {
			t.Fatal(err)
		}
		jobIDs[status.JobID] = struct{}{}
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed = make(map[string]string, jobCount)
		errs    []error
	)
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for {
				claim, err := repo.ClaimNext(ctx, fmt.Sprintf("db2-racer-%d", worker), 5*time.Second)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				if claim == nil {
					return
				}
				if _, ours := jobIDs[claim.Status.JobID]; !ours {
					// A previous scheduler test may have intentionally left a
					// queued row. Release it but exclude it from this assertion.
					failed := *claim.Status
					failed.Status, failed.Error = "failed", "contention test cleanup"
					failed.UpdatedAt = time.Now().UTC()
					_ = repo.RecordTransition(ctx, claim.Status, &failed)
					continue
				}
				mu.Lock()
				if owner, duplicate := claimed[claim.Status.JobID]; duplicate {
					errs = append(errs, fmt.Errorf("job %s claimed by %s and worker-%d", claim.Status.JobID, owner, worker))
				}
				claimed[claim.Status.JobID] = fmt.Sprintf("worker-%d", worker)
				mu.Unlock()

				failed := *claim.Status
				failed.Status, failed.Error = "failed", "controlled contention test completion"
				failed.UpdatedAt = time.Now().UTC()
				if err := repo.RecordTransition(ctx, claim.Status, &failed); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("concurrent claim errors: %v", errs)
	}
	if len(claimed) != jobCount {
		t.Fatalf("unique claims=%d want=%d", len(claimed), jobCount)
	}

	var attemptCount, activeLeases int
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*)
		FROM build_attempts
		WHERE job_id = ANY($1::uuid[])
	`, mapKeys(jobIDs)).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*)
		FROM worker_leases l
		JOIN build_attempts a ON a.id = l.attempt_id
		WHERE a.job_id = ANY($1::uuid[])
	`, mapKeys(jobIDs)).Scan(&activeLeases); err != nil {
		t.Fatal(err)
	}
	if attemptCount != jobCount || activeLeases != 0 {
		t.Fatalf("attempts=%d active_leases=%d", attemptCount, activeLeases)
	}

	runtime, err := repo.RuntimeStatus(ctx)
	if err != nil || runtime.Authority != "postgresql" || runtime.ExpiredLeases != 0 {
		t.Fatalf("runtime status=%+v err=%v", runtime, err)
	}
}

func testCrossReplicaInfraCleanup(t *testing.T, ctx context.Context, db *persistence.Database, repo *persistence.JobRepository, now time.Time) {
	t.Helper()
	req := &builder.BuildRequest{
		PackageName: "app-misc/jq", Version: "1.8.1", Arch: "amd64",
		IdempotencyKey: "infra-cleanup-" + uuid.NewString(),
	}
	queued := &builder.BuildStatus{
		JobID: uuid.NewString(), Status: "queued", PackageName: req.PackageName,
		Version: req.Version, Arch: req.Arch, CreatedAt: now, UpdatedAt: now, Request: req,
	}
	if _, err := repo.CreateJob(ctx, req, queued); err != nil {
		t.Fatal(err)
	}
	first, err := repo.ClaimNext(ctx, "cleanup-dead-replica", time.Minute)
	if err != nil || first == nil || first.Status.JobID != queued.JobID {
		t.Fatalf("first cleanup claim=%+v err=%v", first, err)
	}
	if err := repo.RecordInfra(ctx, first.Status, builder.InfraRecord{
		Provider: "pve", ProviderInstanceID: "pve-cleanup-" + uuid.NewString(),
		State: "provisioning", RemoteStateRef: "/shared/terraform/pve-cleanup",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE worker_leases SET expires_at = clock_timestamp() - interval '1 second'
		WHERE attempt_id = $1
	`, first.Status.AttemptID); err != nil {
		t.Fatal(err)
	}
	// ClaimNext performs lease recovery; the retry may be claimed immediately,
	// but it has a different attempt fence and cannot protect the old VM.
	if _, err := repo.ClaimNext(ctx, "cleanup-live-replica", time.Minute); err != nil {
		t.Fatal(err)
	}

	claim, err := repo.ClaimInfraCleanup(ctx, "replica-a/infra-cleaner", time.Minute)
	if err != nil || claim == nil || claim.CleanupFence != 1 {
		t.Fatalf("infra cleanup claim=%+v err=%v", claim, err)
	}
	if duplicate, err := repo.ClaimInfraCleanup(ctx, "replica-b/infra-cleaner", time.Minute); err != nil || duplicate != nil {
		t.Fatalf("duplicate cleanup claim=%+v err=%v", duplicate, err)
	}
	if err := repo.CompleteInfraCleanup(ctx, claim.ID, "wrong-owner", claim.CleanupFence); err == nil {
		t.Fatal("wrong cleanup owner unexpectedly completed the resource")
	}
	if err := repo.FailInfraCleanup(ctx, claim.ID, "replica-a/infra-cleaner", claim.CleanupFence, "synthetic destroy failure"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE infra_instances SET next_cleanup_at = clock_timestamp()
		WHERE id = $1
	`, claim.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := repo.ClaimInfraCleanup(ctx, "replica-b/infra-cleaner", time.Minute)
	if err != nil || retry == nil || retry.CleanupFence != 2 {
		t.Fatalf("infra cleanup retry=%+v err=%v", retry, err)
	}
	if err := repo.CompleteInfraCleanup(ctx, retry.ID, "replica-b/infra-cleaner", retry.CleanupFence); err != nil {
		t.Fatal(err)
	}
	var state string
	var deleted bool
	if err := db.Pool().QueryRow(ctx, `
		SELECT state, deleted_at IS NOT NULL FROM infra_instances WHERE id = $1
	`, retry.ID).Scan(&state, &deleted); err != nil {
		t.Fatal(err)
	}
	if state != "destroyed" || !deleted {
		t.Fatalf("cleanup result state=%q deleted=%v", state, deleted)
	}
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func testRuntimeMetadata(t *testing.T, ctx context.Context, db *persistence.Database, repo *persistence.JobRepository, now time.Time) {
	t.Helper()
	req := &builder.BuildRequest{
		PackageName: "sys-apps/baselayout", Version: "2.18", Arch: "amd64",
		IdempotencyKey: "db3-runtime-" + uuid.NewString(),
	}
	queued := &builder.BuildStatus{
		JobID: uuid.NewString(), Status: "queued", PackageName: req.PackageName,
		Version: req.Version, Arch: req.Arch, CreatedAt: now, UpdatedAt: now, Request: req,
	}
	if _, err := repo.CreateJob(ctx, req, queued); err != nil {
		t.Fatal(err)
	}
	if err := repo.AppendLogs(ctx, queued, []builder.LogRecord{{
		OccurredAt: now, Message: "[queued] durable log accepted",
	}}); err != nil {
		t.Fatal(err)
	}
	claim, err := repo.ClaimNext(ctx, "db3-worker", 5*time.Second)
	if err != nil || claim == nil || claim.Status.JobID != queued.JobID {
		t.Fatalf("metadata claim=%+v err=%v", claim, err)
	}
	if err := repo.RecordInfra(ctx, claim.Status, builder.InfraRecord{
		Provider: "pve", ProviderInstanceID: "pve-db3-test", State: "running",
		RemoteStateRef: "/var/lib/portage-engine/iac/pve-db3-test",
		Attributes:     map[string]string{"profile_id": "pe/amd64/base"},
	}); err != nil {
		t.Fatal(err)
	}
	artifact := builder.ArtifactRecord{
		Kind: "gentoo-binpkg", State: "staged",
		Digest: strings.Repeat("a", 64), SizeBytes: 1234,
		MediaType: "application/vnd.gentoo.gpkg",
		Location:  "/binpkgs/sys-apps/baselayout-2.18.gpkg.tar",
		Lineage:   map[string]any{"profile_id": "pe/amd64/base"},
	}
	if err := repo.RecordArtifacts(ctx, claim.Status, []builder.ArtifactRecord{artifact}); err != nil {
		t.Fatal(err)
	}
	crashArtifact := builder.ArtifactRecord{
		Kind: "gentoo-binpkg", State: "staged",
		Digest: strings.Repeat("e", 64), SizeBytes: 4321,
		MediaType: "application/vnd.gentoo.gpkg",
		Location:  "/binpkgs/sys-apps/baselayout-crash-window.gpkg.tar",
		Lineage:   map[string]any{"recovery": "external_move_before_finalize"},
	}
	if err := repo.RecordArtifacts(ctx, claim.Status, []builder.ArtifactRecord{crashArtifact}); err != nil {
		t.Fatal(err)
	}
	releasePromotion, err := repo.AcquireArtifactPromotion(ctx, claim.Status)
	if err != nil {
		t.Fatal(err)
	}
	blockedCtx, blockedCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, blockedErr := repo.AcquireArtifactPromotion(blockedCtx, claim.Status)
	blockedCancel()
	if blockedErr == nil {
		t.Fatal("second replica unexpectedly entered the publication critical section")
	}
	if err := releasePromotion(); err != nil {
		t.Fatal(err)
	}
	releasePromotion, err = repo.AcquireArtifactPromotion(ctx, claim.Status)
	if err != nil {
		t.Fatalf("publication lock was not released: %v", err)
	}
	if err := releasePromotion(); err != nil {
		t.Fatal(err)
	}
	if err := repo.AppendLogs(ctx, claim.Status, []builder.LogRecord{{
		OccurredAt: now.Add(30 * time.Second), Message: "[build] durable log from fenced attempt",
	}}); err != nil {
		t.Fatal(err)
	}
	publishedAt := now.Add(time.Minute)
	artifact.State, artifact.Published = "published", &publishedAt
	if err := repo.RecordArtifacts(ctx, claim.Status, []builder.ArtifactRecord{artifact}); err != nil {
		t.Fatal(err)
	}
	completed := *claim.Status
	completed.Status, completed.UpdatedAt = "completed", publishedAt
	if err := repo.RecordTransition(ctx, claim.Status, &completed); err != nil {
		t.Fatal(err)
	}
	deletedAt := publishedAt.Add(time.Minute)
	if err := repo.RecordInfra(ctx, &completed, builder.InfraRecord{
		Provider: "pve", ProviderInstanceID: "pve-db3-test", State: "destroyed",
		RemoteStateRef: "/var/lib/portage-engine/iac/pve-db3-test", DeletedAt: &deletedAt,
	}); err != nil {
		t.Fatalf("terminal infra record: %v", err)
	}
	if err := repo.AppendLogs(ctx, &completed, []builder.LogRecord{{
		OccurredAt: deletedAt, Message: "[cleanup] instance destroyed",
	}}); err != nil {
		t.Fatalf("terminal durable log: %v", err)
	}
	if err := repo.RecordArtifacts(ctx, &completed, []builder.ArtifactRecord{artifact}); err == nil {
		t.Fatal("terminal attempt unexpectedly rewrote artifact metadata without an active lease")
	}

	factoryStatus := &imagefactory.FactoryStatus{
		SchemaVersion: 1, UpdatedAt: now, OverallState: "passed",
		DesktopE2E: imagefactory.FactoryDesktopE2E{State: "passed"},
		Milestones: []imagefactory.FactoryMilestone{{
			ID: "IMG-DB3", Title: "DB-3 evidence", State: "passed",
			CompletedAt: &publishedAt,
			Evidence: []imagefactory.FactoryEvidence{{
				Label: "manifest", Digest: "sha256:" + strings.Repeat("b", 64),
				Path: "evidence/db3-manifest.json", SizeBytes: 42,
			}},
		}},
	}
	if err := repo.RecordFactoryStatus(ctx, "integration-status", factoryStatus); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordFactoryStatus(ctx, "integration-status", factoryStatus); err != nil {
		t.Fatalf("idempotent factory status: %v", err)
	}

	runtime, err := repo.RuntimeMetadataStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.LiveInfra != 0 || runtime.CleanupFailedInfra != 0 ||
		runtime.PublishedArtifacts != 1 || runtime.StagedArtifacts != 1 ||
		runtime.FactoryRuns != 1 || runtime.LastMetadataUpdateAt == nil {
		t.Fatalf("runtime metadata status=%+v", runtime)
	}
	report, err := repo.ReconcileArtifacts(ctx, []persistence.ArtifactInventory{
		{Location: artifact.Location, Digest: artifact.Digest, SizeBytes: artifact.SizeBytes},
		{Location: crashArtifact.Location, Digest: crashArtifact.Digest, SizeBytes: crashArtifact.SizeBytes},
	})
	if err != nil || report.Matched != 2 || report.Published != 2 || report.Missing != 0 || report.Corrupt != 0 {
		t.Fatalf("matching artifact reconciliation=%+v err=%v", report, err)
	}
	report, err = repo.ReconcileArtifacts(ctx, []persistence.ArtifactInventory{
		{Location: artifact.Location, Digest: strings.Repeat("c", 64), SizeBytes: artifact.SizeBytes},
		{Location: crashArtifact.Location, Digest: crashArtifact.Digest, SizeBytes: crashArtifact.SizeBytes},
	})
	if err != nil || report.Corrupt != 1 || report.Matched != 1 {
		t.Fatalf("corrupt artifact reconciliation=%+v err=%v", report, err)
	}
	report, err = repo.ReconcileArtifacts(ctx, []persistence.ArtifactInventory{
		{Location: artifact.Location, Digest: artifact.Digest, SizeBytes: artifact.SizeBytes},
		{Location: crashArtifact.Location, Digest: crashArtifact.Digest, SizeBytes: crashArtifact.SizeBytes},
		{Location: "/binpkgs/orphaned.gpkg.tar", Digest: strings.Repeat("d", 64), SizeBytes: 99},
	})
	if err != nil || report.Matched != 2 || report.Orphaned != 1 {
		t.Fatalf("recovered/orphaned artifact reconciliation=%+v err=%v", report, err)
	}
	runtime, err = repo.RuntimeMetadataStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.PublishedArtifacts != 2 || runtime.CorruptArtifacts != 0 ||
		runtime.MissingArtifacts != 0 || runtime.OrphanedArtifacts != 1 {
		t.Fatalf("reconciled runtime metadata status=%+v", runtime)
	}
	report, err = repo.ReconcileArtifacts(ctx, []persistence.ArtifactInventory{
		{Location: artifact.Location, Digest: artifact.Digest, SizeBytes: artifact.SizeBytes},
		{Location: crashArtifact.Location, Digest: crashArtifact.Digest, SizeBytes: crashArtifact.SizeBytes},
	})
	if err != nil || report.Orphaned != 0 {
		t.Fatalf("removed orphan reconciliation=%+v err=%v", report, err)
	}
	safeSetting := config.CloudSettings{
		Provider: "pve", PVEEndpoint: "https://pve.test:8006",
		PVETokenID: "builder@pve!runtime", BuildMode: "native-gentoo",
	}
	version, err := repo.SaveRuntimeSetting(
		ctx, "control-plane", "cloud", &safeSetting,
		map[string]string{"pve_token_secret": "env:CLOUD_PVE_TOKEN_SECRET"},
		"integration-test", "request-db4",
	)
	if err != nil || version != 1 {
		t.Fatalf("save runtime setting version=%d err=%v", version, err)
	}
	safeSetting.PVEEndpoint = "https://pve-updated.test:8006"
	version, err = repo.SaveRuntimeSetting(
		ctx, "control-plane", "cloud", &safeSetting,
		map[string]string{"pve_token_secret": "env:CLOUD_PVE_TOKEN_SECRET"},
		"integration-test", "request-db4-update",
	)
	if err != nil || version != 2 {
		t.Fatalf("update runtime setting version=%d err=%v", version, err)
	}
	var loadedSetting config.CloudSettings
	loadedVersion, refs, found, err := repo.LoadRuntimeSetting(
		ctx, "control-plane", "cloud", &loadedSetting,
	)
	if err != nil || !found || loadedVersion != 2 ||
		loadedSetting.PVEEndpoint != safeSetting.PVEEndpoint ||
		loadedSetting.PVETokenSecret != "" ||
		refs["pve_token_secret"] != "env:CLOUD_PVE_TOKEN_SECRET" {
		t.Fatalf("load runtime setting version=%d found=%v setting=%+v refs=%v err=%v",
			loadedVersion, found, loadedSetting, refs, err)
	}
	created, err := repo.EnsureRuntimeSetting(
		ctx, "control-plane", "bootstrap-test", &safeSetting, nil, "integration-bootstrap",
	)
	if err != nil || !created {
		t.Fatalf("first runtime setting ensure created=%v err=%v", created, err)
	}
	conflicting := safeSetting
	conflicting.PVEEndpoint = "https://must-not-overwrite.test:8006"
	created, err = repo.EnsureRuntimeSetting(
		ctx, "control-plane", "bootstrap-test", &conflicting, nil, "integration-bootstrap-race",
	)
	if err != nil || created {
		t.Fatalf("second runtime setting ensure created=%v err=%v", created, err)
	}
	var bootstrapped config.CloudSettings
	_, _, found, err = repo.LoadRuntimeSetting(ctx, "control-plane", "bootstrap-test", &bootstrapped)
	if err != nil || !found || bootstrapped.PVEEndpoint != safeSetting.PVEEndpoint {
		t.Fatalf("bootstrap winner was overwritten: setting=%+v found=%v err=%v", bootstrapped, found, err)
	}
	var settingAudits int
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM audit_events
		WHERE action = 'runtime_setting.updated'
		  AND resource_id = 'control-plane/cloud'
	`).Scan(&settingAudits); err != nil || settingAudits != 2 {
		t.Fatalf("runtime setting audits=%d err=%v", settingAudits, err)
	}
	var infraFence int64
	var artifactAttempt string
	if err := db.Pool().QueryRow(ctx, `
		SELECT fence_token FROM infra_instances WHERE provider = 'pve' AND provider_instance_id = 'pve-db3-test'
	`).Scan(&infraFence); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool().QueryRow(ctx, `
		SELECT attempt_id::text FROM artifacts WHERE location = '/binpkgs/sys-apps/baselayout-2.18.gpkg.tar'
	`).Scan(&artifactAttempt); err != nil {
		t.Fatal(err)
	}
	if infraFence != claim.Status.FenceToken || artifactAttempt != claim.Status.AttemptID {
		t.Fatalf("infra fence=%d artifact attempt=%s claim=%+v", infraFence, artifactAttempt, claim.Status)
	}
	logOutput, err := repo.LoadLogs(ctx, queued.JobID)
	if err != nil || !strings.Contains(logOutput, "durable log accepted") ||
		!strings.Contains(logOutput, "fenced attempt") ||
		!strings.Contains(logOutput, "instance destroyed") {
		t.Fatalf("durable log=%q err=%v", logOutput, err)
	}
}

func withQueryValue(dsn, key, value string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse test database URL: %w", err)
	}
	query := parsed.Query()
	query.Set(key, value)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
