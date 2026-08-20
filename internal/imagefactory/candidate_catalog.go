package imagefactory

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/slchris/portage-engine/internal/catalog"
)

// CandidateCatalogAssembly is the reviewed recipe for turning independently
// built image evidence into one release-group candidate catalog.
type CandidateCatalogAssembly struct {
	SchemaVersion        int                        `json:"schema_version"`
	CatalogVersion       int                        `json:"catalog_version"`
	DefaultProfileID     string                     `json:"default_profile_id"`
	DefaultResourceClass string                     `json:"default_resource_class,omitempty"`
	RequiredFeatures     []string                   `json:"required_features"`
	ResourceClasses      []catalog.ResourceClass    `json:"resource_classes,omitempty"`
	DefaultEgressPolicy  string                     `json:"default_egress_policy_id"`
	EgressPolicies       []catalog.EgressPolicy     `json:"egress_policies"`
	Artifacts            []CandidateCatalogArtifact `json:"artifacts"`
}

type CandidateCatalogArtifact struct {
	ImageManifest   string   `json:"image_manifest"`
	BinhostPath     string   `json:"binhost_path"`
	BuildPlan       string   `json:"build_plan"`
	CommonConfig    string   `json:"common_config"`
	BundleManifest  string   `json:"bundle_manifest"`
	BundleSignature string   `json:"bundle_signature"`
	BundlePublicKey string   `json:"bundle_public_key"`
	InputLock       string   `json:"input_lock"`
	OfflineRoot     string   `json:"offline_root"`
	LegacyProfiles  []string `json:"legacy_profiles,omitempty"`
}

type candidateCatalogState struct {
	repositories map[string]catalog.RepositoryDefinition
	bundles      map[string]catalog.MirrorBundle
	profiles     map[string]catalog.ProfileDefinition
	images       map[string]catalog.ImageManifest
}

type candidateEvidencePaths struct {
	manifest, plan, common, bundleManifest string
	bundleSignature, bundlePublicKey       string
	lock, offlineRoot                      string
}

// AssembleCandidateCatalog verifies every bundle and image evidence chain and
// returns a deterministic catalog. It never accepts hand-written repository,
// image, profile, or mirror metadata from the assembly spec.
func AssembleCandidateCatalog(specPath string, now time.Time) (*catalog.Catalog, error) {
	var spec CandidateCatalogAssembly
	if err := decodeStrictFile(specPath, &spec); err != nil {
		return nil, err
	}
	result, err := newCandidateCatalog(spec)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(filepath.Dir(specPath))
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	state := candidateCatalogState{
		repositories: map[string]catalog.RepositoryDefinition{},
		bundles:      map[string]catalog.MirrorBundle{},
		profiles:     map[string]catalog.ProfileDefinition{},
		images:       map[string]catalog.ImageManifest{},
	}
	for _, artifact := range spec.Artifacts {
		if err := state.addArtifact(root, artifact, spec, now); err != nil {
			return nil, err
		}
	}
	for _, value := range state.repositories {
		result.Repositories = append(result.Repositories, value)
	}
	for _, value := range state.bundles {
		result.MirrorBundles = append(result.MirrorBundles, value)
	}
	for _, value := range state.profiles {
		result.Profiles = append(result.Profiles, value)
	}
	for _, value := range state.images {
		result.Images = append(result.Images, value)
	}
	sort.Slice(result.Repositories, func(i, j int) bool { return result.Repositories[i].ID < result.Repositories[j].ID })
	sort.Slice(result.MirrorBundles, func(i, j int) bool { return result.MirrorBundles[i].ID < result.MirrorBundles[j].ID })
	sort.Slice(result.Profiles, func(i, j int) bool { return result.Profiles[i].ID < result.Profiles[j].ID })
	sort.Slice(result.Images, func(i, j int) bool { return result.Images[i].ID < result.Images[j].ID })
	sort.Slice(result.ResourceClasses, func(i, j int) bool { return result.ResourceClasses[i].ID < result.ResourceClasses[j].ID })
	sort.Slice(result.EgressPolicies, func(i, j int) bool { return result.EgressPolicies[i].ID < result.EgressPolicies[j].ID })
	if err := result.Validate(); err != nil {
		return nil, fmt.Errorf("assembled candidate catalog is invalid: %w", err)
	}
	return result, nil
}

func resolveCandidateEvidence(root string, artifact CandidateCatalogArtifact) (candidateEvidencePaths, error) {
	var paths candidateEvidencePaths
	var err error
	fields := []struct {
		value string
		dest  *string
	}{
		{artifact.ImageManifest, &paths.manifest},
		{artifact.BuildPlan, &paths.plan},
		{artifact.CommonConfig, &paths.common},
		{artifact.BundleManifest, &paths.bundleManifest},
		{artifact.BundleSignature, &paths.bundleSignature},
		{artifact.BundlePublicKey, &paths.bundlePublicKey},
		{artifact.InputLock, &paths.lock},
	}
	for _, field := range fields {
		*field.dest, err = confinedEvidencePath(root, field.value)
		if err != nil {
			return candidateEvidencePaths{}, err
		}
	}
	paths.offlineRoot, err = confinedEvidenceDirectory(root, artifact.OfflineRoot)
	return paths, err
}

func (state *candidateCatalogState) addArtifact(
	root string,
	artifact CandidateCatalogArtifact,
	spec CandidateCatalogAssembly,
	now time.Time,
) error {
	paths, err := resolveCandidateEvidence(root, artifact)
	if err != nil {
		return err
	}
	manifest, err := LoadImageManifest(paths.manifest)
	if err != nil {
		return fmt.Errorf("load candidate image manifest: %w", err)
	}
	plan, err := LoadBuildPlan(paths.plan)
	if err != nil {
		return fmt.Errorf("load candidate BuildPlan: %w", err)
	}
	if manifest.Channel != "candidate" || plan.Channel != "candidate" {
		return fmt.Errorf("candidate catalog assembly accepts only candidate-channel evidence")
	}
	if err := manifest.ValidateForPlan(plan); err != nil {
		return fmt.Errorf("candidate image manifest does not match its BuildPlan: %w", err)
	}
	if err := manifest.ValidateEvidenceFiles(paths.common, paths.plan, paths.lock); err != nil {
		return err
	}
	bundle, err := VerifyBundle(
		paths.bundleManifest, paths.bundleSignature, paths.bundlePublicKey,
		paths.lock, paths.offlineRoot, now,
	)
	if err != nil {
		return fmt.Errorf("verify candidate bundle: %w", err)
	}
	if err := state.addBundle(manifest, bundle); err != nil {
		return err
	}
	lock, err := LoadInputLock(paths.lock)
	if err != nil {
		return err
	}
	repositoryIDs, repositoryIDByName, err := state.addRepositories(plan, lock)
	if err != nil {
		return err
	}
	return state.addProfileAndImage(
		artifact, spec, manifest, repositoryIDs, repositoryIDByName,
	)
}

func (state *candidateCatalogState) addBundle(
	manifest *ImageManifest,
	bundle *BundleManifest,
) error {
	bundleDigest, err := canonicalDigest(bundle)
	if err != nil {
		return err
	}
	if manifest.MirrorBundleID != bundle.BundleID ||
		manifest.InputLockDigest != bundle.InputLockDigest {
		return fmt.Errorf("candidate image %q does not bind its signed bundle", manifest.ImageID)
	}
	mirror := catalog.MirrorBundle{
		ID: bundle.BundleID, Digest: bundleDigest, CreatedAt: bundle.CreatedAt,
		FreshUntil: bundle.FreshUntil, AdvisoryWatermark: bundle.AdvisoryWatermark,
		Channel: "candidate",
	}
	if existing, ok := state.bundles[mirror.ID]; ok && existing != mirror {
		return fmt.Errorf("bundle ID %q refers to multiple signed manifests", mirror.ID)
	}
	state.bundles[mirror.ID] = mirror
	return nil
}

func (state *candidateCatalogState) addRepositories(
	plan *BuildPlan,
	lock *InputLock,
) ([]string, map[string]string, error) {
	repositoryIDs := make([]string, 0, len(plan.Repositories))
	byName := make(map[string]string, len(plan.Repositories))
	for id, revision := range plan.Repositories {
		object, err := requiredObject(lock, plan.RepositoryObjectIDs[id], plan.Target)
		if err != nil || object.Kind != "repository-snapshot" {
			return nil, nil, fmt.Errorf("repository %q has no locked snapshot", id)
		}
		catalogID := candidateRepositoryID(id, revision)
		repository := catalog.RepositoryDefinition{
			ID: catalogID, Name: id, Location: filepath.Join("/var/db/repos", id),
			SyncType: "git", SyncURI: plan.RepositoryURIs[id],
			Revision: revision, Digest: "sha256:" + object.SHA256, Channel: "candidate",
		}
		if existing, ok := state.repositories[catalogID]; ok && existing != repository {
			return nil, nil, fmt.Errorf(
				"repository %q revision %q differs between candidate images",
				id, revision,
			)
		}
		state.repositories[catalogID] = repository
		repositoryIDs = append(repositoryIDs, catalogID)
		byName[id] = catalogID
	}
	sort.Strings(repositoryIDs)
	return repositoryIDs, byName, nil
}

func (state *candidateCatalogState) addProfileAndImage(
	artifact CandidateCatalogArtifact,
	spec CandidateCatalogAssembly,
	manifest *ImageManifest,
	repositoryIDs []string,
	repositoryIDByName map[string]string,
) error {
	parents := make([]catalog.ProfileParentDefinition, 0, len(manifest.ProfileParents))
	for _, parent := range manifest.ProfileParents {
		parentRepositoryID, ok := repositoryIDByName[parent.Repository]
		if !ok {
			return fmt.Errorf(
				"profile parent repository %q has no revision-scoped catalog object",
				parent.Repository,
			)
		}
		parents = append(parents, catalog.ProfileParentDefinition{
			RepositoryID: parentRepositoryID, ProfilePath: parent.ProfilePath,
		})
	}
	profileRepositoryID, ok := repositoryIDByName[manifest.ProfileRepository]
	if !ok {
		return fmt.Errorf(
			"profile repository %q has no revision-scoped catalog object",
			manifest.ProfileRepository,
		)
	}
	profile := catalog.ProfileDefinition{
		ID: manifest.ProfileID, Arch: manifest.Arch, ProfilePath: manifest.ProfilePath,
		BinhostPath: artifact.BinhostPath, ProfileRepositoryID: profileRepositoryID,
		Parents: parents, LegacyProfiles: slices.Clone(artifact.LegacyProfiles),
		RepositoryIDs: repositoryIDs, ImageID: manifest.ImageID,
		MirrorBundleID:       manifest.MirrorBundleID,
		DefaultResourceClass: spec.DefaultResourceClass,
		RequiredFeatures:     slices.Clone(spec.RequiredFeatures),
		EgressPolicyID:       spec.DefaultEgressPolicy,
		Default:              manifest.ProfileID == spec.DefaultProfileID, Channel: "candidate",
	}
	if _, duplicate := state.profiles[profile.ID]; duplicate {
		return fmt.Errorf("duplicate candidate profile %q", profile.ID)
	}
	state.profiles[profile.ID] = profile
	image := catalog.ImageManifest{
		ID: manifest.ImageID, ProfileID: manifest.ProfileID,
		Generation: manifest.Generation, Provider: manifest.Provider,
		Arch: manifest.Arch, BuildMode: manifest.BuildMode,
		Template: manifest.Template, Digest: manifest.ImageDigest,
		RootfsSource: manifest.RootfsSource, DisplayModel: manifest.DisplayModel,
		RootfsManifestDigest:    manifest.RootfsManifestDigest,
		PackageSetIDs:           slices.Clone(manifest.PackageSets),
		PackageSetCatalogDigest: manifest.PackageSetCatalogDigest,
		Channel:                 "candidate",
	}
	if _, duplicate := state.images[image.ID]; duplicate {
		return fmt.Errorf("duplicate candidate image %q", image.ID)
	}
	state.images[image.ID] = image
	return nil
}

func newCandidateCatalog(spec CandidateCatalogAssembly) (*catalog.Catalog, error) {
	if spec.SchemaVersion != 1 || spec.CatalogVersion < 1 || !validOperationsID(spec.DefaultProfileID) ||
		!validOperationsID(spec.DefaultEgressPolicy) || len(spec.EgressPolicies) == 0 || len(spec.Artifacts) == 0 {
		return nil, fmt.Errorf("candidate catalog assembly is incomplete")
	}
	result := &catalog.Catalog{
		Version: spec.CatalogVersion, ResourceClasses: slices.Clone(spec.ResourceClasses),
		EgressPolicies: cloneEgressPolicies(spec.EgressPolicies),
	}
	for index := range result.EgressPolicies {
		if result.EgressPolicies[index].Channel != "candidate" {
			return nil, fmt.Errorf("candidate catalog assembly accepts only candidate-channel egress policies")
		}
	}
	return result, nil
}

func cloneEgressPolicies(values []catalog.EgressPolicy) []catalog.EgressPolicy {
	result := make([]catalog.EgressPolicy, len(values))
	for index, policy := range values {
		result[index] = policy
		result[index].DNSResolvers = slices.Clone(policy.DNSResolvers)
		result[index].Rules = make([]catalog.EgressRule, len(policy.Rules))
		for ruleIndex, rule := range policy.Rules {
			result[index].Rules[ruleIndex] = rule
			result[index].Rules[ruleIndex].Hosts = slices.Clone(rule.Hosts)
			result[index].Rules[ruleIndex].CIDRs = slices.Clone(rule.CIDRs)
			result[index].Rules[ruleIndex].Ports = slices.Clone(rule.Ports)
		}
	}
	return result
}

func candidateRepositoryID(name, revision string) string {
	return name + "/rev-" + revision
}
