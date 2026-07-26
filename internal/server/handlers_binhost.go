package server

import (
	"net/http"
	"sort"
)

type binhostInventoryResponse struct {
	Binhosts []binhostProfile `json:"binhosts"`
}

// handleBinhostInventory publishes the safe, read-only consume paths needed by
// portage-client. It contains no credentials and is public like the binhost.
func (s *Server) handleBinhostInventory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.binpkgMu.RLock()
	binhosts := make([]binhostProfile, 0, len(s.binhostProfiles))
	for _, profile := range s.binhostProfiles {
		binhosts = append(binhosts, profile)
	}
	s.binpkgMu.RUnlock()
	sort.Slice(binhosts, func(i, j int) bool { return binhosts[i].ID < binhosts[j].ID })
	writeJSON(w, binhostInventoryResponse{Binhosts: binhosts})
}
