package imagefactory

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

var repoComponentPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+._-]{0,127}$`)
var templateNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

const strictTerraformCLIConfig = `provider_installation {
  filesystem_mirror {
    path    = "__PORTAGE_ENGINE_OFFLINE_TERRAFORM_PROVIDERS__"
    include = ["registry.terraform.io/telmate/proxmox"]
  }
}

disable_checkpoint = true`

// CommonConfig contains site-local, non-secret PVE settings. Credentials are
// supplied through PKR_VAR_proxmox_username and PKR_VAR_proxmox_token only.
type CommonConfig struct {
	SchemaVersion   int    `json:"schema_version"`
	ProxmoxURL      string `json:"proxmox_url"`
	ProxmoxNode     string `json:"proxmox_node"`
	ProxmoxPool     string `json:"proxmox_pool"`
	ProxmoxStorage  string `json:"proxmox_storage"`
	ProxmoxBridge   string `json:"proxmox_bridge"`
	ProxmoxInsecure bool   `json:"proxmox_insecure"`
	// ProxmoxHostMemoryHeadroomMB is reserved in addition to the candidate VM's
	// configured memory. A zero value keeps compatibility with older site files;
	// production image-factory runners should set an explicit non-zero guard.
	ProxmoxHostMemoryHeadroomMB int    `json:"proxmox_host_memory_headroom_mb,omitempty"`
	SSHUsername                 string `json:"ssh_username"`
	SSHPrivateKeyFile           string `json:"ssh_private_key_file"`
}

// BuildPlan is the single reviewed source of truth for one image target.
// Packer variables and the final image manifest are derived from this file.
type BuildPlan struct {
	SchemaVersion                int                     `json:"schema_version"`
	Target                       string                  `json:"target"`
	PlanObjectID                 string                  `json:"plan_object_id"`
	ImageID                      string                  `json:"image_id"`
	Generation                   string                  `json:"generation"`
	Provider                     string                  `json:"provider"`
	Arch                         string                  `json:"arch"`
	BuildMode                    string                  `json:"build_mode"`
	SourceTemplate               string                  `json:"source_template"`
	SourceVMID                   int                     `json:"source_vmid"`
	SourceProvenanceObjectID     string                  `json:"source_provenance_object_id"`
	Template                     string                  `json:"template"`
	TemplateSummary              string                  `json:"template_summary"`
	ProfileID                    string                  `json:"profile_id"`
	ProfilePath                  string                  `json:"profile_path"`
	ProfileRepository            string                  `json:"profile_repository"`
	ProfileParents               []CatalystProfileParent `json:"profile_parents,omitempty"`
	ProfileRepositoryKeyObjectID string                  `json:"profile_repository_key_object_id,omitempty"`
	GentooRepositoryKeyObjectID  string                  `json:"gentoo_repository_key_object_id"`
	TrustedCAObjectID            string                  `json:"trusted_ca_object_id,omitempty"`
	MirrorBundleID               string                  `json:"mirror_bundle_id"`
	// SourceRepositories binds the repository revisions present in an input
	// image manifest. Repositories separately names the revisions this build
	// will verify and install, allowing an immutable successor to advance them.
	SourceRepositories  map[string]string `json:"source_repositories,omitempty"`
	SourceDisplayModel  string            `json:"source_display_model,omitempty"`
	Repositories        map[string]string `json:"repositories"`
	RepositoryObjectIDs map[string]string `json:"repository_object_ids"`
	RepositoryURIs      map[string]string `json:"repository_uris"`
	RootfsSource        string            `json:"rootfs_source"`
	Channel             string            `json:"channel"`
	GentooMirror        string            `json:"gentoo_mirror"`
	Binhost             string            `json:"binhost,omitempty"`
	PackageSets         []string          `json:"package_sets"`
	Packages            []string          `json:"packages"`
	Desktop             bool              `json:"desktop"`
	DisplayModel        string            `json:"display_model"`
	Cores               int               `json:"cores"`
	Memory              int               `json:"memory"`
}

// PackerVars is generated only after the plan, lock and endpoints validate.
type PackerVars struct {
	ProxmoxURL                 string   `json:"proxmox_url"`
	ProxmoxNode                string   `json:"proxmox_node"`
	ProxmoxPool                string   `json:"proxmox_pool"`
	ProxmoxStorage             string   `json:"proxmox_storage"`
	ProxmoxBridge              string   `json:"proxmox_bridge"`
	ProxmoxInsecure            bool     `json:"proxmox_insecure"`
	SSHUsername                string   `json:"ssh_username"`
	SSHPrivateKeyFile          string   `json:"ssh_private_key_file"`
	SourceVMID                 int      `json:"source_vmid"`
	TemplateName               string   `json:"template_name"`
	TemplateDescription        string   `json:"template_description"`
	Cores                      int      `json:"cores"`
	Memory                     int      `json:"memory"`
	ProfileID                  string   `json:"profile_id"`
	ProfilePath                string   `json:"profile_path"`
	ImageGeneration            string   `json:"image_generation"`
	MirrorBundleID             string   `json:"mirror_bundle_id"`
	ProfileRepository          string   `json:"profile_repository"`
	ProfileParents             []string `json:"profile_parents"`
	RepositoryNames            []string `json:"repository_names"`
	RepositoryURIs             []string `json:"repository_uris"`
	RepositoryRevisions        []string `json:"repository_revisions"`
	RepositoryBundlePaths      []string `json:"repository_bundle_paths"`
	RepositoryBundleNames      []string `json:"repository_bundle_names"`
	LockedRepositoryInputPaths []string `json:"locked_repository_input_paths"`
	ProfileRepositoryKeyName   string   `json:"profile_repository_key_name,omitempty"`
	GentooRepositoryKeyName    string   `json:"gentoo_repository_key_name"`
	TrustedCAName              string   `json:"trusted_ca_name,omitempty"`
	GentooMirror               string   `json:"gentoo_mirror"`
	Binhost                    string   `json:"binhost"`
	PackageSets                []string `json:"package_sets"`
	PackageSetCatalogDigest    string   `json:"package_set_catalog_digest"`
	Packages                   []string `json:"packages"`
	Desktop                    bool     `json:"desktop"`
	DisplayModel               string   `json:"display_model"`
	BuildPlanPath              string   `json:"build_plan_path"`
	PackerManifestPath         string   `json:"packer_manifest_path"`
	PlanDigest                 string   `json:"plan_digest"`
	InputLockDigest            string   `json:"input_lock_digest"`
	CommonConfigDigest         string   `json:"common_config_digest"`
	SourceTemplate             string   `json:"source_template"`
	SourceProvenanceObjectID   string   `json:"source_provenance_object_id"`
	SourceProvenanceDigest     string   `json:"source_provenance_digest"`
	DistfileManifestPath       string   `json:"distfile_manifest_path"`
}

// PlanEvidence is retained before PVE is contacted.
type PlanEvidence struct {
	SchemaVersion            int               `json:"schema_version"`
	Target                   string            `json:"target"`
	PlanDigest               string            `json:"plan_digest"`
	InputLockDigest          string            `json:"input_lock_digest"`
	CommonConfigDigest       string            `json:"common_config_digest"`
	MirrorBundleID           string            `json:"mirror_bundle_id"`
	SourceVMID               int               `json:"source_vmid"`
	SourceTemplate           string            `json:"source_template"`
	SourceProvenanceObjectID string            `json:"source_provenance_object_id"`
	SourceProvenanceDigest   string            `json:"source_provenance_digest"`
	ApprovedHosts            []string          `json:"approved_hosts"`
	RepositoryBundleIDs      map[string]string `json:"repository_bundle_ids"`
	RepositoryKeyIDs         map[string]string `json:"repository_key_ids"`
	TrustedCAObjectID        string            `json:"trusted_ca_object_id,omitempty"`
	DistfileManifestID       string            `json:"distfile_manifest_id"`
	PackageSetCatalogID      string            `json:"package_set_catalog_id"`
	PackageSetCatalogDigest  string            `json:"package_set_catalog_digest"`
	PackageSets              []string          `json:"package_sets"`
}

type SourceCheckEvidence struct {
	SchemaVersion            int       `json:"schema_version"`
	CheckedAt                time.Time `json:"checked_at"`
	SourceVMID               int       `json:"source_vmid"`
	SourceTemplate           string    `json:"source_template"`
	SourceProvenanceObjectID string    `json:"source_provenance_object_id"`
	SourceProvenanceDigest   string    `json:"source_provenance_digest"`
	Verified                 bool      `json:"verified"`
}

type OutputStampEvidence struct {
	SchemaVersion  int       `json:"schema_version"`
	StampedAt      time.Time `json:"stamped_at"`
	Template       string    `json:"template"`
	VMID           int       `json:"vmid"`
	Node           string    `json:"node"`
	ManifestDigest string    `json:"manifest_digest"`
	ImageDigest    string    `json:"image_digest"`
	Verified       bool      `json:"verified"`
}

type ClosureManifest struct {
	SchemaVersion    int             `json:"schema_version"`
	Target           string          `json:"target"`
	RepositoryCommit string          `json:"repository_commit"`
	Objects          []ClosureObject `json:"objects"`
}

type ClosureObject struct {
	Filename string `json:"filename"`
	URI      string `json:"uri"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
}

func LoadCommonConfig(path string) (*CommonConfig, error) {
	var config CommonConfig
	if err := decodeStrictFile(path, &config); err != nil {
		return nil, err
	}
	if config.SchemaVersion != 1 || !repoComponentPattern.MatchString(config.ProxmoxNode) || !repoComponentPattern.MatchString(config.ProxmoxPool) || !repoComponentPattern.MatchString(config.ProxmoxStorage) || !repoComponentPattern.MatchString(config.ProxmoxBridge) || !repoComponentPattern.MatchString(config.SSHUsername) || config.SSHPrivateKeyFile == "" {
		return nil, fmt.Errorf("common config has missing or unsupported fields")
	}
	if config.ProxmoxHostMemoryHeadroomMB != 0 && (config.ProxmoxHostMemoryHeadroomMB < 1024 || config.ProxmoxHostMemoryHeadroomMB > 262144) {
		return nil, fmt.Errorf("proxmox_host_memory_headroom_mb must be zero or 1024..262144")
	}
	endpoint, err := url.Parse(config.ProxmoxURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || strings.TrimSuffix(endpoint.Path, "/") != "/api2/json" {
		return nil, fmt.Errorf("proxmox_url must be an HTTPS /api2/json endpoint without userinfo, query, or fragment")
	}
	return &config, nil
}

func LoadBuildPlan(path string) (*BuildPlan, error) {
	var plan BuildPlan
	if err := decodeStrictFile(path, &plan); err != nil {
		return nil, err
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	return &plan, nil
}

func (p *BuildPlan) Validate() error {
	if p.SchemaVersion != 1 || !objectIDPattern.MatchString(p.Target) || !objectIDPattern.MatchString(p.PlanObjectID) || !objectIDPattern.MatchString(p.ImageID) || !objectIDPattern.MatchString(p.Generation) || !objectIDPattern.MatchString(p.ProfileID) || !validProfilePath(p.ProfilePath) || !objectIDPattern.MatchString(p.MirrorBundleID) || !objectIDPattern.MatchString(p.SourceProvenanceObjectID) {
		return fmt.Errorf("build plan has invalid IDs")
	}
	if p.Provider != "pve" || p.Arch != "amd64" || p.BuildMode != "native-gentoo" || p.Channel != "candidate" {
		return fmt.Errorf("IMG-1 supports only candidate pve/amd64/native-gentoo plans")
	}
	if p.Target != "base-systemd" && p.Target != "desktop-verifier" {
		return fmt.Errorf("unsupported image target %q", p.Target)
	}
	if p.Desktop != (p.Target == "desktop-verifier") {
		return fmt.Errorf("desktop flag does not match target")
	}
	expectedDisplay := "std"
	if p.Desktop {
		expectedDisplay = "qxl"
	}
	if p.DisplayModel != expectedDisplay {
		return fmt.Errorf("display_model must be %q for target %q", expectedDisplay, p.Target)
	}
	if p.Target == "base-systemd" && p.RootfsSource != "approved-qcow2" && p.RootfsSource != "approved-qcow2-seed" && p.RootfsSource != "approved-pbs-snapshot" && p.RootfsSource != "catalyst-stage4-qcow2" && p.RootfsSource != "packer-base-image" {
		return fmt.Errorf("base-systemd rootfs_source is not approved")
	}
	if p.Target == "desktop-verifier" && p.RootfsSource != "packer-base-image" && p.RootfsSource != "packer-desktop-image" {
		return fmt.Errorf("desktop-verifier rootfs_source is not approved")
	}
	if p.SourceVMID < 100 || p.SourceVMID > 999999999 || !templateNamePattern.MatchString(p.SourceTemplate) || !templateNamePattern.MatchString(p.Template) || p.SourceTemplate == p.Template || p.TemplateSummary == "" || strings.ContainsAny(p.TemplateSummary, "\r\n|") || p.RootfsSource == "" {
		return fmt.Errorf("build plan has invalid source or output template metadata")
	}
	if p.Cores < 1 || p.Cores > 64 || p.Memory < 1024 || p.Memory > 524288 {
		return fmt.Errorf("build plan resources are outside approved bounds")
	}
	if (len(p.Repositories) != 1 && len(p.Repositories) != 2) || len(p.RepositoryObjectIDs) != len(p.Repositories) || len(p.RepositoryURIs) != len(p.Repositories) || !fullRevisionPattern.MatchString(p.Repositories["gentoo"]) {
		return fmt.Errorf("build plan requires one or two fully pinned repositories")
	}
	imageSource := p.Target == "desktop-verifier" || p.RootfsSource == "packer-base-image"
	if imageSource {
		if len(p.SourceRepositories) != len(p.Repositories) || !fullRevisionPattern.MatchString(p.SourceRepositories["gentoo"]) {
			return fmt.Errorf("image-derived build plan requires fully pinned source_repositories")
		}
		for name := range p.Repositories {
			if !fullRevisionPattern.MatchString(p.SourceRepositories[name]) {
				return fmt.Errorf("source repository %q is absent or not fully pinned", name)
			}
		}
		if p.SourceDisplayModel != "std" && p.SourceDisplayModel != "qxl" {
			return fmt.Errorf("image-derived build plan requires source_display_model std or qxl")
		}
	} else if len(p.SourceRepositories) != 0 || p.SourceDisplayModel != "" {
		return fmt.Errorf("non-image build plan contains unexpected source image contract")
	}
	if !repoComponentPattern.MatchString(p.ProfileRepository) {
		return fmt.Errorf("build plan profile repository is invalid")
	}
	if _, exists := p.Repositories[p.ProfileRepository]; !exists {
		return fmt.Errorf("profile repository %q is not pinned", p.ProfileRepository)
	}
	if p.ProfileRepository == "gentoo" {
		if len(p.Repositories) != 1 || len(p.ProfileParents) != 0 {
			return fmt.Errorf("official profile plans cannot add profile parents or repositories")
		}
	} else if len(p.Repositories) != 2 || len(p.ProfileParents) == 0 || len(p.ProfileParents) > 8 {
		return fmt.Errorf("external profile plans require one repository and an exact parent chain")
	}
	if p.ProfileRepository != "gentoo" && !objectIDPattern.MatchString(p.ProfileRepositoryKeyObjectID) {
		return fmt.Errorf("external profile plan requires a locked verification key")
	}
	if !objectIDPattern.MatchString(p.GentooRepositoryKeyObjectID) {
		return fmt.Errorf("build plan requires a locked Gentoo repository verification key")
	}
	if p.TrustedCAObjectID != "" && !objectIDPattern.MatchString(p.TrustedCAObjectID) {
		return fmt.Errorf("build plan trusted CA object ID is invalid")
	}
	if p.ProfileRepository == "gentoo" && p.ProfileRepositoryKeyObjectID != "" {
		return fmt.Errorf("official profile plan contains an unexpected external verification key")
	}
	seenParents := map[string]struct{}{}
	for _, parent := range p.ProfileParents {
		if !repoComponentPattern.MatchString(parent.Repository) || !validProfilePath(parent.ProfilePath) {
			return fmt.Errorf("build plan contains an invalid profile parent")
		}
		if _, exists := p.Repositories[parent.Repository]; !exists {
			return fmt.Errorf("profile parent repository %q is not pinned", parent.Repository)
		}
		line := parent.Repository + ":" + parent.ProfilePath
		if _, duplicate := seenParents[line]; duplicate {
			return fmt.Errorf("duplicate profile parent %q", line)
		}
		seenParents[line] = struct{}{}
	}
	for name, revision := range p.Repositories {
		if !repoComponentPattern.MatchString(name) || !fullRevisionPattern.MatchString(revision) || !objectIDPattern.MatchString(p.RepositoryObjectIDs[name]) || p.RepositoryURIs[name] == "" {
			return fmt.Errorf("repository %q lacks a full revision, locked object, or URI", name)
		}
	}
	if len(p.PackageSets) == 0 || len(p.PackageSets) > 32 || len(p.Packages) > 128 {
		return fmt.Errorf("build plan package-set selection is empty or too large")
	}
	seenSets := make(map[string]struct{}, len(p.PackageSets))
	for _, id := range p.PackageSets {
		if !objectIDPattern.MatchString(id) {
			return fmt.Errorf("invalid package set ID %q", id)
		}
		if _, exists := seenSets[id]; exists {
			return fmt.Errorf("duplicate package set ID %q", id)
		}
		seenSets[id] = struct{}{}
	}
	for _, atom := range p.Packages {
		if !validPackageAtom(atom) {
			return fmt.Errorf("invalid package atom %q", atom)
		}
	}
	return nil
}

// PreparePlan validates every duplicated boundary before Packer can mutate PVE.
func PreparePlan(commonPath, planPath, lockPath, offlineRoot, packerManifestPath string) (*PackerVars, *PlanEvidence, error) {
	common, err := LoadCommonConfig(commonPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load common config: %w", err)
	}
	plan, err := LoadBuildPlan(planPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load build plan: %w", err)
	}
	lock, err := LoadInputLock(lockPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load input lock: %w", err)
	}
	if lock.BundleID != plan.MirrorBundleID {
		return nil, nil, fmt.Errorf("input lock bundle %q does not match plan %q", lock.BundleID, plan.MirrorBundleID)
	}
	hosts := []string{}
	endpoints := []struct{ name, raw string }{
		{"proxmox_url", common.ProxmoxURL},
		{"gentoo_mirror", plan.GentooMirror},
		{"binhost", plan.Binhost},
	}
	for name, raw := range plan.RepositoryURIs {
		endpoints = append(endpoints, struct{ name, raw string }{"repository_uri[" + name + "]", raw})
	}
	for _, endpoint := range endpoints {
		name, raw := endpoint.name, endpoint.raw
		if raw == "" {
			continue
		}
		host, err := validateEndpoint(raw, lock.AllowedHosts)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		hosts = append(hosts, host)
	}
	if info, err := os.Stat(common.SSHPrivateKeyFile); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, nil, fmt.Errorf("ssh_private_key_file must be a regular owner-only file")
	}
	sourceObject, err := requiredObject(lock, plan.SourceProvenanceObjectID, plan.Target)
	if err != nil {
		return nil, nil, err
	}
	repositoryNames := make([]string, 0, len(plan.Repositories))
	for name := range plan.Repositories {
		repositoryNames = append(repositoryNames, name)
	}
	sort.Strings(repositoryNames)
	repositoryObjects := make(map[string]*InputObject, len(repositoryNames))
	for _, name := range repositoryNames {
		object, err := requiredObject(lock, plan.RepositoryObjectIDs[name], plan.Target)
		if err != nil {
			return nil, nil, err
		}
		if object.Kind != "repository-snapshot" {
			return nil, nil, fmt.Errorf("repository object %q must have kind repository-snapshot", object.ID)
		}
		repositoryObjects[name] = object
	}
	manifestObject, err := uniqueObjectByKind(lock, "distfile-manifest", plan.Target)
	if err != nil {
		return nil, nil, err
	}
	packageSetObject, err := uniqueObjectByKind(lock, "package-set-catalog", plan.Target)
	if err != nil {
		return nil, nil, err
	}
	offlineRoot, err = filepath.Abs(offlineRoot)
	if err != nil {
		return nil, nil, err
	}
	preflight, err := Preflight(offlineRoot, lock, plan.Target)
	if err != nil {
		return nil, nil, fmt.Errorf("preflight locked inputs: %w", err)
	}
	if len(preflight.Missing) != 0 {
		return nil, nil, fmt.Errorf("preflight locked inputs: %d object(s) missing or invalid", len(preflight.Missing))
	}
	if err := requirePackerExecutionSurface(lock, plan.Target); err != nil {
		return nil, nil, err
	}
	if err := validateTerraformCLIConfig(offlineRoot, lock, plan.Target); err != nil {
		return nil, nil, err
	}
	repositoryBundlePaths := make([]string, 0, len(repositoryNames))
	repositoryBundleNames := make([]string, 0, len(repositoryNames))
	repositoryURIs := make([]string, 0, len(repositoryNames))
	repositoryRevisions := make([]string, 0, len(repositoryNames))
	repositoryBundleIDs := make(map[string]string, len(repositoryNames))
	lockedRepositoryInputPaths := make([]string, 0, len(repositoryNames)+1)
	seenBundleNames := map[string]struct{}{}
	for _, name := range repositoryNames {
		object := repositoryObjects[name]
		bundleName := filepath.Base(object.Path)
		if _, duplicate := seenBundleNames[bundleName]; duplicate {
			return nil, nil, fmt.Errorf("repository bundles have duplicate basename %q", bundleName)
		}
		seenBundleNames[bundleName] = struct{}{}
		repositoryBundlePaths = append(repositoryBundlePaths, filepath.Join(offlineRoot, object.Path))
		lockedRepositoryInputPaths = append(lockedRepositoryInputPaths, filepath.Join(offlineRoot, object.Path))
		repositoryBundleNames = append(repositoryBundleNames, bundleName)
		repositoryURIs = append(repositoryURIs, plan.RepositoryURIs[name])
		repositoryRevisions = append(repositoryRevisions, plan.Repositories[name])
		repositoryBundleIDs[name] = object.ID
	}
	profileRepositoryKeyName := ""
	repositoryKeyIDs := map[string]string{}
	gentooKeyObject, err := requiredObject(lock, plan.GentooRepositoryKeyObjectID, plan.Target)
	if err != nil {
		return nil, nil, err
	}
	if gentooKeyObject.Kind != "release-key" {
		return nil, nil, fmt.Errorf("Gentoo repository key object %q must have kind release-key", gentooKeyObject.ID)
	}
	gentooRepositoryKeyName := filepath.Base(gentooKeyObject.Path)
	if _, duplicate := seenBundleNames[gentooRepositoryKeyName]; duplicate {
		return nil, nil, fmt.Errorf("Gentoo key and repository bundle have duplicate basename %q", gentooRepositoryKeyName)
	}
	seenBundleNames[gentooRepositoryKeyName] = struct{}{}
	lockedRepositoryInputPaths = append(lockedRepositoryInputPaths, filepath.Join(offlineRoot, gentooKeyObject.Path))
	repositoryKeyIDs["gentoo"] = gentooKeyObject.ID
	trustedCAName := ""
	if plan.TrustedCAObjectID != "" {
		caObject, err := requiredObject(lock, plan.TrustedCAObjectID, plan.Target)
		if err != nil {
			return nil, nil, err
		}
		if caObject.Kind != "ca-bundle" {
			return nil, nil, fmt.Errorf("trusted CA object %q must have kind ca-bundle", caObject.ID)
		}
		trustedCAName = filepath.Base(caObject.Path)
		if _, duplicate := seenBundleNames[trustedCAName]; duplicate {
			return nil, nil, fmt.Errorf("trusted CA and repository inputs have duplicate basename %q", trustedCAName)
		}
		seenBundleNames[trustedCAName] = struct{}{}
		lockedRepositoryInputPaths = append(lockedRepositoryInputPaths, filepath.Join(offlineRoot, caObject.Path))
	}
	if plan.ProfileRepository != "gentoo" {
		keyObject, err := requiredObject(lock, plan.ProfileRepositoryKeyObjectID, plan.Target)
		if err != nil {
			return nil, nil, err
		}
		if keyObject.Kind != "release-key" {
			return nil, nil, fmt.Errorf("profile repository key object %q must have kind release-key", keyObject.ID)
		}
		profileRepositoryKeyName = filepath.Base(keyObject.Path)
		if _, duplicate := seenBundleNames[profileRepositoryKeyName]; duplicate {
			return nil, nil, fmt.Errorf("profile key and repository bundle have duplicate basename %q", profileRepositoryKeyName)
		}
		lockedRepositoryInputPaths = append(lockedRepositoryInputPaths, filepath.Join(offlineRoot, keyObject.Path))
		repositoryKeyIDs[plan.ProfileRepository] = keyObject.ID
	}
	distfileManifestPath := filepath.Join(offlineRoot, manifestObject.Path)
	packageSetCatalogPath := filepath.Join(offlineRoot, packageSetObject.Path)
	packageSetCatalog, err := LoadPackageSetCatalog(packageSetCatalogPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load package-set catalog: %w", err)
	}
	effectivePackages, err := packageSetCatalog.Resolve(plan.PackageSets, plan.Packages)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve package sets: %w", err)
	}
	var closure ClosureManifest
	if err := decodeStrictFile(distfileManifestPath, &closure); err != nil {
		return nil, nil, fmt.Errorf("load distfile closure: %w", err)
	}
	if closure.SchemaVersion != 1 || closure.Target != plan.Target || closure.RepositoryCommit != plan.Repositories["gentoo"] || len(closure.Objects) == 0 {
		return nil, nil, fmt.Errorf("distfile closure does not match target and repository commit")
	}
	filenames := make(map[string]struct{}, len(closure.Objects))
	for _, object := range closure.Objects {
		if filepath.Base(object.Filename) != object.Filename || object.Size < 0 || !fullRevisionPattern.MatchString(object.SHA256) || len(object.SHA256) != 64 {
			return nil, nil, fmt.Errorf("distfile closure contains invalid object %q", object.Filename)
		}
		if _, exists := filenames[object.Filename]; exists {
			return nil, nil, fmt.Errorf("distfile closure contains duplicate filename %q", object.Filename)
		}
		filenames[object.Filename] = struct{}{}
		if _, err := validateEndpoint(object.URI, lock.AllowedHosts); err != nil {
			return nil, nil, fmt.Errorf("distfile %q: %w", object.Filename, err)
		}
	}
	planPath, err = filepath.Abs(planPath)
	if err != nil {
		return nil, nil, err
	}
	packerManifestPath, err = filepath.Abs(packerManifestPath)
	if err != nil {
		return nil, nil, err
	}
	planDigest, err := digestFile(planPath)
	if err != nil {
		return nil, nil, err
	}
	planObject, err := requiredObject(lock, plan.PlanObjectID, plan.Target)
	if err != nil {
		return nil, nil, err
	}
	if planObject.Kind != "build-plan" {
		return nil, nil, fmt.Errorf("BuildPlan object %q must have kind build-plan", plan.PlanObjectID)
	}
	expectedSourceKind := expectedSourceObjectKind(plan)
	if sourceObject.Kind != expectedSourceKind {
		return nil, nil, fmt.Errorf("source provenance object %q must have kind %s", plan.SourceProvenanceObjectID, expectedSourceKind)
	}
	if sourceObject.Kind == "image-manifest" {
		if err := validateSourceImageManifest(filepath.Join(offlineRoot, sourceObject.Path), plan); err != nil {
			return nil, nil, err
		}
	}
	if sourceObject.Kind == "pbs-source-attestation" {
		attestation, err := LoadPBSSourceAttestation(filepath.Join(offlineRoot, sourceObject.Path))
		if err != nil {
			return nil, nil, fmt.Errorf("load PBS source attestation: %w", err)
		}
		if err := attestation.ValidateForPlan(plan); err != nil {
			return nil, nil, err
		}
	}
	if sourceObject.Kind == "qcow2-manifest" {
		if err := validateCatalystSourceManifest(filepath.Join(offlineRoot, sourceObject.Path), plan); err != nil {
			return nil, nil, err
		}
	}
	if planDigest != planObject.SHA256 {
		return nil, nil, fmt.Errorf("BuildPlan digest does not match locked object %q", plan.PlanObjectID)
	}
	lockDigest, err := digestFile(lockPath)
	if err != nil {
		return nil, nil, err
	}
	commonDigest, err := digestFile(commonPath)
	if err != nil {
		return nil, nil, err
	}
	planDigest = "sha256:" + planDigest
	lockDigest = "sha256:" + lockDigest
	commonDigest = "sha256:" + commonDigest
	sourceDigest := "sha256:" + sourceObject.SHA256
	profileParents := make([]string, 0, len(plan.ProfileParents))
	for _, parent := range plan.ProfileParents {
		profileParents = append(profileParents, parent.Repository+":"+parent.ProfilePath)
	}
	vars := &PackerVars{
		ProxmoxURL: common.ProxmoxURL, ProxmoxNode: common.ProxmoxNode, ProxmoxPool: common.ProxmoxPool,
		ProxmoxStorage: common.ProxmoxStorage, ProxmoxBridge: common.ProxmoxBridge, ProxmoxInsecure: common.ProxmoxInsecure,
		SSHUsername: common.SSHUsername, SSHPrivateKeyFile: common.SSHPrivateKeyFile, SourceVMID: plan.SourceVMID,
		TemplateName: plan.Template, TemplateDescription: plan.TemplateSummary + " | portage-engine-provenance=" + planDigest,
		Cores: plan.Cores, Memory: plan.Memory, ProfileID: plan.ProfileID, ProfilePath: plan.ProfilePath, ProfileRepository: plan.ProfileRepository, ProfileParents: profileParents,
		ImageGeneration: plan.Generation, MirrorBundleID: plan.MirrorBundleID, RepositoryNames: repositoryNames, RepositoryURIs: repositoryURIs,
		RepositoryRevisions: repositoryRevisions, RepositoryBundlePaths: repositoryBundlePaths, RepositoryBundleNames: repositoryBundleNames, GentooMirror: plan.GentooMirror, Binhost: plan.Binhost,
		LockedRepositoryInputPaths: lockedRepositoryInputPaths, ProfileRepositoryKeyName: profileRepositoryKeyName, GentooRepositoryKeyName: gentooRepositoryKeyName, TrustedCAName: trustedCAName,
		PackageSets: plan.PackageSets, PackageSetCatalogDigest: "sha256:" + packageSetObject.SHA256,
		Packages: effectivePackages, Desktop: plan.Desktop, DisplayModel: plan.DisplayModel, BuildPlanPath: planPath, PackerManifestPath: packerManifestPath,
		PlanDigest: planDigest, InputLockDigest: lockDigest, CommonConfigDigest: commonDigest, SourceTemplate: plan.SourceTemplate,
		SourceProvenanceObjectID: plan.SourceProvenanceObjectID, SourceProvenanceDigest: sourceDigest,
		DistfileManifestPath: distfileManifestPath,
	}
	evidence := &PlanEvidence{SchemaVersion: 1, Target: plan.Target, PlanDigest: planDigest, InputLockDigest: lockDigest, CommonConfigDigest: commonDigest,
		MirrorBundleID: plan.MirrorBundleID, SourceVMID: plan.SourceVMID, SourceTemplate: plan.SourceTemplate,
		SourceProvenanceObjectID: plan.SourceProvenanceObjectID, SourceProvenanceDigest: sourceDigest, ApprovedHosts: hosts,
		RepositoryBundleIDs: repositoryBundleIDs, RepositoryKeyIDs: repositoryKeyIDs, TrustedCAObjectID: plan.TrustedCAObjectID, DistfileManifestID: manifestObject.ID,
		PackageSetCatalogID: packageSetObject.ID, PackageSetCatalogDigest: "sha256:" + packageSetObject.SHA256,
		PackageSets: append([]string(nil), plan.PackageSets...)}
	return vars, evidence, nil
}

func validateSourceImageManifest(path string, plan *BuildPlan) error {
	manifest, err := LoadImageManifest(path)
	if err != nil {
		return fmt.Errorf("load source image manifest: %w", err)
	}
	if plan == nil {
		return fmt.Errorf("source image requires a BuildPlan")
	}
	if manifest.Template != plan.SourceTemplate || manifest.Provider != plan.Provider || manifest.Arch != plan.Arch || manifest.BuildMode != plan.BuildMode || !maps.Equal(manifest.Repositories, plan.SourceRepositories) || manifest.Channel != "candidate" {
		return fmt.Errorf("source image manifest does not match its ABI/repository contract")
	}
	if manifest.DisplayModel != "" && manifest.DisplayModel != plan.SourceDisplayModel {
		return fmt.Errorf("source image manifest display model does not match the BuildPlan")
	}
	if plan.Target == "desktop-verifier" {
		if plan.RootfsSource == "packer-desktop-image" {
			if manifest.Target != "desktop-verifier" || manifest.ProfileRepository != plan.ProfileRepository || manifest.ProfilePath != plan.ProfilePath || !slices.Equal(manifest.ProfileParents, plan.ProfileParents) {
				return fmt.Errorf("desktop successor source image manifest does not match its exact desktop profile contract")
			}
			return nil
		}
		if manifest.Target != "base-systemd" {
			return fmt.Errorf("desktop source image must be a base-systemd image")
		}
		if len(plan.ProfileParents) == 0 {
			return fmt.Errorf("desktop source image requires a reviewed base profile parent")
		}
		parent := plan.ProfileParents[0]
		if manifest.ProfileRepository != parent.Repository || manifest.ProfilePath != parent.ProfilePath {
			return fmt.Errorf("desktop source image manifest does not match its base profile contract")
		}
		return nil
	}
	if manifest.Target != "base-systemd" {
		return fmt.Errorf("base successor source image must be a base-systemd image")
	}
	if plan.Target != "base-systemd" || plan.RootfsSource != "packer-base-image" || manifest.ProfileRepository != plan.ProfileRepository || manifest.ProfilePath != plan.ProfilePath || !slices.Equal(manifest.ProfileParents, plan.ProfileParents) {
		return fmt.Errorf("base successor source image manifest does not match its exact profile contract")
	}
	return nil
}

func validateCatalystSourceManifest(path string, plan *BuildPlan) error {
	manifest, err := LoadQCOW2Manifest(path)
	if err != nil {
		return fmt.Errorf("load Catalyst QCOW2 source manifest: %w", err)
	}
	if plan == nil || manifest.Arch != plan.Arch || manifest.ProfileID != plan.ProfileID {
		return fmt.Errorf("Catalyst QCOW2 source manifest does not match the BuildPlan architecture/profile contract")
	}
	return nil
}

func requirePackerExecutionSurface(lock *InputLock, target string) error {
	required := map[string]bool{
		"factory/run-offline.sh":                      true,
		"factory/smoke-offline.sh":                    true,
		"factory/packer/template.pkr.hcl":             false,
		"factory/packer/scripts/provision.sh":         true,
		"factory/packer/scripts/sanitize-and-gate.sh": true,
		"factory/packer/scripts/hydrate-distfiles.py": true,
		"factory/catalyst/verify-profile.py":          true,
		"factory/desktop/guest-agent.py":              true,
	}
	for path, executable := range required {
		var object *InputObject
		for index := range lock.Objects {
			candidate := &lock.Objects[index]
			if candidate.Path == path && requiredFor(*candidate, target) {
				object = candidate
				break
			}
		}
		if object == nil {
			return fmt.Errorf("Packer execution surface %q is absent from the input lock for %q", path, target)
		}
		if object.Kind != "script" || object.Executable != executable {
			return fmt.Errorf("Packer execution surface %q must be a locked script with executable=%t", path, executable)
		}
	}
	return nil
}

func validateTerraformCLIConfig(offlineRoot string, lock *InputLock, target string) error {
	var config *InputObject
	for index := range lock.Objects {
		candidate := &lock.Objects[index]
		if candidate.Path != "terraform/terraform.rc" || !requiredFor(*candidate, target) {
			continue
		}
		if config != nil {
			return fmt.Errorf("multiple Terraform CLI configs are applicable to %q", target)
		}
		config = candidate
	}
	if config == nil {
		return fmt.Errorf("Terraform CLI config is absent from the input lock for %q", target)
	}
	if config.Kind != "terraform-lock" || config.Executable {
		return fmt.Errorf("Terraform CLI config must be a locked terraform-lock object with executable=false")
	}
	contents, err := os.ReadFile(filepath.Join(offlineRoot, filepath.FromSlash(config.Path)))
	if err != nil {
		return fmt.Errorf("read Terraform CLI config: %w", err)
	}
	if strings.TrimSpace(string(contents)) != strictTerraformCLIConfig {
		return fmt.Errorf("Terraform CLI config must use only the approved filesystem mirror template without direct fallback")
	}
	return nil
}

func expectedSourceObjectKind(plan *BuildPlan) string {
	if plan.Target == "desktop-verifier" || plan.RootfsSource == "packer-base-image" {
		return "image-manifest"
	}
	if plan.RootfsSource == "catalyst-stage4-qcow2" {
		return "qcow2-manifest"
	}
	if plan.RootfsSource == "approved-pbs-snapshot" {
		return "pbs-source-attestation"
	}
	return "seed"
}

func uniqueObjectByKind(lock *InputLock, kind, target string) (*InputObject, error) {
	var match *InputObject
	for i := range lock.Objects {
		object := &lock.Objects[i]
		if object.Kind != kind || !requiredFor(*object, target) {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("multiple %s objects are applicable to %q", kind, target)
		}
		match = object
	}
	if match == nil {
		return nil, fmt.Errorf("no %s object is applicable to %q", kind, target)
	}
	return match, nil
}

// validateEndpoint validates artifact-plane endpoints. Plain HTTP is allowed
// because every object fetched through these endpoints is independently bound
// to a reviewed digest/signature. Credential-bearing control-plane endpoints
// (for example Proxmox) are validated separately and remain HTTPS-only.
func validateEndpoint(raw string, allowedHosts []string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("artifact endpoint must be HTTP or HTTPS without userinfo, query, or fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	for _, allowed := range allowedHosts {
		if strings.EqualFold(host, allowed) {
			return host, nil
		}
	}
	return "", fmt.Errorf("host %q is not in the input-lock allowlist", host)
}

func requiredObject(lock *InputLock, id, target string) (*InputObject, error) {
	for i := range lock.Objects {
		object := &lock.Objects[i]
		if object.ID == id {
			if !requiredFor(*object, target) {
				return nil, fmt.Errorf("source provenance object %q is not approved for %q", id, target)
			}
			return object, nil
		}
	}
	return nil, fmt.Errorf("source provenance object %q is absent from input lock", id)
}

// CheckPVESource verifies that the VMID is an immutable template with the name
// and provenance marker approved by the BuildPlan.
func CheckPVESource(ctx context.Context, common *CommonConfig, plan *BuildPlan, evidence *PlanEvidence, username, token string) error {
	if username == "" || token == "" {
		return fmt.Errorf("PVE token credentials are required")
	}
	base := strings.TrimSuffix(common.ProxmoxURL, "/")
	requestURL := fmt.Sprintf("%s/nodes/%s/qemu/%d/config", base, url.PathEscape(common.ProxmoxNode), plan.SourceVMID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+username+"="+token)
	client := pveClient(common)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("query PVE source template: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("query PVE source template: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data struct {
			Template    json.Number `json:"template"`
			Name        string      `json:"name"`
			Description string      `json:"description"`
			CIUpgrade   json.Number `json:"ciupgrade"`
			CIUser      string      `json:"ciuser"`
			IPConfig0   string      `json:"ipconfig0"`
			VGA         string      `json:"vga"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("decode PVE source template: %w", err)
	}
	template, _ := strconv.Atoi(payload.Data.Template.String())
	if template != 1 || payload.Data.Name != plan.SourceTemplate {
		return fmt.Errorf("PVE source VMID %d is not the approved template %q", plan.SourceVMID, plan.SourceTemplate)
	}
	if payload.Data.CIUpgrade.String() != "0" {
		return fmt.Errorf("PVE source VMID %d must set ciupgrade=0 to prevent implicit first-boot package updates", plan.SourceVMID)
	}
	if payload.Data.CIUser != "root" || payload.Data.IPConfig0 != "ip=dhcp" {
		return fmt.Errorf("PVE source VMID %d must set ciuser=root and ipconfig0=ip=dhcp for deterministic Packer connectivity", plan.SourceVMID)
	}
	if plan.SourceDisplayModel != "" {
		actualDisplay := pveVGAModel(payload.Data.VGA)
		if actualDisplay != plan.SourceDisplayModel {
			return fmt.Errorf("PVE source VMID %d display model %q does not match approved %q", plan.SourceVMID, actualDisplay, plan.SourceDisplayModel)
		}
	}
	marker := "portage-engine-provenance=" + evidence.SourceProvenanceDigest
	if !strings.Contains(payload.Data.Description, marker) {
		return fmt.Errorf("PVE source template lacks approved provenance marker %q", marker)
	}
	if common.ProxmoxHostMemoryHeadroomMB > 0 {
		var status struct {
			Data struct {
				Memory struct {
					Free  json.Number `json:"free"`
					Total json.Number `json:"total"`
				} `json:"memory"`
			} `json:"data"`
		}
		statusURL := fmt.Sprintf("%s/nodes/%s/status", base, url.PathEscape(common.ProxmoxNode))
		if err := getPVEJSON(ctx, common, statusURL, username, token, &status); err != nil {
			return fmt.Errorf("query PVE node memory capacity: %w", err)
		}
		freeBytes, freeErr := strconv.ParseInt(status.Data.Memory.Free.String(), 10, 64)
		totalBytes, totalErr := strconv.ParseInt(status.Data.Memory.Total.String(), 10, 64)
		requiredBytes := int64(plan.Memory+common.ProxmoxHostMemoryHeadroomMB) * 1024 * 1024
		if freeErr != nil || totalErr != nil || freeBytes < 0 || totalBytes <= 0 || freeBytes > totalBytes {
			return fmt.Errorf("PVE node returned invalid memory capacity")
		}
		if freeBytes < requiredBytes {
			return fmt.Errorf("PVE node %q has %d MiB free; image requires %d MiB plus %d MiB host headroom", common.ProxmoxNode, freeBytes/(1024*1024), plan.Memory, common.ProxmoxHostMemoryHeadroomMB)
		}
	}
	return nil
}

// StampPVEOutput binds a smoke-tested PVE template to the exact generated
// image-manifest file so a later BuildPlan can verify the actual base output.
func StampPVEOutput(ctx context.Context, common *CommonConfig, manifest *ImageManifest, manifestPath, username, token string) (*OutputStampEvidence, error) {
	if username == "" || token == "" {
		return nil, fmt.Errorf("PVE token credentials are required")
	}
	manifestDigest, err := digestFile(manifestPath)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(common.ProxmoxURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/cluster/resources?type=vm", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+username+"="+token)
	resp, err := pveClient(common).Do(req)
	if err != nil {
		return nil, fmt.Errorf("query PVE candidate template: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query PVE candidate template: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			VMID     json.Number `json:"vmid"`
			Node     string      `json:"node"`
			Name     string      `json:"name"`
			Template json.Number `json:"template"`
			Type     string      `json:"type"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode PVE candidate template: %w", err)
	}
	vmid, node, matches := 0, "", 0
	for _, resource := range payload.Data {
		template, _ := strconv.Atoi(resource.Template.String())
		if resource.Name == manifest.Template && resource.Type == "qemu" && template == 1 {
			vmid, _ = strconv.Atoi(resource.VMID.String())
			node = resource.Node
			matches++
		}
	}
	if matches != 1 || vmid < 100 || node == "" {
		return nil, fmt.Errorf("expected exactly one PVE template named %q, found %d", manifest.Template, matches)
	}
	markerDigest := "sha256:" + manifestDigest
	description := fmt.Sprintf("Portage Engine candidate %s | portage-engine-provenance=%s | portage-engine-image=%s", manifest.ImageID, markerDigest, manifest.ImageDigest)
	// PVE's default ciupgrade=1 makes cloud-init run a distribution package
	// update on first boot. That is an uncontrolled network mutation on Gentoo,
	// so make the disabled state part of the stamped template contract.
	form := url.Values{
		"description": []string{description},
		"ciupgrade":   []string{"0"},
		"ciuser":      []string{"root"},
		"ipconfig0":   []string{"ip=dhcp"},
	}
	updateURL := fmt.Sprintf("%s/nodes/%s/qemu/%d/config", base, url.PathEscape(node), vmid)
	updateReq, err := http.NewRequestWithContext(ctx, http.MethodPut, updateURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	updateReq.Header.Set("Authorization", "PVEAPIToken="+username+"="+token)
	updateReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	updateResp, err := pveClient(common).Do(updateReq)
	if err != nil {
		return nil, fmt.Errorf("stamp PVE candidate template: %w", err)
	}
	if updateResp.StatusCode != http.StatusOK {
		_ = updateResp.Body.Close()
		return nil, fmt.Errorf("stamp PVE candidate template: HTTP %d", updateResp.StatusCode)
	}
	_ = updateResp.Body.Close()

	verifyReq, err := http.NewRequestWithContext(ctx, http.MethodGet, updateURL, nil)
	if err != nil {
		return nil, err
	}
	verifyReq.Header.Set("Authorization", "PVEAPIToken="+username+"="+token)
	verifyResp, err := pveClient(common).Do(verifyReq)
	if err != nil {
		return nil, fmt.Errorf("verify PVE candidate stamp: %w", err)
	}
	defer func() { _ = verifyResp.Body.Close() }()
	if verifyResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("verify PVE candidate stamp: HTTP %d", verifyResp.StatusCode)
	}
	var verified struct {
		Data struct {
			Description string      `json:"description"`
			CIUpgrade   json.Number `json:"ciupgrade"`
			CIUser      string      `json:"ciuser"`
			IPConfig0   string      `json:"ipconfig0"`
			VGA         string      `json:"vga"`
		} `json:"data"`
	}
	verifyDecoder := json.NewDecoder(verifyResp.Body)
	verifyDecoder.UseNumber()
	if err := verifyDecoder.Decode(&verified); err != nil {
		return nil, fmt.Errorf("decode PVE candidate stamp: %w", err)
	}
	actualDisplay := pveVGAModel(verified.Data.VGA)
	if verified.Data.Description != description || verified.Data.CIUpgrade.String() != "0" ||
		verified.Data.CIUser != "root" || verified.Data.IPConfig0 != "ip=dhcp" ||
		actualDisplay != manifest.DisplayModel {
		return nil, fmt.Errorf("PVE candidate stamp read-back mismatch")
	}
	return &OutputStampEvidence{SchemaVersion: 1, StampedAt: time.Now().UTC(), Template: manifest.Template,
		VMID: vmid, Node: node, ManifestDigest: markerDigest, ImageDigest: manifest.ImageDigest, Verified: true}, nil
}

// pveVGAModel accepts both PVE's legacy "std,memory=64" form and the
// normalized "memory=64,type=std" form returned by newer QEMU endpoints.
// PVE treats an absent type as the default std adapter.
func pveVGAModel(raw string) string {
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		key, value, hasValue := strings.Cut(field, "=")
		if hasValue {
			if key == "type" && value != "" {
				return value
			}
			continue
		}
		if field != "" {
			return field
		}
	}
	return "std"
}

func pveClient(common *CommonConfig) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: common.ProxmoxInsecure} // #nosec G402 -- explicit site-local opt-in.
	return &http.Client{Timeout: 20 * time.Second, Transport: transport}
}
