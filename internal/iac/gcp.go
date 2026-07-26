// Package iac manages infrastructure provisioning using Terraform.
package iac

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GCPInstanceSpec defines the specification for a GCP instance.
type GCPInstanceSpec struct {
	Project      string   `json:"project"`
	Region       string   `json:"region"`
	Zone         string   `json:"zone"`
	MachineType  string   `json:"machine_type"`
	CPUCount     int      `json:"cpu_count"`
	MemoryMB     int      `json:"memory_mb"`
	DiskSizeGB   int      `json:"disk_size_gb"`
	DiskType     string   `json:"disk_type"`
	ImageProject string   `json:"image_project"`
	ImageFamily  string   `json:"image_family"`
	Network      string   `json:"network"`
	Subnetwork   string   `json:"subnetwork"`
	Preemptible  bool     `json:"preemptible"`
	Tags         []string `json:"tags"`
}

// GCPConfig holds GCP-specific configuration for IaC.
type GCPConfig struct {
	Project           string   `json:"project"`
	Region            string   `json:"region"`
	Zone              string   `json:"zone"`
	CredentialsFile   string   `json:"credentials_file"`
	CredentialsJSON   string   `json:"credentials_json"` // Inline JSON credentials
	StateDir          string   `json:"state_dir"`
	SSHKeyPath        string   `json:"ssh_key_path"`
	SSHUser           string   `json:"ssh_user"`
	AllowedIPRanges   []string `json:"allowed_ip_ranges"`
	BuilderPort       int      `json:"builder_port"`
	ServerCallbackURL string   `json:"server_callback_url"`
	InstanceTTL       int      `json:"instance_ttl"` // TTL in minutes, 0 means no auto-termination
}

// DefaultGCPInstanceSpec returns the default GCP instance specification.
func DefaultGCPInstanceSpec() *GCPInstanceSpec {
	return &GCPInstanceSpec{
		Project:      "portage-engine",
		Region:       "us-central1",
		Zone:         "us-central1-a",
		MachineType:  "n1-standard-4",
		CPUCount:     4,
		MemoryMB:     15360, // 15GB
		DiskSizeGB:   100,
		DiskType:     "pd-ssd",
		ImageProject: "ubuntu-os-cloud",
		ImageFamily:  "ubuntu-2204-lts",
		Network:      "default",
		Subnetwork:   "",
		Preemptible:  false,
		Tags:         []string{"portage-builder"},
	}
}

// GCPAvailableRegions returns a list of available GCP regions.
func GCPAvailableRegions() []string {
	return []string{
		"us-central1",
		"us-east1",
		"us-east4",
		"us-west1",
		"us-west2",
		"us-west3",
		"us-west4",
		"europe-west1",
		"europe-west2",
		"europe-west3",
		"europe-west4",
		"europe-west6",
		"europe-north1",
		"asia-east1",
		"asia-east2",
		"asia-northeast1",
		"asia-northeast2",
		"asia-northeast3",
		"asia-south1",
		"asia-southeast1",
		"asia-southeast2",
		"australia-southeast1",
		"southamerica-east1",
	}
}

// GCPAvailableZones returns available zones for a given region.
func GCPAvailableZones(region string) []string {
	zones := []string{"a", "b", "c", "f"}
	result := make([]string, 0, len(zones))
	for _, z := range zones {
		result = append(result, region+"-"+z)
	}
	return result
}

// GCPMachineTypes returns common GCP machine types.
func GCPMachineTypes() map[string]struct {
	CPUs     int
	MemoryMB int
} {
	return map[string]struct {
		CPUs     int
		MemoryMB int
	}{
		"n1-standard-1":  {1, 3840},
		"n1-standard-2":  {2, 7680},
		"n1-standard-4":  {4, 15360},
		"n1-standard-8":  {8, 30720},
		"n1-standard-16": {16, 61440},
		"n1-highmem-2":   {2, 13312},
		"n1-highmem-4":   {4, 26624},
		"n1-highmem-8":   {8, 53248},
		"n1-highcpu-2":   {2, 1843},
		"n1-highcpu-4":   {4, 3686},
		"n1-highcpu-8":   {8, 7373},
		"e2-micro":       {2, 1024},
		"e2-small":       {2, 2048},
		"e2-medium":      {2, 4096},
		"e2-standard-2":  {2, 8192},
		"e2-standard-4":  {4, 16384},
		"e2-standard-8":  {8, 32768},
		"c2-standard-4":  {4, 16384},
		"c2-standard-8":  {8, 32768},
		"c2-standard-16": {16, 65536},
	}
}

// GCPProvisioner handles GCP-specific infrastructure provisioning.
type GCPProvisioner struct {
	config   *GCPConfig
	stateDir string
}

// NewGCPProvisioner creates a new GCP provisioner.
func NewGCPProvisioner(config *GCPConfig) (*GCPProvisioner, error) {
	if config == nil {
		return nil, fmt.Errorf("config is required")
	}

	stateDir := config.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(os.TempDir(), "portage-terraform", "gcp")
	}

	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	return &GCPProvisioner{
		config:   config,
		stateDir: stateDir,
	}, nil
}

// GenerateMainTF generates the main.tf file for GCP.
func (p *GCPProvisioner) GenerateMainTF(spec *GCPInstanceSpec, instanceName string) string {
	if spec == nil {
		spec = DefaultGCPInstanceSpec()
	}

	// Override with config values if spec values are empty
	if spec.Project == "" {
		spec.Project = p.config.Project
	}
	if spec.Region == "" {
		spec.Region = p.config.Region
	}
	if spec.Zone == "" {
		spec.Zone = p.config.Zone
	}

	tags := spec.Tags
	if len(tags) == 0 {
		tags = []string{"portage-builder"}
	}
	tags = append(tags, fmt.Sprintf("allow-builder-%d", p.config.BuilderPort))

	tagsStr := `["` + strings.Join(tags, `", "`) + `"]`

	sshKeyBlock := ""
	if p.config.SSHKeyPath != "" {
		sshKeyBlock = fmt.Sprintf(`
  metadata = {
    ssh-keys = "%s:${file("%s")}"
  }`, p.config.SSHUser, p.config.SSHKeyPath+".pub")
	}

	networkBlock := fmt.Sprintf(`
  network_interface {
    network = "%s"`, spec.Network)

	if spec.Subnetwork != "" {
		networkBlock += fmt.Sprintf(`
    subnetwork = "%s"`, spec.Subnetwork)
	}

	networkBlock += `
    access_config {
      // Ephemeral public IP
    }
  }`

	preemptibleBlock := ""
	if spec.Preemptible {
		preemptibleBlock = `
  scheduling {
    preemptible       = true
    automatic_restart = false
  }`
	}

	return fmt.Sprintf(`# Generated by Portage Engine IaC
# Instance: %s
# Generated at: %s

terraform {
  required_version = ">= 1.0.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 5.0"
    }
  }

  backend "local" {
    path = "terraform.tfstate"
  }
}

provider "google" {
  project = "%s"
  region  = "%s"
  zone    = "%s"
}

resource "google_compute_instance" "portage_builder" {
  name         = "%s"
  machine_type = "%s"
  zone         = "%s"

  boot_disk {
    initialize_params {
      image = "%s/%s"
      size  = %d
      type  = "%s"
    }
  }
%s
%s
  tags = %s
%s
  labels = {
    purpose = "portage-builder"
    managed = "terraform"
  }

  metadata_startup_script = <<-EOF
    #!/bin/bash
    echo "Portage Builder instance started"
  EOF
}

output "instance_name" {
  value = google_compute_instance.portage_builder.name
}

output "ip_address" {
  value = google_compute_instance.portage_builder.network_interface[0].access_config[0].nat_ip
}

output "private_ip" {
  value = google_compute_instance.portage_builder.network_interface[0].network_ip
}

output "zone" {
  value = google_compute_instance.portage_builder.zone
}

output "machine_type" {
  value = google_compute_instance.portage_builder.machine_type
}
`,
		instanceName,
		time.Now().Format(time.RFC3339),
		spec.Project,
		spec.Region,
		spec.Zone,
		instanceName,
		spec.MachineType,
		spec.Zone,
		spec.ImageProject,
		spec.ImageFamily,
		spec.DiskSizeGB,
		spec.DiskType,
		networkBlock,
		preemptibleBlock,
		tagsStr,
		sshKeyBlock,
	)
}

// GenerateFirewallTF generates firewall rules for GCP.
func (p *GCPProvisioner) GenerateFirewallTF(instanceName string) string {
	allowedIPs := p.config.AllowedIPRanges
	if len(allowedIPs) == 0 {
		allowedIPs = []string{"0.0.0.0/0"}
	}

	sourceRanges := `["` + strings.Join(allowedIPs, `", "`) + `"]`

	return fmt.Sprintf(`# Firewall rules for Portage Builder
# Instance: %s

resource "google_compute_firewall" "portage_ssh_%s" {
  name    = "portage-ssh-%s"
  network = "default"

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }

  source_ranges = ["0.0.0.0/0"]
  target_tags   = ["portage-builder"]

  description = "Allow SSH access to Portage Builder instances"
}

resource "google_compute_firewall" "portage_builder_%s" {
  name    = "portage-builder-%s"
  network = "default"

  allow {
    protocol = "tcp"
    ports    = ["%d"]
  }

  source_ranges = %s
  target_tags   = ["allow-builder-%d"]

  description = "Allow builder API access to Portage Builder instances"
}

resource "google_compute_firewall" "portage_icmp_%s" {
  name    = "portage-icmp-%s"
  network = "default"

  allow {
    protocol = "icmp"
  }

  source_ranges = ["0.0.0.0/0"]
  target_tags   = ["portage-builder"]

  description = "Allow ICMP for health checks"
}
`,
		instanceName,
		instanceName, instanceName,
		instanceName, instanceName, p.config.BuilderPort, sourceRanges, p.config.BuilderPort,
		instanceName, instanceName,
	)
}

// GenerateVariablesTF generates variables.tf for GCP.
func (p *GCPProvisioner) GenerateVariablesTF(spec *GCPInstanceSpec) string {
	if spec == nil {
		spec = DefaultGCPInstanceSpec()
	}

	return fmt.Sprintf(`# Variables for Portage Builder GCP Infrastructure

variable "project" {
  description = "GCP Project ID"
  type        = string
  default     = "%s"
}

variable "region" {
  description = "GCP Region"
  type        = string
  default     = "%s"
}

variable "zone" {
  description = "GCP Zone"
  type        = string
  default     = "%s"
}

variable "machine_type" {
  description = "GCP Machine Type"
  type        = string
  default     = "%s"
}

variable "disk_size_gb" {
  description = "Boot disk size in GB"
  type        = number
  default     = %d
}

variable "disk_type" {
  description = "Boot disk type"
  type        = string
  default     = "%s"
}

variable "image_project" {
  description = "Image project"
  type        = string
  default     = "%s"
}

variable "image_family" {
  description = "Image family"
  type        = string
  default     = "%s"
}

variable "preemptible" {
  description = "Use preemptible instance"
  type        = bool
  default     = %t
}

variable "builder_port" {
  description = "Port for builder API"
  type        = number
  default     = %d
}
`,
		spec.Project,
		spec.Region,
		spec.Zone,
		spec.MachineType,
		spec.DiskSizeGB,
		spec.DiskType,
		spec.ImageProject,
		spec.ImageFamily,
		spec.Preemptible,
		p.config.BuilderPort,
	)
}

// InstanceOutput represents the output from a provisioned instance.
type InstanceOutput struct {
	InstanceName string `json:"instance_name"`
	IPAddress    string `json:"ip_address"`
	PrivateIP    string `json:"private_ip"`
	Zone         string `json:"zone"`
	MachineType  string `json:"machine_type"`
}

// ParseTerraformOutputs parses terraform output JSON.
func ParseTerraformOutputs(outputJSON []byte) (*InstanceOutput, error) {
	var outputs map[string]struct {
		Value string `json:"value"`
	}

	if err := json.Unmarshal(outputJSON, &outputs); err != nil {
		return nil, fmt.Errorf("failed to parse terraform outputs: %w", err)
	}

	return &InstanceOutput{
		InstanceName: outputs["instance_name"].Value,
		IPAddress:    outputs["ip_address"].Value,
		PrivateIP:    outputs["private_ip"].Value,
		Zone:         outputs["zone"].Value,
		MachineType:  outputs["machine_type"].Value,
	}, nil
}

// GenerateBuilderConfig generates configuration for a new builder node.
func GenerateBuilderConfig(output *InstanceOutput, builderPort int) string {
	return fmt.Sprintf(`# Builder Configuration
# Generated for instance: %s

BUILDER_PORT=%d
INSTANCE_ID=%s
ARCHITECTURE=amd64
NATIVE_JOB_POLICY=single-use
BUILD_WORK_DIR=/var/tmp/portage-builds
BUILD_ARTIFACT_DIR=/var/tmp/portage-artifacts
DATA_DIR=/var/lib/portage-engine
PERSISTENCE_ENABLED=true
RETENTION_DAYS=7
`,
		output.InstanceName,
		builderPort,
		output.InstanceName,
	)
}

// GenerateRemoteBuilderEntry generates the entry for server's REMOTE_BUILDERS config.
func GenerateRemoteBuilderEntry(output *InstanceOutput, port int) string {
	return fmt.Sprintf("%s:%d", output.IPAddress, port)
}

// GCPInstanceSpecFromMap creates a GCPInstanceSpec from a map.
func GCPInstanceSpecFromMap(m map[string]string) *GCPInstanceSpec {
	spec := DefaultGCPInstanceSpec()

	setStringField := func(key string, target *string) {
		if v, ok := m[key]; ok && v != "" {
			*target = v
		}
	}

	setIntField := func(key string, target *int) {
		if v, ok := m[key]; ok && v != "" {
			if n, err := parseInt(v); err == nil {
				*target = n
			}
		}
	}

	setBoolField := func(key string, target *bool) {
		if v, ok := m[key]; ok && v != "" {
			*target = v == "true" || v == "1" || v == "yes"
		}
	}

	setStringField("project", &spec.Project)
	setStringField("region", &spec.Region)
	setStringField("zone", &spec.Zone)
	setStringField("machine_type", &spec.MachineType)
	setIntField("cpu_count", &spec.CPUCount)
	setIntField("memory_mb", &spec.MemoryMB)
	setIntField("disk_size_gb", &spec.DiskSizeGB)
	setStringField("disk_type", &spec.DiskType)
	setStringField("image_project", &spec.ImageProject)
	setStringField("image_family", &spec.ImageFamily)
	setStringField("network", &spec.Network)
	setStringField("subnetwork", &spec.Subnetwork)
	setBoolField("preemptible", &spec.Preemptible)

	return spec
}

// parseInt parses an integer from a string.
func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// ValidateGCPSpec validates a GCP instance specification.
func ValidateGCPSpec(spec *GCPInstanceSpec) error {
	if spec.Project == "" {
		return fmt.Errorf("project is required")
	}
	if spec.Region == "" {
		return fmt.Errorf("region is required")
	}
	if spec.Zone == "" {
		return fmt.Errorf("zone is required")
	}
	if spec.MachineType == "" {
		return fmt.Errorf("machine_type is required")
	}
	if spec.DiskSizeGB < 10 {
		return fmt.Errorf("disk_size_gb must be at least 10")
	}
	if spec.DiskSizeGB > 65536 {
		return fmt.Errorf("disk_size_gb must be at most 65536")
	}

	// Validate disk type
	validDiskTypes := map[string]bool{
		"pd-standard": true,
		"pd-ssd":      true,
		"pd-balanced": true,
		"pd-extreme":  true,
	}
	if !validDiskTypes[spec.DiskType] {
		return fmt.Errorf("invalid disk_type: %s", spec.DiskType)
	}

	return nil
}

// GCPConfigFromServerConfig creates GCPConfig from ServerConfig fields.
// This allows seamless integration with the existing configuration system.
func GCPConfigFromServerConfig(project, region, zone, keyFile, sshKeyPath, sshUser, stateDir, callbackURL string,
	allowedIPs []string, builderPort int) *GCPConfig {
	return &GCPConfig{
		Project:           project,
		Region:            region,
		Zone:              zone,
		CredentialsFile:   keyFile,
		StateDir:          stateDir,
		SSHKeyPath:        sshKeyPath,
		SSHUser:           sshUser,
		AllowedIPRanges:   allowedIPs,
		BuilderPort:       builderPort,
		ServerCallbackURL: callbackURL,
	}
}

// GCPInstanceSpecFromServerConfig creates GCPInstanceSpec from ServerConfig fields.
func GCPInstanceSpecFromServerConfig(project, region, zone, machineType, diskType, imageFamily, imageProject, network, subnetwork string,
	diskSizeGB int, preemptible bool) *GCPInstanceSpec {
	spec := &GCPInstanceSpec{
		Project:      project,
		Region:       region,
		Zone:         zone,
		MachineType:  machineType,
		DiskSizeGB:   diskSizeGB,
		DiskType:     diskType,
		ImageProject: imageProject,
		ImageFamily:  imageFamily,
		Network:      network,
		Subnetwork:   subnetwork,
		Preemptible:  preemptible,
		Tags:         []string{"portage-builder"},
	}

	// Apply defaults for empty values
	if spec.MachineType == "" {
		spec.MachineType = "n1-standard-4"
	}
	if spec.DiskSizeGB == 0 {
		spec.DiskSizeGB = 100
	}
	if spec.DiskType == "" {
		spec.DiskType = "pd-ssd"
	}
	if spec.ImageFamily == "" {
		spec.ImageFamily = "ubuntu-2204-lts"
	}
	if spec.ImageProject == "" {
		spec.ImageProject = "ubuntu-os-cloud"
	}
	if spec.Network == "" {
		spec.Network = "default"
	}

	// Get CPU/memory from machine type
	if types := GCPMachineTypes(); types != nil {
		if mt, ok := types[spec.MachineType]; ok {
			spec.CPUCount = mt.CPUs
			spec.MemoryMB = mt.MemoryMB
		}
	}

	return spec
}

// GenerateMainTFWithCloudInit generates main.tf with cloud-init startup script.
func (p *GCPProvisioner) GenerateMainTFWithCloudInit(spec *GCPInstanceSpec, instanceName string, initConfig *CloudInitConfig) string {
	if spec == nil {
		spec = DefaultGCPInstanceSpec()
	}

	// Override with config values if spec values are empty
	if spec.Project == "" {
		spec.Project = p.config.Project
	}
	if spec.Region == "" {
		spec.Region = p.config.Region
	}
	if spec.Zone == "" {
		spec.Zone = p.config.Zone
	}

	tags := spec.Tags
	if len(tags) == 0 {
		tags = []string{"portage-builder"}
	}
	tags = append(tags, fmt.Sprintf("allow-builder-%d", p.config.BuilderPort))

	tagsStr := `["` + strings.Join(tags, `", "`) + `"]`

	sshKeyBlock := ""
	if p.config.SSHKeyPath != "" {
		sshKeyBlock = fmt.Sprintf(`
  metadata = {
    ssh-keys = "%s:${file("%s")}"
  }`, p.config.SSHUser, p.config.SSHKeyPath+".pub")
	}

	networkBlock := fmt.Sprintf(`
  network_interface {
    network = "%s"`, spec.Network)

	if spec.Subnetwork != "" {
		networkBlock += fmt.Sprintf(`
    subnetwork = "%s"`, spec.Subnetwork)
	}

	networkBlock += `
    access_config {
      // Ephemeral public IP
    }
  }`

	preemptibleBlock := ""
	if spec.Preemptible {
		preemptibleBlock = `
  scheduling {
    preemptible       = true
    automatic_restart = false
  }`
	}

	// Generate cloud-init script
	if initConfig == nil {
		initConfig = DefaultCloudInitConfig()
	}
	// Only override if not already set
	if initConfig.BuilderPort == 0 {
		initConfig.BuilderPort = p.config.BuilderPort
	}
	if initConfig.ServerCallbackURL == "" {
		initConfig.ServerCallbackURL = p.config.ServerCallbackURL
	}
	initConfig.InstanceID = instanceName

	startupScript := GenerateCloudInitScript(initConfig)
	// Escape the script for a Terraform (unquoted) heredoc: Terraform interprets
	// ${ and %{ as interpolation/template sequences, so they must be escaped to
	// $${ and %%{ respectively. Terraform has no shell-style quoted-heredoc
	// (<<-'DELIM') syntax, so we must escape rather than rely on quoting.
	escapedScript := strings.ReplaceAll(startupScript, "${", "$${")
	escapedScript = strings.ReplaceAll(escapedScript, "%{", "%%{")

	return fmt.Sprintf(`# Generated by Portage Engine IaC
# Instance: %s
# Generated at: %s
# Cloud-Init enabled: true

terraform {
  required_version = ">= 1.0.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 5.0"
    }
  }

  backend "local" {
    path = "terraform.tfstate"
  }
}

provider "google" {
  project = "%s"
  region  = "%s"
  zone    = "%s"
}

resource "google_compute_instance" "portage_builder" {
  name         = "%s"
  machine_type = "%s"
  zone         = "%s"

  boot_disk {
    initialize_params {
      image = "%s/%s"
      size  = %d
      type  = "%s"
    }
  }
%s
%s
  tags = %s
%s
  labels = {
    purpose = "portage-builder"
    managed = "terraform"
  }

  metadata_startup_script = <<-CLOUDINIT
%s
  CLOUDINIT
}

output "instance_name" {
  value = google_compute_instance.portage_builder.name
}

output "ip_address" {
  value = google_compute_instance.portage_builder.network_interface[0].access_config[0].nat_ip
}

output "private_ip" {
  value = google_compute_instance.portage_builder.network_interface[0].network_ip
}

output "zone" {
  value = google_compute_instance.portage_builder.zone
}

output "machine_type" {
  value = google_compute_instance.portage_builder.machine_type
}
`,
		instanceName,
		time.Now().Format(time.RFC3339),
		spec.Project,
		spec.Region,
		spec.Zone,
		instanceName,
		spec.MachineType,
		spec.Zone,
		spec.ImageProject,
		spec.ImageFamily,
		spec.DiskSizeGB,
		spec.DiskType,
		networkBlock,
		preemptibleBlock,
		tagsStr,
		sshKeyBlock,
		escapedScript,
	)
}

// CloudInitConfigFromGCPConfig creates a CloudInitConfig from GCPConfig.
func CloudInitConfigFromGCPConfig(gcpConfig *GCPConfig) *CloudInitConfig {
	config := DefaultCloudInitConfig()
	config.BuilderPort = gcpConfig.BuilderPort
	config.ServerCallbackURL = gcpConfig.ServerCallbackURL
	return config
}
