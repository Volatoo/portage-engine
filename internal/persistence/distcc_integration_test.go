package persistence_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/internal/distcc"
	"github.com/slchris/portage-engine/internal/migrations"
	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/pkg/config"
)

func TestPostgresDistributedBuildAlpha(t *testing.T) {
	adminDSN := os.Getenv("PORTAGE_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("set PORTAGE_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	schema := "distcc_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	defer func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_, _ = admin.Exec(dropCtx, "DROP SCHEMA "+identifier+" CASCADE")
	}()
	testDSN, err := withSearchPath(adminDSN, schema)
	if err != nil {
		t.Fatal(err)
	}
	databaseConfig := config.DatabaseConfig{
		Enabled: true, Required: true, URL: testDSN, MaxConns: 12,
		ConnectTimeoutSeconds: 10, HealthTimeoutSeconds: 2,
	}
	runner, err := migrations.NewRunner(ctx, databaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().Up(ctx); err != nil {
		_ = runner.Close()
		t.Fatalf("apply schema through reserved migration 29: %v", err)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := persistence.Open(ctx, databaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if health := db.Check(ctx); !health.OK ||
		health.SchemaVersion != persistence.MaxSchemaVersion {
		t.Fatalf("schema health=%+v", health)
	}

	iamRepo := persistence.NewIAMRepository(db)
	project, err := iamRepo.CreateProject(ctx, "distcc-alpha", "distcc integration")
	if err != nil {
		t.Fatal(err)
	}
	repo := persistence.NewJobRepository(db)
	request := &builder.BuildRequest{
		ProjectID: project.ID, PackageName: "sys-devel/llvm", Arch: "amd64",
		IdempotencyKey: "distcc-" + uuid.NewString(),
	}
	status := &builder.BuildStatus{
		JobID: uuid.NewString(), ProjectID: project.ID, Status: "queued",
		PackageName: request.PackageName, Arch: request.Arch,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Request: request,
	}
	if _, err := repo.CreateJob(ctx, request, status); err != nil {
		t.Fatal(err)
	}
	claim, err := repo.ClaimNext(ctx, "distcc-admission", time.Minute, legacyExecutorCapabilities)
	if err != nil || claim == nil || claim.Status.JobID != status.JobID {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}

	pool := distcc.Pool{
		Architecture: "amd64", CHOST: "x86_64-pc-linux-gnu",
		CompilerDigest:           "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ToolchainImageGeneration: "gentoo-g1", CPUFeatures: []string{"avx2"},
		NetworkZone: "build-a", ProjectTrustDomain: project.ID,
	}
	worker, err := repo.HeartbeatCompileWorker(ctx, distcc.WorkerHeartbeat{
		StableName: "compile-a", Endpoint: "10.44.0.10:3632",
		MaxSlots: 2, Pool: pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	mutated := pool
	mutated.CPUFeatures = []string{"avx512f"}
	if _, err := repo.HeartbeatCompileWorker(ctx, distcc.WorkerHeartbeat{
		StableName: "compile-a", Endpoint: "10.44.0.10:3632",
		MaxSlots: 2, Pool: mutated,
	}); err == nil {
		t.Fatal("stable compile worker changed exact pool dimensions")
	}

	base := distcc.ReservationRequest{
		ProjectID: project.ID, JobID: claim.Status.JobID,
		AttemptID: claim.Status.AttemptID, AttemptFence: claim.Status.FenceToken, Slots: 1,
		LeaseDuration: time.Minute, WorkerFreshness: time.Minute,
		Pool: pool, FallbackPolicy: distcc.FallbackLocal,
	}
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		leases []*distcc.Lease
		errs   []error
	)
	for index := 0; index < 12; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			request := base
			request.BuilderID = fmt.Sprintf("builder-%d", index)
			request.BuilderNetworkIdentity = fmt.Sprintf("worker-cert:%d", index)
			lease, reserveErr := repo.ReserveCompileSlots(ctx, request)
			mu.Lock()
			defer mu.Unlock()
			if reserveErr != nil {
				errs = append(errs, reserveErr)
			} else if lease != nil {
				leases = append(leases, lease)
			}
		}(index)
	}
	wg.Wait()
	if len(errs) != 0 || len(leases) != 2 {
		t.Fatalf("atomic reservations=%d errors=%v", len(leases), errs)
	}
	var liveSlots int
	if err := db.Pool().QueryRow(ctx, `
		SELECT COALESCE(sum(slots), 0) FROM compile_slot_leases
		WHERE worker_id = $1 AND state = 'active'
		  AND expires_at > clock_timestamp()
	`, worker.ID).Scan(&liveSlots); err != nil || liveSlots != 2 {
		t.Fatalf("live slots=%d err=%v", liveSlots, err)
	}
	if err := repo.CheckCompileLease(ctx, *leases[0], time.Minute); err != nil {
		t.Fatal(err)
	}
	stale := *leases[0]
	stale.Fence++
	if err := repo.HeartbeatCompileLease(ctx, stale, time.Minute, time.Minute); err == nil {
		t.Fatal("stale compile fence renewed a lease")
	}
	if err := repo.RecordCompileObservation(ctx, *leases[0], distcc.Observation{
		Outcome: distcc.OutcomeRemote, Count: 7, NetworkBytes: 4096,
		QueueMillis: leases[0].QueueTime.Milliseconds(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordCompileObservation(ctx, *leases[0], distcc.Observation{
		Outcome: distcc.OutcomeHit, Count: 6,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReleaseCompileLease(ctx, *leases[0], ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReleaseCompileLease(ctx, stale, "lease-fenced"); err == nil {
		t.Fatal("stale fence released compile capacity")
	}
	requestAfterRelease := base
	requestAfterRelease.BuilderID = "builder-after-release"
	requestAfterRelease.BuilderNetworkIdentity = "worker-cert:after-release"
	replacement, err := repo.ReserveCompileSlots(ctx, requestAfterRelease)
	if err != nil || replacement == nil || replacement.Fence <= leases[0].Fence {
		t.Fatalf("replacement=%+v err=%v", replacement, err)
	}
	metrics, err := repo.CompileMetrics(ctx, time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.WorkersFresh != 1 || metrics.SlotsTotal != 2 ||
		metrics.SlotsLeased != 2 || metrics.RemoteCompiles != 7 ||
		metrics.Hits != 6 || metrics.NetworkBytes != 4096 {
		t.Fatalf("unexpected distcc metrics %+v", metrics)
	}
}
