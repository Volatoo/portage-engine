package iac

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultGCPInstanceSpec(t *testing.T) {
	t.Parallel()

	spec := DefaultGCPInstanceSpec()

	if spec == nil {
		t.Fatal("DefaultGCPInstanceSpec returned nil")
	}

	if spec.Project != "portage-engine" {
		t.Errorf("Project = %s, want portage-engine", spec.Project)
	}
	if spec.Region != "us-central1" {
		t.Errorf("Region = %s, want us-central1", spec.Region)
	}
	if spec.Zone != "us-central1-a" {
		t.Errorf("Zone = %s, want us-central1-a", spec.Zone)
	}
	if spec.MachineType != "n1-standard-4" {
		t.Errorf("MachineType = %s, want n1-standard-4", spec.MachineType)
	}
	if spec.CPUCount != 4 {
		t.Errorf("CPUCount = %d, want 4", spec.CPUCount)
	}
	if spec.DiskSizeGB != 100 {
		t.Errorf("DiskSizeGB = %d, want 100", spec.DiskSizeGB)
	}
	if spec.DiskType != "pd-ssd" {
		t.Errorf("DiskType = %s, want pd-ssd", spec.DiskType)
	}
	if spec.ImageFamily != "ubuntu-2204-lts" {
		t.Errorf("ImageFamily = %s, want ubuntu-2204-lts", spec.ImageFamily)
	}
	if len(spec.Tags) != 1 || spec.Tags[0] != "portage-builder" {
		t.Errorf("Tags = %v, want [portage-builder]", spec.Tags)
	}
}

func TestGCPAvailableRegions(t *testing.T) {
	t.Parallel()

	regions := GCPAvailableRegions()

	if len(regions) == 0 {
		t.Error("GCPAvailableRegions returned empty list")
	}

	// Check for some well-known regions
	expectedRegions := []string{"us-central1", "us-east1", "europe-west1", "asia-east1"}
	for _, expected := range expectedRegions {
		found := false
		for _, r := range regions {
			if r == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected region %s not found", expected)
		}
	}
}

func TestGCPAvailableZones(t *testing.T) {
	t.Parallel()

	zones := GCPAvailableZones("us-central1")

	if len(zones) == 0 {
		t.Error("GCPAvailableZones returned empty list")
	}

	// Check that zones have correct prefix
	for _, z := range zones {
		if !strings.HasPrefix(z, "us-central1-") {
			t.Errorf("Zone %s does not have expected prefix", z)
		}
	}

	// Check for zone 'a'
	found := false
	for _, z := range zones {
		if z == "us-central1-a" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected zone us-central1-a not found")
	}
}

func TestGCPMachineTypes(t *testing.T) {
	t.Parallel()

	types := GCPMachineTypes()

	if len(types) == 0 {
		t.Error("GCPMachineTypes returned empty map")
	}

	// Check for common machine types
	expectedTypes := []string{"n1-standard-4", "e2-standard-4", "c2-standard-4"}
	for _, expected := range expectedTypes {
		if _, ok := types[expected]; !ok {
			t.Errorf("Expected machine type %s not found", expected)
		}
	}

	// Verify n1-standard-4 specs
	if spec, ok := types["n1-standard-4"]; ok {
		if spec.CPUs != 4 {
			t.Errorf("n1-standard-4 CPUs = %d, want 4", spec.CPUs)
		}
		if spec.MemoryMB != 15360 {
			t.Errorf("n1-standard-4 MemoryMB = %d, want 15360", spec.MemoryMB)
		}
	}
}

func TestNewGCPProvisioner(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &GCPConfig{
		Project:     "test-project",
		Region:      "us-central1",
		Zone:        "us-central1-a",
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewGCPProvisioner(config)
	if err != nil {
		t.Fatalf("NewGCPProvisioner failed: %v", err)
	}

	if provisioner == nil {
		t.Fatal("NewGCPProvisioner returned nil")
	}

	if provisioner.stateDir != tmpDir {
		t.Errorf("stateDir = %s, want %s", provisioner.stateDir, tmpDir)
	}
}

func TestNewGCPProvisioner_NilConfig(t *testing.T) {
	t.Parallel()

	_, err := NewGCPProvisioner(nil)
	if err == nil {
		t.Error("Expected error for nil config")
	}
}

func TestNewGCPProvisioner_DefaultStateDir(t *testing.T) {
	t.Parallel()

	config := &GCPConfig{
		Project:     "test-project",
		Region:      "us-central1",
		Zone:        "us-central1-a",
		BuilderPort: 9090,
	}

	provisioner, err := NewGCPProvisioner(config)
	if err != nil {
		t.Fatalf("NewGCPProvisioner failed: %v", err)
	}

	expectedDir := filepath.Join(os.TempDir(), "portage-terraform", "gcp")
	if provisioner.stateDir != expectedDir {
		t.Errorf("stateDir = %s, want %s", provisioner.stateDir, expectedDir)
	}
}

func TestGCPProvisioner_GenerateMainTF(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &GCPConfig{
		Project:     "my-project",
		Region:      "europe-west1",
		Zone:        "europe-west1-b",
		StateDir:    tmpDir,
		BuilderPort: 9090,
		SSHKeyPath:  "/path/to/key",
		SSHUser:     "admin",
	}

	provisioner, err := NewGCPProvisioner(config)
	if err != nil {
		t.Fatalf("NewGCPProvisioner failed: %v", err)
	}

	spec := &GCPInstanceSpec{
		Project:      "my-project",
		Region:       "europe-west1",
		Zone:         "europe-west1-b",
		MachineType:  "n1-standard-8",
		DiskSizeGB:   200,
		DiskType:     "pd-ssd",
		ImageProject: "ubuntu-os-cloud",
		ImageFamily:  "ubuntu-2204-lts",
		Network:      "default",
		Tags:         []string{"portage-builder", "custom-tag"},
	}

	tf := provisioner.GenerateMainTF(spec, "test-instance")

	// Check required elements are present
	checks := []string{
		`project = "my-project"`,
		`region  = "europe-west1"`,
		`zone    = "europe-west1-b"`,
		`name         = "test-instance"`,
		`machine_type = "n1-standard-8"`,
		`size  = 200`,
		`type  = "pd-ssd"`,
		`image = "ubuntu-os-cloud/ubuntu-2204-lts"`,
		`output "ip_address"`,
		`output "private_ip"`,
		`output "instance_name"`,
		`output "zone"`,
		`output "machine_type"`,
	}

	for _, check := range checks {
		if !strings.Contains(tf, check) {
			t.Errorf("Generated TF missing: %s", check)
		}
	}
}

func TestGCPProvisioner_GenerateMainTF_DefaultSpec(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &GCPConfig{
		Project:     "test-project",
		Region:      "us-central1",
		Zone:        "us-central1-a",
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewGCPProvisioner(config)
	if err != nil {
		t.Fatalf("NewGCPProvisioner failed: %v", err)
	}

	// When spec is nil, GenerateMainTF uses DefaultGCPInstanceSpec which
	// has its own default values, then overrides empty fields from config.
	// The default spec already has values, so config won't override.
	tf := provisioner.GenerateMainTF(nil, "default-instance")

	// Should use defaults from DefaultGCPInstanceSpec
	if !strings.Contains(tf, `project = "portage-engine"`) {
		t.Error("Generated TF missing default project")
	}
	if !strings.Contains(tf, `region  = "us-central1"`) {
		t.Error("Generated TF missing default region")
	}
}

func TestGCPProvisioner_GenerateFirewallTF(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &GCPConfig{
		Project:         "test-project",
		Region:          "us-central1",
		Zone:            "us-central1-a",
		StateDir:        tmpDir,
		BuilderPort:     9090,
		AllowedIPRanges: []string{"10.0.0.0/8", "192.168.0.0/16"},
	}

	provisioner, err := NewGCPProvisioner(config)
	if err != nil {
		t.Fatalf("NewGCPProvisioner failed: %v", err)
	}

	tf := provisioner.GenerateFirewallTF("test-instance")

	// Check required elements
	checks := []string{
		`resource "google_compute_firewall" "portage_ssh_test-instance"`,
		`resource "google_compute_firewall" "portage_builder_test-instance"`,
		`resource "google_compute_firewall" "portage_icmp_test-instance"`,
		`ports    = ["22"]`,
		`ports    = ["9090"]`,
		`source_ranges = ["10.0.0.0/8", "192.168.0.0/16"]`,
		`target_tags   = ["portage-builder"]`,
		`target_tags   = ["allow-builder-9090"]`,
	}

	for _, check := range checks {
		if !strings.Contains(tf, check) {
			t.Errorf("Generated firewall TF missing: %s", check)
		}
	}
}

func TestGCPProvisioner_GenerateFirewallTF_DefaultIPs(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &GCPConfig{
		Project:     "test-project",
		Region:      "us-central1",
		Zone:        "us-central1-a",
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewGCPProvisioner(config)
	if err != nil {
		t.Fatalf("NewGCPProvisioner failed: %v", err)
	}

	tf := provisioner.GenerateFirewallTF("test-instance")

	// Should use 0.0.0.0/0 as default
	if !strings.Contains(tf, `source_ranges = ["0.0.0.0/0"]`) {
		t.Error("Generated firewall TF should default to 0.0.0.0/0")
	}
}

func TestGCPProvisioner_GenerateVariablesTF(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	config := &GCPConfig{
		Project:     "test-project",
		Region:      "us-central1",
		Zone:        "us-central1-a",
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewGCPProvisioner(config)
	if err != nil {
		t.Fatalf("NewGCPProvisioner failed: %v", err)
	}

	spec := &GCPInstanceSpec{
		Project:      "my-project",
		Region:       "europe-west1",
		Zone:         "europe-west1-b",
		MachineType:  "n1-standard-8",
		DiskSizeGB:   200,
		DiskType:     "pd-ssd",
		ImageProject: "ubuntu-os-cloud",
		ImageFamily:  "ubuntu-2204-lts",
		Preemptible:  true,
	}

	tf := provisioner.GenerateVariablesTF(spec)

	checks := []string{
		`variable "project"`,
		`variable "region"`,
		`variable "zone"`,
		`variable "machine_type"`,
		`variable "disk_size_gb"`,
		`variable "disk_type"`,
		`variable "preemptible"`,
		`variable "builder_port"`,
		`default     = "my-project"`,
		`default     = "europe-west1"`,
		`default     = "n1-standard-8"`,
		`default     = 200`,
		`default     = true`,
	}

	for _, check := range checks {
		if !strings.Contains(tf, check) {
			t.Errorf("Generated variables TF missing: %s", check)
		}
	}
}

func TestParseInstanceOutputs(t *testing.T) {
	t.Parallel()

	outputJSON := `{
		"instance_name": {"value": "test-instance"},
		"ip_address": {"value": "35.192.0.1"},
		"private_ip": {"value": "10.128.0.2"},
		"zone": {"value": "us-central1-a"},
		"machine_type": {"value": "n1-standard-4"}
	}`

	output, err := ParseTerraformOutputs([]byte(outputJSON))
	if err != nil {
		t.Fatalf("ParseTerraformOutputs failed: %v", err)
	}

	if output.InstanceName != "test-instance" {
		t.Errorf("InstanceName = %s, want test-instance", output.InstanceName)
	}
	if output.IPAddress != "35.192.0.1" {
		t.Errorf("IPAddress = %s, want 35.192.0.1", output.IPAddress)
	}
	if output.PrivateIP != "10.128.0.2" {
		t.Errorf("PrivateIP = %s, want 10.128.0.2", output.PrivateIP)
	}
	if output.Zone != "us-central1-a" {
		t.Errorf("Zone = %s, want us-central1-a", output.Zone)
	}
	if output.MachineType != "n1-standard-4" {
		t.Errorf("MachineType = %s, want n1-standard-4", output.MachineType)
	}
}

func TestParseInstanceOutputs_InvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := ParseTerraformOutputs([]byte("invalid json"))
	if err == nil {
		t.Error("Expected error for invalid JSON")
	}
}

func TestGenerateBuilderConfig(t *testing.T) {
	t.Parallel()

	output := &InstanceOutput{
		InstanceName: "my-builder",
		IPAddress:    "35.192.0.1",
		PrivateIP:    "10.128.0.2",
		Zone:         "us-central1-a",
		MachineType:  "n1-standard-4",
	}

	config := GenerateBuilderConfig(output, 9090)

	checks := []string{
		"BUILDER_PORT=9090",
		"INSTANCE_ID=my-builder",
		"ARCHITECTURE=amd64",
		"NATIVE_JOB_POLICY=single-use",
		"PERSISTENCE_ENABLED=true",
	}

	for _, check := range checks {
		if !strings.Contains(config, check) {
			t.Errorf("Generated config missing: %s", check)
		}
	}
}

func TestGenerateRemoteBuilderEntry(t *testing.T) {
	t.Parallel()

	output := &InstanceOutput{
		InstanceName: "my-builder",
		IPAddress:    "35.192.0.1",
	}

	entry := GenerateRemoteBuilderEntry(output, 9090)
	expected := "35.192.0.1:9090"

	if entry != expected {
		t.Errorf("GenerateRemoteBuilderEntry = %s, want %s", entry, expected)
	}
}

func TestGCPInstanceSpecFromMap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    map[string]string
		validate func(*testing.T, *GCPInstanceSpec)
	}{
		{
			name:  "empty map uses defaults",
			input: map[string]string{},
			validate: func(t *testing.T, spec *GCPInstanceSpec) {
				if spec.Project != "portage-engine" {
					t.Errorf("Project = %s, want portage-engine", spec.Project)
				}
			},
		},
		{
			name: "override project",
			input: map[string]string{
				"project": "custom-project",
			},
			validate: func(t *testing.T, spec *GCPInstanceSpec) {
				if spec.Project != "custom-project" {
					t.Errorf("Project = %s, want custom-project", spec.Project)
				}
			},
		},
		{
			name: "override all values",
			input: map[string]string{
				"project":       "my-proj",
				"region":        "europe-west1",
				"zone":          "europe-west1-b",
				"machine_type":  "n1-standard-8",
				"cpu_count":     "8",
				"memory_mb":     "32768",
				"disk_size_gb":  "500",
				"disk_type":     "pd-balanced",
				"image_project": "debian-cloud",
				"image_family":  "debian-11",
				"network":       "custom-network",
				"subnetwork":    "custom-subnet",
				"preemptible":   "true",
			},
			validate: validateGCPAllFields,
		},
		{
			name: "preemptible variations",
			input: map[string]string{
				"preemptible": "1",
			},
			validate: func(t *testing.T, spec *GCPInstanceSpec) {
				if !spec.Preemptible {
					t.Error("Preemptible should be true for value '1'")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := GCPInstanceSpecFromMap(tt.input)
			tt.validate(t, spec)
		})
	}
}

func validateGCPAllFields(t *testing.T, spec *GCPInstanceSpec) {
	validateGCPBasic(t, spec)
	validateGCPResources(t, spec)
	validateGCPImage(t, spec)
	validateGCPNetwork(t, spec)
}

func validateGCPBasic(t *testing.T, spec *GCPInstanceSpec) {
	if spec.Project != "my-proj" || spec.Region != "europe-west1" || spec.Zone != "europe-west1-b" || spec.MachineType != "n1-standard-8" {
		t.Error("Basic fields mismatch")
	}
}

func validateGCPResources(t *testing.T, spec *GCPInstanceSpec) {
	if spec.CPUCount != 8 || spec.MemoryMB != 32768 || spec.DiskSizeGB != 500 || spec.DiskType != "pd-balanced" {
		t.Error("Resource fields mismatch")
	}
}

func validateGCPImage(t *testing.T, spec *GCPInstanceSpec) {
	if spec.ImageProject != "debian-cloud" || spec.ImageFamily != "debian-11" {
		t.Error("Image fields mismatch")
	}
}

func validateGCPNetwork(t *testing.T, spec *GCPInstanceSpec) {
	if spec.Network != "custom-network" || spec.Subnetwork != "custom-subnet" || !spec.Preemptible {
		t.Error("Network fields mismatch")
	}
}

func TestValidateGCPSpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    *GCPInstanceSpec
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid spec",
			spec:    DefaultGCPInstanceSpec(),
			wantErr: false,
		},
		{
			name: "missing project",
			spec: &GCPInstanceSpec{
				Region:      "us-central1",
				Zone:        "us-central1-a",
				MachineType: "n1-standard-4",
				DiskSizeGB:  100,
				DiskType:    "pd-ssd",
			},
			wantErr: true,
			errMsg:  "project is required",
		},
		{
			name: "missing region",
			spec: &GCPInstanceSpec{
				Project:     "my-project",
				Zone:        "us-central1-a",
				MachineType: "n1-standard-4",
				DiskSizeGB:  100,
				DiskType:    "pd-ssd",
			},
			wantErr: true,
			errMsg:  "region is required",
		},
		{
			name: "missing zone",
			spec: &GCPInstanceSpec{
				Project:     "my-project",
				Region:      "us-central1",
				MachineType: "n1-standard-4",
				DiskSizeGB:  100,
				DiskType:    "pd-ssd",
			},
			wantErr: true,
			errMsg:  "zone is required",
		},
		{
			name: "missing machine_type",
			spec: &GCPInstanceSpec{
				Project:    "my-project",
				Region:     "us-central1",
				Zone:       "us-central1-a",
				DiskSizeGB: 100,
				DiskType:   "pd-ssd",
			},
			wantErr: true,
			errMsg:  "machine_type is required",
		},
		{
			name: "disk too small",
			spec: &GCPInstanceSpec{
				Project:     "my-project",
				Region:      "us-central1",
				Zone:        "us-central1-a",
				MachineType: "n1-standard-4",
				DiskSizeGB:  5,
				DiskType:    "pd-ssd",
			},
			wantErr: true,
			errMsg:  "disk_size_gb must be at least 10",
		},
		{
			name: "disk too large",
			spec: &GCPInstanceSpec{
				Project:     "my-project",
				Region:      "us-central1",
				Zone:        "us-central1-a",
				MachineType: "n1-standard-4",
				DiskSizeGB:  100000,
				DiskType:    "pd-ssd",
			},
			wantErr: true,
			errMsg:  "disk_size_gb must be at most 65536",
		},
		{
			name: "invalid disk type",
			spec: &GCPInstanceSpec{
				Project:     "my-project",
				Region:      "us-central1",
				Zone:        "us-central1-a",
				MachineType: "n1-standard-4",
				DiskSizeGB:  100,
				DiskType:    "invalid-type",
			},
			wantErr: true,
			errMsg:  "invalid disk_type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateGCPSpec(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error containing %q", tt.errMsg)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("Error = %v, want error containing %q", err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}
		})
	}
}

func TestGCPInstanceSpec_JSON(t *testing.T) {
	t.Parallel()

	spec := DefaultGCPInstanceSpec()
	spec.Project = "test-project"

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("Failed to marshal spec: %v", err)
	}

	var decoded GCPInstanceSpec
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal spec: %v", err)
	}

	if decoded.Project != "test-project" {
		t.Errorf("Project = %s, want test-project", decoded.Project)
	}
	if decoded.MachineType != spec.MachineType {
		t.Errorf("MachineType = %s, want %s", decoded.MachineType, spec.MachineType)
	}
}

func TestGCPConfig_JSON(t *testing.T) {
	t.Parallel()

	config := &GCPConfig{
		Project:           "my-project",
		Region:            "us-central1",
		Zone:              "us-central1-a",
		CredentialsFile:   "/path/to/creds.json",
		StateDir:          "/var/lib/terraform",
		SSHKeyPath:        "/home/user/.ssh/id_rsa",
		SSHUser:           "admin",
		AllowedIPRanges:   []string{"10.0.0.0/8"},
		BuilderPort:       9090,
		ServerCallbackURL: "http://server:8080",
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Failed to marshal config: %v", err)
	}

	var decoded GCPConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal config: %v", err)
	}

	if decoded.Project != "my-project" {
		t.Errorf("Project = %s, want my-project", decoded.Project)
	}
	if decoded.BuilderPort != 9090 {
		t.Errorf("BuilderPort = %d, want 9090", decoded.BuilderPort)
	}
	if len(decoded.AllowedIPRanges) != 1 || decoded.AllowedIPRanges[0] != "10.0.0.0/8" {
		t.Errorf("AllowedIPRanges = %v", decoded.AllowedIPRanges)
	}
}

func TestInstanceOutput_JSON(t *testing.T) {
	t.Parallel()

	output := &InstanceOutput{
		InstanceName: "test-instance",
		IPAddress:    "35.192.0.1",
		PrivateIP:    "10.128.0.2",
		Zone:         "us-central1-a",
		MachineType:  "n1-standard-4",
	}

	data, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("Failed to marshal output: %v", err)
	}

	var decoded InstanceOutput
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal output: %v", err)
	}

	if decoded.InstanceName != "test-instance" {
		t.Errorf("InstanceName = %s, want test-instance", decoded.InstanceName)
	}
	if decoded.IPAddress != "35.192.0.1" {
		t.Errorf("IPAddress = %s, want 35.192.0.1", decoded.IPAddress)
	}
}

func TestGCPConfigFromServerConfig(t *testing.T) {
	t.Parallel()

	config := GCPConfigFromServerConfig(
		"my-project",
		"europe-west1",
		"europe-west1-b",
		"/path/to/key.json",
		"/home/user/.ssh/id_rsa",
		"admin",
		"/var/lib/terraform",
		"http://server:8080",
		[]string{"10.0.0.0/8", "192.168.0.0/16"},
		9090,
	)

	if config.Project != "my-project" {
		t.Errorf("Project = %s, want my-project", config.Project)
	}
	if config.Region != "europe-west1" {
		t.Errorf("Region = %s, want europe-west1", config.Region)
	}
	if config.Zone != "europe-west1-b" {
		t.Errorf("Zone = %s, want europe-west1-b", config.Zone)
	}
	if config.CredentialsFile != "/path/to/key.json" {
		t.Errorf("CredentialsFile = %s", config.CredentialsFile)
	}
	if config.StateDir != "/var/lib/terraform" {
		t.Errorf("StateDir = %s", config.StateDir)
	}
	if config.BuilderPort != 9090 {
		t.Errorf("BuilderPort = %d, want 9090", config.BuilderPort)
	}
	if len(config.AllowedIPRanges) != 2 {
		t.Errorf("AllowedIPRanges length = %d, want 2", len(config.AllowedIPRanges))
	}
}

func TestGCPInstanceSpecFromServerConfig(t *testing.T) {
	t.Parallel()

	spec := GCPInstanceSpecFromServerConfig(
		"my-project",
		"us-east1",
		"us-east1-b",
		"n1-standard-8",
		"pd-balanced",
		"debian-11",
		"debian-cloud",
		"custom-network",
		"custom-subnet",
		200,
		true,
	)

	if spec.Project != "my-project" {
		t.Errorf("Project = %s", spec.Project)
	}
	if spec.Region != "us-east1" {
		t.Errorf("Region = %s", spec.Region)
	}
	if spec.Zone != "us-east1-b" {
		t.Errorf("Zone = %s", spec.Zone)
	}
	if spec.MachineType != "n1-standard-8" {
		t.Errorf("MachineType = %s", spec.MachineType)
	}
	if spec.DiskSizeGB != 200 {
		t.Errorf("DiskSizeGB = %d", spec.DiskSizeGB)
	}
	if spec.DiskType != "pd-balanced" {
		t.Errorf("DiskType = %s", spec.DiskType)
	}
	if spec.ImageFamily != "debian-11" {
		t.Errorf("ImageFamily = %s", spec.ImageFamily)
	}
	if spec.ImageProject != "debian-cloud" {
		t.Errorf("ImageProject = %s", spec.ImageProject)
	}
	if spec.Network != "custom-network" {
		t.Errorf("Network = %s", spec.Network)
	}
	if spec.Subnetwork != "custom-subnet" {
		t.Errorf("Subnetwork = %s", spec.Subnetwork)
	}
	if !spec.Preemptible {
		t.Error("Preemptible should be true")
	}
	if spec.CPUCount != 8 {
		t.Errorf("CPUCount = %d, want 8", spec.CPUCount)
	}
	if spec.MemoryMB != 30720 {
		t.Errorf("MemoryMB = %d, want 30720", spec.MemoryMB)
	}
}

func TestGCPInstanceSpecFromServerConfig_Defaults(t *testing.T) {
	t.Parallel()

	spec := GCPInstanceSpecFromServerConfig(
		"my-project",
		"us-central1",
		"us-central1-a",
		"", // empty machine type
		"", // empty disk type
		"", // empty image family
		"", // empty image project
		"", // empty network
		"", // empty subnetwork
		0,  // zero disk size
		false,
	)

	if spec.MachineType != "n1-standard-4" {
		t.Errorf("MachineType = %s, want n1-standard-4", spec.MachineType)
	}
	if spec.DiskSizeGB != 100 {
		t.Errorf("DiskSizeGB = %d, want 100", spec.DiskSizeGB)
	}
	if spec.DiskType != "pd-ssd" {
		t.Errorf("DiskType = %s, want pd-ssd", spec.DiskType)
	}
	if spec.ImageFamily != "ubuntu-2204-lts" {
		t.Errorf("ImageFamily = %s, want ubuntu-2204-lts", spec.ImageFamily)
	}
	if spec.ImageProject != "ubuntu-os-cloud" {
		t.Errorf("ImageProject = %s, want ubuntu-os-cloud", spec.ImageProject)
	}
	if spec.Network != "default" {
		t.Errorf("Network = %s, want default", spec.Network)
	}
}

func TestGCPConfigWithCredentials(t *testing.T) {
	t.Parallel()

	config := &GCPConfig{
		Project:           "my-project",
		Region:            "us-central1",
		Zone:              "us-central1-a",
		CredentialsFile:   "/path/to/key.json",
		CredentialsJSON:   "",
		StateDir:          "/tmp/terraform",
		BuilderPort:       9090,
		InstanceTTL:       60,
		AllowedIPRanges:   []string{"10.0.0.0/8"},
		ServerCallbackURL: "http://server:8080",
	}

	if config.CredentialsFile != "/path/to/key.json" {
		t.Errorf("CredentialsFile = %s", config.CredentialsFile)
	}
	if config.InstanceTTL != 60 {
		t.Errorf("InstanceTTL = %d, want 60", config.InstanceTTL)
	}
}

func TestGCPConfigWithInlineCredentials(t *testing.T) {
	t.Parallel()

	jsonCreds := `{"type":"service_account","project_id":"test"}`
	config := &GCPConfig{
		Project:         "my-project",
		Region:          "us-central1",
		Zone:            "us-central1-a",
		CredentialsJSON: jsonCreds,
		InstanceTTL:     30,
	}

	if config.CredentialsJSON != jsonCreds {
		t.Errorf("CredentialsJSON mismatch")
	}
	if config.InstanceTTL != 30 {
		t.Errorf("InstanceTTL = %d, want 30", config.InstanceTTL)
	}
}

func TestGCPProvisioner_GenerateMainTFWithCloudInit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	config := &GCPConfig{
		Project:     "test-project",
		Region:      "us-central1",
		Zone:        "us-central1-a",
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewGCPProvisioner(config)
	if err != nil {
		t.Fatalf("NewGCPProvisioner failed: %v", err)
	}

	spec := DefaultGCPInstanceSpec()
	spec.Project = "my-project"
	spec.Zone = "us-central1-b"
	spec.MachineType = "n1-standard-8"

	cloudInit := DefaultCloudInitConfig()
	cloudInit.BuilderPort = 9090
	cloudInit.ServerCallbackURL = "http://10.0.0.1:8080/callback"

	tf := provisioner.GenerateMainTFWithCloudInit(spec, "builder-001", cloudInit)

	// Verify basic terraform structure
	if !strings.Contains(tf, `provider "google"`) {
		t.Error("Missing google provider block")
	}
	if !strings.Contains(tf, "my-project") {
		t.Error("Missing project in terraform")
	}
	if !strings.Contains(tf, "us-central1-b") {
		t.Error("Missing zone in terraform")
	}
	if !strings.Contains(tf, "n1-standard-8") {
		t.Error("Missing machine type in terraform")
	}
	if !strings.Contains(tf, "builder-001") {
		t.Error("Missing instance name in terraform")
	}

	// Verify cloud-init metadata is included
	if !strings.Contains(tf, "metadata_startup_script") {
		t.Error("Missing metadata_startup_script in terraform")
	}

	// Verify native Gentoo bootstrap content is embedded.
	if !strings.Contains(tf, "NATIVE_JOB_POLICY=single-use") {
		t.Error("Missing native single-use policy in startup script")
	}
	if !strings.Contains(tf, "9090") {
		t.Error("Missing builder port in startup script")
	}

	// Finding #49: the heredoc must be Terraform-valid, not a shell-style quoted
	// heredoc (<<-'CLOUDINIT'), which is not valid HCL.
	if strings.Contains(tf, "<<-'CLOUDINIT'") || strings.Contains(tf, `<<-"CLOUDINIT"`) {
		t.Error("Generated TF uses shell-style quoted heredoc, which is invalid HCL")
	}
	if !strings.Contains(tf, "<<-CLOUDINIT") {
		t.Error("Generated TF should use an unquoted Terraform heredoc")
	}
}

func TestGCPProvisioner_GenerateMainTFWithCloudInit_CustomConfig(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	config := &GCPConfig{
		Project:     "test-project",
		Region:      "us-central1",
		Zone:        "us-central1-a",
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewGCPProvisioner(config)
	if err != nil {
		t.Fatalf("NewGCPProvisioner failed: %v", err)
	}

	spec := DefaultGCPInstanceSpec()
	cloudInit := &CloudInitConfig{
		PortageMirror:     "https://distfiles.gentoo.org",
		PortageBinpkgHost: "http://binhost.invalid/binpkgs",
		BuilderPort:       8888,
		ServerCallbackURL: "http://custom.server:8080",
	}

	tf := provisioner.GenerateMainTFWithCloudInit(spec, "custom-builder", cloudInit)

	// Verify custom settings are included
	if !strings.Contains(tf, "http://binhost.invalid/binpkgs") {
		t.Error("Missing custom native binhost")
	}
	if !strings.Contains(tf, "8888") {
		t.Error("Missing custom builder port")
	}
	if !strings.Contains(tf, "http://custom.server:8080") {
		t.Error("Missing custom server callback URL")
	}
	if strings.Contains(tf, "docker run") {
		t.Error("Removed Docker backend leaked into GCP startup script")
	}
}

func TestGCPProvisioner_GenerateMainTFWithCloudInit_NilCloudInit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	config := &GCPConfig{
		Project:     "test-project",
		Region:      "us-central1",
		Zone:        "us-central1-a",
		StateDir:    tmpDir,
		BuilderPort: 9090,
	}

	provisioner, err := NewGCPProvisioner(config)
	if err != nil {
		t.Fatalf("NewGCPProvisioner failed: %v", err)
	}

	spec := DefaultGCPInstanceSpec()
	tf := provisioner.GenerateMainTFWithCloudInit(spec, "test-instance", nil)

	// Should still generate valid terraform with default cloud-init
	if !strings.Contains(tf, `provider "google"`) {
		t.Error("Missing google provider block")
	}
	if !strings.Contains(tf, "test-instance") {
		t.Error("Missing instance name")
	}
	if !strings.Contains(tf, "metadata_startup_script") {
		t.Error("Missing startup script even with nil cloud init")
	}
}
