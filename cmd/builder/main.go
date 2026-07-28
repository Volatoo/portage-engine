// Package main provides the Portage Builder service.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/pkg/config"
)

func main() {
	cfg := loadConfig()
	bldr := builder.NewLocalBuilder(cfg.Workers, cfg)
	if cfg.PullEnabled {
		if err := runPullMode(cfg, bldr); err != nil {
			log.Printf("Outbound-pull builder stopped: %v", err)
			os.Exit(1)
		}
		return
	}

	mux := setupHTTPHandlers(bldr)
	handler := authMiddleware(cfg.AuthToken, mux)
	server := startServer(cfg, handler)

	stopHeartbeat := startHeartbeat(cfg, bldr)
	defer stopHeartbeat()

	waitForShutdown(server, bldr)
}

func runPullMode(cfg *config.BuilderConfig, bldr *builder.LocalBuilder) error {
	log.Printf("Starting outbound-pull builder; no inbound build listener will be opened")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	defer bldr.Shutdown()
	return builder.RunPullAgent(ctx, cfg, bldr)
}

// heartbeatInterval is how often the builder refreshes its registration with
// the central server. The server marks builders offline when heartbeats stop.
const heartbeatInterval = 30 * time.Second

// startHeartbeat registers this builder with the central server and keeps the
// registration alive with periodic heartbeats, so the server's builder
// registry (health checks, artifact routing, dashboard) sees manually-started
// builders too — previously only cloud-init deployed instances registered.
// No-op when SERVER_URL is unset. Returns a stop function.
func startHeartbeat(cfg *config.BuilderConfig, bldr *builder.LocalBuilder) func() {
	if cfg.ServerURL == "" {
		log.Println("SERVER_URL not set; not registering with a central server")
		return func() {}
	}

	client := builder.NewBuilderClient(cfg.ServerURL)
	client.SetAPIKey(cfg.ServerAPIKey)

	builderID := cfg.InstanceID
	hostname, _ := os.Hostname()
	if builderID == "" {
		builderID = hostname
	}
	endpoint := cfg.AdvertiseURL
	if endpoint == "" {
		endpoint = fmt.Sprintf("http://%s:%d", hostname, cfg.Port)
	}

	send := func() {
		status := "online"
		if !bldr.AcceptingBuilds() {
			status = "draining"
		}
		hb := &builder.HeartbeatRequest{
			BuilderID:  builderID,
			Status:     status,
			Endpoint:   endpoint,
			Capacity:   bldr.Capacity(),
			ActiveJobs: bldr.ActiveJobs(),
			Timestamp:  time.Now(),
		}
		if err := client.SendHeartbeat(hb); err != nil {
			log.Printf("Warning: heartbeat to %s failed: %v", cfg.ServerURL, err)
		}
	}

	log.Printf("Registering builder %q (endpoint %s) with server %s", builderID, endpoint, cfg.ServerURL)
	send()

	// #nosec G118 -- cancel is returned to the caller, which defers it for shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				send()
			}
		}
	}()
	return cancel
}

// authMiddleware requires a shared token on every endpoint except /health.
// The token is presented as "X-API-Key: <token>" or "Authorization: Bearer <token>".
// If token is empty, auth is disabled (a startup warning is logged separately).
func authMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		provided := r.Header.Get("X-API-Key")
		if provided == "" {
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				provided = strings.TrimPrefix(auth, "Bearer ")
			}
		}

		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			http.Error(w, "unauthorized: invalid or missing builder token", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// loadConfig loads and parses configuration.
func loadConfig() *config.BuilderConfig {
	configPath := flag.String("config", "configs/builder.conf", "Path to configuration file")
	port := flag.Int("port", 9090, "Builder service port")
	flag.Parse()

	cfg, err := config.LoadBuilderConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	if *port != 9090 {
		cfg.Port = *port
	}

	for _, w := range cfg.Validate() {
		log.Printf("WARNING: %s", w)
	}

	if cfg.PullEnabled {
		log.Printf("Configured Portage Builder in outbound-pull mode")
	} else {
		log.Printf("Starting Portage Builder Service on port %d", cfg.Port)
	}
	return cfg
}

// setupHTTPHandlers sets up all HTTP handlers.
func setupHTTPHandlers(bldr *builder.LocalBuilder) *http.ServeMux {
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if !bldr.AcceptingBuilds() {
			http.Error(w, "DRAINING: native builder requires an external VM/rootfs reset", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	// Status endpoint
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		status := bldr.GetStatus()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	})

	// Build request endpoint
	mux.HandleFunc("/api/v1/build", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, builder.MaxBuildRequestBodyBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var req builder.LocalBuildRequest
		if err := decoder.Decode(&req); err != nil {
			writeBuilderDecodeError(w, err)
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			writeBuilderDecodeError(w, err)
			return
		}

		jobID, err := bldr.SubmitBuild(&req)
		if err != nil {
			if errors.Is(err, builder.ErrNativeBuilderDraining) {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		response := map[string]interface{}{
			"job_id": jobID,
			"status": "queued",
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})

	// Job status endpoint
	mux.HandleFunc("/api/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		jobID := r.URL.Path[len("/api/v1/jobs/"):]
		if jobID == "" {
			http.Error(w, "Job ID required", http.StatusBadRequest)
			return
		}

		status, err := bldr.GetJobStatus(jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	})

	// List all jobs
	mux.HandleFunc("/api/v1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		jobs := bldr.ListJobs()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jobs)
	})

	// Artifact info endpoint
	mux.HandleFunc("/api/v1/artifacts/info/", func(w http.ResponseWriter, r *http.Request) {
		jobID := r.URL.Path[len("/api/v1/artifacts/info/"):]
		if jobID == "" {
			http.Error(w, "Job ID required", http.StatusBadRequest)
			return
		}

		info, err := bldr.GetArtifactInfo(jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	})

	// Install-verification endpoint: proves a freshly built binpkg installs
	// cleanly from the binhost in a throwaway native Portage root.
	mux.HandleFunc("/api/v1/verify", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req builder.VerifyInstallRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PackageName == "" {
			http.Error(w, "package_name required", http.StatusBadRequest)
			return
		}
		out, err := bldr.VerifyInstall(req)
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"ok": err == nil, "log": out}
		if err != nil {
			resp["error"] = err.Error()
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// Artifact download endpoint
	mux.HandleFunc("/api/v1/artifacts/download/", func(w http.ResponseWriter, r *http.Request) {
		jobID := r.URL.Path[len("/api/v1/artifacts/download/"):]
		if jobID == "" {
			http.Error(w, "Job ID required", http.StatusBadRequest)
			return
		}

		var artifactPath string
		var err error
		if rel := r.URL.Query().Get("path"); rel != "" {
			artifactPath, err = bldr.GetArtifactPathByRel(jobID, rel)
		} else {
			artifactPath, err = bldr.GetArtifactPath(jobID)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		// Get file info for headers
		fileInfo, err := os.Stat(artifactPath) // #nosec G703 -- builder path confinement resolves and verifies this artifact path.
		if err != nil {
			http.Error(w, "Failed to get file info", http.StatusInternalServerError)
			return
		}

		// Open the file
		file, err := os.Open(artifactPath) // #nosec G304,G703 -- builder path confinement resolves and verifies this artifact path.
		if err != nil {
			http.Error(w, "Failed to open artifact file", http.StatusInternalServerError)
			return
		}
		defer func() { _ = file.Close() }()

		// Set headers for download
		fileName := fileInfo.Name()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", fileName))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size()))

		// Stream the file
		http.ServeContent(w, r, fileName, fileInfo.ModTime(), file)
	})

	return mux
}

func writeBuilderDecodeError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "Invalid request", http.StatusBadRequest)
}

// startServer starts the HTTP server.
func startServer(cfg *config.BuilderConfig, handler http.Handler) *http.Server {
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           loggingMiddleware(handler),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("Builder service listening on :%d", cfg.Port)
	return server
}

// waitForShutdown waits for shutdown signal and performs cleanup.
func waitForShutdown(server *http.Server, bldr *builder.LocalBuilder) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	<-sigChan
	log.Println("Shutting down builder service...")

	bldr.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("%s %q %q", r.Method, r.RequestURI, r.RemoteAddr) // #nosec G706 -- request-derived values are safely quoted.
		next.ServeHTTP(w, r)
		log.Printf("Completed in %v", time.Since(start))
	})
}
