// Package persistence defines the PostgreSQL storage contract used by the
// control plane. DB-0 establishes connection, transaction, migration, and
// compatibility boundaries without making PostgreSQL the job source of truth.
package persistence

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/slchris/portage-engine/pkg/config"
)

const (
	// MinSchemaVersion and MaxSchemaVersion define the migration range this
	// server binary can safely use. A newer schema fails closed.
	MinSchemaVersion int64 = 7
	MaxSchemaVersion int64 = 7
)

// Querier is the narrow database surface repositories may use. Keeping
// pgxpool.Pool out of handlers makes transaction ownership explicit.
type Querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Database owns the process-local PostgreSQL connection pool.
type Database struct {
	pool          *pgxpool.Pool
	healthTimeout time.Duration
}

// Health is a point-in-time database and schema compatibility result.
type Health struct {
	OK            bool
	SchemaVersion int64
	Reason        string
}

// ConnectionString returns a correctly escaped PostgreSQL URL. DATABASE_URL
// wins when set; otherwise split PG* fields are encoded without interpolating
// credentials into an unescaped string.
func ConnectionString(cfg config.DatabaseConfig) (string, error) {
	if cfg.URL != "" {
		return cfg.URL, nil
	}
	if cfg.Host == "" || cfg.Port <= 0 || cfg.Port > 65535 || cfg.Name == "" || cfg.User == "" {
		return "", fmt.Errorf("incomplete PostgreSQL configuration")
	}
	mode := cfg.SSLMode
	if mode == "" {
		mode = "verify-full"
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.User, cfg.Password),
		Host:   net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Path:   "/" + cfg.Name,
	}
	q := u.Query()
	q.Set("sslmode", mode)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Open creates and verifies a PostgreSQL pool. It does not run migrations.
func Open(ctx context.Context, cfg config.DatabaseConfig) (*Database, error) {
	dsn, err := ConnectionString(cfg)
	if err != nil {
		return nil, err
	}
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL configuration: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = int32(cfg.MaxConns)
	}
	if cfg.MinConns >= 0 {
		poolCfg.MinConns = int32(cfg.MinConns)
	}
	poolCfg.ConnConfig.RuntimeParams["application_name"] = "portage-engine"

	connectTimeout := time.Duration(cfg.ConnectTimeoutSeconds) * time.Second
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(connectCtx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}

	healthTimeout := time.Duration(cfg.HealthTimeoutSeconds) * time.Second
	if healthTimeout <= 0 {
		healthTimeout = 2 * time.Second
	}
	return &Database{pool: pool, healthTimeout: healthTimeout}, nil
}

// Pool exposes the narrow Querier interface for repository construction.
func (d *Database) Pool() Querier {
	return d.pool
}

// WithTx runs fn in a transaction, rolling back on error or panic and
// committing only after fn succeeds.
func (d *Database) WithTx(ctx context.Context, opts pgx.TxOptions, fn func(Querier) error) (err error) {
	tx, err := d.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = tx.Rollback(context.Background())
			panic(recovered)
		}
		if err != nil {
			_ = tx.Rollback(context.Background())
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// SchemaVersion returns the highest currently applied goose migration.
func (d *Database) SchemaVersion(ctx context.Context) (int64, error) {
	var version int64
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(version_id), 0)
		FROM goose_db_version
		WHERE is_applied = true
	`).Scan(&version)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return 0, fmt.Errorf("schema is not initialized; run portage-migrate up")
		}
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

// Check verifies connectivity and binary/schema compatibility.
func (d *Database) Check(ctx context.Context) Health {
	checkCtx, cancel := context.WithTimeout(ctx, d.healthTimeout)
	defer cancel()
	if err := d.pool.Ping(checkCtx); err != nil {
		return Health{Reason: fmt.Sprintf("database ping failed: %v", err)}
	}
	version, err := d.SchemaVersion(checkCtx)
	if err != nil {
		return Health{Reason: err.Error()}
	}
	if version < MinSchemaVersion {
		return Health{SchemaVersion: version, Reason: fmt.Sprintf("schema %d is older than minimum %d; run portage-migrate up", version, MinSchemaVersion)}
	}
	if version > MaxSchemaVersion {
		return Health{SchemaVersion: version, Reason: fmt.Sprintf("schema %d is newer than server maximum %d; upgrade the server", version, MaxSchemaVersion)}
	}
	return Health{OK: true, SchemaVersion: version}
}

// Close releases all PostgreSQL connections.
func (d *Database) Close() {
	if d != nil && d.pool != nil {
		d.pool.Close()
	}
}
