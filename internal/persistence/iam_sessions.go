package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/slchris/portage-engine/internal/iam"
)

// IAMSessionPolicy is the server-side lifetime bound applied in addition to
// the issuer's exp claim. PostgreSQL time is authoritative for idle expiry.
type IAMSessionPolicy struct {
	IdleTimeout time.Duration
	MaxLifetime time.Duration
}

// IAMSession is a redacted token/session record. Raw bearer bytes are never
// written to PostgreSQL or returned by the API.
type IAMSession struct {
	ID                string     `json:"id"`
	SubjectID         string     `json:"subject_id"`
	ProviderSessionID string     `json:"provider_session_id,omitempty"`
	ProviderTokenID   string     `json:"provider_token_id,omitempty"`
	IssuedAt          time.Time  `json:"issued_at"`
	AuthenticatedAt   *time.Time `json:"authenticated_at,omitempty"`
	ExpiresAt         time.Time  `json:"expires_at"`
	LastSeenAt        time.Time  `json:"last_seen_at"`
	ACR               string     `json:"acr,omitempty"`
	AMR               []string   `json:"amr,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	RevokeReason      string     `json:"revoke_reason,omitempty"`
}

// AuthorizeSession registers or refreshes one verified OIDC token and rejects
// revoked, idle-expired, over-age, or pre-watermark credentials.
func (r *IAMRepository) AuthorizeSession(
	ctx context.Context,
	principal iam.Principal,
	identity iam.ExternalIdentity,
	policy IAMSessionPolicy,
) (iam.Principal, error) {
	if principal.SubjectID == "" || identity.TokenHash == "" ||
		identity.IssuedAt.IsZero() || identity.ExpiresAt.IsZero() {
		return iam.Principal{}, fmt.Errorf("OIDC session identity is incomplete")
	}
	if policy.IdleTimeout <= 0 || policy.MaxLifetime <= 0 {
		return iam.Principal{}, fmt.Errorf("OIDC session policy is invalid")
	}
	if identity.ExpiresAt.Sub(identity.IssuedAt) > policy.MaxLifetime {
		return iam.Principal{}, fmt.Errorf("OIDC token lifetime exceeds server policy")
	}
	var session IAMSession
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO iam_subject_security (subject_id)
			VALUES ($1)
			ON CONFLICT (subject_id) DO NOTHING
		`, principal.SubjectID); err != nil {
			return err
		}
		var validAfter time.Time
		if err := q.QueryRow(ctx, `
			SELECT tokens_valid_after
			FROM iam_subject_security
			WHERE subject_id = $1
			FOR UPDATE
		`, principal.SubjectID).Scan(&validAfter); err != nil {
			return err
		}
		if !identity.IssuedAt.After(validAfter) {
			return fmt.Errorf("OIDC token predates the subject revocation watermark")
		}
		var authenticatedAt any
		if !identity.AuthenticatedAt.IsZero() {
			authenticatedAt = identity.AuthenticatedAt
		}
		return q.QueryRow(ctx, `
			INSERT INTO iam_sessions (
				id, subject_id, token_hash, provider_session_id,
				provider_token_id, issued_at, authenticated_at, expires_at,
				last_seen_at, acr, amr
			)
			SELECT
				$1, $2, $3, $4, $5, $6, $7, $8,
				clock_timestamp(), $9, $10
			WHERE $8 > clock_timestamp()
			  AND $6 > clock_timestamp() - make_interval(secs => $12)
			ON CONFLICT (token_hash) DO UPDATE
			SET last_seen_at = clock_timestamp()
			WHERE iam_sessions.subject_id = EXCLUDED.subject_id
			  AND iam_sessions.issued_at = EXCLUDED.issued_at
			  AND iam_sessions.expires_at = EXCLUDED.expires_at
			  AND iam_sessions.revoked_at IS NULL
			  AND iam_sessions.expires_at > clock_timestamp()
			  AND iam_sessions.last_seen_at >
			      clock_timestamp() - make_interval(secs => $11)
			  AND iam_sessions.issued_at >
			      clock_timestamp() - make_interval(secs => $12)
			RETURNING id::text, subject_id::text, provider_session_id,
			          provider_token_id, issued_at, authenticated_at,
			          expires_at, last_seen_at, acr, amr, revoked_at,
			          revoke_reason
		`, uuid.New(), principal.SubjectID, identity.TokenHash,
			identity.ProviderSessionID, identity.ProviderTokenID,
			identity.IssuedAt, authenticatedAt, identity.ExpiresAt,
			identity.ACR, identity.AMR,
			int64(policy.IdleTimeout/time.Second),
			int64(policy.MaxLifetime/time.Second),
		).Scan(
			&session.ID, &session.SubjectID, &session.ProviderSessionID,
			&session.ProviderTokenID, &session.IssuedAt,
			&session.AuthenticatedAt, &session.ExpiresAt,
			&session.LastSeenAt, &session.ACR, &session.AMR,
			&session.RevokedAt, &session.RevokeReason,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return iam.Principal{}, fmt.Errorf("OIDC session is revoked or outside lifecycle policy")
	}
	if err != nil {
		return iam.Principal{}, fmt.Errorf("authorize OIDC session: %w", err)
	}
	principal.SessionID = session.ID
	principal.TokenIssuedAt = session.IssuedAt
	principal.TokenExpiresAt = session.ExpiresAt
	if session.AuthenticatedAt != nil {
		principal.AuthenticatedAt = *session.AuthenticatedAt
	}
	principal.ACR = session.ACR
	principal.AMR = append([]string(nil), session.AMR...)
	return principal, nil
}

// AuthorizePlatformSession validates a Portage Engine opaque session by hash.
// The raw token is never stored or returned by PostgreSQL.
func (r *IAMRepository) AuthorizePlatformSession(
	ctx context.Context,
	raw string,
	policy IAMSessionPolicy,
) (iam.Principal, error) {
	if raw == "" || policy.IdleTimeout <= 0 || policy.MaxLifetime <= 0 {
		return iam.Principal{}, fmt.Errorf("platform session is invalid")
	}
	digest := sha256.Sum256([]byte(raw))
	tokenHash := hex.EncodeToString(digest[:])
	var principal iam.Principal
	var authenticatedAt *time.Time
	err := r.db.pool.QueryRow(ctx, `
		UPDATE iam_sessions AS sess
		SET last_seen_at = clock_timestamp()
		FROM iam_subjects AS subject, iam_subject_security AS security
		WHERE sess.token_hash = $1
		  AND sess.subject_id = subject.id
		  AND security.subject_id = subject.id
		  AND sess.revoked_at IS NULL
		  AND sess.expires_at > clock_timestamp()
		  AND sess.last_seen_at >
		      clock_timestamp() - make_interval(secs => $2)
		  AND sess.issued_at >
		      clock_timestamp() - make_interval(secs => $3)
		  AND sess.issued_at > security.tokens_valid_after
		RETURNING subject.id::text, subject.issuer, subject.subject,
		          subject.preferred_username, subject.display_name,
		          subject.email, sess.id::text, sess.issued_at,
		          sess.expires_at, sess.authenticated_at, sess.acr, sess.amr
	`, tokenHash, int64(policy.IdleTimeout/time.Second),
		int64(policy.MaxLifetime/time.Second)).Scan(
		&principal.SubjectID, &principal.Issuer, &principal.Subject,
		&principal.PreferredUsername, &principal.DisplayName, &principal.Email,
		&principal.SessionID, &principal.TokenIssuedAt,
		&principal.TokenExpiresAt, &authenticatedAt, &principal.ACR,
		&principal.AMR,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return iam.Principal{}, fmt.Errorf("platform session is expired or revoked")
	}
	if err != nil {
		return iam.Principal{}, fmt.Errorf("authorize platform session: %w", err)
	}
	if authenticatedAt != nil {
		principal.AuthenticatedAt = *authenticatedAt
	}
	principal.Authentication = "federated-session"
	return principal, nil
}

func (r *IAMRepository) ListSessions(
	ctx context.Context,
	subjectID string,
) ([]IAMSession, error) {
	rows, err := r.db.pool.Query(ctx, `
		SELECT id::text, subject_id::text, provider_session_id,
		       provider_token_id, issued_at, authenticated_at, expires_at,
		       last_seen_at, acr, amr, revoked_at, revoke_reason
		FROM iam_sessions
		WHERE subject_id = $1
		ORDER BY last_seen_at DESC, id
		LIMIT 200
	`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("list IAM sessions: %w", err)
	}
	defer rows.Close()
	result := make([]IAMSession, 0)
	for rows.Next() {
		var session IAMSession
		if err := rows.Scan(
			&session.ID, &session.SubjectID, &session.ProviderSessionID,
			&session.ProviderTokenID, &session.IssuedAt,
			&session.AuthenticatedAt, &session.ExpiresAt,
			&session.LastSeenAt, &session.ACR, &session.AMR,
			&session.RevokedAt, &session.RevokeReason,
		); err != nil {
			return nil, err
		}
		result = append(result, session)
	}
	return result, rows.Err()
}

func (r *IAMRepository) RevokeSession(
	ctx context.Context,
	requester iam.Principal,
	sessionID, reason string,
	allowOtherSubjects bool,
) (IAMSession, error) {
	var session IAMSession
	err := r.db.pool.QueryRow(ctx, `
		UPDATE iam_sessions
		SET revoked_at = COALESCE(revoked_at, clock_timestamp()),
		    revoked_by_subject_id = CASE
		      WHEN revoked_at IS NULL THEN NULLIF($2, '')::uuid
		      ELSE revoked_by_subject_id END,
		    revoke_reason = CASE
		      WHEN revoked_at IS NULL THEN $3 ELSE revoke_reason END
		WHERE id = $1
		  AND ($4::boolean OR subject_id = NULLIF($2, '')::uuid)
		RETURNING id::text, subject_id::text, provider_session_id,
		          provider_token_id, issued_at, authenticated_at, expires_at,
		          last_seen_at, acr, amr, revoked_at, revoke_reason
	`, sessionID, requester.SubjectID, reason, allowOtherSubjects).Scan(
		&session.ID, &session.SubjectID, &session.ProviderSessionID,
		&session.ProviderTokenID, &session.IssuedAt,
		&session.AuthenticatedAt, &session.ExpiresAt,
		&session.LastSeenAt, &session.ACR, &session.AMR,
		&session.RevokedAt, &session.RevokeReason,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return IAMSession{}, fmt.Errorf("session is unknown or not authorized")
	}
	if err != nil {
		return IAMSession{}, fmt.Errorf("revoke IAM session: %w", err)
	}
	return session, nil
}

// RevokeAllSessions advances a subject watermark and revokes every already
// observed token. The second-truncated watermark matches JWT iat precision.
func (r *IAMRepository) RevokeAllSessions(
	ctx context.Context,
	requester iam.Principal,
	subjectID, reason string,
	allowOtherSubjects bool,
) (int64, error) {
	if !allowOtherSubjects && subjectID != requester.SubjectID {
		return 0, fmt.Errorf("subject session revocation is not authorized")
	}
	var revoked int64
	err := r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO iam_subject_security (
				subject_id, tokens_valid_after, updated_at
			) VALUES (
				$1, date_trunc('second', clock_timestamp()), clock_timestamp()
			)
			ON CONFLICT (subject_id) DO UPDATE
			SET tokens_valid_after = GREATEST(
			      iam_subject_security.tokens_valid_after,
			      EXCLUDED.tokens_valid_after
			    ),
			    updated_at = clock_timestamp()
		`, subjectID)
		if err != nil {
			return err
		}
		tag, err := q.Exec(ctx, `
			UPDATE iam_sessions
			SET revoked_at = COALESCE(revoked_at, clock_timestamp()),
			    revoked_by_subject_id = CASE
			      WHEN revoked_at IS NULL THEN NULLIF($2, '')::uuid
			      ELSE revoked_by_subject_id END,
			    revoke_reason = CASE
			      WHEN revoked_at IS NULL THEN $3 ELSE revoke_reason END
			WHERE subject_id = $1 AND revoked_at IS NULL
		`, subjectID, requester.SubjectID, reason)
		revoked = tag.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("revoke all IAM sessions: %w", err)
	}
	return revoked, nil
}
