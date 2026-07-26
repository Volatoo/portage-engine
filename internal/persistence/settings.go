package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// LoadRuntimeSetting decodes a versioned, non-secret control-plane setting.
// secret_refs contains provider/env references only, never credential values.
func (r *JobRepository) LoadRuntimeSetting(
	ctx context.Context,
	scope, key string,
	value any,
) (int64, map[string]string, bool, error) {
	var (
		version  int64
		rawValue []byte
		rawRefs  []byte
	)
	err := r.db.Pool().QueryRow(ctx, `
		SELECT version, value, secret_refs
		FROM runtime_settings
		WHERE scope = $1 AND settings_key = $2
	`, scope, key).Scan(&version, &rawValue, &rawRefs)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, fmt.Errorf("load runtime setting %s/%s: %w", scope, key, err)
	}
	if err := json.Unmarshal(rawValue, value); err != nil {
		return 0, nil, false, fmt.Errorf("decode runtime setting %s/%s: %w", scope, key, err)
	}
	refs := map[string]string{}
	if err := json.Unmarshal(rawRefs, &refs); err != nil {
		return 0, nil, false, fmt.Errorf("decode secret refs %s/%s: %w", scope, key, err)
	}
	return version, refs, true, nil
}

// SaveRuntimeSetting atomically increments the setting version and appends an
// audit event. Callers must remove all secret values before invoking it.
func (r *JobRepository) SaveRuntimeSetting(
	ctx context.Context,
	scope, key string,
	value any,
	secretRefs map[string]string,
	actor, requestID string,
) (int64, error) {
	rawValue, err := json.Marshal(value)
	if err != nil {
		return 0, fmt.Errorf("encode runtime setting %s/%s: %w", scope, key, err)
	}
	rawRefs, err := json.Marshal(secretRefs)
	if err != nil {
		return 0, fmt.Errorf("encode secret refs %s/%s: %w", scope, key, err)
	}
	if actor == "" {
		actor = "control-plane"
	}
	var version int64
	err = r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		if err := q.QueryRow(ctx, `
			INSERT INTO runtime_settings (
				scope, settings_key, version, value, secret_refs, updated_by, updated_at
			) VALUES ($1, $2, 1, $3::jsonb, $4::jsonb, $5, clock_timestamp())
			ON CONFLICT (scope, settings_key)
			DO UPDATE SET
				version = runtime_settings.version + 1,
				value = EXCLUDED.value,
				secret_refs = EXCLUDED.secret_refs,
				updated_by = EXCLUDED.updated_by,
				updated_at = clock_timestamp()
			RETURNING version
		`, scope, key, string(rawValue), string(rawRefs), actor).Scan(&version); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `
			INSERT INTO audit_events (
				actor, action, resource_type, resource_id, request_id, detail
			) VALUES (
				$1, 'runtime_setting.updated', 'runtime_setting', $2, $3,
				jsonb_build_object('scope', $4::text, 'version', $5::bigint, 'secret_refs', $6::jsonb)
			)
		`, actor, scope+"/"+key, requestID, scope, version, string(rawRefs))
		return err
	})
	r.recordWrite(err)
	if err != nil {
		return 0, fmt.Errorf("save runtime setting %s/%s: %w", scope, key, err)
	}
	return version, nil
}

// EnsureRuntimeSetting creates an initial setting exactly once. Concurrent
// replicas never overwrite the winner's bootstrap value.
func (r *JobRepository) EnsureRuntimeSetting(
	ctx context.Context,
	scope, key string,
	value any,
	secretRefs map[string]string,
	actor string,
) (bool, error) {
	rawValue, err := json.Marshal(value)
	if err != nil {
		return false, fmt.Errorf("encode initial runtime setting %s/%s: %w", scope, key, err)
	}
	rawRefs, err := json.Marshal(secretRefs)
	if err != nil {
		return false, fmt.Errorf("encode initial secret refs %s/%s: %w", scope, key, err)
	}
	if actor == "" {
		actor = "control-plane-bootstrap"
	}
	created := false
	err = r.db.WithTx(ctx, pgx.TxOptions{}, func(q Querier) error {
		var version int64
		err := q.QueryRow(ctx, `
			INSERT INTO runtime_settings (
				scope, settings_key, version, value, secret_refs, updated_by, updated_at
			) VALUES ($1, $2, 1, $3::jsonb, $4::jsonb, $5, clock_timestamp())
			ON CONFLICT (scope, settings_key) DO NOTHING
			RETURNING version
		`, scope, key, string(rawValue), string(rawRefs), actor).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		created = true
		_, err = q.Exec(ctx, `
			INSERT INTO audit_events (
				actor, action, resource_type, resource_id, detail
			) VALUES (
				$1, 'runtime_setting.created', 'runtime_setting', $2,
				jsonb_build_object('scope', $3::text, 'version', $4::bigint, 'secret_refs', $5::jsonb)
			)
		`, actor, scope+"/"+key, scope, version, string(rawRefs))
		return err
	})
	r.recordWrite(err)
	if err != nil {
		return false, fmt.Errorf("ensure runtime setting %s/%s: %w", scope, key, err)
	}
	return created, nil
}
