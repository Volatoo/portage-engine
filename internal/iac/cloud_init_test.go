package iac

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultCloudInitConfigIsNativeReady(t *testing.T) {
	cfg := DefaultCloudInitConfig()
	if cfg.BuilderPort != 9090 || cfg.Architecture != "amd64" {
		t.Fatalf("unexpected native defaults: %+v", cfg)
	}
	if cfg.DataDir == "" || cfg.WorkDir == "" || cfg.ArtifactDir == "" {
		t.Fatalf("native directories are incomplete: %+v", cfg)
	}
}

func TestGenerateCloudInitScriptAlwaysUsesNativeGentoo(t *testing.T) {
	for _, cfg := range []*CloudInitConfig{
		nil,
		{BuilderPort: 9090, Architecture: "amd64"},
	} {
		script := GenerateCloudInitScript(cfg)
		for _, required := range []string{
			"#!/bin/bash",
			"NATIVE_JOB_POLICY=single-use",
		} {
			if !strings.Contains(script, required) {
				t.Fatalf("native bootstrap missing %q", required)
			}
		}
		for _, forbidden := range []string{"docker pull", "docker run", "USE_DOCKER=true"} {
			if strings.Contains(script, forbidden) {
				t.Fatalf("removed Docker backend leaked into bootstrap: %q", forbidden)
			}
		}
	}
}

func TestGenerateCloudInitScriptCarriesNativeInputs(t *testing.T) {
	cfg := DefaultCloudInitConfig()
	cfg.PortageMirror = "http://mirror.invalid/gentoo"
	cfg.PortageSyncURI = "rsync://mirror.invalid/gentoo-portage"
	cfg.PortageBinpkgHost = "http://binhost.invalid/binpkgs"
	cfg.ServerCallbackURL = "http://control.invalid"
	cfg.BuilderToken = "builder-token"
	cfg.BuilderBinaryURL = "http://nas.invalid/portage-builder"
	cfg.BuilderBinarySHA256 = strings.Repeat("a", 64)

	script := GenerateCloudInitScript(cfg)
	for _, value := range []string{
		cfg.PortageMirror,
		cfg.PortageSyncURI,
		cfg.PortageBinpkgHost,
		cfg.ServerCallbackURL,
		cfg.BuilderBinaryURL,
		cfg.BuilderBinarySHA256,
	} {
		if !strings.Contains(script, value) {
			t.Fatalf("native bootstrap dropped %q", value)
		}
	}
}

func TestGenerateUserDataAndStartupScript(t *testing.T) {
	cfg := DefaultCloudInitConfig()
	userData := GenerateUserData(cfg)
	if !strings.HasPrefix(userData, "#cloud-config") || !strings.Contains(userData, "runcmd:") {
		t.Fatalf("invalid cloud-config wrapper: %q", userData[:min(len(userData), 80)])
	}
	if startup := GenerateStartupScript(cfg); !strings.HasPrefix(startup, "#!/bin/bash") {
		t.Fatal("startup script is not a shell script")
	}
}

func TestIndentScript(t *testing.T) {
	if got := indentScript("line1\nline2", "  "); got != "  line1\n  line2" {
		t.Fatalf("indentScript = %q", got)
	}
}

func TestCloudInitConfigJSONCompatibility(t *testing.T) {
	cfg := DefaultCloudInitConfig()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CloudInitConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.BuilderPort != cfg.BuilderPort || decoded.Architecture != cfg.Architecture {
		t.Fatalf("JSON round trip changed native config: %+v", decoded)
	}
}

func TestGeneratedNativeScriptIsValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	path := filepath.Join(t.TempDir(), "deploy.sh")
	if err := os.WriteFile(path, []byte(GenerateCloudInitScript(nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("native bootstrap is invalid bash: %v\n%s", err, out)
	}
}
