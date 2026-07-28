package imagefactory

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var fullRevisionPattern = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
var prefixedSHA256Pattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ImageManifest binds the intended spec to the exact offline lock and Packer
// result consumed by the build.
type ImageManifest struct {
	SchemaVersion            int                     `json:"schema_version"`
	CreatedAt                time.Time               `json:"created_at"`
	Target                   string                  `json:"target"`
	ImageID                  string                  `json:"image_id"`
	Generation               string                  `json:"generation"`
	Provider                 string                  `json:"provider"`
	Arch                     string                  `json:"arch"`
	BuildMode                string                  `json:"build_mode"`
	SourceTemplate           string                  `json:"source_template"`
	SourceVMID               int                     `json:"source_vmid"`
	SourceProvenanceObjectID string                  `json:"source_provenance_object_id"`
	SourceProvenanceDigest   string                  `json:"source_provenance_digest"`
	Template                 string                  `json:"template"`
	ProfileID                string                  `json:"profile_id"`
	ProfilePath              string                  `json:"profile_path"`
	ProfileRepository        string                  `json:"profile_repository"`
	ProfileParents           []CatalystProfileParent `json:"profile_parents,omitempty"`
	PackageSets              []string                `json:"package_sets"`
	PackageSetCatalogDigest  string                  `json:"package_set_catalog_digest"`
	MirrorBundleID           string                  `json:"mirror_bundle_id"`
	Repositories             map[string]string       `json:"repositories"`
	RootfsSource             string                  `json:"rootfs_source"`
	DisplayModel             string                  `json:"display_model"`
	Channel                  string                  `json:"channel"`
	InputLockDigest          string                  `json:"input_lock_digest"`
	CommonConfigDigest       string                  `json:"common_config_digest"`
	BuildPlanDigest          string                  `json:"build_plan_digest"`
	PackerManifestDigest     string                  `json:"packer_manifest_digest"`
	PackerArtifactID         string                  `json:"packer_artifact_id"`
	ImageDigest              string                  `json:"image_digest"`
	RootfsManifestDigest     string                  `json:"rootfs_manifest_digest"`
}

// CatalogFragment is a reviewable candidate that can be merged into the
// server catalog only after smoke evidence is attached.
type CatalogFragment struct {
	Profile CatalogProfileFragment `json:"profile"`
	Image   CatalogImageFragment   `json:"image"`
}

type CatalogProfileFragment struct {
	ID                  string                         `json:"id"`
	Arch                string                         `json:"arch"`
	ProfilePath         string                         `json:"profile_path"`
	ProfileRepositoryID string                         `json:"profile_repository_id"`
	Parents             []CatalogProfileParentFragment `json:"parents,omitempty"`
	RepositoryIDs       []string                       `json:"repository_ids"`
	ImageID             string                         `json:"image_id"`
	MirrorBundleID      string                         `json:"mirror_bundle_id"`
	Channel             string                         `json:"channel"`
}

type CatalogProfileParentFragment struct {
	RepositoryID string `json:"repository_id"`
	ProfilePath  string `json:"profile_path"`
}

type CatalogImageFragment struct {
	ID                      string   `json:"id"`
	ProfileID               string   `json:"profile_id"`
	Generation              string   `json:"generation"`
	Provider                string   `json:"provider"`
	Arch                    string   `json:"arch"`
	BuildMode               string   `json:"build_mode"`
	Template                string   `json:"template"`
	Digest                  string   `json:"digest"`
	RootfsSource            string   `json:"rootfs_source"`
	DisplayModel            string   `json:"display_model,omitempty"`
	RootfsManifestDigest    string   `json:"rootfs_manifest_digest"`
	PackageSets             []string `json:"package_sets,omitempty"`
	PackageSetCatalogDigest string   `json:"package_set_catalog_digest,omitempty"`
	Channel                 string   `json:"channel"`
}

func (m *ImageManifest) CatalogFragment() CatalogFragment {
	repositoryIDs := make([]string, 0, len(m.Repositories))
	for name := range m.Repositories {
		repositoryIDs = append(repositoryIDs, name)
	}
	slices.Sort(repositoryIDs)
	parents := make([]CatalogProfileParentFragment, 0, len(m.ProfileParents))
	for _, parent := range m.ProfileParents {
		parents = append(parents, CatalogProfileParentFragment{RepositoryID: parent.Repository, ProfilePath: parent.ProfilePath})
	}
	return CatalogFragment{
		Profile: CatalogProfileFragment{ID: m.ProfileID, Arch: m.Arch, ProfilePath: m.ProfilePath,
			ProfileRepositoryID: m.ProfileRepository, Parents: parents, RepositoryIDs: repositoryIDs, ImageID: m.ImageID, MirrorBundleID: m.MirrorBundleID, Channel: m.Channel},
		Image: CatalogImageFragment{ID: m.ImageID, ProfileID: m.ProfileID, Generation: m.Generation, Provider: m.Provider, Arch: m.Arch,
			BuildMode: m.BuildMode, Template: m.Template, Digest: m.ImageDigest, RootfsSource: m.RootfsSource, DisplayModel: m.DisplayModel,
			RootfsManifestDigest: m.RootfsManifestDigest, PackageSets: append([]string(nil), m.PackageSets...),
			PackageSetCatalogDigest: m.PackageSetCatalogDigest, Channel: m.Channel},
	}
}

func LoadImageManifest(path string) (*ImageManifest, error) {
	var manifest ImageManifest
	if err := decodeStrictFile(path, &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != 1 || manifest.CreatedAt.IsZero() || manifest.PackerArtifactID == "" ||
		!repoComponentPattern.MatchString(manifest.ProfileRepository) || manifest.Repositories[manifest.ProfileRepository] == "" || (manifest.ProfileRepository == "gentoo" && len(manifest.ProfileParents) != 0) || (manifest.ProfileRepository != "gentoo" && len(manifest.ProfileParents) == 0) ||
		!prefixedSHA256Pattern.MatchString(manifest.ImageDigest) || !prefixedSHA256Pattern.MatchString(manifest.InputLockDigest) ||
		!prefixedSHA256Pattern.MatchString(manifest.CommonConfigDigest) || !prefixedSHA256Pattern.MatchString(manifest.BuildPlanDigest) ||
		!prefixedSHA256Pattern.MatchString(manifest.PackerManifestDigest) || !prefixedSHA256Pattern.MatchString(manifest.SourceProvenanceDigest) ||
		!prefixedSHA256Pattern.MatchString(manifest.RootfsManifestDigest) || !prefixedSHA256Pattern.MatchString(manifest.PackageSetCatalogDigest) || len(manifest.PackageSets) == 0 {
		return nil, fmt.Errorf("candidate image manifest is incomplete")
	}
	return &manifest, nil
}

func (m *ImageManifest) ValidateForPlan(plan *BuildPlan) error {
	if m.Target != plan.Target || m.ImageID != plan.ImageID || m.Generation != plan.Generation || m.Template != plan.Template ||
		m.ProfileID != plan.ProfileID || m.ProfilePath != plan.ProfilePath || m.ProfileRepository != plan.ProfileRepository || !slices.Equal(m.ProfileParents, plan.ProfileParents) || m.SourceTemplate != plan.SourceTemplate ||
		m.SourceVMID != plan.SourceVMID || m.SourceProvenanceObjectID != plan.SourceProvenanceObjectID || m.MirrorBundleID != plan.MirrorBundleID ||
		m.Provider != plan.Provider || m.Arch != plan.Arch || m.BuildMode != plan.BuildMode || m.RootfsSource != plan.RootfsSource ||
		m.Channel != plan.Channel || m.DisplayModel != plan.DisplayModel || !maps.Equal(m.Repositories, plan.Repositories) || !slices.Equal(m.PackageSets, plan.PackageSets) {
		return fmt.Errorf("candidate image manifest does not match BuildPlan")
	}
	return nil
}

func (m *ImageManifest) ValidateEvidenceFiles(commonPath, planPath, lockPath string) error {
	plan, err := LoadBuildPlan(planPath)
	if err != nil {
		return err
	}
	lock, err := LoadInputLock(lockPath)
	if err != nil {
		return err
	}
	planDigest, err := digestFile(planPath)
	if err != nil {
		return err
	}
	lockDigest, err := digestFile(lockPath)
	if err != nil {
		return err
	}
	commonDigest, err := digestFile(commonPath)
	if err != nil {
		return err
	}
	if m.BuildPlanDigest != "sha256:"+planDigest || m.InputLockDigest != "sha256:"+lockDigest || m.CommonConfigDigest != "sha256:"+commonDigest {
		return fmt.Errorf("candidate manifest evidence digests do not match plan and lock")
	}
	planObject, err := requiredObject(lock, plan.PlanObjectID, plan.Target)
	if err != nil {
		return err
	}
	sourceObject, err := requiredObject(lock, plan.SourceProvenanceObjectID, plan.Target)
	if err != nil {
		return err
	}
	packageSetObject, err := uniqueObjectByKind(lock, "package-set-catalog", plan.Target)
	if err != nil {
		return err
	}
	expectedSourceKind := expectedSourceObjectKind(plan)
	if lock.BundleID != plan.MirrorBundleID || planObject.Kind != "build-plan" || planObject.SHA256 != planDigest || sourceObject.Kind != expectedSourceKind || m.SourceProvenanceDigest != "sha256:"+sourceObject.SHA256 || m.RootfsManifestDigest != "sha256:"+sourceObject.SHA256 || m.PackageSetCatalogDigest != "sha256:"+packageSetObject.SHA256 {
		return fmt.Errorf("candidate manifest provenance does not match locked inputs")
	}
	switch sourceObject.Kind {
	case "pbs-source-attestation":
		attestation, err := LoadPBSSourceAttestation(filepath.Join(filepath.Dir(lockPath), sourceObject.Path))
		if err != nil {
			return fmt.Errorf("load PBS source attestation: %w", err)
		}
		if err := attestation.ValidateForPlan(plan); err != nil {
			return err
		}
	case "image-manifest":
		if err := validateSourceImageManifest(filepath.Join(filepath.Dir(lockPath), sourceObject.Path), plan); err != nil {
			return err
		}
	}
	return nil
}

// GenerateManifest validates the plan and hashes all evidence inputs.
func GenerateManifest(commonPath, planPath, lockPath, packerManifestPath string, now time.Time) (*ImageManifest, error) {
	if _, err := LoadCommonConfig(commonPath); err != nil {
		return nil, fmt.Errorf("load common config: %w", err)
	}
	plan, err := LoadBuildPlan(planPath)
	if err != nil {
		return nil, fmt.Errorf("load build plan: %w", err)
	}
	lock, err := LoadInputLock(lockPath)
	if err != nil {
		return nil, fmt.Errorf("load input lock: %w", err)
	}
	if lock.BundleID != plan.MirrorBundleID {
		return nil, fmt.Errorf("input lock bundle %q does not match build plan %q", lock.BundleID, plan.MirrorBundleID)
	}
	planObject, err := requiredObject(lock, plan.PlanObjectID, plan.Target)
	if err != nil {
		return nil, err
	}
	sourceObject, err := requiredObject(lock, plan.SourceProvenanceObjectID, plan.Target)
	if err != nil {
		return nil, err
	}
	packageSetObject, err := uniqueObjectByKind(lock, "package-set-catalog", plan.Target)
	if err != nil {
		return nil, err
	}
	if planObject.Kind != "build-plan" {
		return nil, fmt.Errorf("BuildPlan object %q must have kind build-plan", plan.PlanObjectID)
	}
	expectedSourceKind := expectedSourceObjectKind(plan)
	if sourceObject.Kind != expectedSourceKind {
		return nil, fmt.Errorf("source provenance object %q must have kind %s", plan.SourceProvenanceObjectID, expectedSourceKind)
	}
	switch sourceObject.Kind {
	case "pbs-source-attestation":
		attestation, err := LoadPBSSourceAttestation(filepath.Join(filepath.Dir(lockPath), sourceObject.Path))
		if err != nil {
			return nil, fmt.Errorf("load PBS source attestation: %w", err)
		}
		if err := attestation.ValidateForPlan(plan); err != nil {
			return nil, err
		}
	case "image-manifest":
		if err := validateSourceImageManifest(filepath.Join(filepath.Dir(lockPath), sourceObject.Path), plan); err != nil {
			return nil, err
		}
	}
	var packerData map[string]any
	if err := decodeStrictFile(packerManifestPath, &packerData); err != nil {
		return nil, fmt.Errorf("load Packer manifest: %w", err)
	}
	planDigest, err := digestFile(planPath)
	if err != nil {
		return nil, err
	}
	if planDigest != planObject.SHA256 {
		return nil, fmt.Errorf("BuildPlan digest does not match locked object %q", plan.PlanObjectID)
	}
	lockDigest, err := digestFile(lockPath)
	if err != nil {
		return nil, err
	}
	commonDigest, err := digestFile(commonPath)
	if err != nil {
		return nil, err
	}
	artifactID, err := validatePackerResult(packerData, plan, "sha256:"+planDigest, "sha256:"+lockDigest, "sha256:"+commonDigest, "sha256:"+sourceObject.SHA256, "sha256:"+packageSetObject.SHA256)
	if err != nil {
		return nil, err
	}
	packerDigest, err := digestFile(packerManifestPath)
	if err != nil {
		return nil, err
	}
	imageDigestBytes := sha256.Sum256([]byte(commonDigest + "\n" + planDigest + "\n" + lockDigest + "\n" + packerDigest + "\n" + artifactID))
	return &ImageManifest{
		SchemaVersion: plan.SchemaVersion, CreatedAt: now.UTC(), Target: plan.Target, ImageID: plan.ImageID,
		Generation: plan.Generation, Provider: plan.Provider, Arch: plan.Arch,
		BuildMode: plan.BuildMode, SourceTemplate: plan.SourceTemplate, SourceVMID: plan.SourceVMID,
		SourceProvenanceObjectID: plan.SourceProvenanceObjectID, SourceProvenanceDigest: "sha256:" + sourceObject.SHA256,
		Template: plan.Template, ProfileID: plan.ProfileID, ProfilePath: plan.ProfilePath, ProfileRepository: plan.ProfileRepository, ProfileParents: append([]CatalystProfileParent(nil), plan.ProfileParents...),
		PackageSets: append([]string(nil), plan.PackageSets...), PackageSetCatalogDigest: "sha256:" + packageSetObject.SHA256, MirrorBundleID: plan.MirrorBundleID,
		Repositories: plan.Repositories, RootfsSource: plan.RootfsSource, DisplayModel: plan.DisplayModel, Channel: plan.Channel,
		InputLockDigest: "sha256:" + lockDigest, CommonConfigDigest: "sha256:" + commonDigest, PackerManifestDigest: "sha256:" + packerDigest,
		BuildPlanDigest: "sha256:" + planDigest, PackerArtifactID: artifactID,
		ImageDigest: "sha256:" + fmt.Sprintf("%x", imageDigestBytes[:]), RootfsManifestDigest: "sha256:" + sourceObject.SHA256,
	}, nil
}

func validatePackerResult(data map[string]any, plan *BuildPlan, planDigest, lockDigest, commonDigest, sourceDigest, packageSetCatalogDigest string) (string, error) {
	builds, ok := data["builds"].([]any)
	if !ok || len(builds) != 1 {
		return "", fmt.Errorf("packer manifest must contain exactly one build")
	}
	build, ok := builds[0].(map[string]any)
	if !ok {
		return "", fmt.Errorf("packer manifest build has an invalid shape")
	}
	artifactID, ok := build["artifact_id"].(string)
	if !ok || artifactID == "" {
		return "", fmt.Errorf("packer manifest build has no artifact_id")
	}
	customData, ok := build["custom_data"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("packer manifest build has no custom_data")
	}
	repositoryNames := make([]string, 0, len(plan.Repositories))
	for name := range plan.Repositories {
		repositoryNames = append(repositoryNames, name)
	}
	slices.Sort(repositoryNames)
	repositoryRevisions := make([]string, 0, len(repositoryNames))
	for _, name := range repositoryNames {
		repositoryRevisions = append(repositoryRevisions, plan.Repositories[name])
	}
	profileParents := make([]string, 0, len(plan.ProfileParents))
	for _, parent := range plan.ProfileParents {
		profileParents = append(profileParents, parent.Repository+":"+parent.ProfilePath)
	}
	expected := map[string]string{
		"image_generation":            plan.Generation,
		"mirror_bundle_id":            plan.MirrorBundleID,
		"profile_id":                  plan.ProfileID,
		"profile_path":                plan.ProfilePath,
		"profile_repository":          plan.ProfileRepository,
		"profile_parents":             strings.Join(profileParents, ","),
		"package_sets":                strings.Join(plan.PackageSets, ","),
		"package_set_catalog_digest":  packageSetCatalogDigest,
		"repository_names":            strings.Join(repositoryNames, ","),
		"repository_revisions":        strings.Join(repositoryRevisions, ","),
		"template_name":               plan.Template,
		"source_template":             plan.SourceTemplate,
		"source_vmid":                 strconv.Itoa(plan.SourceVMID),
		"source_provenance_object_id": plan.SourceProvenanceObjectID,
		"source_provenance_digest":    sourceDigest,
		"desktop":                     strconv.FormatBool(plan.Desktop),
		"display_model":               plan.DisplayModel,
		"build_plan_digest":           planDigest,
		"input_lock_digest":           lockDigest,
		"common_config_digest":        commonDigest,
	}
	for key, value := range expected {
		actual, ok := customData[key].(string)
		if !ok || actual != value {
			return "", fmt.Errorf("packer manifest custom_data %q does not match image spec", key)
		}
	}
	return artifactID, nil
}

// WriteJSONAtomic writes evidence without leaving a partial file.
func WriteJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func WriteTextAtomic(path, value string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
