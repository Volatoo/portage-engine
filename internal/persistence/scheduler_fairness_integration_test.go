package persistence_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/internal/migrations"
	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/pkg/config"
)

func TestPostgresSchedulerFairnessAndAutoscaling(t *testing.T) {
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

	schema := "fair_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	defer func() {
		dropCtx, dropCancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer dropCancel()
		if _, err := admin.Exec(
			dropCtx, "DROP SCHEMA "+identifier+" CASCADE",
		); err != nil {
			t.Logf("drop test schema: %v", err)
		}
	}()

	testDSN, err := withSearchPath(adminDSN, schema)
	if err != nil {
		t.Fatal(err)
	}
	databaseConfig := config.DatabaseConfig{
		Enabled: true, Required: true, URL: testDSN,
		MaxConns: 8, ConnectTimeoutSeconds: 10, HealthTimeoutSeconds: 2,
	}
	runner, err := migrations.NewRunner(ctx, databaseConfig)
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
	db, err := persistence.Open(ctx, databaseConfig)
	if err != nil {
		t.Fatalf("open application database: %v", err)
	}
	defer db.Close()

	iamRepo := persistence.NewIAMRepository(db)
	high, err := iamRepo.CreateProject(
		ctx, "fair-high", "weighted fairness integration project",
	)
	if err != nil {
		t.Fatal(err)
	}
	low, err := iamRepo.CreateProject(
		ctx, "fair-low", "anti-starvation integration project",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE project_policies
		SET priority_weight = CASE
		      WHEN project_id = $1 THEN 300 ELSE 100 END,
		    starvation_threshold_seconds = 300,
		    max_active_jobs = 16,
		    max_claimed_attempts = 16,
		    max_provision_attempts = 16,
		    max_build_attempts = 16,
		    max_verify_attempts = 16,
		    max_publish_attempts = 16,
		    max_failures_per_hour = 1000
		WHERE project_id IN ($1, $2)
	`, high.ID, low.ID); err != nil {
		t.Fatal(err)
	}

	repo := persistence.NewJobRepository(db)
	highJobs := createFairnessJobs(t, ctx, repo, high.ID, "high", 12)
	lowJobs := createFairnessJobs(t, ctx, repo, low.ID, "low", 12)
	claimedByProject := map[string]int{}
	for index := 0; index < 8; index++ {
		claim, err := repo.ClaimNext(
			ctx, fmt.Sprintf("fair-worker-%d", index),
			time.Minute, legacyExecutorCapabilities,
		)
		if err != nil || claim == nil {
			t.Fatalf("weighted claim %d=%+v err=%v", index, claim, err)
		}
		claimedByProject[claim.Status.ProjectID]++
		failFairnessClaim(t, ctx, repo, claim, "weighted fairness sample")
	}
	if claimedByProject[high.ID] < 5 || claimedByProject[low.ID] < 1 {
		t.Fatalf("weighted dispatch distribution=%v", claimedByProject)
	}
	cancelFairnessJobs(t, ctx, repo, append(highJobs, lowJobs...))

	phaseHigh := createFairnessJobs(t, ctx, repo, high.ID, "phase-high", 6)
	phaseLow := createFairnessJobs(t, ctx, repo, low.ID, "phase-low", 6)
	for index := 0; index < 12; index++ {
		claim, err := repo.ClaimNext(
			ctx, fmt.Sprintf("phase-admission-%d", index),
			time.Minute, legacyExecutorCapabilities,
		)
		if err != nil || claim == nil {
			t.Fatalf("phase admission %d=%+v err=%v", index, claim, err)
		}
		if err := repo.ActivatePhasePlan(ctx, claim.Status); err != nil {
			t.Fatalf("activate phase plan %d: %v", index, err)
		}
	}
	phaseByProject := map[string]int{}
	for index := 0; index < 8; index++ {
		claim, err := repo.ClaimPhaseWork(
			ctx, fmt.Sprintf("phase-worker-%d", index), time.Minute,
			legacyExecutorCapabilities, "provision",
		)
		if err != nil || claim == nil {
			t.Fatalf("weighted phase claim %d=%+v err=%v", index, claim, err)
		}
		phaseByProject[claim.ProjectID]++
		if err := repo.CompletePhaseWork(ctx, claim); err != nil {
			t.Fatalf("complete weighted phase claim %d: %v", index, err)
		}
	}
	if phaseByProject[high.ID] < 5 || phaseByProject[low.ID] < 1 {
		t.Fatalf("weighted phase distribution=%v", phaseByProject)
	}
	var fairEvents int
	var fairEvidenceComplete bool
	if err := db.Pool().QueryRow(ctx, `
		SELECT
		  count(*),
		  bool_and(
		    payload ? 'kind'
		    AND payload ? 'priority_weight'
		    AND payload ? 'starvation_threshold_seconds'
		    AND payload ? 'starved'
		    AND payload ? 'virtual_runtime_before'
		    AND payload ? 'virtual_runtime_after'
		    AND payload ? 'item_wait_seconds'
		  )
		FROM job_events
		WHERE event_type = 'scheduler.fair_dispatch'
		  AND job_id = ANY($1::uuid[])
	`, append(phaseHigh, phaseLow...)).Scan(
		&fairEvents, &fairEvidenceComplete,
	); err != nil || fairEvents != 20 || !fairEvidenceComplete {
		t.Fatalf(
			"fair dispatch evidence count=%d complete=%v err=%v",
			fairEvents, fairEvidenceComplete, err,
		)
	}
	cancelFairnessJobs(t, ctx, repo, append(phaseHigh, phaseLow...))

	starvedHigh := createFairnessJobs(t, ctx, repo, high.ID, "starve-high", 1)
	starvedLow := createFairnessJobs(t, ctx, repo, low.ID, "starve-low", 1)
	if _, err := db.Pool().Exec(ctx, `
		UPDATE project_policies
		SET starvation_threshold_seconds = 30
		WHERE project_id = $1
	`, low.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE project_scheduler_fairness
		SET admission_vruntime = CASE
		      WHEN project_id = $1 THEN 1000000000 ELSE 0 END
		WHERE project_id IN ($1, $2)
	`, low.ID, high.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE build_jobs
		SET created_at = clock_timestamp() - interval '2 minutes'
		WHERE id = $1
	`, starvedLow[0]); err != nil {
		t.Fatal(err)
	}
	starvationClaim, err := repo.ClaimNext(
		ctx, "starvation-worker", time.Minute, legacyExecutorCapabilities,
	)
	if err != nil || starvationClaim == nil ||
		starvationClaim.Status.ProjectID != low.ID {
		t.Fatalf(
			"anti-starvation claim=%+v err=%v", starvationClaim, err,
		)
	}
	failFairnessClaim(
		t, ctx, repo, starvationClaim, "anti-starvation sample",
	)
	cancelFairnessJobs(
		t, ctx, repo, append(starvedHigh, starvedLow...),
	)

	autoscaleJobs := createFairnessJobs(
		t, ctx, repo, high.ID, "autoscale", 6,
	)
	const poolID = "pve-default-amd64-test-pool"
	for _, jobID := range autoscaleJobs {
		if _, err := db.Pool().Exec(ctx, `
			UPDATE build_jobs
			SET required_capabilities =
			  '["context:legacy","capacity-pool:pve-default-amd64-test-pool"]'::jsonb
			WHERE id = $1
		`, jobID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE workers
		SET last_seen_at = clock_timestamp() - interval '2 minutes'
		WHERE capabilities ->> 'role' = 'phase-executor'
	`); err != nil {
		t.Fatal(err)
	}
	workerID := uuid.New()
	capabilities := `{
	  "role":"phase-executor",
	  "executor_protocol":5,
	  "labels":[
	    "context:legacy","phase:provision","phase:build",
	    "phase:verify","phase:publish",
	    "capacity-pool:pve-default-amd64-test-pool",
	    "provider:pve","zone:default","arch:amd64",
	    "build-mode:native-gentoo","profile:test/profile",
	    "image:test/image@g1"
	  ]
	}`
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO workers (
		  id, stable_name, desired_state, capabilities, max_slots,
		  last_seen_at, executor_protocol
		) VALUES (
		  $1, 'autoscale-capability-slot', 'active', $2::jsonb, 1,
		  clock_timestamp(), $3
		)
	`, workerID, capabilities, builder.ExecutorProtocolVersion); err != nil {
		t.Fatal(err)
	}
	// Admission workers deliberately carry the same provider/pool labels so
	// they can route phases, but they are not provider capacity. A provider
	// slot ceiling must count only actual phase executors or a separated API
	// role can consume the final slot and suppress scale-up forever.
	admissionID := uuid.New()
	admissionCapabilities := `{
	  "role":"control-plane-admission",
	  "executor_protocol":5,
	  "labels":[
	    "worker-kind:admission",
	    "capacity-pool:pve-default-amd64-test-pool",
	    "provider:pve"
	  ]
	}`
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO workers (
		  id, stable_name, desired_state, capabilities, max_slots,
		  last_seen_at, executor_protocol
		) VALUES (
		  $1, 'autoscale-admission-decoy', 'active', $2::jsonb, 1,
		  clock_timestamp(), $3
		)
	`, admissionID, admissionCapabilities, builder.ExecutorProtocolVersion); err != nil {
		t.Fatal(err)
	}
	policy := builder.SchedulerAutoscalePolicy{
		Mode: "observe", MinSlots: 1, MaxSlots: 10, TargetReady: 2,
		Cooldown: 0, ScaleDownDelay: time.Hour,
		Pools: []builder.SchedulerCapacityPoolDefinition{{
			ID: poolID, Provider: "pve", ExecutionZone: "default",
			Arch: "amd64", BuildMode: "native-gentoo",
			ProfileID: "test/profile", ImageID: "test/image",
			ImageGeneration: "g1",
			Selector: []string{
				"capacity-pool:" + poolID, "provider:pve", "zone:default",
				"arch:amd64", "build-mode:native-gentoo",
				"profile:test/profile", "image:test/image@g1",
			},
		}},
	}
	scaleUp, err := repo.ReconcileAutoscaling(ctx, policy)
	if err != nil || scaleUp.Recommendation != "scale-up" ||
		scaleUp.DesiredSlots != 3 || scaleUp.ActiveSlots != 1 ||
		scaleUp.Backlog != 6 || scaleUp.UnschedulableBacklog != 0 {
		t.Fatalf("scale-up status=%+v err=%v", scaleUp, err)
	}
	if len(scaleUp.Pools) != 1 ||
		scaleUp.Pools[0].ID != poolID ||
		scaleUp.Pools[0].DesiredSlots != 3 ||
		scaleUp.Pools[0].Backlog != 6 ||
		scaleUp.Pools[0].UnschedulableBacklog != 0 {
		t.Fatalf("capacity-pool scale-up status=%+v", scaleUp.Pools)
	}
	actuatePolicy := policy
	actuatePolicy.Mode = "actuate"
	actuatePolicy.ProviderMaxSlots = map[string]int{"pve": 2}
	if actuated, err := repo.ReconcileAutoscaling(
		ctx, actuatePolicy,
	); err != nil || actuated.Pools[0].Recommendation != "scale-up" {
		t.Fatalf("actuated capacity recommendation=%+v err=%v", actuated, err)
	}
	var actuatedKind, actuatedState string
	if err := db.Pool().QueryRow(ctx, `
		SELECT action_kind, state
		FROM scheduler_capacity_actions
		WHERE pool_id = $1
	`, poolID).Scan(&actuatedKind, &actuatedState); err != nil ||
		actuatedKind != "scale-up" || actuatedState != "requested" {
		t.Fatalf(
			"actuated action kind=%q state=%q err=%v",
			actuatedKind, actuatedState, err,
		)
	}
	if _, err := repo.ReconcileAutoscaling(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool().QueryRow(ctx, `
		SELECT state
		FROM scheduler_capacity_actions
		WHERE pool_id = $1
		ORDER BY requested_at DESC
		LIMIT 1
	`, poolID).Scan(&actuatedState); err != nil ||
		actuatedState != "canceled" {
		t.Fatalf("observe mode did not cancel pending action: state=%q err=%v", actuatedState, err)
	}
	cancelFairnessJobs(t, ctx, repo, autoscaleJobs)
	held, err := repo.ReconcileAutoscaling(ctx, policy)
	if err != nil || held.Recommendation != "scale-up" ||
		held.DesiredSlots != 3 || held.UnderTargetSince == nil {
		t.Fatalf("scale-down dwell status=%+v err=%v", held, err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE scheduler_autoscale_state
		SET under_target_since = clock_timestamp() - interval '2 hours'
		WHERE scope = 'phase-executor:global'
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE scheduler_capacity_pool_state
		SET under_target_since = clock_timestamp() - interval '2 hours'
		WHERE pool_id = $1
	`, poolID); err != nil {
		t.Fatal(err)
	}
	scaledDown, err := repo.ReconcileAutoscaling(ctx, policy)
	if err != nil || scaledDown.Recommendation != "hold" ||
		scaledDown.DesiredSlots != 1 {
		t.Fatalf("scale-down status=%+v err=%v", scaledDown, err)
	}
	runtime, err := repo.RuntimeStatus(ctx)
	if err != nil || !runtime.Fairness.Enabled ||
		runtime.Fairness.AdmissionDispatches < 9 ||
		runtime.WorkerScoring.DecisionsLastHour < 29 ||
		runtime.WorkerScoring.MultiCandidateLastHour < 1 ||
		len(runtime.WorkerScoring.Recent) == 0 ||
		len(runtime.TargetHistory.Targets) < 2 ||
		len(runtime.TargetHistory.Targets[0].Windows) != 3 ||
		runtime.TargetHistory.Targets[0].Windows[2].Samples < 1 ||
		runtime.Autoscaler.DesiredSlots != 1 ||
		len(runtime.Autoscaler.Pools) != 1 ||
		runtime.Autoscaler.Pools[0].DesiredSlots != 0 ||
		runtime.Autoscaler.Pools[0].Recommendation != "scale-down" {
		t.Fatalf("scheduler runtime=%+v err=%v", runtime, err)
	}
	var scoringRows int
	var scoringEvidenceComplete bool
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*),
		       bool_and(
		         candidate_count > 0
		         AND pressure_score BETWEEN 0 AND 1000
		         AND recent_failures >= 0
		         AND reason <> ''
		       )
		FROM scheduler_worker_decisions
	`).Scan(&scoringRows, &scoringEvidenceComplete); err != nil ||
		scoringRows < 29 || !scoringEvidenceComplete {
		t.Fatalf(
			"worker scoring rows=%d complete=%v err=%v",
			scoringRows, scoringEvidenceComplete, err,
		)
	}
	testCapacityActuatorFences(
		t, ctx, db, repo, poolID, workerID,
	)
	testCapacityActionInstanceBindings(
		t, ctx, db, repo, policy, poolID,
	)
	testStaleWorkerPruningRemovesScoringEvidence(t, ctx, db, repo)
}

func testStaleWorkerPruningRemovesScoringEvidence(
	t *testing.T,
	ctx context.Context,
	db *persistence.Database,
	repo *persistence.JobRepository,
) {
	t.Helper()
	var workerID uuid.UUID
	if err := db.Pool().QueryRow(ctx, `
		SELECT d.worker_id
		FROM scheduler_worker_decisions d
		WHERE NOT EXISTS (
		  SELECT 1 FROM worker_leases l WHERE l.worker_id = d.worker_id
		)
		LIMIT 1
	`).Scan(&workerID); err != nil {
		t.Fatalf("select stale-prune scoring fixture: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE workers
		SET last_seen_at = clock_timestamp() - interval '2 hours'
		WHERE id = $1
	`, workerID); err != nil {
		t.Fatal(err)
	}
	pruned, err := repo.PruneStaleWorkers(ctx, time.Now().Add(-time.Hour))
	if err != nil || pruned < 1 {
		t.Fatalf("prune worker with scoring evidence: pruned=%d err=%v", pruned, err)
	}
	var workers, decisions int
	if err := db.Pool().QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM workers WHERE id = $1),
		  (SELECT count(*) FROM scheduler_worker_decisions WHERE worker_id = $1)
	`, workerID).Scan(&workers, &decisions); err != nil || workers != 0 || decisions != 0 {
		t.Fatalf("stale worker residue workers=%d decisions=%d err=%v",
			workers, decisions, err)
	}
}

// testCapacityActionInstanceBindings covers the reconcile pass that arrives
// while an action is in retry backoff with a provider instance already bound
// to it. Cancelling there used to leak a billed VM that no fenced write could
// reach again.
func testCapacityActionInstanceBindings(
	t *testing.T,
	ctx context.Context,
	db *persistence.Database,
	repo *persistence.JobRepository,
	policy builder.SchedulerAutoscalePolicy,
	poolID string,
) {
	t.Helper()
	requested, err := repo.RequestCapacityAction(
		ctx, poolID, "scale-up", 1, 0, "bound instance scale-up",
	)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := repo.ClaimCapacityAction(ctx, "actuator-c", time.Minute)
	if err != nil || claim == nil || claim.ID != requested.ID {
		t.Fatalf("bound instance claim=%+v err=%v", claim, err)
	}
	instance, err := repo.ReserveCapacityInstance(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RetryCapacityAction(
		ctx, claim, "provider provisioning timed out",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReconcileAutoscaling(ctx, policy); err != nil {
		t.Fatal(err)
	}
	var state, kind string
	if err := db.Pool().QueryRow(ctx, `
		SELECT state, action_kind
		FROM scheduler_capacity_actions
		WHERE id = $1
	`, requested.ID).Scan(&state, &kind); err != nil ||
		state != "requested" || kind != "scale-up" {
		t.Fatalf(
			"bound capacity action state=%q kind=%q err=%v", state, kind, err,
		)
	}
	// Any action closed while its instance is still live must return to the
	// queue with its binding intact, or the instance is stranded forever. The
	// attempt count stands in for an action that has already failed six claims
	// against a provider that keeps timing out, which is the case where the
	// requeue delay decides whether the actuator backs off or spins.
	if _, err := db.Pool().Exec(ctx, `
		UPDATE scheduler_capacity_actions
		SET state = 'canceled', claim_owner = '',
		    claim_lease_expires_at = NULL, attempts = 6,
		    completed_at = clock_timestamp()
		WHERE id = $1
	`, requested.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReconcileAutoscaling(ctx, policy); err != nil {
		t.Fatal(err)
	}
	// The actuator's own diagnosis is the only record of why a live VM was
	// abandoned, and the requeue has to serve the same bounded backoff a retry
	// would have: 2^6 seconds at six attempts.
	var reopenDetail string
	var reopenBackoffSeconds float64
	if err := db.Pool().QueryRow(ctx, `
		SELECT failure_detail,
		       extract(
		         epoch FROM next_attempt_at - updated_at
		       )::double precision
		FROM scheduler_capacity_actions
		WHERE id = $1
	`, requested.ID).Scan(&reopenDetail, &reopenBackoffSeconds); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reopenDetail, "provider provisioning timed out") ||
		!strings.Contains(reopenDetail, "reopened") {
		t.Fatalf("reopened capacity action failure_detail=%q", reopenDetail)
	}
	if reopenBackoffSeconds < 63 || reopenBackoffSeconds > 65 {
		t.Fatalf(
			"reopened capacity action requeue delay=%.3fs, want the 64s "+
				"backoff six attempts earn", reopenBackoffSeconds,
		)
	}
	// Everything below is about the surviving instance binding, so the parked
	// action is released rather than waited out.
	if _, err := db.Pool().Exec(ctx, `
		UPDATE scheduler_capacity_actions
		SET next_attempt_at = clock_timestamp()
		WHERE id = $1
	`, requested.ID); err != nil {
		t.Fatal(err)
	}
	reopened, err := repo.ClaimCapacityAction(ctx, "actuator-d", time.Minute)
	if err != nil || reopened == nil || reopened.ID != requested.ID {
		t.Fatalf("reopened capacity claim=%+v err=%v", reopened, err)
	}
	resumed, err := repo.ReserveCapacityInstance(ctx, reopened)
	if err != nil || resumed.ID != instance.ID ||
		resumed.OwnerToken != instance.OwnerToken {
		t.Fatalf(
			"resumed capacity instance=%+v original=%+v err=%v",
			resumed, instance, err,
		)
	}
}

func testCapacityActuatorFences(
	t *testing.T,
	ctx context.Context,
	db *persistence.Database,
	repo *persistence.JobRepository,
	poolID string,
	workerID uuid.UUID,
) {
	t.Helper()
	requested, err := repo.RequestCapacityAction(
		ctx, poolID, "scale-up", 1, 0, "integration scale-up",
	)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := repo.RequestCapacityAction(
		ctx, poolID, "scale-up", 1, 0, "duplicate",
	)
	if err != nil || duplicate.ID != requested.ID {
		t.Fatalf("duplicate capacity action=%+v err=%v", duplicate, err)
	}
	claim, err := repo.ClaimCapacityAction(
		ctx, "actuator-a", time.Minute,
	)
	if err != nil || claim == nil || claim.ID != requested.ID ||
		claim.Fence != 1 {
		t.Fatalf("capacity action claim=%+v err=%v", claim, err)
	}
	if other, err := repo.ClaimCapacityAction(
		ctx, "actuator-b", time.Minute,
	); err != nil || other != nil {
		t.Fatalf("duplicate actuator claim=%+v err=%v", other, err)
	}
	instance, err := repo.ReserveCapacityInstance(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	replayedInstance, err := repo.ReserveCapacityInstance(ctx, claim)
	if err != nil || replayedInstance.ID != instance.ID ||
		replayedInstance.OwnerToken != instance.OwnerToken {
		t.Fatalf(
			"capacity reservation replay=%+v original=%+v err=%v",
			replayedInstance, instance, err,
		)
	}
	staleClaim := *claim
	staleClaim.Fence++
	if err := repo.RecordCapacityInstanceProvisioned(
		ctx, &staleClaim, instance, "/shared/stale", nil,
	); err == nil {
		t.Fatal("stale capacity action recorded provider state")
	}
	if err := repo.RecordCapacityInstanceProvisioned(
		ctx, claim, instance, "/shared/capacity/"+instance.ID,
		map[string]string{"node": "test-node"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE workers
		SET stable_name = 'capacity-executor/phase-slot-0',
		    desired_state = 'active',
		    capabilities = jsonb_set(
		      capabilities, '{labels}',
		      (capabilities -> 'labels') || to_jsonb($2::text)
		    ),
		    last_seen_at = clock_timestamp()
		WHERE id = $1
	`, workerID, "capacity-instance:"+instance.ID); err != nil {
		t.Fatal(err)
	}
	confirmed, err := repo.ConfirmCapacityInstanceHeartbeat(
		ctx, claim, instance,
	)
	if err != nil || !confirmed {
		t.Fatalf("capacity heartbeat confirmed=%v err=%v", confirmed, err)
	}

	down, err := repo.RequestCapacityAction(
		ctx, poolID, "scale-down", 0, 1, "integration scale-down",
	)
	if err != nil {
		t.Fatal(err)
	}
	downClaim, err := repo.ClaimCapacityAction(
		ctx, "actuator-b", time.Minute,
	)
	if err != nil || downClaim == nil || downClaim.ID != down.ID {
		t.Fatalf("scale-down claim=%+v err=%v", downClaim, err)
	}
	draining, err := repo.SelectCapacityInstanceForDrain(ctx, downClaim)
	if err != nil || draining == nil || draining.ID != instance.ID {
		t.Fatalf("draining instance=%+v err=%v", draining, err)
	}
	replayedDrain, err := repo.SelectCapacityInstanceForDrain(ctx, downClaim)
	if err != nil || replayedDrain.ID != draining.ID ||
		replayedDrain.DeleteActionID != downClaim.ID {
		t.Fatalf(
			"capacity drain replay=%+v original=%+v err=%v",
			replayedDrain, draining, err,
		)
	}
	var desiredState string
	if err := db.Pool().QueryRow(ctx, `
		SELECT desired_state FROM workers WHERE id = $1
	`, workerID).Scan(&desiredState); err != nil || desiredState != "draining" {
		t.Fatalf("worker desired state=%q err=%v", desiredState, err)
	}

	var phaseWorkID string
	if err := db.Pool().QueryRow(ctx, `
		SELECT id::text FROM phase_work_items LIMIT 1
	`).Scan(&phaseWorkID); err != nil {
		t.Fatal(err)
	}
	var attemptID string
	var attemptFence int64
	if err := db.Pool().QueryRow(ctx, `
		SELECT work.attempt_id::text, attempt.fence_token
		FROM phase_work_items work
		JOIN build_attempts attempt ON attempt.id = work.attempt_id
		WHERE work.id = $1
	`, phaseWorkID).Scan(&attemptID, &attemptFence); err != nil {
		t.Fatal(err)
	}
	leaseID := uuid.New()
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO worker_leases (
		  id, worker_id, attempt_id, fence_token, expires_at
		) VALUES (
		  $1, $2, $3, $4, clock_timestamp() + interval '1 minute'
		)
	`, leaseID, workerID, attemptID, attemptFence); err != nil {
		t.Fatal(err)
	}
	drained, err := repo.CapacityInstanceDrained(
		ctx, downClaim, draining,
	)
	if err != nil || drained {
		t.Fatalf("live admission lease incorrectly drained=%v err=%v", drained, err)
	}
	if err := repo.BeginCapacityInstanceDelete(
		ctx, downClaim, draining,
	); err == nil {
		t.Fatal("live admission lease allowed capacity deletion")
	}
	if _, err := db.Pool().Exec(ctx, `
		DELETE FROM worker_leases WHERE id = $1
	`, leaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE phase_work_items
		SET execution_mode = 'active', state = 'claimed',
		    claim_owner = 'capacity-executor/phase-slot-0',
		    claim_fence = claim_fence + 1,
		    lease_expires_at = clock_timestamp() + interval '1 minute',
		    started_at = COALESCE(started_at, clock_timestamp()),
		    finished_at = NULL, updated_at = clock_timestamp()
		WHERE id = $1
	`, phaseWorkID); err != nil {
		t.Fatal(err)
	}
	drained, err = repo.CapacityInstanceDrained(
		ctx, downClaim, draining,
	)
	if err != nil || drained {
		t.Fatalf("live phase incorrectly drained=%v err=%v", drained, err)
	}
	if err := repo.BeginCapacityInstanceDelete(
		ctx, downClaim, draining,
	); err == nil {
		t.Fatal("live phase capacity instance entered deletion")
	}
	if _, err := db.Pool().Exec(ctx, `
		UPDATE phase_work_items
		SET state = 'ready', claim_owner = '', lease_expires_at = NULL,
		    updated_at = clock_timestamp()
		WHERE id = $1
	`, phaseWorkID); err != nil {
		t.Fatal(err)
	}
	staleGeneration := *draining
	staleGeneration.Generation++
	if err := repo.BeginCapacityInstanceDelete(
		ctx, downClaim, &staleGeneration,
	); err == nil {
		t.Fatal("stale capacity instance generation entered deletion")
	}
	drained, err = repo.CapacityInstanceDrained(
		ctx, downClaim, draining,
	)
	if err != nil || !drained {
		t.Fatalf("idle capacity instance drained=%v err=%v", drained, err)
	}
	if err := repo.BeginCapacityInstanceDelete(
		ctx, downClaim, draining,
	); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteCapacityInstanceDelete(
		ctx, downClaim, &staleGeneration,
	); err == nil {
		t.Fatal("stale capacity instance generation completed deletion")
	}
	if err := repo.CompleteCapacityInstanceDelete(
		ctx, downClaim, draining,
	); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteCapacityInstanceDelete(
		ctx, downClaim, draining,
	); err == nil {
		t.Fatal("replayed capacity deletion completion was accepted")
	}
	var actionState, instanceState string
	if err := db.Pool().QueryRow(ctx, `
		SELECT action.state, instance.state
		FROM scheduler_capacity_actions action
		JOIN scheduler_capacity_instances instance
		  ON instance.id = $2
		WHERE action.id = $1
	`, downClaim.ID, draining.ID).Scan(
		&actionState, &instanceState,
	); err != nil || actionState != "completed" || instanceState != "deleted" {
		t.Fatalf(
			"capacity terminal states action=%s instance=%s err=%v",
			actionState, instanceState, err,
		)
	}
}

func createFairnessJobs(
	t *testing.T,
	ctx context.Context,
	repo *persistence.JobRepository,
	projectID, prefix string,
	count int,
) []string {
	t.Helper()
	jobIDs := make([]string, 0, count)
	for index := 0; index < count; index++ {
		now := time.Now().UTC().Add(time.Duration(index) * time.Microsecond)
		request := &builder.BuildRequest{
			ProjectID:   projectID,
			PackageName: "app-misc/hello", Version: "2.12.1", Arch: "amd64",
			IdempotencyKey: fmt.Sprintf(
				"%s-%02d-%s", prefix, index, uuid.NewString(),
			),
		}
		status := &builder.BuildStatus{
			JobID: uuid.NewString(), ProjectID: projectID, Status: "queued",
			PackageName: request.PackageName, Version: request.Version,
			Arch: request.Arch, CreatedAt: now, UpdatedAt: now,
			Request: request,
		}
		if _, err := repo.CreateJob(ctx, request, status); err != nil {
			t.Fatalf("create %s fairness job %d: %v", prefix, index, err)
		}
		jobIDs = append(jobIDs, status.JobID)
	}
	return jobIDs
}

func failFairnessClaim(
	t *testing.T,
	ctx context.Context,
	repo *persistence.JobRepository,
	claim *builder.SchedulerClaim,
	reason string,
) {
	t.Helper()
	failed := *claim.Status
	failed.Status, failed.Error = "failed", reason
	failed.UpdatedAt = time.Now().UTC()
	if err := repo.RecordTransition(ctx, claim.Status, &failed); err != nil {
		t.Fatalf("finish fairness claim: %v", err)
	}
}

func cancelFairnessJobs(
	t *testing.T,
	ctx context.Context,
	repo *persistence.JobRepository,
	jobIDs []string,
) {
	t.Helper()
	for _, jobID := range jobIDs {
		statuses, err := repo.LoadVisible(ctx)
		if err != nil {
			t.Fatal(err)
		}
		status := statuses[jobID]
		if status == nil {
			continue
		}
		switch status.Status {
		case "completed", "success", "failed", "canceled":
			continue
		}
		if _, err := repo.CancelJob(
			ctx, jobID, "fairness integration cleanup",
		); err != nil {
			t.Fatalf("cancel fairness job %s: %v", jobID, err)
		}
	}
}
