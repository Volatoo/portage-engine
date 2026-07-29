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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	case "setup":
		runSetup(args)
	case "configure":
		runConfigure(args)
	case "build":
		runBuild(args)
	case "status":
		runStatus(args)
	case "whoami":
		runWhoAmI(args)
	case "token-exchange":
		runTokenExchange(args)
	case "project-policy":
		runProjectPolicy(args)
	case "project-policy-set":
		runProjectPolicySet(args)
	case "sessions":
		runSessions(args)
	case "session-revoke":
		runSessionRevoke(args)
	case "sessions-revoke-all":
		runSessionsRevokeAll(args)
	case "workload-identities":
		runWorkloadIdentities(args)
	case "workload-revoke-cert":
		runWorkloadRevoke(args, false)
	case "workload-revoke-issuer":
		runWorkloadRevoke(args, true)
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
  setup       Verify the independently published release-key fingerprint,
              install that key into Portage's keyring, and configure binrepo.

  configure   Point Portage at the server's binhost (writes binrepos.conf).
              After this, install packages the normal way:
                emerge --getbinpkg <pkg>
              (or add FEATURES="getbinpkg" to make.conf to make it automatic).

  build       Request the server build a package (Portage has no native way to
              do this). Optionally wait for completion with -wait.

  status      Show the status of a previously requested build job.

  whoami      Show the authenticated identity and authorized projects.
  token-exchange  Exchange one upstream provider credential for a short-lived
                  Portage Engine session. The credential is read from an
                  environment variable or stdin, never from a command flag.

  project-policy      Show limits and current usage for one project.
  project-policy-set  Replace a project's admission policy (owner only).
  sessions            List redacted federated session metadata.
  session-revoke      Revoke one session (defaults to the current session).
  sessions-revoke-all Revoke all sessions for an identity (requires step-up).
  workload-identities  List workload issuer generations and recent leaves.
  workload-revoke-cert Revoke one workload leaf and its attempt (step-up).
  workload-revoke-issuer Revoke one issuer generation and its workers (step-up).

  bundle      Generate a Portage config bundle file (USE flags, make.conf, ...)
              without submitting a build.

Run 'portage-client <command> -h' for command-specific flags.

Examples:
  # Recommended one-time trust bootstrap. Obtain the fingerprint through an
  # independent operator-controlled channel, not from this same command.
  sudo portage-client setup -server=https://portage.example.org \
    -expected-fingerprint=<FULL_PRIMARY_FINGERPRINT> \
    -profile-id=pe/amd64/glibc/systemd/base-v1

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

func runTokenExchange(args []string) {
	fs := flag.NewFlagSet("token-exchange", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "Server URL")
	providerID := fs.String("provider", "", "Configured identity provider ID")
	credentialEnv := fs.String(
		"credential-env", "PORTAGE_PROVIDER_CREDENTIAL",
		"Environment variable containing the upstream credential",
	)
	fromStdin := fs.Bool("credential-stdin", false, "Read the upstream credential from stdin")
	out := fs.String("out", "", "Write the platform token to a mode-0600 file instead of stdout")
	_ = fs.Parse(args)
	if strings.TrimSpace(*providerID) == "" {
		log.Fatal("-provider is required")
	}
	credential := ""
	if *fromStdin {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<10))
		if err != nil {
			log.Fatalf("read provider credential: %v", err)
		}
		credential = strings.TrimSpace(string(data))
	} else {
		credential = strings.TrimSpace(os.Getenv(*credentialEnv))
	}
	if credential == "" {
		log.Fatalf("provider credential is empty; set %s or use -credential-stdin", *credentialEnv)
	}
	result, err := exchangeProviderCredential(
		&http.Client{Timeout: httpTimeout}, strings.TrimRight(*server, "/"),
		*providerID, credential,
	)
	if err != nil {
		log.Fatalf("identity exchange failed: %v", err)
	}
	if *out != "" {
		// #nosec G306 -- a bearer session is intentionally written owner-only.
		if err := os.WriteFile(*out, []byte(result.AccessToken+"\n"), 0o600); err != nil {
			log.Fatalf("write platform token: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Wrote Portage Engine session to %s (expires in %ds)\n",
			*out, result.ExpiresIn)
		return
	}
	fmt.Println(result.AccessToken)
}

type tokenExchangeResult struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

func exchangeProviderCredential(
	client *http.Client,
	server, providerID, credential string,
) (tokenExchangeResult, error) {
	payload, err := json.Marshal(map[string]string{
		"provider_id": strings.TrimSpace(providerID),
		"credential":  strings.TrimSpace(credential),
	})
	if err != nil {
		return tokenExchangeResult{}, err
	}
	request, err := http.NewRequest(
		http.MethodPost, server+"/api/v1/iam/exchange", bytes.NewReader(payload),
	)
	if err != nil {
		return tokenExchangeResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return tokenExchangeResult{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return tokenExchangeResult{}, fmt.Errorf(
			"server returned %s: %s", response.Status, strings.TrimSpace(string(body)),
		)
	}
	var result tokenExchangeResult
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return tokenExchangeResult{}, err
	}
	if !strings.HasPrefix(result.AccessToken, "pe1_") ||
		!strings.EqualFold(result.TokenType, "Bearer") || result.ExpiresIn < 1 {
		return tokenExchangeResult{}, fmt.Errorf("server returned an invalid platform session")
	}
	return result, nil
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
	content, err := writeBinrepoConfig(
		base, selected, *name, *priority, *verify, *out,
	)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Wrote %s:\n\n%s\n", *out, content)
	fmt.Printf("Selected profile %s (%s), binhost path %s\n", selected.ProfileID, selected.Arch, selected.BinhostPath)
	fmt.Println("Next: enable binary fetching, then install as usual, e.g.:")
	fmt.Println("  emerge --getbinpkg <pkg>")
	fmt.Println("  # or add to /etc/portage/make.conf:  FEATURES=\"getbinpkg\"")
}

func runSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "Server URL")
	profileID := fs.String("profile-id", "", "Catalog profile ID")
	expected := fs.String(
		"expected-fingerprint", "",
		"Full primary fingerprint obtained through an independent trusted channel",
	)
	keyring := fs.String(
		"keyring", "/etc/portage/gnupg", "Portage verification GnuPG home",
	)
	out := fs.String(
		"out", "/etc/portage/binrepos.conf/portage-engine.conf",
		"Output binrepos.conf path",
	)
	name := fs.String("name", "portage-engine", "binrepo name")
	priority := fs.Int("priority", 1, "binrepo priority")
	_ = fs.Parse(args)
	expectedFingerprint, err := normalizeFingerprint(*expected)
	if err != nil {
		log.Fatalf("-expected-fingerprint: %v", err)
	}
	base := strings.TrimRight(*server, "/")
	client := &http.Client{Timeout: httpTimeout}
	selected, err := fetchBinhostProfile(client, base, *profileID)
	if err != nil {
		log.Fatalf("resolve binhost profile: %v", err)
	}
	publicKey, err := fetchReleasePublicKey(client, base)
	if err != nil {
		log.Fatalf("fetch release public key: %v", err)
	}
	fingerprint, err := inspectPublicKeyFingerprint(publicKey)
	if err != nil {
		log.Fatalf("inspect release public key: %v", err)
	}
	if fingerprint != expectedFingerprint {
		log.Fatalf(
			"release key fingerprint mismatch: got %s, expected %s",
			fingerprint, expectedFingerprint,
		)
	}
	if err := installPortagePublicKey(*keyring, publicKey, fingerprint); err != nil {
		log.Fatalf("install Portage release key: %v", err)
	}
	content, err := writeBinrepoConfig(
		base, selected, *name, *priority, true, *out,
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Trusted release key %s in %s\n", fingerprint, *keyring)
	fmt.Printf("Wrote %s:\n\n%s\n", *out, content)
	fmt.Println("Next: emerge --getbinpkg <pkg>")
}

func fetchReleasePublicKey(client *http.Client, base string) ([]byte, error) {
	response, err := client.Get(base + "/api/v1/gpg/public-key")
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf(
			"server returned %s: %s",
			response.Status, strings.TrimSpace(string(body)),
		)
	}
	publicKey, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(publicKey) == 0 || len(publicKey) > 1<<20 {
		return nil, fmt.Errorf("release public key is empty or exceeds 1 MiB")
	}
	return publicKey, nil
}

func normalizeFingerprint(value string) (string, error) {
	value = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(value), " ", ""))
	if len(value) < 40 || len(value) > 64 {
		return "", fmt.Errorf("full 40..64 hexadecimal primary fingerprint is required")
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789ABCDEF", character) {
			return "", fmt.Errorf("fingerprint must be hexadecimal")
		}
	}
	return value, nil
}

func inspectPublicKeyFingerprint(publicKey []byte) (string, error) {
	scratch, err := os.MkdirTemp("", "portage-client-key-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	keyFile := filepath.Join(scratch, "release.asc")
	if err := os.WriteFile(keyFile, publicKey, 0o600); err != nil {
		return "", err
	}
	command := exec.Command( // #nosec G204 -- fixed gpg executable and server-owned key file.
		"gpg", "--homedir", scratch, "--batch", "--no-options",
		"--with-colons", "--import-options", "show-only", "--import", keyFile,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gpg show-only failed: %w: %s",
			err, strings.TrimSpace(string(output)))
	}
	fingerprints := primaryFingerprints(output)
	if len(fingerprints) != 1 {
		return "", fmt.Errorf(
			"public key bundle must contain exactly one primary key, found %d",
			len(fingerprints),
		)
	}
	return normalizeFingerprint(fingerprints[0])
}

func primaryFingerprints(colonOutput []byte) []string {
	var result []string
	want := false
	for _, line := range strings.Split(string(colonOutput), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "pub" {
			want = true
			continue
		}
		if want && fields[0] == "fpr" && len(fields) > 9 {
			result = append(result, fields[9])
			want = false
		}
	}
	return result
}

func installPortagePublicKey(
	keyring string,
	publicKey []byte,
	fingerprint string,
) error {
	if err := os.MkdirAll(keyring, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(keyring, 0o700); err != nil { // #nosec G302 -- GnuPG home must be owner-only.
		return err
	}
	temp, err := os.CreateTemp(keyring, ".release-key-*.asc")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(publicKey); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	importKey := exec.Command( // #nosec G204 -- fixed gpg executable and operator-selected confined paths.
		"gpg", "--homedir", keyring, "--batch", "--no-options", "--import", name,
	)
	if output, err := importKey.CombinedOutput(); err != nil {
		return fmt.Errorf("gpg import failed: %w: %s",
			err, strings.TrimSpace(string(output)))
	}
	ownerTrust := exec.Command( // #nosec G204 -- fixed gpg executable and operator-selected keyring.
		"gpg", "--homedir", keyring, "--batch", "--no-options", "--import-ownertrust",
	)
	ownerTrust.Stdin = strings.NewReader(fingerprint + ":6:\n")
	if output, err := ownerTrust.CombinedOutput(); err != nil {
		return fmt.Errorf("gpg ownertrust failed: %w: %s",
			err, strings.TrimSpace(string(output)))
	}
	return nil
}

func writeBinrepoConfig(
	base string,
	selected *binhostProfile,
	name string,
	priority int,
	verify bool,
	out string,
) (string, error) {
	content := fmt.Sprintf(
		"[%s]\npriority = %d\nsync-uri = %s%s\nverify-signature = %t\n",
		name, priority, base, selected.SyncPath, verify,
	)
	// Portage configuration is intentionally world-readable.
	if err := os.MkdirAll(dirOf(out), 0o755); err != nil { // #nosec G301 -- Portage config directory.
		return "", fmt.Errorf("create %s: %w", dirOf(out), err)
	}
	if err := os.WriteFile(out, []byte(content), 0o644); err != nil { // #nosec G306 -- Portage config file.
		return "", fmt.Errorf("write %s: %w", out, err)
	}
	return content, nil
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
	apiKey := fs.String("api-key", os.Getenv("PORTAGE_ENGINE_API_KEY"), "Legacy administrator API key")
	token := fs.String("token", os.Getenv("PORTAGE_ENGINE_TOKEN"), "Portage Engine federated session token")
	project := fs.String("project", os.Getenv("PORTAGE_ENGINE_PROJECT"), "Project UUID or name")
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
	auth := requestAuth{APIKey: *apiKey, BearerToken: *token, Project: *project}
	if err := auth.Validate(); err != nil {
		log.Fatalf("build: %v", err)
	}

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
		jobID, err := postSubmit(client, base, auth, req)
		if err != nil {
			log.Printf("build submit failed for %s: %v", pkg.Atom, err)
			failures++
			continue
		}
		fmt.Printf("Build submitted for %s (job ID: %s)\n", pkg.Atom, jobID)

		if *wait {
			if err := pollStatus(client, base, auth, jobID); err != nil {
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
	apiKey := fs.String("api-key", os.Getenv("PORTAGE_ENGINE_API_KEY"), "Legacy administrator API key")
	token := fs.String("token", os.Getenv("PORTAGE_ENGINE_TOKEN"), "Portage Engine federated session token")
	project := fs.String("project", os.Getenv("PORTAGE_ENGINE_PROJECT"), "Project UUID or name")
	jobID := fs.String("job", "", "Job ID")
	_ = fs.Parse(args)
	auth := requestAuth{APIKey: *apiKey, BearerToken: *token, Project: *project}
	if err := auth.Validate(); err != nil {
		log.Fatalf("status: %v", err)
	}

	if *jobID == "" {
		log.Fatal("status: -job is required")
	}

	base := strings.TrimRight(*server, "/")
	client := &http.Client{Timeout: httpTimeout}
	status, errMsg, _, err := fetchStatus(client, base, auth, *jobID)
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

type requestAuth struct {
	APIKey      string
	BearerToken string
	StepUpKey   string
	StepUpToken string
	Project     string
}

func (a requestAuth) Validate() error {
	if a.APIKey != "" && a.BearerToken != "" {
		return fmt.Errorf("use either the legacy API key or a federated session token, not both")
	}
	if a.StepUpKey != "" && a.StepUpToken != "" {
		return fmt.Errorf("use either a legacy step-up key or a fresh federated step-up token, not both")
	}
	if a.APIKey != "" && a.StepUpToken != "" {
		return fmt.Errorf("a fresh federated step-up token requires federated session authentication")
	}
	if a.BearerToken != "" && a.StepUpKey != "" {
		return fmt.Errorf("a legacy step-up key requires legacy API-key authentication")
	}
	return nil
}

func (a requestAuth) Apply(request *http.Request) {
	if a.BearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+a.BearerToken)
	} else if a.APIKey != "" {
		request.Header.Set("X-API-Key", a.APIKey)
	}
	if a.StepUpToken != "" {
		request.Header.Set("X-Step-Up-Authorization", "Bearer "+a.StepUpToken)
	} else if a.StepUpKey != "" {
		request.Header.Set("X-Step-Up-Key", a.StepUpKey)
	}
	if a.Project != "" {
		request.Header.Set("X-Project-ID", a.Project)
	}
}

// postSubmit POSTs a config-bundle build to /api/v1/builds/submit.
func postSubmit(c *http.Client, base string, auth requestAuth, req *builder.LocalBuildRequest) (string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, base+"/api/v1/builds/submit", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	auth.Apply(httpReq)

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
func pollStatus(c *http.Client, base string, auth requestAuth, jobID string) error {
	for {
		status, errMsg, terminal, err := fetchStatus(c, base, auth, jobID)
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
func fetchStatus(c *http.Client, base string, auth requestAuth, jobID string) (status, errMsg string, terminal bool, err error) {
	httpReq, err := http.NewRequest(http.MethodGet, base+"/api/v1/packages/status?job_id="+jobID, nil)
	if err != nil {
		return "", "", false, err
	}
	auth.Apply(httpReq)

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

func runWhoAmI(args []string) {
	fs := flag.NewFlagSet("whoami", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "Server URL")
	apiKey := fs.String("api-key", os.Getenv("PORTAGE_ENGINE_API_KEY"), "Legacy administrator API key")
	token := fs.String("token", os.Getenv("PORTAGE_ENGINE_TOKEN"), "Portage Engine federated session token")
	_ = fs.Parse(args)
	auth := requestAuth{APIKey: *apiKey, BearerToken: *token}
	if err := auth.Validate(); err != nil {
		log.Fatalf("whoami: %v", err)
	}

	request, err := http.NewRequest(
		http.MethodGet,
		strings.TrimRight(*server, "/")+"/api/v1/iam/me",
		nil,
	)
	if err != nil {
		log.Fatal(err)
	}
	auth.Apply(request)
	response, err := (&http.Client{Timeout: httpTimeout}).Do(request)
	if err != nil {
		log.Fatalf("whoami: %v", err)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	closeErr := response.Body.Close()
	if err != nil {
		log.Fatalf("whoami: %v", err)
	}
	if closeErr != nil {
		log.Fatalf("whoami: close response: %v", closeErr)
	}
	if response.StatusCode != http.StatusOK {
		log.Fatalf("whoami: server returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		log.Fatalf("whoami: decode response: %v", err)
	}
	output, _ := json.MarshalIndent(decoded, "", "  ")
	fmt.Println(string(output))
}

func runProjectPolicy(args []string) {
	fs := flag.NewFlagSet("project-policy", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "Server URL")
	apiKey := fs.String("api-key", os.Getenv("PORTAGE_ENGINE_API_KEY"), "Legacy administrator API key")
	token := fs.String("token", os.Getenv("PORTAGE_ENGINE_TOKEN"), "Portage Engine federated session token")
	project := fs.String("project", os.Getenv("PORTAGE_ENGINE_PROJECT"), "Project ID or name")
	_ = fs.Parse(args)
	auth := requestAuth{APIKey: *apiKey, BearerToken: *token, Project: *project}
	if err := auth.Validate(); err != nil {
		log.Fatalf("project-policy: %v", err)
	}
	body, err := projectPolicyRequest(
		http.MethodGet, strings.TrimRight(*server, "/"), auth, nil,
	)
	if err != nil {
		log.Fatalf("project-policy: %v", err)
	}
	fmt.Println(string(body))
}

func runProjectPolicySet(args []string) {
	fs := flag.NewFlagSet("project-policy-set", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "Server URL")
	apiKey := fs.String("api-key", os.Getenv("PORTAGE_ENGINE_API_KEY"), "Legacy administrator API key")
	token := fs.String("token", os.Getenv("PORTAGE_ENGINE_TOKEN"), "Portage Engine federated session token")
	stepUpKey := fs.String("step-up-key", os.Getenv("PORTAGE_ENGINE_STEP_UP_KEY"), "Independent legacy step-up key")
	stepUpToken := fs.String("step-up-token", os.Getenv("PORTAGE_ENGINE_STEP_UP_TOKEN"), "Fresh Portage Engine federated session token for step-up")
	project := fs.String("project", os.Getenv("PORTAGE_ENGINE_PROJECT"), "Project ID or name")
	version := fs.Int64("version", 0, "Current policy version returned by project-policy")
	suspended := fs.Bool("suspended", false, "Suspend new submissions, retries, and claims")
	priorityWeight := fs.Int(
		"priority-weight", 0,
		"Weighted-fair scheduling share (1-1000; 0 preserves current value)",
	)
	starvationSeconds := fs.Int(
		"starvation-seconds", 0,
		"Maximum normal fair-queue wait before FIFO anti-starvation (30-86400; 0 preserves current value)",
	)
	maxQueued := fs.Int("max-queued", 0, "Maximum queued jobs")
	maxActive := fs.Int("max-active", 0, "Maximum simultaneously active builds")
	maxDaily := fs.Int("max-daily", 0, "Maximum new submissions per UTC day")
	maxVCPUs := fs.Int("max-vcpus", 0, "Maximum vCPUs reserved by active attempts")
	maxMemoryMiB := fs.Int("max-memory-mib", 0, "Maximum memory MiB reserved by active attempts")
	maxDiskGiB := fs.Int("max-disk-gib", 0, "Maximum disk GiB reserved by active attempts")
	maxArtifactBytes := fs.Int64(
		"max-artifact-bytes", 0,
		"Maximum bytes retained in one build attempt's artifact quarantine",
	)
	maxDailyBuildMinutes := fs.Int64(
		"max-daily-build-minutes", 0,
		"Maximum reserved/charged build minutes per UTC day",
	)
	maxDailyCloudCost := fs.Int64(
		"max-daily-cloud-cost-microunits", 0,
		"Maximum estimated cloud-cost microunits per UTC day",
	)
	maxFailuresHour := fs.Int(
		"max-failures-hour", 0,
		"Failed or expired attempts per trailing hour before automatic cooldown",
	)
	abuseCooldownSeconds := fs.Int(
		"abuse-cooldown-seconds", 0,
		"Automatic abuse suspension duration in seconds",
	)
	clearAbuseSuspension := fs.Bool(
		"clear-abuse-suspension", false,
		"Clear the current automatic failure-storm suspension",
	)
	maxClaimed := fs.Int("max-claimed", 0, "Maximum attempts waiting at the claimed checkpoint")
	maxProvision := fs.Int("max-provision", 0, "Maximum attempts in the provision phase")
	maxBuild := fs.Int("max-build", 0, "Maximum attempts in the build phase")
	maxVerify := fs.Int("max-verify", 0, "Maximum attempts in the verify phase")
	maxPublish := fs.Int("max-publish", 0, "Maximum attempts in the publish phase")
	_ = fs.Parse(args)
	auth := requestAuth{
		APIKey: *apiKey, BearerToken: *token,
		StepUpKey: *stepUpKey, StepUpToken: *stepUpToken, Project: *project,
	}
	if err := auth.Validate(); err != nil {
		log.Fatalf("project-policy-set: %v", err)
	}
	if *version <= 0 || *maxQueued <= 0 || *maxActive <= 0 || *maxDaily <= 0 ||
		*maxVCPUs <= 0 || *maxMemoryMiB <= 0 || *maxDiskGiB <= 0 ||
		*maxArtifactBytes <= 0 || *maxClaimed <= 0 || *maxProvision <= 0 ||
		*maxBuild <= 0 || *maxVerify <= 0 || *maxPublish <= 0 ||
		*maxDailyBuildMinutes <= 0 || *maxDailyCloudCost <= 0 ||
		*maxFailuresHour <= 0 || *abuseCooldownSeconds <= 0 {
		log.Fatal("project-policy-set: version, job limits, and resource limits must be positive")
	}
	if *priorityWeight < 0 || *priorityWeight > 1000 ||
		*starvationSeconds < 0 || *starvationSeconds > 86400 ||
		(*starvationSeconds > 0 && *starvationSeconds < 30) {
		log.Fatal("project-policy-set: fairness weight or starvation threshold is invalid")
	}
	payload := map[string]any{
		"version": *version, "suspended": *suspended,
		"priority_weight":              *priorityWeight,
		"starvation_threshold_seconds": *starvationSeconds,
		"max_queued_jobs":              *maxQueued, "max_active_jobs": *maxActive,
		"max_daily_submissions": *maxDaily,
		"max_active_vcpus":      *maxVCPUs, "max_active_memory_mib": *maxMemoryMiB,
		"max_active_disk_gib":             *maxDiskGiB,
		"max_artifact_bytes_per_job":      *maxArtifactBytes,
		"max_daily_build_seconds":         *maxDailyBuildMinutes * 60,
		"max_daily_cloud_cost_microunits": *maxDailyCloudCost,
		"max_failures_per_hour":           *maxFailuresHour,
		"abuse_cooldown_seconds":          *abuseCooldownSeconds,
		"clear_abuse_suspension":          *clearAbuseSuspension,
		"max_claimed_attempts":            *maxClaimed,
		"max_provision_attempts":          *maxProvision,
		"max_build_attempts":              *maxBuild,
		"max_verify_attempts":             *maxVerify,
		"max_publish_attempts":            *maxPublish,
	}
	body, err := projectPolicyRequest(
		http.MethodPut, strings.TrimRight(*server, "/"), auth, payload,
	)
	if err != nil {
		log.Fatalf("project-policy-set: %v", err)
	}
	fmt.Println(string(body))
}

func projectPolicyRequest(
	method, base string,
	auth requestAuth,
	payload any,
) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, base+"/api/v1/projects/policy", body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	auth.Apply(request)
	response, err := (&http.Client{Timeout: httpTimeout}).Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("decode policy response: %w", err)
	}
	output, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		return nil, err
	}
	return output, nil
}

func runSessions(args []string) {
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "Server URL")
	token := fs.String("token", os.Getenv("PORTAGE_ENGINE_TOKEN"), "Portage Engine federated session token")
	subjectID := fs.String("subject-id", "", "Subject UUID (system administrators may inspect another subject)")
	_ = fs.Parse(args)
	auth := requestAuth{BearerToken: *token}
	path := "/api/v1/iam/sessions"
	if strings.TrimSpace(*subjectID) != "" {
		path += "?subject_id=" + url.QueryEscape(strings.TrimSpace(*subjectID))
	}
	printSessionResponse("sessions", http.MethodGet, *server, path, auth, nil)
}

func runSessionRevoke(args []string) {
	fs := flag.NewFlagSet("session-revoke", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "Server URL")
	token := fs.String("token", os.Getenv("PORTAGE_ENGINE_TOKEN"), "Portage Engine federated session token")
	stepUpToken := fs.String("step-up-token", os.Getenv("PORTAGE_ENGINE_STEP_UP_TOKEN"), "Fresh federated session token when revoking another subject's session")
	sessionID := fs.String("session-id", "", "Session UUID; empty revokes the current session")
	_ = fs.Parse(args)
	auth := requestAuth{BearerToken: *token, StepUpToken: *stepUpToken}
	path := "/api/v1/iam/sessions"
	if strings.TrimSpace(*sessionID) != "" {
		path += "?session_id=" + url.QueryEscape(strings.TrimSpace(*sessionID))
	}
	printSessionResponse("session-revoke", http.MethodDelete, *server, path, auth, nil)
}

func runSessionsRevokeAll(args []string) {
	fs := flag.NewFlagSet("sessions-revoke-all", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "Server URL")
	token := fs.String("token", os.Getenv("PORTAGE_ENGINE_TOKEN"), "Portage Engine federated session token")
	stepUpToken := fs.String("step-up-token", os.Getenv("PORTAGE_ENGINE_STEP_UP_TOKEN"), "Fresh Portage Engine federated session token")
	subjectID := fs.String("subject-id", "", "Subject UUID; empty revokes the current subject")
	reason := fs.String("reason", "", "Audit reason")
	_ = fs.Parse(args)
	auth := requestAuth{BearerToken: *token, StepUpToken: *stepUpToken}
	payload := map[string]string{
		"subject_id": strings.TrimSpace(*subjectID),
		"reason":     strings.TrimSpace(*reason),
	}
	printSessionResponse(
		"sessions-revoke-all", http.MethodPost, *server,
		"/api/v1/iam/sessions/revoke-all", auth, payload,
	)
}

func runWorkloadIdentities(args []string) {
	fs := flag.NewFlagSet("workload-identities", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "Server URL")
	apiKey := fs.String("api-key", os.Getenv("PORTAGE_ENGINE_API_KEY"), "Legacy administrator API key")
	token := fs.String("token", os.Getenv("PORTAGE_ENGINE_TOKEN"), "Portage Engine federated session token")
	_ = fs.Parse(args)
	printSessionResponse(
		"workload-identities", http.MethodGet, *server,
		"/api/v1/worker-gateway/identities",
		requestAuth{APIKey: *apiKey, BearerToken: *token}, nil,
	)
}

func runWorkloadRevoke(args []string, issuer bool) {
	command, path, noun := "workload-revoke-cert",
		"/api/v1/worker-gateway/certificates/revoke", "leaf"
	if issuer {
		command, path, noun = "workload-revoke-issuer",
			"/api/v1/worker-gateway/issuers/revoke", "issuer generation"
	}
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "Server URL")
	apiKey := fs.String("api-key", os.Getenv("PORTAGE_ENGINE_API_KEY"), "Legacy administrator API key")
	token := fs.String("token", os.Getenv("PORTAGE_ENGINE_TOKEN"), "Portage Engine federated session token")
	stepUpKey := fs.String("step-up-key", os.Getenv("PORTAGE_ENGINE_STEP_UP_KEY"), "Independent legacy step-up key")
	stepUpToken := fs.String("step-up-token", os.Getenv("PORTAGE_ENGINE_STEP_UP_TOKEN"), "Fresh Portage Engine federated session token")
	fingerprint := fs.String("fingerprint", "", "Lowercase SHA-256 fingerprint of the "+noun)
	reason := fs.String("reason", "", "Required audit and revocation reason")
	_ = fs.Parse(args)
	if len(strings.TrimSpace(*fingerprint)) != 64 ||
		strings.TrimSpace(*reason) == "" {
		log.Fatalf("%s: -fingerprint and -reason are required", command)
	}
	printSessionResponse(
		command, http.MethodPost, *server, path,
		requestAuth{
			APIKey: *apiKey, BearerToken: *token,
			StepUpKey: *stepUpKey, StepUpToken: *stepUpToken,
		},
		map[string]string{
			"fingerprint": strings.ToLower(strings.TrimSpace(*fingerprint)),
			"reason":      strings.TrimSpace(*reason),
		},
	)
}

func printSessionResponse(
	command, method, serverURL, path string,
	auth requestAuth,
	payload any,
) {
	if err := auth.Validate(); err != nil {
		log.Fatalf("%s: %v", command, err)
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			log.Fatalf("%s: %v", command, err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(
		method, strings.TrimRight(serverURL, "/")+path, body,
	)
	if err != nil {
		log.Fatalf("%s: %v", command, err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	auth.Apply(request)
	response, err := (&http.Client{Timeout: httpTimeout}).Do(request)
	if err != nil {
		log.Fatalf("%s: %v", command, err)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	closeErr := response.Body.Close()
	if err != nil {
		log.Fatalf("%s: %v", command, err)
	}
	if closeErr != nil {
		log.Fatalf("%s: close response: %v", command, closeErr)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		log.Fatalf(
			"%s: server returned %d: %s",
			command, response.StatusCode, strings.TrimSpace(string(data)),
		)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		log.Fatalf("%s: decode response: %v", command, err)
	}
	output, _ := json.MarshalIndent(decoded, "", "  ")
	fmt.Println(string(output))
}
