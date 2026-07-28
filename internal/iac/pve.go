// Package iac manages infrastructure provisioning using Terraform.
package iac

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PVEInstanceSpec defines the specification for a Proxmox VE instance.
type PVEInstanceSpec struct {
	Node         string   `json:"node"`      // PVE node name
	VMID         int      `json:"vmid"`      // VM ID (0 = auto-assign)
	Name         string   `json:"name"`      // VM name
	Cores        int      `json:"cores"`     // CPU cores
	Sockets      int      `json:"sockets"`   // CPU sockets
	MemoryMB     int      `json:"memory_mb"` // Memory in MB
	DiskSizeGB   int      `json:"disk_size_gb"`
	DiskType     string   `json:"disk_type"`   // scsi, virtio, ide
	Storage      string   `json:"storage"`     // Storage pool name
	Template     string   `json:"template"`    // Template/Clone source
	OSType       string   `json:"os_type"`     // l26 (Linux 2.6+), win10, etc.
	Network      string   `json:"network"`     // Network bridge (e.g., vmbr0)
	VLAN         int      `json:"vlan"`        // VLAN tag (0 = no VLAN)
	IPConfig     string   `json:"ip_config"`   // IP configuration (dhcp or static)
	Gateway      string   `json:"gateway"`     // Gateway for static IP
	Nameserver   string   `json:"nameserver"`  // DNS server
	SSHKeys      string   `json:"ssh_keys"`    // SSH public keys
	CICustom     string   `json:"cicustom"`    // Custom cloud-init snippet ref (e.g. vendor=local:snippets/vendor.yaml)
	SMBIOSUUID   string   `json:"smbios_uuid"` // Guest-readable, provider-owned identity
	Tags         []string `json:"tags"`        // PVE tags
	Pool         string   `json:"pool"`        // PVE resource pool
	StartOnBoot  bool     `json:"start_onboot"`
	StartStopped bool     `json:"start_stopped"`
	Agent        bool     `json:"agent"`      // Enable QEMU guest agent
	CloudInit    bool     `json:"cloud_init"` // Use cloud-init
	// Bios/Machine select firmware. Empty defaults to SeaBIOS/i440fx (Debian
	// cloud template). A Gentoo cloud-init QCOW2 needs "ovmf"/"q35" (UEFI); the
	// efidisk is inherited from the template on clone.
	Bios    string `json:"bios"`
	Machine string `json:"machine"`
}

// PVEConfig holds PVE-specific configuration for IaC.
type PVEConfig struct {
	Endpoint          string   `json:"endpoint"`          // PVE API endpoint (e.g., https://pve.example.com:8006)
	Node              string   `json:"node"`              // Default PVE node
	TokenID           string   `json:"token_id"`          // API token ID (user@realm!tokenname)
	TokenSecret       string   `json:"token_secret"`      // API token secret
	Username          string   `json:"username"`          // Alternative: username for password auth
	Password          string   `json:"password"`          // Alternative: password for password auth
	Insecure          bool     `json:"insecure"`          // Skip TLS verification
	StateDir          string   `json:"state_dir"`         // Terraform state directory
	SSHKeyPath        string   `json:"ssh_key_path"`      // SSH private key path
	SSHUser           string   `json:"ssh_user"`          // SSH user
	Storage           string   `json:"storage"`           // Default storage pool
	Network           string   `json:"network"`           // Default network bridge
	Template          string   `json:"template"`          // Default VM template
	Bios              string   `json:"bios"`              // ovmf for UEFI (Gentoo), empty=SeaBIOS
	Machine           string   `json:"machine"`           // q35 for UEFI, empty=i440fx
	AllowedIPRanges   []string `json:"allowed_ip_ranges"` // Allowed IP ranges for firewall
	BuilderPort       int      `json:"builder_port"`      // Builder service port
	ServerCallbackURL string   `json:"server_callback_url"`
	InstanceTTL       int      `json:"instance_ttl"` // TTL in minutes
}

// DefaultPVEInstanceSpec returns the default PVE instance specification.
//
// Template is intentionally empty: proxmox_vm_qemu clones a QEMU VM template by
// name, and there is no universally-present template to default to (the old
// default here was an LXC vztmpl path, which qemu clone can never use). The
// operator must supply one via CLOUD_PVE_TEMPLATE or machine_spec "template";
// see docs/PVE_TESTING.md for how to build a suitable template.
func DefaultPVEInstanceSpec() *PVEInstanceSpec {
	return &PVEInstanceSpec{
		Node:        "pve",
		VMID:        0, // Auto-assign
		Name:        "portage-builder",
		Cores:       4,
		Sockets:     1,
		MemoryMB:    8192,
		DiskSizeGB:  50,
		DiskType:    "scsi",
		Storage:     "local-lvm",
		Template:    "",
		OSType:      "l26",
		Network:     "vmbr0",
		VLAN:        0,
		IPConfig:    "dhcp",
		Tags:        []string{"portage-builder"},
		StartOnBoot: true,
		Agent:       true,
		CloudInit:   true,
	}
}

// PVEInstanceSpecFromMap creates a PVEInstanceSpec from a map.
func PVEInstanceSpecFromMap(m map[string]string) *PVEInstanceSpec {
	spec := DefaultPVEInstanceSpec()

	setStringField := func(key string, target *string) {
		if v, ok := m[key]; ok && v != "" {
			*target = v
		}
	}

	setIntField := func(key string, target *int) {
		if v, ok := m[key]; ok && v != "" {
			_, _ = fmt.Sscanf(v, "%d", target)
		}
	}

	setBoolField := func(key string, target *bool) {
		if v, ok := m[key]; ok {
			*target = v == "true" || v == "1"
		}
	}

	setStringField("node", &spec.Node)
	setIntField("vmid", &spec.VMID)
	setStringField("name", &spec.Name)
	setIntField("cores", &spec.Cores)
	setIntField("sockets", &spec.Sockets)
	setIntField("memory_mb", &spec.MemoryMB)
	setIntField("disk_size_gb", &spec.DiskSizeGB)
	setStringField("disk_type", &spec.DiskType)
	setStringField("storage", &spec.Storage)
	setStringField("template", &spec.Template)
	setStringField("os_type", &spec.OSType)
	setStringField("bios", &spec.Bios)
	setStringField("machine", &spec.Machine)
	setStringField("network", &spec.Network)
	setIntField("vlan", &spec.VLAN)
	setStringField("ip_config", &spec.IPConfig)
	setStringField("gateway", &spec.Gateway)
	setStringField("nameserver", &spec.Nameserver)
	setStringField("ssh_keys", &spec.SSHKeys)
	setStringField("cicustom", &spec.CICustom)
	setStringField("smbios_uuid", &spec.SMBIOSUUID)
	setStringField("pool", &spec.Pool)
	if value := strings.TrimSpace(m["tags"]); value != "" {
		spec.Tags = strings.FieldsFunc(value, func(r rune) bool {
			return r == ',' || r == ';'
		})
	}
	setBoolField("cloud_init", &spec.CloudInit)
	setBoolField("agent", &spec.Agent)
	setBoolField("start_stopped", &spec.StartStopped)

	return spec
}

// PVEProvisioner handles PVE-specific infrastructure provisioning.
type PVEProvisioner struct {
	config   *PVEConfig
	stateDir string
}

// NewPVEProvisioner creates a new PVE provisioner.
func NewPVEProvisioner(config *PVEConfig) (*PVEProvisioner, error) {
	if config == nil {
		return nil, fmt.Errorf("config is required")
	}

	if config.Endpoint == "" {
		return nil, fmt.Errorf("PVE endpoint is required")
	}

	stateDir := config.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(os.TempDir(), "portage-terraform", "pve")
	}

	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	return &PVEProvisioner{
		config:   config,
		stateDir: stateDir,
	}, nil
}

// GenerateMainTF generates the main.tf file for PVE.
func (p *PVEProvisioner) GenerateMainTF(spec *PVEInstanceSpec, instanceName string) string {
	if spec == nil {
		spec = DefaultPVEInstanceSpec()
	}

	// Override with config values if config has them set
	if p.config.Node != "" {
		spec.Node = p.config.Node
	}
	if p.config.Storage != "" {
		spec.Storage = p.config.Storage
	}
	if p.config.Network != "" {
		spec.Network = p.config.Network
	}
	if p.config.Template != "" {
		spec.Template = p.config.Template
	}
	if p.config.Bios != "" {
		spec.Bios = p.config.Bios
	}
	if p.config.Machine != "" {
		spec.Machine = p.config.Machine
	}

	tags := spec.Tags
	if len(tags) == 0 {
		tags = []string{"portage-builder"}
	}
	tagsStr := strings.Join(tags, ",")

	// Build network configuration
	networkConfig := p.generateNetworkConfig(spec)

	// Build disks configuration (telmate 3.x nested `disks` schema)
	disksConfig := p.generateDisksConfig(spec)

	// Build cloud-init configuration
	cloudInitConfig := ""
	firmwareBlock := ""
	smbiosBlock := ""
	if spec.SMBIOSUUID != "" {
		smbiosBlock = fmt.Sprintf(
			"\n  smbios1     = \"uuid=%s\"",
			strings.ToLower(spec.SMBIOSUUID),
		)
	}
	if spec.Bios != "" {
		firmwareBlock += fmt.Sprintf("\n  bios    = \"%s\"", spec.Bios)
	}
	if spec.Machine != "" {
		firmwareBlock += fmt.Sprintf("\n  machine = \"%s\"", spec.Machine)
	}
	osTypeBlock := ""
	if spec.CloudInit {
		cloudInitConfig = p.generateCloudInitConfig(spec, instanceName)
		// telmate's os_type selects the provisioning method; "cloud-init" makes
		// ipconfig0/ciuser/sshkeys take effect.
		osTypeBlock = `
  os_type = "cloud-init"`
	}

	// SSH keys block (telmate attribute is `sshkeys`, newline-separated)
	sshKeysBlock := ""
	if p.config.SSHKeyPath != "" {
		sshKeysBlock = fmt.Sprintf(`
  sshkeys = file("%s.pub")`, p.config.SSHKeyPath)
	} else if spec.SSHKeys != "" {
		sshKeysBlock = fmt.Sprintf(`
  sshkeys = <<-EOF
%s
EOF`, spec.SSHKeys)
	}

	// Pool configuration
	poolBlock := ""
	if spec.Pool != "" {
		poolBlock = fmt.Sprintf(`
  pool = "%s"`, spec.Pool)
	}

	// VMID configuration
	vmidBlock := ""
	if spec.VMID > 0 {
		vmidBlock = fmt.Sprintf(`
  vmid = %d`, spec.VMID)
	}
	powerStateBlock := ""
	if spec.StartStopped {
		powerStateBlock = `
  vm_state              = "stopped"
  define_connection_info = false`
	}

	// Determine auth method
	authBlock := p.generateAuthBlock()
	// Credential secrets are declared as variables (values supplied via
	// TF_VAR_*), never embedded as literals in this file.
	authVariables := p.generateAuthVariables()

	// telmate/proxmox has no stable 3.x release (all 3.x versions are release
	// candidates), and Terraform's "~>" constraint never matches pre-releases —
	// so the version must be pinned exactly. rc04 is the newest rc that works against live PVE 8 clusters: rc08 fatally mishandles the HA-state read of non-HA VMs, and 2.9.x cannot parse PVE 8 API responses (both verified live).
	return fmt.Sprintf(`# Generated by Portage Engine IaC
# Instance: %s
# Generated at: %s

terraform {
  required_version = ">= 1.0.0"

  required_providers {
    proxmox = {
      source  = "telmate/proxmox"
      version = "%s"
    }
  }

  backend "local" {
    path = "terraform.tfstate"
  }
}
%s
provider "proxmox" {
  pm_api_url      = "%s/api2/json"
  pm_tls_insecure = %t
%s
}

resource "proxmox_vm_qemu" "portage_builder" {
  name        = "%s"
  target_node = "%s"
%s%s
  cpu {
    cores   = %d
    sockets = %d
  }
  memory      = %d
  agent       = %d
  onboot      = %t%s%s
  scsihw      = "virtio-scsi-pci"%s%s

  clone       = "%s"
  full_clone  = true

%s

%s
%s
%s

  tags = "%s"

  lifecycle {
    ignore_changes = [
      network,
      disks,
    ]
  }
}

output "instance_name" {
  value = proxmox_vm_qemu.portage_builder.name
}

output "vmid" {
  value = proxmox_vm_qemu.portage_builder.vmid
}

output "ip_address" {
  value = proxmox_vm_qemu.portage_builder.default_ipv4_address
}

output "node" {
  value = proxmox_vm_qemu.portage_builder.target_node
}
`,
		instanceName,
		time.Now().Format(time.RFC3339),
		pveProviderVersion,
		authVariables,
		p.config.Endpoint,
		p.config.Insecure,
		authBlock,
		instanceName,
		spec.Node,
		vmidBlock,
		poolBlock,
		spec.Cores,
		spec.Sockets,
		spec.MemoryMB,
		boolToInt(spec.Agent),
		spec.StartOnBoot,
		powerStateBlock,
		smbiosBlock,
		osTypeBlock,
		firmwareBlock,
		spec.Template,
		disksConfig,
		networkConfig,
		cloudInitConfig,
		sshKeysBlock,
		tagsStr,
	)
}

// pveProviderVersion is the exact telmate/proxmox provider version pinned into
// generated configs. Must be an exact version (not "~>"): the registry carries
// no stable 3.x release, and Terraform constraints skip pre-release versions.
const pveProviderVersion = "3.0.2-rc04"

// generateDisksConfig emits the telmate 3.x nested `disks` block: the cloned
// system disk on the slot family selected by DiskType, plus the cloud-init
// drive matching the one inherited from the template.
func (p *PVEProvisioner) generateDisksConfig(spec *PVEInstanceSpec) string {
	family := spec.DiskType
	if family != "scsi" && family != "virtio" {
		family = "scsi"
	}

	cloudInitDisk := ""
	if spec.CloudInit {
		// Declare the inherited cloud-init drive so the provider keeps it —
		// undeclared, telmate issues delete:ide2 on the post-clone update and
		// the VM loses its cloud-init configuration (verified live).
		cloudInitDisk = fmt.Sprintf(`
    ide {
      ide2 {
        cloudinit {
          storage = "%s"
        }
      }
    }`, spec.Storage)
	}

	return fmt.Sprintf(`  disks {
    %s {
      %s0 {
        disk {
          storage = "%s"
          size    = "%dG"
        }
      }
    }%s
  }`, family, family, spec.Storage, spec.DiskSizeGB, cloudInitDisk)
}

// generateAuthBlock generates the authentication block for the provider.
//
// Secrets (token secret / password) are NOT embedded as literals in the
// generated HCL (finding #30). Instead they are referenced via Terraform
// variables and supplied out-of-band through TF_VAR_* environment variables (see
// generateAuthVariables and the env built in Manager.prepareEnvironment /
// PVEProvisioner.AuthEnv). Only the non-secret token ID / username is emitted
// inline, since it is not a credential on its own.
func (p *PVEProvisioner) generateAuthBlock() string {
	if p.config.TokenID != "" && p.config.TokenSecret != "" {
		return fmt.Sprintf(`  pm_api_token_id     = "%s"
  pm_api_token_secret = var.pve_token_secret`, p.config.TokenID)
	}
	if p.config.Username != "" && p.config.Password != "" {
		return fmt.Sprintf(`  pm_user     = "%s"
  pm_password = var.pve_password`, p.config.Username)
	}
	return ""
}

// generateAuthVariables declares the Terraform variables that carry PVE
// credentials so their values never appear in the generated main.tf. The values
// are provided at apply time via TF_VAR_pve_token_secret / TF_VAR_pve_password.
func (p *PVEProvisioner) generateAuthVariables() string {
	if p.config.TokenID != "" && p.config.TokenSecret != "" {
		return `
variable "pve_token_secret" {
  description = "Proxmox VE API token secret (supplied via TF_VAR_pve_token_secret)"
  type        = string
  sensitive   = true
}
`
	}
	if p.config.Username != "" && p.config.Password != "" {
		return `
variable "pve_password" {
  description = "Proxmox VE password (supplied via TF_VAR_pve_password)"
  type        = string
  sensitive   = true
}
`
	}
	return ""
}

// AuthEnv returns the TF_VAR_* environment entries supplying the PVE credential
// secrets referenced by the generated HCL, so they are passed to terraform
// without being written to disk.
func (p *PVEProvisioner) AuthEnv() []string {
	var env []string
	if p.config.TokenID != "" && p.config.TokenSecret != "" {
		env = append(env, "TF_VAR_pve_token_secret="+p.config.TokenSecret)
	} else if p.config.Username != "" && p.config.Password != "" {
		env = append(env, "TF_VAR_pve_password="+p.config.Password)
	}
	return env
}

// generateNetworkConfig generates the network configuration block.
func (p *PVEProvisioner) generateNetworkConfig(spec *PVEInstanceSpec) string {
	vlanTag := ""
	if spec.VLAN > 0 {
		vlanTag = fmt.Sprintf(`
    tag = %d`, spec.VLAN)
	}

	// telmate 3.x requires an explicit device id on network blocks.
	return fmt.Sprintf(`  network {
    id     = 0
    model  = "virtio"
    bridge = "%s"
    firewall = true%s
  }`, spec.Network, vlanTag)
}

// generateCloudInitConfig generates cloud-init configuration.
func (p *PVEProvisioner) generateCloudInitConfig(spec *PVEInstanceSpec, _ string) string {
	// PVE defaults ciupgrade to enabled on cloud-init VMs. On Gentoo that
	// translates into an unbounded `emerge --sync` during first boot, which is
	// both an implicit network dependency and a source of non-reproducibility.
	// Keep package updates under Portage Engine's locked build plan instead.
	ciupgradeBlock := `  ciupgrade = false`
	ipConfigBlock := ""
	if spec.IPConfig == "dhcp" {
		ipConfigBlock = `  ipconfig0 = "ip=dhcp"`
	} else if spec.IPConfig != "" {
		gateway := ""
		if spec.Gateway != "" {
			gateway = fmt.Sprintf(",gw=%s", spec.Gateway)
		}
		ipConfigBlock = fmt.Sprintf(`  ipconfig0 = "ip=%s%s"`, spec.IPConfig, gateway)
	}

	nameserverBlock := ""
	if spec.Nameserver != "" {
		nameserverBlock = fmt.Sprintf(`
  nameserver = "%s"`, spec.Nameserver)
	}

	ciuserBlock := ""
	if p.config.SSHUser != "" {
		ciuserBlock = fmt.Sprintf(`
  ciuser = "%s"`, p.config.SSHUser)
	}

	// Preserve the template's custom cloud-init snippet (e.g. one that installs
	// qemu-guest-agent on first boot). Without an explicit cicustom, telmate
	// clears the inherited value on the post-clone update and the agent never
	// comes up on cloud images that don't ship it.
	cicustomBlock := ""
	if spec.CICustom != "" {
		cicustomBlock = fmt.Sprintf(`
  cicustom = "%s"`, spec.CICustom)
	}

	return fmt.Sprintf(`%s
%s%s%s%s`, ciupgradeBlock, ipConfigBlock, nameserverBlock, ciuserBlock, cicustomBlock)
}

// GenerateVariablesTF generates variables.tf for PVE.
func (p *PVEProvisioner) GenerateVariablesTF(spec *PVEInstanceSpec) string {
	if spec == nil {
		spec = DefaultPVEInstanceSpec()
	}

	return fmt.Sprintf(`# Variables for Portage Builder PVE Infrastructure

variable "pve_endpoint" {
  description = "Proxmox VE API endpoint"
  type        = string
  default     = "%s"
}

variable "pve_node" {
  description = "Proxmox VE node name"
  type        = string
  default     = "%s"
}

variable "vm_cores" {
  description = "Number of CPU cores"
  type        = number
  default     = %d
}

variable "vm_memory" {
  description = "Memory in MB"
  type        = number
  default     = %d
}

variable "vm_disk_size" {
  description = "Disk size in GB"
  type        = number
  default     = %d
}

variable "vm_storage" {
  description = "Storage pool name"
  type        = string
  default     = "%s"
}

variable "vm_network" {
  description = "Network bridge"
  type        = string
  default     = "%s"
}
`,
		p.config.Endpoint,
		spec.Node,
		spec.Cores,
		spec.MemoryMB,
		spec.DiskSizeGB,
		spec.Storage,
		spec.Network,
	)
}

// boolToInt converts a boolean to an integer (0 or 1).
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
