package tests

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
		"proxy_cookie_flags ~ secure httponly samesite=lax;",
		"location ^~ /shell/ { return 404; }",
		"location = /api/shell { return 404; }",
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
