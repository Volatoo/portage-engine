package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/iam"
)

type BackchannelLogoutResult struct {
	Duplicate       bool  `json:"duplicate"`
	RevokedSessions int64 `json:"revoked_sessions"`
}

// ApplyBackchannelLogout records replay state and revokes only sessions in the
// exact issuer namespace. Raw logout JWT and jti values never reach storage.
func (r *IAMRepository) ApplyBackchannelLogout(
	ctx context.Context,
	logout iam.LogoutIdentity,
) (BackchannelLogoutResult, error) {
	if logout.Issuer == "" || logout.ProviderTokenID == "" ||
		(logout.Subject == "" && logout.ProviderSessionID == "") ||
		logout.IssuedAt.IsZero() || !logout.ExpiresAt.After(logout.IssuedAt) {
		return BackchannelLogoutResult{}, fmt.Errorf("logout identity is incomplete")
	}
	digest := sha256.Sum256([]byte(logout.Issuer + "\x00" + logout.ProviderTokenID))
	tokenHash := hex.EncodeToString(digest[:])
	var result BackchannelLogoutResult
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		eventID := uuid.New()
		err := q.QueryRow(ctx, `
			INSERT INTO iam_logout_events (
				id, issuer, token_id_hash, subject, provider_session_id,
				issued_at, expires_at, received_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, clock_timestamp())
			ON CONFLICT (token_id_hash) DO NOTHING
			RETURNING id
		`, eventID, logout.Issuer, tokenHash, logout.Subject,
			logout.ProviderSessionID, logout.IssuedAt, logout.ExpiresAt,
		).Scan(&eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			result.Duplicate = true
			return q.QueryRow(ctx, `
				SELECT revoked_sessions
				FROM iam_logout_events
				WHERE token_id_hash = $1
			`, tokenHash).Scan(&result.RevokedSessions)
		}
		if err != nil {
			return err
		}
		if logout.ProviderSessionID != "" {
			tag, updateErr := q.Exec(ctx, `
				UPDATE iam_sessions AS sess
				SET revoked_at = COALESCE(sess.revoked_at, clock_timestamp()),
				    revoke_reason = CASE WHEN sess.revoked_at IS NULL
				      THEN 'provider_backchannel_logout'
				      ELSE sess.revoke_reason END
				FROM iam_subjects AS subject
				WHERE sess.subject_id = subject.id
				  AND subject.issuer = $1
				  AND sess.provider_session_id = $2
				  AND ($3 = '' OR subject.subject = $3)
				  AND sess.revoked_at IS NULL
			`, logout.Issuer, logout.ProviderSessionID, logout.Subject)
			if updateErr != nil {
				return updateErr
			}
			result.RevokedSessions = tag.RowsAffected()
		} else {
			var subjectID string
			resolveErr := q.QueryRow(ctx, `
				SELECT id::text FROM iam_subjects
				WHERE issuer = $1 AND subject = $2
			`, logout.Issuer, logout.Subject).Scan(&subjectID)
			if resolveErr != nil && !errors.Is(resolveErr, pgx.ErrNoRows) {
				return resolveErr
			}
			if subjectID != "" {
				if _, err := q.Exec(ctx, `
					INSERT INTO iam_subject_security (
						subject_id, tokens_valid_after, updated_at
					) VALUES (
						$1, date_trunc('second', $2::timestamptz),
						clock_timestamp()
					)
					ON CONFLICT (subject_id) DO UPDATE
					SET tokens_valid_after = GREATEST(
					      iam_subject_security.tokens_valid_after,
					      EXCLUDED.tokens_valid_after
					    ),
					    updated_at = clock_timestamp()
				`, subjectID, logout.IssuedAt); err != nil {
					return err
				}
				tag, err := q.Exec(ctx, `
					UPDATE iam_sessions
					SET revoked_at = COALESCE(revoked_at, clock_timestamp()),
					    revoke_reason = CASE WHEN revoked_at IS NULL
					      THEN 'provider_backchannel_logout'
					      ELSE revoke_reason END
					WHERE subject_id = $1 AND revoked_at IS NULL
				`, subjectID)
				if err != nil {
					return err
				}
				result.RevokedSessions = tag.RowsAffected()
			}
		}
		_, err = q.Exec(ctx, `
			UPDATE iam_logout_events
			SET revoked_sessions = $2
			WHERE id = $1
		`, eventID, result.RevokedSessions)
		return err
	})
	if err != nil {
		return BackchannelLogoutResult{}, fmt.Errorf("apply back-channel logout: %w", err)
	}
	return result, nil
}
