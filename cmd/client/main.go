// Package main provides the Portage Engine client.
//
// The client is a management/request tool, NOT the way packages are consumed.
// Consuming prebuilt packages is done natively by Portage: run `configure` once
// to point /etc/portage/binrepos.conf at the server's binhost, then use the
// normal `emerge --getbinpkg <pkg>` (emerge fetches from the binhost and falls
// back to a source build automatically). Portage has no native "request a
// build" mechanism, so the `build`/`status` subcommands cover that gap.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/builder"
)

const httpTimeout = 60 * time.Second

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "configure":
		runConfigure(args)
	case "build":
		runBuild(args)
	case "status":
		runStatus(args)
	case "bundle":
		runBundle(args)
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`Portage Engine client

Usage: portage-client <command> [flags]

Commands:
  configure   Point Portage at the server's binhost (writes binrepos.conf).
              After this, install packages the normal way:
                emerge --getbinpkg <pkg>
              (or add FEATURES="getbinpkg" to make.conf to make it automatic).

  build       Request the server build a package (Portage has no native way to
              do this). Optionally wait for completion with -wait.

  status      Show the status of a previously requested build job.

  bundle      Generate a Portage config bundle file (USE flags, make.conf, ...)
              without submitting a build.

Run 'portage-client <command> -h' for command-specific flags.

Examples:
  # One-time: configure the consume path, then install natively.
  sudo portage-client configure -server=http://binhost:8080 \
    -profile-id=pe/amd64/glibc/systemd/base-v1
  emerge --getbinpkg dev-lang/python

  # Ask the server to build a package with specific USE flags, and wait.
  portage-client build -package=dev-lang/python -version=3.11 \
    -profile-id=pe/amd64/glibc/systemd/base-v1 -use=ssl,threads -wait

  # Check a job later.
  portage-client status -job=<job-id>
`)
}

// --- configure: write binrepos.conf for the native consume path ---

func runConfigure(args []string) {
	fs := flag.NewFlagSet("configure", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "Server URL (binhost base)")
	profileID := fs.String("profile-id", "", "Catalog profile ID (default profile when omitted)")
	name := fs.String("name", "portage-engine", "binrepo name")
	priority := fs.Int("priority", 1, "binrepo priority")
	out := fs.String("out", "/etc/portage/binrepos.conf/portage-engine.conf", "Output binrepos.conf path")
	verify := fs.Bool("verify-signature", true, "Require GPG signature verification")
	_ = fs.Parse(args)

	base := strings.TrimRight(*server, "/")
	selected, err := fetchBinhostProfile(&http.Client{Timeout: httpTimeout}, base, *profileID)
	if err != nil {
		log.Fatalf("failed to resolve binhost profile: %v", err)
	}
	content := fmt.Sprintf("[%s]\npriority = %d\nsync-uri = %s%s\nverify-signature = %t\n",
		*name, *priority, base, selected.SyncPath, *verify)

	// binrepos.conf lives under /etc/portage and must be world-readable so
	// Portage (and emerge run as any user) can read the binhost definition.
	if err := os.MkdirAll(dirOf(*out), 0o755); err != nil { // #nosec G301 -- Portage config dir must be world-readable.
		log.Fatalf("failed to create %s: %v", dirOf(*out), err)
	}
	if err := os.WriteFile(*out, []byte(content), 0o644); err != nil { // #nosec G306 -- Portage config file must be world-readable.
		log.Fatalf("failed to write %s: %v", *out, err)
	}

	fmt.Printf("Wrote %s:\n\n%s\n", *out, content)
	fmt.Printf("Selected profile %s (%s), binhost path %s\n", selected.ProfileID, selected.Arch, selected.BinhostPath)
	fmt.Println("Next: enable binary fetching, then install as usual, e.g.:")
	fmt.Println("  emerge --getbinpkg <pkg>")
	fmt.Println("  # or add to /etc/portage/make.conf:  FEATURES=\"getbinpkg\"")
}

type binhostProfile struct {
	ProfileID   string `json:"profile_id"`
	Arch        string `json:"arch"`
	BinhostPath string `json:"binhost_path"`
	Default     bool   `json:"default"`
	SyncPath    string `json:"sync_path"`
}

func fetchBinhostProfile(client *http.Client, base, profileID string) (*binhostProfile, error) {
	response, err := client.Get(base + "/api/v1/binhosts")
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("server returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var inventory struct {
		Binhosts []binhostProfile `json:"binhosts"`
	}
	if err := json.NewDecoder(response.Body).Decode(&inventory); err != nil {
		return nil, fmt.Errorf("decode binhost inventory: %w", err)
	}
	for i := range inventory.Binhosts {
		item := &inventory.Binhosts[i]
		if (profileID != "" && item.ProfileID == profileID) || (profileID == "" && item.Default) {
			if !strings.HasPrefix(item.SyncPath, "/binpkgs/") || item.BinhostPath == "" {
				return nil, fmt.Errorf("server returned an invalid binhost path for profile %q", item.ProfileID)
			}
			return item, nil
		}
	}
	if profileID != "" {
		return nil, fmt.Errorf("profile %q was not published by the server", profileID)
	}
	return nil, fmt.Errorf("server did not publish a default binhost profile")
}

func dirOf(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "."
}

// --- build: request the server build a package ---

func runBuild(args []string) {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "Server URL")
	apiKey := fs.String("api-key", os.Getenv("PORTAGE_ENGINE_API_KEY"), "API key (or PORTAGE_ENGINE_API_KEY)")
	packageName := fs.String("package", "", "Package atom (e.g., dev-lang/python)")
	packageVersion := fs.String("version", "", "Package version")
	useFlags := fs.String("use", "", "USE flags (comma-separated)")
	keywords := fs.String("keywords", "", "Keywords (comma-separated)")
	configFile := fs.String("config", "", "Portage configuration file (JSON)")
	portageDir := fs.String("portage-dir", "", "Read configuration from a Portage directory (e.g., /etc/portage)")
	arch := fs.String("arch", "amd64", "Target architecture")
	profileID := fs.String("profile-id", "", "Server catalog profile ID")
	profile := fs.String("profile", "", "Legacy Portage profile path (catalog compatibility mapping)")
	repositoryIDs := fs.String("repositories", "", "Approved repository IDs (comma-separated)")
	resourceClass := fs.String("resource-class", "", "Server catalog resource class")
	userID := fs.String("user", "default", "User ID")
	description := fs.String("desc", "", "Build description")
	wait := fs.Bool("wait", false, "Wait for the build to complete")
	_ = fs.Parse(args)

	if *packageName == "" && *configFile == "" && *portageDir == "" {
		log.Fatal("build: one of -package, -config, or -portage-dir is required")
	}

	config := loadPortageConfig(*portageDir, *configFile)
	specs := createPackageSpecs(*packageName, *packageVersion, parseCSV(*useFlags), parseCSV(*keywords))
	bundle := createConfigBundle(config, specs, *userID, *arch, *profileID, *profile, *description)

	base := strings.TrimRight(*server, "/")
	client := &http.Client{Timeout: httpTimeout}

	var failures int
	for _, pkg := range bundle.Packages.Packages {
		req := &builder.LocalBuildRequest{
			PackageName:   pkg.Atom,
			Version:       pkg.Version,
			Arch:          *arch,
			ProfileID:     *profileID,
			RepositoryIDs: parseCSV(*repositoryIDs),
			ResourceClass: *resourceClass,
			ConfigBundle:  bundle,
		}
		jobID, err := postSubmit(client, base, *apiKey, req)
		if err != nil {
			log.Printf("build submit failed for %s: %v", pkg.Atom, err)
			failures++
			continue
		}
		fmt.Printf("Build submitted for %s (job ID: %s)\n", pkg.Atom, jobID)

		if *wait {
			if err := pollStatus(client, base, *apiKey, jobID); err != nil {
				log.Printf("build %s did not complete successfully: %v", jobID, err)
				failures++
			}
		}
	}
	if failures > 0 {
		os.Exit(1)
	}
}

// --- status: query one job ---

func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "Server URL")
	apiKey := fs.String("api-key", os.Getenv("PORTAGE_ENGINE_API_KEY"), "API key (or PORTAGE_ENGINE_API_KEY)")
	jobID := fs.String("job", "", "Job ID")
	_ = fs.Parse(args)

	if *jobID == "" {
		log.Fatal("status: -job is required")
	}

	base := strings.TrimRight(*server, "/")
	client := &http.Client{Timeout: httpTimeout}
	status, errMsg, _, err := fetchStatus(client, base, *apiKey, *jobID)
	if err != nil {
		log.Fatalf("failed to fetch status: %v", err)
	}
	fmt.Printf("Job %s: %s\n", *jobID, status)
	if errMsg != "" {
		fmt.Printf("  error: %s\n", errMsg)
	}
}

// --- bundle: generate a config bundle file ---

func runBundle(args []string) {
	fs := flag.NewFlagSet("bundle", flag.ExitOnError)
	packageName := fs.String("package", "", "Package atom (e.g., dev-lang/python)")
	packageVersion := fs.String("version", "", "Package version")
	useFlags := fs.String("use", "", "USE flags (comma-separated)")
	keywords := fs.String("keywords", "", "Keywords (comma-separated)")
	configFile := fs.String("config", "", "Portage configuration file (JSON)")
	portageDir := fs.String("portage-dir", "", "Read configuration from a Portage directory")
	arch := fs.String("arch", "amd64", "Target architecture")
	profileID := fs.String("profile-id", "", "Server catalog profile ID")
	profile := fs.String("profile", "", "Legacy Portage profile path (catalog compatibility mapping)")
	userID := fs.String("user", "default", "User ID")
	description := fs.String("desc", "", "Build description")
	out := fs.String("out", "", "Output bundle path (required)")
	_ = fs.Parse(args)

	if *out == "" {
		log.Fatal("bundle: -out is required")
	}

	config := loadPortageConfig(*portageDir, *configFile)
	specs := createPackageSpecs(*packageName, *packageVersion, parseCSV(*useFlags), parseCSV(*keywords))
	bundle := createConfigBundle(config, specs, *userID, *arch, *profileID, *profile, *description)

	transfer := builder.NewConfigTransfer("")
	if err := transfer.ExportBundle(bundle, *out); err != nil {
		log.Fatalf("failed to export bundle: %v", err)
	}
	fmt.Printf("Configuration bundle saved to: %s\n", *out)
}

// --- shared helpers ---

func loadPortageConfig(portageDir, configFile string) *builder.PortageConfig {
	switch {
	case portageDir != "":
		transfer := builder.NewConfigTransfer("")
		config, err := transfer.ReadSystemPortageConfig(portageDir)
		if err != nil {
			log.Fatalf("failed to read Portage configuration from %s: %v", portageDir, err)
		}
		log.Printf("Loaded configuration from %s (%d package.use entries, %d repos)",
			portageDir, len(config.PackageUse), len(config.Repos))
		return config
	case configFile != "":
		config, err := loadConfigFromFile(configFile)
		if err != nil {
			log.Fatalf("failed to load config: %v", err)
		}
		return config
	default:
		return &builder.PortageConfig{
			PackageUse:      make(map[string][]string),
			PackageKeywords: make(map[string][]string),
			MakeConf:        make(map[string]string),
			Environment:     make(map[string]string),
		}
	}
}

func parseCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func createPackageSpecs(name, version string, useFlags, keywords []string) []builder.PackageSpec {
	if name == "" {
		return []builder.PackageSpec{}
	}
	return []builder.PackageSpec{{
		Atom:     name,
		Version:  version,
		UseFlags: useFlags,
		Keywords: keywords,
	}}
}

func createConfigBundle(config *builder.PortageConfig, specs []builder.PackageSpec, userID, arch, profileID, profile, desc string) *builder.ConfigBundle {
	packages := &builder.BuildPackageSpec{Packages: specs}
	metadata := builder.BundleMetadata{
		UserID:      userID,
		TargetArch:  arch,
		ProfileID:   profileID,
		Profile:     profile,
		CreatedAt:   time.Now().Format(time.RFC3339),
		Description: desc,
	}
	transfer := builder.NewConfigTransfer("")
	bundle, err := transfer.CreateConfigBundle(config, packages, metadata)
	if err != nil {
		log.Fatalf("failed to create config bundle: %v", err)
	}
	return bundle
}

func loadConfigFromFile(path string) (*builder.PortageConfig, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- user-provided config path.
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	var config builder.PortageConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	return &config, nil
}

// postSubmit POSTs a config-bundle build to /api/v1/builds/submit.
func postSubmit(c *http.Client, base, apiKey string, req *builder.LocalBuildRequest) (string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, base+"/api/v1/builds/submit", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("X-API-Key", apiKey)
	}

	resp, err := c.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		JobID  string `json:"job_id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if out.JobID == "" {
		return "", fmt.Errorf("server did not return a job_id")
	}
	return out.JobID, nil
}

// pollStatus polls until the job succeeds or fails.
func pollStatus(c *http.Client, base, apiKey, jobID string) error {
	for {
		status, errMsg, terminal, err := fetchStatus(c, base, apiKey, jobID)
		if err != nil {
			return err
		}
		fmt.Printf("  [%s] status: %s\n", jobID, status)
		if terminal {
			if status == "failed" {
				return fmt.Errorf("build failed: %s", errMsg)
			}
			return nil
		}
		time.Sleep(5 * time.Second)
	}
}

// fetchStatus queries the status endpoint once.
func fetchStatus(c *http.Client, base, apiKey, jobID string) (status, errMsg string, terminal bool, err error) {
	httpReq, err := http.NewRequest(http.MethodGet, base+"/api/v1/packages/status?job_id="+jobID, nil)
	if err != nil {
		return "", "", false, err
	}
	if apiKey != "" {
		httpReq.Header.Set("X-API-Key", apiKey)
	}

	resp, err := c.Do(httpReq)
	if err != nil {
		return "", "", false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", "", false, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", false, fmt.Errorf("decode status: %w", err)
	}

	term := out.Status == "success" || out.Status == "completed" || out.Status == "failed"
	return out.Status, out.Error, term, nil
}
