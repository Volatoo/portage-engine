// Command portage-migrate applies the embedded, reviewed PostgreSQL schema.
// It is intentionally separate from portage-server so migrations run once per
// deployment instead of racing across control-plane replicas.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/slchris/portage-engine/internal/migrations"
	"github.com/slchris/portage-engine/pkg/config"
)

var (
	configPath  = flag.String("config", "configs/server.conf", "Path to server bootstrap configuration")
	showVersion = flag.Bool("version", false, "Print binary version and exit")
	timeout     = flag.Duration("timeout", 2*time.Minute, "Maximum migration command duration")
)

var version = "dev"

//nolint:gocritic // fatal CLI errors terminate the process; the OS releases the short-lived migration connection.
func main() {
	flag.Parse()
	if *showVersion {
		fmt.Printf("portage-migrate %s\n", version)
		return
	}
	command := "status"
	if flag.NArg() > 0 {
		command = flag.Arg(0)
	}
	if flag.NArg() > 1 {
		log.Fatalf("unexpected argument %q", flag.Arg(1))
	}
	if command == "supported-schema" {
		support, err := migrations.SupportedSchema()
		if err != nil {
			log.Fatalf("read supported schema: %v", err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(support); err != nil {
			log.Fatalf("encode supported schema: %v", err)
		}
		return
	}

	cfg, err := config.LoadServerConfig(*configPath)
	if err != nil {
		log.Fatalf("load configuration: %v", err)
	}
	if !cfg.Database.Enabled {
		log.Fatal("database is disabled; set DATABASE_ENABLED=true and PostgreSQL connection fields")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	runner, err := migrations.NewRunner(ctx, cfg.Database)
	if err != nil {
		cancel()
		log.Fatalf("open migration runner: %v", err)
	}
	defer cancel()
	defer func() {
		if err := runner.Close(); err != nil {
			log.Printf("close migration runner: %v", err)
		}
	}()

	provider := runner.Provider()
	switch command {
	case "up":
		results, err := provider.Up(ctx)
		if err != nil {
			log.Fatalf("apply migrations: %v", err)
		}
		for _, result := range results {
			fmt.Println(result)
		}
		current, target, err := provider.GetVersions(ctx)
		if err != nil {
			log.Fatalf("read migration versions: %v", err)
		}
		fmt.Printf("schema ready: current=%d target=%d applied=%d\n", current, target, len(results))
	case "status":
		statuses, err := provider.Status(ctx)
		if err != nil {
			log.Fatalf("migration status: %v", err)
		}
		for _, status := range statuses {
			applied := "-"
			if !status.AppliedAt.IsZero() {
				applied = status.AppliedAt.UTC().Format(time.RFC3339)
			}
			fmt.Printf("%06d %-8s %s %s\n", status.Source.Version, status.State, applied, status.Source.Path)
		}
	case "db-version":
		current, err := provider.GetDBVersion(ctx)
		if err != nil {
			log.Fatalf("read database version: %v", err)
		}
		fmt.Println(current)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q; use up, status, db-version, or supported-schema\n", command)
		os.Exit(2)
	}
}
