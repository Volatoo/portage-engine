package server

import (
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/slchris/portage-engine/internal/binpkg"
)

type binhostInventoryResponse struct {
	Binhosts []binhostProfile `json:"binhosts"`
}

type publicPackage struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Arch         string   `json:"arch"`
	UseFlags     []string `json:"use_flags,omitempty"`
	ProfileID    string   `json:"profile_id"`
	ProfilePath  string   `json:"profile_path"`
	Channel      string   `json:"channel,omitempty"`
	BinhostPath  string   `json:"binhost_path"`
	DownloadPath string   `json:"download_path"`
}

type publicPackageResponse struct {
	Packages []publicPackage `json:"packages"`
	Total    int             `json:"total"`
	Limit    int             `json:"limit"`
	Offset   int             `json:"offset"`
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

// handlePublicPackages lists only published binpkg metadata. Build requests,
// internal package metadata, job ownership, and storage credentials remain on
// authenticated endpoints.
func (s *Server) handlePublicPackages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if len(query) > 200 {
		http.Error(w, "q must be at most 200 characters", http.StatusBadRequest)
		return
	}
	profileFilter := strings.TrimSpace(r.URL.Query().Get("profile_id"))
	archFilter := strings.TrimSpace(r.URL.Query().Get("arch"))
	limit, ok := publicPackagePageValue(r, "limit", 50, 1, 200)
	if !ok {
		http.Error(w, "limit must be between 1 and 200", http.StatusBadRequest)
		return
	}
	offset, ok := publicPackagePageValue(r, "offset", 0, 0, 1_000_000)
	if !ok {
		http.Error(w, "offset must be between 0 and 1000000", http.StatusBadRequest)
		return
	}

	s.binpkgMu.RLock()
	profiles := make([]binhostProfile, 0, len(s.binhostProfiles))
	stores := make(map[string]*binpkg.Store, len(s.binpkgStores))
	for _, profile := range s.binhostProfiles {
		profiles = append(profiles, profile)
	}
	for binhostPath, store := range s.binpkgStores {
		stores[binhostPath] = store
	}
	legacy := s.binpkgStore
	s.binpkgMu.RUnlock()

	// New(cfg) without Initialize is used by focused tests and trusted embedded
	// callers. Preserve a safe default-profile view for that compatibility mode.
	if len(profiles) == 0 && legacy != nil {
		profiles = append(profiles, binhostProfile{
			ID: "default", Arch: "amd64", Default: true, SyncPath: "/binpkgs",
		})
		stores[""] = legacy
	}

	if profileFilter != "" {
		found := false
		for _, profile := range profiles {
			if profile.ID == profileFilter {
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "unknown profile_id", http.StatusBadRequest)
			return
		}
	}

	packages := make([]publicPackage, 0)
	for _, profile := range profiles {
		if profileFilter != "" && profile.ID != profileFilter {
			continue
		}
		if archFilter != "" && profile.Arch != archFilter {
			continue
		}
		store := stores[profile.BinhostPath]
		if store == nil {
			continue
		}
		for _, pkg := range store.Snapshot() {
			searchable := strings.ToLower(strings.Join([]string{
				pkg.Name, pkg.Version, pkg.Arch, profile.ID, profile.ProfilePath,
			}, " "))
			if query != "" && !strings.Contains(searchable, query) {
				continue
			}
			packages = append(packages, publicPackage{
				Name: pkg.Name, Version: pkg.Version, Arch: pkg.Arch,
				UseFlags: pkg.UseFlags, ProfileID: profile.ID,
				ProfilePath: profile.ProfilePath, Channel: profile.Channel,
				BinhostPath:  profile.BinhostPath,
				DownloadPath: path.Join("/binpkgs", profile.BinhostPath, pkg.Path),
			})
		}
	}
	sort.Slice(packages, func(i, j int) bool {
		left, right := packages[i], packages[j]
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.Version != right.Version {
			return left.Version > right.Version
		}
		if left.ProfileID != right.ProfileID {
			return left.ProfileID < right.ProfileID
		}
		return left.DownloadPath < right.DownloadPath
	})

	total := len(packages)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	writeJSON(w, publicPackageResponse{
		Packages: packages[offset:end], Total: total, Limit: limit, Offset: offset,
	})
}

func publicPackagePageValue(
	r *http.Request, name string, fallback, minimum, maximum int,
) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(raw)
	return value, err == nil && value >= minimum && value <= maximum
}
