// Package main implements the Portage Engine Server.
// The server handles package queries, build requests, and package synchronization.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/slchris/portage-engine/internal/server"
	"github.com/slchris/portage-engine/internal/workergateway"
	"github.com/slchris/portage-engine/pkg/config"
)

var (
	configPath  = flag.String("config", "configs/server.conf", "Path to configuration file")
	port        = flag.Int("port", 8080, "Server port")
	showVersion = flag.Bool("version", false, "Print version and exit")
)

// Version information (injected at build time via -ldflags).
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Printf("portage-server %s (commit: %s, built: %s)\n", version, commit, buildTime)
		return
	}

	// Load configuration
	cfg, err := config.LoadServerConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Validate configuration and print warnings
	if warnings := cfg.Validate(); len(warnings) > 0 {
		for _, w := range warnings {
			log.Printf("WARNING: %s", w)
		}
	}
	if cfg.RuntimeRole != "control-plane" && cfg.RuntimeRole != "api" &&
		cfg.RuntimeRole != "executor" {
		log.Fatalf("Invalid SERVER_RUNTIME_ROLE %q", cfg.RuntimeRole)
	}
	if cfg.RuntimeRole == "executor" && cfg.ControlPlaneID == "" {
		log.Fatal("Executor role requires a stable CONTROL_PLANE_ID")
	}
	if cfg.RuntimeRole == "executor" {
		if len(cfg.ExecutorCapabilities) == 0 {
			log.Fatal("Executor role requires explicit EXECUTOR_CAPABILITIES for one immutable pool")
		}
		instanceID, err := uuid.Parse(cfg.ExecutorCapacityInstanceID)
		if err != nil || instanceID.String() != cfg.ExecutorCapacityInstanceID {
			log.Fatal("Executor role requires a lowercase UUID EXECUTOR_CAPACITY_INSTANCE_ID")
		}
	}

	// Override port if specified
	if *port != 8080 {
		cfg.Port = *port
	}
	if err := cfg.ValidateStartup(); err != nil {
		log.Fatalf("Configuration validation failed: %v", err)
	}

	// Propagate version info to the server package
	server.Version = version
	server.Commit = commit
	server.BuildTime = buildTime
	executorOnly := cfg.RuntimeRole == "executor"

	// Create server instance
	srv := server.New(cfg)
	var workerGateway *http.Server
	var workerIssuer workergateway.Issuer
	if executorOnly {
		workerIssuer, err = configuredTrustedWorkerIssuer(cfg)
	} else {
		workerGateway, workerIssuer, err = newWorkerGatewayServer(
			cfg, srv.WorkerGatewayHandler(),
		)
	}
	if err != nil {
		log.Fatalf("Failed to configure worker gateway: %v", err)
	}
	if workerIssuer != nil {
		if err := srv.SetWorkerIssuer(workerIssuer); err != nil {
			log.Fatalf("Failed to install worker issuer: %v", err)
		}
	}

	// Initialize server (GPG keys, etc.)
	if err := srv.Initialize(); err != nil {
		log.Fatalf("Failed to initialize server: %v", err)
	}
	// HTTP server configuration.
	//
	// No ReadTimeout/WriteTimeout: these are whole-request deadlines that would
	// abort large binary-package uploads/downloads mid-stream. ReadHeaderTimeout
	// still bounds slow-header (Slowloris) attacks, and IdleTimeout reaps idle
	// keep-alive connections; per-request work uses request context deadlines.
	var httpServer *http.Server
	if !executorOnly {
		httpServer = &http.Server{
			Addr:              fmt.Sprintf(":%d", cfg.Port),
			Handler:           srv.Router(),
			ReadHeaderTimeout: 15 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		go func() {
			log.Printf("Starting Portage Engine Server on port %d", cfg.Port)
			if err := httpServer.ListenAndServe(); err != nil &&
				err != http.ErrServerClosed {
				log.Fatalf("Server failed to start: %v", err)
			}
		}()
	}
	if workerGateway != nil && !executorOnly {
		go func() {
			log.Printf("Starting outbound Worker Gateway with required mTLS on port %d", cfg.WorkerGatewayPort)
			if err := workerGateway.ListenAndServeTLS(cfg.WorkerGatewayTLSCert, cfg.WorkerGatewayTLSKey); err != nil &&
				err != http.ErrServerClosed {
				log.Fatalf("Worker Gateway failed to start: %v", err)
			}
		}()
	}
	if executorOnly {
		log.Printf(
			"Portage Engine executor %s is running without network listeners",
			cfg.ControlPlaneID,
		)
	}

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stop the HTTP server first so no new requests can reach the builder's work
	// queue, and in-flight requests drain, before we close that queue. Doing this
	// in the opposite order lets an in-flight build request panic on send to a
	// closed channel.
	if httpServer != nil {
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("Server forced to shutdown: %v", err)
		}
	}
	if workerGateway != nil && !executorOnly {
		if err := workerGateway.Shutdown(ctx); err != nil {
			log.Printf("Worker Gateway forced to shutdown: %v", err)
		}
	}

	// Now shut down server components (saves state, closes the work queue).
	srv.Shutdown()

	log.Println("Server exited")
}

func newWorkerGatewayServer(
	cfg *config.ServerConfig,
	handler http.Handler,
) (*http.Server, workergateway.Issuer, error) {
	if !cfg.WorkerGatewayEnabled {
		return nil, nil, nil
	}
	if !cfg.Database.Enabled || !cfg.Database.Required {
		return nil, nil, fmt.Errorf("worker gateway requires PostgreSQL required mode")
	}
	if cfg.WorkerGatewayPort <= 0 || cfg.WorkerGatewayPort > 65535 ||
		cfg.WorkerGatewayPort == cfg.Port {
		return nil, nil, fmt.Errorf("invalid or conflicting worker gateway port")
	}
	trustedIssuer, clientRoots, err := configuredTrustedWorkerIssuerAndRoots(cfg)
	if err != nil {
		return nil, nil, err
	}
	serverCA, err := os.ReadFile(cfg.WorkerGatewayServerCA)
	if err != nil {
		return nil, nil, fmt.Errorf("read worker gateway server CA: %w", err)
	}
	serverRoots := x509.NewCertPool()
	if !serverRoots.AppendCertsFromPEM(serverCA) {
		return nil, nil, fmt.Errorf("parse worker gateway server CA")
	}
	serverPair, err := tls.LoadX509KeyPair(cfg.WorkerGatewayTLSCert, cfg.WorkerGatewayTLSKey)
	if err != nil {
		return nil, nil, fmt.Errorf("load worker gateway server certificate: %w", err)
	}
	if len(serverPair.Certificate) == 0 {
		return nil, nil, fmt.Errorf("worker gateway server certificate chain is empty")
	}
	serverLeaf, err := x509.ParseCertificate(serverPair.Certificate[0])
	if err != nil {
		return nil, nil, fmt.Errorf("parse worker gateway server certificate: %w", err)
	}
	advertiseURL, err := url.Parse(cfg.WorkerGatewayAdvertiseURL)
	if err != nil || advertiseURL.Scheme != "https" || advertiseURL.Hostname() == "" {
		return nil, nil, fmt.Errorf("worker gateway advertise URL must be an HTTPS origin")
	}
	intermediates := x509.NewCertPool()
	for _, der := range serverPair.Certificate[1:] {
		certificate, parseErr := x509.ParseCertificate(der)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("parse worker gateway server chain: %w", parseErr)
		}
		intermediates.AddCert(certificate)
	}
	if _, err := serverLeaf.Verify(x509.VerifyOptions{
		DNSName: advertiseURL.Hostname(), Roots: serverRoots, Intermediates: intermediates,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return nil, nil, fmt.Errorf("verify worker gateway server certificate: %w", err)
	}
	return &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.WorkerGatewayPort),
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       45 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			ClientAuth: tls.RequireAndVerifyClientCert,
			ClientCAs:  clientRoots,
		},
	}, trustedIssuer, nil
}

func configuredTrustedWorkerIssuer(
	cfg *config.ServerConfig,
) (workergateway.Issuer, error) {
	issuer, _, err := configuredTrustedWorkerIssuerAndRoots(cfg)
	return issuer, err
}

func configuredTrustedWorkerIssuerAndRoots(
	cfg *config.ServerConfig,
) (workergateway.Issuer, *x509.CertPool, error) {
	if !cfg.WorkerGatewayEnabled {
		return nil, nil, nil
	}
	if !cfg.Database.Enabled || !cfg.Database.Required {
		return nil, nil, fmt.Errorf("worker issuer requires PostgreSQL required mode")
	}
	caPEM, err := os.ReadFile(cfg.WorkerGatewayClientCA)
	if err != nil {
		return nil, nil, fmt.Errorf("read worker client CA: %w", err)
	}
	clientRoots := x509.NewCertPool()
	if !clientRoots.AppendCertsFromPEM(caPEM) {
		return nil, nil, fmt.Errorf("parse worker client CA")
	}
	issuer, err := configuredWorkerIssuer(cfg)
	if err != nil {
		return nil, nil, err
	}
	trustedIssuer, err := workergateway.NewTrustingIssuer(issuer, clientRoots)
	if err != nil {
		return nil, nil, err
	}
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 30*time.Second)
	probe, err := trustedIssuer.Issue(
		probeCtx,
		workergateway.Identity{
			WorkerID: "startup-probe", JobID: "startup-probe",
			AttemptID: "startup-probe", FenceToken: 1,
		}, time.Minute)
	cancelProbe()
	if err != nil {
		return nil, nil, fmt.Errorf("validate worker issuer: %w", err)
	}
	defer clear(probe.KeyPEM)
	probeBlock, _ := pem.Decode(probe.CertPEM)
	if probeBlock == nil {
		return nil, nil, fmt.Errorf("validate worker issuer output")
	}
	probeLeaf, err := x509.ParseCertificate(probeBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse worker issuer probe: %w", err)
	}
	if _, err := probeLeaf.Verify(x509.VerifyOptions{
		Roots: clientRoots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return nil, nil, fmt.Errorf("worker issuer is not trusted by WORKER_GATEWAY_CLIENT_CA: %w", err)
	}
	return trustedIssuer, clientRoots, nil
}

func configuredWorkerIssuer(
	cfg *config.ServerConfig,
) (workergateway.Issuer, error) {
	switch cfg.WorkerGatewayIssuerProvider {
	case workergateway.IssuerProviderFile:
		return workergateway.NewFileIssuer(
			cfg.WorkerGatewayIssuerID,
			cfg.WorkerGatewayIssuerCert,
			cfg.WorkerGatewayIssuerKey,
		), nil
	case workergateway.IssuerProviderVault:
		return workergateway.NewVaultIssuer(workergateway.VaultIssuerConfig{
			ID:           cfg.WorkerGatewayIssuerID,
			Address:      cfg.WorkerGatewayVaultAddress,
			Mount:        cfg.WorkerGatewayVaultMount,
			Role:         cfg.WorkerGatewayVaultRole,
			TokenPath:    cfg.WorkerGatewayVaultTokenPath,
			Namespace:    cfg.WorkerGatewayVaultNamespace,
			ServerCAPath: cfg.WorkerGatewayVaultServerCA,
			Timeout: time.Duration(
				cfg.WorkerGatewayVaultTimeout,
			) * time.Second,
		})
	default:
		return nil, fmt.Errorf(
			"unsupported worker issuer provider %q",
			cfg.WorkerGatewayIssuerProvider,
		)
	}
}
