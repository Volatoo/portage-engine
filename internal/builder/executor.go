// Package builder provides build execution capabilities.
package builder

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/profilecontract"
)

// BuildOptions controls the unsigned binary-package format produced by a
// builder. Private-key operations belong exclusively to portage-signer.
type BuildOptions struct {
	// Format is the BINPKG_FORMAT Portage produces: "gpkg" (default) or "xpak".
	Format string
}

// BuildExecutor handles the actual package build process.
type BuildExecutor struct {
	workDir        string
	artifactDir    string
	configTransfer *ConfigTransfer
	opts           BuildOptions
}

// NewBuildExecutor creates a new build executor with default (gpkg, unsigned)
// options.
func NewBuildExecutor(workDir, artifactDir string) *BuildExecutor {
	return NewBuildExecutorWithOptions(workDir, artifactDir, BuildOptions{Format: "gpkg"})
}

// NewBuildExecutorWithOptions creates a build executor with explicit options.
func NewBuildExecutorWithOptions(workDir, artifactDir string, opts BuildOptions) *BuildExecutor {
	if opts.Format == "" {
		opts.Format = "gpkg"
	}
	return &BuildExecutor{
		workDir:        workDir,
		artifactDir:    artifactDir,
		configTransfer: NewConfigTransfer(workDir),
		opts:           opts,
	}
}

// ExecuteBuild executes a build with the given configuration bundle.
func (be *BuildExecutor) ExecuteBuild(
	ctx context.Context,
	bundle *ConfigBundle,
	job *BuildJob,
) error {
	// Reject any bundle whose fields contain shell metacharacters or option
	// injection before constructing any command.
	if err := validateBundle(bundle); err != nil {
		return fmt.Errorf("invalid build request: %w", err)
	}

	// Create build workspace
	buildID := job.ID
	buildWorkDir := filepath.Join(be.workDir, buildID)
	// Portage drops fetch/build helpers to the portage user. It must be able to
	// traverse the job-scoped PORTAGE_CONFIGROOT on native build nodes.
	if err := os.MkdirAll(buildWorkDir, 0755); err != nil {
		return fmt.Errorf("failed to create build workspace: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(buildWorkDir)
	}()

	if err := seedNativeMakeConf(buildWorkDir); err != nil {
		return fmt.Errorf("failed to seed native Portage config: %w", err)
	}
	// Apply configuration to build environment
	if err := be.configTransfer.ApplyConfigToSystem(bundle, buildWorkDir); err != nil {
		return fmt.Errorf("failed to apply configuration: %w", err)
	}
	if err := verifyRepositoryRevisions(ctx, bundle.Config.Repos); err != nil {
		return fmt.Errorf("repository provenance check failed: %w", err)
	}
	configRoot, err := prepareNativeProfile(bundle, buildWorkDir)
	if err != nil {
		return fmt.Errorf("profile selection failed: %w", err)
	}
	pkgDir := filepath.Join(buildWorkDir, "binpkgs")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return fmt.Errorf("failed to create job package directory: %w", err)
	}

	// Build each package
	for _, pkg := range bundle.Packages.Packages {
		if err := be.buildPackage(ctx, pkg, bundle, buildWorkDir, pkgDir, configRoot, job); err != nil {
			return fmt.Errorf("failed to build package %s: %w", pkg.Atom, err)
		}
	}

	// Publish the complete set created by this job, including dependencies.
	// Verification uses --usepkgonly, so publishing only the requested atom is
	// insufficient for a clean/offline install.
	return be.collectArtifacts(pkgDir, job)
}

// buildPackage builds a single package.
func (be *BuildExecutor) buildPackage(
	ctx context.Context,
	pkg PackageSpec,
	bundle *ConfigBundle,
	buildWorkDir string,
	pkgDir string,
	configRoot string,
	job *BuildJob,
) error {
	// Construct emerge command
	cmd := be.constructEmergeCommand(pkg, bundle, buildWorkDir)

	// Execute build
	var stdout, stderr bytes.Buffer
	execCmd := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	execCmd.Stdout = &stdout
	execCmd.Stderr = &stderr
	execCmd.Dir = buildWorkDir

	// Set environment variables
	execCmd.Env = append(os.Environ(), be.buildEnvironment(pkg, bundle, pkgDir, configRoot)...)

	job.appendLog(fmt.Sprintf("Building package: %s\n", pkg.Atom))
	job.appendLog(fmt.Sprintf("Command: %s\n", strings.Join(cmd, " ")))

	startTime := time.Now()
	err := execCmd.Run()
	duration := time.Since(startTime)

	job.appendLog(fmt.Sprintf("Build duration: %s\n", duration))
	job.appendLog(fmt.Sprintf("STDOUT:\n%s\n", stdout.String()))
	if stderr.Len() > 0 {
		job.appendLog(fmt.Sprintf("STDERR:\n%s\n", stderr.String()))
	}

	if err != nil {
		return fmt.Errorf("emerge failed: %w", err)
	}

	return nil
}

// constructEmergeCommand constructs the emerge command for a package.
func (be *BuildExecutor) constructEmergeCommand(
	pkg PackageSpec,
	_ *ConfigBundle,
	_ string,
) []string {
	cmd := []string{"emerge"}

	// Add global options
	cmd = append(cmd, "--buildpkg")      // Build binary package
	cmd = append(cmd, "--usepkg=n")      // Don't use existing binpkgs
	cmd = append(cmd, "--oneshot")       // Don't add to world file
	cmd = append(cmd, "--verbose")       // Verbose output
	cmd = append(cmd, "--quiet-build=n") // Show build output

	// Add options to automatically resolve dependency conflicts
	cmd = append(cmd, "--autounmask")          // Automatically unmask packages
	cmd = append(cmd, "--autounmask-write")    // Write unmask changes to config
	cmd = append(cmd, "--autounmask-continue") // Continue after writing changes
	cmd = append(cmd, "--backtrack=50")        // Increase backtrack for complex deps

	// Add package-specific USE flags if provided
	if len(pkg.UseFlags) > 0 {
		useFlags := strings.Join(pkg.UseFlags, " ")
		cmd = append(cmd, fmt.Sprintf("--use=%s", useFlags))
	}

	// Add keywords if provided
	if len(pkg.Keywords) > 0 {
		keywords := strings.Join(pkg.Keywords, " ")
		cmd = append(cmd, fmt.Sprintf("--accept-keywords=%s", keywords))
	}

	// Add the package atom
	if pkg.Version != "" {
		// A versioned Gentoo atom is an exact-match atom and therefore must use
		// the '=' operator. Without it, emerge rejects category/package-version
		// as an invalid atom before dependency resolution starts.
		cmd = append(cmd, fmt.Sprintf("=%s-%s", pkg.Atom, pkg.Version))
	} else {
		cmd = append(cmd, pkg.Atom)
	}

	return cmd
}

// buildEnvironment constructs the environment variables for the native build.
func (be *BuildExecutor) buildEnvironment(pkg PackageSpec, bundle *ConfigBundle, pkgDir, configRoot string) []string {
	env := []string{}

	// Collect any user-supplied FEATURES so we extend rather than clobber them.
	features := map[string]bool{"buildpkg": true}

	appendUserEnv := func(m map[string]string) {
		for key, value := range m {
			if key == "FEATURES" {
				for _, f := range strings.Fields(value) {
					features[f] = true
				}
				continue
			}
			env = append(env, fmt.Sprintf("%s=%s", key, value))
		}
	}
	if bundle.Config != nil {
		appendUserEnv(bundle.Config.Environment)
	}
	appendUserEnv(pkg.Environment)

	// Select the binary package format. The builder always emits unsigned
	// artifacts; signing occurs only after the quarantine verification gate.
	env = append(env, fmt.Sprintf("BINPKG_FORMAT=%s", be.opts.Format))

	// Emit FEATURES in sorted order for determinism.
	featureList := make([]string, 0, len(features))
	for f := range features {
		featureList = append(featureList, f)
	}
	sort.Strings(featureList)
	env = append(env, fmt.Sprintf("FEATURES=%s", strings.Join(featureList, " ")))

	env = append(env, fmt.Sprintf("PKGDIR=%s", pkgDir))
	if configRoot != "" {
		env = append(env, fmt.Sprintf("PORTAGE_CONFIGROOT=%s", configRoot))
	}

	return env
}

func seedNativeMakeConf(configRoot string) error {
	destinationDir := filepath.Join(configRoot, "etc", "portage")
	if err := os.MkdirAll(destinationDir, 0o755); err != nil {
		return err
	}
	source := filepath.Join("/etc", "portage", "make.conf")
	info, err := os.Stat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return fmt.Errorf("host make.conf is not a bounded regular file")
	}
	content, err := os.ReadFile(source) // #nosec G304 -- fixed operator-owned Portage path.
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destinationDir, "make.conf"), content, 0o644) // #nosec G703 -- destinationDir is the builder-owned isolated config root.
}

func prepareNativeProfile(bundle *ConfigBundle, configRoot string) (string, error) {
	makeProfile := filepath.Join(configRoot, "etc", "portage", "make.profile")
	if err := os.Remove(makeProfile); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	metadata := bundle.Metadata
	if metadata.ProfileRepositoryID == "" {
		// Direct local compatibility requests predate the catalog fields. They
		// still get an isolated config root, but inherit the image's already
		// selected profile rather than silently ignoring the bundle.
		profile, err := filepath.EvalSymlinks("/etc/portage/make.profile")
		if err != nil {
			return "", fmt.Errorf("resolve image make.profile: %w", err)
		}
		if err := os.Symlink(profile, makeProfile); err != nil {
			return "", err
		}
		return configRoot, nil
	}
	var profileRepository *RepoConfig
	for index := range bundle.Config.Repos {
		if bundle.Config.Repos[index].RegistryID == metadata.ProfileRepositoryID {
			profileRepository = &bundle.Config.Repos[index]
			break
		}
	}
	if profileRepository == nil || profileRepository.Name != metadata.ProfileRepositoryName {
		return "", fmt.Errorf("resolved profile repository is absent")
	}
	selection := profilecontract.Selection{RepositoryName: profileRepository.Name, RepositoryRoot: profileRepository.Location, ProfilePath: metadata.Profile,
		VerifyExactParents: len(metadata.ProfileParents) > 0}
	for _, parent := range metadata.ProfileParents {
		selection.Parents = append(selection.Parents, profilecontract.Parent{RepositoryName: parent.RepositoryName, ProfilePath: parent.ProfilePath})
	}
	profileDir, err := profilecontract.Verify(selection)
	if err != nil {
		return "", err
	}
	if err := os.Symlink(profileDir, makeProfile); err != nil {
		return "", err
	}
	return configRoot, nil
}

// collectArtifacts collects built package artifacts.
func (be *BuildExecutor) collectArtifacts(pkgDir string, job *BuildJob) error {
	// The package structure is typically:
	//   GPKG:  PKGDIR/category/package/package-version.gpkg.tar
	//   XPAK:  PKGDIR/category/package-version.tbz2
	var rels []string

	err := filepath.Walk(pkgDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Match modern GPKG (.gpkg.tar) and legacy XPAK (.tbz2) so artifacts are
		// found regardless of the configured format.
		if !info.IsDir() && (strings.HasSuffix(info.Name(), ".gpkg.tar") || strings.HasSuffix(info.Name(), ".tbz2")) {
			rel, relErr := filepath.Rel(pkgDir, path)
			if relErr != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("invalid package path %s", path)
			}
			rels = append(rels, rel)
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to search for packages: %w", err)
	}

	if len(rels) == 0 {
		return fmt.Errorf("no binary packages found")
	}
	sort.Strings(rels)

	// Copy artifacts to the shared serving directory with category preserved,
	// while the job records only its own paths (never stale warm-pool output).
	for _, rel := range rels {
		pkgPath := filepath.Join(pkgDir, rel)
		destPath := filepath.Join(be.artifactDir, rel)
		if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
			return fmt.Errorf("failed to create artifact directory: %w", err)
		}
		if err := be.copyFile(pkgPath, destPath); err != nil {
			return fmt.Errorf("failed to copy artifact: %w", err)
		}
		job.appendLog(fmt.Sprintf("Artifact collected: %s\n", destPath))
	}
	primary := primaryArtifact(rels, job.Request.PackageName, func(rel string) int64 {
		if info, statErr := os.Stat(filepath.Join(be.artifactDir, rel)); statErr == nil {
			return info.Size()
		}
		return 0
	})
	job.setArtifactURL(filepath.Join(be.artifactDir, primary))
	job.setArtifacts(rels)

	return nil
}

// copyFile copies a file from src to dst.
func (be *BuildExecutor) copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		_ = sourceFile.Close()
	}()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		_ = destFile.Close()
	}()

	if _, err := sourceFile.WriteTo(destFile); err != nil {
		return err
	}

	return destFile.Sync()
}

// verifyRepositoryRevisions fails closed when a catalog-pinned git repository
// is not already present at the requested commit. The build path deliberately
// does not fetch or check out here: image/mirror preparation owns mutation,
// while package jobs only consume immutable inputs.
func verifyRepositoryRevisions(ctx context.Context, repos []RepoConfig) error {
	for _, repo := range repos {
		if repo.Revision == "" {
			continue
		}
		if repo.SyncType != "git" {
			return fmt.Errorf("repository %q pins a revision but is not git-backed", repo.Name)
		}
		output, err := exec.CommandContext(ctx, "git", "-C", repo.Location, "rev-parse", "HEAD").Output()
		if err != nil {
			return fmt.Errorf("read repository %q revision: %w", repo.Name, err)
		}
		status, err := exec.CommandContext(ctx, "git", "-C", repo.Location, "status", "--porcelain=v1", "--untracked-files=all").Output()
		if err != nil {
			return fmt.Errorf("read repository %q worktree state: %w", repo.Name, err)
		}
		if err := validateRepositoryState(repo.Revision, string(output), string(status)); err != nil {
			return fmt.Errorf("repository %q: %w", repo.Name, err)
		}
	}
	return nil
}

func revisionMatches(expected, actual string) bool {
	expected = strings.ToLower(strings.TrimSpace(expected))
	actual = strings.ToLower(strings.TrimSpace(actual))
	return len(expected) >= 7 && strings.HasPrefix(actual, expected)
}

func validateRepositoryState(expected, actual, porcelain string) error {
	if !revisionMatches(expected, actual) {
		return fmt.Errorf("is at %s, expected %s", strings.TrimSpace(actual), expected)
	}
	if strings.TrimSpace(porcelain) != "" {
		return fmt.Errorf("worktree has tracked or untracked modifications")
	}
	return nil
}
