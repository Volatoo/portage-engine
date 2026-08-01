// Package builder provides validation for untrusted build specifications.
package builder

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// MaxBuildRequestBodyBytes is the largest JSON build request accepted by the
// server or a builder. Structural limits below are still enforced after decode.
const MaxBuildRequestBodyBytes int64 = 2 << 20 // 2 MiB

const (
	maxBundlePackages      = 128
	maxPackageRules        = 4096
	maxValuesPerRule       = 256
	maxPackageListEntries  = 4096
	maxRepositories        = 32
	maxMakeConfEntries     = 128
	maxEnvironmentEntries  = 64
	maxFieldLength         = 4096
	maxMetadataDescription = 1024
	maxMetadataUserID      = 128
	maxProfileLength       = 256
	maxMakeJobs            = 64
)

// The build endpoint accepts a ConfigBundle from clients. Every field which
// reaches Portage, a filesystem path, or a network fetch must pass a semantic
// allowlist before a job is queued.
var (
	// A build target atom: category/package, optionally with a :slot suffix.
	// Version operators are deliberately represented by the separate Version
	// field, which also prevents an atom from being interpreted as an option.
	atomPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+._-]*/[a-zA-Z0-9][a-zA-Z0-9+._-]*(:[a-zA-Z0-9][a-zA-Z0-9+._/-]*)?$`)

	// Package configuration files accept a wider atom syntax than build targets:
	// optional version operators, wildcard atoms, slots, and repo qualifiers.
	portageRuleAtomPattern = regexp.MustCompile(`^(?:<=|>=|=|~|<|>)?[a-zA-Z0-9*][a-zA-Z0-9+._*-]*/[a-zA-Z0-9*][a-zA-Z0-9+._*-]*(?::[a-zA-Z0-9*+._/=-]+)?(?:::[a-zA-Z0-9][a-zA-Z0-9+._-]*)?$`)

	versionPattern = regexp.MustCompile(`^[0-9][a-zA-Z0-9._-]*$`)
	useFlagPattern = regexp.MustCompile(`^[+-]?[a-zA-Z0-9][a-zA-Z0-9+_@-]*$`)
	keywordPattern = regexp.MustCompile(`^[~*]?[a-zA-Z0-9*][a-zA-Z0-9_-]*$`)

	repoNamePattern     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
	profilePattern      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+._/-]*$`)
	registryIDPattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+._/-]{0,255}$`)
	revisionPattern     = regexp.MustCompile(`^[a-fA-F0-9]{7,64}$`)
	digestPattern       = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	machineValuePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	userIDPattern       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9@._:-]*$`)
	idempotencyPattern  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)

	// Values are written inside a quoted make.conf assignment or passed directly
	// through execve. Quotes, backslashes, command substitutions, shell control
	// characters, and newlines are intentionally excluded.
	portageValuePattern = regexp.MustCompile(`^[a-zA-Z0-9 \t,.:=@%+/_*~!?-]*$`)
	makeJobsPattern     = regexp.MustCompile(`^(?:-j|--jobs=|-l|--load-average=)([0-9]{1,3})$`)
)

// User-controlled Portage variables are conservative by design. Infrastructure
// variables such as ROOT, PORTAGE_CONFIGROOT, PKGDIR, FEATURES, command hooks,
// and signing settings belong to the operator policy and are never accepted in
// a ConfigBundle.
var allowedPortageVariables = map[string]struct{}{
	"ACCEPT_KEYWORDS":      {},
	"ACCEPT_LICENSE":       {},
	"ACCEPT_PROPERTIES":    {},
	"ACCEPT_RESTRICT":      {},
	"APACHE2_MODULES":      {},
	"ARCH":                 {},
	"CFLAGS":               {},
	"CPU_FLAGS_X86":        {},
	"CXXFLAGS":             {},
	"FCFLAGS":              {},
	"FFLAGS":               {},
	"GRUB_PLATFORMS":       {},
	"INPUT_DEVICES":        {},
	"L10N":                 {},
	"LANG":                 {},
	"LC_ALL":               {},
	"LC_MESSAGES":          {},
	"LDFLAGS":              {},
	"LINGUAS":              {},
	"LLVM_TARGETS":         {},
	"LUA_SINGLE_TARGET":    {},
	"LUA_TARGETS":          {},
	"MAKEOPTS":             {},
	"NGINX_MODULES_HTTP":   {},
	"NGINX_MODULES_MAIL":   {},
	"NGINX_MODULES_STREAM": {},
	"PHP_TARGETS":          {},
	"PYTHON_SINGLE_TARGET": {},
	"PYTHON_TARGETS":       {},
	"QEMU_SOFTMMU_TARGETS": {},
	"QEMU_USER_TARGETS":    {},
	"RUBY_TARGETS":         {},
	"USE":                  {},
	"VIDEO_CARDS":          {},
}

func validatePackageSpec(pkg PackageSpec) error {
	if !atomPattern.MatchString(pkg.Atom) {
		return fmt.Errorf("invalid package atom %q", pkg.Atom)
	}
	if pkg.Version != "" && !versionPattern.MatchString(pkg.Version) {
		return fmt.Errorf("invalid package version %q", pkg.Version)
	}
	if err := validateUseFlags(pkg.UseFlags, "package USE flags"); err != nil {
		return err
	}
	if len(pkg.Keywords) > maxValuesPerRule {
		return fmt.Errorf("too many package keywords: %d (max %d)", len(pkg.Keywords), maxValuesPerRule)
	}
	for _, keyword := range pkg.Keywords {
		if !keywordPattern.MatchString(keyword) {
			return fmt.Errorf("invalid keyword %q", keyword)
		}
	}
	if err := validatePortageVariables(pkg.Environment, maxEnvironmentEntries, "package environment"); err != nil {
		return err
	}
	return nil
}

func validateUseFlags(flags []string, field string) error {
	if len(flags) > maxValuesPerRule {
		return fmt.Errorf("too many %s: %d (max %d)", field, len(flags), maxValuesPerRule)
	}
	for _, flag := range flags {
		if !useFlagPattern.MatchString(flag) {
			return fmt.Errorf("invalid USE flag %q in %s", flag, field)
		}
	}
	return nil
}

func validatePortageVariables(values map[string]string, limit int, field string) error {
	if len(values) > limit {
		return fmt.Errorf("too many %s entries: %d (max %d)", field, len(values), limit)
	}
	for key, value := range values {
		if _, ok := allowedPortageVariables[key]; !ok {
			return fmt.Errorf("portage variable %q is not allowed in %s", key, field)
		}
		if len(value) > maxFieldLength || !portageValuePattern.MatchString(value) {
			return fmt.Errorf("invalid value for Portage variable %q in %s", key, field)
		}
		if key == "MAKEOPTS" {
			if err := validateMakeOpts(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateMakeOpts(value string) error {
	for _, option := range strings.Fields(value) {
		match := makeJobsPattern.FindStringSubmatch(option)
		if match == nil {
			return fmt.Errorf("MAKEOPTS option %q is not allowed", option)
		}
		n, err := strconv.Atoi(match[1])
		if err != nil || n < 1 || n > maxMakeJobs {
			return fmt.Errorf("MAKEOPTS option %q exceeds the allowed range 1..%d", option, maxMakeJobs)
		}
	}
	return nil
}

func validatePackageRuleMap(rules map[string][]string, keywords bool, field string) error {
	if len(rules) > maxPackageRules {
		return fmt.Errorf("too many %s entries: %d (max %d)", field, len(rules), maxPackageRules)
	}
	for atom, values := range rules {
		if !portageRuleAtomPattern.MatchString(atom) {
			return fmt.Errorf("invalid package atom %q in %s", atom, field)
		}
		if len(values) > maxValuesPerRule {
			return fmt.Errorf("too many values for %q in %s", atom, field)
		}
		for _, value := range values {
			if keywords {
				if !keywordPattern.MatchString(value) {
					return fmt.Errorf("invalid keyword %q for %q", value, atom)
				}
			} else if !useFlagPattern.MatchString(value) {
				return fmt.Errorf("invalid USE flag %q for %q", value, atom)
			}
		}
	}
	return nil
}

func validatePackageAtomList(atoms []string, field string) error {
	if len(atoms) > maxPackageListEntries {
		return fmt.Errorf("too many %s entries: %d (max %d)", field, len(atoms), maxPackageListEntries)
	}
	for _, atom := range atoms {
		if !portageRuleAtomPattern.MatchString(atom) {
			return fmt.Errorf("invalid package atom %q in %s", atom, field)
		}
	}
	return nil
}

func validateRepositories(repos []RepoConfig) error {
	if len(repos) > maxRepositories {
		return fmt.Errorf("too many repositories: %d (max %d)", len(repos), maxRepositories)
	}
	seen := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		if repo.RegistryID != "" && !registryIDPattern.MatchString(repo.RegistryID) {
			return fmt.Errorf("invalid repository registry ID %q", repo.RegistryID)
		}
		if !repoNamePattern.MatchString(repo.Name) {
			return fmt.Errorf("invalid repository name %q", repo.Name)
		}
		if _, exists := seen[repo.Name]; exists {
			return fmt.Errorf("duplicate repository %q", repo.Name)
		}
		seen[repo.Name] = struct{}{}

		if repo.Location != "" {
			expected := filepath.Join("/var/db/repos", repo.Name)
			if filepath.Clean(repo.Location) != expected {
				return fmt.Errorf("repository %q location must be %q", repo.Name, expected)
			}
		}
		if repo.Priority < -9999 || repo.Priority > 9999 {
			return fmt.Errorf("repository %q priority is out of range", repo.Name)
		}
		if repo.Revision != "" && !revisionPattern.MatchString(repo.Revision) {
			return fmt.Errorf("repository %q has an invalid revision", repo.Name)
		}
		if repo.Digest != "" && !digestPattern.MatchString(repo.Digest) {
			return fmt.Errorf("repository %q has an invalid digest", repo.Name)
		}

		switch repo.SyncType {
		case "", "git", "rsync", "webrsync":
		default:
			return fmt.Errorf("repository %q has unsupported sync type %q", repo.Name, repo.SyncType)
		}
		if repo.SyncURI == "" {
			if repo.SyncType != "" {
				return fmt.Errorf("repository %q has a sync type but no sync URI", repo.Name)
			}
			continue
		}
		if repo.SyncType == "" {
			return fmt.Errorf("repository %q has a sync URI but no sync type", repo.Name)
		}
		if len(repo.SyncURI) > maxFieldLength {
			return fmt.Errorf("repository %q sync URI is too long", repo.Name)
		}
		parsed, err := url.ParseRequestURI(repo.SyncURI)
		if err != nil || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
			return fmt.Errorf("repository %q has an invalid sync URI", repo.Name)
		}
		switch {
		case repo.SyncType == "rsync":
			if parsed.Scheme != "rsync" || parsed.Host == "" {
				return fmt.Errorf("repository %q rsync URI must use rsync://", repo.Name)
			}
		case parsed.Scheme == "https":
			if parsed.Host == "" {
				return fmt.Errorf("repository %q HTTPS URI requires a host", repo.Name)
			}
		case parsed.Scheme == "http":
			// Trusted-LAN HTTP is an integrity-protected artifact transport,
			// never a credential transport. Match the server catalog contract:
			// only Git snapshots with a full immutable revision and digest are
			// accepted; URL credentials/query/fragment were rejected above.
			if parsed.Host == "" || repo.SyncType != "git" ||
				(len(repo.Revision) != 40 && len(repo.Revision) != 64) || repo.Digest == "" {
				return fmt.Errorf("repository %q HTTP git mirror requires host, full revision, and snapshot digest", repo.Name)
			}
		default:
			return fmt.Errorf("repository %q has an invalid sync URI: expected https://, trusted-LAN http:// for pinned Git snapshots, or rsync:// for rsync repositories", repo.Name)
		}
	}
	return nil
}

func validateMetadata(metadata BundleMetadata) error {
	if metadata.UserID != "" {
		if len(metadata.UserID) > maxMetadataUserID || !userIDPattern.MatchString(metadata.UserID) {
			return fmt.Errorf("invalid bundle user ID")
		}
	}
	if metadata.TargetArch != "" && !keywordPattern.MatchString(metadata.TargetArch) {
		return fmt.Errorf("invalid bundle target architecture %q", metadata.TargetArch)
	}
	if metadata.ProfileID != "" && !registryIDPattern.MatchString(metadata.ProfileID) {
		return fmt.Errorf("invalid bundle profile ID %q", metadata.ProfileID)
	}
	if metadata.Profile != "" {
		if len(metadata.Profile) > maxProfileLength || !profilePattern.MatchString(metadata.Profile) || strings.HasPrefix(metadata.Profile, "/") {
			return fmt.Errorf("invalid bundle profile %q", metadata.Profile)
		}
		for _, part := range strings.Split(metadata.Profile, "/") {
			if part == "." || part == ".." || part == "" {
				return fmt.Errorf("invalid bundle profile %q", metadata.Profile)
			}
		}
	}
	if metadata.ProfileRepositoryID != "" && !registryIDPattern.MatchString(metadata.ProfileRepositoryID) {
		return fmt.Errorf("invalid profile repository ID")
	}
	if metadata.ProfileRepositoryName != "" && !repoNamePattern.MatchString(metadata.ProfileRepositoryName) {
		return fmt.Errorf("invalid profile repository name")
	}
	if (metadata.ProfileRepositoryID == "") != (metadata.ProfileRepositoryName == "") {
		return fmt.Errorf("profile repository ID and name must be provided together")
	}
	if len(metadata.ProfileParents) > maxRepositories {
		return fmt.Errorf("too many profile parents")
	}
	seenParents := make(map[string]struct{}, len(metadata.ProfileParents))
	for _, parent := range metadata.ProfileParents {
		if !registryIDPattern.MatchString(parent.RepositoryID) || !repoNamePattern.MatchString(parent.RepositoryName) || !profilePattern.MatchString(parent.ProfilePath) || strings.HasPrefix(parent.ProfilePath, "/") {
			return fmt.Errorf("invalid profile parent")
		}
		for _, part := range strings.Split(parent.ProfilePath, "/") {
			if part == "" || part == "." || part == ".." {
				return fmt.Errorf("invalid profile parent path")
			}
		}
		key := parent.RepositoryID + ":" + parent.ProfilePath
		if _, duplicate := seenParents[key]; duplicate {
			return fmt.Errorf("duplicate profile parent %q", key)
		}
		seenParents[key] = struct{}{}
	}
	if metadata.CreatedAt != "" {
		if _, err := time.Parse(time.RFC3339, metadata.CreatedAt); err != nil {
			return fmt.Errorf("invalid bundle creation time")
		}
	}
	if len(metadata.Description) > maxMetadataDescription || !validDisplayText(metadata.Description) {
		return fmt.Errorf("invalid bundle description")
	}
	return nil
}

func validDisplayText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) && r != '\t' {
			return false
		}
	}
	return true
}

// validateBundle validates every field of a bundle before it reaches a path,
// network fetch, Portage configuration file, or build command.
func validateBundle(bundle *ConfigBundle) error {
	if bundle == nil {
		return fmt.Errorf("nil bundle")
	}
	if bundle.Config == nil {
		return fmt.Errorf("config bundle has no Portage config")
	}
	if bundle.Packages == nil || len(bundle.Packages.Packages) == 0 {
		return fmt.Errorf("config bundle contains no packages")
	}
	if len(bundle.Packages.Packages) > maxBundlePackages {
		return fmt.Errorf("too many packages: %d (max %d)", len(bundle.Packages.Packages), maxBundlePackages)
	}

	if err := validatePackageRuleMap(bundle.Config.PackageUse, false, "package.use"); err != nil {
		return err
	}
	if err := validatePackageRuleMap(bundle.Config.PackageKeywords, true, "package.accept_keywords"); err != nil {
		return err
	}
	if err := validatePackageAtomList(bundle.Config.PackageMask, "package.mask"); err != nil {
		return err
	}
	if err := validatePackageAtomList(bundle.Config.PackageUnmask, "package.unmask"); err != nil {
		return err
	}
	if err := validatePortageVariables(bundle.Config.MakeConf, maxMakeConfEntries, "make.conf"); err != nil {
		return err
	}
	if err := validatePortageVariables(bundle.Config.Environment, maxEnvironmentEntries, "global environment"); err != nil {
		return err
	}
	if err := validateUseFlags(bundle.Config.GlobalUse, "global USE flags"); err != nil {
		return err
	}
	if err := validateRepositories(bundle.Config.Repos); err != nil {
		return err
	}
	if err := validateMetadata(bundle.Metadata); err != nil {
		return err
	}
	if bundle.Metadata.ProfileRepositoryID != "" {
		registered := make(map[string]RepoConfig, len(bundle.Config.Repos))
		for _, repository := range bundle.Config.Repos {
			registered[repository.RegistryID] = repository
		}
		profileRepository, ok := registered[bundle.Metadata.ProfileRepositoryID]
		if !ok || profileRepository.Name != bundle.Metadata.ProfileRepositoryName {
			return fmt.Errorf("profile repository is not present in resolved repositories")
		}
		for _, parent := range bundle.Metadata.ProfileParents {
			repository, ok := registered[parent.RepositoryID]
			if !ok || repository.Name != parent.RepositoryName {
				return fmt.Errorf("profile parent repository %q is not present in resolved repositories", parent.RepositoryID)
			}
		}
	}

	seenPackages := make(map[string]struct{}, len(bundle.Packages.Packages))
	for _, pkg := range bundle.Packages.Packages {
		if err := validatePackageSpec(pkg); err != nil {
			return err
		}
		key := pkg.Atom + "\x00" + pkg.Version
		if _, exists := seenPackages[key]; exists {
			return fmt.Errorf("duplicate package build specification for %q", pkg.Atom)
		}
		seenPackages[key] = struct{}{}
	}
	return nil
}

// validateBuildRequest is the server-side choke point before a request can be
// queued, provision infrastructure, or be forwarded to a remote builder.
func validateBuildRequest(req *BuildRequest) error {
	if req == nil {
		return fmt.Errorf("nil build request")
	}
	if !atomPattern.MatchString(req.PackageName) {
		return fmt.Errorf("invalid package name %q", req.PackageName)
	}
	if req.Version != "" && !versionPattern.MatchString(req.Version) {
		return fmt.Errorf("invalid package version %q", req.Version)
	}
	if req.Arch != "" && !keywordPattern.MatchString(req.Arch) {
		return fmt.Errorf("invalid architecture %q", req.Arch)
	}
	if req.IdempotencyKey != "" && !idempotencyPattern.MatchString(req.IdempotencyKey) {
		return fmt.Errorf("invalid idempotency key")
	}
	if err := validateUseFlags(req.UseFlags, "request USE flags"); err != nil {
		return err
	}
	if req.ProfileID != "" && !registryIDPattern.MatchString(req.ProfileID) {
		return fmt.Errorf("invalid profile ID %q", req.ProfileID)
	}
	if len(req.RepositoryIDs) > maxRepositories {
		return fmt.Errorf("too many repository IDs")
	}
	seenRepositories := make(map[string]struct{}, len(req.RepositoryIDs))
	for _, id := range req.RepositoryIDs {
		if !registryIDPattern.MatchString(id) {
			return fmt.Errorf("invalid repository ID %q", id)
		}
		if _, exists := seenRepositories[id]; exists {
			return fmt.Errorf("duplicate repository ID %q", id)
		}
		seenRepositories[id] = struct{}{}
	}
	if req.ResourceClass != "" && !registryIDPattern.MatchString(req.ResourceClass) {
		return fmt.Errorf("invalid resource class %q", req.ResourceClass)
	}
	if req.ProjectID != "" {
		if _, err := uuid.Parse(req.ProjectID); err != nil {
			return fmt.Errorf("invalid project ID")
		}
	}
	if err := validateRequestMachineSpec(req.MachineSpec); err != nil {
		return err
	}
	if req.ConfigBundle != nil {
		return validateBundle(req.ConfigBundle)
	}
	return nil
}

func validateRequestMachineSpec(spec map[string]string) error {
	if len(spec) > 8 {
		return fmt.Errorf("too many machine spec entries")
	}
	for key, value := range spec {
		switch key {
		case "cores":
			if err := validateBoundedInt(value, 1, 128, key); err != nil {
				return err
			}
		case "memory":
			if err := validateBoundedInt(value, 512, 1<<20, key); err != nil {
				return err
			}
		case "disk_size":
			if err := validateBoundedInt(value, 10, 4096, key); err != nil {
				return err
			}
		case "machine_type", "instance_type":
			if value == "" || len(value) > 128 || !machineValuePattern.MatchString(value) {
				return fmt.Errorf("invalid machine spec %s", key)
			}
		case "preemptible":
			if value != "true" && value != "false" {
				return fmt.Errorf("invalid machine spec preemptible")
			}
		default:
			return fmt.Errorf("machine spec key %q is operator-owned or unsupported", key)
		}
	}
	return nil
}

func validateBoundedInt(value string, minValue, maxValue int, key string) error {
	n, err := strconv.Atoi(value)
	if err != nil || n < minValue || n > maxValue {
		return fmt.Errorf("machine spec %s must be between %d and %d", key, minValue, maxValue)
	}
	return nil
}

// validateLocalBuildRequest validates every untrusted field before any local
// native executor can queue it.
func validateLocalBuildRequest(req *LocalBuildRequest) error {
	if req == nil {
		return fmt.Errorf("nil build request")
	}
	if req.ExecutionID != "" {
		if _, err := uuid.Parse(req.ExecutionID); err != nil {
			return fmt.Errorf("invalid execution ID")
		}
	}
	if !atomPattern.MatchString(req.PackageName) {
		return fmt.Errorf("invalid package name %q", req.PackageName)
	}
	if req.Version != "" && !versionPattern.MatchString(req.Version) {
		return fmt.Errorf("invalid package version %q", req.Version)
	}
	if req.Arch != "" && !keywordPattern.MatchString(req.Arch) {
		return fmt.Errorf("invalid arch %q", req.Arch)
	}
	if req.ProfileID != "" && !registryIDPattern.MatchString(req.ProfileID) {
		return fmt.Errorf("invalid profile ID %q", req.ProfileID)
	}
	if req.ResourceClass != "" && !registryIDPattern.MatchString(req.ResourceClass) {
		return fmt.Errorf("invalid resource class %q", req.ResourceClass)
	}
	if req.ProjectID != "" {
		if _, err := uuid.Parse(req.ProjectID); err != nil {
			return fmt.Errorf("invalid project ID")
		}
	}
	if req.AttemptID != "" {
		if _, err := uuid.Parse(req.AttemptID); err != nil {
			return fmt.Errorf("invalid attempt ID")
		}
	}
	if req.AttemptFence < 0 || (req.AttemptID == "") != (req.AttemptFence == 0) {
		return fmt.Errorf("invalid build attempt fence")
	}
	if req.CompileLease != nil {
		if err := req.CompileLease.Validate(time.Now()); err != nil {
			return fmt.Errorf("invalid distcc lease: %w", err)
		}
		if req.ProjectID == "" ||
			req.CompileLease.Pool.ProjectTrustDomain != req.ProjectID ||
			req.CompileLease.AttemptID != req.AttemptID ||
			req.CompileLease.AttemptFence != req.AttemptFence {
			return fmt.Errorf("distcc lease does not match the claimed project/attempt")
		}
	}
	if len(req.RepositoryIDs) > maxRepositories {
		return fmt.Errorf("too many repository IDs")
	}
	for _, id := range req.RepositoryIDs {
		if !registryIDPattern.MatchString(id) {
			return fmt.Errorf("invalid repository ID %q", id)
		}
	}
	if len(req.UseFlags) > maxValuesPerRule {
		return fmt.Errorf("too many request USE flags")
	}
	for flag, state := range req.UseFlags {
		if !useFlagPattern.MatchString(flag) {
			return fmt.Errorf("invalid USE flag %q", flag)
		}
		switch state {
		case "", "0", "1", "false", "true", "disabled", "enabled":
		default:
			return fmt.Errorf("invalid state %q for USE flag %q", state, flag)
		}
	}
	if err := validatePortageVariables(req.Environment, maxEnvironmentEntries, "request environment"); err != nil {
		return err
	}
	if req.ConfigBundle != nil {
		if err := validateBundle(req.ConfigBundle); err != nil {
			return err
		}
	}
	if len(req.PackageSpecs) > maxBundlePackages {
		return fmt.Errorf("too many package specifications")
	}
	for _, spec := range req.PackageSpecs {
		if err := validatePackageSpec(spec); err != nil {
			return err
		}
	}
	return nil
}
