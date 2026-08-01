// Package migrations owns the embedded PostgreSQL schema and its one-shot
// migration runner. It is separate from persistence so portage-server does not
// link migration drivers or gain schema-mutation capability.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/pkg/config"
)

//go:embed sql/*.sql
var migrationFiles embed.FS

// SchemaSupport is the schema range supported by this migration binary and
// the highest migration embedded in it. Callers must reject a disagreement:
// it means the binary compatibility constants and reviewed migrations were
// not advanced together.
type SchemaSupport struct {
	Min            int64 `json:"min"`
	Max            int64 `json:"max"`
	EmbeddedLatest int64 `json:"embedded_latest"`
}

// SupportedSchema returns the schema authority carried by this exact binary.
// It does not connect to PostgreSQL, so recovery tooling can inspect a binary
// before restoring any data.
func SupportedSchema() (SchemaSupport, error) {
	entries, err := fs.ReadDir(migrationFiles, "sql")
	if err != nil {
		return SchemaSupport{}, fmt.Errorf("read embedded migrations: %w", err)
	}

	var latest int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		versionText, _, found := strings.Cut(entry.Name(), "_")
		if !found || versionText == "" {
			return SchemaSupport{}, fmt.Errorf("embedded migration has invalid name %q", entry.Name())
		}
		version, err := strconv.ParseInt(versionText, 10, 64)
		if err != nil || version <= 0 {
			return SchemaSupport{}, fmt.Errorf("embedded migration has invalid version %q", entry.Name())
		}
		if version > latest {
			latest = version
		}
	}
	if latest == 0 {
		return SchemaSupport{}, fmt.Errorf("no embedded SQL migrations found")
	}

	support := SchemaSupport{
		Min:            persistence.MinSchemaVersion,
		Max:            persistence.MaxSchemaVersion,
		EmbeddedLatest: latest,
	}
	if support.Min <= 0 || support.Min > support.Max {
		return SchemaSupport{}, fmt.Errorf(
			"invalid binary schema range: min=%d max=%d", support.Min, support.Max,
		)
	}
	if support.Max != support.EmbeddedLatest {
		return SchemaSupport{}, fmt.Errorf(
			"binary schema maximum %d does not match latest embedded migration %d",
			support.Max, support.EmbeddedLatest,
		)
	}
	return support, nil
}

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
