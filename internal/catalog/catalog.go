// Package catalog owns the server-side mapping from user-facing build IDs to
// immutable infrastructure inputs. Clients name profiles and repositories;
// they never select provider endpoints, VM templates, or repository URLs.
package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxCatalogBytes int64 = 4 << 20

var (
	idPattern     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+._/-]{0,255}$`)
	repoNameRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	revisionRegex = regexp.MustCompile(`^[a-fA-F0-9]{7,64}$`)
	binhostPart   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+._-]{0,127}$`)
)

// Catalog is an immutable, versioned control-plane inventory.
type Catalog struct {
	Version         int                    `json:"version"`
	Profiles        []ProfileDefinition    `json:"profiles"`
	Repositories    []RepositoryDefinition `json:"repositories"`
	Images          []ImageManifest        `json:"images"`
	MirrorBundles   []MirrorBundle         `json:"mirror_bundles"`
	ResourceClasses []ResourceClass        `json:"resource_classes,omitempty"`

	profilesByID      map[string]*ProfileDefinition
	profilesByLegacy  map[string]*ProfileDefinition
	repositoriesByID  map[string]*RepositoryDefinition
	imagesByID        map[string]*ImageManifest
	mirrorBundlesByID map[string]*MirrorBundle
	resourceByID      map[string]*ResourceClass
	defaultProfileID  string
}

// ProfileDefinition maps a stable API ID to a profile, repositories, mirror
// bundle, and image generation approved by the operator.
type ProfileDefinition struct {
	ID                   string                    `json:"id"`
	Arch                 string                    `json:"arch"`
	ProfilePath          string                    `json:"profile_path"`
	BinhostPath          string                    `json:"binhost_path"`
	ProfileRepositoryID  string                    `json:"profile_repository_id"`
	Parents              []ProfileParentDefinition `json:"parents,omitempty"`
	LegacyProfiles       []string                  `json:"legacy_profiles,omitempty"`
	RepositoryIDs        []string                  `json:"repository_ids"`
	ImageID              string                    `json:"image_id"`
	MirrorBundleID       string                    `json:"mirror_bundle_id"`
	DefaultResourceClass string                    `json:"default_resource_class,omitempty"`
	Default              bool                      `json:"default,omitempty"`
	Channel              string                    `json:"channel"`
}

// ProfileParentDefinition binds an external profile to an exact parent
// repository and path. The repository commit is resolved separately from the
// same catalog and must already exist on the selected image generation.
type ProfileParentDefinition struct {
	RepositoryID string `json:"repository_id"`
	ProfilePath  string `json:"profile_path"`
}

// RepositoryDefinition is the only source of repository transport settings
// applied to a builder. User-supplied URLs are never copied into this type.
type RepositoryDefinition struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
	SyncType string `json:"sync_type,omitempty"`
	SyncURI  string `json:"sync_uri,omitempty"`
	Revision string `json:"revision,omitempty"`
	Digest   string `json:"digest,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Channel  string `json:"channel"`
}

// ImageManifest identifies one immutable builder or verifier image generation.
type ImageManifest struct {
	ID                      string   `json:"id"`
	ProfileID               string   `json:"profile_id,omitempty"`
	Generation              string   `json:"generation"`
	Provider                string   `json:"provider"`
	Arch                    string   `json:"arch"`
	BuildMode               string   `json:"build_mode"`
	Template                string   `json:"template,omitempty"`
	Digest                  string   `json:"digest,omitempty"`
	RootfsSource            string   `json:"rootfs_source"`
	DisplayModel            string   `json:"display_model,omitempty"`
	RootfsManifestDigest    string   `json:"rootfs_manifest_digest,omitempty"`
	PackageSetIDs           []string `json:"package_sets,omitempty"`
	PackageSetCatalogDigest string   `json:"package_set_catalog_digest,omitempty"`
	Channel                 string   `json:"channel"`
}

// MirrorBundle identifies the complete set of offline inputs used to produce
// an image or execute a build.
type MirrorBundle struct {
	ID                string    `json:"id"`
	Digest            string    `json:"digest,omitempty"`
	CreatedAt         time.Time `json:"created_at,omitempty"`
	FreshUntil        time.Time `json:"fresh_until,omitempty"`
	AdvisoryWatermark string    `json:"advisory_watermark,omitempty"`
	Channel           string    `json:"channel"`
}

// ResourceClass maps a user-facing size to an operator-approved set of safe
// resource knobs. Endpoint, template, network, storage, and credentials are
// intentionally not representable here.
type ResourceClass struct {
	ID          string            `json:"id"`
	MachineSpec map[string]string `json:"machine_spec"`
}

// ResolveRequest contains the untrusted identifiers accepted from a build
// request. Resolve returns only server-owned values.
type ResolveRequest struct {
	ProfileID     string
	LegacyProfile string
	Arch          string
	RepositoryIDs []string
	ResourceClass string
	CloudProvider string
}

// ResolvedBuildContext is stored on the job and passed through the control
// plane as the authoritative environment selected for the build.
type ResolvedBuildContext struct {
	CatalogVersion          int                     `json:"catalog_version"`
	ProfileID               string                  `json:"profile_id"`
	ProfilePath             string                  `json:"profile_path"`
	BinhostPath             string                  `json:"binhost_path"`
	ProfileRepositoryID     string                  `json:"profile_repository_id"`
	ProfileRepositoryName   string                  `json:"profile_repository_name"`
	ProfileParents          []ResolvedProfileParent `json:"profile_parents,omitempty"`
	ProfileChannel          string                  `json:"profile_channel"`
	Arch                    string                  `json:"arch"`
	Provider                string                  `json:"provider"`
	BuildMode               string                  `json:"build_mode"`
	ImageID                 string                  `json:"image_id"`
	ImageGeneration         string                  `json:"image_generation"`
	ImageDigest             string                  `json:"image_digest,omitempty"`
	ImageChannel            string                  `json:"image_channel"`
	Template                string                  `json:"template,omitempty"`
	RootfsSource            string                  `json:"rootfs_source"`
	DisplayModel            string                  `json:"display_model,omitempty"`
	RootfsManifestDigest    string                  `json:"rootfs_manifest_digest,omitempty"`
	PackageSetIDs           []string                `json:"package_sets,omitempty"`
	PackageSetCatalogDigest string                  `json:"package_set_catalog_digest,omitempty"`
	MirrorBundleID          string                  `json:"mirror_bundle_id"`
	MirrorBundleDigest      string                  `json:"mirror_bundle_digest,omitempty"`
	MirrorBundleChannel     string                  `json:"mirror_bundle_channel"`
	ResourceClass           string                  `json:"resource_class,omitempty"`
	MachineSpec             map[string]string       `json:"machine_spec,omitempty"`
	Repositories            []ResolvedRepository    `json:"repositories"`
	ResolvedAt              time.Time               `json:"resolved_at"`
}

// ResolvedProfileParent is execution metadata derived only from catalog
// repository IDs. Clients cannot provide repository names or paths here.
type ResolvedProfileParent struct {
	RepositoryID   string `json:"repository_id"`
	RepositoryName string `json:"repository_name"`
	ProfilePath    string `json:"profile_path"`
}

// ResolvedRepository is the exact repository configuration authorized by the
// server catalog.
type ResolvedRepository struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
	SyncType string `json:"sync_type,omitempty"`
	SyncURI  string `json:"sync_uri,omitempty"`
	Revision string `json:"revision,omitempty"`
	Digest   string `json:"digest,omitempty"`
	Priority int    `json:"priority,omitempty"`
}

// CompatibilityOptions creates an explicit legacy catalog for deployments
// which have not configured CATALOG_PATH yet.
type CompatibilityOptions struct {
	Provider       string
	BuildMode      string
	Template       string
	GentooSyncType string
	GentooSyncURI  string
}

// Load reads and strictly validates a catalog JSON file.
func Load(path string) (*Catalog, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-configured catalog path.
	if err != nil {
		return nil, fmt.Errorf("open catalog: %w", err)
	}
	defer func() { _ = f.Close() }()

	dec := json.NewDecoder(io.LimitReader(f, maxCatalogBytes+1))
	dec.DisallowUnknownFields()
	var c Catalog
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode catalog: multiple JSON values")
		}
		return nil, fmt.Errorf("decode catalog trailer: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// NewCompatibility returns a catalog that preserves the current single-image
// behavior while making the unresolved trust level visible in job provenance.
func NewCompatibility(opts CompatibilityOptions) *Catalog {
	provider := strings.TrimSpace(opts.Provider)
	if provider == "" {
		provider = "pve"
	}
	buildMode := "native-gentoo"
	syncType := strings.TrimSpace(opts.GentooSyncType)
	syncURI := strings.TrimSpace(opts.GentooSyncURI)
	if syncURI == "" {
		// The existing webrsync path is configured outside repos.conf. Do not
		// manufacture a half-configured repository entry for compatibility mode.
		syncType = ""
	} else if syncType == "" {
		if strings.HasPrefix(syncURI, "rsync://") {
			syncType = "rsync"
		} else {
			syncType = "git"
		}
	}

	c := &Catalog{
		Version: 1,
		Profiles: []ProfileDefinition{{
			ID:                   "compat/amd64/default",
			Arch:                 "amd64",
			ProfilePath:          "default/linux/amd64/23.0",
			BinhostPath:          "releases/amd64/binpackages/23.0/x86-64",
			ProfileRepositoryID:  "gentoo",
			LegacyProfiles:       []string{"default/linux/amd64/23.0"},
			RepositoryIDs:        []string{"gentoo"},
			ImageID:              "compat/current-image",
			MirrorBundleID:       "compat/current-mirrors",
			DefaultResourceClass: "medium",
			Default:              true,
			Channel:              "compatibility",
		}},
		Repositories: []RepositoryDefinition{{
			ID:       "gentoo",
			Name:     "gentoo",
			Location: "/var/db/repos/gentoo",
			SyncType: syncType,
			SyncURI:  syncURI,
			Channel:  "compatibility",
		}},
		Images: []ImageManifest{{
			ID:           "compat/current-image",
			ProfileID:    "compat/amd64/default",
			Generation:   "current",
			Provider:     provider,
			Arch:         "amd64",
			BuildMode:    buildMode,
			Template:     strings.TrimSpace(opts.Template),
			RootfsSource: "existing-template",
			Channel:      "compatibility",
		}},
		MirrorBundles: []MirrorBundle{{
			ID:      "compat/current-mirrors",
			Channel: "compatibility",
		}},
		ResourceClasses: []ResourceClass{{
			ID: "medium",
			MachineSpec: map[string]string{
				"cores":     "4",
				"memory":    "8192",
				"disk_size": "50",
			},
		}},
	}
	if err := c.Validate(); err != nil {
		panic(fmt.Sprintf("invalid built-in compatibility catalog: %v", err))
	}
	return c
}

// Validate checks IDs, references, trust metadata, and the uniqueness of the
// default profile, and constructs read-only lookup indexes.
func (c *Catalog) Validate() error {
	if c == nil {
		return fmt.Errorf("nil catalog")
	}
	if c.Version < 1 {
		return fmt.Errorf("catalog version must be positive")
	}
	if len(c.Profiles) == 0 || len(c.Images) == 0 || len(c.MirrorBundles) == 0 {
		return fmt.Errorf("catalog requires profiles, images, and mirror_bundles")
	}

	c.profilesByID = make(map[string]*ProfileDefinition, len(c.Profiles))
	c.profilesByLegacy = make(map[string]*ProfileDefinition)
	c.repositoriesByID = make(map[string]*RepositoryDefinition, len(c.Repositories))
	c.imagesByID = make(map[string]*ImageManifest, len(c.Images))
	c.mirrorBundlesByID = make(map[string]*MirrorBundle, len(c.MirrorBundles))
	c.resourceByID = make(map[string]*ResourceClass, len(c.ResourceClasses))
	c.defaultProfileID = ""
	binhostOwners := make(map[string]string, len(c.Profiles))

	for i := range c.Repositories {
		r := &c.Repositories[i]
		if err := validateRepository(r); err != nil {
			return fmt.Errorf("repository %q: %w", r.ID, err)
		}
		if _, exists := c.repositoriesByID[r.ID]; exists {
			return fmt.Errorf("duplicate repository ID %q", r.ID)
		}
		c.repositoriesByID[r.ID] = r
	}
	for i := range c.Images {
		image := &c.Images[i]
		if err := validateImage(image); err != nil {
			return fmt.Errorf("image %q: %w", image.ID, err)
		}
		if _, exists := c.imagesByID[image.ID]; exists {
			return fmt.Errorf("duplicate image ID %q", image.ID)
		}
		c.imagesByID[image.ID] = image
	}
	for i := range c.MirrorBundles {
		bundle := &c.MirrorBundles[i]
		if err := validateMirrorBundle(bundle); err != nil {
			return fmt.Errorf("mirror bundle %q: %w", bundle.ID, err)
		}
		if _, exists := c.mirrorBundlesByID[bundle.ID]; exists {
			return fmt.Errorf("duplicate mirror bundle ID %q", bundle.ID)
		}
		c.mirrorBundlesByID[bundle.ID] = bundle
	}
	for i := range c.ResourceClasses {
		class := &c.ResourceClasses[i]
		if !idPattern.MatchString(class.ID) {
			return fmt.Errorf("invalid resource class ID %q", class.ID)
		}
		if _, exists := c.resourceByID[class.ID]; exists {
			return fmt.Errorf("duplicate resource class ID %q", class.ID)
		}
		if err := validateResourceSpec(class.MachineSpec); err != nil {
			return fmt.Errorf("resource class %q: %w", class.ID, err)
		}
		c.resourceByID[class.ID] = class
	}

	for i := range c.Profiles {
		profile := &c.Profiles[i]
		if err := c.validateProfile(profile); err != nil {
			return fmt.Errorf("profile %q: %w", profile.ID, err)
		}
		if _, exists := c.profilesByID[profile.ID]; exists {
			return fmt.Errorf("duplicate profile ID %q", profile.ID)
		}
		c.profilesByID[profile.ID] = profile
		if owner, exists := binhostOwners[profile.BinhostPath]; exists {
			return fmt.Errorf("binhost path %q is shared by profiles %q and %q", profile.BinhostPath, owner, profile.ID)
		}
		binhostOwners[profile.BinhostPath] = profile.ID
		for _, legacy := range profile.LegacyProfiles {
			if other, exists := c.profilesByLegacy[legacy]; exists {
				return fmt.Errorf("legacy profile %q maps to both %q and %q", legacy, other.ID, profile.ID)
			}
			c.profilesByLegacy[legacy] = profile
		}
		if profile.Default {
			if c.defaultProfileID != "" {
				return fmt.Errorf("multiple default profiles: %q and %q", c.defaultProfileID, profile.ID)
			}
			c.defaultProfileID = profile.ID
		}
	}
	if c.defaultProfileID == "" {
		return fmt.Errorf("catalog requires exactly one default profile")
	}
	return nil
}

// Resolve converts untrusted IDs into an immutable server-owned build context.
func (c *Catalog) Resolve(req ResolveRequest) (*ResolvedBuildContext, error) {
	return c.ResolveAt(req, time.Now().UTC())
}

// ResolveAt resolves a profile while enforcing the mirror freshness gate at a
// caller-supplied time. Keeping the clock explicit makes promotion and tests
// deterministic while Resolve remains the production entry point.
func (c *Catalog) ResolveAt(req ResolveRequest, now time.Time) (*ResolvedBuildContext, error) {
	if c == nil || c.profilesByID == nil {
		return nil, fmt.Errorf("catalog is not initialized")
	}
	profile, err := c.resolveProfile(req.ProfileID, req.LegacyProfile)
	if err != nil {
		return nil, err
	}
	if profile.Channel != "stable" && profile.Channel != "compatibility" {
		return nil, fmt.Errorf("profile %q is not published on the stable channel", profile.ID)
	}
	if req.Arch != "" && req.Arch != profile.Arch {
		return nil, fmt.Errorf("architecture %q does not match profile %q architecture %q", req.Arch, profile.ID, profile.Arch)
	}
	image := c.imagesByID[profile.ImageID]
	if req.CloudProvider != "" && req.CloudProvider != image.Provider {
		return nil, fmt.Errorf("provider %q does not match profile %q provider %q", req.CloudProvider, profile.ID, image.Provider)
	}
	bundle := c.mirrorBundlesByID[profile.MirrorBundleID]
	if profile.Channel != "compatibility" && (bundle.FreshUntil.IsZero() || !now.Before(bundle.FreshUntil)) {
		return nil, fmt.Errorf("mirror bundle %q is stale at %s", bundle.ID, now.UTC().Format(time.RFC3339))
	}
	profileRepository := c.repositoriesByID[profile.ProfileRepositoryID]
	profileParents := make([]ResolvedProfileParent, 0, len(profile.Parents))
	for _, parent := range profile.Parents {
		repository := c.repositoriesByID[parent.RepositoryID]
		profileParents = append(profileParents, ResolvedProfileParent{RepositoryID: repository.ID, RepositoryName: repository.Name, ProfilePath: parent.ProfilePath})
	}

	repositoryIDs := append([]string(nil), profile.RepositoryIDs...)
	// Assembly may revision-scope a catalog repository ID so two images in one
	// release can bind different commits of the same logical Portage repository.
	// Preserve the existing user-facing request shape by accepting either that
	// exact immutable ID or the repository Name selected by this profile.
	for _, requested := range req.RepositoryIDs {
		resolvedID := requested
		if !contains(profile.RepositoryIDs, requested) {
			resolvedID = ""
			for _, allowedID := range profile.RepositoryIDs {
				repository := c.repositoriesByID[allowedID]
				if repository.Name == requested {
					if resolvedID != "" {
						return nil, fmt.Errorf("repository name %q is ambiguous for profile %q", requested, profile.ID)
					}
					resolvedID = allowedID
				}
			}
			if resolvedID == "" {
				return nil, fmt.Errorf("repository %q is not allowed by profile %q", requested, profile.ID)
			}
		}
		repositoryIDs = append(repositoryIDs, resolvedID)
	}
	repositoryIDs = uniqueSorted(repositoryIDs)
	repositories := make([]ResolvedRepository, 0, len(repositoryIDs))
	allowed := make(map[string]struct{}, len(profile.RepositoryIDs))
	for _, id := range profile.RepositoryIDs {
		allowed[id] = struct{}{}
	}
	for _, id := range repositoryIDs {
		if _, ok := allowed[id]; !ok {
			return nil, fmt.Errorf("repository %q is not allowed by profile %q", id, profile.ID)
		}
		repo, ok := c.repositoriesByID[id]
		if !ok {
			return nil, fmt.Errorf("repository %q is not registered", id)
		}
		repositories = append(repositories, ResolvedRepository{
			ID: repo.ID, Name: repo.Name, Location: repo.Location,
			SyncType: repo.SyncType, SyncURI: repo.SyncURI,
			Revision: repo.Revision, Digest: repo.Digest, Priority: repo.Priority,
		})
	}

	resourceClass := strings.TrimSpace(req.ResourceClass)
	if resourceClass == "" {
		resourceClass = profile.DefaultResourceClass
	}
	machineSpec := map[string]string{}
	if resourceClass != "" {
		class, ok := c.resourceByID[resourceClass]
		if !ok {
			return nil, fmt.Errorf("resource class %q is not registered", resourceClass)
		}
		for key, value := range class.MachineSpec {
			machineSpec[key] = value
		}
	}

	return &ResolvedBuildContext{
		CatalogVersion:          c.Version,
		ProfileID:               profile.ID,
		ProfilePath:             profile.ProfilePath,
		BinhostPath:             profile.BinhostPath,
		ProfileRepositoryID:     profileRepository.ID,
		ProfileRepositoryName:   profileRepository.Name,
		ProfileParents:          profileParents,
		ProfileChannel:          profile.Channel,
		Arch:                    profile.Arch,
		Provider:                image.Provider,
		BuildMode:               image.BuildMode,
		ImageID:                 image.ID,
		ImageGeneration:         image.Generation,
		ImageDigest:             image.Digest,
		ImageChannel:            image.Channel,
		Template:                image.Template,
		RootfsSource:            image.RootfsSource,
		DisplayModel:            image.DisplayModel,
		RootfsManifestDigest:    image.RootfsManifestDigest,
		PackageSetIDs:           append([]string(nil), image.PackageSetIDs...),
		PackageSetCatalogDigest: image.PackageSetCatalogDigest,
		MirrorBundleID:          bundle.ID,
		MirrorBundleDigest:      bundle.Digest,
		MirrorBundleChannel:     bundle.Channel,
		ResourceClass:           resourceClass,
		MachineSpec:             machineSpec,
		Repositories:            repositories,
		ResolvedAt:              now.UTC(),
	}, nil
}

func (c *Catalog) resolveProfile(id, legacy string) (*ProfileDefinition, error) {
	if id != "" {
		profile, ok := c.profilesByID[id]
		if !ok {
			return nil, fmt.Errorf("profile ID %q is not registered", id)
		}
		if legacy != "" && legacy != profile.ProfilePath && !contains(profile.LegacyProfiles, legacy) {
			return nil, fmt.Errorf("legacy profile %q does not match profile ID %q", legacy, id)
		}
		return profile, nil
	}
	if legacy != "" {
		profile, ok := c.profilesByLegacy[legacy]
		if !ok {
			return nil, fmt.Errorf("legacy profile %q is not registered", legacy)
		}
		return profile, nil
	}
	return c.profilesByID[c.defaultProfileID], nil
}

func (c *Catalog) validateProfile(p *ProfileDefinition) error {
	if !idPattern.MatchString(p.ID) || !idPattern.MatchString(p.ProfilePath) {
		return fmt.Errorf("invalid ID or profile_path")
	}
	if p.Arch == "" || strings.ContainsAny(p.Arch, "/\\") {
		return fmt.Errorf("invalid architecture %q", p.Arch)
	}
	if err := ValidateBinhostPath(p.BinhostPath, p.Arch); err != nil {
		return err
	}
	if !validChannel(p.Channel) {
		return fmt.Errorf("invalid channel %q", p.Channel)
	}
	if _, ok := c.imagesByID[p.ImageID]; !ok {
		return fmt.Errorf("unknown image %q", p.ImageID)
	}
	if c.imagesByID[p.ImageID].Arch != p.Arch {
		return fmt.Errorf("image %q architecture does not match", p.ImageID)
	}
	if c.imagesByID[p.ImageID].ProfileID != p.ID {
		return fmt.Errorf("image %q is not bound to this profile", p.ImageID)
	}
	if c.imagesByID[p.ImageID].Channel != p.Channel {
		return fmt.Errorf("image %q channel does not match profile channel", p.ImageID)
	}
	if _, ok := c.mirrorBundlesByID[p.MirrorBundleID]; !ok {
		return fmt.Errorf("unknown mirror bundle %q", p.MirrorBundleID)
	}
	if c.mirrorBundlesByID[p.MirrorBundleID].Channel != p.Channel {
		return fmt.Errorf("mirror bundle %q channel does not match profile channel", p.MirrorBundleID)
	}
	seenRepositoryNames := make(map[string]struct{}, len(p.RepositoryIDs))
	for _, id := range p.RepositoryIDs {
		repository, ok := c.repositoriesByID[id]
		if !ok {
			return fmt.Errorf("unknown repository %q", id)
		}
		if repository.Channel != p.Channel {
			return fmt.Errorf("repository %q channel does not match profile channel", id)
		}
		if _, duplicate := seenRepositoryNames[repository.Name]; duplicate {
			return fmt.Errorf("profile selects multiple revisions of repository %q", repository.Name)
		}
		seenRepositoryNames[repository.Name] = struct{}{}
	}
	profileRepository, ok := c.repositoriesByID[p.ProfileRepositoryID]
	if !ok || !contains(p.RepositoryIDs, p.ProfileRepositoryID) {
		return fmt.Errorf("profile_repository_id must name an allowed repository")
	}
	if p.Channel != "compatibility" && (profileRepository.SyncType != "git" || (len(profileRepository.Revision) != 40 && len(profileRepository.Revision) != 64)) {
		return fmt.Errorf("profile repository requires a full immutable git commit")
	}
	if profileRepository.Name != "gentoo" && p.Channel != "compatibility" && len(p.Parents) == 0 {
		return fmt.Errorf("external profile requires an explicit parent chain")
	}
	seenParents := make(map[string]struct{}, len(p.Parents))
	for _, parent := range p.Parents {
		if !idPattern.MatchString(parent.RepositoryID) || !idPattern.MatchString(parent.ProfilePath) || strings.HasPrefix(parent.ProfilePath, "/") {
			return fmt.Errorf("invalid parent profile")
		}
		repository, exists := c.repositoriesByID[parent.RepositoryID]
		if !exists || !contains(p.RepositoryIDs, parent.RepositoryID) {
			return fmt.Errorf("parent repository %q is not allowed by the profile", parent.RepositoryID)
		}
		if p.Channel != "compatibility" && (repository.SyncType != "git" || (len(repository.Revision) != 40 && len(repository.Revision) != 64)) {
			return fmt.Errorf("parent repository %q requires a full immutable git commit", parent.RepositoryID)
		}
		key := parent.RepositoryID + ":" + parent.ProfilePath
		if _, duplicate := seenParents[key]; duplicate {
			return fmt.Errorf("duplicate parent profile %q", key)
		}
		seenParents[key] = struct{}{}
	}
	if p.DefaultResourceClass != "" {
		if _, ok := c.resourceByID[p.DefaultResourceClass]; !ok {
			return fmt.Errorf("unknown resource class %q", p.DefaultResourceClass)
		}
	}
	for _, legacy := range p.LegacyProfiles {
		if !idPattern.MatchString(legacy) {
			return fmt.Errorf("invalid legacy profile %q", legacy)
		}
	}
	return nil
}

// ValidateBinhostPath enforces the same namespace shape used by Gentoo's
// official binary package service:
//
//	releases/<arch>/binpackages/<profile-generation>/<target>
//
// A Packages index lives at that root and its PATH entries remain relative
// category/package paths. The profile catalog owns this value so two ABI,
// libc, CPU-level, or policy variants can never be mixed by inference.
func ValidateBinhostPath(value, arch string) error {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") ||
		path.Clean(value) != value {
		return fmt.Errorf("invalid binhost_path %q", value)
	}
	parts := strings.Split(value, "/")
	if len(parts) != 5 || parts[0] != "releases" || parts[1] != arch || parts[2] != "binpackages" {
		return fmt.Errorf("binhost_path must be releases/%s/binpackages/<profile-generation>/<target>", arch)
	}
	if !binhostPart.MatchString(parts[1]) || !binhostPart.MatchString(parts[3]) || !binhostPart.MatchString(parts[4]) {
		return fmt.Errorf("binhost_path contains an invalid component")
	}
	return nil
}

func validateRepository(r *RepositoryDefinition) error {
	if !idPattern.MatchString(r.ID) || !repoNameRegex.MatchString(r.Name) {
		return fmt.Errorf("invalid ID or name")
	}
	expected := filepath.Join("/var/db/repos", r.Name)
	if r.Location == "" {
		r.Location = expected
	}
	if filepath.Clean(r.Location) != expected {
		return fmt.Errorf("location must be %q", expected)
	}
	if !validChannel(r.Channel) {
		return fmt.Errorf("invalid channel %q", r.Channel)
	}
	switch r.SyncType {
	case "", "git", "rsync", "webrsync":
	default:
		return fmt.Errorf("unsupported sync type %q", r.SyncType)
	}
	if r.SyncURI == "" && r.SyncType != "" {
		return fmt.Errorf("sync_uri is required with sync_type")
	}
	if r.SyncURI != "" {
		u, err := url.Parse(r.SyncURI)
		if err != nil || u.Fragment != "" || u.RawQuery != "" || u.User != nil {
			return fmt.Errorf("invalid sync URI")
		}
		switch u.Scheme {
		case "https":
			if u.Host == "" {
				return fmt.Errorf("sync URI requires a host")
			}
			if r.SyncType == "rsync" {
				return fmt.Errorf("rsync repository requires rsync://")
			}
		case "http":
			// Plain HTTP is permitted only as an integrity-protected artifact
			// plane: the catalog must bind both the full Git revision and the
			// repository snapshot digest. Credentials and query strings were
			// already rejected above.
			if u.Host == "" || r.SyncType != "git" || (len(r.Revision) != 40 && len(r.Revision) != 64) || r.Digest == "" {
				return fmt.Errorf("HTTP git mirror requires host, full revision, and snapshot digest")
			}
		case "rsync":
			if u.Host == "" {
				return fmt.Errorf("sync URI requires a host")
			}
			if r.SyncType != "rsync" {
				return fmt.Errorf("rsync:// requires rsync sync_type")
			}
		default:
			return fmt.Errorf("unsupported sync URI scheme %q", u.Scheme)
		}
		if r.SyncType == "" {
			return fmt.Errorf("sync_type is required with sync_uri")
		}
	}
	if r.Revision != "" && !revisionRegex.MatchString(r.Revision) {
		return fmt.Errorf("revision must be a 7..64 character hexadecimal commit")
	}
	if err := validateDigest(r.Digest); err != nil {
		return err
	}
	if r.Channel == "stable" {
		if r.SyncType != "git" {
			return fmt.Errorf("stable repositories must use git until snapshot digest verification is implemented")
		}
		if len(r.Revision) != 40 && len(r.Revision) != 64 {
			return fmt.Errorf("stable git repository requires a full 40- or 64-character commit")
		}
	}
	return nil
}

func validateImage(image *ImageManifest) error {
	if !idPattern.MatchString(image.ID) || image.Generation == "" || image.Provider == "" || image.Arch == "" || image.BuildMode == "" || image.RootfsSource == "" {
		return fmt.Errorf("missing or invalid required field")
	}
	if image.DisplayModel != "" && image.DisplayModel != "std" && image.DisplayModel != "qxl" {
		return fmt.Errorf("image %q has unsupported display_model %q", image.ID, image.DisplayModel)
	}
	if !validChannel(image.Channel) {
		return fmt.Errorf("invalid channel %q", image.Channel)
	}
	if image.ProfileID != "" && !idPattern.MatchString(image.ProfileID) {
		return fmt.Errorf("invalid profile_id")
	}
	if image.Channel != "compatibility" && image.ProfileID == "" {
		return fmt.Errorf("candidate/stable image requires profile_id binding")
	}
	switch image.Provider {
	case "pve", "gcp", "aws":
	default:
		return fmt.Errorf("unsupported provider %q", image.Provider)
	}
	if image.BuildMode != "native-gentoo" {
		return fmt.Errorf("unsupported build mode %q", image.BuildMode)
	}
	if err := validateDigest(image.Digest); err != nil {
		return err
	}
	if err := validateDigest(image.RootfsManifestDigest); err != nil {
		return err
	}
	if err := validateDigest(image.PackageSetCatalogDigest); err != nil {
		return err
	}
	if (len(image.PackageSetIDs) == 0) != (image.PackageSetCatalogDigest == "") {
		return fmt.Errorf("package sets and package-set catalog digest must be provided together")
	}
	seenSets := make(map[string]struct{}, len(image.PackageSetIDs))
	for _, id := range image.PackageSetIDs {
		if !idPattern.MatchString(id) {
			return fmt.Errorf("invalid package set ID %q", id)
		}
		if _, exists := seenSets[id]; exists {
			return fmt.Errorf("duplicate package set ID %q", id)
		}
		seenSets[id] = struct{}{}
	}
	if image.Channel == "stable" && (image.Template == "" || image.Digest == "") {
		return fmt.Errorf("stable image requires template and digest")
	}
	return nil
}

func validateMirrorBundle(bundle *MirrorBundle) error {
	if !idPattern.MatchString(bundle.ID) || !validChannel(bundle.Channel) {
		return fmt.Errorf("invalid ID or channel")
	}
	if err := validateDigest(bundle.Digest); err != nil {
		return err
	}
	if bundle.Channel == "stable" && (bundle.Digest == "" || bundle.CreatedAt.IsZero() || bundle.FreshUntil.IsZero() || bundle.AdvisoryWatermark == "") {
		return fmt.Errorf("stable mirror bundle requires digest, freshness window, and advisory watermark")
	}
	if !bundle.FreshUntil.IsZero() && !bundle.CreatedAt.IsZero() && bundle.FreshUntil.Before(bundle.CreatedAt) {
		return fmt.Errorf("fresh_until precedes created_at")
	}
	return nil
}

func validateDigest(digest string) error {
	if digest != "" && !digestPattern.MatchString(digest) {
		return fmt.Errorf("invalid SHA-256 digest %q", digest)
	}
	return nil
}

func validateResourceSpec(spec map[string]string) error {
	for key, value := range spec {
		if value == "" || len(value) > 128 || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("invalid value for machine spec key %q", key)
		}
		switch key {
		case "cores":
			if err := validateBoundedInteger(value, 1, 128, key); err != nil {
				return err
			}
		case "memory":
			if err := validateBoundedInteger(value, 512, 1<<20, key); err != nil {
				return err
			}
		case "disk_size":
			if err := validateBoundedInteger(value, 10, 4096, key); err != nil {
				return err
			}
		case "machine_type", "instance_type":
			if !repoNameRegex.MatchString(value) {
				return fmt.Errorf("invalid value for machine spec key %q", key)
			}
		case "preemptible":
			if value != "true" && value != "false" {
				return fmt.Errorf("invalid value for machine spec key %q", key)
			}
		default:
			return fmt.Errorf("machine spec key %q is operator-owned or unsupported", key)
		}
	}
	return nil
}

func validateBoundedInteger(value string, minValue, maxValue int, key string) error {
	n, err := strconv.Atoi(value)
	if err != nil || n < minValue || n > maxValue {
		return fmt.Errorf("machine spec %s must be between %d and %d", key, minValue, maxValue)
	}
	return nil
}

func validChannel(channel string) bool {
	switch channel {
	case "compatibility", "candidate", "stable", "retired":
		return true
	default:
		return false
	}
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
