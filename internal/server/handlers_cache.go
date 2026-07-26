package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
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
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	_, _ = fmt.Fprint(w, "event: ready\ndata: {\"transport\":\"redis-pubsub\"}\n\n")
	flusher.Flush()

	err := s.cache.StreamJobEvents(r.Context(), func(payload string) error {
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
