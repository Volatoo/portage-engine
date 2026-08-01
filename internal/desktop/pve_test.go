package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func validPVEConfig(endpoint, artifacts string) *PVEConfig {
	return &PVEConfig{
		SchemaVersion:          1,
		Endpoint:               endpoint,
		InsecureSkipTLSVerify:  true,
		Node:                   "pve01",
		VMID:                   900,
		AllowedSnapshot:        "clean",
		StagingBinhost:         "http://10.31.0.2/portage-engine/staging/g1",
		StagingDigest:          "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		GuestAgentPath:         "/usr/libexec/portage-desktop-agent",
		ArtifactDirectory:      artifacts,
		LifecycleTimeoutSecond: 60,
	}
}

func validPVEV2Config(endpoint, artifacts string) *PVEConfig {
	config := validPVEConfig(endpoint, artifacts)
	config.SchemaVersion = 2
	config.ProfileID = "pe/amd64/no-multilib/systemd/desktop-verifier-v1"
	config.ImageID = "pe/amd64/no-multilib/desktop-verifier-matrix-g1"
	config.ImageGeneration = "desktop-matrix-g1"
	config.DisplayServer = "x11"
	config.StagingKeyPath = "signing-key.asc"
	config.StagingKeyFingerprint = strings.Repeat("A", 40)
	return config
}

func TestPVEConfigRejectsHTTPControlPlane(t *testing.T) {
	config := validPVEConfig("http://pve.internal", t.TempDir())
	if err := config.Validate(); err == nil {
		t.Fatal("HTTP PVE control plane was accepted")
	}
	config.Endpoint = "https://pve.internal/api2/json"
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPVEConfigAllowsImageOnlyScenarioWithoutStaging(t *testing.T) {
	config := validPVEConfig("https://pve.internal", t.TempDir())
	config.StagingBinhost = ""
	config.StagingDigest = ""
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	config.StagingDigest = "sha256:" + strings.Repeat("a", 64)
	if err := config.Validate(); err == nil {
		t.Fatal("partial staging policy was accepted")
	}
}

func TestPVEConfigV2RequiresSignedStagingAndRejectsWayland(t *testing.T) {
	config := validPVEV2Config("https://pve.internal", t.TempDir())
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	config.StagingKeyFingerprint = ""
	if err := config.Validate(); err == nil {
		t.Fatal("schema v2 accepted staging without a signer fingerprint")
	}
	config = validPVEV2Config("https://pve.internal", t.TempDir())
	config.DisplayServer = "wayland"
	if err := config.Validate(); err == nil {
		t.Fatal("direct PVE config accepted an unsupported native Wayland session")
	}
}

func TestPVEConfigBindsV2ScenarioIdentityAndSigner(t *testing.T) {
	scenario, err := LoadScenario(filepath.Join("..", "..", "tests", "desktop", "scenarios", "gtk-mousepad.json"))
	if err != nil {
		t.Fatal(err)
	}
	config := validPVEV2Config("https://pve.internal", t.TempDir())
	config.StagingDigest = scenario.Steps[2].Input["staging_digest"]
	config.StagingKeyFingerprint = scenario.Steps[2].Input["signer_fingerprint"]
	if err := config.ValidateScenario(scenario); err != nil {
		t.Fatal(err)
	}
	config.ImageGeneration = "another-generation"
	if err := config.ValidateScenario(scenario); err == nil {
		t.Fatal("direct PVE policy accepted scenario image-generation drift")
	}
	config = validPVEV2Config("https://pve.internal", t.TempDir())
	config.StagingDigest = scenario.Steps[2].Input["staging_digest"]
	config.StagingKeyFingerprint = strings.Repeat("B", 40)
	if err := config.ValidateScenario(scenario); err == nil {
		t.Fatal("direct PVE policy accepted scenario signer drift")
	}
}

func TestLoadPVEConfigRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pve.json")
	data := `{"schema_version":1,"endpoint":"https://pve.internal","insecure_skip_tls_verify":false,"node":"pve01","vmid":900,"allowed_snapshot":"clean","staging_binhost":"http://mirror/staging","staging_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","guest_agent_path":"/usr/libexec/portage-desktop-agent","artifact_directory":"/tmp/evidence","lifecycle_timeout_seconds":60,"password":"forbidden"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPVEConfig(path); err == nil {
		t.Fatal("unknown secret-shaped field was accepted")
	}
}

func TestPVEQGADriverPreparesDigestLockedStaging(t *testing.T) {
	var mutex sync.Mutex
	commands := make([][]string, 0, 2)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken=desktop@pve!e2e=secret" {
			t.Errorf("unexpected authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api2/json/nodes/pve01/qemu/900/status/current":
			_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
		case "/api2/json/nodes/pve01/qemu/900/agent/exec":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			mutex.Lock()
			commands = append(commands, append([]string(nil), r.Form["command"]...))
			pid := len(commands)
			mutex.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]int{"pid": pid}})
		case "/api2/json/nodes/pve01/qemu/900/agent/exec-status":
			_, _ = w.Write([]byte(`{"data":{"exited":1,"exitcode":0,"out-data":"","err-data":"","out-truncated":0,"err-truncated":0}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	config := validPVEConfig(server.URL, t.TempDir())
	driver, err := NewPVEQGADriver(config, "desktop@pve!e2e", "secret")
	if err != nil {
		t.Fatal(err)
	}
	observation, err := driver.Do(context.Background(), ActionRequest{ScenarioID: "desktop/test", StepID: "start", Action: "start"})
	if err != nil || observation.State != "passed" {
		t.Fatalf("observation = %+v, err = %v", observation, err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	wantReady := []string{config.GuestAgentPath, "desktop-ready"}
	wantPrepare := []string{config.GuestAgentPath, "prepare", config.StagingBinhost, config.StagingDigest}
	if len(commands) != 3 || !slices.Equal(commands[0], []string{"/usr/bin/true"}) || !slices.Equal(commands[1], wantReady) || !slices.Equal(commands[2], wantPrepare) {
		t.Fatalf("guest commands = %#v", commands)
	}
}

func TestPVEQGADriverV2VerifiesImageAndPreparesSignedStaging(t *testing.T) {
	var mutex sync.Mutex
	commands := make([][]string, 0, 4)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api2/json/nodes/pve01/qemu/900/status/current":
			_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
		case "/api2/json/nodes/pve01/qemu/900/agent/exec":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			mutex.Lock()
			commands = append(commands, append([]string(nil), r.Form["command"]...))
			pid := len(commands)
			mutex.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]int{"pid": pid}})
		case "/api2/json/nodes/pve01/qemu/900/agent/exec-status":
			_, _ = w.Write([]byte(`{"data":{"exited":1,"exitcode":0,"out-data":"","err-data":"","out-truncated":0,"err-truncated":0}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	config := validPVEV2Config(server.URL, t.TempDir())
	driver, err := NewPVEQGADriver(config, "desktop@pve!e2e", "secret")
	if err != nil {
		t.Fatal(err)
	}
	request := ActionRequest{
		ScenarioID: "desktop/test-v2", ProfileID: config.ProfileID, ImageID: config.ImageID,
		ImageGeneration: config.ImageGeneration, DisplayServer: config.DisplayServer, Action: "start",
	}
	if _, err := driver.Do(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	want := [][]string{
		{"/usr/bin/true"},
		{config.GuestAgentPath, "assert-image", config.ProfileID, config.ImageID, config.ImageGeneration, config.DisplayServer},
		{config.GuestAgentPath, "desktop-ready"},
		{config.GuestAgentPath, "prepare", config.StagingBinhost, config.StagingDigest, config.StagingKeyPath, config.StagingKeyFingerprint},
	}
	if !slices.EqualFunc(commands, want, slices.Equal) {
		t.Fatalf("guest commands = %#v, want %#v", commands, want)
	}
}

func TestPVEQGADriverV2RejectsActionIdentityDriftBeforeExecution(t *testing.T) {
	config := validPVEV2Config("https://pve.internal", t.TempDir())
	driver, err := NewPVEQGADriver(config, "desktop@pve!e2e", "secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = driver.Do(context.Background(), ActionRequest{
		ProfileID: config.ProfileID, ImageID: config.ImageID, ImageGeneration: "wrong", DisplayServer: config.DisplayServer, Action: "launch_fixture",
	})
	if err == nil {
		t.Fatal("direct PVE driver accepted action image-generation drift")
	}
}

func TestPVEQGADriverStartsImageOnlyScenarioWithoutStaging(t *testing.T) {
	var mutex sync.Mutex
	commands := make([][]string, 0, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api2/json/nodes/pve01/qemu/900/status/current":
			_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
		case "/api2/json/nodes/pve01/qemu/900/agent/exec":
			_ = r.ParseForm()
			mutex.Lock()
			commands = append(commands, append([]string(nil), r.Form["command"]...))
			mutex.Unlock()
			_, _ = w.Write([]byte(`{"data":{"pid":1}}`))
		case "/api2/json/nodes/pve01/qemu/900/agent/exec-status":
			_, _ = w.Write([]byte(`{"data":{"exited":1,"exitcode":0,"out-data":"","err-data":"","out-truncated":0,"err-truncated":0}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	config := validPVEConfig(server.URL, t.TempDir())
	config.StagingBinhost = ""
	config.StagingDigest = ""
	driver, err := NewPVEQGADriver(config, "desktop@pve!e2e", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Do(context.Background(), ActionRequest{Action: "start"}); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(commands) != 2 || !slices.Equal(commands[0], []string{"/usr/bin/true"}) || !slices.Equal(commands[1], []string{config.GuestAgentPath, "desktop-ready"}) {
		t.Fatalf("image-only start executed unexpected commands: %#v", commands)
	}
}

func TestPVEQGADriverRejectsScenarioDigestDriftBeforeExecution(t *testing.T) {
	config := validPVEConfig("https://pve.internal", t.TempDir())
	driver, err := NewPVEQGADriver(config, "desktop@pve!e2e", "secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = driver.Do(context.Background(), ActionRequest{Action: "install", Input: map[string]string{
		"atom": "app-editors/gvim", "staging_digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}})
	if err == nil {
		t.Fatal("scenario staging digest drift was accepted")
	}
}

func TestPVEQGADriverReadsDigestVerifiedEvidenceInChunks(t *testing.T) {
	payload := make([]byte, guestEvidenceChunk*2+17)
	for index := range payload {
		payload[index] = byte(index % 251)
	}
	digest := sha256.Sum256(payload)
	outputs := map[int]string{}
	var mutex sync.Mutex
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api2/json/nodes/pve01/qemu/900/agent/exec":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			command := r.Form["command"]
			var output string
			switch {
			case len(command) == 3 && command[1] == "evidence-info":
				output = fmt.Sprintf(`{"size":%d,"sha256":"sha256:%s"}`, len(payload), hex.EncodeToString(digest[:]))
			case len(command) == 5 && command[1] == "read-evidence":
				offset, _ := strconv.Atoi(command[3])
				limit, _ := strconv.Atoi(command[4])
				output = base64.StdEncoding.EncodeToString(payload[offset : offset+limit])
			default:
				t.Fatalf("unexpected guest command: %#v", command)
			}
			mutex.Lock()
			pid := len(outputs) + 1
			outputs[pid] = output
			mutex.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]int{"pid": pid}})
		case "/api2/json/nodes/pve01/qemu/900/agent/exec-status":
			pid, _ := strconv.Atoi(r.URL.Query().Get("pid"))
			mutex.Lock()
			output := outputs[pid]
			mutex.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"exited": 1, "exitcode": 0, "out-data": output, "err-data": "", "out-truncated": 0, "err-truncated": 0}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	driver, err := NewPVEQGADriver(validPVEConfig(server.URL, t.TempDir()), "desktop@pve!e2e", "secret")
	if err != nil {
		t.Fatal(err)
	}
	data, err := driver.guestFileRead(context.Background(), "/run/portage-engine/desktop-evidence/test.log")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(data, payload) {
		t.Fatal("chunked guest evidence changed during transfer")
	}
}
