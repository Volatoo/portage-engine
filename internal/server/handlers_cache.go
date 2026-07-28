package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/slchris/portage-engine/internal/iam"
)

func (s *Server) handleCacheStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.cache == nil {
		status := http.StatusOK
		if s.config.Cache.Enabled {
			status = http.StatusServiceUnavailable
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enabled": s.config.Cache.Enabled, "ok": !s.config.Cache.Enabled,
			"error":                s.cacheInitErr,
			"correctness_fallback": "postgresql polling",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	health := s.cache.Health(ctx)
	cancel()
	if !health.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(health)
}

func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cache == nil {
		http.Error(w, "live events require Redis; poll build status as fallback", http.StatusServiceUnavailable)
		return
	}
	_, project, err := s.authorizeProject(r, iam.RoleViewer)
	if err != nil {
		writeIAMError(w, http.StatusForbidden, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	ready, _ := json.Marshal(map[string]string{
		"transport": "redis-pubsub", "project_id": project.ProjectID,
	})
	_, _ = fmt.Fprintf(w, "event: ready\ndata: %s\n\n", ready)
	flusher.Flush()

	err = s.cache.StreamJobEvents(r.Context(), func(payload string) error {
		var event struct {
			JobID string `json:"job_id"`
		}
		if json.Unmarshal([]byte(payload), &event) != nil || event.JobID == "" {
			return nil
		}
		status, statusErr := s.builder.GetStatus(event.JobID)
		if statusErr != nil || (project.ProjectID != "" && status.ProjectID != project.ProjectID) {
			return nil
		}
		if _, err := fmt.Fprintf(w, "event: job\ndata: %s\n\n", payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
	if err != nil && r.Context().Err() == nil {
		// Headers are already committed; closing the stream makes EventSource
		// reconnect and status polling remains the correctness fallback.
		return
	}
}
