package imagefactory

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

var catalystNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
var generatedPathPattern = regexp.MustCompile(`^/[a-zA-Z0-9_./-]+$`)

const catalystTarget = "catalyst-base-systemd"
const catalystProfileTarget = "catalyst-profile-systemd"
const catalystOfficialProfile = "default/linux/amd64/23.0/systemd"

type CatalystProfileParent struct {
	Repository  string `json:"repository"`
	ProfilePath string `json:"profile_path"`
}

// CatalystPlan is the reviewed rootfs contract. IMG-2 keeps the official
// amd64/systemd lane; IMG-3 adds one signed external profile repository whose
// exact parent chain is part of the plan.
type CatalystPlan struct {
	SchemaVersion                int                     `json:"schema_version"`
	Target                       string                  `json:"target"`
	PlanObjectID                 string                  `json:"plan_object_id"`
	RootfsID                     string                  `json:"rootfs_id"`
	Generation                   string                  `json:"generation"`
	Arch                         string                  `json:"arch"`
	Subarch                      string                  `json:"subarch"`
	RelType                      string                  `json:"rel_type"`
	VersionStamp                 string                  `json:"version_stamp"`
	SnapshotID                   string                  `json:"snapshot_id"`
	ProfileID                    string                  `json:"profile_id"`
	ProfilePath                  string                  `json:"profile_path"`
	ProfileRepository            string                  `json:"profile_repository"`
	ProfileParents               []CatalystProfileParent `json:"profile_parents,omitempty"`
	ProfileRepositoryObjectID    string                  `json:"profile_repository_object_id,omitempty"`
	ProfileRepositoryKeyObjectID string                  `json:"profile_repository_key_object_id,omitempty"`
	MirrorBundleID               string                  `json:"mirror_bundle_id"`
	Repositories                 map[string]string       `json:"repositories"`
	RepositoryObjectID           string                  `json:"repository_object_id"`
	GentooRepositoryKeyObjectID  string                  `json:"gentoo_repository_key_object_id"`
	Stage3ObjectID               string                  `json:"stage3_object_id"`
	Stage3DigestsObjectID        string                  `json:"stage3_digests_object_id"`
	ReleaseKeyObjectID           string                  `json:"release_key_object_id"`
	CatalystRuntimeObjectID      string                  `json:"catalyst_runtime_object_id"`
	DistfileManifestObjectID     string                  `json:"distfile_manifest_object_id"`
	PackageSetCatalogObjectID    string                  `json:"package_set_catalog_object_id"`
	PackageSets                  []string                `json:"package_sets"`
	Packages                     []string                `json:"packages"`
	RuntimeGentooRepositoryURI   string                  `json:"runtime_gentoo_repository_uri"`
	RuntimeGentooMirror          string                  `json:"runtime_gentoo_mirror"`
	RuntimeBinhost               string                  `json:"runtime_binhost,omitempty"`
	RuntimeProfileRepositoryURI  string                  `json:"runtime_profile_repository_uri,omitempty"`
	SeedFilename                 string                  `json:"seed_filename"`
	OutputFilename               string                  `json:"output_filename"`
	QCOW2Filename                string                  `json:"qcow2_filename"`
	DiskSizeGiB                  int                     `json:"disk_size_gib"`
	Jobs                         int                     `json:"jobs"`
}

type CatalystInputEvidence struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// CatalystPrepared is both the pre-build evidence and the runner contract.
// Absolute paths are confined to the verified offline root or a fresh workdir.
type CatalystPrepared struct {
	SchemaVersion               int                     `json:"schema_version"`
	Target                      string                  `json:"target"`
	PlanDigest                  string                  `json:"plan_digest"`
	InputLockDigest             string                  `json:"input_lock_digest"`
	MirrorBundleID              string                  `json:"mirror_bundle_id"`
	RepositoryCommit            string                  `json:"repository_commit"`
	SnapshotID                  string                  `json:"snapshot_id"`
	ProfileID                   string                  `json:"profile_id"`
	ProfilePath                 string                  `json:"profile_path"`
	ProfileRepository           string                  `json:"profile_repository"`
	ProfileRepositoryCommit     string                  `json:"profile_repository_commit,omitempty"`
	ProfileParents              []CatalystProfileParent `json:"profile_parents,omitempty"`
	PackageSets                 []string                `json:"package_sets"`
	Packages                    []string                `json:"packages"`
	PackageSetCatalogDigest     string                  `json:"package_set_catalog_digest"`
	Inputs                      []CatalystInputEvidence `json:"inputs"`
	OfflineRoot                 string                  `json:"offline_root"`
	WorkRoot                    string                  `json:"work_root"`
	RuntimeArchivePath          string                  `json:"runtime_archive_path"`
	RuntimeRoot                 string                  `json:"runtime_root"`
	CatalystBinaryPath          string                  `json:"catalyst_binary_path"`
	CatalystSharedir            string                  `json:"catalyst_sharedir"`
	RepositoryBundlePath        string                  `json:"repository_bundle_path"`
	GentooRepositoryKeyPath     string                  `json:"gentoo_repository_key_path"`
	Stage3Path                  string                  `json:"stage3_path"`
	Stage3DigestsPath           string                  `json:"stage3_digests_path"`
	ReleaseKeyPath              string                  `json:"release_key_path"`
	DistfileManifestPath        string                  `json:"distfile_manifest_path"`
	PackageSetCatalogPath       string                  `json:"package_set_catalog_path"`
	ProfileRepositoryBundlePath string                  `json:"profile_repository_bundle_path,omitempty"`
	ProfileRepositoryKeyPath    string                  `json:"profile_repository_key_path,omitempty"`
	ProfileRepositorySourcePath string                  `json:"profile_repository_source_path,omitempty"`
	ConfigPath                  string                  `json:"config_path"`
	SpecPath                    string                  `json:"spec_path"`
	EnvscriptPath               string                  `json:"envscript_path"`
	FSScriptPath                string                  `json:"fsscript_path"`
	RootOverlayPath             string                  `json:"root_overlay_path"`
	PortageConfigPath           string                  `json:"portage_config_path"`
	StoreDir                    string                  `json:"storedir"`
	ReposStoreDir               string                  `json:"repos_storedir"`
	DistDir                     string                  `json:"distdir"`
	SeedDestination             string                  `json:"seed_destination"`
	ExpectedRootfsPath          string                  `json:"expected_rootfs_path"`
	QCOW2Filename               string                  `json:"qcow2_filename"`
	DiskSizeGiB                 int                     `json:"disk_size_gib"`
	GeneratedConfigDigest       string                  `json:"generated_config_digest"`
	GeneratedSpecDigest         string                  `json:"generated_spec_digest"`
	GeneratedEnvDigest          string                  `json:"generated_envscript_digest"`
	GeneratedFSScriptDigest     string                  `json:"generated_fsscript_digest"`
	GeneratedOverlayDigest      string                  `json:"generated_root_overlay_digest"`
}

type CatalystGateEvidence struct {
	SchemaVersion            int       `json:"schema_version"`
	CompletedAt              time.Time `json:"completed_at"`
	Target                   string    `json:"target"`
	PlanDigest               string    `json:"plan_digest"`
	InputLockDigest          string    `json:"input_lock_digest"`
	RepositoryCommit         string    `json:"repository_commit"`
	SnapshotID               string    `json:"snapshot_id"`
	Stage3SignatureVerified  bool      `json:"stage3_signature_verified"`
	RepositoryCommitVerified bool      `json:"repository_commit_verified"`
	ProfileCommitVerified    bool      `json:"profile_commit_verified"`
	ProfileParentsVerified   bool      `json:"profile_parents_verified"`
	NetworkNamespaceDenied   bool      `json:"network_namespace_denied"`
	FreshWorkDirectory       bool      `json:"fresh_work_directory"`
}

type CatalystRootfsManifest struct {
	SchemaVersion           int                     `json:"schema_version"`
	CreatedAt               time.Time               `json:"created_at"`
	Target                  string                  `json:"target"`
	RootfsID                string                  `json:"rootfs_id"`
	Generation              string                  `json:"generation"`
	Arch                    string                  `json:"arch"`
	ProfileID               string                  `json:"profile_id"`
	ProfilePath             string                  `json:"profile_path"`
	ProfileRepository       string                  `json:"profile_repository"`
	ProfileRepositoryCommit string                  `json:"profile_repository_commit,omitempty"`
	ProfileParents          []CatalystProfileParent `json:"profile_parents,omitempty"`
	MirrorBundleID          string                  `json:"mirror_bundle_id"`
	RepositoryCommit        string                  `json:"repository_commit"`
	SnapshotID              string                  `json:"snapshot_id"`
	PackageSets             []string                `json:"package_sets"`
	Packages                []string                `json:"packages"`
	PackageSetCatalogDigest string                  `json:"package_set_catalog_digest"`
	PlanDigest              string                  `json:"plan_digest"`
	InputLockDigest         string                  `json:"input_lock_digest"`
	ConfigDigest            string                  `json:"config_digest"`
	SpecDigest              string                  `json:"spec_digest"`
	EnvscriptDigest         string                  `json:"envscript_digest"`
	FSScriptDigest          string                  `json:"fsscript_digest"`
	RootOverlayDigest       string                  `json:"root_overlay_digest"`
	Inputs                  []CatalystInputEvidence `json:"inputs"`
	RootfsFilename          string                  `json:"rootfs_filename"`
	RootfsDigest            string                  `json:"rootfs_digest"`
	RootfsSize              int64                   `json:"rootfs_size"`
	QCOW2Filename           string                  `json:"qcow2_filename"`
	DiskSizeGiB             int                     `json:"disk_size_gib"`
	Gate                    CatalystGateEvidence    `json:"gate"`
}

type QCOW2Manifest struct {
	SchemaVersion        int       `json:"schema_version"`
	CreatedAt            time.Time `json:"created_at"`
	Target               string    `json:"target"`
	RootfsID             string    `json:"rootfs_id"`
	Generation           string    `json:"generation"`
	Arch                 string    `json:"arch"`
	ProfileID            string    `json:"profile_id"`
	RootfsManifestDigest string    `json:"rootfs_manifest_digest"`
	AssemblerDigest      string    `json:"assembler_digest"`
	QCOW2Filename        string    `json:"qcow2_filename"`
	QCOW2Digest          string    `json:"qcow2_digest"`
	QCOW2Size            int64     `json:"qcow2_size"`
	VirtualSizeGiB       int       `json:"virtual_size_gib"`
}

func LoadCatalystPlan(path string) (*CatalystPlan, error) {
	var plan CatalystPlan
	if err := decodeStrictFile(path, &plan); err != nil {
		return nil, err
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	return &plan, nil
}

func (p *CatalystPlan) Validate() error {
	for _, validate := range []func() error{
		p.validateIdentity,
		p.validateProfileContract,
		p.validateOutputContract,
		p.validatePackageSelection,
	} {
		if err := validate(); err != nil {
			return err
		}
	}
	return nil
}

func (p *CatalystPlan) validateIdentity() error {
	ids := []string{p.Target, p.PlanObjectID, p.RootfsID, p.Generation, p.ProfileID, p.MirrorBundleID, p.RepositoryObjectID, p.GentooRepositoryKeyObjectID,
		p.Stage3ObjectID, p.Stage3DigestsObjectID, p.ReleaseKeyObjectID, p.CatalystRuntimeObjectID,
		p.DistfileManifestObjectID, p.PackageSetCatalogObjectID}
	if p.SchemaVersion != 2 {
		return fmt.Errorf("unsupported Catalyst plan schema")
	}
	for _, id := range ids {
		if !objectIDPattern.MatchString(id) {
			return fmt.Errorf("catalyst plan has invalid ID %q", id)
		}
	}
	if p.ReleaseKeyObjectID == p.GentooRepositoryKeyObjectID {
		return fmt.Errorf("catalyst stage release and Gentoo repository keys must be distinct objects")
	}
	if (p.Target != catalystTarget && p.Target != catalystProfileTarget) || p.Arch != "amd64" || p.Subarch != "amd64" || p.RelType != "default" {
		return fmt.Errorf("catalyst lane supports only reviewed amd64/default systemd targets")
	}
	if !fullRevisionPattern.MatchString(p.Repositories["gentoo"]) {
		return fmt.Errorf("catalyst plan requires a full Gentoo commit")
	}
	return nil
}

func (p *CatalystPlan) validateProfileContract() error {
	switch p.Target {
	case catalystTarget:
		if p.ProfileRepository != "gentoo" || p.ProfilePath != catalystOfficialProfile || len(p.Repositories) != 1 || len(p.ProfileParents) != 0 || p.ProfileRepositoryObjectID != "" || p.ProfileRepositoryKeyObjectID != "" || p.RuntimeProfileRepositoryURI != "" {
			return fmt.Errorf("IMG-2 target requires only the official Gentoo systemd profile") //nolint:staticcheck // IMG-2 is a proper milestone name.
		}
	case catalystProfileTarget:
		if !repoComponentPattern.MatchString(p.ProfileRepository) || p.ProfileRepository == "gentoo" || !objectIDPattern.MatchString(p.ProfileRepositoryObjectID) || !objectIDPattern.MatchString(p.ProfileRepositoryKeyObjectID) || len(p.Repositories) != 2 || !fullRevisionPattern.MatchString(p.Repositories[p.ProfileRepository]) {
			return fmt.Errorf("IMG-3 external profile requires one named repository, full commit, bundle and verification key") //nolint:staticcheck // IMG-3 is a proper milestone name.
		}
		if !validProfilePath(p.ProfilePath) || len(p.ProfileParents) == 0 || len(p.ProfileParents) > 8 || p.RuntimeProfileRepositoryURI == "" {
			return fmt.Errorf("IMG-3 external profile path, parent chain, or runtime repository URI is invalid") //nolint:staticcheck // IMG-3 is a proper milestone name.
		}
		if p.ProfileRepositoryKeyObjectID == p.ReleaseKeyObjectID || p.ProfileRepositoryKeyObjectID == p.GentooRepositoryKeyObjectID {
			return fmt.Errorf("catalyst profile repository key must be independent from Gentoo trust objects")
		}
		seenParents := map[string]struct{}{}
		for _, parent := range p.ProfileParents {
			if !repoComponentPattern.MatchString(parent.Repository) || !validProfilePath(parent.ProfilePath) {
				return fmt.Errorf("invalid Catalyst external profile parent")
			}
			if _, exists := p.Repositories[parent.Repository]; !exists {
				return fmt.Errorf("profile parent repository %q is not pinned", parent.Repository)
			}
			line := parent.Repository + ":" + parent.ProfilePath
			if _, duplicate := seenParents[line]; duplicate {
				return fmt.Errorf("duplicate Catalyst profile parent %q", line)
			}
			seenParents[line] = struct{}{}
		}
	}
	return nil
}

func (p *CatalystPlan) validateOutputContract() error {
	for _, value := range []string{p.VersionStamp, p.SnapshotID, p.SeedFilename, p.OutputFilename, p.QCOW2Filename} {
		if !catalystNamePattern.MatchString(value) {
			return fmt.Errorf("catalyst plan has invalid filename or stamp %q", value)
		}
	}
	if p.SnapshotID == "stable" {
		return fmt.Errorf("catalyst snapshot_id stable is forbidden because it triggers a remote repository update")
	}
	if !strings.HasPrefix(p.SeedFilename, "stage3-amd64-") || !strings.HasPrefix(p.OutputFilename, "stage4-amd64-") || !strings.HasSuffix(p.OutputFilename, ".tar.xz") || !strings.HasSuffix(p.QCOW2Filename, ".qcow2") {
		return fmt.Errorf("catalyst seed/output filenames do not match the amd64 stage contract")
	}
	if p.OutputFilename != "stage4-"+p.Subarch+"-"+p.VersionStamp+".tar.xz" {
		return fmt.Errorf("catalyst output filename must be derived from target, subarch and version_stamp")
	}
	if p.DiskSizeGiB < 8 || p.DiskSizeGiB > 512 || p.Jobs < 1 || p.Jobs > 64 || len(p.PackageSets) == 0 || len(p.PackageSets) > 32 || len(p.Packages) > 128 {
		return fmt.Errorf("catalyst resources or package selection are outside approved bounds")
	}
	return nil
}

func (p *CatalystPlan) validatePackageSelection() error {
	seen := map[string]struct{}{}
	for _, id := range p.PackageSets {
		if !objectIDPattern.MatchString(id) {
			return fmt.Errorf("invalid package set ID %q", id)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("duplicate package set ID %q", id)
		}
		seen[id] = struct{}{}
	}
	for _, atom := range p.Packages {
		if !validPackageAtom(atom) {
			return fmt.Errorf("invalid package atom %q", atom)
		}
	}
	return nil
}

func validProfilePath(value string) bool {
	return objectIDPattern.MatchString(value) && !filepath.IsAbs(value) && value != "." && filepath.Clean(value) == value && !strings.HasPrefix(value, "../")
}

func PrepareCatalystPlan(planPath, lockPath, offlineRoot, workRoot string) (*CatalystPrepared, error) {
	plan, err := LoadCatalystPlan(planPath)
	if err != nil {
		return nil, fmt.Errorf("load Catalyst plan: %w", err)
	}
	lock, err := LoadInputLock(lockPath)
	if err != nil {
		return nil, fmt.Errorf("load input lock: %w", err)
	}
	if lock.BundleID != plan.MirrorBundleID {
		return nil, fmt.Errorf("input lock bundle %q does not match plan %q", lock.BundleID, plan.MirrorBundleID)
	}
	for name, endpoint := range map[string]string{"runtime_gentoo_repository_uri": plan.RuntimeGentooRepositoryURI, "runtime_gentoo_mirror": plan.RuntimeGentooMirror, "runtime_binhost": plan.RuntimeBinhost, "runtime_profile_repository_uri": plan.RuntimeProfileRepositoryURI} {
		if endpoint == "" && (name == "runtime_binhost" || name == "runtime_profile_repository_uri") {
			continue
		}
		if _, err := validateEndpoint(endpoint, lock.AllowedHosts); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	offlineRoot, err = filepath.Abs(offlineRoot)
	if err != nil {
		return nil, err
	}
	if report, err := Preflight(offlineRoot, lock, plan.Target); err != nil {
		return nil, fmt.Errorf("preflight Catalyst inputs: %w", err)
	} else if len(report.Missing) != 0 {
		return nil, fmt.Errorf("preflight Catalyst inputs: %d object(s) missing or invalid", len(report.Missing))
	}
	type requestedObject struct{ id, kind string }
	requested := []requestedObject{{plan.PlanObjectID, "catalyst-plan"}, {plan.RepositoryObjectID, "repository-snapshot"}, {plan.GentooRepositoryKeyObjectID, "release-key"}, {plan.Stage3ObjectID, "stage3"},
		{plan.Stage3DigestsObjectID, "stage3-digests"}, {plan.ReleaseKeyObjectID, "release-key"}, {plan.CatalystRuntimeObjectID, "catalyst-runtime"},
		{plan.DistfileManifestObjectID, "distfile-manifest"}, {plan.PackageSetCatalogObjectID, "package-set-catalog"}}
	if plan.Target == catalystProfileTarget {
		requested = append(requested, requestedObject{plan.ProfileRepositoryObjectID, "repository-snapshot"}, requestedObject{plan.ProfileRepositoryKeyObjectID, "release-key"})
	}
	objects := make(map[string]*InputObject, len(requested))
	inputs := make([]CatalystInputEvidence, 0, len(requested))
	for _, want := range requested {
		object, err := requiredObject(lock, want.id, plan.Target)
		if err != nil {
			return nil, err
		}
		if object.Kind != want.kind {
			return nil, fmt.Errorf("object %q must have kind %s", want.id, want.kind)
		}
		objects[want.id] = object
		inputs = append(inputs, CatalystInputEvidence{ID: object.ID, Kind: object.Kind, Path: object.Path, Digest: "sha256:" + object.SHA256, Size: object.Size})
	}
	if objects[plan.ReleaseKeyObjectID].SHA256 == objects[plan.GentooRepositoryKeyObjectID].SHA256 {
		return nil, fmt.Errorf("catalyst stage release and Gentoo repository keys have identical content")
	}
	if plan.Target == catalystProfileTarget {
		profileKeyDigest := objects[plan.ProfileRepositoryKeyObjectID].SHA256
		if profileKeyDigest == objects[plan.ReleaseKeyObjectID].SHA256 || profileKeyDigest == objects[plan.GentooRepositoryKeyObjectID].SHA256 {
			return nil, fmt.Errorf("catalyst profile repository key is not an independent trust object")
		}
	}
	planDigest, err := digestFile(planPath)
	if err != nil {
		return nil, err
	}
	if planDigest != objects[plan.PlanObjectID].SHA256 {
		return nil, fmt.Errorf("catalyst plan digest does not match locked object %q", plan.PlanObjectID)
	}
	lockDigest, err := digestFile(lockPath)
	if err != nil {
		return nil, err
	}
	catalogPath := filepath.Join(offlineRoot, objects[plan.PackageSetCatalogObjectID].Path)
	catalog, err := LoadPackageSetCatalog(catalogPath)
	if err != nil {
		return nil, fmt.Errorf("load package-set catalog: %w", err)
	}
	packages, err := catalog.Resolve(plan.PackageSets, plan.Packages)
	if err != nil {
		return nil, fmt.Errorf("resolve Catalyst package sets: %w", err)
	}
	closurePath := filepath.Join(offlineRoot, objects[plan.DistfileManifestObjectID].Path)
	if err := validateCatalystClosure(closurePath, plan, lock); err != nil {
		return nil, err
	}
	workRoot, err = filepath.Abs(workRoot)
	if err != nil || !generatedPathPattern.MatchString(workRoot) {
		return nil, fmt.Errorf("catalyst work root must be an absolute path without shell metacharacters")
	}
	if err := os.MkdirAll(workRoot, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(workRoot)
	if err != nil || len(entries) != 0 {
		return nil, fmt.Errorf("catalyst work root must be a fresh empty directory")
	}
	generated := filepath.Join(workRoot, "generated")
	overlay := filepath.Join(generated, "root-overlay")
	portageConfig := filepath.Join(generated, "portage")
	if err := os.MkdirAll(generated, 0o700); err != nil {
		return nil, err
	}
	// PrepareCatalystPlan runs under the offline runner's umask 077. MkdirAll's
	// requested mode is filtered by that umask and does not repair existing
	// intermediate directories, so chmod every directory explicitly. Portage's
	// fetch/userpriv processes must be able to traverse both config trees.
	generatedDirectories := []string{
		overlay,
		filepath.Join(overlay, "etc"),
		filepath.Join(overlay, "etc", "portage"),
		filepath.Join(overlay, "etc", "portage", "sets"),
		filepath.Join(overlay, "etc", "portage", "repos.conf"),
		filepath.Join(overlay, "etc", "portage", "package.use"),
		filepath.Join(overlay, "etc", "cloud"),
		filepath.Join(overlay, "etc", "cloud", "cloud.cfg.d"),
		portageConfig,
		filepath.Join(portageConfig, "package.use"),
	}
	for _, dir := range generatedDirectories {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			return nil, err
		}
	}
	runtimeRoot := filepath.Join(workRoot, "runtime")
	profileRepositorySource := filepath.Join(workRoot, "profile-repository")
	storedir := filepath.Join(workRoot, "storedir")
	reposStore := filepath.Join(workRoot, "repos")
	distdir := filepath.Join(workRoot, "distfiles")
	keyPath := filepath.Join(workRoot, "gentoo-repository.asc")
	envPath := filepath.Join(generated, "offline.env")
	fsscriptPath := filepath.Join(generated, "stage4-fsscript.sh")
	configPath := filepath.Join(generated, "catalyst.conf")
	specPath := filepath.Join(generated, "stage4.spec")
	seedDestination := filepath.Join(storedir, "builds", plan.RelType, plan.SeedFilename)
	expectedRootfs := filepath.Join(storedir, "builds", plan.RelType, plan.OutputFilename)
	if err := writeCatalystGeneratedFiles(plan, packages, runtimeRoot, storedir, reposStore, distdir, keyPath, envPath, fsscriptPath, configPath, specPath, overlay, portageConfig, profileRepositorySource); err != nil {
		return nil, err
	}
	digests := make([]string, 4)
	for i, path := range []string{configPath, specPath, envPath, fsscriptPath} {
		digest, err := digestFile(path)
		if err != nil {
			return nil, err
		}
		digests[i] = "sha256:" + digest
	}
	overlayDigest, err := digestRegularDirectory(overlay)
	if err != nil {
		return nil, err
	}
	prepared := &CatalystPrepared{SchemaVersion: 1, Target: plan.Target, PlanDigest: "sha256:" + planDigest, InputLockDigest: "sha256:" + lockDigest,
		MirrorBundleID: plan.MirrorBundleID, RepositoryCommit: plan.Repositories["gentoo"], SnapshotID: plan.SnapshotID, ProfileID: plan.ProfileID, ProfilePath: plan.ProfilePath,
		ProfileRepository: plan.ProfileRepository, ProfileParents: append([]CatalystProfileParent(nil), plan.ProfileParents...),
		PackageSets: append([]string(nil), plan.PackageSets...), Packages: packages, PackageSetCatalogDigest: "sha256:" + objects[plan.PackageSetCatalogObjectID].SHA256, Inputs: inputs,
		OfflineRoot: offlineRoot, WorkRoot: workRoot, RuntimeArchivePath: filepath.Join(offlineRoot, objects[plan.CatalystRuntimeObjectID].Path), RuntimeRoot: runtimeRoot,
		CatalystBinaryPath: filepath.Join(runtimeRoot, "bin", "catalyst"), CatalystSharedir: filepath.Join(runtimeRoot, "share", "catalyst"),
		RepositoryBundlePath: filepath.Join(offlineRoot, objects[plan.RepositoryObjectID].Path), GentooRepositoryKeyPath: filepath.Join(offlineRoot, objects[plan.GentooRepositoryKeyObjectID].Path), Stage3Path: filepath.Join(offlineRoot, objects[plan.Stage3ObjectID].Path),
		Stage3DigestsPath: filepath.Join(offlineRoot, objects[plan.Stage3DigestsObjectID].Path), ReleaseKeyPath: filepath.Join(offlineRoot, objects[plan.ReleaseKeyObjectID].Path),
		DistfileManifestPath: closurePath, PackageSetCatalogPath: catalogPath, ConfigPath: configPath, SpecPath: specPath, EnvscriptPath: envPath, FSScriptPath: fsscriptPath, RootOverlayPath: overlay,
		PortageConfigPath: portageConfig, StoreDir: storedir, ReposStoreDir: reposStore, DistDir: distdir, SeedDestination: seedDestination,
		ExpectedRootfsPath: expectedRootfs, QCOW2Filename: plan.QCOW2Filename, DiskSizeGiB: plan.DiskSizeGiB,
		GeneratedConfigDigest: digests[0], GeneratedSpecDigest: digests[1], GeneratedEnvDigest: digests[2], GeneratedFSScriptDigest: digests[3], GeneratedOverlayDigest: "sha256:" + overlayDigest}
	if plan.Target == catalystProfileTarget {
		prepared.ProfileRepositoryCommit = plan.Repositories[plan.ProfileRepository]
		prepared.ProfileRepositoryBundlePath = filepath.Join(offlineRoot, objects[plan.ProfileRepositoryObjectID].Path)
		prepared.ProfileRepositoryKeyPath = filepath.Join(offlineRoot, objects[plan.ProfileRepositoryKeyObjectID].Path)
		prepared.ProfileRepositorySourcePath = profileRepositorySource
	}
	return prepared, nil
}

func validateCatalystClosure(path string, plan *CatalystPlan, lock *InputLock) error {
	var closure ClosureManifest
	if err := decodeStrictFile(path, &closure); err != nil {
		return fmt.Errorf("load Catalyst distfile closure: %w", err)
	}
	if closure.SchemaVersion != 1 || closure.Target != plan.Target || closure.RepositoryCommit != plan.Repositories["gentoo"] || len(closure.Objects) == 0 {
		return fmt.Errorf("catalyst distfile closure does not match target and repository commit")
	}
	seen := map[string]struct{}{}
	for _, object := range closure.Objects {
		if filepath.Base(object.Filename) != object.Filename || object.Size < 0 || len(object.SHA256) != 64 || !fullRevisionPattern.MatchString(object.SHA256) {
			return fmt.Errorf("catalyst distfile closure contains invalid object %q", object.Filename)
		}
		if _, ok := seen[object.Filename]; ok {
			return fmt.Errorf("catalyst distfile closure contains duplicate filename %q", object.Filename)
		}
		seen[object.Filename] = struct{}{}
		if _, err := validateEndpoint(object.URI, lock.AllowedHosts); err != nil {
			return fmt.Errorf("catalyst distfile %q: %w", object.Filename, err)
		}
	}
	return nil
}

func writeCatalystGeneratedFiles(plan *CatalystPlan, packages []string, runtimeRoot, storedir, reposStore, distdir, keyPath, envPath, fsscriptPath, configPath, specPath, overlay, portageConfig, profileRepositorySource string) error {
	files := catalystGeneratedFileContents(plan, packages, runtimeRoot, storedir, reposStore, distdir, keyPath, envPath, fsscriptPath, configPath, specPath, overlay, portageConfig, profileRepositorySource)
	for path, content := range files {
		if err := WriteTextAtomic(path, content); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if strings.HasPrefix(path, overlay+string(filepath.Separator)) || strings.HasPrefix(path, portageConfig+string(filepath.Separator)) {
			mode = 0o644
		}
		if path == fsscriptPath {
			mode = 0o700
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
	}
	return nil
}

func catalystGeneratedFileContents(plan *CatalystPlan, packages []string, runtimeRoot, storedir, reposStore, distdir, keyPath, envPath, fsscriptPath, configPath, specPath, overlay, portageConfig, profileRepositorySource string) map[string]string {
	config := fmt.Sprintf("# Generated from locked CatalystPlan; do not edit.\ndigests = [\"sha256\", \"sha512\"]\ncompression_mode = \"xz\"\nenvscript = \"%s\"\noptions = [\"bindist\", \"sticky-config\", \"versioned_cache\"]\njobs = %d\nsharedir = \"%s\"\nstoredir = \"%s\"\nport_logdir = \"%s\"\ndistdir = \"%s\"\ntarget_distdir = \"/var/cache/distfiles\"\nrepos_storedir = \"%s\"\nrepo_basedir = \"/var/db/repos\"\nrepo_name = \"gentoo\"\nrepo_openpgp_key_path = \"%s\"\n", envPath, plan.Jobs, filepath.Join(runtimeRoot, "share", "catalyst"), storedir, filepath.Join(storedir, "logs"), distdir, reposStore, keyPath)
	seedStem := strings.TrimSuffix(plan.SeedFilename, ".tar.xz")
	seedStem = strings.TrimSuffix(seedStem, ".tar.bz2")
	profileSpec := plan.ProfilePath
	keepRepos := "gentoo"
	reposSpec := ""
	if plan.Target == catalystProfileTarget {
		profileSpec = plan.ProfileRepository + ":" + plan.ProfilePath
		keepRepos += " " + plan.ProfileRepository
		reposSpec = "repos: " + profileRepositorySource + "\n"
	}
	spec := fmt.Sprintf("# Generated from locked CatalystPlan; do not edit.\ntarget: stage4\nsubarch: %s\nversion_stamp: %s\nrel_type: %s\nprofile: %s\nsnapshot_treeish: %s\nsource_subpath: %s/%s\nportage_confdir: %s\n%skeep_repos: %s\nstage4/packages: %s\nstage4/fsscript: %s\nstage4/root_overlay: %s\n", plan.Subarch, plan.VersionStamp, plan.RelType, profileSpec, plan.SnapshotID, plan.RelType, seedStem, portageConfig, reposSpec, keepRepos, strings.Join(packages, " "), fsscriptPath, overlay)
	envscript := "# Executed inside the network-denied Catalyst namespace.\nunset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY all_proxy\nexport GENTOO_MIRRORS=\"\"\nexport PORTAGE_BINHOST=\"\"\nexport FEATURES=\"network-sandbox userpriv usersandbox\"\n"
	fsscript := fmt.Sprintf("#!/bin/sh\nset -eu\n# A stage4 inherits the stage3 VDB. Reconcile both that world and the reviewed\n# image package set under the selected profile before the artifact is sealed,\n# so Python targets/USE changes cannot be deferred invisibly to Packer.\nemerge --verbose --update --deep --newuse --with-bdeps=y @world\nemerge --verbose --update --deep --newuse --with-bdeps=y %s\nsystemctl enable systemd-networkd.service systemd-resolved.service sshd.service qemu-guest-agent.service\nfor unit in cloud-init-local.service cloud-init-network.service cloud-init-main.service cloud-init.service cloud-config.service cloud-final.service; do\n  if systemctl list-unit-files \"$unit\" --no-legend 2>/dev/null | grep -q \"^$unit\"; then\n    systemctl enable \"$unit\"\n  fi\ndone\nrm -f /etc/resolv.conf\nln -s ../run/systemd/resolve/stub-resolv.conf /etc/resolv.conf\nrm -f /etc/ssh/ssh_host_*\n: > /etc/machine-id\nchmod 0644 /etc/machine-id\nrm -rf /var/lib/cloud/* /var/log/journal/* /var/lib/systemd/random-seed\n", strings.Join(packages, " "))
	runtimeMakeConf := fmt.Sprintf("GENTOO_MIRRORS=\"%s\"\nPORTAGE_BINHOST=\"%s\"\nFEATURES=\"network-sandbox userpriv usersandbox\"\nGRUB_PLATFORMS=\"efi-64\"\n", plan.RuntimeGentooMirror, plan.RuntimeBinhost)
	runtimeRepo := fmt.Sprintf("[gentoo]\nlocation = /var/db/repos/gentoo\nsync-type = git\nsync-uri = %s\nauto-sync = no\n", plan.RuntimeGentooRepositoryURI)
	cloudConfig := "datasource_list: [ NoCloud, ConfigDrive ]\nssh_deletekeys: true\nssh_genkeytypes: [ed25519, rsa]\n"
	setContent := strings.Join(packages, "\n") + "\n"
	buildMakeConf := "FEATURES=\"network-sandbox userpriv usersandbox\"\nGENTOO_MIRRORS=\"\"\nPORTAGE_BINHOST=\"\"\nGRUB_PLATFORMS=\"efi-64\"\n"
	files := map[string]string{configPath: config, specPath: spec, envPath: envscript, fsscriptPath: fsscript,
		filepath.Join(overlay, "etc", "portage", "make.conf"):                          runtimeMakeConf,
		filepath.Join(overlay, "etc", "portage", "repos.conf", "gentoo.conf"):          runtimeRepo,
		filepath.Join(overlay, "etc", "portage", "sets", "portage-engine-image"):       setContent,
		filepath.Join(overlay, "etc", "cloud", "cloud.cfg.d", "90-portage-engine.cfg"): cloudConfig,
		filepath.Join(portageConfig, "make.conf"):                                      buildMakeConf}
	if slices.Contains(packages, "sys-kernel/gentoo-kernel-bin") {
		// gentoo-kernel-bin installs an initramfs through installkernel. Bind the
		// required backend in both the build config and the resulting image so a
		// later world update cannot silently reverse the image policy.
		installkernelUse := "sys-kernel/installkernel dracut\n"
		files[filepath.Join(portageConfig, "package.use", "portage-engine-image")] = installkernelUse
		files[filepath.Join(overlay, "etc", "portage", "package.use", "portage-engine-image")] = installkernelUse
	}
	if plan.Target == catalystProfileTarget {
		files[filepath.Join(overlay, "etc", "portage", "repos.conf", plan.ProfileRepository+".conf")] = fmt.Sprintf("[%s]\nlocation = /var/db/repos/%s\nsync-type = git\nsync-uri = %s\nauto-sync = no\n", plan.ProfileRepository, plan.ProfileRepository, plan.RuntimeProfileRepositoryURI)
	}
	return files
}

func digestRegularDirectory(root string) (string, error) {
	entries := []string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			entries = append(entries, "D\x00"+filepath.ToSlash(rel)+"\x00"+fmt.Sprintf("%04o", info.Mode().Perm()))
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("generated overlay contains non-regular entry %q", path)
		}
		digest, err := digestFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, "F\x00"+filepath.ToSlash(rel)+"\x00"+fmt.Sprintf("%04o", info.Mode().Perm())+"\x00"+digest)
		return nil
	}); err != nil {
		return "", err
	}
	slices.Sort(entries)
	temp, err := os.CreateTemp("", "portage-engine-overlay-digest-")
	if err != nil {
		return "", err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := temp.WriteString(strings.Join(entries, "\n")); err != nil {
		_ = temp.Close()
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	return digestFile(name)
}

func NewCatalystGateEvidence(prepared *CatalystPrepared, now time.Time) *CatalystGateEvidence {
	profileVerified := prepared.Target != catalystProfileTarget || prepared.ProfileRepositoryCommit != ""
	return &CatalystGateEvidence{SchemaVersion: 1, CompletedAt: now.UTC(), Target: prepared.Target, PlanDigest: prepared.PlanDigest,
		InputLockDigest: prepared.InputLockDigest, RepositoryCommit: prepared.RepositoryCommit, SnapshotID: prepared.SnapshotID,
		Stage3SignatureVerified: true, RepositoryCommitVerified: true, ProfileCommitVerified: profileVerified, ProfileParentsVerified: profileVerified,
		NetworkNamespaceDenied: true, FreshWorkDirectory: true}
}

func GenerateCatalystRootfsManifest(planPath, lockPath, preparedPath, gatePath, rootfsPath string, now time.Time) (*CatalystRootfsManifest, error) {
	plan, err := LoadCatalystPlan(planPath)
	if err != nil {
		return nil, err
	}
	var prepared CatalystPrepared
	if err := decodeStrictFile(preparedPath, &prepared); err != nil {
		return nil, err
	}
	var gate CatalystGateEvidence
	if err := decodeStrictFile(gatePath, &gate); err != nil {
		return nil, err
	}
	lock, err := LoadInputLock(lockPath)
	if err != nil {
		return nil, err
	}
	planDigest, err := digestFile(planPath)
	if err != nil {
		return nil, err
	}
	lockDigest, err := digestFile(lockPath)
	if err != nil {
		return nil, err
	}
	if prepared.SchemaVersion != 1 || gate.SchemaVersion != 1 || gate.CompletedAt.IsZero() || prepared.Target != plan.Target || gate.Target != prepared.Target || prepared.PlanDigest != gate.PlanDigest || prepared.InputLockDigest != gate.InputLockDigest ||
		prepared.PlanDigest != "sha256:"+planDigest || prepared.InputLockDigest != "sha256:"+lockDigest || lock.BundleID != plan.MirrorBundleID ||
		prepared.MirrorBundleID != plan.MirrorBundleID || prepared.RepositoryCommit != plan.Repositories["gentoo"] || prepared.RepositoryCommit != gate.RepositoryCommit ||
		prepared.SnapshotID != plan.SnapshotID || prepared.SnapshotID != gate.SnapshotID || prepared.ProfileID != plan.ProfileID || prepared.ProfilePath != plan.ProfilePath || prepared.ProfileRepository != plan.ProfileRepository ||
		!slices.Equal(prepared.ProfileParents, plan.ProfileParents) || !slices.Equal(prepared.PackageSets, plan.PackageSets) || !gate.Stage3SignatureVerified || !gate.RepositoryCommitVerified || !gate.ProfileCommitVerified || !gate.ProfileParentsVerified || !gate.NetworkNamespaceDenied || !gate.FreshWorkDirectory {
		return nil, fmt.Errorf("catalyst execution evidence is incomplete or inconsistent")
	}
	effectivePackages, err := validateCatalystPreparedInputs(plan, lock, &prepared)
	if err != nil {
		return nil, err
	}
	if err := validateCatalystGeneratedFiles(plan, &prepared, effectivePackages); err != nil {
		return nil, err
	}
	rootfsPath, err = filepath.Abs(rootfsPath)
	if err != nil || rootfsPath != prepared.ExpectedRootfsPath || filepath.Base(rootfsPath) != plan.OutputFilename {
		return nil, fmt.Errorf("rootfs path is not the plan's exact Catalyst output")
	}
	info, err := os.Stat(rootfsPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return nil, fmt.Errorf("catalyst rootfs output is missing or empty")
	}
	digest, err := digestFile(rootfsPath)
	if err != nil {
		return nil, err
	}
	return &CatalystRootfsManifest{SchemaVersion: 1, CreatedAt: now.UTC(), Target: plan.Target, RootfsID: plan.RootfsID, Generation: plan.Generation,
		Arch: plan.Arch, ProfileID: plan.ProfileID, ProfilePath: plan.ProfilePath, ProfileRepository: plan.ProfileRepository, ProfileRepositoryCommit: prepared.ProfileRepositoryCommit,
		ProfileParents: append([]CatalystProfileParent(nil), prepared.ProfileParents...), MirrorBundleID: plan.MirrorBundleID, RepositoryCommit: plan.Repositories["gentoo"], SnapshotID: plan.SnapshotID,
		PackageSets: append([]string(nil), prepared.PackageSets...), Packages: append([]string(nil), prepared.Packages...), PackageSetCatalogDigest: prepared.PackageSetCatalogDigest,
		PlanDigest: prepared.PlanDigest, InputLockDigest: prepared.InputLockDigest, ConfigDigest: prepared.GeneratedConfigDigest, SpecDigest: prepared.GeneratedSpecDigest,
		EnvscriptDigest: prepared.GeneratedEnvDigest, FSScriptDigest: prepared.GeneratedFSScriptDigest, RootOverlayDigest: prepared.GeneratedOverlayDigest,
		Inputs: append([]CatalystInputEvidence(nil), prepared.Inputs...), RootfsFilename: plan.OutputFilename, RootfsDigest: "sha256:" + digest, RootfsSize: info.Size(),
		QCOW2Filename: plan.QCOW2Filename, DiskSizeGiB: plan.DiskSizeGiB, Gate: gate}, nil
}

func validateCatalystPreparedInputs(plan *CatalystPlan, lock *InputLock, prepared *CatalystPrepared) ([]string, error) {
	generated := filepath.Join(prepared.WorkRoot, "generated")
	expectedPaths := map[string]string{
		prepared.RuntimeRoot:        filepath.Join(prepared.WorkRoot, "runtime"),
		prepared.CatalystBinaryPath: filepath.Join(prepared.WorkRoot, "runtime", "bin", "catalyst"),
		prepared.CatalystSharedir:   filepath.Join(prepared.WorkRoot, "runtime", "share", "catalyst"),
		prepared.ConfigPath:         filepath.Join(generated, "catalyst.conf"),
		prepared.SpecPath:           filepath.Join(generated, "stage4.spec"),
		prepared.EnvscriptPath:      filepath.Join(generated, "offline.env"),
		prepared.FSScriptPath:       filepath.Join(generated, "stage4-fsscript.sh"),
		prepared.RootOverlayPath:    filepath.Join(generated, "root-overlay"),
		prepared.PortageConfigPath:  filepath.Join(generated, "portage"),
		prepared.StoreDir:           filepath.Join(prepared.WorkRoot, "storedir"),
		prepared.ReposStoreDir:      filepath.Join(prepared.WorkRoot, "repos"),
		prepared.DistDir:            filepath.Join(prepared.WorkRoot, "distfiles"),
		prepared.SeedDestination:    filepath.Join(prepared.WorkRoot, "storedir", "builds", plan.RelType, plan.SeedFilename),
		prepared.ExpectedRootfsPath: filepath.Join(prepared.WorkRoot, "storedir", "builds", plan.RelType, plan.OutputFilename),
	}
	if plan.Target == catalystProfileTarget {
		expectedPaths[prepared.ProfileRepositorySourcePath] = filepath.Join(prepared.WorkRoot, "profile-repository")
		if prepared.ProfileRepositoryCommit != plan.Repositories[plan.ProfileRepository] {
			return nil, fmt.Errorf("catalyst external profile commit does not match the plan")
		}
	} else if prepared.ProfileRepositoryCommit != "" || prepared.ProfileRepositoryBundlePath != "" ||
		prepared.ProfileRepositoryKeyPath != "" || prepared.ProfileRepositorySourcePath != "" {
		return nil, fmt.Errorf("official Catalyst target contains unexpected external profile evidence")
	}
	if !generatedPathPattern.MatchString(prepared.WorkRoot) {
		return nil, fmt.Errorf("catalyst prepared work root is unsafe")
	}
	for actual, expected := range expectedPaths {
		if actual != expected {
			return nil, fmt.Errorf("catalyst prepared path %q does not match work root", actual)
		}
	}
	if err := validateCatalystInputEvidence(plan, lock, prepared); err != nil {
		return nil, err
	}
	catalog, err := LoadPackageSetCatalog(prepared.PackageSetCatalogPath)
	if err != nil {
		return nil, fmt.Errorf("reload package-set catalog: %w", err)
	}
	effectivePackages, err := catalog.Resolve(plan.PackageSets, plan.Packages)
	if err != nil || !slices.Equal(effectivePackages, prepared.Packages) {
		return nil, fmt.Errorf("catalyst package expansion no longer matches the plan")
	}
	return effectivePackages, nil
}

func validateCatalystInputEvidence(plan *CatalystPlan, lock *InputLock, prepared *CatalystPrepared) error {
	if report, err := Preflight(prepared.OfflineRoot, lock, plan.Target); err != nil || len(report.Missing) != 0 {
		return fmt.Errorf("catalyst locked inputs no longer pass preflight")
	}
	seenInputs := make(map[string]struct{}, len(prepared.Inputs))
	for _, input := range prepared.Inputs {
		if _, duplicate := seenInputs[input.ID]; duplicate {
			return fmt.Errorf("catalyst execution evidence contains duplicate input %q", input.ID)
		}
		seenInputs[input.ID] = struct{}{}
		object, err := requiredObject(lock, input.ID, plan.Target)
		if err != nil || object.Kind != input.Kind || object.Path != input.Path || object.Size != input.Size ||
			"sha256:"+object.SHA256 != input.Digest {
			return fmt.Errorf("catalyst input evidence %q does not match the lock", input.ID)
		}
	}
	expectedInputCount := 9
	if plan.Target == catalystProfileTarget {
		expectedInputCount = 11
	}
	if len(seenInputs) != expectedInputCount {
		return fmt.Errorf("catalyst input evidence is incomplete")
	}
	bindings := map[string]string{
		plan.CatalystRuntimeObjectID:     prepared.RuntimeArchivePath,
		plan.RepositoryObjectID:          prepared.RepositoryBundlePath,
		plan.GentooRepositoryKeyObjectID: prepared.GentooRepositoryKeyPath,
		plan.Stage3ObjectID:              prepared.Stage3Path,
		plan.Stage3DigestsObjectID:       prepared.Stage3DigestsPath,
		plan.ReleaseKeyObjectID:          prepared.ReleaseKeyPath,
		plan.DistfileManifestObjectID:    prepared.DistfileManifestPath,
		plan.PackageSetCatalogObjectID:   prepared.PackageSetCatalogPath,
	}
	if plan.Target == catalystProfileTarget {
		bindings[plan.ProfileRepositoryObjectID] = prepared.ProfileRepositoryBundlePath
		bindings[plan.ProfileRepositoryKeyObjectID] = prepared.ProfileRepositoryKeyPath
	}
	for id, actual := range bindings {
		object, err := requiredObject(lock, id, plan.Target)
		if err != nil {
			return err
		}
		if actual != filepath.Join(prepared.OfflineRoot, object.Path) {
			return fmt.Errorf("catalyst prepared input path %q does not match the lock", actual)
		}
	}
	return nil
}

func validateCatalystGeneratedFiles(plan *CatalystPlan, prepared *CatalystPrepared, effectivePackages []string) error {
	generated := filepath.Join(prepared.WorkRoot, "generated")
	expectedDigests := map[string]string{
		prepared.ConfigPath: prepared.GeneratedConfigDigest, prepared.SpecPath: prepared.GeneratedSpecDigest,
		prepared.EnvscriptPath: prepared.GeneratedEnvDigest, prepared.FSScriptPath: prepared.GeneratedFSScriptDigest,
	}
	expectedContents := catalystGeneratedFileContents(plan, effectivePackages, prepared.RuntimeRoot, prepared.StoreDir, prepared.ReposStoreDir, prepared.DistDir,
		filepath.Join(prepared.WorkRoot, "gentoo-repository.asc"), prepared.EnvscriptPath, prepared.FSScriptPath, prepared.ConfigPath, prepared.SpecPath,
		prepared.RootOverlayPath, prepared.PortageConfigPath, prepared.ProfileRepositorySourcePath)
	expectedDirModes := map[string]os.FileMode{
		generated:                0o700,
		prepared.RootOverlayPath: 0o755,
		filepath.Join(prepared.RootOverlayPath, "etc"):                           0o755,
		filepath.Join(prepared.RootOverlayPath, "etc", "portage"):                0o755,
		filepath.Join(prepared.RootOverlayPath, "etc", "portage", "sets"):        0o755,
		filepath.Join(prepared.RootOverlayPath, "etc", "portage", "repos.conf"):  0o755,
		filepath.Join(prepared.RootOverlayPath, "etc", "portage", "package.use"): 0o755,
		filepath.Join(prepared.RootOverlayPath, "etc", "cloud"):                  0o755,
		filepath.Join(prepared.RootOverlayPath, "etc", "cloud", "cloud.cfg.d"):   0o755,
		prepared.PortageConfigPath:                                               0o755,
		filepath.Join(prepared.PortageConfigPath, "package.use"):                 0o755,
	}
	expectedFileModes := make(map[string]os.FileMode, len(expectedContents))
	for path := range expectedContents {
		expectedFileModes[path] = 0o600
		if strings.HasPrefix(path, prepared.RootOverlayPath+string(filepath.Separator)) ||
			strings.HasPrefix(path, prepared.PortageConfigPath+string(filepath.Separator)) {
			expectedFileModes[path] = 0o644
		}
	}
	expectedFileModes[prepared.FSScriptPath] = 0o700
	if err := validateCatalystGeneratedTree(generated, expectedContents, expectedDirModes, expectedFileModes); err != nil {
		return err
	}
	for path, expected := range expectedDigests {
		digest, err := digestFile(path)
		if err != nil || "sha256:"+digest != expected {
			return fmt.Errorf("generated Catalyst file %q no longer matches prepared evidence", path)
		}
	}
	overlayDigest, err := digestRegularDirectory(prepared.RootOverlayPath)
	if err != nil || "sha256:"+overlayDigest != prepared.GeneratedOverlayDigest {
		return fmt.Errorf("generated Catalyst root overlay no longer matches prepared evidence")
	}
	return nil
}

func validateCatalystGeneratedTree(generated string, expectedContents map[string]string, expectedDirModes, expectedFileModes map[string]os.FileMode) error {
	if err := filepath.WalkDir(generated, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			expectedMode, expected := expectedDirModes[path]
			if !expected || info.Mode().Perm() != expectedMode {
				return fmt.Errorf("generated Catalyst directory %q has unexpected path or mode", path)
			}
			return nil
		}
		expectedMode, expected := expectedFileModes[path]
		if _, hasContent := expectedContents[path]; !expected || !hasContent || !info.Mode().IsRegular() || info.Mode().Perm() != expectedMode {
			return fmt.Errorf("generated Catalyst file %q has unexpected path, type, or mode", path)
		}
		return nil
	}); err != nil {
		return err
	}
	for path, expectedContent := range expectedContents {
		actual, err := os.ReadFile(path) // #nosec G304 -- paths were generated inside the fresh Catalyst work root.
		if err != nil || string(actual) != expectedContent {
			return fmt.Errorf("generated Catalyst file %q no longer matches the locked plan", path)
		}
	}
	return nil
}

func LoadCatalystRootfsManifest(path string) (*CatalystRootfsManifest, error) {
	var manifest CatalystRootfsManifest
	if err := decodeStrictFile(path, &manifest); err != nil {
		return nil, err
	}
	if err := validateCatalystRootfsManifest(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func validateCatalystRootfsManifest(manifest *CatalystRootfsManifest) error {
	digests := []string{manifest.RootfsDigest, manifest.PlanDigest, manifest.InputLockDigest, manifest.PackageSetCatalogDigest, manifest.ConfigDigest,
		manifest.SpecDigest, manifest.EnvscriptDigest, manifest.FSScriptDigest, manifest.RootOverlayDigest}
	for _, digest := range digests {
		if !prefixedSHA256Pattern.MatchString(digest) {
			return fmt.Errorf("catalyst rootfs manifest has an invalid digest")
		}
	}
	expectedInputs := 9
	profileValid := manifest.Target == catalystTarget && manifest.ProfileRepository == "gentoo" && manifest.ProfilePath == catalystOfficialProfile && manifest.ProfileRepositoryCommit == "" && len(manifest.ProfileParents) == 0
	if manifest.Target == catalystProfileTarget {
		expectedInputs = 11
		profileValid = repoComponentPattern.MatchString(manifest.ProfileRepository) && manifest.ProfileRepository != "gentoo" && validProfilePath(manifest.ProfilePath) && fullRevisionPattern.MatchString(manifest.ProfileRepositoryCommit) && len(manifest.ProfileParents) > 0
	}
	if manifest.SchemaVersion != 1 || manifest.CreatedAt.IsZero() || !profileValid || manifest.Arch != "amd64" ||
		!objectIDPattern.MatchString(manifest.RootfsID) || !objectIDPattern.MatchString(manifest.Generation) || !objectIDPattern.MatchString(manifest.ProfileID) ||
		!objectIDPattern.MatchString(manifest.MirrorBundleID) || !fullRevisionPattern.MatchString(manifest.RepositoryCommit) || !catalystNamePattern.MatchString(manifest.SnapshotID) ||
		len(manifest.PackageSets) == 0 || len(manifest.Packages) == 0 || len(manifest.Inputs) != expectedInputs || manifest.RootfsSize <= 0 || manifest.DiskSizeGiB < 8 || manifest.DiskSizeGiB > 512 ||
		!catalystNamePattern.MatchString(manifest.RootfsFilename) || !strings.HasSuffix(manifest.RootfsFilename, ".tar.xz") || !catalystNamePattern.MatchString(manifest.QCOW2Filename) || !strings.HasSuffix(manifest.QCOW2Filename, ".qcow2") ||
		manifest.Gate.SchemaVersion != 1 || manifest.Gate.CompletedAt.IsZero() || manifest.Gate.Target != manifest.Target || manifest.Gate.PlanDigest != manifest.PlanDigest ||
		manifest.Gate.InputLockDigest != manifest.InputLockDigest || manifest.Gate.RepositoryCommit != manifest.RepositoryCommit || manifest.Gate.SnapshotID != manifest.SnapshotID ||
		!manifest.Gate.Stage3SignatureVerified || !manifest.Gate.RepositoryCommitVerified || !manifest.Gate.ProfileCommitVerified || !manifest.Gate.ProfileParentsVerified || !manifest.Gate.NetworkNamespaceDenied || !manifest.Gate.FreshWorkDirectory {
		return fmt.Errorf("catalyst rootfs manifest is incomplete")
	}
	return nil
}

func GenerateQCOW2Manifest(rootfsManifestPath, qcow2Path, assemblerPath string, now time.Time) (*QCOW2Manifest, error) {
	rootfs, err := LoadCatalystRootfsManifest(rootfsManifestPath)
	if err != nil {
		return nil, err
	}
	if filepath.Base(qcow2Path) != rootfs.QCOW2Filename {
		return nil, fmt.Errorf("QCOW2 filename does not match rootfs manifest") //nolint:staticcheck // QCOW2 is a proper format name.
	}
	info, err := os.Stat(qcow2Path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return nil, fmt.Errorf("QCOW2 output is missing or empty") //nolint:staticcheck // QCOW2 is a proper format name.
	}
	rootfsManifestDigest, err := digestFile(rootfsManifestPath)
	if err != nil {
		return nil, err
	}
	qcow2Digest, err := digestFile(qcow2Path)
	if err != nil {
		return nil, err
	}
	assemblerDigest, err := digestFile(assemblerPath)
	if err != nil {
		return nil, err
	}
	return &QCOW2Manifest{SchemaVersion: 1, CreatedAt: now.UTC(), Target: rootfs.Target, RootfsID: rootfs.RootfsID, Generation: rootfs.Generation, Arch: rootfs.Arch,
		ProfileID: rootfs.ProfileID, RootfsManifestDigest: "sha256:" + rootfsManifestDigest, AssemblerDigest: "sha256:" + assemblerDigest,
		QCOW2Filename: rootfs.QCOW2Filename, QCOW2Digest: "sha256:" + qcow2Digest, QCOW2Size: info.Size(), VirtualSizeGiB: rootfs.DiskSizeGiB}, nil
}

func VerifyQCOW2Artifact(manifestPath, qcow2Path string) (string, error) {
	manifest, err := LoadQCOW2Manifest(manifestPath)
	if err != nil {
		return "", err
	}
	if filepath.Base(qcow2Path) != manifest.QCOW2Filename {
		return "", fmt.Errorf("QCOW2 filename does not match manifest") //nolint:staticcheck // QCOW2 is a proper format name.
	}
	digest, err := digestFile(qcow2Path)
	if err != nil {
		return "", err
	}
	if "sha256:"+digest != manifest.QCOW2Digest {
		return "", fmt.Errorf("QCOW2 digest does not match manifest") //nolint:staticcheck // QCOW2 is a proper format name.
	}
	manifestDigest, err := digestFile(manifestPath)
	if err != nil {
		return "", err
	}
	return "sha256:" + manifestDigest, nil
}

// LoadQCOW2Manifest validates provenance metadata without requiring the large
// QCOW2 payload to be present. Successor Packer bundles carry this small file
// as their source attestation and separately bind it to the immutable PVE
// template description.
func LoadQCOW2Manifest(manifestPath string) (*QCOW2Manifest, error) {
	var manifest QCOW2Manifest
	if err := decodeStrictFile(manifestPath, &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != 1 || manifest.CreatedAt.IsZero() || (manifest.Target != catalystTarget && manifest.Target != catalystProfileTarget) || manifest.Arch != "amd64" || !objectIDPattern.MatchString(manifest.RootfsID) ||
		!objectIDPattern.MatchString(manifest.Generation) || !objectIDPattern.MatchString(manifest.ProfileID) || !prefixedSHA256Pattern.MatchString(manifest.RootfsManifestDigest) ||
		!prefixedSHA256Pattern.MatchString(manifest.AssemblerDigest) || !prefixedSHA256Pattern.MatchString(manifest.QCOW2Digest) || manifest.QCOW2Size <= 0 ||
		manifest.VirtualSizeGiB < 8 || manifest.VirtualSizeGiB > 512 || filepath.Base(manifest.QCOW2Filename) != manifest.QCOW2Filename || !strings.HasSuffix(manifest.QCOW2Filename, ".qcow2") {
		return nil, fmt.Errorf("invalid QCOW2 manifest")
	}
	return &manifest, nil
}
