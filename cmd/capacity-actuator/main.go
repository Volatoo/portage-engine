// Command capacity-actuator consumes PostgreSQL-fenced autoscale actions and
// owns the exact provider instances created for persistent phase executors.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/slchris/portage-engine/internal/capacity"
	"github.com/slchris/portage-engine/internal/iac"
	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/pkg/config"
)

const maxActuatorConfigBytes = 1 << 20

var (
	actuatorConfigPath = flag.String(
		"config", "configs/capacity-actuator.json",
		"Path to the secret-free capacity actuator configuration",
	)
	serverConfigPath = flag.String(
		"server-config", "configs/server.conf",
		"Path to the server configuration containing PostgreSQL settings",
	)
)

type fileConfig struct {
	Owner                 string                `json:"owner"`
	LeaseSeconds          int                   `json:"lease_seconds"`
	PollSeconds           int                   `json:"poll_seconds"`
	OperationLimitMinutes int                   `json:"operation_limit_minutes"`
	IdleSeconds           int                   `json:"idle_seconds"`
	PVE                   capacityPVEFileConfig `json:"pve"`
}

type capacityPVEFileConfig struct {
	Endpoint     string                         `json:"endpoint"`
	Insecure     bool                           `json:"insecure"`
	Node         string                         `json:"node"`
	Nodes        []string                       `json:"nodes,omitempty"`
	Storage      string                         `json:"storage"`
	Network      string                         `json:"network"`
	WorkspaceDir string                         `json:"workspace_dir"`
	Templates    []capacity.PVEExecutorTemplate `json:"templates"`
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	actuatorConfig, err := loadFileConfig(*actuatorConfigPath)
	if err != nil {
		return err
	}
	serverConfig, err := config.LoadServerConfig(*serverConfigPath)
	if err != nil {
		return fmt.Errorf("load server database configuration: %w", err)
	}
	if !serverConfig.Database.Enabled || !serverConfig.Database.Required {
		return fmt.Errorf("capacity actuator requires PostgreSQL required mode")
	}
	db, err := persistence.Open(context.Background(), serverConfig.Database)
	if err != nil {
		return fmt.Errorf("open capacity actuator database: %w", err)
	}
	defer db.Close()
	health := db.Check(context.Background())
	if !health.OK {
		return fmt.Errorf("capacity actuator database is not ready: %s", health.Reason)
	}
	credentials, err := loadPVECredentials()
	if err != nil {
		return err
	}
	pveProvider, err := capacity.NewPVEProvider(capacity.PVEProviderConfig{
		Endpoint:     actuatorConfig.PVE.Endpoint,
		Insecure:     actuatorConfig.PVE.Insecure,
		Node:         actuatorConfig.PVE.Node,
		Nodes:        actuatorConfig.PVE.Nodes,
		Storage:      actuatorConfig.PVE.Storage,
		Network:      actuatorConfig.PVE.Network,
		WorkspaceDir: actuatorConfig.PVE.WorkspaceDir,
		Credentials:  credentials,
		Templates:    actuatorConfig.PVE.Templates,
	})
	if err != nil {
		return fmt.Errorf("configure capacity PVE provider: %w", err)
	}
	actuator, err := capacity.New(
		persistence.NewJobRepository(db),
		capacity.ProviderSet{"pve": pveProvider},
		capacity.Config{
			Owner: actuatorConfig.Owner,
			Lease: time.Duration(actuatorConfig.LeaseSeconds) * time.Second,
			PollInterval: time.Duration(
				actuatorConfig.PollSeconds,
			) * time.Second,
			OperationLimit: time.Duration(
				actuatorConfig.OperationLimitMinutes,
			) * time.Minute,
			IdleInterval: time.Duration(
				actuatorConfig.IdleSeconds,
			) * time.Second,
		},
	)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(
		context.Background(), syscall.SIGINT, syscall.SIGTERM,
	)
	defer stop()
	log.Printf(
		"Capacity actuator %s started (schema %d; providers: pve)",
		actuatorConfig.Owner, health.SchemaVersion,
	)
	err = actuator.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func loadFileConfig(path string) (*fileConfig, error) {
	file, err := os.Open(path) // #nosec G304 -- operator-selected config path.
	if err != nil {
		return nil, fmt.Errorf("open capacity actuator config: %w", err)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, maxActuatorConfigBytes+1))
	decoder.DisallowUnknownFields()
	var result fileConfig
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode capacity actuator config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("capacity actuator config contains multiple JSON values")
		}
		return nil, fmt.Errorf("decode capacity actuator config trailer: %w", err)
	}
	return &result, nil
}

func loadPVECredentials() (*iac.CloudCredentials, error) {
	tokenID := firstEnvironment("CLOUD_PVE_TOKEN_ID", "PM_API_TOKEN_ID")
	tokenSecret := firstEnvironment(
		"CLOUD_PVE_TOKEN_SECRET", "PM_API_TOKEN_SECRET",
	)
	username := firstEnvironment("CLOUD_PVE_USERNAME", "PM_USER")
	password := firstEnvironment("CLOUD_PVE_PASSWORD", "PM_PASS")
	switch {
	case tokenID != "" && tokenSecret != "":
		return &iac.CloudCredentials{
			PVETokenID: tokenID, PVETokenSecret: tokenSecret,
		}, nil
	case username != "" && password != "":
		return &iac.CloudCredentials{
			PVEUsername: username, PVEPassword: password,
		}, nil
	default:
		return nil, fmt.Errorf(
			"capacity actuator requires a complete PVE token or username/password pair",
		)
	}
}

func firstEnvironment(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}
