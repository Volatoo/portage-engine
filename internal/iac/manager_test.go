package iac

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNewManager tests creating a new IaC manager.
func TestNewManager(t *testing.T) {
	manager := NewManager()
	if manager == nil {
		t.Fatal("NewManager returned nil")
	}

	if manager.instances == nil {
		t.Error("instances map not initialized")
	}

	if manager.workspaceDir == "" {
		t.Error("workspaceDir should not be empty")
	}
}

func TestWithWorkspaceDirUsesDurableStateLocation(t *testing.T) {
	want := filepath.Join(t.TempDir(), "terraform-workspaces")
	manager := NewManager(WithWorkspaceDir(want))
	if manager.workspaceDir != want {
		t.Fatalf("workspaceDir = %q, want %q", manager.workspaceDir, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("durable workspace was not created: info=%v err=%v", info, err)
	}
}

func TestGetOrDefault(t *testing.T) {
	tests := []struct {
		name         string
		m            map[string]string
		key          string
		defaultValue string
		want         string
	}{
		{
			name:         "key exists",
			m:            map[string]string{"region": "us-west-1"},
			key:          "region",
			defaultValue: "us-east-1",
			want:         "us-west-1",
		},
		{
			name:         "key missing",
			m:            map[string]string{},
			key:          "region",
			defaultValue: "us-east-1",
			want:         "us-east-1",
		},
		{
			name:         "nil map",
			m:            nil,
			key:          "region",
			defaultValue: "us-east-1",
			want:         "us-east-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getOrDefault(tt.m, tt.key, tt.defaultValue)
			if got != tt.want {
				t.Errorf("getOrDefault() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpdateHeartbeat(t *testing.T) {
	manager := NewManager()

	// Add a test instance
	instanceID := "test-instance"
	manager.instances[instanceID] = &Instance{
		ID:            instanceID,
		Provider:      "gcp",
		Status:        "provisioning",
		LastHeartbeat: time.Now().Add(-10 * time.Minute),
	}

	// Update heartbeat
	err := manager.UpdateHeartbeat(instanceID)
	if err != nil {
		t.Errorf("UpdateHeartbeat() error = %v", err)
	}

	// Verify heartbeat was updated
	instance := manager.instances[instanceID]
	if time.Since(instance.LastHeartbeat) > 5*time.Second {
		t.Error("LastHeartbeat not updated")
	}
	if instance.Status != "running" {
		t.Errorf("Status = %v, want running", instance.Status)
	}

	// Test non-existent instance
	err = manager.UpdateHeartbeat("non-existent")
	if err == nil {
		t.Error("UpdateHeartbeat() expected error for non-existent instance")
	}
}

func TestCheckStaleInstances(t *testing.T) {
	manager := NewManager()

	// Add test instances
	manager.instances["fresh"] = &Instance{
		ID:            "fresh",
		Provider:      "gcp",
		Status:        "running",
		LastHeartbeat: time.Now(),
	}
	manager.instances["stale"] = &Instance{
		ID:            "stale",
		Provider:      "gcp",
		Status:        "running",
		LastHeartbeat: time.Now().Add(-15 * time.Minute),
	}
	manager.instances["already-error"] = &Instance{
		ID:            "already-error",
		Provider:      "gcp",
		Status:        "error",
		LastHeartbeat: time.Now().Add(-20 * time.Minute),
	}

	// Check stale instances - should return all instances with LastHeartbeat > timeout
	stale := manager.CheckStaleInstances(10 * time.Minute)
	if len(stale) != 2 {
		t.Errorf("CheckStaleInstances() returned %d instances, want 2", len(stale))
	}

	// Verify the returned instances are the stale ones
	staleIDs := make(map[string]bool)
	for _, inst := range stale {
		staleIDs[inst.ID] = true
	}
	if !staleIDs["stale"] || !staleIDs["already-error"] {
		t.Error("CheckStaleInstances() did not return expected stale instances")
	}
}

func TestListInstances(t *testing.T) {
	manager := NewManager()

	// Test empty list
	instances := manager.ListInstances()
	if len(instances) != 0 {
		t.Errorf("ListInstances() returned %d instances, want 0", len(instances))
	}

	// Add test instances
	manager.instances["test1"] = &Instance{
		ID:       "test1",
		Provider: "gcp",
		Status:   "running",
	}
	manager.instances["test2"] = &Instance{
		ID:       "test2",
		Provider: "aws",
		Status:   "provisioning",
	}

	instances = manager.ListInstances()
	if len(instances) != 2 {
		t.Errorf("ListInstances() returned %d instances, want 2", len(instances))
	}
}

func TestGenerateFirewallConfig(t *testing.T) {
	manager := NewManager()

	tests := []struct {
		name string
		req  *ProvisionRequest
	}{
		{
			name: "aliyun firewall",
			req: &ProvisionRequest{
				Provider:        "aliyun",
				AllowedIPRanges: []string{"0.0.0.0/0"},
				BuilderPort:     8080,
			},
		},
		{
			name: "gcp firewall",
			req: &ProvisionRequest{
				Provider:        "gcp",
				AllowedIPRanges: []string{"0.0.0.0/0"},
				BuilderPort:     8080,
			},
		},
		{
			name: "aws firewall",
			req: &ProvisionRequest{
				Provider:        "aws",
				AllowedIPRanges: []string{"0.0.0.0/0"},
				BuilderPort:     8080,
			},
		},
		{
			name: "no ip ranges",
			req: &ProvisionRequest{
				Provider:    "gcp",
				BuilderPort: 8080,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := manager.generateFirewallConfig(tt.req)
			if len(tt.req.AllowedIPRanges) > 0 && len(config) == 0 {
				t.Error("generateFirewallConfig() returned empty config when firewall rules configured")
			}
		})
	}
}

func TestGenerateTerraformConfig(t *testing.T) {
	manager := NewManager()

	tests := []struct {
		name      string
		req       *ProvisionRequest
		wantEmpty bool
	}{
		{
			name: "aliyun config",
			req: &ProvisionRequest{
				Arch:     "amd64",
				Provider: "aliyun",
				Spec: map[string]string{
					"region": "cn-hangzhou",
				},
				Credentials: &CloudCredentials{
					AliyunAccessKey: "test-ak",
					AliyunSecretKey: "test-sk",
				},
			},
			wantEmpty: false,
		},
		{
			name: "gcp config",
			req: &ProvisionRequest{
				Arch:     "amd64",
				Provider: "gcp",
				Spec: map[string]string{
					"region":  "us-central1",
					"project": "test-project",
				},
				Credentials: &CloudCredentials{
					GCPKeyFile: "/path/to/key.json",
				},
			},
			wantEmpty: false,
		},
		{
			name: "aws config",
			req: &ProvisionRequest{
				Arch:     "arm64",
				Provider: "aws",
				Spec: map[string]string{
					"region": "us-west-2",
				},
				Credentials: &CloudCredentials{
					AWSAccessKey: "test-access",
					AWSSecretKey: "test-secret",
				},
			},
			wantEmpty: false,
		},
		{
			name: "invalid provider",
			req: &ProvisionRequest{
				Provider: "invalid",
			},
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := manager.generateTerraformConfig(tt.req)
			if !tt.wantEmpty {
				if err != nil {
					t.Fatalf("generateTerraformConfig() error: %v", err)
				}
				if len(config) == 0 {
					t.Error("generateTerraformConfig() returned empty config")
				}
			}
			if tt.wantEmpty && err == nil {
				t.Error("generateTerraformConfig() should return an error for invalid provider")
			}
		})
	}
}

func TestGenerateDeploymentScript(t *testing.T) {
	manager := NewManager()

	script := manager.generateDeploymentScript(&ProvisionRequest{
		ServerCallback: "http://localhost:8080",
		BuilderPort:    9090,
		BuilderToken:   "secret-token",
		Arch:           "amd64",
		BinpkgHost:     "http://localhost:8080/binpkgs",
	})

	if len(script) == 0 {
		t.Fatal("generateDeploymentScript() returned empty script")
	}

	// The builder config directory must be created BEFORE it is written to
	// (the script runs under set -e).
	mkdirIdx := strings.Index(script, "mkdir -p /etc/portage-engine")
	writeIdx := strings.Index(script, "cat > /etc/portage-engine/builder.conf")
	if mkdirIdx < 0 || writeIdx < 0 || mkdirIdx > writeIdx {
		t.Error("mkdir /etc/portage-engine must precede the builder.conf write")
	}

	// The token must be wired into the builder config.
	if !strings.Contains(script, "BUILDER_TOKEN=") {
		t.Error("BUILDER_TOKEN not present in generated config")
	}

	// The Portage paths must point the builder at the synced tree / make.conf.
	for _, want := range []string{"PORTAGE_REPOS_PATH=", "PORTAGE_CONF_PATH=", "MAKE_CONF_PATH="} {
		if !strings.Contains(script, want) {
			t.Errorf("generated config missing %q", want)
		}
	}

	// A binhost farm must never compile for the exact CPU.
	if strings.Contains(script, "-march=native") {
		t.Error("make.conf must not use -march=native on a binhost farm")
	}

	// $(nproc) must be resolved by the shell, not written literally into
	// make.conf (Portage does not evaluate command substitution).
	if strings.Contains(script, `MAKEOPTS="-j$(nproc)"`) {
		t.Error("$(nproc) leaked literally into make.conf")
	}
}

func TestPrepareEnvironment(t *testing.T) {
	tests := []struct {
		name    string
		req     *ProvisionRequest
		wantEnv []string
	}{
		{
			name: "aliyun credentials",
			req: &ProvisionRequest{
				Provider: "aliyun",
				Credentials: &CloudCredentials{
					AliyunAccessKey: "test-ak",
					AliyunSecretKey: "test-sk",
				},
			},
			wantEnv: []string{
				"ALICLOUD_ACCESS_KEY=test-ak",
				"ALICLOUD_SECRET_KEY=test-sk",
			},
		},
		{
			name: "gcp credentials",
			req: &ProvisionRequest{
				Provider: "gcp",
				Credentials: &CloudCredentials{
					GCPKeyFile: "/path/to/key.json",
				},
			},
			wantEnv: []string{
				"GOOGLE_APPLICATION_CREDENTIALS=/path/to/key.json",
			},
		},
		{
			name: "aws credentials",
			req: &ProvisionRequest{
				Provider: "aws",
				Credentials: &CloudCredentials{
					AWSAccessKey: "test-access",
					AWSSecretKey: "test-secret",
				},
			},
			wantEnv: []string{
				"AWS_ACCESS_KEY_ID=test-access",
				"AWS_SECRET_ACCESS_KEY=test-secret",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewManager()

			gotEnv := manager.prepareEnvironment(tt.req)
			if len(gotEnv) != len(tt.wantEnv) {
				t.Errorf("prepareEnvironment() returned %d env vars, want %d", len(gotEnv), len(tt.wantEnv))
			}

			// Convert to map for easier comparison
			envMap := make(map[string]bool)
			for _, e := range gotEnv {
				envMap[e] = true
			}

			for _, want := range tt.wantEnv {
				if !envMap[want] {
					t.Errorf("prepareEnvironment() missing env var %s", want)
				}
			}
		})
	}
}

func TestProvisionWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	manager := NewManager()

	req := &ProvisionRequest{
		Arch:     "amd64",
		Provider: "gcp",
		Spec: map[string]string{
			"region":  "us-central1",
			"project": "test-project",
		},
		Credentials: &CloudCredentials{
			GCPKeyFile: "/path/to/key.json",
		},
		SSH: &SSHConfig{
			KeyPath: "/path/to/ssh/key",
			User:    "root",
		},
		ServerCallback: "http://localhost:8080/api/heartbeat",
	}

	// Test config generation
	config, err := manager.generateTerraformConfig(req)
	if err != nil {
		t.Fatalf("generateTerraformConfig() error: %v", err)
	}
	if len(config) == 0 {
		t.Error("Generated config is empty")
	}

	// Verify workspace directory creation
	workspaceDir := manager.workspaceDir
	configPath := filepath.Join(workspaceDir, "test-workspace", "main.tf")

	// Create the config file to simulate what Provision() does
	testWorkspace := filepath.Join(workspaceDir, "test-workspace")
	if err := os.MkdirAll(testWorkspace, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	defer func() { _ = os.RemoveAll(testWorkspace) }()
	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("main.tf was not created")
	}
}

func TestInstanceLifecycle(t *testing.T) {
	manager := NewManager()

	instanceID := "test-instance-lifecycle"

	// Create instance
	manager.instances[instanceID] = &Instance{
		ID:              instanceID,
		Provider:        "gcp",
		Arch:            "amd64",
		Status:          "provisioning",
		LastHeartbeat:   time.Now(),
		IPAddress:       "192.168.1.100",
		BuilderEndpoint: "http://192.168.1.100:8080",
	}

	// Verify instance exists
	instances := manager.ListInstances()
	found := false
	for _, inst := range instances {
		if inst.ID == instanceID {
			found = true
			break
		}
	}
	if !found {
		t.Error("Instance not found in list")
	}

	// Update heartbeat
	if err := manager.UpdateHeartbeat(instanceID); err != nil {
		t.Errorf("UpdateHeartbeat() error = %v", err)
	}

	// Check instance is not stale
	stale := manager.CheckStaleInstances(10 * time.Minute)
	for _, inst := range stale {
		if inst.ID == instanceID {
			t.Error("Instance should not be stale")
		}
	}
}

func TestManagerWithTTL(t *testing.T) {
	ttl := 30 * time.Minute
	manager := NewManager(WithDefaultTTL(ttl))

	if manager.defaultTTL != ttl {
		t.Errorf("defaultTTL = %v, want %v", manager.defaultTTL, ttl)
	}
}

func TestManagerWithCleanupInterval(t *testing.T) {
	interval := 10 * time.Minute
	manager := NewManager(WithCleanupInterval(interval))

	if manager.cleanupInterval != interval {
		t.Errorf("cleanupInterval = %v, want %v", manager.cleanupInterval, interval)
	}
}

func TestUpdateInstanceActivity(t *testing.T) {
	manager := NewManager()

	instanceID := "test-activity-instance"
	oldTime := time.Now().Add(-1 * time.Hour)

	manager.instances[instanceID] = &Instance{
		ID:           instanceID,
		Provider:     "gcp",
		Status:       "running",
		LastActivity: oldTime,
	}

	manager.UpdateInstanceActivity(instanceID)

	inst := manager.instances[instanceID]
	if inst.LastActivity.Before(oldTime) || inst.LastActivity.Equal(oldTime) {
		t.Error("LastActivity was not updated")
	}
}

func TestSetInstanceActiveTasks(t *testing.T) {
	manager := NewManager()

	instanceID := "test-tasks-instance"
	manager.instances[instanceID] = &Instance{
		ID:          instanceID,
		Provider:    "gcp",
		Status:      "running",
		ActiveTasks: 0,
	}

	manager.SetInstanceActiveTasks(instanceID, 5)

	inst := manager.instances[instanceID]
	if inst.ActiveTasks != 5 {
		t.Errorf("ActiveTasks = %d, want 5", inst.ActiveTasks)
	}
}

func TestGetExpiredInstances(t *testing.T) {
	manager := NewManager()

	// Instance with no TTL (no auto-termination)
	manager.instances["no-ttl"] = &Instance{
		ID:           "no-ttl",
		Provider:     "gcp",
		Status:       "running",
		TTL:          0,
		LastActivity: time.Now().Add(-2 * time.Hour),
		ActiveTasks:  0,
	}

	// Instance with TTL but has active tasks
	manager.instances["active-tasks"] = &Instance{
		ID:           "active-tasks",
		Provider:     "gcp",
		Status:       "running",
		TTL:          30 * time.Minute,
		LastActivity: time.Now().Add(-1 * time.Hour),
		ActiveTasks:  1,
	}

	// Expired instance
	manager.instances["expired"] = &Instance{
		ID:           "expired",
		Provider:     "gcp",
		Status:       "running",
		TTL:          30 * time.Minute,
		LastActivity: time.Now().Add(-1 * time.Hour),
		ActiveTasks:  0,
	}

	// Not yet expired instance
	manager.instances["not-expired"] = &Instance{
		ID:           "not-expired",
		Provider:     "gcp",
		Status:       "running",
		TTL:          2 * time.Hour,
		LastActivity: time.Now().Add(-1 * time.Hour),
		ActiveTasks:  0,
	}

	expired := manager.GetExpiredInstances()

	if len(expired) != 1 {
		t.Errorf("Expected 1 expired instance, got %d", len(expired))
	}

	if len(expired) > 0 && expired[0].ID != "expired" {
		t.Errorf("Expected expired instance ID 'expired', got '%s'", expired[0].ID)
	}
}

func TestInstanceTTLFields(t *testing.T) {
	now := time.Now()
	inst := &Instance{
		ID:           "test-ttl",
		Provider:     "gcp",
		Status:       "running",
		CreatedAt:    now,
		TTL:          60 * time.Minute,
		LastActivity: now,
		ActiveTasks:  0,
	}

	if inst.TTL != 60*time.Minute {
		t.Errorf("TTL = %v, want 60m", inst.TTL)
	}
	if inst.CreatedAt != now {
		t.Errorf("CreatedAt mismatch")
	}
	if inst.ActiveTasks != 0 {
		t.Errorf("ActiveTasks = %d, want 0", inst.ActiveTasks)
	}
}

// TestTerminateKeepsInstanceOnDestroyFailure verifies that a failed destroy does
// NOT drop the instance from tracking (so the cleanup routine can retry it,
// instead of leaking a billed VM with its terraform state deleted).
func TestTerminateKeepsInstanceOnDestroyFailure(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "instances.json")
	m := NewManager(WithStateFile(stateFile))
	id := "leak-test"
	// A non-existent TerraformDir makes `terraform destroy` fail to start
	// (chdir error), deterministically, whether or not terraform is installed.
	m.instances[id] = &Instance{
		ID:           id,
		Provider:     "gcp",
		Status:       "running",
		TerraformDir: "/nonexistent/terraform/dir/for/test",
	}

	err := m.Terminate(id)
	if err == nil {
		t.Fatal("expected Terminate to return an error on destroy failure")
	}

	// Instance must still be tracked and marked destroy_failed.
	inst, exists := m.instances[id]
	if !exists {
		t.Fatal("instance was dropped from tracking after a failed destroy (would leak the VM)")
	}
	if inst.Status != "destroy_failed" {
		t.Errorf("expected status destroy_failed, got %q", inst.Status)
	}
	data, readErr := os.ReadFile(stateFile)
	if readErr != nil || !strings.Contains(string(data), `"status": "destroy_failed"`) {
		t.Fatalf("destroy failure was not persisted for restart retry: data=%s err=%v", data, readErr)
	}
}

func TestTerminatePersistsSuccessfulRemoval(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeTerraform := filepath.Join(binDir, "terraform")
	if err := os.WriteFile(fakeTerraform, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stateFile := filepath.Join(root, "instances.json")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	m := NewManager(WithStateFile(stateFile))
	m.instances["done"] = &Instance{ID: "done", Status: "running", TerraformDir: workspace}
	m.persistInstances()
	if err := m.Terminate("done"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("terminated instance remained in persisted state: %s", data)
	}
}

func TestTerminateRetainsWorkspaceUntilDurableCleanupAcknowledgement(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "terraform"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stateFile := filepath.Join(root, "instances.json")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	m := NewManager(WithStateFile(stateFile))
	m.instances["needs-ack"] = &Instance{
		ID: "needs-ack", Provider: "pve", Status: "running", TerraformDir: workspace,
		lifecycle: func(_ *Instance, state, _ string, _ *time.Time) error {
			if state == "destroyed" {
				return os.ErrPermission
			}
			return nil
		},
	}
	if err := m.Terminate("needs-ack"); err == nil {
		t.Fatal("expected durable acknowledgement failure")
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("workspace removed before acknowledgement: %v", err)
	}
	if inst, ok := m.instances["needs-ack"]; !ok || inst.Status != "destroy_failed" {
		t.Fatalf("instance not retained for reconciliation: %+v", inst)
	}
}

func TestDestroyRecordedAllowsReplicaCleanupInsideSharedWorkspace(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	commandLog := filepath.Join(root, "terraform-commands.log")
	t.Setenv("TF_TEST_LOG", commandLog)
	fakeTerraform := filepath.Join(binDir, "terraform")
	if err := os.WriteFile(fakeTerraform, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TF_TEST_LOG\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workspaceRoot := filepath.Join(root, "shared-workspaces")
	workspace := filepath.Join(workspaceRoot, "pve-old-attempt")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	replica := NewManager(WithWorkspaceDir(workspaceRoot))
	if err := replica.DestroyRecorded("pve", "pve-old-attempt", workspace); err != nil {
		t.Fatalf("DestroyRecorded: %v", err)
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(commands)), "\n")
	if len(lines) != 2 ||
		!strings.HasPrefix(lines[0], "init -reconfigure -input=false") ||
		!strings.HasPrefix(lines[1], "destroy -auto-approve") {
		t.Fatalf("terraform commands = %q", lines)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("workspace was removed before durable acknowledgement: %v", err)
	}
	if err := replica.RemoveRecordedWorkspace(workspace); err != nil {
		t.Fatalf("RemoveRecordedWorkspace: %v", err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("destroyed workspace still exists: %v", err)
	}

	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := replica.DestroyRecorded("pve", "escape", outside); err == nil {
		t.Fatal("expected workspace containment rejection")
	}
}

func TestPersistInstancesDoesNotWriteDestroyCredentials(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "instances.json")
	m := NewManager(WithStateFile(stateFile))
	m.instances["safe-state"] = &Instance{
		ID:           "safe-state",
		Provider:     "pve",
		Status:       "running",
		TerraformDir: "/var/lib/portage-engine/terraform/safe-state",
		destroyEnv: []string{
			"PM_USER=terraform@example",
			"PM_PASS=must-not-reach-disk",
			"TF_VAR_pve_password=must-not-reach-disk",
		},
	}
	m.persistInstances()

	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "must-not-reach-disk") ||
		strings.Contains(string(data), `"destroy_env"`) {
		t.Fatalf("instance state persisted destroy credentials: %s", data)
	}
	if info, err := os.Stat(stateFile); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
}

func TestLoadInstancesScrubsLegacyDestroyCredentials(t *testing.T) {
	root := t.TempDir()
	stateFile := filepath.Join(root, "instances.json")
	legacy := `[
	  {
	    "id": "legacy",
	    "provider": "pve",
	    "status": "running",
	    "terraform_dir": "/var/lib/portage-engine/terraform/legacy",
	    "destroy_env": ["PM_PASS=legacy-secret"]
	  }
	]`
	if err := os.WriteFile(stateFile, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(WithStateFile(stateFile))
	if _, ok := m.instances["legacy"]; !ok {
		t.Fatal("legacy instance was not restored")
	}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "legacy-secret") ||
		strings.Contains(string(data), `"destroy_env"`) {
		t.Fatalf("legacy credential material was not scrubbed: %s", data)
	}
}

func TestTerminateRestoredInstanceUsesCredentialResolver(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeTerraform := filepath.Join(binDir, "terraform")
	script := "#!/bin/sh\n" +
		"test \"$PM_USER\" = \"terraform@example\"\n" +
		"test \"$PM_PASS\" = \"resolved-at-runtime\"\n" +
		"test \"$TF_VAR_pve_password\" = \"resolved-at-runtime\"\n"
	if err := os.WriteFile(fakeTerraform, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stateFile := filepath.Join(root, "instances.json")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	first := NewManager(WithStateFile(stateFile))
	first.instances["restored"] = &Instance{
		ID: "restored", Provider: "pve", Status: "running",
		TerraformDir: workspace,
	}
	first.persistInstances()

	restored := NewManager(WithStateFile(stateFile))
	restored.SetCredentialResolver(func(provider string) *CloudCredentials {
		if provider != "pve" {
			t.Fatalf("resolver provider = %q, want pve", provider)
		}
		return &CloudCredentials{
			PVEUsername: "terraform@example",
			PVEPassword: "resolved-at-runtime",
		}
	})
	if err := restored.Terminate("restored"); err != nil {
		t.Fatalf("Terminate restored instance: %v", err)
	}
}

// TestCleanupRetriesDestroyFailedInstances verifies that destroy_failed
// instances are selected for cleanup regardless of TTL.
func TestCleanupRetriesDestroyFailedInstances(t *testing.T) {
	m := NewManager()
	id := "retry-test"
	m.instances[id] = &Instance{
		ID:           id,
		Provider:     "gcp",
		Status:       "destroy_failed",
		TTL:          0, // even with no TTL, destroy_failed must be retried
		TerraformDir: "/nonexistent/terraform/dir/for/test",
		LastActivity: time.Now(),
	}

	// cleanupExpiredInstances calls Terminate, which will fail again (dir gone)
	// and keep it tracked — the point is that it was *selected*, proving the
	// retry path targets destroy_failed instances.
	m.cleanupExpiredInstances()

	inst, exists := m.instances[id]
	if !exists {
		t.Fatal("destroy_failed instance disappeared unexpectedly")
	}
	if inst.Status != "destroy_failed" {
		t.Errorf("expected it to remain destroy_failed after a failed retry, got %q", inst.Status)
	}
}

func TestReconcileLoadedInstancesReclaimsSingleUseVMs(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	fakeTerraform := filepath.Join(binDir, "terraform")
	if err := os.WriteFile(fakeTerraform, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	terraformDir := filepath.Join(root, "restored-instance")
	if err := os.MkdirAll(terraformDir, 0o750); err != nil {
		t.Fatal(err)
	}
	m := NewManager(WithWorkspaceDir(filepath.Join(root, "workspaces")))
	m.instances["restored-running"] = &Instance{
		ID:              "restored-running",
		Provider:        "pve",
		Status:          "running",
		BuilderEndpoint: "http://127.0.0.1:9090",
		TerraformDir:    terraformDir,
	}

	m.reconcileLoadedInstances()

	if _, exists := m.instances["restored-running"]; exists {
		t.Fatal("healthy restored native VM was retained for reuse")
	}
	if _, err := os.Stat(terraformDir); !os.IsNotExist(err) {
		t.Fatalf("restored VM workspace was not removed after destroy: %v", err)
	}
}

// TestGenerateAliyunFirewall_PerCIDRRules verifies finding #28: the Aliyun
// firewall must emit one valid security-group-rule resource per allowed CIDR
// instead of a single rule with an invalid comma-joined cidr_ip.
func TestGenerateAliyunFirewall_PerCIDRRules(t *testing.T) {
	t.Parallel()

	manager := NewManager()
	req := &ProvisionRequest{
		Provider:    "aliyun",
		BuilderPort: 9090,
	}
	allowedIPs := []string{"1.2.3.0/24", "10.0.0.0/8"}

	config := manager.generateAliyunFirewall(req, allowedIPs)

	// One builder rule resource per CIDR.
	if got := strings.Count(config, `resource "alicloud_security_group_rule" "builder_`); got != len(allowedIPs) {
		t.Errorf("got %d per-CIDR builder rules, want %d\n%s", got, len(allowedIPs), config)
	}

	// Each CIDR must appear as its own single-value cidr_ip.
	for _, cidr := range allowedIPs {
		want := `cidr_ip           = "` + cidr + `"`
		if !strings.Contains(config, want) {
			t.Errorf("missing per-CIDR rule for %s", cidr)
		}
	}

	// The invalid comma-joined cidr_ip must never be produced.
	if strings.Contains(config, `cidr_ip           = "1.2.3.0/24,10.0.0.0/8"`) {
		t.Error("firewall emitted invalid multi-CIDR cidr_ip")
	}
}

// TestProvision_UnimplementedProvider verifies finding #27: AWS and Aliyun are
// non-functional stubs and must fail fast with a clear error rather than
// generating broken terraform.
func TestProvision_UnimplementedProvider(t *testing.T) {
	t.Parallel()

	manager := NewManager()

	// aws is now supported (its Provision path runs terraform, so it is NOT
	// exercised here — that needs a live account). Only genuinely-unimplemented
	// providers must fail fast with a clear error.
	for _, provider := range []string{"aliyun", "azure", ""} {
		t.Run("provider_"+provider, func(t *testing.T) {
			t.Parallel()
			_, err := manager.Provision(&ProvisionRequest{Provider: provider})
			if err == nil {
				t.Fatalf("Provision(%q) should return an error", provider)
			}
			if !strings.Contains(err.Error(), "not implemented") {
				t.Errorf("Provision(%q) error = %q, want 'not implemented'", provider, err)
			}
		})
	}
}

// TestSSHHostKeyArgs verifies finding #50: host-key verification is only
// disabled when explicitly opted in, uses a supplied known_hosts path when
// available, and otherwise fails closed with strict checking.
func TestSSHHostKeyArgs(t *testing.T) {
	t.Parallel()

	joinArgs := func(a []string) string { return strings.Join(a, " ") }

	t.Run("insecure opt-in", func(t *testing.T) {
		t.Parallel()
		args := joinArgs(sshHostKeyArgs(&SSHConfig{InsecureHostKey: true}))
		if !strings.Contains(args, "StrictHostKeyChecking=no") ||
			!strings.Contains(args, "UserKnownHostsFile=/dev/null") {
			t.Errorf("insecure opt-in should disable host key checking, got %q", args)
		}
	})

	t.Run("known_hosts path", func(t *testing.T) {
		t.Parallel()
		args := joinArgs(sshHostKeyArgs(&SSHConfig{KnownHostsPath: "/etc/known_hosts"}))
		if !strings.Contains(args, "StrictHostKeyChecking=yes") ||
			!strings.Contains(args, "UserKnownHostsFile=/etc/known_hosts") {
			t.Errorf("known_hosts should enable strict checking against the file, got %q", args)
		}
	})

	t.Run("default fails closed", func(t *testing.T) {
		t.Parallel()
		args := joinArgs(sshHostKeyArgs(nil))
		if strings.Contains(args, "StrictHostKeyChecking=no") {
			t.Errorf("default must not disable host key checking, got %q", args)
		}
		if !strings.Contains(args, "StrictHostKeyChecking=yes") {
			t.Errorf("default should use strict checking, got %q", args)
		}
	})
}

// TestGenerateAWSConfig verifies the generated AWS Terraform is structurally
// correct: a data-source AMI (not a hardcoded region-specific ID), an injected
// SSH key pair, an arch-appropriate instance type, and — when terraform is
// available — that the HCL actually validates.
func TestGenerateAWSConfig(t *testing.T) {
	t.Parallel()
	m := NewManager()

	req := &ProvisionRequest{
		Provider: "aws",
		Arch:     "arm64",
		SSH:      &SSHConfig{KeyPath: "/home/build/.ssh/id_ed25519", User: "ubuntu"},
		Spec:     map[string]string{"region": "eu-west-1"},
	}
	main := m.generateAWSConfig(req, "eu-west-1", "")
	fw := m.generateAWSFirewall(req, []string{"10.0.0.0/8"})

	// Must NOT contain the old hardcoded, region-specific AMI.
	if strings.Contains(main, "ami-0c55b159cbfafe1f0") {
		t.Error("still uses the hardcoded region-specific AMI")
	}
	for _, want := range []string{
		`data "aws_ami" "ubuntu"`,           // dynamic AMI lookup
		"data.aws_ami.ubuntu.id",            // instance uses the resolved AMI
		`resource "aws_key_pair" "portage"`, // SSH key injection
		"aws_key_pair.portage.key_name",
		`file("/home/build/.ssh/id_ed25519.pub")`,
		`"t4g.large"`,                      // arm64 -> Graviton
		`values = ["arm64"]`,               // architecture filter
		"ubuntu-jammy-22.04-arm64-server-", // name-arch token
	} {
		if !strings.Contains(main, want) {
			t.Errorf("generated AWS config missing %q", want)
		}
	}

	// amd64 default instance type.
	amd := m.generateAWSConfig(&ProvisionRequest{Provider: "aws", Arch: "amd64"}, "us-east-1", "")
	if !strings.Contains(amd, `"t3.large"`) {
		t.Error("amd64 should default to t3.large")
	}

	// Full `terraform init`/`validate` downloads the AWS provider (slow, network),
	// so it is opt-in via PORTAGE_TF_VALIDATE=1 rather than run on every `go test`.
	if os.Getenv("PORTAGE_TF_VALIDATE") != "1" {
		return
	}
	tf, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("terraform not available; skipping HCL validation")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "firewall.tf"), []byte(fw), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-input=false", "-no-color"}, {"validate", "-no-color"}} {
		cmd := exec.Command(tf, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("terraform %v failed: %v\n%s", args, err, out)
		}
	}
}
