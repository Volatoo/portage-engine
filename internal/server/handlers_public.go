package server

import (
	"net/http"
	"time"
)

type publicComponentStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type publicServiceStatus struct {
	Status     string                  `json:"status"`
	Version    string                  `json:"version"`
	UpdatedAt  time.Time               `json:"updated_at"`
	Components []publicComponentStatus `json:"components"`
}

// handlePublicStatus intentionally returns a coarse, stable status document.
// Detailed health reasons, topology, worker counts, and dependency metadata
// remain available only from the system-administrator /health endpoint.
func (s *Server) handlePublicStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := s.publicStatus()
	w.Header().Set("Cache-Control", "public, max-age=15")
	writeJSON(w, status)
}

func (s *Server) publicStatus() publicServiceStatus {
	s.publicStatusMu.Lock()
	defer s.publicStatusMu.Unlock()
	now := time.Now().UTC()
	if now.Before(s.publicStatusUntil) {
		return s.publicStatusCache
	}

	repositoryStatus := "operational"
	if !s.checkStorageHealth() {
		repositoryStatus = "degraded"
	}

	buildStatus := "operational"
	databaseHealth := s.checkDatabaseHealth()
	ledgerOK, _ := s.checkLedgerHealth()
	if (s.config.Database.Required && !databaseHealth.OK) ||
		(s.jobLedger != nil && !ledgerOK) {
		buildStatus = "degraded"
	}

	overall := "operational"
	if repositoryStatus != "operational" || buildStatus != "operational" {
		overall = "degraded"
	}

	status := publicServiceStatus{
		Status: overall, Version: Version, UpdatedAt: now,
		Components: []publicComponentStatus{
			{Name: "Web and API", Status: "operational"},
			{Name: "Package repository", Status: repositoryStatus},
			{Name: "Build service", Status: buildStatus},
		},
	}
	s.publicStatusCache = status
	s.publicStatusUntil = now.Add(10 * time.Second)
	return status
}
