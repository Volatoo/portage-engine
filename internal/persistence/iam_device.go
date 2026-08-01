package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/iam"
)

const (
	DeviceAuthorizationPending  = "pending"
	DeviceAuthorizationApproved = "approved"
	DeviceAuthorizationDenied   = "denied"
	DeviceAuthorizationConsumed = "consumed"
	DeviceAuthorizationExpired  = "expired"
	DeviceAuthorizationSlowDown = "slow_down"
)

var ErrDeviceAuthorizationInvalid = errors.New("device authorization is invalid or expired")

// DeviceAuthorization is the redacted durable state returned to handlers.
// Device-code bytes and access-token bytes never cross this persistence API.
type DeviceAuthorization struct {
	ID              string
	UserCode        string
	Status          string
	ExpiresAt       time.Time
	IntervalSeconds int
}

// DeviceAuthorizationPoll is one atomic polling result. Principal is set only
// when this transaction created and consumed a new platform session.
type DeviceAuthorizationPoll struct {
	Status          string
	IntervalSeconds int
	Principal       iam.Principal
}

// CreateDeviceAuthorization records only the digest of the device capability.
// A false result is a collision and the caller must generate fresh codes.
func (r *IAMRepository) CreateDeviceAuthorization(
	ctx context.Context,
	deviceCodeHash, userCode string,
	ttlSeconds int,
	intervalSeconds int,
) (bool, error) {
	if len(deviceCodeHash) != 64 || userCode == "" || intervalSeconds < 1 ||
		intervalSeconds > 60 || ttlSeconds < 1 || ttlSeconds > 3600 {
		return false, fmt.Errorf("device authorization is incomplete")
	}
	created := false
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		// Rows are useful only for a short replay/audit window. Bounded cleanup
		// keeps unauthenticated starts from growing this table without limit.
		if _, err := q.Exec(ctx, `
			DELETE FROM iam_device_authorizations
			WHERE expires_at < clock_timestamp() - interval '1 day'
		`); err != nil {
			return err
		}
		var id string
		err := q.QueryRow(ctx, `
			INSERT INTO iam_device_authorizations (
				id, device_code_hash, user_code, expires_at, interval_seconds
			)
			VALUES (
				$1, $2, $3,
				clock_timestamp() + make_interval(secs => $4), $5
			)
			ON CONFLICT DO NOTHING
			RETURNING id::text
		`, uuid.New(), deviceCodeHash, userCode, ttlSeconds, intervalSeconds).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("create device authorization: %w", err)
	}
	return created, nil
}

// DecideDeviceAuthorization binds a pending user code to the exact active
// platform session that approved it. The authenticated subject is never
// inferred from email, groups, or mutable profile claims.
func (r *IAMRepository) DecideDeviceAuthorization(
	ctx context.Context,
	userCode string,
	principal iam.Principal,
	approve bool,
	policy IAMSessionPolicy,
) (DeviceAuthorization, error) {
	if userCode == "" || principal.SubjectID == "" || principal.SessionID == "" ||
		policy.IdleTimeout <= 0 || policy.MaxLifetime <= 0 {
		return DeviceAuthorization{}, ErrDeviceAuthorizationInvalid
	}
	var result DeviceAuthorization
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if _, err := q.Exec(ctx, `
			UPDATE iam_device_authorizations
			SET status = 'expired'
			WHERE user_code = $1
			  AND status IN ('pending', 'approved')
			  AND expires_at <= clock_timestamp()
		`, userCode); err != nil {
			return err
		}
		if approve {
			return q.QueryRow(ctx, `
				UPDATE iam_device_authorizations AS device
				SET status = 'approved', subject_id = session.subject_id,
				    approver_session_id = session.id,
				    approved_at = clock_timestamp()
				FROM iam_sessions AS session, iam_subject_security AS security
				WHERE device.user_code = $1
				  AND device.status = 'pending'
				  AND device.expires_at > clock_timestamp()
				  AND session.id = $2
				  AND session.subject_id = $3
				  AND security.subject_id = session.subject_id
				  AND session.revoked_at IS NULL
				  AND session.expires_at > clock_timestamp()
				  AND session.last_seen_at >
				      clock_timestamp() - make_interval(secs => $4)
				  AND session.issued_at >
				      clock_timestamp() - make_interval(secs => $5)
				  AND session.issued_at > security.tokens_valid_after
				RETURNING device.id::text, device.user_code, device.status,
				          device.expires_at, device.interval_seconds
			`, userCode, principal.SessionID, principal.SubjectID,
				int64(policy.IdleTimeout/time.Second),
				int64(policy.MaxLifetime/time.Second)).Scan(
				&result.ID, &result.UserCode, &result.Status,
				&result.ExpiresAt, &result.IntervalSeconds,
			)
		}
		return q.QueryRow(ctx, `
			UPDATE iam_device_authorizations AS device
			SET status = 'denied', denied_at = clock_timestamp()
			FROM iam_sessions AS session, iam_subject_security AS security
			WHERE device.user_code = $1
			  AND device.status = 'pending'
			  AND device.expires_at > clock_timestamp()
			  AND session.id = $2
			  AND session.subject_id = $3
			  AND security.subject_id = session.subject_id
			  AND session.revoked_at IS NULL
			  AND session.expires_at > clock_timestamp()
			  AND session.last_seen_at >
			      clock_timestamp() - make_interval(secs => $4)
			  AND session.issued_at >
			      clock_timestamp() - make_interval(secs => $5)
			  AND session.issued_at > security.tokens_valid_after
			RETURNING device.id::text, device.user_code, device.status,
			          device.expires_at, device.interval_seconds
		`, userCode, principal.SessionID, principal.SubjectID,
			int64(policy.IdleTimeout/time.Second),
			int64(policy.MaxLifetime/time.Second)).Scan(
			&result.ID, &result.UserCode, &result.Status,
			&result.ExpiresAt, &result.IntervalSeconds,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceAuthorization{}, ErrDeviceAuthorizationInvalid
	}
	if err != nil {
		return DeviceAuthorization{}, fmt.Errorf("decide device authorization: %w", err)
	}
	return result, nil
}

// PollDeviceAuthorization serializes polling, durable slow-down and successful
// access-token creation under the device row lock. Exactly one transaction can
// turn an approved code into a platform session.
func (r *IAMRepository) PollDeviceAuthorization(
	ctx context.Context,
	deviceCodeHash, accessTokenHash string,
	policy IAMSessionPolicy,
) (DeviceAuthorizationPoll, error) {
	if len(deviceCodeHash) != 64 || len(accessTokenHash) != 64 ||
		policy.IdleTimeout <= 0 || policy.MaxLifetime <= 0 {
		return DeviceAuthorizationPoll{}, ErrDeviceAuthorizationInvalid
	}
	var result DeviceAuthorizationPoll
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		var id, status string
		var expired, tooFast bool
		if err := q.QueryRow(ctx, `
			SELECT id::text, status, interval_seconds,
			       expires_at <= clock_timestamp(),
			       last_polled_at IS NOT NULL AND
			         last_polled_at + make_interval(secs => interval_seconds) >
			           clock_timestamp()
			FROM iam_device_authorizations
			WHERE device_code_hash = $1
			FOR UPDATE
		`, deviceCodeHash).Scan(
			&id, &status, &result.IntervalSeconds, &expired, &tooFast,
		); err != nil {
			return err
		}
		if expired {
			if status == DeviceAuthorizationPending ||
				status == DeviceAuthorizationApproved {
				if _, err := q.Exec(ctx, `
					UPDATE iam_device_authorizations SET status = 'expired'
					WHERE id = $1
				`, id); err != nil {
					return err
				}
			}
			result.Status = DeviceAuthorizationExpired
			return nil
		}
		if status == DeviceAuthorizationDenied {
			result.Status = DeviceAuthorizationDenied
			return nil
		}
		if status == DeviceAuthorizationConsumed ||
			status == DeviceAuthorizationExpired {
			result.Status = DeviceAuthorizationExpired
			return nil
		}
		if tooFast {
			return q.QueryRow(ctx, `
				UPDATE iam_device_authorizations
				SET interval_seconds = LEAST(interval_seconds + 5, 60),
				    last_polled_at = clock_timestamp()
				WHERE id = $1
				RETURNING interval_seconds
			`, id).Scan(&result.IntervalSeconds)
		}
		if status == DeviceAuthorizationPending {
			if _, err := q.Exec(ctx, `
				UPDATE iam_device_authorizations
				SET last_polled_at = clock_timestamp()
				WHERE id = $1
			`, id); err != nil {
				return err
			}
			result.Status = DeviceAuthorizationPending
			return nil
		}
		if status != DeviceAuthorizationApproved {
			result.Status = DeviceAuthorizationExpired
			return nil
		}

		// Serialize issuance with the subject-wide revocation watermark. If a
		// revoke-all owns this row first, PostgreSQL rechecks the updated
		// watermark after this lock wait and issuance fails. If this poll owns it
		// first, revoke-all waits, then sees and revokes the newly inserted
		// session in its following statement.
		var lockedSubjectID string
		lockErr := q.QueryRow(ctx, `
			SELECT security.subject_id::text
			FROM iam_device_authorizations AS device
			JOIN iam_sessions AS source
			  ON source.id = device.approver_session_id
			 AND source.subject_id = device.subject_id
			JOIN iam_subject_security AS security
			  ON security.subject_id = source.subject_id
			WHERE device.id = $1
			  AND device.status = 'approved'
			  AND device.expires_at > clock_timestamp()
			  AND source.revoked_at IS NULL
			  AND source.expires_at > clock_timestamp()
			  AND source.last_seen_at >
			      clock_timestamp() - make_interval(secs => $2)
			  AND source.issued_at >
			      clock_timestamp() - make_interval(secs => $3)
			  AND source.issued_at > security.tokens_valid_after
			FOR UPDATE OF security
		`, id, int64(policy.IdleTimeout/time.Second),
			int64(policy.MaxLifetime/time.Second)).Scan(&lockedSubjectID)
		if errors.Is(lockErr, pgx.ErrNoRows) {
			if _, updateErr := q.Exec(ctx, `
				UPDATE iam_device_authorizations
				SET status = 'denied', denied_at = clock_timestamp()
				WHERE id = $1 AND status = 'approved'
			`, id); updateErr != nil {
				return updateErr
			}
			result.Status = DeviceAuthorizationDenied
			return nil
		}
		if lockErr != nil {
			return lockErr
		}

		newSessionID := uuid.New()
		var authenticatedAt *time.Time
		err := q.QueryRow(ctx, `
			WITH inserted AS (
				INSERT INTO iam_sessions (
					id, subject_id, token_hash, provider_session_id,
					provider_token_id, issued_at, authenticated_at,
					expires_at, last_seen_at, acr, amr
				)
				SELECT $2, source.subject_id, $3, source.provider_session_id,
				       source.provider_token_id, clock_timestamp(),
				       source.authenticated_at,
				       LEAST(
				         source.expires_at,
				         clock_timestamp() + make_interval(secs => $6)
				       ),
				       clock_timestamp(), source.acr, source.amr
				FROM iam_device_authorizations AS device
				JOIN iam_sessions AS source
				  ON source.id = device.approver_session_id
				 AND source.subject_id = device.subject_id
				JOIN iam_subject_security AS security
				  ON security.subject_id = source.subject_id
				WHERE device.id = $1
				  AND device.status = 'approved'
				  AND device.expires_at > clock_timestamp()
				  AND source.revoked_at IS NULL
				  AND source.expires_at > clock_timestamp()
				  AND source.last_seen_at >
				      clock_timestamp() - make_interval(secs => $4)
				  AND source.issued_at >
				      clock_timestamp() - make_interval(secs => $5)
				  AND source.issued_at > security.tokens_valid_after
				RETURNING id, subject_id, issued_at, authenticated_at,
				          expires_at, acr, amr
			)
			SELECT subject.id::text, subject.issuer, subject.subject,
			       subject.preferred_username, subject.display_name, subject.email,
			       inserted.id::text, inserted.issued_at,
			       inserted.authenticated_at, inserted.expires_at,
			       inserted.acr, inserted.amr
			FROM inserted
			JOIN iam_subjects AS subject ON subject.id = inserted.subject_id
		`, id, newSessionID, accessTokenHash,
			int64(policy.IdleTimeout/time.Second),
			int64(policy.MaxLifetime/time.Second),
			int64(policy.MaxLifetime/time.Second)).Scan(
			&result.Principal.SubjectID, &result.Principal.Issuer,
			&result.Principal.Subject, &result.Principal.PreferredUsername,
			&result.Principal.DisplayName, &result.Principal.Email,
			&result.Principal.SessionID, &result.Principal.TokenIssuedAt,
			&authenticatedAt, &result.Principal.TokenExpiresAt,
			&result.Principal.ACR, &result.Principal.AMR,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, updateErr := q.Exec(ctx, `
				UPDATE iam_device_authorizations
				SET status = 'denied', denied_at = clock_timestamp()
				WHERE id = $1 AND status = 'approved'
			`, id); updateErr != nil {
				return updateErr
			}
			result.Status = DeviceAuthorizationDenied
			return nil
		}
		if err != nil {
			return err
		}
		if authenticatedAt != nil {
			result.Principal.AuthenticatedAt = *authenticatedAt
		}
		result.Principal.Authentication = "federated-session"
		if _, err := q.Exec(ctx, `
			UPDATE iam_device_authorizations
			SET status = 'consumed', consumed_at = clock_timestamp(),
			    last_polled_at = clock_timestamp()
			WHERE id = $1 AND status = 'approved'
		`, id); err != nil {
			return err
		}
		result.Status = DeviceAuthorizationApproved
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceAuthorizationPoll{Status: DeviceAuthorizationExpired}, nil
	}
	if err != nil {
		return DeviceAuthorizationPoll{}, fmt.Errorf("poll device authorization: %w", err)
	}
	if result.Status == "" {
		result.Status = DeviceAuthorizationSlowDown
	}
	return result, nil
}
