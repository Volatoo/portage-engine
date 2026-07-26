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
	ResourceClasses      []catalog.ResourceClass    `json:"resource_classes,omitempty"`
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

// AssembleCandidateCatalog verifies every bundle and image evidence chain and
// returns a deterministic catalog. It never accepts hand-written repository,
// image, profile, or mirror metadata from the assembly spec.
func AssembleCandidateCatalog(specPath string, now time.Time) (*catalog.Catalog, error) {
	var spec CandidateCatalogAssembly
	if err := decodeStrictFile(specPath, &spec); err != nil {
		return nil, err
	}
	if spec.SchemaVersion != 1 || spec.CatalogVersion < 1 || !validOperationsID(spec.DefaultProfileID) || len(spec.Artifacts) == 0 {
		return nil, fmt.Errorf("candidate catalog assembly is incomplete")
	}
	root, err := filepath.Abs(filepath.Dir(specPath))
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	result := &catalog.Catalog{Version: spec.CatalogVersion, ResourceClasses: slices.Clone(spec.ResourceClasses)}
	repositories := map[string]catalog.RepositoryDefinition{}
	bundles := map[string]catalog.MirrorBundle{}
	profiles := map[string]catalog.ProfileDefinition{}
	images := map[string]catalog.ImageManifest{}

	for _, artifact := range spec.Artifacts {
		manifestPath, err := confinedEvidencePath(root, artifact.ImageManifest)
		if err != nil {
			return nil, err
		}
		planPath, err := confinedEvidencePath(root, artifact.BuildPlan)
		if err != nil {
			return nil, err
		}
		commonPath, err := confinedEvidencePath(root, artifact.CommonConfig)
		if err != nil {
			return nil, err
		}
		bundleManifestPath, err := confinedEvidencePath(root, artifact.BundleManifest)
		if err != nil {
			return nil, err
		}
		bundleSignaturePath, err := confinedEvidencePath(root, artifact.BundleSignature)
		if err != nil {
			return nil, err
		}
		bundlePublicKeyPath, err := confinedEvidencePath(root, artifact.BundlePublicKey)
		if err != nil {
			return nil, err
		}
		lockPath, err := confinedEvidencePath(root, artifact.InputLock)
		if err != nil {
			return nil, err
		}
		offlineRoot, err := confinedEvidenceDirectory(root, artifact.OfflineRoot)
		if err != nil {
			return nil, err
		}

		manifest, err := LoadImageManifest(manifestPath)
		if err != nil {
			return nil, fmt.Errorf("load candidate image manifest: %w", err)
		}
		plan, err := LoadBuildPlan(planPath)
		if err != nil {
			return nil, fmt.Errorf("load candidate BuildPlan: %w", err)
		}
		if manifest.Channel != "candidate" || plan.Channel != "candidate" {
			return nil, fmt.Errorf("candidate catalog assembly accepts only candidate-channel evidence")
		}
		if err := manifest.ValidateForPlan(plan); err != nil {
			return nil, fmt.Errorf("candidate image manifest does not match its BuildPlan: %w", err)
		}
		if err := manifest.ValidateEvidenceFiles(commonPath, planPath, lockPath); err != nil {
			return nil, err
		}
		bundle, err := VerifyBundle(bundleManifestPath, bundleSignaturePath, bundlePublicKeyPath, lockPath, offlineRoot, now)
		if err != nil {
			return nil, fmt.Errorf("verify candidate bundle: %w", err)
		}
		bundleDigest, err := canonicalDigest(bundle)
		if err != nil {
			return nil, err
		}
		if manifest.MirrorBundleID != bundle.BundleID || manifest.InputLockDigest != bundle.InputLockDigest {
			return nil, fmt.Errorf("candidate image %q does not bind its signed bundle", manifest.ImageID)
		}
		mirror := catalog.MirrorBundle{ID: bundle.BundleID, Digest: bundleDigest, CreatedAt: bundle.CreatedAt,
			FreshUntil: bundle.FreshUntil, AdvisoryWatermark: bundle.AdvisoryWatermark, Channel: "candidate"}
		if existing, ok := bundles[mirror.ID]; ok && existing != mirror {
			return nil, fmt.Errorf("bundle ID %q refers to multiple signed manifests", mirror.ID)
		}
		bundles[mirror.ID] = mirror

		lock, err := LoadInputLock(lockPath)
		if err != nil {
			return nil, err
		}
		repositoryIDs := make([]string, 0, len(plan.Repositories))
		repositoryIDByName := make(map[string]string, len(plan.Repositories))
		for id, revision := range plan.Repositories {
			objectID := plan.RepositoryObjectIDs[id]
			object, err := requiredObject(lock, objectID, plan.Target)
			if err != nil || object.Kind != "repository-snapshot" {
				return nil, fmt.Errorf("repository %q has no locked snapshot", id)
			}
			// Repository definitions are global catalog objects, while a release
			// group may legitimately contain images built from different immutable
			// revisions of the same logical repository. Scope the catalog ID by the
			// full revision and retain the Portage repository name separately.
			// Profiles then select exactly one revision-scoped object per name.
			catalogID := candidateRepositoryID(id, revision)
			repository := catalog.RepositoryDefinition{ID: catalogID, Name: id, Location: filepath.Join("/var/db/repos", id), SyncType: "git",
				SyncURI: plan.RepositoryURIs[id], Revision: revision, Digest: "sha256:" + object.SHA256, Channel: "candidate"}
			if existing, ok := repositories[catalogID]; ok && existing != repository {
				return nil, fmt.Errorf("repository %q revision %q differs between candidate images", id, revision)
			}
			repositories[catalogID] = repository
			repositoryIDs = append(repositoryIDs, catalogID)
			repositoryIDByName[id] = catalogID
		}
		sort.Strings(repositoryIDs)
		parents := make([]catalog.ProfileParentDefinition, 0, len(manifest.ProfileParents))
		for _, parent := range manifest.ProfileParents {
			parentRepositoryID, ok := repositoryIDByName[parent.Repository]
			if !ok {
				return nil, fmt.Errorf("profile parent repository %q has no revision-scoped catalog object", parent.Repository)
			}
			parents = append(parents, catalog.ProfileParentDefinition{RepositoryID: parentRepositoryID, ProfilePath: parent.ProfilePath})
		}
		profileRepositoryID, ok := repositoryIDByName[manifest.ProfileRepository]
		if !ok {
			return nil, fmt.Errorf("profile repository %q has no revision-scoped catalog object", manifest.ProfileRepository)
		}
		profile := catalog.ProfileDefinition{ID: manifest.ProfileID, Arch: manifest.Arch, ProfilePath: manifest.ProfilePath, BinhostPath: artifact.BinhostPath,
			ProfileRepositoryID: profileRepositoryID, Parents: parents, LegacyProfiles: slices.Clone(artifact.LegacyProfiles), RepositoryIDs: repositoryIDs,
			ImageID: manifest.ImageID, MirrorBundleID: manifest.MirrorBundleID, DefaultResourceClass: spec.DefaultResourceClass,
			Default: manifest.ProfileID == spec.DefaultProfileID, Channel: "candidate"}
		if _, duplicate := profiles[profile.ID]; duplicate {
			return nil, fmt.Errorf("duplicate candidate profile %q", profile.ID)
		}
		profiles[profile.ID] = profile
		image := catalog.ImageManifest{ID: manifest.ImageID, ProfileID: manifest.ProfileID, Generation: manifest.Generation, Provider: manifest.Provider,
			Arch: manifest.Arch, BuildMode: manifest.BuildMode, Template: manifest.Template, Digest: manifest.ImageDigest, RootfsSource: manifest.RootfsSource, DisplayModel: manifest.DisplayModel,
			RootfsManifestDigest: manifest.RootfsManifestDigest, PackageSetIDs: slices.Clone(manifest.PackageSets),
			PackageSetCatalogDigest: manifest.PackageSetCatalogDigest, Channel: "candidate"}
		if _, duplicate := images[image.ID]; duplicate {
			return nil, fmt.Errorf("duplicate candidate image %q", image.ID)
		}
		images[image.ID] = image
	}

	for _, value := range repositories {
		result.Repositories = append(result.Repositories, value)
	}
	for _, value := range bundles {
		result.MirrorBundles = append(result.MirrorBundles, value)
	}
	for _, value := range profiles {
		result.Profiles = append(result.Profiles, value)
	}
	for _, value := range images {
		result.Images = append(result.Images, value)
	}
	sort.Slice(result.Repositories, func(i, j int) bool { return result.Repositories[i].ID < result.Repositories[j].ID })
	sort.Slice(result.MirrorBundles, func(i, j int) bool { return result.MirrorBundles[i].ID < result.MirrorBundles[j].ID })
	sort.Slice(result.Profiles, func(i, j int) bool { return result.Profiles[i].ID < result.Profiles[j].ID })
	sort.Slice(result.Images, func(i, j int) bool { return result.Images[i].ID < result.Images[j].ID })
	sort.Slice(result.ResourceClasses, func(i, j int) bool { return result.ResourceClasses[i].ID < result.ResourceClasses[j].ID })
	if err := result.Validate(); err != nil {
		return nil, fmt.Errorf("assembled candidate catalog is invalid: %w", err)
	}
	return result, nil
}

func candidateRepositoryID(name, revision string) string {
	return name + "/rev-" + revision
}
