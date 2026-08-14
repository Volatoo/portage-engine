package iac

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPVEInstanceSpec(t *testing.T) {
	t.Parallel()

	spec := DefaultPVEInstanceSpec()

	if spec == nil {
		t.Fatal("DefaultPVEInstanceSpec returned nil")
	}

	verifyBasicFields(t, spec)
	verifyResourceFields(t, spec)
	verifyStorageFields(t, spec)
	verifyBooleanFields(t, spec)
	verifyTagsField(t, spec)
}

func verifyBasicFields(t *testing.T, spec *PVEInstanceSpec) {
	if spec.Node != "pve" {
		t.Errorf("Node = %s, want pve", spec.Node)
	}
	if spec.OSType != "l26" {
		t.Errorf("OSType = %s, want l26", spec.OSType)
	}
}

func verifyResourceFields(t *testing.T, spec *PVEInstanceSpec) {
	if spec.Cores != 4 {
		t.Errorf("Cores = %d, want 4", spec.Cores)
	}
	if spec.Sockets != 1 {
		t.Errorf("Sockets = %d, want 1", spec.Sockets)
	}
	if spec.MemoryMB != 8192 {
		t.Errorf("MemoryMB = %d, want 8192", spec.MemoryMB)
	}
}

func verifyStorageFields(t *testing.T, spec *PVEInstanceSpec) {
	if spec.DiskSizeGB != 50 {
		t.Errorf("DiskSizeGB = %d, want 50", spec.DiskSizeGB)
	}
	if spec.DiskType != "scsi" {
		t.Errorf("DiskType = %s, want scsi", spec.DiskType)
	}
	if spec.Storage != "local-lvm" {
		t.Errorf("Storage = %s, want local-lvm", spec.Storage)
	}
	if spec.Network != "vmbr0" {
		t.Errorf("Network = %s, want vmbr0", spec.Network)
	}
}

func verifyBooleanFields(t *testing.T, spec *PVEInstanceSpec) {
	if !spec.Agent {
		t.Error("Agent should be true by default")
	}
	if !spec.StartOnBoot {
		t.Error("StartOnBoot should be true by default")
	}
	if !spec.CloudInit {
		t.Error("CloudInit should be true by default")
	}
}

func verifyTagsField(t *testing.T, spec *PVEInstanceSpec) {
	if len(spec.Tags) != 1 || spec.Tags[0] != "portage-builder" {
		t.Errorf("Tags = %v, want [portage-builder]", spec.Tags)
	}
}

func TestPVEInstanceSpecFromMap(t *testing.T) {
	t.Parallel()

	testCases := createPVETestCases()

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := PVEInstanceSpecFromMap(tc.input)
			if spec == nil {
				t.Fatal("PVEInstanceSpecFromMap returned nil")
			}
			tc.validate(t, spec)
		})
	}
}

func createPVETestCases() []struct {
	name     string
	input    map[string]string
	validate func(*testing.T, *PVEInstanceSpec)
} {
	return []struct {
		name     string
		input    map[string]string
		validate func(*testing.T, *PVEInstanceSpec)
	}{
		{name: "empty map returns defaults", input: map[string]string{}, validate: validatePVEDefaults},
		{name: "custom node and cores", input: map[string]string{"node": "pve-node1", "cores": "8"}, validate: validatePVENodeCores},
		{name: "custom memory and disk", input: map[string]string{"memory_mb": "16384", "disk_size_gb": "100", "disk_type": "virtio"}, validate: validatePVEMemoryDisk},
		{name: "network configuration", input: map[string]string{"network": "vmbr1", "vlan": "100", "ip_config": "192.168.1.100/24", "gateway": "192.168.1.1", "nameserver": "8.8.8.8"}, validate: validatePVENetwork},
		{name: "vmid and pool", input: map[string]string{"vmid": "200", "pool": "builders"}, validate: validatePVEVMIDPool},
		{name: "boolean values", input: map[string]string{"cloud_init": "false", "agent": "0"}, validate: validatePVEBooleans},
	}
}

func validatePVEDefaults(t *testing.T, spec *PVEInstanceSpec) {
	if spec.Node != "pve" {
		t.Errorf("Node = %s, want pve", spec.Node)
	}
	if spec.Cores != 4 {
		t.Errorf("Cores = %d, want 4", spec.Cores)
	}
}

func validatePVENodeCores(t *testing.T, spec *PVEInstanceSpec) {
	if spec.Node != "pve-node1" {
		t.Errorf("Node = %s, want pve-node1", spec.Node)
	}
	if spec.Cores != 8 {
		t.Errorf("Cores = %d, want 8", spec.Cores)
	}
}

func validatePVEMemoryDisk(t *testing.T, spec *PVEInstanceSpec) {
	if spec.MemoryMB != 16384 {
		t.Errorf("MemoryMB = %d, want 16384", spec.MemoryMB)
	}
	if spec.DiskSizeGB != 100 {
		t.Errorf("DiskSizeGB = %d, want 100", spec.DiskSizeGB)
	}
	if spec.DiskType != "virtio" {
		t.Errorf("DiskType = %s, want virtio", spec.DiskType)
	}
}

func validatePVENetwork(t *testing.T, spec *PVEInstanceSpec) {
	if spec.Network != "vmbr1" || spec.VLAN != 100 || spec.IPConfig != "192.168.1.100/24" || spec.Gateway != "192.168.1.1" || spec.Nameserver != "8.8.8.8" {
		t.Error("Network configuration mismatch")
	}
}

func validatePVEVMIDPool(t *testing.T, spec *PVEInstanceSpec) {
	if spec.VMID != 200 {
		t.Errorf("VMID = %d, want 200", spec.VMID)
	}
	if spec.Pool != "builders" {
		t.Errorf("Pool = %s, want builders", spec.Pool)
	}
}

func validatePVEBooleans(t *testing.T, spec *PVEInstanceSpec) {
	if spec.CloudInit {
		t.Error("CloudInit should be false")
	}
	if spec.Agent {
		t.Error("Agent should be false")
	}
}

func TestNewPVEProvisioner(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &PVEConfig{
		Endpoint:    "https://pve.example.com:8006",
		Node:        "pve-node1",
		TokenID:     "root@pam!terraform",
		TokenSecret: "secret-token",
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewPVEProvisioner(config)
	if err != nil {
		t.Fatalf("NewPVEProvisioner failed: %v", err)
	}

	if provisioner == nil {
		t.Fatal("NewPVEProvisioner returned nil")
	}

	if provisioner.stateDir != tmpDir {
		t.Errorf("stateDir = %s, want %s", provisioner.stateDir, tmpDir)
	}
}

func TestNewPVEProvisioner_NilConfig(t *testing.T) {
	t.Parallel()

	_, err := NewPVEProvisioner(nil)
	if err == nil {
		t.Error("Expected error for nil config")
	}
}

func TestNewPVEProvisioner_MissingEndpoint(t *testing.T) {
	t.Parallel()

	config := &PVEConfig{
		Node:        "pve-node1",
		TokenID:     "root@pam!terraform",
		TokenSecret: "secret-token",
	}

	_, err := NewPVEProvisioner(config)
	if err == nil {
		t.Error("Expected error for missing endpoint")
	}
}

func TestNewPVEProvisioner_DefaultStateDir(t *testing.T) {
	t.Parallel()

	config := &PVEConfig{
		Endpoint:    "https://pve.example.com:8006",
		Node:        "pve-node1",
		TokenID:     "root@pam!terraform",
		TokenSecret: "secret-token",
		BuilderPort: 9090,
	}

	provisioner, err := NewPVEProvisioner(config)
	if err != nil {
		t.Fatalf("NewPVEProvisioner failed: %v", err)
	}

	expectedDir := filepath.Join(os.TempDir(), "portage-terraform", "pve")
	if provisioner.stateDir != expectedDir {
		t.Errorf("stateDir = %s, want %s", provisioner.stateDir, expectedDir)
	}
}

func TestPVEProvisioner_GenerateMainTF(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &PVEConfig{
		Endpoint:    "https://pve.example.com:8006",
		Node:        "pve-node1",
		TokenID:     "root@pam!terraform",
		TokenSecret: "secret-token",
		StateDir:    tmpDir,
		BuilderPort: 9090,
		SSHUser:     "root",
		Insecure:    true,
	}

	provisioner, err := NewPVEProvisioner(config)
	if err != nil {
		t.Fatalf("NewPVEProvisioner failed: %v", err)
	}

	spec := &PVEInstanceSpec{
		Node:        "pve-node1",
		Cores:       4,
		Sockets:     1,
		MemoryMB:    8192,
		DiskSizeGB:  50,
		DiskType:    "scsi",
		Storage:     "local-lvm",
		Template:    "debian-12-template",
		OSType:      "l26",
		Network:     "vmbr0",
		Tags:        []string{"portage-builder", "test"},
		Agent:       true,
		StartOnBoot: true,
		CloudInit:   true,
		IPConfig:    "dhcp",
	}

	tf := provisioner.GenerateMainTF(spec, "test-builder")

	// Verify key components (telmate 3.x schema: exact-pinned rc version,
	// cpu block, nested disks with a cloud-init drive, network id).
	expectedStrings := []string{
		`required_providers`,
		`proxmox`,
		`telmate/proxmox`,
		`version = "` + pveProviderVersion + `"`,
		`pm_api_url`,
		`https://pve.example.com:8006/api2/json`,
		`pm_tls_insecure = true`,
		`proxmox_vm_qemu`,
		`name        = "test-builder"`,
		`target_node = "pve-node1"`,
		`cpu {`,
		`cores   = 4`,
		`sockets = 1`,
		`memory      = 8192`,
		`agent       = 1`,
		`os_type = "cloud-init"`,
		`ciupgrade = false`,
		`clone       = "debian-12-template"`,
		`disks {`,
		`scsi0 {`,
		`storage = "local-lvm"`,
		`size    = "50G"`,
		`cloudinit {`,
		`id     = 0`,
		`bridge = "vmbr0"`,
		`firewall = true`,
		`ipconfig0 = "ip=dhcp"`,
		`tags = "portage-builder,test"`,
		`output "instance_name"`,
		`output "vmid"`,
		`output "ip_address"`,
		`output "node"`,
	}

	for _, expected := range expectedStrings {
		if !strings.Contains(tf, expected) {
			t.Errorf("Generated TF missing expected string: %s", expected)
		}
	}
	for _, deprecated := range []string{"\n  cores       =", "\n  sockets     ="} {
		if strings.Contains(tf, deprecated) {
			t.Errorf("generated deprecated Telmate 3.x CPU argument %q", deprecated)
		}
	}

	// The secret must NOT be embedded as a plaintext literal in main.tf; the
	// proxmox provider reads credentials from PM_* environment variables.
	if strings.Contains(tf, "secret-token") {
		t.Error("token secret leaked as a plaintext literal in generated TF")
	}
}

func TestPVEProvisioner_EnforcedVMStartsStopped(t *testing.T) {
	t.Parallel()
	provisioner, err := NewPVEProvisioner(&PVEConfig{
		Endpoint: "https://pve.example.internal", Node: "pve01", Storage: "local-lvm",
		Network: "vmbr0", Template: "gentoo", TokenID: "test@pve!builder", TokenSecret: "secret",
		StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := DefaultPVEInstanceSpec()
	spec.StartStopped = true
	tf := provisioner.GenerateMainTF(spec, "egress-test")
	for _, expected := range []string{`vm_state              = "stopped"`, `define_connection_info = false`, `firewall = true`} {
		if !strings.Contains(tf, expected) {
			t.Fatalf("enforced VM Terraform is missing %q", expected)
		}
	}
}

func TestPVEProvisioner_SMBIOSUUIDUsesTelmate3Block(t *testing.T) {
	t.Parallel()
	provisioner, err := NewPVEProvisioner(&PVEConfig{
		Endpoint: "https://pve.example.internal", Node: "pve01",
		Storage: "local-lvm", Network: "vmbr0", Template: "gentoo",
		TokenID: "test@pve!builder", TokenSecret: "secret",
		StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := DefaultPVEInstanceSpec()
	spec.SMBIOSUUID = "123E4567-E89B-12D3-A456-426614174000"
	tf := provisioner.GenerateMainTF(spec, "identity-test")
	for _, expected := range []string{
		"smbios {", `uuid = "123e4567-e89b-12d3-a456-426614174000"`,
	} {
		if !strings.Contains(tf, expected) {
			t.Fatalf("generated Telmate 3.x SMBIOS block is missing %q", expected)
		}
	}
	if strings.Contains(tf, "smbios1") {
		t.Fatal("generated deprecated top-level smbios1 argument")
	}
}

func TestPVEProvisioner_GenerateMainTF_WithVMID(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &PVEConfig{
		Endpoint:    "https://pve.example.com:8006",
		Node:        "pve-node1",
		TokenID:     "root@pam!terraform",
		TokenSecret: "secret-token",
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewPVEProvisioner(config)
	if err != nil {
		t.Fatalf("NewPVEProvisioner failed: %v", err)
	}

	spec := DefaultPVEInstanceSpec()
	spec.VMID = 200
	spec.Pool = "builders"

	tf := provisioner.GenerateMainTF(spec, "test-builder")

	if !strings.Contains(tf, "vmid = 200") {
		t.Error("Generated TF missing vmid")
	}
	if !strings.Contains(tf, `pool = "builders"`) {
		t.Error("Generated TF missing pool")
	}
}

func TestPVEProvisioner_GenerateMainTF_WithVLAN(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &PVEConfig{
		Endpoint:    "https://pve.example.com:8006",
		Node:        "pve-node1",
		TokenID:     "root@pam!terraform",
		TokenSecret: "secret-token",
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewPVEProvisioner(config)
	if err != nil {
		t.Fatalf("NewPVEProvisioner failed: %v", err)
	}

	spec := DefaultPVEInstanceSpec()
	spec.VLAN = 100

	tf := provisioner.GenerateMainTF(spec, "test-builder")

	if !strings.Contains(tf, "tag = 100") {
		t.Error("Generated TF missing VLAN tag")
	}
}

func TestPVEProvisioner_GenerateMainTF_WithStaticIP(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &PVEConfig{
		Endpoint:    "https://pve.example.com:8006",
		Node:        "pve-node1",
		TokenID:     "root@pam!terraform",
		TokenSecret: "secret-token",
		StateDir:    tmpDir,
		BuilderPort: 9090,
		SSHUser:     "admin",
	}

	provisioner, err := NewPVEProvisioner(config)
	if err != nil {
		t.Fatalf("NewPVEProvisioner failed: %v", err)
	}

	spec := DefaultPVEInstanceSpec()
	spec.IPConfig = "192.168.1.100/24"
	spec.Gateway = "192.168.1.1"
	spec.Nameserver = "8.8.8.8"

	tf := provisioner.GenerateMainTF(spec, "test-builder")

	if !strings.Contains(tf, `ip=192.168.1.100/24,gw=192.168.1.1`) {
		t.Error("Generated TF missing static IP configuration")
	}
	if !strings.Contains(tf, `nameserver = "8.8.8.8"`) {
		t.Error("Generated TF missing nameserver")
	}
	if !strings.Contains(tf, `ciuser = "admin"`) {
		t.Error("Generated TF missing ciuser")
	}
	if !strings.Contains(tf, `ciupgrade = false`) {
		t.Error("Generated TF must disable implicit cloud-init package upgrades")
	}
}

func TestPVEProvisioner_GenerateMainTF_PasswordAuth(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &PVEConfig{
		Endpoint:    "https://pve.example.com:8006",
		Node:        "pve-node1",
		Username:    "root@pam",
		Password:    "password123",
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewPVEProvisioner(config)
	if err != nil {
		t.Fatalf("NewPVEProvisioner failed: %v", err)
	}

	tf := provisioner.GenerateMainTF(nil, "test-builder")

	// The password must be referenced via a variable, never embedded as a literal
	// (finding #30).
	if !strings.Contains(tf, `pm_password = var.pve_password`) {
		t.Error("Generated TF should reference var.pve_password")
	}
	if !strings.Contains(tf, `variable "pve_password"`) {
		t.Error("Generated TF should declare the pve_password variable")
	}
	if strings.Contains(tf, "password123") {
		t.Error("Generated TF must NOT contain the plaintext password")
	}

	// AuthEnv must supply the secret out-of-band via TF_VAR_*.
	env := provisioner.AuthEnv()
	if len(env) != 1 || env[0] != "TF_VAR_pve_password=password123" {
		t.Errorf("AuthEnv = %v, want [TF_VAR_pve_password=password123]", env)
	}
}

func TestPVEProvisioner_GenerateMainTF_DefaultSpec(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &PVEConfig{
		Endpoint:    "https://pve.example.com:8006",
		Node:        "pve-node1",
		Storage:     "ceph-pool",
		Network:     "vmbr1",
		Template:    "my-template",
		TokenID:     "root@pam!terraform",
		TokenSecret: "secret-token",
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewPVEProvisioner(config)
	if err != nil {
		t.Fatalf("NewPVEProvisioner failed: %v", err)
	}

	// Pass nil spec to use defaults with config overrides
	tf := provisioner.GenerateMainTF(nil, "test-builder")

	if !strings.Contains(tf, `target_node = "pve-node1"`) {
		t.Error("Generated TF should use config node")
	}
	if !strings.Contains(tf, `storage = "ceph-pool"`) {
		t.Error("Generated TF should use config storage")
	}
	if !strings.Contains(tf, `bridge = "vmbr1"`) {
		t.Error("Generated TF should use config network")
	}
	if !strings.Contains(tf, `clone       = "my-template"`) {
		t.Error("Generated TF should use config template")
	}
}

func TestPVEProvisioner_GenerateVariablesTF(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &PVEConfig{
		Endpoint:    "https://pve.example.com:8006",
		Node:        "pve-node1",
		TokenID:     "root@pam!terraform",
		TokenSecret: "secret-token",
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewPVEProvisioner(config)
	if err != nil {
		t.Fatalf("NewPVEProvisioner failed: %v", err)
	}

	spec := &PVEInstanceSpec{
		Node:       "pve-node1",
		Cores:      8,
		MemoryMB:   16384,
		DiskSizeGB: 100,
		Storage:    "ceph-pool",
		Network:    "vmbr1",
	}

	tf := provisioner.GenerateVariablesTF(spec)

	expectedStrings := []string{
		`variable "pve_endpoint"`,
		`variable "pve_node"`,
		`variable "vm_cores"`,
		`variable "vm_memory"`,
		`variable "vm_disk_size"`,
		`variable "vm_storage"`,
		`variable "vm_network"`,
		`default     = "https://pve.example.com:8006"`,
		`default     = "pve-node1"`,
		`default     = 8`,
		`default     = 16384`,
		`default     = 100`,
		`default     = "ceph-pool"`,
		`default     = "vmbr1"`,
	}

	for _, expected := range expectedStrings {
		if !strings.Contains(tf, expected) {
			t.Errorf("Generated variables TF missing expected string: %s", expected)
		}
	}
}

func TestBoolToInt(t *testing.T) {
	t.Parallel()

	if boolToInt(true) != 1 {
		t.Error("boolToInt(true) should return 1")
	}
	if boolToInt(false) != 0 {
		t.Error("boolToInt(false) should return 0")
	}
}

// TestPVEProvisioner_GenerateMainTF_NoPlaintextTokenSecret verifies finding #30:
// the API token secret must be passed via a Terraform variable, not embedded as
// a literal in the generated main.tf.
func TestPVEProvisioner_GenerateMainTF_NoPlaintextTokenSecret(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	const secret = "super-secret-token-value"
	config := &PVEConfig{
		Endpoint:    "https://pve.example.com:8006",
		Node:        "pve-node1",
		TokenID:     "root@pam!terraform",
		TokenSecret: secret,
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewPVEProvisioner(config)
	if err != nil {
		t.Fatalf("NewPVEProvisioner failed: %v", err)
	}

	tf := provisioner.GenerateMainTF(nil, "test-builder")

	if strings.Contains(tf, secret) {
		t.Error("Generated TF must NOT contain the plaintext token secret")
	}
	if !strings.Contains(tf, `pm_api_token_secret = var.pve_token_secret`) {
		t.Error("Generated TF should reference var.pve_token_secret")
	}
	if !strings.Contains(tf, `variable "pve_token_secret"`) {
		t.Error("Generated TF should declare the pve_token_secret variable")
	}
	// The token ID is not itself a credential and may still appear inline.
	if !strings.Contains(tf, `pm_api_token_id     = "root@pam!terraform"`) {
		t.Error("Generated TF should still include the token ID inline")
	}

	env := provisioner.AuthEnv()
	if len(env) != 1 || env[0] != "TF_VAR_pve_token_secret="+secret {
		t.Errorf("AuthEnv = %v, want [TF_VAR_pve_token_secret=%s]", env, secret)
	}
}

// hostilePVEEndpointCases are the endpoints the Go API client in
// pve_scheduler.go already refuses. Terraform is the other way the same
// credential leaves this process — GenerateMainTF writes the endpoint into
// pm_api_url next to pm_api_token_secret, and GenerateVariablesTF repeats it as
// the pve_endpoint default — so the provisioner has to refuse them too, and
// refuse them at construction, before any generator can be called.
func hostilePVEEndpointCases() []struct {
	name     string
	endpoint string
	insecure bool
} {
	return []struct {
		name     string
		endpoint string
		insecure bool
	}{
		{"attacker host over plain http", "http://attacker.example.net:8006", false},
		{"userinfo smuggles a second credential", "https://root@pam!x:leak@attacker.example.net:8006", false},
		{"path swallows the appended api2/json", "https://attacker.example.net/collect", false},
		{"query swallows the appended api2/json", "https://attacker.example.net:8006/?x=", false},
		{"fragment truncates the request target", "https://attacker.example.net:8006/#", false},
		{"non-http scheme", "file:///etc/passwd", false},
		{"scheme relative", "//attacker.example.net:8006", false},
		{"blank endpoint", "   ", false},
		// Still hostile with the operator's plaintext opt-in set: insecure
		// relaxes the scheme and nothing else.
		{"userinfo with insecure set", "http://root:leak@attacker.example.net:8006", true},
		{"path with insecure set", "http://10.31.0.200:8006/collect", true},
	}
}

// TestNewPVEProvisioner_RejectsHostileEndpointBeforeGeneratingHCL is the attack:
// a provisioner built on an endpoint the Go client would refuse must fail to
// construct, so the PVE token never reaches a generated provider block pointed
// at an attacker.
func TestNewPVEProvisioner_RejectsHostileEndpointBeforeGeneratingHCL(t *testing.T) {
	t.Parallel()

	const secret = "super-secret-token-value"

	for _, tc := range hostilePVEEndpointCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			config := &PVEConfig{
				Endpoint:    tc.endpoint,
				Node:        "pve-node1",
				TokenID:     "root@pam!terraform",
				TokenSecret: secret,
				Insecure:    tc.insecure,
				StateDir:    t.TempDir(),
			}

			provisioner, err := NewPVEProvisioner(config)
			if err != nil {
				return
			}

			// Constructed anyway: show exactly what terraform would have been
			// handed, so the failure names the leak rather than the rule.
			mainTF := provisioner.GenerateMainTF(nil, "test-builder")
			variablesTF := provisioner.GenerateVariablesTF(nil)
			t.Errorf("NewPVEProvisioner accepted hostile endpoint %q (insecure=%t);\n"+
				"generated main.tf carries pm_api_url pointed at it alongside pm_api_token_secret: %v\n"+
				"generated variables.tf carries it as the pve_endpoint default: %v",
				tc.endpoint, tc.insecure,
				strings.Contains(mainTF, tc.endpoint),
				strings.Contains(variablesTF, tc.endpoint))
		})
	}
}

// TestNewPVEProvisioner_EndpointCannotEscapeGeneratedHCLString covers the
// injection that is separate from where the credential is delivered: net/url
// accepts a double quote as an ordinary host character, so an endpoint carrying
// one clears the SSRF gate and then closes pm_api_url's string literal early.
func TestNewPVEProvisioner_EndpointCannotEscapeGeneratedHCLString(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{
		`https://pve.example.com:8006"`,
		`https://pve.example.com"`,
		`https://a"b:8006`,
		`https://pve.example.com:8006'`,
		`https://pve.example.com:8006$`,
		`https://pve.example.com:8006<`,
	} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()

			config := &PVEConfig{
				Endpoint:    endpoint,
				Node:        "pve-node1",
				TokenID:     "root@pam!terraform",
				TokenSecret: "super-secret-token-value",
				StateDir:    t.TempDir(),
			}

			provisioner, err := NewPVEProvisioner(config)
			if err != nil {
				return
			}

			mainTF := provisioner.GenerateMainTF(nil, "test-builder")
			line := pveGeneratedLine(t, mainTF, "pm_api_url")
			t.Errorf("NewPVEProvisioner accepted endpoint %q; generated provider line is %s",
				endpoint, line)
		})
	}
}

// TestPVEProvisioner_GeneratedEndpointStaysInsideOneHCLLiteral is the sink-side
// half of the same claim: whatever survives construction must still sit inside
// exactly one quoted literal on each line that carries it.
func TestPVEProvisioner_GeneratedEndpointStaysInsideOneHCLLiteral(t *testing.T) {
	t.Parallel()

	provisioner, err := NewPVEProvisioner(&PVEConfig{
		Endpoint:    "https://pve.example.com:8006",
		Node:        "pve-node1",
		TokenID:     "root@pam!terraform",
		TokenSecret: "super-secret-token-value",
		StateDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewPVEProvisioner failed: %v", err)
	}

	for _, tc := range []struct {
		what   string
		body   string
		anchor string
	}{
		{"main.tf", provisioner.GenerateMainTF(nil, "test-builder"), "pm_api_url"},
		{"variables.tf", provisioner.GenerateVariablesTF(nil), `default     = "https`},
	} {
		line := pveGeneratedLine(t, tc.body, tc.anchor)
		if quotes := strings.Count(line, `"`); quotes != 2 {
			t.Errorf("%s line %q has %d quotes, want 2 — the endpoint escaped its literal",
				tc.what, line, quotes)
		}
	}
}

// pveGeneratedLine returns the single generated line containing anchor.
func pveGeneratedLine(t *testing.T, body, anchor string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, anchor) {
			return line
		}
	}
	t.Fatalf("generated config has no %q line:\n%s", anchor, body)
	return ""
}

// TestNewPVEProvisioner_InsecureOptInGovernsPlainHTTP pins the endpoint gate to
// the provisioner's own plaintext opt-in — the same flag it emits as
// pm_tls_insecure — rather than to a hardcoded answer in either direction.
// http://10.31.0.200:8006 is a real trusted-LAN deployment and must keep
// building; the identical endpoint without the opt-in must not.
func TestNewPVEProvisioner_InsecureOptInGovernsPlainHTTP(t *testing.T) {
	t.Parallel()

	const lanEndpoint = "http://10.31.0.200:8006"

	provisioner, err := NewPVEProvisioner(&PVEConfig{
		Endpoint:    lanEndpoint,
		Node:        "pve01",
		TokenID:     "root@pam!terraform",
		TokenSecret: "super-secret-token-value",
		Insecure:    true,
		StateDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("trusted-LAN endpoint %q with the insecure opt-in must build: %v", lanEndpoint, err)
	}
	if tf := provisioner.GenerateMainTF(nil, "test-builder"); !strings.Contains(tf, `pm_api_url      = "`+lanEndpoint+`/api2/json"`) {
		t.Errorf("generated main.tf lost the LAN endpoint:\n%s", tf)
	}

	if _, err := NewPVEProvisioner(&PVEConfig{
		Endpoint:    lanEndpoint,
		Node:        "pve01",
		TokenID:     "root@pam!terraform",
		TokenSecret: "super-secret-token-value",
		Insecure:    false,
		StateDir:    t.TempDir(),
	}); err == nil {
		t.Errorf("plain HTTP endpoint %q was accepted without the insecure opt-in", lanEndpoint)
	}
}

// TestNewPVEProvisioner_AcceptsEndpointsTheSystemProduces guards the gate
// against over-tightening: these are the shapes the deploy configs, the LAN
// cluster and the image factory (proxmox_url minus /api2/json) actually hand in.
func TestNewPVEProvisioner_AcceptsEndpointsTheSystemProduces(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		endpoint string
		insecure bool
	}{
		{"https://pve.example.com:8006", false},
		{"https://pve.example.internal:8006", false},
		{"https://pve-node1.lan", false},
		{"https://pve.internal:8006/", false},
		{"https://[fd00::1]:8006", false},
		{"http://10.31.0.200:8006", true},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			t.Parallel()

			provisioner, err := NewPVEProvisioner(&PVEConfig{
				Endpoint:    tc.endpoint,
				Node:        "pve01",
				TokenID:     "root@pam!terraform",
				TokenSecret: "super-secret-token-value",
				Insecure:    tc.insecure,
				StateDir:    t.TempDir(),
			})
			if err != nil {
				t.Fatalf("legitimate endpoint %q was rejected: %v", tc.endpoint, err)
			}
			// The trailing slash is dropped so the generated URL matches the one
			// the Go client builds instead of becoming host//api2/json.
			want := strings.TrimRight(tc.endpoint, "/")
			if tf := provisioner.GenerateMainTF(nil, "test-builder"); !strings.Contains(tf, `pm_api_url      = "`+want+`/api2/json"`) {
				t.Errorf("generated main.tf does not carry %q:\n%s", want, tf)
			}
			if vars := provisioner.GenerateVariablesTF(nil); !strings.Contains(vars, `default     = "`+want+`"`) {
				t.Errorf("generated variables.tf does not carry %q:\n%s", want, vars)
			}
		})
	}
}
