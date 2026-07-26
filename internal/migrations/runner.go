// Package migrations owns the embedded PostgreSQL schema and its one-shot
// migration runner. It is separate from persistence so portage-server does not
// link migration drivers or gain schema-mutation capability.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/pkg/config"
)

//go:embed sql/*.sql
var migrationFiles embed.FS

// Runner owns a Goose provider backed by embedded reviewed SQL.
type Runner struct {
	provider *goose.Provider
}

// NewRunner opens a dedicated database/sql connection for schema
// administration. Application server replicas never call this function.
func NewRunner(ctx context.Context, cfg config.DatabaseConfig) (*Runner, error) {
	dsn, err := persistence.ConnectionString(cfg)
	if err != nil {
		return nil, err
	}
	connCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL configuration: %w", err)
	}
	connCfg.RuntimeParams["application_name"] = "portage-migrate"
	if cfg.ConnectTimeoutSeconds > 0 {
		connCfg.ConnectTimeout = time.Duration(cfg.ConnectTimeoutSeconds) * time.Second
	}

	db := stdlib.OpenDB(*connCfg)
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping PostgreSQL for migration: %w", err)
	}
	sub, err := fs.Sub(migrationFiles, "sql")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("load embedded migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create migration provider: %w", err)
	}
	return &Runner{provider: provider}, nil
}

// Provider returns the migration provider to the CLI and integration tests.
func (r *Runner) Provider() *goose.Provider {
	return r.provider
}

// Close closes the provider and its underlying database connection.
func (r *Runner) Close() error {
	if r == nil || r.provider == nil {
		return nil
	}
	return r.provider.Close()
}
