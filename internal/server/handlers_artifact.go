package server

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// builderProxyClient is used for all server→builder proxy calls; it has a
// bounded timeout so a hung builder cannot tie up a request goroutine forever.
var builderProxyClient = &http.Client{Timeout: 60 * time.Second}

// getFromBuilder issues an authenticated GET to a builder endpoint, presenting
// the shared builder token when one is configured.
func (s *Server) getFromBuilder(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if s.config.BuilderToken != "" {
		req.Header.Set("X-API-Key", s.config.BuilderToken)
	}
	return builderProxyClient.Do(req)
}

// handleArtifactInfo returns artifact metadata for a job from a builder.
func (s *Server) handleArtifactInfo(w http.ResponseWriter, r *http.Request) {
	s.metrics.IncHTTPRequests()

	if r.Method != http.MethodGet {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract job ID from URL
	jobID := r.URL.Path[len("/api/v1/artifacts/info/"):]
	if jobID == "" {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Job ID required", http.StatusBadRequest)
		return
	}

	// Get builder URL for this job
	builderURL, err := s.getBuilderURLForJob(jobID)
	if err != nil {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Proxy request to builder
	infoURL := fmt.Sprintf("%s/api/v1/artifacts/info/%s", builderURL, jobID)
	resp, err := s.getFromBuilder(infoURL)
	if err != nil {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, fmt.Sprintf("Failed to contact builder: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		http.Error(w, string(body), resp.StatusCode)
		return
	}

	// Forward response
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.Copy(w, resp.Body)
}

// handleArtifactDownload proxies artifact download requests to the builder.
func (s *Server) handleArtifactDownload(w http.ResponseWriter, r *http.Request) {
	s.metrics.IncHTTPRequests()

	if r.Method != http.MethodGet {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract job ID from URL
	jobID := r.URL.Path[len("/api/v1/artifacts/download/"):]
	if jobID == "" {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Job ID required", http.StatusBadRequest)
		return
	}

	// Get builder URL for this job
	builderURL, err := s.getBuilderURLForJob(jobID)
	if err != nil {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Proxy request to builder
	downloadURL := fmt.Sprintf("%s/api/v1/artifacts/download/%s", builderURL, jobID)
	resp, err := s.getFromBuilder(downloadURL)
	if err != nil {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, fmt.Sprintf("Failed to contact builder: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		http.Error(w, string(body), resp.StatusCode)
		return
	}

	// Forward headers
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Stream the file
	_, _ = io.Copy(w, resp.Body)
}

// getBuilderURLForJob determines the builder URL that has the job.
// It checks registered builders and returns the URL of the one that has the job.
func (s *Server) getBuilderURLForJob(jobID string) (string, error) {
	// First, try to get from registered builders
	builders := s.builderRegistry.List()
	if len(builders) == 0 {
		// Fall back to default builder from config (RemoteBuilders)
		if len(s.builder.CloudSettings().RemoteBuilders) > 0 {
			return normalizeBuilderURL(s.builder.CloudSettings().RemoteBuilders[0]), nil
		}
		return "", fmt.Errorf("no builders registered and no default builder URL configured")
	}

	// Try each builder until we find the job
	for _, b := range builders {
		builderURL := b.Endpoint
		if builderURL == "" {
			continue
		}
		// Ensure the URL is normalized
		builderURL = normalizeBuilderURL(builderURL)
		statusURL := fmt.Sprintf("%s/api/v1/jobs/%s", builderURL, jobID)

		resp, err := s.getFromBuilder(statusURL)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return builderURL, nil
		}
	}

	// Fall back to first remote builder
	if len(s.builder.CloudSettings().RemoteBuilders) > 0 {
		return normalizeBuilderURL(s.builder.CloudSettings().RemoteBuilders[0]), nil
	}

	return "", fmt.Errorf("job not found on any registered builder: %s", jobID)
}

// binhostReadOnly wraps a file server so the binhost only answers GET and HEAD.
func (s *Server) binhostReadOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleGPGPublicKey serves the GPG public key for builders.
func (s *Server) handleGPGPublicKey(w http.ResponseWriter, r *http.Request) {
	s.metrics.IncHTTPRequests()

	if r.Method != http.MethodGet {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !s.config.GPGEnabled {
		http.Error(w, "GPG not enabled on server", http.StatusNotFound)
		return
	}
	_, publicKey, _ := s.gpgKeyMaterial()
	if len(publicKey) == 0 {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "isolated signer public key is not ready", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/pgp-keys")
	w.Header().Set("Content-Disposition", "attachment; filename=portage-engine.asc")
	_, _ = w.Write(publicKey)
}
