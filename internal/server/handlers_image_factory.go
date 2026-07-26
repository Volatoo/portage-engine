package server

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/internal/imagefactory"
)

type imageFactoryProfileView struct {
	ID          string   `json:"id"`
	Arch        string   `json:"arch"`
	ProfilePath string   `json:"profile_path"`
	BinhostPath string   `json:"binhost_path"`
	ImageID     string   `json:"image_id"`
	Channel     string   `json:"channel"`
	Default     bool     `json:"default"`
	PackageSets []string `json:"package_sets,omitempty"`
}

type imageFactoryCatalogView struct {
	Version       int                       `json:"version"`
	Profiles      []imageFactoryProfileView `json:"profiles"`
	Images        []catalog.ImageManifest   `json:"images"`
	MirrorBundles []catalog.MirrorBundle    `json:"mirror_bundles"`
}

type imageFactoryStatusResponse struct {
	Configured bool                        `json:"configured"`
	Catalog    imageFactoryCatalogView     `json:"catalog"`
	Status     *imagefactory.FactoryStatus `json:"status,omitempty"`
	Message    string                      `json:"message,omitempty"`
}

func imageFactoryCatalog(c *catalog.Catalog) imageFactoryCatalogView {
	view := imageFactoryCatalogView{}
	if c == nil {
		return view
	}
	view.Version = c.Version
	view.Images = append([]catalog.ImageManifest(nil), c.Images...)
	view.MirrorBundles = append([]catalog.MirrorBundle(nil), c.MirrorBundles...)
	images := make(map[string]catalog.ImageManifest, len(c.Images))
	for _, image := range c.Images {
		images[image.ID] = image
	}
	for _, profile := range c.Profiles {
		item := imageFactoryProfileView{ID: profile.ID, Arch: profile.Arch, ProfilePath: profile.ProfilePath, BinhostPath: profile.BinhostPath, ImageID: profile.ImageID, Channel: profile.Channel, Default: profile.Default}
		item.PackageSets = append([]string(nil), images[profile.ImageID].PackageSetIDs...)
		view.Profiles = append(view.Profiles, item)
	}
	sort.Slice(view.Profiles, func(i, j int) bool { return view.Profiles[i].ID < view.Profiles[j].ID })
	return view
}

// handleImageFactoryStatus exposes immutable catalog inventory and optional
// operator-produced milestone evidence. It never talks to PVE/PBS or exposes
// credentials, and deliberately offers no mutation endpoint.
func (s *Server) handleImageFactoryStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response := imageFactoryStatusResponse{Catalog: imageFactoryCatalog(s.builder.BuildCatalog())}
	path := strings.TrimSpace(s.config.ImageFactoryStatusPath)
	if path == "" {
		response.Message = "IMAGE_FACTORY_STATUS_PATH is not configured"
		writeJSON(w, response)
		return
	}
	status, err := imagefactory.LoadFactoryStatus(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if s.jobLedger != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		err = s.jobLedger.RecordFactoryStatus(ctx, path, status)
		cancel()
		if err != nil {
			http.Error(w, "persist image-factory status: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	response.Configured = true
	response.Status = status
	writeJSON(w, response)
}

func (s *Server) syncFactoryStatus() error {
	path := strings.TrimSpace(s.config.ImageFactoryStatusPath)
	if path == "" || s.jobLedger == nil {
		return nil
	}
	status, err := imagefactory.LoadFactoryStatus(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.jobLedger.RecordFactoryStatus(ctx, path, status)
}
