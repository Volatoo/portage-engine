// Package builder provides configuration transfer capabilities.
package builder

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PortageConfig represents user's Portage configuration.
type PortageConfig struct {
	// Package.use entries
	PackageUse map[string][]string `json:"package_use"`
	// Package.accept_keywords entries
	PackageKeywords map[string][]string `json:"package_keywords"`
	// Package.mask entries
	PackageMask []string `json:"package_mask"`
	// Package.unmask entries
	PackageUnmask []string `json:"package_unmask"`
	// Make.conf settings
	MakeConf map[string]string `json:"make_conf"`
	// Environment variables
	Environment map[string]string `json:"environment"`
	// Global USE flags
	GlobalUse []string `json:"global_use"`
	// Repository configurations
	Repos []RepoConfig `json:"repos"`
}

// RepoConfig represents a repository configuration.
type RepoConfig struct {
	RegistryID string `json:"registry_id,omitempty"`
	Name       string `json:"name"`
	Location   string `json:"location"`
	SyncType   string `json:"sync_type"`
	SyncURI    string `json:"sync_uri"`
	Revision   string `json:"revision,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Priority   int    `json:"priority"`
}

// BuildPackageSpec specifies packages to build with their configurations.
type BuildPackageSpec struct {
	Packages []PackageSpec `json:"packages"`
}

// PackageSpec represents a single package build specification.
type PackageSpec struct {
	Atom        string            `json:"atom"` // e.g., "dev-lang/python:3.11"
	Version     string            `json:"version,omitempty"`
	UseFlags    []string          `json:"use_flags,omitempty"`
	Keywords    []string          `json:"keywords,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
}

// ConfigBundle bundles Portage configuration and package specifications.
type ConfigBundle struct {
	Config   *PortageConfig    `json:"config"`
	Packages *BuildPackageSpec `json:"packages"`
	Metadata BundleMetadata    `json:"metadata"`
}

// BundleMetadata contains metadata about the configuration bundle.
type BundleMetadata struct {
	UserID                string             `json:"user_id"`
	TargetArch            string             `json:"target_arch"`
	ProfileID             string             `json:"profile_id,omitempty"`
	Profile               string             `json:"profile"`
	ProfileRepositoryID   string             `json:"profile_repository_id,omitempty"`
	ProfileRepositoryName string             `json:"profile_repository_name,omitempty"`
	ProfileParents        []ProfileParentRef `json:"profile_parents,omitempty"`
	CreatedAt             string             `json:"created_at"`
	Description           string             `json:"description,omitempty"`
}

// ProfileParentRef is the resolved, server-owned parent chain expected in the
// selected external profile's parent file.
type ProfileParentRef struct {
	RepositoryID   string `json:"repository_id"`
	RepositoryName string `json:"repository_name"`
	ProfilePath    string `json:"profile_path"`
}

// ConfigTransfer handles configuration transfer operations.
type ConfigTransfer struct {
	workDir string
}

// NewConfigTransfer creates a new configuration transfer handler.
func NewConfigTransfer(workDir string) *ConfigTransfer {
	return &ConfigTransfer{
		workDir: workDir,
	}
}

// ReadSystemPortageConfig reads Portage configuration from /etc/portage directory.
// This captures the user's actual system configuration for maximum consistency.
func (ct *ConfigTransfer) ReadSystemPortageConfig(portageDir string) (*PortageConfig, error) {
	if portageDir == "" {
		portageDir = "/etc/portage"
	}

	// Check if portage directory exists
	if _, err := os.Stat(portageDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("portage directory not found: %s", portageDir)
	}

	config := &PortageConfig{
		PackageUse:      make(map[string][]string),
		PackageKeywords: make(map[string][]string),
		PackageMask:     []string{},
		PackageUnmask:   []string{},
		MakeConf:        make(map[string]string),
		Environment:     make(map[string]string),
		GlobalUse:       []string{},
		Repos:           []RepoConfig{},
	}

	// Read make.conf
	if err := ct.readMakeConf(filepath.Join(portageDir, "make.conf"), config); err != nil {
		// Try /etc/make.conf as fallback
		_ = ct.readMakeConf("/etc/make.conf", config)
	}

	// Read package.use (can be file or directory)
	_ = ct.readPackageUse(filepath.Join(portageDir, "package.use"), config)

	// Read package.accept_keywords
	_ = ct.readPackageKeywords(filepath.Join(portageDir, "package.accept_keywords"), config)

	// Read package.mask
	_ = ct.readPackageMask(filepath.Join(portageDir, "package.mask"), config)

	// Read package.unmask
	_ = ct.readPackageUnmask(filepath.Join(portageDir, "package.unmask"), config)

	// Read repos.conf
	_ = ct.readReposConf(filepath.Join(portageDir, "repos.conf"), config)

	return config, nil
}

// readMakeConf reads make.conf file.
func (ct *ConfigTransfer) readMakeConf(path string, config *PortageConfig) error {
	data, err := os.ReadFile(path) // #nosec G304 -- path is discovered beneath the user-selected /etc/portage import root.
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Parse VAR="value" or VAR=value
		if strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.Trim(strings.TrimSpace(parts[1]), "\"'")
				config.MakeConf[key] = value

				// Extract global USE flags
				if key == "USE" {
					config.GlobalUse = strings.Fields(value)
				}
			}
		}
	}

	return nil
}

// readPackageUse reads package.use file or directory.
func (ct *ConfigTransfer) readPackageUse(path string, config *PortageConfig) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if info.IsDir() {
		// Read all files in directory
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
				_ = ct.parsePackageUseFile(filepath.Join(path, entry.Name()), config)
			}
		}
	} else {
		// Read single file
		return ct.parsePackageUseFile(path, config)
	}

	return nil
}

// parsePackageUseFile parses a package.use file.
func (ct *ConfigTransfer) parsePackageUseFile(path string, config *PortageConfig) error {
	data, err := os.ReadFile(path) // #nosec G304 -- path is discovered beneath the user-selected /etc/portage import root.
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Parse: package-atom use-flags
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			pkgAtom := fields[0]
			useFlags := fields[1:]
			config.PackageUse[pkgAtom] = append(config.PackageUse[pkgAtom], useFlags...)
		}
	}

	return nil
}

// readPackageKeywords reads package.accept_keywords file or directory.
func (ct *ConfigTransfer) readPackageKeywords(path string, config *PortageConfig) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
				_ = ct.parsePackageKeywordsFile(filepath.Join(path, entry.Name()), config)
			}
		}
	} else {
		return ct.parsePackageKeywordsFile(path, config)
	}

	return nil
}

// parsePackageKeywordsFile parses a package.accept_keywords file.
func (ct *ConfigTransfer) parsePackageKeywordsFile(path string, config *PortageConfig) error {
	data, err := os.ReadFile(path) // #nosec G304 -- path is discovered beneath the user-selected /etc/portage import root.
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) >= 2 {
			pkgAtom := fields[0]
			keywords := fields[1:]
			config.PackageKeywords[pkgAtom] = append(config.PackageKeywords[pkgAtom], keywords...)
		}
	}

	return nil
}

// readPackageMask reads package.mask file or directory.
func (ct *ConfigTransfer) readPackageMask(path string, config *PortageConfig) error {
	return ct.readPackageList(path, &config.PackageMask)
}

// readPackageUnmask reads package.unmask file or directory.
func (ct *ConfigTransfer) readPackageUnmask(path string, config *PortageConfig) error {
	return ct.readPackageList(path, &config.PackageUnmask)
}

// readPackageList reads a list of package atoms from file or directory.
func (ct *ConfigTransfer) readPackageList(path string, list *[]string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
				_ = ct.parsePackageListFile(filepath.Join(path, entry.Name()), list)
			}
		}
	} else {
		return ct.parsePackageListFile(path, list)
	}

	return nil
}

// parsePackageListFile parses a file containing package atoms.
func (ct *ConfigTransfer) parsePackageListFile(path string, list *[]string) error {
	data, err := os.ReadFile(path) // #nosec G304 -- path is discovered beneath the user-selected /etc/portage import root.
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		*list = append(*list, line)
	}

	return nil
}

// readReposConf reads repository configurations.
func (ct *ConfigTransfer) readReposConf(path string, config *PortageConfig) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && strings.HasSuffix(entry.Name(), ".conf") {
				_ = ct.parseRepoConfFile(filepath.Join(path, entry.Name()), config)
			}
		}
	} else {
		return ct.parseRepoConfFile(path, config)
	}

	return nil
}

// parseRepoConfFile parses a repository configuration file.
func (ct *ConfigTransfer) parseRepoConfFile(path string, config *PortageConfig) error {
	data, err := os.ReadFile(path) // #nosec G304 -- path is discovered beneath the user-selected /etc/portage import root.
	if err != nil {
		return err
	}

	var currentRepo *RepoConfig
	lines := strings.Split(string(data), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if ct.isCommentOrEmpty(line) {
			continue
		}

		if ct.isSectionHeader(line) {
			currentRepo = ct.handleSectionHeader(line, currentRepo, config)
			continue
		}

		ct.parseRepoConfigLine(line, currentRepo)
	}

	ct.addFinalRepo(currentRepo, config)
	return nil
}

// isCommentOrEmpty checks if a line is a comment or empty.
func (ct *ConfigTransfer) isCommentOrEmpty(line string) bool {
	return line == "" || strings.HasPrefix(line, "#")
}

// isSectionHeader checks if a line is a section header.
func (ct *ConfigTransfer) isSectionHeader(line string) bool {
	return strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]")
}

// handleSectionHeader processes a section header and returns new current repo.
func (ct *ConfigTransfer) handleSectionHeader(line string, currentRepo *RepoConfig, config *PortageConfig) *RepoConfig {
	if currentRepo != nil {
		config.Repos = append(config.Repos, *currentRepo)
	}
	repoName := strings.Trim(line, "[]")
	return &RepoConfig{Name: repoName}
}

// parseRepoConfigLine parses a key=value line in repo config.
func (ct *ConfigTransfer) parseRepoConfigLine(line string, currentRepo *RepoConfig) {
	if currentRepo == nil || !strings.Contains(line, "=") {
		return
	}

	parts := strings.SplitN(line, "=", 2)
	if len(parts) != 2 {
		return
	}

	key := strings.TrimSpace(parts[0])
	value := strings.TrimSpace(parts[1])
	ct.setRepoConfigField(key, value, currentRepo)
}

// setRepoConfigField sets a field in repo config based on key.
func (ct *ConfigTransfer) setRepoConfigField(key, value string, repo *RepoConfig) {
	switch key {
	case "location":
		repo.Location = value
	case "sync-type":
		repo.SyncType = value
	case "sync-uri":
		repo.SyncURI = value
	case "priority":
		_, _ = fmt.Sscanf(value, "%d", &repo.Priority)
	}
}

// addFinalRepo adds the last repo to the config.
func (ct *ConfigTransfer) addFinalRepo(currentRepo *RepoConfig, config *PortageConfig) {
	if currentRepo != nil {
		config.Repos = append(config.Repos, *currentRepo)
	}
}

// CreateConfigBundle creates a configuration bundle from user's Portage setup.
func (ct *ConfigTransfer) CreateConfigBundle(
	config *PortageConfig,
	packages *BuildPackageSpec,
	metadata BundleMetadata,
) (*ConfigBundle, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if packages == nil {
		return nil, fmt.Errorf("packages cannot be nil")
	}

	bundle := &ConfigBundle{
		Config:   config,
		Packages: packages,
		Metadata: metadata,
	}
	if err := validateBundle(bundle); err != nil {
		return nil, fmt.Errorf("invalid config bundle: %w", err)
	}

	return bundle, nil
}

// ExportBundle exports the configuration bundle to a tarball.
func (ct *ConfigTransfer) ExportBundle(bundle *ConfigBundle, outputPath string) error {
	if err := validateBundle(bundle); err != nil {
		return fmt.Errorf("invalid config bundle: %w", err)
	}

	// Create output file
	outFile, err := os.Create(outputPath) // #nosec G304 -- outputPath is the caller-selected bundle destination.
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer func() {
		_ = outFile.Close()
	}()

	// Create gzip writer
	gzWriter := gzip.NewWriter(outFile)
	defer func() {
		_ = gzWriter.Close()
	}()

	// Create tar writer
	tarWriter := tar.NewWriter(gzWriter)
	defer func() {
		_ = tarWriter.Close()
	}()

	// Add bundle metadata
	metadataJSON, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal bundle: %w", err)
	}

	if err := ct.addFileToTar(tarWriter, "bundle.json", metadataJSON); err != nil {
		return fmt.Errorf("failed to add bundle.json: %w", err)
	}

	// Generate and add Portage configuration files
	if err := ct.addPortageConfigToTar(tarWriter, bundle.Config); err != nil {
		return fmt.Errorf("failed to add portage config: %w", err)
	}

	// Add package build specs
	packagesJSON, err := json.MarshalIndent(bundle.Packages, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal packages: %w", err)
	}

	if err := ct.addFileToTar(tarWriter, "packages.json", packagesJSON); err != nil {
		return fmt.Errorf("failed to add packages.json: %w", err)
	}

	return nil
}

// addFileToTar adds a file to the tar archive.
func (ct *ConfigTransfer) addFileToTar(tw *tar.Writer, name string, data []byte) error {
	header := &tar.Header{
		Name: name,
		Mode: 0644,
		Size: int64(len(data)),
	}

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("failed to write header: %w", err)
	}

	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("failed to write data: %w", err)
	}

	return nil
}

// addPortageConfigToTar adds Portage configuration files to the tar archive.
func (ct *ConfigTransfer) addPortageConfigToTar(tw *tar.Writer, config *PortageConfig) error {
	if err := ct.addPackageUseToTar(tw, config.PackageUse); err != nil {
		return err
	}

	if err := ct.addPackageKeywordsToTar(tw, config.PackageKeywords); err != nil {
		return err
	}

	if err := ct.addPackageMaskToTar(tw, config.PackageMask); err != nil {
		return err
	}

	if err := ct.addPackageUnmaskToTar(tw, config.PackageUnmask); err != nil {
		return err
	}

	if err := ct.addMakeConfToTar(tw, config.MakeConf); err != nil {
		return err
	}

	if err := ct.addReposConfToTar(tw, config.Repos); err != nil {
		return err
	}

	return nil
}

// addPackageUseToTar adds package.use to tarball.
func (ct *ConfigTransfer) addPackageUseToTar(tw *tar.Writer, packageUse map[string][]string) error {
	if len(packageUse) == 0 {
		return nil
	}

	lines := make([]string, 0, len(packageUse))
	for pkg, flags := range packageUse {
		lines = append(lines, fmt.Sprintf("%s %s", pkg, strings.Join(flags, " ")))
	}
	content := strings.Join(lines, "\n") + "\n"
	return ct.addFileToTar(tw, "etc/portage/package.use/00-user", []byte(content))
}

// addPackageKeywordsToTar adds package.accept_keywords to tarball.
func (ct *ConfigTransfer) addPackageKeywordsToTar(tw *tar.Writer, packageKeywords map[string][]string) error {
	if len(packageKeywords) == 0 {
		return nil
	}

	lines := make([]string, 0, len(packageKeywords))
	for pkg, keywords := range packageKeywords {
		lines = append(lines, fmt.Sprintf("%s %s", pkg, strings.Join(keywords, " ")))
	}
	content := strings.Join(lines, "\n") + "\n"
	return ct.addFileToTar(tw, "etc/portage/package.accept_keywords/00-user", []byte(content))
}

// addPackageMaskToTar adds package.mask to tarball.
func (ct *ConfigTransfer) addPackageMaskToTar(tw *tar.Writer, packageMask []string) error {
	if len(packageMask) == 0 {
		return nil
	}

	content := strings.Join(packageMask, "\n") + "\n"
	return ct.addFileToTar(tw, "etc/portage/package.mask/00-user", []byte(content))
}

// addPackageUnmaskToTar adds package.unmask to tarball.
func (ct *ConfigTransfer) addPackageUnmaskToTar(tw *tar.Writer, packageUnmask []string) error {
	if len(packageUnmask) == 0 {
		return nil
	}

	content := strings.Join(packageUnmask, "\n") + "\n"
	return ct.addFileToTar(tw, "etc/portage/package.unmask/00-user", []byte(content))
}

// makeConfFragmentPath is where the user's make.conf overrides are stored inside
// the config bundle. Portage does NOT source make.conf.d/, so the executor
// appends this fragment to the container's real /etc/portage/make.conf instead
// of relying on a make.conf.d drop-in.
const makeConfFragmentPath = "etc/portage/make.conf.portage-engine"

// renderMakeConf renders a make.conf fragment from the given settings.
func renderMakeConf(makeConf map[string]string) []byte {
	lines := make([]string, 0, len(makeConf)+1)
	lines = append(lines, "# Appended by portage-engine (user make.conf overrides)")
	// Emit keys in sorted order for deterministic output.
	keys := make([]string, 0, len(makeConf))
	for k := range makeConf {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("%s=\"%s\"", key, makeConf[key]))
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// addMakeConfToTar adds the make.conf override fragment to the tarball.
func (ct *ConfigTransfer) addMakeConfToTar(tw *tar.Writer, makeConf map[string]string) error {
	if len(makeConf) == 0 {
		return nil
	}
	return ct.addFileToTar(tw, makeConfFragmentPath, renderMakeConf(makeConf))
}

// addReposConfToTar adds repos.conf to tarball.
func (ct *ConfigTransfer) addReposConfToTar(tw *tar.Writer, repos []RepoConfig) error {
	if len(repos) == 0 {
		return nil
	}

	for _, repo := range repos {
		var lines []string
		lines = append(lines, fmt.Sprintf("[%s]", repo.Name))
		if repo.Location != "" {
			lines = append(lines, fmt.Sprintf("location = %s", repo.Location))
		}
		if repo.SyncType != "" {
			lines = append(lines, fmt.Sprintf("sync-type = %s", repo.SyncType))
		}
		if repo.SyncURI != "" {
			lines = append(lines, fmt.Sprintf("sync-uri = %s", repo.SyncURI))
		}
		if repo.Priority != 0 {
			lines = append(lines, fmt.Sprintf("priority = %d", repo.Priority))
		}

		content := strings.Join(lines, "\n") + "\n"
		filename := fmt.Sprintf("etc/portage/repos.conf/%s.conf", repo.Name)
		if err := ct.addFileToTar(tw, filename, []byte(content)); err != nil {
			return err
		}
	}

	return nil
}

// ImportBundle imports a configuration bundle from a tarball.
func (ct *ConfigTransfer) ImportBundle(bundlePath string) (*ConfigBundle, error) {
	// Open the tarball
	file, err := os.Open(bundlePath) // #nosec G304 -- bundlePath is the caller-selected bundle source.
	if err != nil {
		return nil, fmt.Errorf("failed to open bundle: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	// Create gzip reader
	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() {
		_ = gzReader.Close()
	}()

	// Create tar reader
	tarReader := tar.NewReader(gzReader)

	// Read bundle.json
	var bundle *ConfigBundle
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar header: %w", err)
		}

		if header.Name == "bundle.json" {
			data, err := io.ReadAll(tarReader)
			if err != nil {
				return nil, fmt.Errorf("failed to read bundle.json: %w", err)
			}

			bundle = &ConfigBundle{}
			if err := json.Unmarshal(data, bundle); err != nil {
				return nil, fmt.Errorf("failed to unmarshal bundle.json: %w", err)
			}
			break
		}
	}

	if bundle == nil {
		return nil, fmt.Errorf("bundle.json not found in tarball")
	}
	if err := validateBundle(bundle); err != nil {
		return nil, fmt.Errorf("invalid config bundle: %w", err)
	}

	return bundle, nil
}

// ApplyConfigToSystem applies the configuration bundle to a target system.
func (ct *ConfigTransfer) ApplyConfigToSystem(bundle *ConfigBundle, targetRoot string) error {
	if err := validateBundle(bundle); err != nil {
		return fmt.Errorf("invalid config bundle: %w", err)
	}
	if targetRoot == "" {
		targetRoot = "/"
	}

	portageDir := filepath.Join(targetRoot, "etc", "portage")
	if err := ct.createPortageDirs(portageDir); err != nil {
		return err
	}

	config := bundle.Config

	if err := ct.writePackageUse(portageDir, config.PackageUse); err != nil {
		return err
	}

	if err := ct.writePackageKeywords(portageDir, config.PackageKeywords); err != nil {
		return err
	}

	if err := ct.writePackageMask(portageDir, config.PackageMask); err != nil {
		return err
	}

	if err := ct.writePackageUnmask(portageDir, config.PackageUnmask); err != nil {
		return err
	}

	if err := ct.writeMakeConf(portageDir, config.MakeConf); err != nil {
		return err
	}

	if err := ct.writeReposConf(portageDir, config.Repos); err != nil {
		return err
	}

	return nil
}

// createPortageDirs creates necessary Portage directories.
func (ct *ConfigTransfer) createPortageDirs(portageDir string) error {
	dirs := []string{
		filepath.Join(portageDir, "package.use"),
		filepath.Join(portageDir, "package.accept_keywords"),
		filepath.Join(portageDir, "package.mask"),
		filepath.Join(portageDir, "package.unmask"),
		filepath.Join(portageDir, "make.conf.d"),
		filepath.Join(portageDir, "repos.conf"),
	}

	for _, dir := range dirs {
		// Native Portage drops helpers to the portage user, which must be able
		// to traverse and read the job-scoped configuration tree.
		if err := os.MkdirAll(dir, 0755); err != nil { // #nosec G301 -- Portage helpers run as the portage user and require traversal.
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}
	return nil
}

// writePackageUse writes package.use configuration.
func (ct *ConfigTransfer) writePackageUse(portageDir string, packageUse map[string][]string) error {
	if len(packageUse) == 0 {
		return nil
	}

	lines := make([]string, 0, len(packageUse))
	for pkg, flags := range packageUse {
		lines = append(lines, fmt.Sprintf("%s %s", pkg, strings.Join(flags, " ")))
	}
	content := strings.Join(lines, "\n") + "\n"
	path := filepath.Join(portageDir, "package.use", "00-user")

	if err := os.WriteFile(path, []byte(content), 0644); err != nil { // #nosec G306 -- Portage package.use must be readable by the portage user.
		return fmt.Errorf("failed to write package.use: %w", err)
	}
	return nil
}

// writePackageKeywords writes package.accept_keywords configuration.
func (ct *ConfigTransfer) writePackageKeywords(portageDir string, packageKeywords map[string][]string) error {
	if len(packageKeywords) == 0 {
		return nil
	}

	lines := make([]string, 0, len(packageKeywords))
	for pkg, keywords := range packageKeywords {
		lines = append(lines, fmt.Sprintf("%s %s", pkg, strings.Join(keywords, " ")))
	}
	content := strings.Join(lines, "\n") + "\n"
	path := filepath.Join(portageDir, "package.accept_keywords", "00-user")

	if err := os.WriteFile(path, []byte(content), 0644); err != nil { // #nosec G306 -- Portage package.accept_keywords must be readable by the portage user.
		return fmt.Errorf("failed to write package.accept_keywords: %w", err)
	}
	return nil
}

// writePackageMask writes package.mask configuration.
func (ct *ConfigTransfer) writePackageMask(portageDir string, packageMask []string) error {
	if len(packageMask) == 0 {
		return nil
	}

	content := strings.Join(packageMask, "\n") + "\n"
	path := filepath.Join(portageDir, "package.mask", "00-user")

	if err := os.WriteFile(path, []byte(content), 0644); err != nil { // #nosec G306 -- Portage package.mask must be readable by the portage user.
		return fmt.Errorf("failed to write package.mask: %w", err)
	}
	return nil
}

// writePackageUnmask writes package.unmask configuration.
func (ct *ConfigTransfer) writePackageUnmask(portageDir string, packageUnmask []string) error {
	if len(packageUnmask) == 0 {
		return nil
	}

	content := strings.Join(packageUnmask, "\n") + "\n"
	path := filepath.Join(portageDir, "package.unmask", "00-user")

	if err := os.WriteFile(path, []byte(content), 0644); err != nil { // #nosec G306 -- Portage package.unmask must be readable by the portage user.
		return fmt.Errorf("failed to write package.unmask: %w", err)
	}
	return nil
}

// writeMakeConf appends the make.conf overrides to the real make.conf. Portage
// only reads the make.conf file itself (not make.conf.d/), so we append to it
// rather than dropping a file into make.conf.d, and we append rather than
// overwrite so the stage3's CHOST/CFLAGS/etc. are preserved.
func (ct *ConfigTransfer) writeMakeConf(portageDir string, makeConf map[string]string) error {
	if len(makeConf) == 0 {
		return nil
	}

	path := filepath.Join(portageDir, "make.conf")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644) // #nosec G302,G304 -- make.conf is intentionally readable and path is beneath the selected Portage root.
	if err != nil {
		return fmt.Errorf("failed to open make.conf: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Write(append([]byte("\n"), renderMakeConf(makeConf)...)); err != nil {
		return fmt.Errorf("failed to append make.conf: %w", err)
	}
	return nil
}

// writeReposConf writes repos.conf configuration.
func (ct *ConfigTransfer) writeReposConf(portageDir string, repos []RepoConfig) error {
	if len(repos) == 0 {
		return nil
	}

	for _, repo := range repos {
		var lines []string
		lines = append(lines, fmt.Sprintf("[%s]", repo.Name))
		if repo.Location != "" {
			lines = append(lines, fmt.Sprintf("location = %s", repo.Location))
		}
		if repo.SyncType != "" {
			lines = append(lines, fmt.Sprintf("sync-type = %s", repo.SyncType))
		}
		if repo.SyncURI != "" {
			lines = append(lines, fmt.Sprintf("sync-uri = %s", repo.SyncURI))
		}
		if repo.Priority != 0 {
			lines = append(lines, fmt.Sprintf("priority = %d", repo.Priority))
		}

		content := strings.Join(lines, "\n") + "\n"
		path := filepath.Join(portageDir, "repos.conf", fmt.Sprintf("%s.conf", repo.Name))

		if err := os.WriteFile(path, []byte(content), 0644); err != nil { // #nosec G306 -- Portage repos.conf must be readable by the portage user.
			return fmt.Errorf("failed to write repos.conf for %s: %w", repo.Name, err)
		}
	}

	return nil
}
