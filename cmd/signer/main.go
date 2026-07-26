// Command signer runs the isolated Portage GPKG signing worker. It has no HTTP
// listener and receives only digest-bound tasks through PostgreSQL.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/slchris/portage-engine/internal/gpg"
	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/internal/signing"
	"github.com/slchris/portage-engine/pkg/config"
)

var (
	configPath  = flag.String("config", "configs/server.conf", "Path to bootstrap configuration")
	showVersion = flag.Bool("version", false, "Print version and exit")
)

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

//nolint:gocritic // fatal startup errors terminate the process before the worker accepts signing tasks.
func main() {
	flag.Parse()
	if *showVersion {
		fmt.Printf("portage-signer %s (commit: %s, built: %s)\n", version, commit, buildTime)
		return
	}
	cfg, err := config.LoadServerConfig(*configPath)
	if err != nil {
		log.Fatalf("load signer configuration: %v", err)
	}
	if !cfg.Database.Enabled || !cfg.Database.Required {
		log.Fatal("isolated signer requires DATABASE_ENABLED=true and DATABASE_REQUIRED=true")
	}
	if !cfg.GPGEnabled {
		log.Fatal("isolated signer requires GPG_ENABLED=true")
	}
	if cfg.BinpkgPath == "" || cfg.GPGHome == "" || cfg.GPGPublicKeyPath == "" {
		log.Fatal("BINPKG_PATH, GPG_HOME, and GPG_PUBLIC_KEY_PATH are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	db, err := persistence.Open(ctx, cfg.Database)
	if err != nil {
		stop()
		log.Fatalf("connect signer PostgreSQL queue: %v", err)
	}
	defer stop()
	defer db.Close()
	health := db.Check(ctx)
	if !health.OK {
		log.Fatalf("signer PostgreSQL schema is not ready: %s", health.Reason)
	}

	options := []gpg.SignerOption{gpg.WithGnupgHome(cfg.GPGHome)}
	if cfg.GPGAutoCreate {
		options = append(options, gpg.WithAutoCreate(cfg.GPGKeyName, cfg.GPGKeyEmail))
	}
	key := gpg.NewSigner(cfg.GPGKeyID, cfg.GPGKeyPath, true, options...)
	if err := gpg.CheckGPG(); err != nil {
		log.Fatalf("initialize signer executable: %v", err)
	}
	if err := key.Initialize(); err != nil {
		log.Fatalf("initialize isolated signing key: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.GPGPublicKeyPath), 0o750); err != nil {
		log.Fatalf("create signer public-key directory: %v", err)
	}
	publicKey, err := key.GetPublicKey()
	if err != nil {
		log.Fatalf("read signer public key: %v", err)
	}
	if err := writeAtomic(cfg.GPGPublicKeyPath, []byte(publicKey), 0o644); err != nil {
		log.Fatalf("publish signer public key: %v", err)
	}
	publicMetadata, err := json.Marshal(struct {
		KeyID     string    `json:"key_id"`
		UpdatedAt time.Time `json:"updated_at"`
	}{KeyID: key.KeyID(), UpdatedAt: time.Now().UTC()})
	if err != nil {
		log.Fatalf("encode signer public metadata: %v", err)
	}
	if err := writeAtomic(cfg.GPGPublicKeyPath+".json", publicMetadata, 0o644); err != nil {
		log.Fatalf("write signer public metadata: %v", err)
	}

	hostname, _ := os.Hostname()
	owner := "signer/" + hostname + "/" + uuid.NewString()
	repository := persistence.NewJobRepository(db)
	runner := &signing.Runner{
		Queue: repository, BinpkgPath: cfg.BinpkgPath, Owner: owner,
		Lease: 5 * time.Minute,
		GPG:   signing.GPG{Home: cfg.GPGHome, KeyID: key.KeyID()},
	}
	log.Printf("isolated signer ready (owner=%s key=%s schema=%d)", owner, key.KeyID(), health.SchemaVersion)
	if err := runner.Run(ctx); err != nil {
		log.Fatalf("signer queue failed: %v", err)
	}
	log.Printf("isolated signer stopped")
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
