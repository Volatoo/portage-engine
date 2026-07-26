package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

func (s *Server) handleRuntimeMetadataStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.jobLedger == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false, "ok": false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	status, err := s.jobLedger.RuntimeMetadataStatus(ctx)
	cancel()
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "ok": false, "error": err.Error()})
		return
	}
	integrityOK := status.MissingArtifacts == 0 && status.CorruptArtifacts == 0
	if !integrityOK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "ok": integrityOK, "status": status})
}
