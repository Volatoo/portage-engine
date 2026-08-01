package persistence_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/slchris/portage-engine/internal/iam"
	"github.com/slchris/portage-engine/internal/imagefactory"
	"github.com/slchris/portage-engine/internal/migrations"
	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/internal/signing"
	"github.com/slchris/portage-engine/internal/workergateway"
	"github.com/slchris/portage-engine/pkg/config"
)

var legacyExecutorCapabilities = []string{
	"context:legacy",
	"phase:provision",
	"phase:build",
	"phase:verify",
	"phase:publish",
}

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

	testDSN, err := withSearchPath(adminDSN, schema)
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

	iamRepo := persistence.NewIAMRepository(db)
	alice, err := iamRepo.ObserveSubject(ctx, iam.ExternalIdentity{
		Issuer: "https://issuer.example.test", Subject: "alice",
		PreferredUsername: "alice",
	}, false)
	if err != nil {
		t.Fatalf("observe IAM subject: %v", err)
	}
	sessionNow := time.Now().UTC()
	rawPlatformSession := "pe1_postgres-integration-session"
	platformDigest := sha256.Sum256([]byte(rawPlatformSession))
	sessionIdentity := iam.ExternalIdentity{
		Issuer: alice.Issuer, Subject: alice.Subject,
		TokenHash:         hex.EncodeToString(platformDigest[:]),
		ProviderSessionID: "provider-session-alice",
		ProviderTokenID:   "token-alice-1",
		IssuedAt:          sessionNow.Add(-time.Minute),
		AuthenticatedAt:   sessionNow.Add(-2 * time.Minute),
		ExpiresAt:         sessionNow.Add(time.Hour),
		ACR:               "urn:test:mfa", AMR: []string{"pwd", "otp"},
	}
	alice, err = iamRepo.AuthorizeSession(
		ctx, alice, sessionIdentity,
		persistence.IAMSessionPolicy{
			IdleTimeout: time.Hour, MaxLifetime: 12 * time.Hour,
		},
	)
	if err != nil || alice.SessionID == "" || alice.ACR != "urn:test:mfa" ||
		len(alice.AMR) != 2 {
		t.Fatalf("authorize IAM session = %+v, %v", alice, err)
	}
	if platformPrincipal, err := iamRepo.AuthorizePlatformSession(
		ctx, rawPlatformSession,
		persistence.IAMSessionPolicy{
			IdleTimeout: time.Hour, MaxLifetime: 12 * time.Hour,
		},
	); err != nil || platformPrincipal.SubjectID != alice.SubjectID ||
		platformPrincipal.Authentication != "federated-session" {
		t.Fatalf("authorize platform session = %+v, %v", platformPrincipal, err)
	}
	testDeviceAuthorizationAtomicConsumption(
		t, ctx, db, iamRepo, alice, rawPlatformSession,
	)
	expiredIdentity := sessionIdentity
	expiredIdentity.TokenHash = strings.Repeat("d", 64)
	expiredIdentity.IssuedAt = sessionNow.Add(-2 * time.Hour)
	expiredIdentity.ExpiresAt = sessionNow.Add(-time.Hour)
	if _, err := iamRepo.AuthorizeSession(
		ctx, alice, expiredIdentity,
		persistence.IAMSessionPolicy{
			IdleTimeout: time.Hour, MaxLifetime: 12 * time.Hour,
		},
	); err == nil || !strings.Contains(err.Error(), "lifecycle policy") {
		t.Fatalf("expired new IAM session was accepted: %v", err)
	}
	overAgeIdentity := sessionIdentity
	overAgeIdentity.TokenHash = strings.Repeat("e", 64)
	overAgeIdentity.IssuedAt = sessionNow.Add(-13 * time.Hour)
	overAgeIdentity.ExpiresAt = sessionNow.Add(time.Minute)
	if _, err := iamRepo.AuthorizeSession(
		ctx, alice, overAgeIdentity,
		persistence.IAMSessionPolicy{
			IdleTimeout: time.Hour, MaxLifetime: 12 * time.Hour,
		},
	); err == nil || !strings.Contains(err.Error(), "lifetime exceeds") {
		t.Fatalf("over-age new IAM session was accepted: %v", err)
	}
	sessions, err := iamRepo.ListSessions(ctx, alice.SubjectID)
	if err != nil || len(sessions) != 1 || sessions[0].ID != alice.SessionID {
		t.Fatalf("list IAM sessions = %+v, %v", sessions, err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE iam_sessions
		SET last_seen_at = clock_timestamp() - interval '2 hours'
		WHERE id = $1
	`, alice.SessionID); err != nil {
		t.Fatalf("age IAM session: %v", err)
	}
	if _, err := iamRepo.AuthorizeSession(
		ctx, alice, sessionIdentity,
		persistence.IAMSessionPolicy{
			IdleTimeout: time.Hour, MaxLifetime: 12 * time.Hour,
		},
	); err == nil || !strings.Contains(err.Error(), "lifecycle policy") {
		t.Fatalf("idle-expired IAM session was accepted: %v", err)
	}
	if _, err := iamRepo.RevokeSession(
		ctx, alice, alice.SessionID, "integration-test", false,
	); err != nil {
		t.Fatalf("revoke IAM session: %v", err)
	}
	if _, err := iamRepo.AuthorizeSession(
		ctx, alice, sessionIdentity,
		persistence.IAMSessionPolicy{
			IdleTimeout: time.Hour, MaxLifetime: 12 * time.Hour,
		},
	); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked IAM session was accepted: %v", err)
	}
	secondIdentity := sessionIdentity
	secondIdentity.TokenHash = strings.Repeat("b", 64)
	secondIdentity.ProviderTokenID = "token-alice-2"
	secondIdentity.IssuedAt = sessionNow.Add(-30 * time.Second)
	secondIdentity.ExpiresAt = sessionNow.Add(time.Hour)
	alice, err = iamRepo.AuthorizeSession(
		ctx, alice, secondIdentity,
		persistence.IAMSessionPolicy{
			IdleTimeout: time.Hour, MaxLifetime: 12 * time.Hour,
		},
	)
	if err != nil {
		t.Fatalf("authorize replacement IAM session: %v", err)
	}
	logout := iam.LogoutIdentity{
		Issuer: alice.Issuer, Subject: alice.Subject,
		ProviderSessionID: secondIdentity.ProviderSessionID,
		ProviderTokenID:   "logout-integration-1",
		IssuedAt:          sessionNow, ExpiresAt: sessionNow.Add(5 * time.Minute),
	}
	if result, err := iamRepo.ApplyBackchannelLogout(ctx, logout); err != nil ||
		result.Duplicate || result.RevokedSessions != 1 {
		t.Fatalf("apply back-channel logout = %+v, %v", result, err)
	}
	if result, err := iamRepo.ApplyBackchannelLogout(ctx, logout); err != nil ||
		!result.Duplicate || result.RevokedSessions != 1 {
		t.Fatalf("replay back-channel logout = %+v, %v", result, err)
	}
	thirdIdentity := sessionIdentity
	thirdIdentity.TokenHash = strings.Repeat("f", 64)
	thirdIdentity.ProviderSessionID = "provider-session-alice-2"
	thirdIdentity.ProviderTokenID = "token-alice-3"
	thirdIdentity.IssuedAt = sessionNow.Add(-15 * time.Second)
	thirdIdentity.ExpiresAt = sessionNow.Add(time.Hour)
	alice, err = iamRepo.AuthorizeSession(
		ctx, alice, thirdIdentity,
		persistence.IAMSessionPolicy{
			IdleTimeout: time.Hour, MaxLifetime: 12 * time.Hour,
		},
	)
	if err != nil {
		t.Fatalf("authorize post-logout IAM session: %v", err)
	}
	if count, err := iamRepo.RevokeAllSessions(
		ctx, alice, alice.SubjectID, "integration-revoke-all", false,
	); err != nil || count != 1 {
		t.Fatalf("revoke all IAM sessions count=%d err=%v", count, err)
	}
	unseenOldIdentity := sessionIdentity
	unseenOldIdentity.TokenHash = strings.Repeat("c", 64)
	unseenOldIdentity.ProviderTokenID = "unseen-old-token"
	unseenOldIdentity.IssuedAt = sessionNow.Add(-10 * time.Minute)
	unseenOldIdentity.ExpiresAt = sessionNow.Add(time.Hour)
	if _, err := iamRepo.AuthorizeSession(
		ctx, alice, unseenOldIdentity,
		persistence.IAMSessionPolicy{
			IdleTimeout: time.Hour, MaxLifetime: 12 * time.Hour,
		},
	); err == nil || !strings.Contains(err.Error(), "watermark") {
		t.Fatalf("unseen pre-watermark token was accepted: %v", err)
	}
	alpha, err := iamRepo.CreateProject(ctx, "alpha-project", "IAM integration fixture")
	if err != nil {
		t.Fatalf("create IAM project: %v", err)
	}
	if _, err := iamRepo.PutMembership(
		ctx, alpha.ID, alice.Issuer, alice.Subject, iam.RoleDeveloper, "",
	); err != nil {
		t.Fatalf("grant IAM project membership: %v", err)
	}
	if access, err := iamRepo.ResolveProject(ctx, alice, alpha.ID, iam.RoleDeveloper); err != nil ||
		access.ProjectID != alpha.ID || access.Role != iam.RoleDeveloper {
		t.Fatalf("resolve authorized project = %+v, %v", access, err)
	}
	if _, err := iamRepo.ResolveProject(ctx, alice, alpha.ID, iam.RoleMaintainer); err == nil {
		t.Fatal("developer unexpectedly received maintainer access")
	}
	bob, err := iamRepo.ObserveSubject(ctx, iam.ExternalIdentity{
		Issuer: "https://issuer.example.test", Subject: "bob",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iamRepo.PutMembership(
		ctx, alpha.ID, bob.Issuer, bob.Subject, iam.RoleOwner, "",
	); err != nil {
		t.Fatal(err)
	}
	if err := iamRepo.DeleteMembership(ctx, alpha.ID, bob.SubjectID); err == nil ||
		!strings.Contains(err.Error(), "last owner") {
		t.Fatalf("last project owner removal was not rejected: %v", err)
	}

	emptyRuntime, err := persistence.NewJobRepository(db).RuntimeStatus(ctx)
	if err != nil || !emptyRuntime.TargetHistory.Projection.Valid ||
		emptyRuntime.TargetHistory.Projection.State != "empty" ||
		emptyRuntime.TargetHistory.Projection.SourceWatermarkPresent ||
		emptyRuntime.TargetHistory.Projection.LagSeconds != 0 ||
		emptyRuntime.LeaseExpiries != (builder.LeaseExpiryStatus{}) {
		t.Fatalf("empty scheduler observability snapshot=%+v err=%v", emptyRuntime, err)
	}

	jobRepo := persistence.NewJobRepository(db)
	t.Run("monitor terminal event watermark fallback", func(t *testing.T) {
		testMonitorTerminalEventWatermarkFallback(t, ctx, db, jobRepo)
	})
	now := time.Now().UTC()
	iamJobID := uuid.NewString()
	iamRequest := &builder.BuildRequest{
		ProjectID: alpha.ID, RequestedBy: alice.SubjectID,
		PackageName: "app-misc/iam-fixture", Arch: "amd64",
		ResourceClass: "small",
		MachineSpec: map[string]string{
			"cores": "2", "memory": "2048", "disk_size": "20",
		},
		IdempotencyKey: "iam-project-scope",
	}
	iamStatus := &builder.BuildStatus{
		JobID: iamJobID, ProjectID: alpha.ID, RequestedBy: alice.SubjectID,
		Status: "queued", PackageName: iamRequest.PackageName, Arch: "amd64",
		CreatedAt: now, UpdatedAt: now, Request: iamRequest,
	}
	if result, err := jobRepo.CreateJob(ctx, iamRequest, iamStatus); err != nil || !result.Created {
		t.Fatalf("create project-owned job = %+v, %v", result, err)
	}
	var storedProject, storedSubject string
	if err := db.Pool().QueryRow(ctx, `
		SELECT project_id::text, requested_by_subject_id::text
		FROM build_jobs WHERE id = $1
	`, iamJobID).Scan(&storedProject, &storedSubject); err != nil {
		t.Fatal(err)
	}
	if storedProject != alpha.ID || storedSubject != alice.SubjectID {
		t.Fatalf("durable IAM ownership = project %s subject %s", storedProject, storedSubject)
	}
	if err := iamRepo.RecordAudit(ctx, persistence.AuditRecord{
		Principal: alice, Action: "build.submit", ResourceType: "build_job",
		ResourceID: iamJobID, ProjectID: alpha.ID, RequestID: "iam-fixture",
		SourceIP: "127.0.0.1", Outcome: "success",
		Detail: map[string]any{"package": iamRequest.PackageName},
	}); err != nil {
		t.Fatalf("record project audit event: %v", err)
	}
	var auditRows int
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*)
		FROM audit_events
		WHERE actor_subject_id = $1 AND project_id = $2
		  AND resource_id = $3 AND outcome = 'success'
	`, alice.SubjectID, alpha.ID, iamJobID).Scan(&auditRows); err != nil || auditRows != 1 {
		t.Fatalf("project audit rows=%d err=%v", auditRows, err)
	}

	policy, err := iamRepo.GetProjectPolicy(ctx, alpha.ID)
	if err != nil || policy.QueuedJobs != 1 || policy.SubmissionsToday != 1 {
		t.Fatalf("initial project policy=%+v err=%v", policy, err)
	}
	policy, err = iamRepo.UpdateProjectPolicy(ctx, alpha.ID, persistence.ProjectPolicyUpdate{
		Version: policy.Version, MaxQueuedJobs: 2, MaxActiveJobs: 2,
		MaxDailySubmissions: 3, MaxActiveVCPUs: 4,
		MaxActiveMemoryMiB: 8192, MaxActiveDiskGiB: 50,
	}, alice.SubjectID)
	if err != nil || policy.Version != 2 || policy.MaxActiveJobs != 2 {
		t.Fatalf("updated project policy=%+v err=%v", policy, err)
	}
	if _, err := iamRepo.UpdateProjectPolicy(ctx, alpha.ID, persistence.ProjectPolicyUpdate{
		Version: 1, MaxQueuedJobs: 10, MaxActiveJobs: 10, MaxDailySubmissions: 10,
		MaxActiveVCPUs: 10, MaxActiveMemoryMiB: 16384, MaxActiveDiskGiB: 100,
	}, alice.SubjectID); err == nil || !strings.Contains(err.Error(), "version conflict") {
		t.Fatalf("stale policy replacement was not rejected: %v", err)
	}
	if duplicate, err := jobRepo.CreateJob(ctx, iamRequest, iamStatus); err != nil || duplicate.Created {
		t.Fatalf("idempotent admission bypass=%+v err=%v", duplicate, err)
	}
	secondRequest := &builder.BuildRequest{
		ProjectID: alpha.ID, RequestedBy: alice.SubjectID,
		PackageName: "app-misc/alpha-second", Arch: "amd64",
		IdempotencyKey: "iam-project-second",
	}
	secondStatus := &builder.BuildStatus{
		JobID: uuid.NewString(), ProjectID: alpha.ID, RequestedBy: alice.SubjectID,
		Status: "queued", PackageName: secondRequest.PackageName, Arch: "amd64",
		CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second), Request: secondRequest,
	}
	if result, err := jobRepo.CreateJob(ctx, secondRequest, secondStatus); err != nil || !result.Created {
		t.Fatalf("second project job=%+v err=%v", result, err)
	}
	queuedLimitRequest := &builder.BuildRequest{
		ProjectID: alpha.ID, RequestedBy: alice.SubjectID,
		PackageName: "app-misc/queued-limit", Arch: "amd64",
		IdempotencyKey: "iam-project-queued-limit",
	}
	queuedLimitStatus := &builder.BuildStatus{
		JobID: uuid.NewString(), ProjectID: alpha.ID, RequestedBy: alice.SubjectID,
		Status: "queued", PackageName: queuedLimitRequest.PackageName, Arch: "amd64",
		CreatedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(2 * time.Second),
		Request: queuedLimitRequest,
	}
	_, err = jobRepo.CreateJob(ctx, queuedLimitRequest, queuedLimitStatus)
	admission, ok := builder.AsAdmissionError(err)
	if !ok || admission.Code != "queued_limit_reached" || admission.Used != 2 {
		t.Fatalf("queued admission error=%+v raw=%v", admission, err)
	}

	var (
		quotaClaims [2]*builder.SchedulerClaim
		quotaErrors [2]error
		quotaWG     sync.WaitGroup
		quotaStart  = make(chan struct{})
	)
	for index := range quotaClaims {
		quotaWG.Add(1)
		go func(index int) {
			defer quotaWG.Done()
			<-quotaStart
			quotaClaims[index], quotaErrors[index] = jobRepo.ClaimNext(
				ctx, fmt.Sprintf("iam-quota-worker-%d", index), time.Minute,
			)
		}(index)
	}
	close(quotaStart)
	quotaWG.Wait()
	var firstClaim *builder.SchedulerClaim
	for index, claim := range quotaClaims {
		if quotaErrors[index] != nil {
			t.Fatalf("concurrent quota claim %d: %v", index, quotaErrors[index])
		}
		if claim != nil {
			if firstClaim != nil {
				t.Fatalf("resource budget oversold: claims=%+v", quotaClaims)
			}
			firstClaim = claim
		}
	}
	if firstClaim == nil || firstClaim.Status.ProjectID != alpha.ID {
		t.Fatalf("concurrent resource quota claims=%+v", quotaClaims)
	}
	expectedVCPUs, expectedMemoryMiB, expectedDiskGiB := 4, 8192, 50
	if firstClaim.Request.ResourceClass == "small" {
		expectedVCPUs, expectedMemoryMiB, expectedDiskGiB = 2, 2048, 20
	}
	policy, err = iamRepo.GetProjectPolicy(ctx, alpha.ID)
	if err != nil || policy.ReservedVCPUs != expectedVCPUs ||
		policy.ReservedMemoryMiB != expectedMemoryMiB ||
		policy.ReservedDiskGiB != expectedDiskGiB || policy.ClaimedReservations != 1 {
		t.Fatalf("claimed resource usage=%+v err=%v", policy, err)
	}
	if blockedClaim, err := jobRepo.ClaimNext(ctx, "iam-quota-worker-b", time.Minute); err != nil || blockedClaim != nil {
		t.Fatalf("resource cap claim=%+v err=%v", blockedClaim, err)
	}
	firstProvisioning := *firstClaim.Status
	firstProvisioning.Status = "provisioning"
	firstProvisioning.UpdatedAt = now.Add(250 * time.Millisecond)
	if err := jobRepo.RecordTransition(ctx, firstClaim.Status, &firstProvisioning); err != nil {
		t.Fatal(err)
	}
	policy, err = iamRepo.GetProjectPolicy(ctx, alpha.ID)
	if err != nil || policy.ProvisionReservations != 1 || policy.ClaimedReservations != 0 {
		t.Fatalf("provision resource phase=%+v err=%v", policy, err)
	}
	firstFailed := firstProvisioning
	firstFailed.Status, firstFailed.Error = "failed", "quota test release"
	firstFailed.UpdatedAt = now.Add(3 * time.Second)
	if err := jobRepo.RecordTransition(ctx, &firstProvisioning, &firstFailed); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	secondClaim, err := jobRepo.ClaimNext(ctx, "iam-quota-worker-b", time.Minute)
	if err != nil || secondClaim == nil || secondClaim.Status.ProjectID != alpha.ID {
		t.Fatalf("second quota claim=%+v err=%v", secondClaim, err)
	}
	secondFailed := *secondClaim.Status
	secondFailed.Status, secondFailed.Error = "failed", "quota test release"
	secondFailed.UpdatedAt = now.Add(4 * time.Second)
	if err := jobRepo.RecordTransition(ctx, secondClaim.Status, &secondFailed); err != nil {
		t.Fatal(err)
	}
	policy, err = iamRepo.GetProjectPolicy(ctx, alpha.ID)
	if err != nil || policy.ReservedVCPUs != 0 || policy.ReservedMemoryMiB != 0 ||
		policy.ReservedDiskGiB != 0 {
		t.Fatalf("released resource usage=%+v err=%v", policy, err)
	}
	if result, err := jobRepo.CreateJob(ctx, queuedLimitRequest, queuedLimitStatus); err != nil || !result.Created {
		t.Fatalf("third daily submission=%+v err=%v", result, err)
	}
	dailyLimitRequest := *queuedLimitRequest
	dailyLimitRequest.PackageName = "app-misc/daily-limit"
	dailyLimitRequest.IdempotencyKey = "iam-project-daily-limit"
	dailyLimitStatus := *queuedLimitStatus
	dailyLimitStatus.JobID = uuid.NewString()
	dailyLimitStatus.PackageName = dailyLimitRequest.PackageName
	dailyLimitStatus.Request = &dailyLimitRequest
	_, err = jobRepo.CreateJob(ctx, &dailyLimitRequest, &dailyLimitStatus)
	admission, ok = builder.AsAdmissionError(err)
	if !ok || admission.Code != "daily_submission_limit_reached" || admission.Used != 3 {
		t.Fatalf("daily admission error=%+v raw=%v", admission, err)
	}
	policy, err = iamRepo.UpdateProjectPolicy(ctx, alpha.ID, persistence.ProjectPolicyUpdate{
		Version: policy.Version, Suspended: true, MaxQueuedJobs: 2,
		MaxActiveJobs: 2, MaxDailySubmissions: 3,
		MaxActiveVCPUs: 4, MaxActiveMemoryMiB: 8192, MaxActiveDiskGiB: 50,
	}, alice.SubjectID)
	if err != nil || !policy.Suspended {
		t.Fatalf("suspend policy=%+v err=%v", policy, err)
	}
	if duplicate, err := jobRepo.CreateJob(ctx, queuedLimitRequest, queuedLimitStatus); err != nil || duplicate.Created {
		t.Fatalf("suspended idempotent replay=%+v err=%v", duplicate, err)
	}
	suspendedRequest := dailyLimitRequest
	suspendedRequest.IdempotencyKey = "iam-project-suspended"
	suspendedStatus := dailyLimitStatus
	suspendedStatus.JobID = uuid.NewString()
	suspendedStatus.Request = &suspendedRequest
	_, err = jobRepo.CreateJob(ctx, &suspendedRequest, &suspendedStatus)
	admission, ok = builder.AsAdmissionError(err)
	if !ok || admission.Code != "project_suspended" {
		t.Fatalf("suspended admission error=%+v raw=%v", admission, err)
	}
	testResourceReservationReleasePaths(t, ctx, db, iamRepo, jobRepo)
	testRuntimeBudgetsAndAbuseGate(t, ctx, db, iamRepo, jobRepo)
	testArtifactBudgetLifecycle(t, ctx, iamRepo, jobRepo)
	testProjectPhaseCaps(t, ctx, iamRepo, jobRepo)
	testDurablePhaseWorkQueue(t, ctx, db, iamRepo, jobRepo)
	testExecutorCapabilityRouting(t, ctx, db, iamRepo, jobRepo)
	testActivePhaseContextAndFinalization(t, ctx, db, iamRepo, jobRepo)
	testDurableWorkerGatewaySpool(t, ctx, db, iamRepo, jobRepo)
	testConcurrentIdempotentAdmission(t, ctx, db, iamRepo, jobRepo)

	legacyWorkerID, currentWorkerID, fencedJobID := uuid.New(), uuid.New(), uuid.New()
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO workers (id, stable_name, max_slots, executor_protocol)
		VALUES ($1, $2, 1, 0), ($3, $4, 1, $5)
	`, legacyWorkerID, "legacy-executor-"+uuid.NewString(),
		currentWorkerID, "current-executor-"+uuid.NewString(),
		builder.ExecutorProtocolVersion); err != nil {
		t.Fatalf("seed executor protocol fence: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO build_jobs (
			id, project_id, package_atom, state, request, request_digest,
			minimum_executor_protocol
		) VALUES (
			$1, (SELECT id FROM projects WHERE name = 'default'),
			'app-misc/cmatrix', 'queued', '{}'::jsonb, 'fixture', $2
		)
	`, fencedJobID, builder.ExecutorProtocolVersion); err != nil {
		t.Fatalf("seed protocol-fenced job: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO build_attempts (
			id, job_id, attempt_no, state, worker_id, fence_token
		) VALUES ($1, $2, 1, 'claimed', $3, 1)
	`, uuid.New(), fencedJobID, legacyWorkerID); err == nil ||
		!strings.Contains(err.Error(), "does not satisfy job minimum") {
		t.Fatalf("legacy executor was not fenced: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO build_attempts (
			id, job_id, attempt_no, state, worker_id, fence_token
		) VALUES ($1, $2, 1, 'claimed', $3, 1)
	`, uuid.New(), fencedJobID, currentWorkerID); err != nil {
		t.Fatalf("current executor was rejected: %v", err)
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

func testDeviceAuthorizationAtomicConsumption(
	t *testing.T,
	ctx context.Context,
	db *persistence.Database,
	repo *persistence.IAMRepository,
	approver iam.Principal,
	approverToken string,
) {
	t.Helper()
	policy := persistence.IAMSessionPolicy{
		IdleTimeout: time.Hour, MaxLifetime: 12 * time.Hour,
	}
	// Re-authorize through the same local fixture used by the server so the
	// approval principal is exactly an active federated platform session.
	principal, err := repo.AuthorizePlatformSession(ctx, approverToken, policy)
	if err != nil {
		t.Fatalf("authorize device approver fixture: %v", err)
	}
	if principal.SubjectID != approver.SubjectID ||
		principal.Authentication != "federated-session" {
		t.Fatalf("device approver=%+v", principal)
	}

	rawDeviceCode := "ped1_postgres-one-time-capability"
	deviceDigest := sha256.Sum256([]byte(rawDeviceCode))
	deviceHash := hex.EncodeToString(deviceDigest[:])
	created, err := repo.CreateDeviceAuthorization(
		ctx, deviceHash, "ABCD-EFGH", 600, 5,
	)
	if err != nil || !created {
		t.Fatalf("create device authorization created=%t err=%v", created, err)
	}
	var storedHash string
	var remainingTTL float64
	if err := db.Pool().QueryRow(ctx, `
		SELECT device_code_hash,
		       extract(epoch FROM expires_at - clock_timestamp())
		FROM iam_device_authorizations WHERE user_code = 'ABCD-EFGH'
	`).Scan(&storedHash, &remainingTTL); err != nil {
		t.Fatalf("read stored device authorization: %v", err)
	}
	if storedHash != deviceHash || storedHash == rawDeviceCode ||
		strings.Contains(storedHash, rawDeviceCode) ||
		remainingTTL < 590 || remainingTTL > 600 {
		t.Fatalf("device code digest=%q remaining_ttl=%f", storedHash, remainingTTL)
	}
	if decision, err := repo.DecideDeviceAuthorization(
		ctx, "ABCD-EFGH", principal, true, policy,
	); err != nil || decision.Status != persistence.DeviceAuthorizationApproved {
		t.Fatalf("approve device authorization=%+v err=%v", decision, err)
	}

	type pollResult struct {
		raw    string
		result persistence.DeviceAuthorizationPoll
		err    error
	}
	start := make(chan struct{})
	results := make(chan pollResult, 2)
	var pollers sync.WaitGroup
	for _, raw := range []string{"pe1_device-winner-a", "pe1_device-winner-b"} {
		pollers.Add(1)
		go func(candidate string) {
			defer pollers.Done()
			<-start
			digest := sha256.Sum256([]byte(candidate))
			result, pollErr := repo.PollDeviceAuthorization(
				ctx, deviceHash, hex.EncodeToString(digest[:]), policy,
			)
			results <- pollResult{raw: candidate, result: result, err: pollErr}
		}(raw)
	}
	close(start)
	pollers.Wait()
	close(results)
	approved, expired := 0, 0
	winner, loser, winnerSessionID := "", "", ""
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent device poll: %v", result.err)
		}
		switch result.result.Status {
		case persistence.DeviceAuthorizationApproved:
			approved++
			winner = result.raw
			winnerSessionID = result.result.Principal.SessionID
		case persistence.DeviceAuthorizationExpired:
			expired++
			loser = result.raw
		default:
			t.Fatalf("concurrent device poll result=%+v", result.result)
		}
	}
	if approved != 1 || expired != 1 || winner == "" || loser == "" {
		t.Fatalf("concurrent polls approved=%d expired=%d winner=%q loser=%q",
			approved, expired, winner, loser)
	}
	defer func() {
		if _, cleanupErr := db.Pool().Exec(
			ctx, `DELETE FROM iam_sessions WHERE id = $1`, winnerSessionID,
		); cleanupErr != nil {
			t.Errorf("clean up winning device session: %v", cleanupErr)
		}
	}()
	if issued, err := repo.AuthorizePlatformSession(ctx, winner, policy); err != nil ||
		issued.SubjectID != approver.SubjectID {
		t.Fatalf("winning device session=%+v err=%v", issued, err)
	}
	if _, err := repo.AuthorizePlatformSession(ctx, loser, policy); err == nil {
		t.Fatal("losing concurrent access token became usable")
	}
	replayDigest := sha256.Sum256([]byte("pe1_device-replay"))
	if replay, err := repo.PollDeviceAuthorization(
		ctx, deviceHash, hex.EncodeToString(replayDigest[:]), policy,
	); err != nil || replay.Status != persistence.DeviceAuthorizationExpired {
		t.Fatalf("consumed replay=%+v err=%v", replay, err)
	}

	deniedRaw := "ped1_postgres-denied"
	deniedDigest := sha256.Sum256([]byte(deniedRaw))
	deniedHash := hex.EncodeToString(deniedDigest[:])
	if created, err := repo.CreateDeviceAuthorization(
		ctx, deniedHash, "JKLM-NPQR", 60, 5,
	); err != nil || !created {
		t.Fatalf("create denied device created=%t err=%v", created, err)
	}
	if _, err := repo.DecideDeviceAuthorization(
		ctx, "JKLM-NPQR", principal, false, policy,
	); err != nil {
		t.Fatalf("deny device authorization: %v", err)
	}
	unusedDigest := sha256.Sum256([]byte("pe1_never-issued"))
	if denied, err := repo.PollDeviceAuthorization(
		ctx, deniedHash, hex.EncodeToString(unusedDigest[:]), policy,
	); err != nil || denied.Status != persistence.DeviceAuthorizationDenied {
		t.Fatalf("denied poll=%+v err=%v", denied, err)
	}

	expiredRaw := "ped1_postgres-expired"
	expiredDigest := sha256.Sum256([]byte(expiredRaw))
	expiredHash := hex.EncodeToString(expiredDigest[:])
	if created, err := repo.CreateDeviceAuthorization(
		ctx, expiredHash, "STUV-WXYZ", 60, 5,
	); err != nil || !created {
		t.Fatalf("create expiring device created=%t err=%v", created, err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE iam_device_authorizations
		SET created_at = clock_timestamp() - interval '2 minutes',
		    expires_at = clock_timestamp() - interval '1 minute'
		WHERE device_code_hash = $1
	`, expiredHash); err != nil {
		t.Fatalf("expire device fixture: %v", err)
	}
	if expiredResult, err := repo.PollDeviceAuthorization(
		ctx, expiredHash, hex.EncodeToString(unusedDigest[:]), policy,
	); err != nil || expiredResult.Status != persistence.DeviceAuthorizationExpired {
		t.Fatalf("expired poll=%+v err=%v", expiredResult, err)
	}

	pendingRaw := "ped1_postgres-slow-down"
	pendingDigest := sha256.Sum256([]byte(pendingRaw))
	pendingHash := hex.EncodeToString(pendingDigest[:])
	if created, err := repo.CreateDeviceAuthorization(
		ctx, pendingHash, "2345-6789", 60, 5,
	); err != nil || !created {
		t.Fatalf("create pending device created=%t err=%v", created, err)
	}
	if pending, err := repo.PollDeviceAuthorization(
		ctx, pendingHash, hex.EncodeToString(unusedDigest[:]), policy,
	); err != nil || pending.Status != persistence.DeviceAuthorizationPending {
		t.Fatalf("pending poll=%+v err=%v", pending, err)
	}
	if slowDown, err := repo.PollDeviceAuthorization(
		ctx, pendingHash, hex.EncodeToString(unusedDigest[:]), policy,
	); err != nil || slowDown.Status != persistence.DeviceAuthorizationSlowDown ||
		slowDown.IntervalSeconds != 10 {
		t.Fatalf("slow-down poll=%+v err=%v", slowDown, err)
	}

	// A CLI token is the approving browser session in another shape, so it
	// must carry that lineage and die with it.
	parentRaw := "pe1_postgres-derivation-parent"
	parentDigest := sha256.Sum256([]byte(parentRaw))
	parentNow := time.Now().UTC()
	parent, err := repo.AuthorizeSession(ctx, approver, iam.ExternalIdentity{
		Issuer: approver.Issuer, Subject: approver.Subject,
		TokenHash:       hex.EncodeToString(parentDigest[:]),
		ProviderTokenID: "token-derivation-parent",
		IssuedAt:        parentNow.Add(-time.Minute),
		ExpiresAt:       parentNow.Add(time.Hour),
		AMR:             []string{"pwd"},
	}, policy)
	if err != nil || parent.SessionID == "" {
		t.Fatalf("authorize derivation parent=%+v err=%v", parent, err)
	}
	defer func() {
		// The cascade deletes the derived row with its parent.
		if _, cleanupErr := db.Pool().Exec(
			ctx, `DELETE FROM iam_sessions WHERE id = $1`, parent.SessionID,
		); cleanupErr != nil {
			t.Errorf("clean up derivation parent session: %v", cleanupErr)
		}
	}()
	derivationRaw := "ped1_postgres-derivation"
	derivationDigest := sha256.Sum256([]byte(derivationRaw))
	derivationHash := hex.EncodeToString(derivationDigest[:])
	if created, err := repo.CreateDeviceAuthorization(
		ctx, derivationHash, "3456-789A", 600, 5,
	); err != nil || !created {
		t.Fatalf("create derivation device created=%t err=%v", created, err)
	}
	if _, err := repo.DecideDeviceAuthorization(
		ctx, "3456-789A", parent, true, policy,
	); err != nil {
		t.Fatalf("approve derivation device: %v", err)
	}
	derivedRaw := "pe1_postgres-derived-token"
	derivedDigest := sha256.Sum256([]byte(derivedRaw))
	derived, err := repo.PollDeviceAuthorization(
		ctx, derivationHash, hex.EncodeToString(derivedDigest[:]), policy,
	)
	if err != nil || derived.Status != persistence.DeviceAuthorizationApproved ||
		derived.Principal.SessionID == "" {
		t.Fatalf("derivation poll=%+v err=%v", derived, err)
	}
	sessions, err := repo.ListSessions(ctx, approver.SubjectID)
	if err != nil {
		t.Fatalf("list derivation sessions: %v", err)
	}
	var derivedRow, parentRow persistence.IAMSession
	for _, session := range sessions {
		switch session.ID {
		case derived.Principal.SessionID:
			derivedRow = session
		case parent.SessionID:
			parentRow = session
		}
	}
	if derivedRow.Kind != "cli" ||
		derivedRow.DerivedFromSessionID != parent.SessionID ||
		parentRow.Kind != "browser" || parentRow.DerivedFromSessionID != "" {
		t.Fatalf(
			"session lineage derived=%+v parent=%+v", derivedRow, parentRow,
		)
	}
	if _, err := repo.RevokeSession(
		ctx, parent, parent.SessionID, "integration-derivation", false,
	); err != nil {
		t.Fatalf("revoke derivation parent: %v", err)
	}
	if _, err := repo.AuthorizePlatformSession(ctx, derivedRaw, policy); err == nil {
		t.Fatal("CLI session outlived the browser session it was derived from")
	}
}

func testMonitorTerminalEventWatermarkFallback(
	t *testing.T,
	ctx context.Context,
	db *persistence.Database,
	repo *persistence.JobRepository,
) {
	t.Helper()
	request := &builder.BuildRequest{
		PackageName: "app-misc/monitor-watermark", Arch: "amd64",
		IdempotencyKey: "monitor-watermark-attempt-fallback",
	}
	createdAt := time.Now().UTC().Add(-15 * time.Minute)
	status := &builder.BuildStatus{
		JobID: uuid.NewString(), Status: "queued",
		PackageName: request.PackageName, Arch: request.Arch,
		CreatedAt: createdAt, UpdatedAt: createdAt, Request: request,
	}
	if result, err := repo.CreateJob(ctx, request, status); err != nil || !result.Created {
		t.Fatalf("create monitor watermark fixture: result=%+v err=%v", result, err)
	}
	claim, err := repo.ClaimNext(ctx, "monitor-watermark-worker", time.Minute)
	if err != nil || claim == nil || claim.Status.JobID != status.JobID {
		t.Fatalf("claim monitor watermark fixture: claim=%+v err=%v", claim, err)
	}
	completed := *claim.Status
	completed.Status = "completed"
	completed.UpdatedAt = time.Now().UTC()
	if err := repo.RecordTransition(ctx, claim.Status, &completed); err != nil {
		t.Fatalf("complete monitor watermark fixture: %v", err)
	}

	attemptFinishedAt := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond)
	jobUpdatedAt := attemptFinishedAt.Add(5 * time.Minute)
	if _, err := db.Pool().Exec(ctx, `
		UPDATE build_attempts
		SET finished_at = $2, updated_at = $3
		WHERE id = $1
	`, claim.Status.AttemptID, attemptFinishedAt, jobUpdatedAt); err != nil {
		t.Fatalf("shape monitor attempt watermark fallback fixture: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE build_jobs
		SET completed_at = NULL, updated_at = $2
		WHERE id = $1
	`, status.JobID, jobUpdatedAt); err != nil {
		t.Fatalf("shape monitor job watermark fallback fixture: %v", err)
	}

	var outcomeEventAt time.Time
	if err := db.Pool().QueryRow(ctx, `
		SELECT completed_at
		FROM monitor_job_outcomes
		WHERE job_id = $1
	`, status.JobID).Scan(&outcomeEventAt); err != nil ||
		!outcomeEventAt.Equal(attemptFinishedAt) {
		t.Fatalf(
			"projected terminal event time=%s want=%s err=%v",
			outcomeEventAt, attemptFinishedAt, err,
		)
	}

	projectionRepo := persistence.NewJobRepository(db)
	for _, snapshotKind := range []string{"fresh", "cached"} {
		runtime, err := projectionRepo.RuntimeStatus(ctx)
		projection := runtime.TargetHistory.Projection
		if err != nil || !projection.Valid || projection.State != "current" ||
			projection.LagSeconds != 0 || projection.SourceWatermarkAt == nil ||
			projection.ProjectedWatermarkAt == nil ||
			!projection.SourceWatermarkAt.Equal(attemptFinishedAt) ||
			!projection.ProjectedWatermarkAt.Equal(attemptFinishedAt) ||
			projection.SourceWatermarkAt.Equal(jobUpdatedAt) {
			t.Fatalf(
				"%s monitor watermark fallback projection=%+v err=%v",
				snapshotKind, projection, err,
			)
		}
	}
	// A terminal event arriving behind a cached snapshot makes the projected
	// watermark trail the source by the gap between the two events. Reported
	// lag must stay the age of the snapshot being served, which the cache TTL
	// bounds, instead of that ten-minute gap.
	if _, err := db.Pool().Exec(ctx, `
		UPDATE build_jobs SET completed_at = clock_timestamp() WHERE id = $1
	`, status.JobID); err != nil {
		t.Fatalf("advance monitor source watermark: %v", err)
	}
	runtime, err := projectionRepo.RuntimeStatus(ctx)
	projection := runtime.TargetHistory.Projection
	if err != nil || projection.State != "lagging" ||
		projection.LagSeconds < 0 || projection.LagSeconds > 30 {
		t.Fatalf("cached monitor projection lag=%+v err=%v", projection, err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE build_jobs SET completed_at = NULL WHERE id = $1
	`, status.JobID); err != nil {
		t.Fatalf("restore monitor watermark fixture: %v", err)
	}
	if err := repo.HideJob(ctx, &completed, "monitor watermark fixture cleanup"); err != nil {
		t.Fatalf("hide monitor watermark fixture: %v", err)
	}
}

func testRuntimeBudgetsAndAbuseGate(
	t *testing.T,
	ctx context.Context,
	db *persistence.Database,
	iamRepo *persistence.IAMRepository,
	jobRepo *persistence.JobRepository,
) {
	t.Helper()
	newRequest := func(projectID, key string) (*builder.BuildRequest, *builder.BuildStatus) {
		request := &builder.BuildRequest{
			ProjectID: projectID, PackageName: "app-misc/" + key,
			Arch: "amd64", ResourceClass: "minute",
			MachineSpec: map[string]string{
				"cores": "1", "memory": "1024", "disk_size": "20",
			},
			ResolvedContext: &catalog.ResolvedBuildContext{
				ProfileID: "pe/amd64/runtime-budget-v1",
				Arch:      "amd64", Provider: "pve", ExecutionZone: "default",
				BuildMode: "native-gentoo", ImageID: "pe/runtime-budget-g1",
				ImageGeneration: "g1", ResourceClass: "minute",
				MaxRuntimeMinutes:            1,
				CloudCostMicrounitsPerMinute: 100,
			},
			IdempotencyKey: key,
		}
		now := time.Now().UTC()
		status := &builder.BuildStatus{
			JobID: uuid.NewString(), ProjectID: projectID, Status: "queued",
			PackageName: request.PackageName, Arch: request.Arch,
			CreatedAt: now, UpdatedAt: now, Request: request,
			ResolvedContext: request.ResolvedContext,
		}
		return request, status
	}
	terminal := func(claim *builder.SchedulerClaim, state string, at time.Time) {
		t.Helper()
		previous := *claim.Status
		current := previous
		current.Status, current.UpdatedAt = state, at
		if state == "failed" {
			current.Error = "test failure"
		}
		if err := jobRepo.RecordTransition(ctx, &previous, &current); err != nil {
			t.Fatalf("settle %s attempt: %v", state, err)
		}
	}

	budgetProject, err := iamRepo.CreateProject(
		ctx, "runtime-budget", "runtime budget integration fixture",
	)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := iamRepo.GetProjectPolicy(ctx, budgetProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	policy, err = iamRepo.UpdateProjectPolicy(
		ctx, budgetProject.ID, persistence.ProjectPolicyUpdate{
			Version: policy.Version, MaxQueuedJobs: 10, MaxActiveJobs: 10,
			MaxDailySubmissions: 100, MaxActiveVCPUs: 10,
			MaxActiveMemoryMiB: 10240, MaxActiveDiskGiB: 200,
			MaxDailyBuildSeconds: 61, MaxDailyCloudCostMicrounits: 101,
			MaxFailuresPerHour: 10, AbuseCooldownSeconds: 60,
		}, "runtime-budget-test",
	)
	if err != nil {
		t.Fatal(err)
	}
	firstRequest, firstStatus := newRequest(budgetProject.ID, "runtime-first")
	secondRequest, secondStatus := newRequest(budgetProject.ID, "runtime-second")
	var runtimeCapabilities []string
	for _, phase := range []string{"provision", "build", "verify", "publish"} {
		labels, err := builder.PhaseCapabilityRequirements(firstRequest, phase)
		if err != nil {
			t.Fatal(err)
		}
		runtimeCapabilities = append(runtimeCapabilities, labels...)
	}
	runtimeCapabilities, err = builder.NormalizeExecutorCapabilities(runtimeCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	if claim, err := jobRepo.ClaimPhaseWork(
		ctx, "runtime-capability-worker", time.Minute, runtimeCapabilities,
	); err != nil || claim != nil {
		t.Fatalf("register runtime capability worker: claim=%+v err=%v", claim, err)
	}
	if _, err := jobRepo.CreateJob(ctx, firstRequest, firstStatus); err != nil {
		t.Fatal(err)
	}
	if _, err := jobRepo.CreateJob(ctx, secondRequest, secondStatus); err != nil {
		t.Fatal(err)
	}
	first, err := jobRepo.ClaimNext(
		ctx, "runtime-budget-first", time.Minute, runtimeCapabilities,
	)
	if err != nil || first == nil {
		t.Fatalf("claim first runtime budget=%+v err=%v", first, err)
	}
	if blocked, err := jobRepo.ClaimNext(
		ctx, "runtime-budget-blocked", time.Minute, runtimeCapabilities,
	); err != nil || blocked != nil {
		t.Fatalf("runtime budget was oversubscribed claim=%+v err=%v", blocked, err)
	}
	// A worker timestamp can be skewed. Runtime settlement must use the
	// PostgreSQL authority clock rather than charging this synthetic future.
	terminal(first, "completed", time.Now().UTC().Add(5*time.Minute))
	policy, err = iamRepo.GetProjectPolicy(ctx, budgetProject.ID)
	if err != nil || policy.BuildSecondsToday != 1 ||
		policy.CloudCostMicrounitsToday != 0 ||
		policy.ActiveRuntimeBudgets != 0 {
		t.Fatalf("settled runtime policy=%+v err=%v", policy, err)
	}
	second, err := jobRepo.ClaimNext(
		ctx, "runtime-budget-second", time.Minute, runtimeCapabilities,
	)
	if err != nil || second == nil {
		t.Fatalf("claim after unused budget release=%+v err=%v", second, err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE project_attempt_usage
		SET metering_started_at = clock_timestamp() - interval '2 minutes',
		    cloud_started_at = clock_timestamp() - interval '2 minutes'
		WHERE attempt_id = $1
	`, second.Status.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := jobRepo.RenewClaim(ctx, second.Status, time.Minute); err == nil {
		t.Fatal("runtime-expired attempt renewed its lease")
	}
	if err := jobRepo.CheckClaim(ctx, second.Status); err == nil {
		t.Fatal("runtime-expired attempt passed the external-side-effect fence")
	}
	if _, err := jobRepo.CancelJob(
		ctx, second.Status.JobID, "runtime deadline test cleanup",
	); err != nil {
		t.Fatalf("cleanup runtime-expired job: %v", err)
	}
	var chargedSeconds, chargedCloud, reservedSeconds, reservedCloud int64
	if err := db.Pool().QueryRow(ctx, `
		SELECT charged_build_seconds, charged_cloud_cost_microunits,
		       reserved_build_seconds, reserved_cloud_cost_microunits
		FROM project_attempt_usage
		WHERE attempt_id = $1
	`, second.Status.AttemptID).Scan(
		&chargedSeconds, &chargedCloud, &reservedSeconds, &reservedCloud,
	); err != nil {
		t.Fatal(err)
	}
	if chargedSeconds != reservedSeconds || chargedCloud != reservedCloud {
		t.Fatalf(
			"deadline settlement exceeded reservation: charged=%d/%d reserved=%d/%d",
			chargedSeconds, chargedCloud, reservedSeconds, reservedCloud,
		)
	}

	abuseProject, err := iamRepo.CreateProject(
		ctx, "runtime-abuse", "failure storm integration fixture",
	)
	if err != nil {
		t.Fatal(err)
	}
	abusePolicy, err := iamRepo.GetProjectPolicy(ctx, abuseProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	abusePolicy, err = iamRepo.UpdateProjectPolicy(
		ctx, abuseProject.ID, persistence.ProjectPolicyUpdate{
			Version: abusePolicy.Version, MaxQueuedJobs: 10, MaxActiveJobs: 10,
			MaxDailySubmissions: 100, MaxActiveVCPUs: 10,
			MaxActiveMemoryMiB: 10240, MaxActiveDiskGiB: 200,
			MaxDailyBuildSeconds: 3600, MaxDailyCloudCostMicrounits: 10000,
			MaxFailuresPerHour: 2, AbuseCooldownSeconds: 60,
		}, "runtime-abuse-test",
	)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		request, status := newRequest(
			abuseProject.ID, fmt.Sprintf("abuse-failure-%d", index),
		)
		if _, err := jobRepo.CreateJob(ctx, request, status); err != nil {
			t.Fatal(err)
		}
		claim, err := jobRepo.ClaimNext(
			ctx, fmt.Sprintf("abuse-worker-%d", index), time.Minute,
			runtimeCapabilities,
		)
		if err != nil || claim == nil {
			t.Fatalf("claim abuse failure %d=%+v err=%v", index, claim, err)
		}
		terminal(claim, "failed", time.Now().UTC())
	}
	abusePolicy, err = iamRepo.GetProjectPolicy(ctx, abuseProject.ID)
	if err != nil || !abusePolicy.AbuseSuspended ||
		abusePolicy.FailuresLastHour != 2 || abusePolicy.AbuseGeneration == 0 {
		t.Fatalf("failure storm policy=%+v err=%v", abusePolicy, err)
	}
	rejectedRequest, rejectedStatus := newRequest(abuseProject.ID, "abuse-rejected")
	_, err = jobRepo.CreateJob(ctx, rejectedRequest, rejectedStatus)
	admission, ok := builder.AsAdmissionError(err)
	if !ok || admission.Code != "project_abuse_suspended" {
		t.Fatalf("failure storm admission error=%+v raw=%v", admission, err)
	}
	abusePolicy, err = iamRepo.UpdateProjectPolicy(
		ctx, abuseProject.ID, persistence.ProjectPolicyUpdate{
			Version: abusePolicy.Version, MaxQueuedJobs: abusePolicy.MaxQueuedJobs,
			MaxActiveJobs:               abusePolicy.MaxActiveJobs,
			MaxDailySubmissions:         abusePolicy.MaxDailySubmissions,
			MaxActiveVCPUs:              abusePolicy.MaxActiveVCPUs,
			MaxActiveMemoryMiB:          abusePolicy.MaxActiveMemoryMiB,
			MaxActiveDiskGiB:            abusePolicy.MaxActiveDiskGiB,
			MaxDailyBuildSeconds:        abusePolicy.MaxDailyBuildSeconds,
			MaxDailyCloudCostMicrounits: abusePolicy.MaxDailyCloudCostMicrounits,
			MaxFailuresPerHour:          abusePolicy.MaxFailuresPerHour,
			AbuseCooldownSeconds:        abusePolicy.AbuseCooldownSeconds,
			ClearAbuseSuspension:        true,
		}, "runtime-abuse-owner",
	)
	if err != nil || abusePolicy.AbuseSuspended ||
		abusePolicy.AbuseSuspendedUntil != nil {
		t.Fatalf("clear failure storm suspension=%+v err=%v", abusePolicy, err)
	}
	result, err := jobRepo.CreateJob(
		ctx, rejectedRequest, rejectedStatus,
	)
	if err != nil || !result.Created {
		t.Fatalf("submission after abuse clear=%+v err=%v", result, err)
	}
	if _, err := jobRepo.CancelJob(
		ctx, result.JobID, "runtime abuse test cleanup",
	); err != nil {
		t.Fatalf("cleanup post-clear job: %v", err)
	}
	var auditCount int
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM audit_events
		WHERE project_id = $1 AND action = 'project.abuse_suspended'
	`, abuseProject.ID).Scan(&auditCount); err != nil || auditCount == 0 {
		t.Fatalf("abuse suspension audit count=%d err=%v", auditCount, err)
	}
}

func testActivePhaseContextAndFinalization(
	t *testing.T,
	ctx context.Context,
	db *persistence.Database,
	iamRepo *persistence.IAMRepository,
	jobRepo *persistence.JobRepository,
) {
	t.Helper()
	project, err := iamRepo.CreateProject(
		ctx, "active-phase-finalize", "active phase context fixture",
	)
	if err != nil {
		t.Fatal(err)
	}
	request := &builder.BuildRequest{
		ProjectID: project.ID, PackageName: "app-misc/active-phase",
		Arch: "amd64", ResourceClass: "small",
		IdempotencyKey: "active-phase-finalize",
		MachineSpec: map[string]string{
			"cores": "2", "memory": "2048", "disk_size": "20",
		},
	}
	now := time.Now().UTC()
	status := &builder.BuildStatus{
		JobID: uuid.NewString(), ProjectID: project.ID, Status: "queued",
		PackageName: request.PackageName, Arch: request.Arch,
		CreatedAt: now, UpdatedAt: now, Request: request,
	}
	if result, err := jobRepo.CreateJob(
		ctx, request, status,
	); err != nil || !result.Created {
		t.Fatalf("create active phase fixture: result=%+v err=%v", result, err)
	}
	schedulerClaim, err := jobRepo.ClaimNext(
		ctx, "active-phase-admission", 2*time.Minute,
	)
	if err != nil || schedulerClaim == nil ||
		schedulerClaim.Status.JobID != status.JobID {
		t.Fatalf("claim active phase fixture: claim=%+v err=%v", schedulerClaim, err)
	}
	if err := jobRepo.ActivatePhasePlan(ctx, schedulerClaim.Status); err != nil {
		t.Fatal(err)
	}

	var publish *builder.PhaseWorkClaim
	for _, phase := range []string{"provision", "build", "verify", "publish"} {
		claim, err := jobRepo.ClaimPhaseWork(
			ctx, "active-phase-"+phase, time.Minute,
			legacyExecutorCapabilities, phase,
		)
		if err != nil || claim == nil || claim.Phase != phase ||
			claim.Request == nil || claim.Status == nil ||
			claim.Status.LeaseOwner != "active-phase-admission" {
			t.Fatalf("claim %s: claim=%+v err=%v", phase, claim, err)
		}
		if phase == "provision" {
			value := &builder.PhaseExecutionContext{
				WorkerID: uuid.NewString(),
				Instance: &builder.PhaseInstanceContext{
					ID: "pve-active-phase", Provider: "pve", Status: "running",
					Arch: "amd64", BuilderEndpoint: "pull://fixture",
					TerraformDir: "/shared/terraform/pve-active-phase",
					Metadata:     map[string]string{"node": "pve"},
				},
			}
			if err := jobRepo.SavePhaseExecutionContext(ctx, claim, value); err != nil {
				t.Fatal(err)
			}
			loaded, err := jobRepo.LoadPhaseExecutionContext(ctx, claim)
			if err != nil || loaded.WorkerID != value.WorkerID ||
				loaded.Instance == nil || loaded.Instance.ID != value.Instance.ID {
				t.Fatalf("phase context=%+v err=%v", loaded, err)
			}
			forbidden := *value
			instance := *value.Instance
			instance.Metadata = map[string]string{"api_token": "must-not-persist"}
			forbidden.Instance = &instance
			if err := jobRepo.SavePhaseExecutionContext(
				ctx, claim, &forbidden,
			); err == nil {
				t.Fatal("secret-like phase metadata was persisted")
			}
		}
		if phase == "publish" {
			publish = claim
			break
		}
		if err := jobRepo.CompletePhaseWork(ctx, claim); err != nil {
			t.Fatalf("complete %s: %v", phase, err)
		}
	}
	if publish == nil {
		t.Fatal("publish phase was not claimed")
	}
	completed := *publish.Status
	completed.Status, completed.Error, completed.UpdatedAt = "completed", "", time.Now().UTC()
	completed.ArtifactURL = "/binpkgs/releases/amd64/binpackages/23.0/x86-64/app-misc/active-phase.gpkg.tar"
	if err := jobRepo.FinalizePhaseWork(
		ctx, publish, publish.Status, &completed,
	); err != nil {
		t.Fatal(err)
	}
	var jobState, attemptState, publishState string
	var leases int
	if err := db.Pool().QueryRow(ctx, `
		SELECT j.state, a.state, w.state,
		       (SELECT count(*) FROM worker_leases l WHERE l.attempt_id = a.id)
		FROM build_jobs j
		JOIN build_attempts a ON a.job_id = j.id
		JOIN phase_work_items w ON w.attempt_id = a.id AND w.phase = 'publish'
		WHERE j.id = $1
	`, status.JobID).Scan(
		&jobState, &attemptState, &publishState, &leases,
	); err != nil {
		t.Fatal(err)
	}
	if jobState != "completed" || attemptState != "completed" ||
		publishState != "completed" || leases != 0 {
		t.Fatalf("atomic final state job=%s attempt=%s publish=%s leases=%d",
			jobState, attemptState, publishState, leases)
	}
	if err := jobRepo.CompletePhaseWork(ctx, publish); err == nil {
		t.Fatal("finalized phase accepted a duplicate completion")
	}
}

func TestProjectResourceAndArtifactMigrationsRequireDrainedAttempts(t *testing.T) {
	adminDSN := os.Getenv("PORTAGE_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("set PORTAGE_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	schema := "iam1b_drain_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	cfg := config.DatabaseConfig{
		Enabled: true, Required: true, URL: testDSN,
		MaxConns: 2, ConnectTimeoutSeconds: 10, HealthTimeoutSeconds: 2,
	}
	runner, err := migrations.NewRunner(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runner.Close() }()
	if _, err := runner.Provider().UpTo(ctx, 10); err != nil {
		t.Fatal(err)
	}
	fixture, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fixture.Close(context.Background()) }()
	jobID := uuid.New()
	if _, err := fixture.Exec(ctx, `
		INSERT INTO build_jobs (
			id, project_id, package_atom, state, request, request_digest
		) VALUES (
			$1, (SELECT id FROM projects WHERE name = 'default'),
			'app-misc/migration-drain', 'building', '{}'::jsonb, 'drain-fixture'
		)
	`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().Up(ctx); err == nil ||
		!strings.Contains(err.Error(), "requires all active build attempts") {
		t.Fatalf("schema v11 accepted an active job: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 10 {
		t.Fatalf("failed migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx, `
		UPDATE build_jobs SET state = 'failed' WHERE id = $1
	`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().UpTo(ctx, 11); err != nil {
		t.Fatalf("schema v11 failed after drain: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 11 {
		t.Fatalf("drained migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx, `
		UPDATE build_jobs SET state = 'collecting' WHERE id = $1
	`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().Up(ctx); err == nil ||
		!strings.Contains(err.Error(), "schema v12 requires all active build attempts") {
		t.Fatalf("schema v12 accepted an active job: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 11 {
		t.Fatalf("failed artifact migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx, `
		UPDATE build_jobs SET state = 'failed' WHERE id = $1
	`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().UpTo(ctx, 12); err != nil {
		t.Fatalf("schema v12 failed after drain: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 12 {
		t.Fatalf("drained artifact migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx, `
		UPDATE build_jobs SET state = 'verifying' WHERE id = $1
	`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().Up(ctx); err == nil ||
		!strings.Contains(err.Error(), "schema v13 requires all active build attempts") {
		t.Fatalf("schema v13 accepted an active job: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 12 {
		t.Fatalf("failed phase-cap migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx, `
		UPDATE build_jobs SET state = 'failed' WHERE id = $1
	`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().UpTo(ctx, 13); err != nil {
		t.Fatalf("schema v13 failed after drain: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 13 {
		t.Fatalf("drained phase-cap migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx, `
		UPDATE build_jobs SET state = 'publishing' WHERE id = $1
	`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().Up(ctx); err == nil ||
		!strings.Contains(err.Error(), "schema v14 requires all active build attempts") {
		t.Fatalf("schema v14 accepted an active job: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 13 {
		t.Fatalf("failed phase-work migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx, `
		UPDATE build_jobs SET state = 'failed' WHERE id = $1
	`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().UpTo(ctx, 14); err != nil {
		t.Fatalf("schema v14 failed after drain: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 14 {
		t.Fatalf("drained phase-work migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx, `
		UPDATE build_jobs SET state = 'building' WHERE id = $1
	`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().Up(ctx); err == nil ||
		!strings.Contains(err.Error(), "schema v15 requires all active build attempts") {
		t.Fatalf("schema v15 accepted an active job: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 14 {
		t.Fatalf("failed gateway-spool migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx, `
		UPDATE build_jobs SET state = 'failed' WHERE id = $1
	`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().UpTo(ctx, 15); err != nil {
		t.Fatalf("schema v15 failed after drain: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 15 {
		t.Fatalf("drained gateway-spool migration version=%d err=%v", version, err)
	}
	workerID, attemptID, leaseID := uuid.New(), uuid.New(), uuid.New()
	if _, err := fixture.Exec(ctx, `
		INSERT INTO workers (id, stable_name, max_slots, executor_protocol)
		VALUES ($1, $2, 1, $3)
	`, workerID, "v16-drain-"+uuid.NewString(),
		builder.ExecutorProtocolVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx,
		`UPDATE build_jobs SET state = 'claimed' WHERE id = $1`,
		jobID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, `
		INSERT INTO build_attempts (
			id, job_id, attempt_no, state, worker_id, fence_token
		) VALUES ($1, $2, 1, 'claimed', $3, 1)
	`, attemptID, jobID, workerID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, `
		INSERT INTO worker_leases (
			id, worker_id, attempt_id, fence_token, expires_at
		) VALUES ($1, $2, $3, 1, clock_timestamp() + interval '1 minute')
	`, leaseID, workerID, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().Up(ctx); err == nil ||
		!strings.Contains(err.Error(), "schema v16 requires active phase plans and worker leases") {
		t.Fatalf("schema v16 accepted an active worker lease: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 15 {
		t.Fatalf("failed active-phase migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx,
		`DELETE FROM worker_leases WHERE id = $1`, leaseID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, `
		UPDATE build_attempts SET state = 'failed', finished_at = clock_timestamp()
		WHERE id = $1
	`, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx,
		`UPDATE build_jobs SET state = 'failed' WHERE id = $1`, jobID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().UpTo(ctx, 16); err != nil {
		t.Fatalf("schema v16 failed after drain: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 16 {
		t.Fatalf("drained active-phase migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx,
		`UPDATE build_jobs SET state = 'queued' WHERE id = $1`, jobID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().Up(ctx); err == nil ||
		!strings.Contains(err.Error(), "schema v17 requires queued jobs") {
		t.Fatalf("schema v17 accepted queued work: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 16 {
		t.Fatalf("failed runtime-budget migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx,
		`UPDATE build_jobs SET state = 'failed' WHERE id = $1`, jobID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().UpTo(ctx, 17); err != nil {
		t.Fatalf("schema v17 failed after drain: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 17 {
		t.Fatalf("drained runtime-budget migration version=%d err=%v", version, err)
	}
	if _, err := runner.Provider().UpTo(ctx, 19); err != nil {
		t.Fatalf("schema v18/v19 additive IAM migrations failed: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 19 {
		t.Fatalf("IAM provider lifecycle migration version=%d err=%v", version, err)
	}
	if _, err := runner.Provider().UpTo(ctx, 20); err != nil {
		t.Fatalf("schema v20 capability migration failed after drain: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 20 {
		t.Fatalf("capability migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx, `
		INSERT INTO worker_gateway_sessions (
			worker_id, project_id, job_id, attempt_id, attempt_fence, state
		)
		SELECT $2, project_id, id, $3, 1, 'active'
		FROM build_jobs
		WHERE id = $1
	`, jobID, "v21-drain-"+uuid.NewString(), attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().Up(ctx); err == nil ||
		!strings.Contains(err.Error(), "schema v21 requires active worker gateway sessions") {
		t.Fatalf("schema v21 accepted an active gateway session: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 20 {
		t.Fatalf("failed workload issuer migration version=%d err=%v", version, err)
	}
	if _, err := fixture.Exec(ctx, `
		DELETE FROM worker_gateway_sessions WHERE attempt_id = $1
	`, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Provider().UpTo(ctx, 21); err != nil {
		t.Fatalf("schema v21 failed after gateway drain: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 21 {
		t.Fatalf("workload issuer migration version=%d err=%v", version, err)
	}
	if _, err := runner.Provider().UpTo(ctx, 22); err != nil {
		t.Fatalf("schema v22 fairness migration failed: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 22 {
		t.Fatalf("scheduler fairness migration version=%d err=%v", version, err)
	}
	var fairnessRows int
	if err := fixture.QueryRow(ctx, `
		SELECT count(*) FROM project_scheduler_fairness
	`).Scan(&fairnessRows); err != nil || fairnessRows == 0 {
		t.Fatalf("scheduler fairness rows=%d err=%v", fairnessRows, err)
	}
	var priorityWeight, starvationSeconds int
	if err := fixture.QueryRow(ctx, `
		SELECT priority_weight, starvation_threshold_seconds
		FROM project_policies
		WHERE project_id = (SELECT id FROM projects WHERE name = 'default')
	`).Scan(&priorityWeight, &starvationSeconds); err != nil ||
		priorityWeight != 100 || starvationSeconds != 300 {
		t.Fatalf(
			"fairness policy weight=%d starvation=%d err=%v",
			priorityWeight, starvationSeconds, err,
		)
	}
	if _, err := runner.Provider().UpTo(ctx, 23); err != nil {
		t.Fatalf("schema v23 capacity-pool migration failed: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 23 {
		t.Fatalf("capacity-pool migration version=%d err=%v", version, err)
	}
	var poolTable string
	if err := fixture.QueryRow(ctx, `
		SELECT to_regclass('scheduler_capacity_pool_state')::text
	`).Scan(&poolTable); err != nil ||
		poolTable != "scheduler_capacity_pool_state" {
		t.Fatalf("capacity-pool table=%q err=%v", poolTable, err)
	}
	if _, err := runner.Provider().UpTo(ctx, 24); err != nil {
		t.Fatalf("schema v24 actuator-fence migration failed: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil || version != 24 {
		t.Fatalf("actuator-fence migration version=%d err=%v", version, err)
	}
	var actionsTable, instancesTable string
	if err := fixture.QueryRow(ctx, `
		SELECT to_regclass('scheduler_capacity_actions')::text,
		       to_regclass('scheduler_capacity_instances')::text
	`).Scan(&actionsTable, &instancesTable); err != nil ||
		actionsTable != "scheduler_capacity_actions" ||
		instancesTable != "scheduler_capacity_instances" {
		t.Fatalf(
			"actuator tables actions=%q instances=%q err=%v",
			actionsTable, instancesTable, err,
		)
	}
	if _, err := runner.Provider().UpTo(ctx, 25); err != nil {
		t.Fatalf("schema v25 worker-scoring migration failed: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil ||
		version != 25 {
		t.Fatalf("worker-scoring migration version=%d err=%v", version, err)
	}
	var decisionsTable string
	if err := fixture.QueryRow(ctx, `
		SELECT to_regclass('scheduler_worker_decisions')::text
	`).Scan(&decisionsTable); err != nil ||
		decisionsTable != "scheduler_worker_decisions" {
		t.Fatalf("worker-scoring table=%q err=%v", decisionsTable, err)
	}
	if _, err := runner.Provider().UpTo(ctx, 26); err != nil {
		t.Fatalf("schema v26 target-history migration failed: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil ||
		version != 26 {
		t.Fatalf("target-history migration version=%d err=%v", version, err)
	}
	var outcomesView string
	if err := fixture.QueryRow(ctx, `
		SELECT to_regclass('monitor_job_outcomes')::text
	`).Scan(&outcomesView); err != nil ||
		outcomesView != "monitor_job_outcomes" {
		t.Fatalf("target-history view=%q err=%v", outcomesView, err)
	}
	if _, err := runner.Provider().UpTo(ctx, 27); err != nil {
		t.Fatalf("schema v27 device-authorization migration failed: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil ||
		version != 27 {
		t.Fatalf("device-authorization migration version=%d err=%v", version, err)
	}
	var deviceTable string
	if err := fixture.QueryRow(ctx, `
		SELECT to_regclass('iam_device_authorizations')::text
	`).Scan(&deviceTable); err != nil ||
		deviceTable != "iam_device_authorizations" {
		t.Fatalf("device-authorization table=%q err=%v", deviceTable, err)
	}
	if _, err := runner.Provider().UpTo(ctx, 28); err != nil {
		t.Fatalf("schema v28 lease/projection observability migration failed: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil ||
		version != 28 {
		t.Fatalf("lease/projection observability migration version=%d err=%v", version, err)
	}
	var countersTable string
	var counterKeys int
	if err := fixture.QueryRow(ctx, `
		SELECT to_regclass('scheduler_lease_expiry_counters')::text,
		       (SELECT count(*) FROM scheduler_lease_expiry_counters)
	`).Scan(&countersTable, &counterKeys); err != nil ||
		countersTable != "scheduler_lease_expiry_counters" || counterKeys != 7 {
		t.Fatalf(
			"lease expiry counters table=%q keys=%d err=%v",
			countersTable, counterKeys, err,
		)
	}
	if _, err := runner.Provider().UpTo(ctx, 29); err != nil {
		t.Fatalf("schema v29 distributed-build migration failed: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil ||
		version != 29 {
		t.Fatalf("distributed-build migration version=%d err=%v", version, err)
	}
	var compileWorkers, compileLeases, compileObservations string
	if err := fixture.QueryRow(ctx, `
		SELECT to_regclass('compile_workers')::text,
		       to_regclass('compile_slot_leases')::text,
		       to_regclass('compile_observations')::text
	`).Scan(&compileWorkers, &compileLeases, &compileObservations); err != nil ||
		compileWorkers != "compile_workers" ||
		compileLeases != "compile_slot_leases" ||
		compileObservations != "compile_observations" {
		t.Fatalf(
			"distcc tables workers=%q leases=%q observations=%q err=%v",
			compileWorkers, compileLeases, compileObservations, err,
		)
	}
	if _, err := runner.Provider().UpTo(ctx, 30); err != nil {
		t.Fatalf("schema v30 CLI session derivation migration failed: %v", err)
	}
	if version, err := runner.Provider().GetDBVersion(ctx); err != nil ||
		version != 30 {
		t.Fatalf("CLI session derivation migration version=%d err=%v", version, err)
	}
	var derivedIndex, sessionKindDefault string
	if err := fixture.QueryRow(ctx, `
		SELECT to_regclass('iam_sessions_derived_from_idx')::text,
		       (
		         SELECT column_default
		         FROM information_schema.columns
		         WHERE table_schema = current_schema()
		           AND table_name = 'iam_sessions'
		           AND column_name = 'session_kind'
		       )
	`).Scan(&derivedIndex, &sessionKindDefault); err != nil ||
		derivedIndex != "iam_sessions_derived_from_idx" ||
		!strings.HasPrefix(sessionKindDefault, "'browser'") {
		t.Fatalf(
			"session derivation index=%q kind default=%q err=%v",
			derivedIndex, sessionKindDefault, err,
		)
	}
}

func testDurableWorkerGatewaySpool(
	t *testing.T,
	ctx context.Context,
	db *persistence.Database,
	iamRepo *persistence.IAMRepository,
	repoA *persistence.JobRepository,
) {
	t.Helper()
	project, err := iamRepo.CreateProject(
		ctx, "gateway-spool", "durable worker gateway fixture",
	)
	if err != nil {
		t.Fatal(err)
	}
	request := &builder.BuildRequest{
		ProjectID: project.ID, PackageName: "app-misc/gateway-spool",
		Arch: "amd64", ResourceClass: "small",
		MachineSpec: map[string]string{
			"cores": "2", "memory": "2048", "disk_size": "20",
		},
		IdempotencyKey: "gateway-spool",
	}
	now := time.Now().UTC()
	status := &builder.BuildStatus{
		JobID: uuid.NewString(), ProjectID: project.ID, Status: "queued",
		PackageName: request.PackageName, Arch: request.Arch,
		CreatedAt: now, UpdatedAt: now, Request: request,
	}
	if result, createErr := repoA.CreateJob(ctx, request, status); createErr != nil ||
		!result.Created {
		t.Fatalf("create gateway fixture=%+v err=%v", result, createErr)
	}
	if _, err := db.Pool().Exec(
		ctx, "UPDATE build_jobs SET priority = 10000 WHERE id = $1", status.JobID,
	); err != nil {
		t.Fatal(err)
	}
	claim, err := repoA.ClaimNext(ctx, "gateway-spool-owner", time.Minute)
	if err != nil || claim == nil || claim.Status.JobID != status.JobID {
		t.Fatalf("claim gateway fixture=%+v err=%v", claim, err)
	}
	identity := workergateway.Identity{
		WorkerID: uuid.NewString(), JobID: claim.Status.JobID,
		AttemptID: claim.Status.AttemptID, FenceToken: claim.Status.FenceToken,
	}
	if err := repoA.RegisterWorkerSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	certificateRecord := workergateway.CertificateRecord{
		Fingerprint:       strings.Repeat("a", 64),
		Serial:            "01",
		IssuerID:          "postgres-integration",
		IssuerProvider:    workergateway.IssuerProviderFile,
		IssuerFingerprint: strings.Repeat("b", 64),
		IssuerSubject:     "CN=postgres-integration-workload-ca",
		IssuerSerial:      "ca01",
		IssuerNotBefore:   now.Add(-time.Hour),
		IssuerNotAfter:    now.Add(48 * time.Hour),
		NotBefore:         now.Add(-time.Minute),
		NotAfter:          now.Add(3 * time.Hour),
	}
	if err := repoA.RegisterWorkerCertificate(
		ctx, identity, certificateRecord,
	); err != nil {
		t.Fatal(err)
	}
	presentedCertificate := workergateway.PresentedCertificate{
		Fingerprint: certificateRecord.Fingerprint,
		Serial:      certificateRecord.Serial,
	}
	if err := repoA.AuthorizeWorkerCertificate(
		ctx, identity, presentedCertificate,
	); err != nil {
		t.Fatalf("registered workload certificate was rejected: %v", err)
	}
	wrongCertificate := presentedCertificate
	wrongCertificate.Fingerprint = strings.Repeat("c", 64)
	if err := repoA.AuthorizeWorkerCertificate(
		ctx, identity, wrongCertificate,
	); err == nil {
		t.Fatal("unknown workload certificate was authorized")
	}
	repoB := persistence.NewJobRepository(db)
	if _, err := repoB.ClaimWorkerCommand(
		ctx, identity, 100*time.Millisecond,
	); !errors.Is(err, workergateway.ErrNoWork) {
		t.Fatalf("initial empty worker poll=%v", err)
	}
	if connected, err := repoA.WorkerSessionConnected(
		ctx, identity,
	); err != nil || !connected {
		t.Fatalf("empty worker poll did not commit connection heartbeat: connected=%t err=%v",
			connected, err)
	}
	task := workergateway.Task{
		ID: uuid.NewString(), Action: workergateway.ActionBuild,
		Payload: json.RawMessage(`{"package_name":"app-misc/gateway-spool"}`),
	}
	if err := repoA.EnqueueWorkerCommand(ctx, identity, task); err != nil {
		t.Fatal(err)
	}
	if err := repoB.EnqueueWorkerCommand(ctx, identity, task); err != nil {
		t.Fatalf("exact stable command replay was rejected: %v", err)
	}
	conflictingTask := task
	conflictingTask.Payload = json.RawMessage(`{"package_name":"app-misc/other"}`)
	if err := repoB.EnqueueWorkerCommand(
		ctx, identity, conflictingTask,
	); err == nil {
		t.Fatal("stable command id accepted a different immutable request")
	}
	first, err := repoB.ClaimWorkerCommand(ctx, identity, 100*time.Millisecond)
	if err != nil || first.DeliveryFence != 1 {
		t.Fatalf("first durable delivery=%+v err=%v", first, err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE worker_gateway_commands
		SET delivery_lease_expires_at = clock_timestamp() - interval '1 second'
		WHERE id = $1
	`, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := repoA.ClaimWorkerCommand(ctx, identity, time.Minute)
	if err != nil || second.ID != first.ID || second.DeliveryFence != 2 {
		t.Fatalf("replayed durable delivery=%+v err=%v", second, err)
	}
	stale := workergateway.Completion{
		TaskID: first.ID, DeliveryFence: first.DeliveryFence,
		Payload: json.RawMessage(`{"status":"success"}`),
	}
	if err := repoB.CompleteWorkerCommand(ctx, identity, stale); !errors.Is(
		err, workergateway.ErrStaleFence,
	) {
		t.Fatalf("stale completion was not fenced: %v", err)
	}
	current := stale
	current.DeliveryFence = second.DeliveryFence
	if err := repoB.CompleteWorkerCommand(ctx, identity, current); err != nil {
		t.Fatal(err)
	}
	if err := repoA.CompleteWorkerCommand(ctx, identity, current); err != nil {
		t.Fatalf("exact completion replay was rejected: %v", err)
	}
	conflictingCompletion := current
	conflictingCompletion.Payload = json.RawMessage(`{"status":"different"}`)
	if err := repoA.CompleteWorkerCommand(
		ctx, identity, conflictingCompletion,
	); !errors.Is(err, workergateway.ErrStaleFence) {
		t.Fatalf("conflicting completion replay was not fenced: %v", err)
	}
	result, err := repoA.WorkerCommandResult(ctx, identity, task.ID)
	if err != nil || result.DeliveryFence != 2 ||
		string(result.Payload) != `{"status": "success"}` &&
			string(result.Payload) != `{"status":"success"}` {
		t.Fatalf("durable result=%+v err=%v", result, err)
	}

	brokerA := workergateway.NewBroker(nil)
	brokerA.SetDurableStore(repoA)
	if err := brokerA.Register(identity); err != nil {
		t.Fatal(err)
	}
	dispatchDone := make(chan error, 1)
	var dispatched map[string]string
	go func() {
		dispatchDone <- brokerA.Dispatch(
			ctx, identity, workergateway.ActionVerify,
			map[string]string{"probe": "two-replica"},
			&dispatched,
		)
	}()
	var crossReplica *workergateway.Task
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		crossReplica, err = repoB.ClaimWorkerCommand(ctx, identity, time.Minute)
		if err == nil {
			break
		}
		if !errors.Is(err, workergateway.ErrNoWork) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if crossReplica == nil {
		t.Fatal("second repository did not observe broker command")
	}
	if err := repoB.CompleteWorkerCommand(ctx, identity, workergateway.Completion{
		TaskID: crossReplica.ID, DeliveryFence: crossReplica.DeliveryFence,
		Payload: json.RawMessage(`{"replica":"b"}`),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-dispatchDone:
		if err != nil || dispatched["replica"] != "b" {
			t.Fatalf("cross-replica dispatch=%v result=%v", err, dispatched)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("durable broker dispatch did not observe second-replica completion")
	}
	restartedBroker := workergateway.NewBroker(nil)
	restartedBroker.SetDurableStore(repoB)
	if status := restartedBroker.Status(); status.Authority != "postgresql" ||
		status.RegisteredSessions < 1 {
		t.Fatalf("restarted broker did not recover durable status: %+v", status)
	}

	uploadID := uuid.NewString()
	destination := "/tmp/gateway-spool-artifact"
	if err := repoA.PrepareWorkerUpload(
		ctx, identity, uploadID, destination, 1024,
	); err != nil {
		t.Fatal(err)
	}
	if err := repoB.PrepareWorkerUpload(
		ctx, identity, uploadID, destination, 1024,
	); err != nil {
		t.Fatalf("exact stable upload replay was rejected: %v", err)
	}
	if err := repoB.PrepareWorkerUpload(
		ctx, identity, uploadID, destination, 2048,
	); err == nil {
		t.Fatal("stable upload id accepted a different immutable limit")
	}
	uploadOne, err := repoB.ClaimWorkerUpload(
		ctx, identity, uploadID, 100*time.Millisecond,
	)
	if err != nil || uploadOne.Fence != 1 {
		t.Fatalf("first upload claim=%+v err=%v", uploadOne, err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE worker_gateway_uploads
		SET upload_lease_expires_at = clock_timestamp() - interval '1 second'
		WHERE id = $1
	`, uploadID); err != nil {
		t.Fatal(err)
	}
	uploadTwo, err := repoA.ClaimWorkerUpload(
		ctx, identity, uploadID, time.Minute,
	)
	if err != nil || uploadTwo.Fence != 2 {
		t.Fatalf("second upload claim=%+v err=%v", uploadTwo, err)
	}
	digest := strings.Repeat("a", 64)
	if err := repoB.CompleteWorkerUpload(
		ctx, identity, uploadID, uploadOne.Fence, digest, 3,
	); !errors.Is(err, workergateway.ErrStaleFence) {
		t.Fatalf("stale upload completion was not fenced: %v", err)
	}
	if err := repoA.CompleteWorkerUpload(
		ctx, identity, uploadID, uploadTwo.Fence, digest, 3,
	); err != nil {
		t.Fatal(err)
	}
	completed, err := repoB.ClaimWorkerUpload(
		ctx, identity, uploadID, time.Minute,
	)
	if err != nil || !completed.Completed || completed.Fence != 2 ||
		completed.Digest != digest || completed.Size != 3 {
		t.Fatalf("completed upload replay=%+v err=%v", completed, err)
	}
	gatewayStatus, err := repoA.WorkerGatewayStatus(ctx)
	if err != nil || gatewayStatus.Authority != "postgresql" ||
		gatewayStatus.RegisteredSessions < 1 ||
		gatewayStatus.PendingTasks != 0 || gatewayStatus.PendingUploads != 0 ||
		gatewayStatus.ActiveIssuers != 1 ||
		gatewayStatus.ActiveCertificates != 1 {
		t.Fatalf("gateway status=%+v err=%v", gatewayStatus, err)
	}
	if err := repoB.RevokeWorkerSession(ctx, identity, "fixture complete"); err != nil {
		t.Fatal(err)
	}
	if err := repoA.AuthorizeWorkerCertificate(
		ctx, identity, presentedCertificate,
	); err == nil {
		t.Fatal("revoked workload certificate remained authorized")
	}
	issuers, certificates, err := repoA.WorkloadIdentityInventory(ctx)
	if err != nil || len(issuers) == 0 || len(certificates) == 0 ||
		certificates[0].State != "revoked" {
		t.Fatalf(
			"workload identity inventory issuers=%+v certificates=%+v err=%v",
			issuers, certificates, err,
		)
	}
}

func testArtifactBudgetLifecycle(
	t *testing.T,
	ctx context.Context,
	iamRepo *persistence.IAMRepository,
	jobRepo *persistence.JobRepository,
) {
	t.Helper()
	project, err := iamRepo.CreateProject(ctx, "artifact-budget", "artifact budget fixture")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := iamRepo.GetProjectPolicy(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	policy, err = iamRepo.UpdateProjectPolicy(ctx, project.ID, persistence.ProjectPolicyUpdate{
		Version: policy.Version, MaxQueuedJobs: policy.MaxQueuedJobs,
		MaxActiveJobs:          policy.MaxActiveJobs,
		MaxDailySubmissions:    policy.MaxDailySubmissions,
		MaxActiveVCPUs:         policy.MaxActiveVCPUs,
		MaxActiveMemoryMiB:     policy.MaxActiveMemoryMiB,
		MaxActiveDiskGiB:       policy.MaxActiveDiskGiB,
		MaxArtifactBytesPerJob: 1 << 35,
	}, "artifact-budget-test")
	if err != nil {
		t.Fatal(err)
	}
	if policy.MaxArtifactBytesPerJob != 1<<35 {
		t.Fatalf("large artifact policy was truncated: %+v", policy)
	}
	policy, err = iamRepo.UpdateProjectPolicy(ctx, project.ID, persistence.ProjectPolicyUpdate{
		Version: policy.Version, MaxQueuedJobs: policy.MaxQueuedJobs,
		MaxActiveJobs:          policy.MaxActiveJobs,
		MaxDailySubmissions:    policy.MaxDailySubmissions,
		MaxActiveVCPUs:         policy.MaxActiveVCPUs,
		MaxActiveMemoryMiB:     policy.MaxActiveMemoryMiB,
		MaxActiveDiskGiB:       policy.MaxActiveDiskGiB,
		MaxArtifactBytesPerJob: 100,
	}, "artifact-budget-small-test")
	if err != nil {
		t.Fatal(err)
	}
	request := &builder.BuildRequest{
		ProjectID: project.ID, PackageName: "app-misc/artifact-budget",
		Arch: "amd64", ResourceClass: "small",
		MachineSpec: map[string]string{
			"cores": "2", "memory": "4096", "disk_size": "40",
		},
		IdempotencyKey: "artifact-budget",
	}
	now := time.Now().UTC()
	status := &builder.BuildStatus{
		JobID: uuid.NewString(), ProjectID: project.ID, Status: "queued",
		PackageName: request.PackageName, Arch: "amd64",
		CreatedAt: now, UpdatedAt: now, Request: request,
	}
	if result, err := jobRepo.CreateJob(ctx, request, status); err != nil || !result.Created {
		t.Fatalf("create artifact budget job=%+v err=%v", result, err)
	}
	claim, err := jobRepo.ClaimNext(ctx, "artifact-budget-worker", time.Minute)
	if err != nil || claim == nil || claim.Status.JobID != status.JobID {
		t.Fatalf("claim artifact budget job=%+v err=%v", claim, err)
	}
	budget, err := jobRepo.SetArtifactGenerationBytes(
		ctx, claim.Status, "collected", 60,
	)
	if err != nil || budget.ActiveBytes != 60 || budget.LimitBytes != 100 {
		t.Fatalf("collected artifact budget=%+v err=%v", budget, err)
	}
	generations := [2]string{"signed-race-a", "signed-race-b"}
	var raceErrs [2]error
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range generations {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, raceErrs[i] = jobRepo.SetArtifactGenerationBytes(
				ctx, claim.Status, generations[i], 40,
			)
		}(i)
	}
	close(start)
	wg.Wait()
	successes := 0
	for i, raceErr := range raceErrs {
		if raceErr == nil {
			successes++
			if err := jobRepo.ReleaseArtifactGeneration(
				ctx, claim.Status, generations[i], "race_test_complete",
			); err != nil {
				t.Fatal(err)
			}
		} else if !strings.Contains(raceErr.Error(), "budget exceeded") {
			t.Fatalf("unexpected concurrent artifact error: %v", raceErr)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent artifact reservations successes=%d errors=%v", successes, raceErrs)
	}
	if _, err := jobRepo.SetArtifactGenerationBytes(
		ctx, claim.Status, "signed", 41,
	); err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("artifact budget accepted 101 bytes: %v", err)
	}
	budget, err = jobRepo.SetArtifactGenerationBytes(ctx, claim.Status, "signed", 40)
	if err != nil || budget.ActiveBytes != 100 || budget.PeakBytes != 100 {
		t.Fatalf("dual-generation artifact budget=%+v err=%v", budget, err)
	}
	if err := jobRepo.ReleaseArtifactGeneration(
		ctx, claim.Status, "collected", "signed_adopted",
	); err != nil {
		t.Fatal(err)
	}
	policy, err = iamRepo.GetProjectPolicy(ctx, project.ID)
	if err != nil || policy.QuarantineBytes != 40 || policy.ActiveArtifactBudgets != 1 {
		t.Fatalf("active artifact policy usage=%+v err=%v", policy, err)
	}
	if _, err := iamRepo.UpdateProjectPolicy(ctx, project.ID, persistence.ProjectPolicyUpdate{
		Version: policy.Version, MaxQueuedJobs: policy.MaxQueuedJobs,
		MaxActiveJobs:          policy.MaxActiveJobs,
		MaxDailySubmissions:    policy.MaxDailySubmissions,
		MaxActiveVCPUs:         policy.MaxActiveVCPUs,
		MaxActiveMemoryMiB:     policy.MaxActiveMemoryMiB,
		MaxActiveDiskGiB:       policy.MaxActiveDiskGiB,
		MaxArtifactBytesPerJob: 10,
	}, "artifact-budget-lower"); err != nil {
		t.Fatal(err)
	}
	budget, err = jobRepo.SetArtifactGenerationBytes(ctx, claim.Status, "signed", 90)
	if err != nil || budget.LimitBytes != 100 || budget.ActiveBytes != 90 {
		t.Fatalf("in-flight budget was preempted=%+v err=%v", budget, err)
	}
	releaseRace := make(chan struct{})
	var setRaceErr, releaseRaceErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-releaseRace
		_, setRaceErr = jobRepo.SetArtifactGenerationBytes(
			ctx, claim.Status, "signed", 95,
		)
	}()
	go func() {
		defer wg.Done()
		<-releaseRace
		releaseRaceErr = jobRepo.ReleaseArtifactGeneration(
			ctx, claim.Status, "signed", "lock_order_test",
		)
	}()
	close(releaseRace)
	wg.Wait()
	if setRaceErr != nil || releaseRaceErr != nil {
		t.Fatalf(
			"artifact set/release lock-order race failed: set=%v release=%v",
			setRaceErr, releaseRaceErr,
		)
	}
	if _, err := jobRepo.SetArtifactGenerationBytes(
		ctx, claim.Status, "signed", 90,
	); err != nil {
		t.Fatalf("restore artifact generation after lock-order race: %v", err)
	}
	completed := *claim.Status
	completed.Status = "completed"
	completed.UpdatedAt = time.Now().UTC()
	if err := jobRepo.RecordTransition(ctx, claim.Status, &completed); err != nil {
		t.Fatal(err)
	}
	policy, err = iamRepo.GetProjectPolicy(ctx, project.ID)
	if err != nil || policy.QuarantineBytes != 0 || policy.ActiveArtifactBudgets != 0 {
		t.Fatalf("released artifact policy usage=%+v err=%v", policy, err)
	}
}

func testProjectPhaseCaps(
	t *testing.T,
	ctx context.Context,
	iamRepo *persistence.IAMRepository,
	jobRepo *persistence.JobRepository,
) {
	t.Helper()
	project, err := iamRepo.CreateProject(ctx, "phase-cap", "phase cap fixture")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := iamRepo.GetProjectPolicy(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	policy, err = iamRepo.UpdateProjectPolicy(ctx, project.ID, persistence.ProjectPolicyUpdate{
		Version: policy.Version, MaxQueuedJobs: 4, MaxActiveJobs: 4,
		MaxDailySubmissions: 4, MaxActiveVCPUs: 16,
		MaxActiveMemoryMiB: 32768, MaxActiveDiskGiB: 200,
		MaxArtifactBytesPerJob: policy.MaxArtifactBytesPerJob,
		MaxClaimedAttempts:     2, MaxProvisionAttempts: 1,
		MaxBuildAttempts: 1, MaxVerifyAttempts: 1, MaxPublishAttempts: 1,
	}, "phase-cap-test")
	if err != nil || policy.MaxProvisionAttempts != 1 {
		t.Fatalf("configure phase cap policy=%+v err=%v", policy, err)
	}

	claims := make([]*builder.SchedulerClaim, 0, 2)
	jobIDs := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		request := &builder.BuildRequest{
			ProjectID:   project.ID,
			PackageName: fmt.Sprintf("app-misc/phase-cap-%d", i),
			Arch:        "amd64", ResourceClass: "small",
			IdempotencyKey: fmt.Sprintf("phase-cap-%d", i),
			MachineSpec: map[string]string{
				"cores": "2", "memory": "2048", "disk_size": "20",
			},
		}
		now := time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		status := &builder.BuildStatus{
			JobID: uuid.NewString(), ProjectID: project.ID, Status: "queued",
			PackageName: request.PackageName, Arch: "amd64",
			CreatedAt: now, UpdatedAt: now, Request: request,
		}
		if result, err := jobRepo.CreateJob(ctx, request, status); err != nil || !result.Created {
			t.Fatalf("create phase-cap job %d: result=%+v err=%v", i, result, err)
		}
		jobIDs = append(jobIDs, status.JobID)
	}
	for i := 0; i < 2; i++ {
		claim, err := jobRepo.ClaimNext(
			ctx, fmt.Sprintf("phase-cap-worker-%d", i), time.Minute,
		)
		if err != nil || claim == nil || claim.Status.ProjectID != project.ID {
			t.Fatalf("claim phase-cap job %d: claim=%+v err=%v", i, claim, err)
		}
		claims = append(claims, claim)
	}
	if extra, err := jobRepo.ClaimNext(ctx, "phase-cap-worker-extra", time.Minute); err != nil || extra != nil {
		t.Fatalf("claimed-cap accepted a third attempt: claim=%+v err=%v", extra, err)
	}

	firstProvision := *claims[0].Status
	firstProvision.Status, firstProvision.UpdatedAt = "provisioning", time.Now().UTC()
	if err := jobRepo.RecordTransition(ctx, claims[0].Status, &firstProvision); err != nil {
		t.Fatal(err)
	}
	secondProvision := *claims[1].Status
	secondProvision.Status, secondProvision.UpdatedAt = "provisioning", time.Now().UTC()
	err = jobRepo.RecordTransition(ctx, claims[1].Status, &secondProvision)
	capacity, ok := builder.AsPhaseCapacityError(err)
	if !ok || capacity.Phase != "provision" || capacity.Limit != 1 || capacity.Used != 1 {
		t.Fatalf("provision phase cap error=%+v raw=%v", capacity, err)
	}

	firstBuild := firstProvision
	firstBuild.Status, firstBuild.UpdatedAt = "building", time.Now().UTC()
	if err := jobRepo.RecordTransition(ctx, &firstProvision, &firstBuild); err != nil {
		t.Fatal(err)
	}
	secondProvision.UpdatedAt = time.Now().UTC()
	if err := jobRepo.RecordTransition(ctx, claims[1].Status, &secondProvision); err != nil {
		t.Fatalf("provision phase was not admitted after release: %v", err)
	}
	for _, current := range []*builder.BuildStatus{&firstBuild, &secondProvision} {
		failed := *current
		failed.Status, failed.Error, failed.UpdatedAt = "failed", "phase cap fixture cleanup", time.Now().UTC()
		if err := jobRepo.RecordTransition(ctx, current, &failed); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := jobRepo.CancelJob(ctx, jobIDs[2], "phase cap fixture cleanup"); err != nil {
		t.Fatal(err)
	}
	policy, err = iamRepo.GetProjectPolicy(ctx, project.ID)
	if err != nil || policy.ActiveJobs != 0 || policy.ClaimedReservations != 0 ||
		policy.ProvisionReservations != 0 || policy.BuildReservations != 0 {
		t.Fatalf("phase cap cleanup policy=%+v err=%v", policy, err)
	}
}

func testExecutorCapabilityRouting(
	t *testing.T,
	ctx context.Context,
	db *persistence.Database,
	iamRepo *persistence.IAMRepository,
	jobRepo *persistence.JobRepository,
) {
	t.Helper()
	project, err := iamRepo.CreateProject(
		ctx, "capability-routing", "executor capability routing fixture",
	)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := iamRepo.GetProjectPolicy(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iamRepo.UpdateProjectPolicy(ctx, project.ID, persistence.ProjectPolicyUpdate{
		Version: policy.Version, MaxQueuedJobs: 2, MaxActiveJobs: 2,
		MaxDailySubmissions: 2, MaxActiveVCPUs: 4,
		MaxActiveMemoryMiB: 8192, MaxActiveDiskGiB: 80,
		MaxArtifactBytesPerJob: policy.MaxArtifactBytesPerJob,
		MaxClaimedAttempts:     2, MaxProvisionAttempts: 2,
		MaxBuildAttempts: 2, MaxVerifyAttempts: 2, MaxPublishAttempts: 2,
	}, "capability-routing-test"); err != nil {
		t.Fatal(err)
	}

	resolved := &catalog.ResolvedBuildContext{
		ProfileID: "pe/amd64/capability-v1",
		Arch:      "amd64", Provider: "pve", ExecutionZone: "lan-a",
		BuildMode: "native-gentoo", ImageID: "pe/base-g6",
		ImageGeneration: "g6", ResourceClass: "small",
		MachineSpec: map[string]string{
			"cores": "2", "memory": "2048", "disk_size": "20",
		},
		MaxRuntimeMinutes: 60, CloudCostMicrounitsPerMinute: 100,
	}
	request := &builder.BuildRequest{
		ProjectID: project.ID, PackageName: "app-misc/capability-route",
		Arch: "amd64", ResourceClass: "small",
		MachineSpec:     resolved.MachineSpec,
		ResolvedContext: resolved,
		IdempotencyKey:  "capability-route-exact",
	}
	now := time.Now().UTC()
	status := &builder.BuildStatus{
		JobID: uuid.NewString(), ProjectID: project.ID, Status: "queued",
		PackageName: request.PackageName, Arch: request.Arch,
		CreatedAt: now, UpdatedAt: now, Request: request,
		ResolvedContext: resolved,
	}

	var exactCapabilities []string
	for _, phase := range []string{"provision", "build", "verify", "publish"} {
		labels, requirementErr := builder.PhaseCapabilityRequirements(request, phase)
		if requirementErr != nil {
			t.Fatal(requirementErr)
		}
		exactCapabilities = append(exactCapabilities, labels...)
	}
	exactCapabilities, err = builder.NormalizeExecutorCapabilities(exactCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	wrongCapabilities := append([]string(nil), exactCapabilities...)
	for index, label := range wrongCapabilities {
		if label == "zone:lan-a" {
			wrongCapabilities[index] = "zone:lan-b"
		}
	}
	wrongCapabilities, err = builder.NormalizeExecutorCapabilities(wrongCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	wrongWorker := "capability-wrong-" + uuid.NewString()
	if claim, err := jobRepo.ClaimPhaseWork(
		ctx, wrongWorker, time.Minute, wrongCapabilities,
	); err != nil || claim != nil {
		t.Fatalf("register mismatched executor: claim=%+v err=%v", claim, err)
	}
	if result, err := jobRepo.CreateJob(ctx, request, status); err != nil || !result.Created {
		t.Fatalf("create capability-routed job: result=%+v err=%v", result, err)
	}
	if claim, err := jobRepo.ClaimNext(
		ctx, "capability-admission-wrong", time.Minute,
	); err != nil || claim != nil {
		t.Fatalf("mismatched executor admitted job: claim=%+v err=%v", claim, err)
	}
	runtimeStatus, err := jobRepo.RuntimeStatus(ctx)
	if err != nil || runtimeStatus.UnschedulableTasks < 1 ||
		runtimeStatus.CapabilityWorkers < 1 {
		t.Fatalf("capability routing status=%+v err=%v", runtimeStatus, err)
	}

	exactWorker := "capability-exact-" + uuid.NewString()
	if claim, err := jobRepo.ClaimPhaseWork(
		ctx, exactWorker, time.Minute, exactCapabilities,
	); err != nil || claim != nil {
		t.Fatalf("register exact executor: claim=%+v err=%v", claim, err)
	}
	if claim, err := jobRepo.ClaimNext(
		ctx, "capability-admission-still-wrong", time.Minute,
	); err != nil || claim != nil {
		t.Fatalf(
			"current mismatched admission worker borrowed another worker's capability: claim=%+v err=%v",
			claim, err,
		)
	}
	schedulerClaim, err := jobRepo.ClaimNext(
		ctx, "capability-admission-exact", time.Minute,
		append(exactCapabilities, "worker-kind:admission"),
	)
	if err != nil || schedulerClaim == nil || schedulerClaim.Status.JobID != status.JobID {
		t.Fatalf("exact executor did not admit job: claim=%+v err=%v", schedulerClaim, err)
	}
	var leaseKind string
	if err := db.Pool().QueryRow(ctx, `
		SELECT lease_kind FROM worker_leases WHERE attempt_id = $1
	`, schedulerClaim.Status.AttemptID).Scan(&leaseKind); err != nil || leaseKind != "admission" {
		t.Fatalf("persisted scheduler lease kind=%q err=%v", leaseKind, err)
	}
	if err := jobRepo.ActivatePhasePlan(ctx, schedulerClaim.Status); err != nil {
		t.Fatal(err)
	}
	if claim, err := jobRepo.ClaimPhaseWork(
		ctx, wrongWorker, time.Minute, wrongCapabilities, "provision",
	); err != nil || claim != nil {
		t.Fatalf("mismatched executor claimed phase: claim=%+v err=%v", claim, err)
	}
	phaseClaim, err := jobRepo.ClaimPhaseWork(
		ctx, exactWorker, time.Minute, exactCapabilities, "provision",
	)
	if err != nil || phaseClaim == nil || phaseClaim.JobID != status.JobID {
		t.Fatalf("exact executor did not claim phase: claim=%+v err=%v", phaseClaim, err)
	}
	if err := jobRepo.FailPhaseWork(ctx, phaseClaim, "capability fixture cleanup"); err != nil {
		t.Fatal(err)
	}
	failed := *schedulerClaim.Status
	failed.Status, failed.Error = "failed", "capability fixture cleanup"
	failed.UpdatedAt = time.Now().UTC()
	if err := jobRepo.RecordTransition(ctx, schedulerClaim.Status, &failed); err != nil {
		t.Fatal(err)
	}
	var storedJobRequirements, storedPhaseRequirements []string
	if err := db.Pool().QueryRow(ctx, `
		SELECT j.required_capabilities, w.required_capabilities
		FROM build_jobs j
		JOIN phase_work_items w ON w.job_id = j.id AND w.phase = 'provision'
		WHERE j.id = $1
	`, status.JobID).Scan(&storedJobRequirements, &storedPhaseRequirements); err != nil {
		t.Fatal(err)
	}
	expected, err := builder.PhaseCapabilityRequirements(request, "provision")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(storedJobRequirements, "\n") != strings.Join(expected, "\n") ||
		strings.Join(storedPhaseRequirements, "\n") != strings.Join(expected, "\n") {
		t.Fatalf(
			"stored requirements job=%v phase=%v expected=%v",
			storedJobRequirements, storedPhaseRequirements, expected,
		)
	}
}

func testDurablePhaseWorkQueue(
	t *testing.T,
	ctx context.Context,
	db *persistence.Database,
	iamRepo *persistence.IAMRepository,
	jobRepo *persistence.JobRepository,
) {
	t.Helper()
	project, err := iamRepo.CreateProject(ctx, "phase-work", "phase work fixture")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := iamRepo.GetProjectPolicy(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = iamRepo.UpdateProjectPolicy(ctx, project.ID, persistence.ProjectPolicyUpdate{
		Version: policy.Version, MaxQueuedJobs: 4, MaxActiveJobs: 4,
		MaxDailySubmissions: 4, MaxActiveVCPUs: 16,
		MaxActiveMemoryMiB: 32768, MaxActiveDiskGiB: 200,
		MaxArtifactBytesPerJob: policy.MaxArtifactBytesPerJob,
		MaxClaimedAttempts:     2, MaxProvisionAttempts: 1,
		MaxBuildAttempts: 1, MaxVerifyAttempts: 1, MaxPublishAttempts: 1,
	}, "phase-work-test")
	if err != nil {
		t.Fatal(err)
	}

	claims := make([]*builder.SchedulerClaim, 0, 2)
	for i := 0; i < 2; i++ {
		request := &builder.BuildRequest{
			ProjectID:   project.ID,
			PackageName: fmt.Sprintf("app-misc/phase-work-%d", i),
			Arch:        "amd64", ResourceClass: "small",
			IdempotencyKey: fmt.Sprintf("phase-work-%d", i),
			MachineSpec: map[string]string{
				"cores": "2", "memory": "2048", "disk_size": "20",
			},
		}
		now := time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		status := &builder.BuildStatus{
			JobID: uuid.NewString(), ProjectID: project.ID, Status: "queued",
			PackageName: request.PackageName, Arch: "amd64",
			CreatedAt: now, UpdatedAt: now, Request: request,
		}
		if result, err := jobRepo.CreateJob(ctx, request, status); err != nil || !result.Created {
			t.Fatalf("create phase work job %d: result=%+v err=%v", i, result, err)
		}
		claim, err := jobRepo.ClaimNext(
			ctx, fmt.Sprintf("phase-work-attempt-%d", i), time.Minute,
		)
		if err != nil || claim == nil || claim.Status.ProjectID != project.ID {
			t.Fatalf("claim phase work attempt %d: claim=%+v err=%v", i, claim, err)
		}
		claims = append(claims, claim)
	}
	for _, claim := range claims {
		if err := jobRepo.ActivatePhasePlan(ctx, claim.Status); err != nil {
			t.Fatalf("activate phase plan: %v", err)
		}
	}
	status, err := jobRepo.PhaseWorkStatus(ctx, project.ID)
	if err != nil || status.Shadow != 0 || status.Active != 8 ||
		status.Ready != 2 || status.Blocked != 6 {
		t.Fatalf("initial phase work status=%+v err=%v", status, err)
	}

	first, err := jobRepo.ClaimPhaseWork(
		ctx, "phase-executor-a", 100*time.Millisecond,
		legacyExecutorCapabilities, "provision",
	)
	if err != nil || first == nil || first.Phase != "provision" {
		latest, statusErr := jobRepo.PhaseWorkStatus(ctx, project.ID)
		policyNow, policyErr := iamRepo.GetProjectPolicy(ctx, project.ID)
		t.Fatalf(
			"first phase claim=%+v err=%v status=%+v statusErr=%v policy=%+v policyErr=%v",
			first, err, latest, statusErr, policyNow, policyErr,
		)
	}
	if blocked, err := jobRepo.ClaimPhaseWork(
		ctx, "phase-executor-b", time.Minute,
		legacyExecutorCapabilities, "provision",
	); err != nil || blocked != nil {
		t.Fatalf("provision cap oversold: claim=%+v err=%v", blocked, err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE phase_work_items
		SET lease_expires_at = clock_timestamp() - interval '1 second'
		WHERE id = $1
	`, first.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := jobRepo.ClaimPhaseWork(
		ctx, "phase-executor-c", time.Minute,
		legacyExecutorCapabilities, "provision",
	)
	if err != nil || reclaimed == nil || reclaimed.ID != first.ID ||
		reclaimed.ClaimFence <= first.ClaimFence {
		var workState, reservationPhase string
		var workExpires *time.Time
		var attemptLease time.Time
		diagErr := db.Pool().QueryRow(ctx, `
			SELECT w.state, w.lease_expires_at, r.phase, l.expires_at
			FROM phase_work_items w
			JOIN project_resource_reservations r ON r.attempt_id = w.attempt_id
			JOIN worker_leases l ON l.attempt_id = w.attempt_id
			WHERE w.id = $1
		`, first.ID).Scan(
			&workState, &workExpires, &reservationPhase, &attemptLease,
		)
		t.Fatalf(
			"reclaimed phase work=%+v first=%+v err=%v state=%s workExpiry=%s reservation=%s attemptExpiry=%s diagErr=%v",
			reclaimed, first, err, workState, workExpires, reservationPhase,
			attemptLease, diagErr,
		)
	}
	runtime, err := jobRepo.RuntimeStatus(ctx)
	if err != nil || runtime.LeaseExpiries.PhaseReclaimed < 1 {
		t.Fatalf("phase lease expiry snapshot=%+v err=%v", runtime.LeaseExpiries, err)
	}
	if err := jobRepo.CompletePhaseWork(ctx, first); err == nil ||
		!strings.Contains(err.Error(), "lock claimed phase work") {
		t.Fatalf("stale phase fence completed reclaimed work: %v", err)
	}
	// The renewal ticker runs beside the phase-executing goroutine and both
	// hold this pointer, so renewal must extend only the durable lease and
	// leave the shared claim untouched.
	claimedExpiry := reclaimed.LeaseExpiresAt
	if err := jobRepo.RenewPhaseWork(ctx, reclaimed, time.Minute); err != nil {
		t.Fatal(err)
	}
	var renewedExpiry time.Time
	if err := db.Pool().QueryRow(ctx, `
		SELECT lease_expires_at FROM phase_work_items WHERE id = $1
	`, reclaimed.ID).Scan(&renewedExpiry); err != nil {
		t.Fatal(err)
	}
	if !reclaimed.LeaseExpiresAt.Equal(claimedExpiry) ||
		!renewedExpiry.After(claimedExpiry) {
		t.Fatalf(
			"phase renewal claim=%s claimed=%s durable=%s",
			reclaimed.LeaseExpiresAt, claimedExpiry, renewedExpiry,
		)
	}
	if err := jobRepo.CompletePhaseWork(ctx, reclaimed); err != nil {
		t.Fatal(err)
	}
	second, err := jobRepo.ClaimPhaseWork(
		ctx, "phase-executor-b", time.Minute,
		legacyExecutorCapabilities, "provision",
	)
	if err != nil || second == nil || second.ID == first.ID {
		rows, rowsErr := db.Pool().Query(ctx, `
			SELECT w.id::text, w.state, w.available_at, r.phase,
			       l.expires_at, j.state, a.state
			FROM phase_work_items w
			JOIN project_resource_reservations r ON r.attempt_id = w.attempt_id
			JOIN worker_leases l ON l.attempt_id = w.attempt_id
			JOIN build_jobs j ON j.id = w.job_id
			JOIN build_attempts a ON a.id = w.attempt_id
			WHERE w.project_id = $1 AND w.phase = 'provision'
			ORDER BY w.id
		`, project.ID)
		var diagnostics []string
		if rowsErr == nil {
			for rows.Next() {
				var id, state, phase, jobState, attemptState string
				var available, lease time.Time
				if scanErr := rows.Scan(
					&id, &state, &available, &phase, &lease, &jobState, &attemptState,
				); scanErr != nil {
					diagnostics = append(diagnostics, scanErr.Error())
					break
				}
				diagnostics = append(diagnostics, fmt.Sprintf(
					"%s:%s:available=%s:reservation=%s:lease=%s:job=%s:attempt=%s",
					id, state, available, phase, lease, jobState, attemptState,
				))
			}
			rows.Close()
		}
		t.Fatalf(
			"second provision claim=%+v err=%v rowsErr=%v diagnostics=%v",
			second, err, rowsErr, diagnostics,
		)
	}
	if err := jobRepo.CompletePhaseWork(ctx, second); err != nil {
		t.Fatal(err)
	}
	status, err = jobRepo.PhaseWorkStatus(ctx, project.ID)
	if err != nil || status.Completed != 2 || status.Ready != 2 ||
		status.Claimed != 0 {
		t.Fatalf("completed phase work status=%+v err=%v", status, err)
	}
	for _, claim := range claims {
		if _, err := jobRepo.CancelJob(
			ctx, claim.Status.JobID, "phase work fixture cleanup",
		); err != nil {
			t.Fatal(err)
		}
	}
}

func testResourceReservationReleasePaths(
	t *testing.T,
	ctx context.Context,
	db *persistence.Database,
	iamRepo *persistence.IAMRepository,
	jobRepo *persistence.JobRepository,
) {
	t.Helper()
	createAndClaim := func(
		projectName, packageName, worker string,
		lease time.Duration,
	) (*builder.SchedulerClaim, string) {
		t.Helper()
		project, err := iamRepo.CreateProject(ctx, projectName, "resource release fixture")
		if err != nil {
			t.Fatal(err)
		}
		request := &builder.BuildRequest{
			ProjectID: project.ID, PackageName: packageName, Arch: "amd64",
			ResourceClass: "small",
			MachineSpec: map[string]string{
				"cores": "2", "memory": "4096", "disk_size": "40",
			},
			IdempotencyKey: projectName,
		}
		now := time.Now().UTC()
		status := &builder.BuildStatus{
			JobID: uuid.NewString(), ProjectID: project.ID, Status: "queued",
			PackageName: packageName, Arch: "amd64", CreatedAt: now,
			UpdatedAt: now, Request: request,
		}
		if result, err := jobRepo.CreateJob(ctx, request, status); err != nil || !result.Created {
			t.Fatalf("create resource release job=%+v err=%v", result, err)
		}
		claim, err := jobRepo.ClaimNext(ctx, worker, lease)
		if err != nil || claim == nil || claim.Status.JobID != status.JobID {
			t.Fatalf("claim resource release job=%+v err=%v", claim, err)
		}
		return claim, status.JobID
	}
	assertReleased := func(attemptID, reason string) {
		t.Helper()
		var state, actualReason string
		var released bool
		if err := db.Pool().QueryRow(ctx, `
			SELECT state, release_reason, released_at IS NOT NULL
			FROM project_resource_reservations
			WHERE attempt_id = $1
		`, attemptID).Scan(&state, &actualReason, &released); err != nil {
			t.Fatal(err)
		}
		if state != "released" || actualReason != reason || !released {
			t.Fatalf("reservation %s state=%s reason=%s released=%v",
				attemptID, state, actualReason, released)
		}
	}

	cancelClaim, cancelJobID := createAndClaim(
		"resource-cancel", "app-misc/cancel-resource", "resource-cancel-worker", time.Minute,
	)
	if _, err := jobRepo.CancelJob(ctx, cancelJobID, "integration cancellation"); err != nil {
		t.Fatal(err)
	}
	assertReleased(cancelClaim.Status.AttemptID, "job_canceled")

	completeClaim, _ := createAndClaim(
		"resource-complete", "app-misc/complete-resource", "resource-complete-worker", time.Minute,
	)
	completed := *completeClaim.Status
	completed.Status = "completed"
	completed.UpdatedAt = time.Now().UTC()
	if err := jobRepo.RecordTransition(ctx, completeClaim.Status, &completed); err != nil {
		t.Fatal(err)
	}
	assertReleased(completeClaim.Status.AttemptID, "job_completed")

	expiryClaim, expiryJobID := createAndClaim(
		"resource-expiry", "app-misc/expire-resource", "resource-expiry-worker", 100*time.Millisecond,
	)
	if _, err := db.Pool().Exec(ctx, `
		UPDATE build_jobs SET max_attempts = 1 WHERE id = $1
	`, expiryJobID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE worker_leases
		SET expires_at = clock_timestamp() - interval '1 second'
		WHERE attempt_id = $1
	`, expiryClaim.Status.AttemptID); err != nil {
		t.Fatal(err)
	}
	if claim, err := jobRepo.ClaimNext(ctx, "resource-recovery-worker", time.Minute); err != nil || claim != nil {
		t.Fatalf("lease recovery unexpectedly claimed=%+v err=%v", claim, err)
	}
	assertReleased(expiryClaim.Status.AttemptID, "lease_expired")
}

func testConcurrentIdempotentAdmission(
	t *testing.T,
	ctx context.Context,
	db *persistence.Database,
	iamRepo *persistence.IAMRepository,
	jobRepo *persistence.JobRepository,
) {
	t.Helper()
	project, err := iamRepo.CreateProject(ctx, "idempotency-race", "admission race fixture")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := iamRepo.GetProjectPolicy(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iamRepo.UpdateProjectPolicy(ctx, project.ID, persistence.ProjectPolicyUpdate{
		Version: policy.Version, MaxQueuedJobs: 1, MaxActiveJobs: 1,
		MaxDailySubmissions: 1, MaxActiveVCPUs: policy.MaxActiveVCPUs,
		MaxActiveMemoryMiB: policy.MaxActiveMemoryMiB,
		MaxActiveDiskGiB:   policy.MaxActiveDiskGiB,
	}, "integration-test"); err != nil {
		t.Fatal(err)
	}

	requests := [2]*builder.BuildRequest{}
	statuses := [2]*builder.BuildStatus{}
	now := time.Now().UTC()
	for i := range requests {
		requests[i] = &builder.BuildRequest{
			ProjectID: project.ID, PackageName: "app-misc/race",
			Arch: "amd64", IdempotencyKey: "same-key",
		}
		statuses[i] = &builder.BuildStatus{
			JobID: uuid.NewString(), ProjectID: project.ID, Status: "queued",
			PackageName: requests[i].PackageName, Arch: "amd64",
			CreatedAt: now, UpdatedAt: now, Request: requests[i],
		}
	}
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results [2]builder.LedgerCreateResult
		errs    [2]error
	)
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = jobRepo.CreateJob(ctx, requests[i], statuses[i])
		}(i)
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent idempotent admission errors=%v", errs)
	}
	if results[0].JobID != results[1].JobID || results[0].Created == results[1].Created {
		t.Fatalf("concurrent idempotent results=%+v", results)
	}

	var count int
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM build_jobs WHERE project_id = $1
	`, project.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("concurrent idempotent durable rows=%d", count)
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
	testDSN, err := withSearchPath(adminDSN, schema)
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
			INSERT INTO workers (
				id, stable_name, max_slots, executor_protocol
			)
			VALUES ($1, 'signing-build-worker', 1, $2)
		`, workerID, builder.ExecutorProtocolVersion); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO build_jobs (id, project_id, package_atom, state, request, request_digest)
			VALUES (
				$1, (SELECT id FROM projects WHERE name = 'default'),
				'app-misc/hello', 'signing', '{}'::jsonb, 'fixture'
			)
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
		MaxOutputBytes: 1 << 20,
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
			INSERT INTO build_jobs (id, project_id, package_atom, state, request, request_digest)
			VALUES (
				$1, (SELECT id FROM projects WHERE name = 'default'),
				'app-misc/hello', 'signing', '{}'::jsonb, 'stale-fixture'
			)
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

	testDSN, err := withSearchPath(adminDSN, schema)
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
			ProfileID: "pe/amd64/base",
			Arch:      "amd64", Provider: "pve", ExecutionZone: "default",
			BuildMode: "native-gentoo", ImageID: "pe/amd64/base-img-42",
			ImageGeneration: "img-42", ResolvedAt: now,
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
	var leaseRemainingNanos int64
	if err := db.Pool().QueryRow(ctx, `
		SELECT (extract(epoch FROM expires_at - clock_timestamp()) * 1000000000)::bigint
		FROM worker_leases
		WHERE attempt_id = $1
	`, first.Status.AttemptID).Scan(&leaseRemainingNanos); err != nil {
		t.Fatal(err)
	}
	leaseRemaining := time.Duration(leaseRemainingNanos)
	if leaseRemaining > 500*time.Millisecond {
		t.Fatalf("lease deadline used a non-database clock: %v", leaseRemaining)
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
	runtime, err := repo.RuntimeStatus(ctx)
	if err != nil || runtime.LeaseExpiries.AttemptRequeued < 1 {
		t.Fatalf("lease expiry snapshot=%+v err=%v", runtime.LeaseExpiries, err)
	}

	testConcurrentClaims(t, ctx, db, repo, now.Add(2*time.Hour))
	testRuntimeMetadata(t, ctx, db, repo, now.Add(3*time.Hour))
	testCrossReplicaInfraCleanup(t, ctx, db, repo, now.Add(4*time.Hour))
}

func testConcurrentClaims(t *testing.T, ctx context.Context, db *persistence.Database, repo *persistence.JobRepository, now time.Time) {
	t.Helper()
	const jobCount = 24
	if _, err := db.Pool().Exec(ctx, `
		UPDATE project_policies
		SET max_failures_per_hour = $1
		WHERE project_id = (SELECT id FROM projects WHERE name = 'default')
	`, jobCount*4); err != nil {
		t.Fatal(err)
	}
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

func withSearchPath(dsn, value string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse test database URL: %w", err)
	}
	query := parsed.Query()
	query.Set("search_path", value)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
