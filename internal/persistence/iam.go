package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/iam"
)

const DefaultProjectName = "default"

type IAMRepository struct {
	db *Database
}

type Project struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type ProjectMember struct {
	ProjectID         string `json:"project_id"`
	SubjectID         string `json:"subject_id"`
	Issuer            string `json:"issuer"`
	Subject           string `json:"subject"`
	PreferredUsername string `json:"preferred_username,omitempty"`
	DisplayName       string `json:"display_name,omitempty"`
	Email             string `json:"email,omitempty"`
	Role              string `json:"role"`
}

// ProjectPolicy is the durable admission policy plus a point-in-time usage
// view. Usage is derived from the job authority; Redis is never consulted.
type ProjectPolicy struct {
	ProjectID                   string     `json:"project_id"`
	Suspended                   bool       `json:"suspended"`
	PriorityWeight              int        `json:"priority_weight"`
	StarvationThresholdSeconds  int        `json:"starvation_threshold_seconds"`
	MaxQueuedJobs               int        `json:"max_queued_jobs"`
	MaxActiveJobs               int        `json:"max_active_jobs"`
	MaxDailySubmissions         int        `json:"max_daily_submissions"`
	MaxActiveVCPUs              int        `json:"max_active_vcpus"`
	MaxActiveMemoryMiB          int        `json:"max_active_memory_mib"`
	MaxActiveDiskGiB            int        `json:"max_active_disk_gib"`
	MaxArtifactBytesPerJob      int64      `json:"max_artifact_bytes_per_job"`
	MaxDailyBuildSeconds        int64      `json:"max_daily_build_seconds"`
	MaxDailyCloudCostMicrounits int64      `json:"max_daily_cloud_cost_microunits"`
	MaxFailuresPerHour          int        `json:"max_failures_per_hour"`
	AbuseCooldownSeconds        int        `json:"abuse_cooldown_seconds"`
	AbuseSuspended              bool       `json:"abuse_suspended"`
	AbuseSuspendedUntil         *time.Time `json:"abuse_suspended_until,omitempty"`
	AbuseReason                 string     `json:"abuse_reason,omitempty"`
	AbuseGeneration             int64      `json:"abuse_generation"`
	MaxClaimedAttempts          int        `json:"max_claimed_attempts"`
	MaxProvisionAttempts        int        `json:"max_provision_attempts"`
	MaxBuildAttempts            int        `json:"max_build_attempts"`
	MaxVerifyAttempts           int        `json:"max_verify_attempts"`
	MaxPublishAttempts          int        `json:"max_publish_attempts"`
	QueuedJobs                  int        `json:"queued_jobs"`
	ActiveJobs                  int        `json:"active_jobs"`
	SubmissionsToday            int        `json:"submissions_today"`
	ReservedVCPUs               int        `json:"reserved_vcpus"`
	ReservedMemoryMiB           int        `json:"reserved_memory_mib"`
	ReservedDiskGiB             int        `json:"reserved_disk_gib"`
	QuarantineBytes             int64      `json:"quarantine_bytes"`
	ActiveArtifactBudgets       int        `json:"active_artifact_budgets"`
	BuildSecondsToday           int64      `json:"build_seconds_today"`
	CloudCostMicrounitsToday    int64      `json:"cloud_cost_microunits_today"`
	ActiveRuntimeBudgets        int        `json:"active_runtime_budgets"`
	FailuresLastHour            int        `json:"failures_last_hour"`
	ClaimedReservations         int        `json:"claimed_reservations"`
	ProvisionReservations       int        `json:"provision_reservations"`
	BuildReservations           int        `json:"build_reservations"`
	VerifyReservations          int        `json:"verify_reservations"`
	PublishReservations         int        `json:"publish_reservations"`
	WaitingReservations         int        `json:"waiting_reservations"`
	PhaseWorkShadow             int        `json:"phase_work_shadow"`
	PhaseWorkActive             int        `json:"phase_work_active"`
	PhaseWorkBlocked            int        `json:"phase_work_blocked"`
	PhaseWorkReady              int        `json:"phase_work_ready"`
	PhaseWorkUnschedulable      int        `json:"phase_work_unschedulable"`
	PhaseWorkClaimed            int        `json:"phase_work_claimed"`
	PhaseWorkFailed             int        `json:"phase_work_failed"`
	SubmissionDayStartsAt       time.Time  `json:"submission_day_starts_at"`
	SubmissionDayEndsAt         time.Time  `json:"submission_day_ends_at"`
	Version                     int64      `json:"version"`
	UpdatedBy                   string     `json:"updated_by"`
	UpdatedAt                   time.Time  `json:"updated_at"`
}

// ProjectPolicyUpdate carries all mutable limits and an optimistic version.
// Requiring the version prevents two owners from silently overwriting policy.
type ProjectPolicyUpdate struct {
	Suspended                   bool  `json:"suspended"`
	PriorityWeight              int   `json:"priority_weight"`
	StarvationThresholdSeconds  int   `json:"starvation_threshold_seconds"`
	MaxQueuedJobs               int   `json:"max_queued_jobs"`
	MaxActiveJobs               int   `json:"max_active_jobs"`
	MaxDailySubmissions         int   `json:"max_daily_submissions"`
	MaxActiveVCPUs              int   `json:"max_active_vcpus"`
	MaxActiveMemoryMiB          int   `json:"max_active_memory_mib"`
	MaxActiveDiskGiB            int   `json:"max_active_disk_gib"`
	MaxArtifactBytesPerJob      int64 `json:"max_artifact_bytes_per_job"`
	MaxDailyBuildSeconds        int64 `json:"max_daily_build_seconds"`
	MaxDailyCloudCostMicrounits int64 `json:"max_daily_cloud_cost_microunits"`
	MaxFailuresPerHour          int   `json:"max_failures_per_hour"`
	AbuseCooldownSeconds        int   `json:"abuse_cooldown_seconds"`
	ClearAbuseSuspension        bool  `json:"clear_abuse_suspension,omitempty"`
	MaxClaimedAttempts          int   `json:"max_claimed_attempts"`
	MaxProvisionAttempts        int   `json:"max_provision_attempts"`
	MaxBuildAttempts            int   `json:"max_build_attempts"`
	MaxVerifyAttempts           int   `json:"max_verify_attempts"`
	MaxPublishAttempts          int   `json:"max_publish_attempts"`
	Version                     int64 `json:"version"`
}

type AuditRecord struct {
	Principal    iam.Principal
	Action       string
	ResourceType string
	ResourceID   string
	ProjectID    string
	RequestID    string
	SourceIP     string
	Outcome      string
	Detail       map[string]any
}

func NewIAMRepository(db *Database) *IAMRepository {
	return &IAMRepository{db: db}
}

// ObserveSubject upserts profile claims only after the token has been
// cryptographically verified. Issuer+subject is the stable identity; mutable
// username/email fields never participate in authorization.
func (r *IAMRepository) ObserveSubject(
	ctx context.Context,
	identity iam.ExternalIdentity,
	systemAdmin bool,
) (iam.Principal, error) {
	var id uuid.UUID
	err := r.db.pool.QueryRow(ctx, `
		INSERT INTO iam_subjects (
			id, issuer, subject, preferred_username, display_name, email,
			last_seen_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, clock_timestamp(), clock_timestamp(), clock_timestamp())
		ON CONFLICT (issuer, subject)
		DO UPDATE SET
			preferred_username = EXCLUDED.preferred_username,
			display_name = EXCLUDED.display_name,
			email = EXCLUDED.email,
			last_seen_at = clock_timestamp(),
			updated_at = clock_timestamp()
		RETURNING id
	`, uuid.New(), identity.Issuer, identity.Subject, identity.PreferredUsername,
		identity.DisplayName, identity.Email).Scan(&id)
	if err != nil {
		return iam.Principal{}, fmt.Errorf("observe IAM subject: %w", err)
	}
	return iam.Principal{
		SubjectID: id.String(), ProviderID: identity.ProviderID,
		Issuer: identity.Issuer, Subject: identity.Subject,
		PreferredUsername: identity.PreferredUsername, DisplayName: identity.DisplayName,
		Email: identity.Email, Authentication: "oidc", SystemAdmin: systemAdmin,
	}, nil
}

func (r *IAMRepository) ListProjects(
	ctx context.Context,
	principal iam.Principal,
) ([]iam.ProjectAccess, error) {
	query := `
		SELECT p.id::text, p.name, pm.role
		FROM project_memberships pm
		JOIN projects p ON p.id = pm.project_id
		WHERE pm.subject_id = $1
		ORDER BY p.name, p.id
	`
	args := []any{principal.SubjectID}
	if principal.SystemAdmin {
		query = `
			SELECT p.id::text, p.name, 'owner'
			FROM projects p
			ORDER BY p.name, p.id
		`
		args = nil
	}
	rows, err := r.db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list authorized projects: %w", err)
	}
	defer rows.Close()
	var result []iam.ProjectAccess
	for rows.Next() {
		var access iam.ProjectAccess
		if err := rows.Scan(&access.ProjectID, &access.ProjectName, &access.Role); err != nil {
			return nil, err
		}
		result = append(result, access)
	}
	return result, rows.Err()
}

// ResolveProject selects one exact project and enforces the required role.
// A non-admin may omit the selector only when exactly one membership exists;
// this avoids silently executing a mutation in the wrong tenant.
func (r *IAMRepository) ResolveProject(
	ctx context.Context,
	principal iam.Principal,
	selector, requiredRole string,
) (iam.ProjectAccess, error) {
	projects, err := r.ListProjects(ctx, principal)
	if err != nil {
		return iam.ProjectAccess{}, err
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		switch {
		case principal.SystemAdmin:
			selector = DefaultProjectName
		case len(projects) == 1:
			if !iam.RoleAtLeast(projects[0].Role, requiredRole) {
				return iam.ProjectAccess{}, fmt.Errorf("project role %s does not grant %s access", projects[0].Role, requiredRole)
			}
			return projects[0], nil
		default:
			return iam.ProjectAccess{}, fmt.Errorf("X-Project-ID is required when the identity has %d projects", len(projects))
		}
	}
	for _, project := range projects {
		if project.ProjectID != selector && project.ProjectName != selector {
			continue
		}
		if !principal.SystemAdmin && !iam.RoleAtLeast(project.Role, requiredRole) {
			return iam.ProjectAccess{}, fmt.Errorf("project role %s does not grant %s access", project.Role, requiredRole)
		}
		return project, nil
	}
	return iam.ProjectAccess{}, fmt.Errorf("project is unknown or not authorized")
}

func (r *IAMRepository) CreateProject(
	ctx context.Context,
	name, description string,
) (Project, error) {
	project := Project{ID: uuid.NewString(), Name: name, Description: description}
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if err := q.QueryRow(ctx, `
			INSERT INTO projects (id, name, description)
			VALUES ($1, $2, $3)
			RETURNING id::text, name, description
		`, project.ID, project.Name, project.Description).
			Scan(&project.ID, &project.Name, &project.Description); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO project_policies (project_id, updated_by)
			VALUES ($1, 'project.create')
		`, project.ID); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `
			INSERT INTO project_scheduler_fairness (
			  project_id, admission_vruntime, phase_vruntime
			)
			SELECT
			  $1,
			  COALESCE(max(admission_vruntime), 0),
			  COALESCE(max(phase_vruntime), 0)
			FROM project_scheduler_fairness
		`, project.ID)
		return err
	})
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return project, nil
}

// GetProjectPolicy returns the current durable limits and PostgreSQL usage.
func (r *IAMRepository) GetProjectPolicy(ctx context.Context, projectID string) (ProjectPolicy, error) {
	return readProjectPolicy(ctx, r.db.pool, projectID)
}

func readProjectPolicy(
	ctx context.Context,
	q Querier,
	projectID string,
) (ProjectPolicy, error) {
	var policy ProjectPolicy
	query := `
		WITH day_window AS (
			SELECT
				(date_trunc('day', clock_timestamp() AT TIME ZONE 'UTC')
					AT TIME ZONE 'UTC') AS starts_at
		)
		SELECT
			pp.project_id::text, pp.suspended,
			pp.priority_weight, pp.starvation_threshold_seconds,
			pp.max_queued_jobs, pp.max_active_jobs, pp.max_daily_submissions,
			pp.max_active_vcpus, pp.max_active_memory_mib, pp.max_active_disk_gib,
			pp.max_artifact_bytes_per_job,
			pp.max_daily_build_seconds, pp.max_daily_cloud_cost_microunits,
			pp.max_failures_per_hour, pp.abuse_cooldown_seconds,
			COALESCE(pp.abuse_suspended_until > clock_timestamp(), false),
			pp.abuse_suspended_until, pp.abuse_reason, pp.abuse_generation,
			pp.max_claimed_attempts, pp.max_provision_attempts,
			pp.max_build_attempts, pp.max_verify_attempts,
			pp.max_publish_attempts,
			count(j.id) FILTER (WHERE j.state = 'queued'),
			count(j.id) FILTER (WHERE j.state IN (
				'claimed', 'provisioning', 'forwarding', 'deploying', 'building',
				'collecting', 'verifying', 'signing', 'publishing'
			)),
			(SELECT count(*) FROM build_jobs daily
			 WHERE daily.project_id = pp.project_id
			   AND daily.created_at >= dw.starts_at
			   AND daily.created_at < dw.starts_at + interval '1 day'),
			COALESCE(bu.build_seconds, 0), COALESCE(bu.cloud_cost, 0),
			COALESCE(bu.active_budgets, 0), COALESCE(fu.failures, 0),
			COALESCE(ru.vcpus, 0), COALESCE(ru.memory_mib, 0),
			COALESCE(ru.disk_gib, 0),
			COALESCE(au.bytes, 0), COALESCE(au.budgets, 0),
			COALESCE(ru.claimed, 0), COALESCE(ru.provision, 0),
			COALESCE(ru.build, 0), COALESCE(ru.verify, 0),
			COALESCE(ru.publish, 0),
			COALESCE(ru.waiting, 0),
			COALESCE(wu.shadow, 0), COALESCE(wu.active, 0),
			COALESCE(wu.blocked, 0), COALESCE(wu.ready, 0),
			COALESCE(wu.unschedulable, 0),
			COALESCE(wu.claimed, 0), COALESCE(wu.failed, 0),
			dw.starts_at, dw.starts_at + interval '1 day',
			pp.version, pp.updated_by, pp.updated_at
		FROM project_policies pp
		CROSS JOIN day_window dw
		LEFT JOIN LATERAL (
			SELECT
				COALESCE(sum(
				  CASE WHEN state = 'active'
				       THEN reserved_build_seconds ELSE charged_build_seconds END
				), 0) AS build_seconds,
				COALESCE(sum(
				  CASE WHEN state = 'active'
				       THEN reserved_cloud_cost_microunits
				       ELSE charged_cloud_cost_microunits END
				), 0) AS cloud_cost,
				count(*) FILTER (WHERE state = 'active')::integer AS active_budgets
			FROM project_attempt_usage
			WHERE project_id = pp.project_id
			  AND budget_day = (dw.starts_at AT TIME ZONE 'UTC')::date
		) bu ON true
		LEFT JOIN LATERAL (
			SELECT count(*)::integer AS failures
			FROM build_attempts a
			JOIN build_jobs failed_job ON failed_job.id = a.job_id
			WHERE failed_job.project_id = pp.project_id
			  AND a.state IN ('failed', 'expired')
			  AND a.finished_at >= clock_timestamp() - interval '1 hour'
		) fu ON true
		LEFT JOIN LATERAL (
			SELECT
				COALESCE(sum(vcpus), 0) AS vcpus,
				COALESCE(sum(memory_mib), 0) AS memory_mib,
				COALESCE(sum(disk_gib), 0) AS disk_gib,
				count(*) FILTER (WHERE phase = 'claimed')::integer AS claimed,
				count(*) FILTER (WHERE phase = 'provision')::integer AS provision,
				count(*) FILTER (WHERE phase = 'build')::integer AS build,
				count(*) FILTER (WHERE phase = 'verify')::integer AS verify,
				count(*) FILTER (WHERE phase = 'publish')::integer AS publish,
				count(*) FILTER (WHERE phase = 'waiting')::integer AS waiting
			FROM project_resource_reservations
			WHERE project_id = pp.project_id AND state = 'active'
		) ru ON true
		LEFT JOIN LATERAL (
			SELECT
				count(*) FILTER (WHERE execution_mode = 'shadow')::integer AS shadow,
				count(*) FILTER (WHERE execution_mode = 'active')::integer AS active,
				count(*) FILTER (
					WHERE execution_mode = 'active' AND state = 'blocked'
				)::integer AS blocked,
				count(*) FILTER (
					WHERE execution_mode = 'active' AND state = 'ready'
				)::integer AS ready,
				count(*) FILTER (
					WHERE execution_mode = 'active' AND state = 'ready'
					  AND NOT EXISTS (
					    SELECT 1
					    FROM build_jobs capability_job
					    JOIN workers capability_worker
					      ON capability_worker.desired_state = 'active'
					     AND capability_worker.last_seen_at >
					         clock_timestamp() - interval '45 seconds'
					     AND capability_worker.executor_protocol >=
					         capability_job.minimum_executor_protocol
					    WHERE capability_job.id = phase_work_items.job_id
					      AND NOT EXISTS (
					        SELECT 1
					        FROM jsonb_array_elements_text(
					          phase_work_items.required_capabilities
					        ) AS required(label)
					        WHERE NOT (
					          COALESCE(
					            capability_worker.capabilities -> 'labels',
					            '[]'::jsonb
					          ) ? required.label
					        )
					      )
					  )
				)::integer AS unschedulable,
				count(*) FILTER (
					WHERE execution_mode = 'active' AND state = 'claimed'
				)::integer AS claimed,
				count(*) FILTER (
					WHERE execution_mode = 'active' AND state = 'failed'
				)::integer AS failed
			FROM phase_work_items
			WHERE project_id = pp.project_id
		) wu ON true
		LEFT JOIN LATERAL (
			SELECT
				COALESCE(sum(active_bytes), 0) AS bytes,
				count(*)::integer AS budgets
			FROM project_artifact_budgets
			WHERE project_id = pp.project_id AND state = 'active'
		) au ON true
		LEFT JOIN build_jobs j
			ON j.project_id = pp.project_id AND j.legacy_visible = true
		WHERE pp.project_id = $1
		GROUP BY pp.project_id, dw.starts_at,
		         ru.vcpus, ru.memory_mib, ru.disk_gib,
		         ru.claimed, ru.provision, ru.build, ru.verify, ru.publish,
		         ru.waiting, wu.shadow, wu.active,
		         wu.blocked, wu.ready, wu.unschedulable, wu.claimed, wu.failed,
		         au.bytes, au.budgets,
		         bu.build_seconds, bu.cloud_cost, bu.active_budgets,
		         fu.failures`
	err := q.QueryRow(ctx, query, projectID).Scan(
		&policy.ProjectID, &policy.Suspended,
		&policy.PriorityWeight, &policy.StarvationThresholdSeconds,
		&policy.MaxQueuedJobs, &policy.MaxActiveJobs, &policy.MaxDailySubmissions,
		&policy.MaxActiveVCPUs, &policy.MaxActiveMemoryMiB, &policy.MaxActiveDiskGiB,
		&policy.MaxArtifactBytesPerJob,
		&policy.MaxDailyBuildSeconds, &policy.MaxDailyCloudCostMicrounits,
		&policy.MaxFailuresPerHour, &policy.AbuseCooldownSeconds,
		&policy.AbuseSuspended, &policy.AbuseSuspendedUntil,
		&policy.AbuseReason, &policy.AbuseGeneration,
		&policy.MaxClaimedAttempts, &policy.MaxProvisionAttempts,
		&policy.MaxBuildAttempts, &policy.MaxVerifyAttempts,
		&policy.MaxPublishAttempts,
		&policy.QueuedJobs, &policy.ActiveJobs, &policy.SubmissionsToday,
		&policy.BuildSecondsToday, &policy.CloudCostMicrounitsToday,
		&policy.ActiveRuntimeBudgets, &policy.FailuresLastHour,
		&policy.ReservedVCPUs, &policy.ReservedMemoryMiB, &policy.ReservedDiskGiB,
		&policy.QuarantineBytes, &policy.ActiveArtifactBudgets,
		&policy.ClaimedReservations, &policy.ProvisionReservations,
		&policy.BuildReservations, &policy.VerifyReservations,
		&policy.PublishReservations,
		&policy.WaitingReservations, &policy.PhaseWorkShadow,
		&policy.PhaseWorkActive, &policy.PhaseWorkBlocked,
		&policy.PhaseWorkReady, &policy.PhaseWorkUnschedulable,
		&policy.PhaseWorkClaimed,
		&policy.PhaseWorkFailed,
		&policy.SubmissionDayStartsAt, &policy.SubmissionDayEndsAt,
		&policy.Version, &policy.UpdatedBy, &policy.UpdatedAt,
	)
	if err != nil {
		return ProjectPolicy{}, fmt.Errorf("read project policy: %w", err)
	}
	return policy, nil
}

// UpdateProjectPolicy performs an optimistic, serialized policy replacement.
func (r *IAMRepository) UpdateProjectPolicy(
	ctx context.Context,
	projectID string,
	update ProjectPolicyUpdate,
	updatedBy string,
) (ProjectPolicy, error) {
	var policy ProjectPolicy
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		tag, err := q.Exec(ctx, `
			UPDATE project_policies
			SET suspended = $2,
			    priority_weight = CASE WHEN $3::integer > 0
			      THEN $3::integer ELSE priority_weight END,
			    starvation_threshold_seconds = CASE WHEN $4::integer > 0
			      THEN $4::integer ELSE starvation_threshold_seconds END,
			    max_queued_jobs = $5,
			    max_active_jobs = $6,
			    max_daily_submissions = $7,
			    max_active_vcpus = $8,
			    max_active_memory_mib = $9,
			    max_active_disk_gib = $10,
			    max_artifact_bytes_per_job = CASE WHEN $11::bigint > 0 THEN $11::bigint
			                                      ELSE max_artifact_bytes_per_job END,
			    max_claimed_attempts = CASE WHEN $12::integer > 0 THEN $12::integer
			                                ELSE max_claimed_attempts END,
			    max_provision_attempts = CASE WHEN $13::integer > 0 THEN $13::integer
			                                  ELSE max_provision_attempts END,
			    max_build_attempts = CASE WHEN $14::integer > 0 THEN $14::integer
			                              ELSE max_build_attempts END,
			    max_verify_attempts = CASE WHEN $15::integer > 0 THEN $15::integer
			                               ELSE max_verify_attempts END,
			    max_publish_attempts = CASE WHEN $16::integer > 0 THEN $16::integer
			                                ELSE max_publish_attempts END,
			    max_daily_build_seconds = CASE WHEN $17::bigint > 0
			      THEN $17::bigint ELSE max_daily_build_seconds END,
			    max_daily_cloud_cost_microunits = CASE WHEN $18::bigint > 0
			      THEN $18::bigint ELSE max_daily_cloud_cost_microunits END,
			    max_failures_per_hour = CASE WHEN $19::integer > 0
			      THEN $19::integer ELSE max_failures_per_hour END,
			    abuse_cooldown_seconds = CASE WHEN $20::integer > 0
			      THEN $20::integer ELSE abuse_cooldown_seconds END,
			    abuse_suspended_until = CASE
			      WHEN $21::boolean THEN NULL ELSE abuse_suspended_until END,
			    abuse_reason = CASE
			      WHEN $21::boolean THEN '' ELSE abuse_reason END,
			    version = version + 1,
			    updated_by = $22,
			    updated_at = clock_timestamp()
			WHERE project_id = $1 AND version = $23
		`, projectID, update.Suspended,
			update.PriorityWeight, update.StarvationThresholdSeconds,
			update.MaxQueuedJobs, update.MaxActiveJobs,
			update.MaxDailySubmissions, update.MaxActiveVCPUs,
			update.MaxActiveMemoryMiB, update.MaxActiveDiskGiB,
			update.MaxArtifactBytesPerJob, update.MaxClaimedAttempts,
			update.MaxProvisionAttempts, update.MaxBuildAttempts,
			update.MaxVerifyAttempts, update.MaxPublishAttempts,
			update.MaxDailyBuildSeconds,
			update.MaxDailyCloudCostMicrounits,
			update.MaxFailuresPerHour, update.AbuseCooldownSeconds,
			update.ClearAbuseSuspension, updatedBy, update.Version)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("project policy version conflict")
		}
		// Read before commit while this transaction still owns the policy-row
		// lock. Returning a post-commit read could expose a later owner's
		// replacement as the result (and audit detail) of this update.
		var readErr error
		policy, readErr = readProjectPolicy(ctx, q, projectID)
		return readErr
	})
	if err != nil {
		return ProjectPolicy{}, fmt.Errorf("update project policy: %w", err)
	}
	return policy, nil
}

// PutMembership can pre-provision an exact issuer+subject before first login.
// Mutable display claims are populated when that subject later authenticates.
func (r *IAMRepository) PutMembership(
	ctx context.Context,
	projectID, issuer, subject, role, grantedBy string,
) (ProjectMember, error) {
	var result ProjectMember
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if _, err := q.Exec(ctx, `
			SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))
		`, projectID); err != nil {
			return err
		}
		var subjectID uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO iam_subjects (id, issuer, subject)
			VALUES ($1, $2, $3)
			ON CONFLICT (issuer, subject) DO UPDATE SET updated_at = iam_subjects.updated_at
			RETURNING id
		`, uuid.New(), issuer, subject).Scan(&subjectID); err != nil {
			return err
		}
		if role != iam.RoleOwner {
			var existingRole string
			err := q.QueryRow(ctx, `
				SELECT role
				FROM project_memberships
				WHERE project_id = $1 AND subject_id = $2
				FOR UPDATE
			`, projectID, subjectID).Scan(&existingRole)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if existingRole == iam.RoleOwner {
				var owners int
				if err := q.QueryRow(ctx, `
					SELECT count(*)
					FROM project_memberships
					WHERE project_id = $1 AND role = 'owner'
				`, projectID).Scan(&owners); err != nil {
					return err
				}
				if owners <= 1 {
					return fmt.Errorf("cannot demote the project's last owner")
				}
			}
		}
		var grantedByValue any
		if grantedBy != "" {
			grantedByValue = grantedBy
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO project_memberships (
				project_id, subject_id, role, granted_by_subject_id
			) VALUES ($1, $2, $3, $4)
			ON CONFLICT (project_id, subject_id)
			DO UPDATE SET role = EXCLUDED.role,
			              granted_by_subject_id = EXCLUDED.granted_by_subject_id,
			              updated_at = clock_timestamp()
		`, projectID, subjectID, role, grantedByValue); err != nil {
			return err
		}
		return q.QueryRow(ctx, `
			SELECT pm.project_id::text, s.id::text, s.issuer, s.subject,
			       s.preferred_username, s.display_name, s.email, pm.role
			FROM project_memberships pm
			JOIN iam_subjects s ON s.id = pm.subject_id
			WHERE pm.project_id = $1 AND pm.subject_id = $2
		`, projectID, subjectID).Scan(
			&result.ProjectID, &result.SubjectID, &result.Issuer, &result.Subject,
			&result.PreferredUsername, &result.DisplayName, &result.Email, &result.Role,
		)
	})
	if err != nil {
		return ProjectMember{}, fmt.Errorf("put project membership: %w", err)
	}
	return result, nil
}

func (r *IAMRepository) DeleteMembership(
	ctx context.Context,
	projectID, subjectID string,
) error {
	var deleted bool
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if _, err := q.Exec(ctx, `
			SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))
		`, projectID); err != nil {
			return err
		}
		var role string
		if err := q.QueryRow(ctx, `
			SELECT role
			FROM project_memberships
			WHERE project_id = $1 AND subject_id = $2
			FOR UPDATE
		`, projectID, subjectID).Scan(&role); err != nil {
			return err
		}
		if role == iam.RoleOwner {
			var owners int
			if err := q.QueryRow(ctx, `
				SELECT count(*)
				FROM project_memberships
				WHERE project_id = $1 AND role = 'owner'
			`, projectID).Scan(&owners); err != nil {
				return err
			}
			if owners <= 1 {
				return fmt.Errorf("cannot remove the project's last owner")
			}
		}
		tag, err := q.Exec(ctx, `
			DELETE FROM project_memberships
			WHERE project_id = $1 AND subject_id = $2
		`, projectID, subjectID)
		if err != nil {
			return err
		}
		deleted = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete project membership: %w", err)
	}
	if !deleted {
		return fmt.Errorf("project membership not found")
	}
	return nil
}

func (r *IAMRepository) ListMembers(
	ctx context.Context,
	projectID string,
) ([]ProjectMember, error) {
	rows, err := r.db.pool.Query(ctx, `
		SELECT pm.project_id::text, s.id::text, s.issuer, s.subject,
		       s.preferred_username, s.display_name, s.email, pm.role
		FROM project_memberships pm
		JOIN iam_subjects s ON s.id = pm.subject_id
		WHERE pm.project_id = $1
		ORDER BY s.issuer, s.subject
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project members: %w", err)
	}
	defer rows.Close()
	var result []ProjectMember
	for rows.Next() {
		var member ProjectMember
		if err := rows.Scan(
			&member.ProjectID, &member.SubjectID, &member.Issuer, &member.Subject,
			&member.PreferredUsername, &member.DisplayName, &member.Email, &member.Role,
		); err != nil {
			return nil, err
		}
		result = append(result, member)
	}
	return result, rows.Err()
}

func (r *IAMRepository) RecordAudit(ctx context.Context, record AuditRecord) error {
	detail, err := json.Marshal(record.Detail)
	if err != nil {
		return fmt.Errorf("marshal audit detail: %w", err)
	}
	var subjectID, projectID any
	if record.Principal.SubjectID != "" {
		subjectID = record.Principal.SubjectID
	}
	if record.ProjectID != "" {
		projectID = record.ProjectID
	}
	var sourceIP any
	if ip := net.ParseIP(strings.TrimSpace(record.SourceIP)); ip != nil {
		sourceIP = ip.String()
	}
	outcome := record.Outcome
	if outcome == "" {
		outcome = "success"
	}
	_, err = r.db.pool.Exec(ctx, `
		INSERT INTO audit_events (
			actor, actor_subject_id, action, resource_type, resource_id,
			project_id, request_id, source_ip, outcome, detail
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)
	`, auditActor(record.Principal), subjectID, record.Action, record.ResourceType,
		record.ResourceID, projectID, record.RequestID, sourceIP, outcome, string(detail))
	if err != nil {
		return fmt.Errorf("record IAM audit event: %w", err)
	}
	return nil
}

func auditActor(principal iam.Principal) string {
	if principal.Subject != "" {
		return principal.Issuer + "#" + principal.Subject
	}
	return principal.Authentication
}

func IsNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
