package tests

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func readRepositoryFile(t *testing.T, relative string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), relative))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func nginxLocationBlock(t *testing.T, config, header string) string {
	t.Helper()
	start := strings.Index(config, header)
	if start < 0 {
		t.Fatalf("nginx location %q is missing", header)
	}
	remainder := config[start:]
	end := strings.Index(remainder, "\n    }")
	if end < 0 {
		t.Fatalf("nginx location %q is not closed", header)
	}
	return remainder[:end]
}

// nginxServerBlock returns the one `server { ... }` carrying this server_name.
//
// Per-vhost rather than whole-file on purpose: a denial in the API or metrics
// vhost does not cover a route the Dashboard host answers, and a whole-file
// search would read one as if it covered the other.
func nginxServerBlock(t *testing.T, config, serverName string) string {
	t.Helper()
	for _, block := range strings.Split(config, "\nserver {") {
		if strings.Contains(block, serverName) {
			if end := strings.Index(block, "\n}"); end >= 0 {
				return block[:end]
			}
			t.Fatalf("nginx server block %q is not closed", serverName)
		}
	}
	t.Fatalf("nginx server block %q is missing", serverName)
	return ""
}

// nginxIndentScopes splits a server block into what applies to the vhost and the
// multi-line `location` blocks inside it. Single-line locations (`{ return 404; }`)
// stay with the vhost text: they carry no directives to inherit.
func nginxIndentScopes(block string) (server string, locations []string) {
	var vhost, current strings.Builder
	inside := false
	for _, line := range strings.Split(block, "\n") {
		switch {
		case !inside && strings.HasPrefix(line, "    location ") && strings.HasSuffix(line, "{"):
			inside = true
			current.Reset()
			current.WriteString(line + "\n")
		case inside && line == "    }":
			current.WriteString(line)
			locations = append(locations, current.String())
			inside = false
		case inside:
			current.WriteString(line + "\n")
		default:
			vhost.WriteString(line + "\n")
		}
	}
	return vhost.String(), locations
}

func TestPublicEdgeReferenceDeniesBackendAndWebShell(t *testing.T) {
	compose := readRepositoryFile(t, "deploy/public-edge/docker-compose.edge.yml")
	nginx := readRepositoryFile(t, "deploy/public-edge/nginx.conf.template")
	for _, service := range []string{"portage-server:", "portage-dashboard:"} {
		if !strings.Contains(compose, service) {
			t.Fatalf("%s is missing from the edge overlay", service)
		}
	}
	if count := strings.Count(compose, "ports: !reset []"); count != 9 {
		t.Fatalf("private service port reset count=%d, want 9", count)
	}
	if !strings.Contains(compose, "ports: !override") ||
		!strings.Contains(compose, "PORTAGE_WORKER_GATEWAY_BIND") ||
		!strings.Contains(compose, ":9443\"") {
		t.Fatal("server ports do not retain only the private mTLS Worker Gateway")
	}
	for _, required := range []string{
		"internal: true",
		"limit_conn_zone $binary_remote_addr zone=source_ip_connections:",
		"limit_req_zone $binary_remote_addr zone=public_requests:",
		"limit_req_zone $binary_remote_addr zone=identity_requests:",
		"limit_req_status 429;",
		"limit_conn_status 429;",
		"proxy_cookie_flags ~ secure httponly samesite=lax;",
		// Prefix matches, not exact ones. The old console moved under /legacy and
		// took its own /shell/ page with it, and /api/shell gained a /preflight
		// sibling; an exact `= /api/shell` left both reachable while this list
		// still read as complete. TestPublicEdgeDeniesEveryWebShellPathTheDashboardRegisters
		// below is what keeps the two in step from now on.
		"location ^~ /shell/ { return 404; }",
		"location ^~ /legacy/shell/ { return 404; }",
		"location ^~ /api/shell { return 404; }",
		"location = /api/v1/instances/shell { return 404; }",
		"auth_basic_user_file /run/secrets/portage_metrics_htpasswd;",
		"proxy_set_header X-Forwarded-For $remote_addr;",
	} {
		if !strings.Contains(compose+nginx, required) {
			t.Errorf("public edge is missing %q", required)
		}
	}
	if strings.Contains(nginx, "$request $status") {
		t.Fatal("edge access log can persist OAuth callback query strings")
	}
	if strings.Contains(nginx, "$proxy_add_x_forwarded_for") {
		t.Fatal("edge trusts an unreviewed client forwarding chain")
	}
}

// TestPublicEdgeDeniesEveryWebShellPathTheDashboardRegisters reads the shell
// routes out of internal/dashboard instead of listing them here.
//
// docs/PRODUCTION_BOUNDARY.md promises Public Beta routes no WebShell, and the
// deny list that kept that promise was written by hand. It fell behind twice in
// one branch — /legacy/shell/ arrived when the old console moved under its mount,
// and /api/shell/preflight arrived beside an exact `= /api/shell` — and in both
// cases every gate still reported the boundary as verified.
func TestPublicEdgeDeniesEveryWebShellPathTheDashboardRegisters(t *testing.T) {
	nginx := readRepositoryFile(t, "deploy/public-edge/nginx.conf.template")
	block := nginxServerBlock(t, nginx, "server_name ${PORTAGE_PUBLIC_HOST};")

	handler := regexp.MustCompile(`HandleFunc\("(/[^"]*shell[^"]*)"`)
	routeSuffix := regexp.MustCompile(`suffix: "(/[^"]*shell[^"]*)"`)
	legacyRouter := regexp.MustCompile(
		`(?s)func \(d \*Dashboard\) legacyRouter\(\).*?\n}`)

	dashboard := readRepositoryFile(t, "internal/dashboard/dashboard.go")
	console := readRepositoryFile(t, "internal/dashboard/console.go")

	registered := make([]string, 0, 8)
	for _, match := range handler.FindAllStringSubmatch(dashboard, -1) {
		registered = append(registered, match[1])
	}
	// The console's own route table: every entry is served by the catch-all at
	// the top level, so the suffix is the public path.
	for _, match := range routeSuffix.FindAllStringSubmatch(console, -1) {
		registered = append(registered, match[1])
	}
	// The old console, which answers under the legacy mount now.
	for _, match := range handler.FindAllStringSubmatch(
		legacyRouter.FindString(console), -1) {
		registered = append(registered, "/legacy"+match[1])
	}
	if len(registered) == 0 {
		t.Fatal("no shell route was found in internal/dashboard; the patterns above have drifted")
	}

	denial := regexp.MustCompile(`location (\^~|=) (\S+) \{ return 404; \}`)
	rules := denial.FindAllStringSubmatch(block, -1)
	for _, route := range registered {
		covered := false
		for _, rule := range rules {
			if (rule[1] == "^~" && strings.HasPrefix(route, rule[2])) ||
				(rule[1] == "=" && route == rule[2]) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("the public Dashboard host does not deny the WebShell route %q", route)
		}
	}
}

// TestPublicEdgeAndDashboardBothRefuseFraming: /device approves a pending CLI
// code with one click and mints a session carrying the approver's acr and amr,
// so a frameable page is a cross-site overlay away from an operator's identity.
// SameSite does not help — inside the frame the document's origin is the
// deployment itself. Asserted on both because the Dashboard is documented as
// reachable over plain HTTP on a restricted network, with no edge in front.
func TestPublicEdgeAndDashboardBothRefuseFraming(t *testing.T) {
	nginx := readRepositoryFile(t, "deploy/public-edge/nginx.conf.template")
	block := nginxServerBlock(t, nginx, "server_name ${PORTAGE_PUBLIC_HOST};")
	frameHeaders := []string{
		`add_header Content-Security-Policy "frame-ancestors 'none'" always;`,
		"add_header X-Frame-Options DENY always;",
	}

	// Scoped by indentation, not by "somewhere in the vhost". One add_header in a
	// location REPLACES the whole server-level set, so a location that carries
	// the headers proves nothing about the responses that do not go through it —
	// and a whole-block search reads one as if it covered the other.
	serverLevel, locations := nginxIndentScopes(block)
	for _, required := range frameHeaders {
		if !strings.Contains(serverLevel, "    "+required) {
			t.Errorf("the public Dashboard host does not declare %q at server level", required)
		}
	}
	for _, location := range locations {
		if !strings.Contains(location, "add_header ") {
			// Inherits the server-level set above.
			continue
		}
		for _, required := range frameHeaders {
			if !strings.Contains(location, "        "+required) {
				t.Errorf("a location writing its own headers does not repeat %q:\n%s",
					required, location)
			}
		}
	}

	dashboard := readRepositoryFile(t, "internal/dashboard/dashboard.go")
	for _, required := range []string{
		`header.Set("Content-Security-Policy", "frame-ancestors 'none'")`,
		`header.Set("X-Frame-Options", "DENY")`,
	} {
		if !strings.Contains(dashboard, required) {
			t.Errorf("the Dashboard itself is missing %q", required)
		}
	}
}

func TestPublicEdgeDeviceIAMUsesIdentityLimitAndNoStore(t *testing.T) {
	nginx := readRepositoryFile(t, "deploy/public-edge/nginx.conf.template")
	block := nginxLocationBlock(t, nginx, "location ^~ /api/v1/iam/device/ {")
	for _, required := range []string{
		"limit_req zone=public_requests burst=10 nodelay;",
		"limit_req zone=identity_requests burst=5 nodelay;",
		"add_header Cache-Control \"no-store\" always;",
		"proxy_pass http://portage_api_backend;",
	} {
		if !strings.Contains(block, required) {
			t.Errorf("device IAM edge location is missing %q", required)
		}
	}
}

// The device token endpoint is polled at the interval the server advertises, so
// its edge budget has to be derived from that interval rather than shared with
// the browser login that runs on the same source IP.
func TestPublicEdgeDeviceTokenPollingBudgetMatchesAdvertisedInterval(t *testing.T) {
	nginx := readRepositoryFile(t, "deploy/public-edge/nginx.conf.template")
	iam := readRepositoryFile(t, "internal/server/iam.go")
	interval := 0
	for _, line := range strings.Split(iam, "\n") {
		if !strings.Contains(line, "deviceAuthorizationInterval = ") {
			continue
		}
		if _, err := fmt.Sscanf(
			strings.TrimSpace(line), "deviceAuthorizationInterval = %d", &interval,
		); err != nil {
			t.Fatalf("cannot read the advertised device interval: %v", err)
		}
	}
	if interval <= 0 {
		t.Fatal("deviceAuthorizationInterval is not declared in internal/server/iam.go")
	}
	minimumRate := 60 / interval

	zone := regexp.MustCompile(
		`limit_req_zone \$binary_remote_addr zone=device_token_requests:\S+ rate=(\d+)r/m;`,
	).FindStringSubmatch(nginx)
	if zone == nil {
		t.Fatal("the device token endpoint has no dedicated per-minute rate zone")
	}
	rate, err := strconv.Atoi(zone[1])
	if err != nil {
		t.Fatal(err)
	}
	if rate < minimumRate {
		t.Errorf("device token zone rate=%dr/m, want at least %dr/m for a %ds poll interval",
			rate, minimumRate, interval)
	}

	block := nginxLocationBlock(t, nginx, "location = /api/v1/iam/device/token {")
	if !strings.Contains(block, "limit_req zone=device_token_requests burst=10 nodelay;") {
		t.Error("device token location does not use its dedicated budget")
	}
	if strings.Contains(block, "zone=identity_requests") {
		t.Error("device token location still shares the browser login identity budget")
	}
	// A default 503 is indistinguishable from an outage and carries no retry
	// semantics, so the identity gate's 429 assertion could never discriminate.
	for _, required := range []string{"limit_req_status 429;", "limit_conn_status 429;"} {
		if !strings.Contains(nginx, required) {
			t.Errorf("edge template is missing %q", required)
		}
	}
}

func TestPublicProviderTemplateHasNoEmbeddedSecret(t *testing.T) {
	var document struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(
		[]byte(readRepositoryFile(t, "configs/auth-providers.example.json")),
		&document,
	); err != nil {
		t.Fatal(err)
	}
	if len(document.Providers) != 3 {
		t.Fatalf("provider count=%d, want 3", len(document.Providers))
	}
	seen := map[string]bool{}
	for _, provider := range document.Providers {
		id, _ := provider["id"].(string)
		seen[id] = true
		if _, exists := provider["client_secret"]; exists {
			t.Errorf("provider %q embeds client_secret", id)
		}
		secretEnv, _ := provider["client_secret_env"].(string)
		redirect, _ := provider["redirect_url"].(string)
		if secretEnv == "" || !strings.HasPrefix(redirect, "https://") ||
			!strings.Contains(redirect, "/auth/provider/"+id+"/callback") {
			t.Errorf("provider %q is not production-Gate ready", id)
		}
	}
	for _, id := range []string{"authentik", "google", "github"} {
		if !seen[id] {
			t.Errorf("provider %q is missing", id)
		}
	}
}

func TestIdentitySchemaNeverUsesEmailAsMergeKey(t *testing.T) {
	migration := readRepositoryFile(t, "internal/migrations/sql/00009_iam_project_rbac.sql")
	repository := readRepositoryFile(t, "internal/persistence/iam.go")
	if !strings.Contains(migration, "UNIQUE (issuer, subject)") ||
		!strings.Contains(repository, "ON CONFLICT (issuer, subject)") {
		t.Fatal("durable identity is not keyed by issuer and subject")
	}
	normalized := strings.ToLower(strings.ReplaceAll(migration, " ", ""))
	if strings.Contains(normalized, "unique(email)") {
		t.Fatal("email can merge identities across providers")
	}
}

func TestIdentityGateWithoutCredentialsIsNotRun(t *testing.T) {
	output := filepath.Join(t.TempDir(), "identity-gate.json")
	command := exec.Command(
		"python3",
		filepath.Join(repositoryRoot(t), "scripts/public-identity-gate.py"),
		"--output",
		output,
	)
	command.Env = []string{"PATH=" + os.Getenv("PATH")}
	err := command.Run()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 3 {
		t.Fatalf("gate without credentials err=%v, want exit 3", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Status           string `json:"status"`
		CredentialsUsed  bool   `json:"credentials_used"`
		SecretsPersisted bool   `json:"secrets_persisted"`
		Checks           []struct {
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "not_run" || manifest.CredentialsUsed ||
		manifest.SecretsPersisted || len(manifest.Checks) != 20 {
		t.Fatalf("unexpected not-run manifest: %+v", manifest)
	}
	for _, check := range manifest.Checks {
		if check.Status != "not_run" {
			t.Fatalf("credential-free check was reported as %q", check.Status)
		}
	}
	for _, forbidden := range []string{
		"pe1_", "pe_session=", "access_token", "client_secret", "logout_token",
	} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("redacted manifest contains forbidden token marker %q", forbidden)
		}
	}
}
