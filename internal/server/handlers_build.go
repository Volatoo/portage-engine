package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/binpkg"
	"github.com/slchris/portage-engine/internal/builder"
)

// handlePackageQuery handles package availability queries.
func (s *Server) handlePackageQuery(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.metrics.IncHTTPRequests()

	if r.Method != http.MethodPost {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req binpkg.QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	store, profile, ok := s.binhostStoreForProfile(req.ProfileID)
	if !ok {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Unknown profile_id", http.StatusBadRequest)
		return
	}
	if req.Arch != "" && profile.Arch != "" && req.Arch != profile.Arch {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Requested arch does not match profile", http.StatusBadRequest)
		return
	}

	// Query only the selected profile's PKGDIR. Cross-profile fallback could
	// silently install ABI-incompatible packages, so it is deliberately absent.
	s.metrics.IncStorageReads()
	pkg, found := store.Query(&req)

	response := binpkg.QueryResponse{
		Found: found, Package: pkg, ProfileID: profile.ID,
		BinhostPath: profile.BinhostPath,
	}

	s.metrics.RecordHTTPLatency("/api/v1/packages/query", time.Since(start))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// handleBuildRequest handles build requests for missing packages.
func (s *Server) handleBuildRequest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.metrics.IncHTTPRequests()

	if r.Method != http.MethodPost {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Support both formats: {"package_name":"cat/pkg"} and {"category":"cat","package":"pkg"}
	var rawReq map[string]interface{}
	if err := decodeBuildJSON(w, r, &rawReq, false); err != nil {
		s.metrics.IncHTTPRequestErrors()
		writeBuildDecodeError(w, err)
		return
	}

	var req builder.BuildRequest

	// Convert to BuildRequest format
	if packageName, ok := rawReq["package_name"].(string); ok {
		req.PackageName = packageName
	} else if category, okCat := rawReq["category"].(string); okCat {
		if pkg, okPkg := rawReq["package"].(string); okPkg {
			req.PackageName = category + "/" + pkg
		}
	}

	if version, ok := rawReq["version"].(string); ok {
		req.Version = version
	}

	if arch, ok := rawReq["arch"].(string); ok {
		req.Arch = arch
	}
	if req.Arch == "" {
		req.Arch = "amd64"
	}
	req.IdempotencyKey = r.Header.Get("Idempotency-Key")

	if provider, ok := rawReq["cloud_provider"].(string); ok {
		req.CloudProvider = provider
	}
	if profileID, ok := rawReq["profile_id"].(string); ok {
		req.ProfileID = profileID
	}
	if resourceClass, ok := rawReq["resource_class"].(string); ok {
		req.ResourceClass = resourceClass
	}
	if repositoryIDs, ok := rawReq["repository_ids"].([]interface{}); ok {
		for _, value := range repositoryIDs {
			if id, ok := value.(string); ok {
				req.RepositoryIDs = append(req.RepositoryIDs, id)
			}
		}
	}

	if useFlags, ok := rawReq["use_flags"].([]interface{}); ok {
		req.UseFlags = make([]string, len(useFlags))
		for i, flag := range useFlags {
			if flagStr, ok := flag.(string); ok {
				req.UseFlags[i] = flagStr
			}
		}
	}

	// Reject requests with no package rather than creating an empty queued job.
	if strings.TrimSpace(req.PackageName) == "" {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "package_name (or category+package) is required", http.StatusBadRequest)
		return
	}

	// Submit build request
	s.metrics.IncBuildsTotal()
	jobID, err := s.builder.SubmitBuild(&req)
	if err != nil {
		s.metrics.IncHTTPRequestErrors()
		status := http.StatusInternalServerError
		switch {
		case builder.IsIdempotencyConflict(err):
			status = http.StatusConflict
		case builder.IsRequestError(err):
			status = http.StatusBadRequest
		case builder.IsLedgerError(err):
			status = http.StatusServiceUnavailable
		}
		http.Error(w, err.Error(), status)
		return
	}

	s.metrics.RecordHTTPLatency("/api/v1/packages/request-build", time.Since(start))

	response := builder.BuildResponse{
		JobID:  jobID,
		Status: "queued",
	}
	if status, statusErr := s.builder.GetStatus(jobID); statusErr == nil {
		response.Status = status.Status
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(response)
}

// handleBuildStatus handles build status queries.
func (s *Server) handleBuildStatus(w http.ResponseWriter, r *http.Request) {
	s.metrics.IncHTTPRequests()

	if r.Method != http.MethodGet {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		s.metrics.IncHTTPRequestErrors()
		http.Error(w, "Missing job_id parameter", http.StatusBadRequest)
		return
	}

	status, err := s.builder.GetStatus(jobID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// handleSubmitBuildWithConfig handles build requests with configuration bundles.
func (s *Server) handleSubmitBuildWithConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req builder.LocalBuildRequest
	if err := decodeBuildJSON(w, r, &req, true); err != nil {
		writeBuildDecodeError(w, err)
		return
	}

	// Validate request
	if req.ConfigBundle == nil {
		http.Error(w, "Missing configuration bundle", http.StatusBadRequest)
		return
	}

	if req.ConfigBundle.Packages == nil || len(req.ConfigBundle.Packages.Packages) == 0 {
		http.Error(w, "No packages specified in bundle", http.StatusBadRequest)
		return
	}

	// Translate to a Manager BuildRequest carrying the full bundle, which is
	// forwarded verbatim to a remote builder so the exact configuration is used.
	buildReq := &builder.BuildRequest{
		PackageName:    req.PackageName,
		Version:        req.Version,
		Arch:           req.Arch,
		ProfileID:      req.ProfileID,
		RepositoryIDs:  req.RepositoryIDs,
		ResourceClass:  req.ResourceClass,
		ConfigBundle:   req.ConfigBundle,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	}
	if buildReq.PackageName == "" && len(req.ConfigBundle.Packages.Packages) > 0 {
		buildReq.PackageName = req.ConfigBundle.Packages.Packages[0].Atom
	}

	s.metrics.IncBuildsTotal()
	jobID, err := s.builder.SubmitBuild(buildReq)
	if err != nil {
		s.metrics.IncHTTPRequestErrors()
		status := http.StatusServiceUnavailable
		if builder.IsIdempotencyConflict(err) {
			status = http.StatusConflict
		} else if builder.IsRequestError(err) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	status := "queued"
	if snapshot, statusErr := s.builder.GetStatus(jobID); statusErr == nil {
		status = snapshot.Status
	}
	_ = json.NewEncoder(w).Encode(builder.BuildResponse{JobID: jobID, Status: status})
}

// decodeBuildJSON applies the public request-size limit before decoding. The
// typed ConfigBundle endpoint additionally rejects unknown fields so clients
// cannot believe a misspelled security-relevant field was applied.
func decodeBuildJSON(w http.ResponseWriter, r *http.Request, dst any, strict bool) error {
	r.Body = http.MaxBytesReader(w, r.Body, builder.MaxBuildRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

func writeBuildDecodeError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "Invalid request body", http.StatusBadRequest)
}

// handleBuildsList returns all build jobs.
func (s *Server) handleBuildsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get limit parameter (default 0 = all, max 200)
	limitStr := r.URL.Query().Get("limit")
	limit := 0
	if limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = parsed
			if limit > 200 {
				limit = 200
			}
		}
	}

	builds := s.builder.ListAllBuilds()

	// Sort by created_at descending (newest first) for stable ordering
	sort.Slice(builds, func(i, j int) bool {
		return builds[i].CreatedAt.After(builds[j].CreatedAt)
	})

	// Apply limit if specified
	if limit > 0 && len(builds) > limit {
		builds = builds[:limit]
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(builds)
}

// handleClusterStatus returns the cluster status.
func (s *Server) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := s.builder.GetClusterStatus()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// handleBuildLogs returns logs for a specific build job.
func (s *Server) handleBuildLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "Missing job_id parameter", http.StatusBadRequest)
		return
	}

	logs, err := s.builder.GetBuildLogs(jobID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	response := map[string]interface{}{
		"job_id":       jobID,
		"logs":         logs,
		"generated_at": time.Now().UTC(),
		"bytes":        len(logs),
		"truncated":    strings.Contains(logs, "[... log truncated: middle omitted ...]"),
		"stages":       summarizeBuildLogStages(logs),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

type buildLogStageSummary struct {
	ID          string     `json:"id"`
	LineCount   int        `json:"line_count"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	LastMessage string     `json:"last_message,omitempty"`
}

var buildLogStages = []string{"queued", "provision", "deploy", "build", "collect", "verify", "sign", "publish", "cleanup"}

// summarizeBuildLogStages turns the bounded raw log into a small, stable
// structure for the dashboard. The raw log remains available for diagnostics;
// this summary lets the UI show stage counts and timing without guessing from
// a truncated browser-side string.
func summarizeBuildLogStages(logs string) []buildLogStageSummary {
	summaries := make([]buildLogStageSummary, len(buildLogStages))
	index := make(map[string]int, len(buildLogStages))
	for i, id := range buildLogStages {
		summaries[i].ID = id
		index[id] = i
	}
	for _, line := range strings.Split(logs, "\n") {
		stage := ""
		for _, id := range buildLogStages {
			if strings.Contains(line, "["+id+"]") {
				stage = id
				break
			}
		}
		if stage == "" {
			continue
		}
		summary := &summaries[index[stage]]
		summary.LineCount++
		if fields := strings.Fields(line); len(fields) > 0 {
			if timestamp, err := time.Parse(time.RFC3339Nano, fields[0]); err == nil {
				if summary.StartedAt == nil {
					startedAt := timestamp
					summary.StartedAt = &startedAt
				}
				updatedAt := timestamp
				summary.UpdatedAt = &updatedAt
			}
		}
		summary.LastMessage = truncateLogSummary(line, 320)
	}
	return summaries
}

func truncateLogSummary(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

// handleSchedulerStatus returns scheduler status with task assignments.
func (s *Server) handleSchedulerStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := s.builder.GetSchedulerStatus()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// handleBuildDelete removes a finished job record (DELETE ?job_id=).
func (s *Server) handleBuildDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "Missing job_id parameter", http.StatusBadRequest)
		return
	}
	if err := s.builder.DeleteJob(jobID); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"deleted": jobID})
}

func (s *Server) handleBuildCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "Missing job_id parameter", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	if len(reason) > 512 {
		http.Error(w, "cancel reason is too long", http.StatusBadRequest)
		return
	}
	if err := s.builder.CancelJob(jobID, reason); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"job_id": jobID, "status": "canceled"})
}

func (s *Server) handleBuildRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "Missing job_id parameter", http.StatusBadRequest)
		return
	}
	if err := s.builder.RetryJob(jobID); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"job_id": jobID, "status": "queued"})
}

// handleBuildsCleanupFailed removes every failed job record.
func (s *Server) handleBuildsCleanupFailed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	n := s.builder.CleanupFailedJobs()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"removed": n})
}

// handleInstancesList returns the live cloud instances.
func (s *Server) handleInstancesList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.builder.ListInstances())
}
