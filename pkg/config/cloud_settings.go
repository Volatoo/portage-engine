package config

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// CloudSettings is the runtime-adjustable subset of ServerConfig that drives
// on-demand cloud builders. PostgreSQL-enabled servers persist only redacted,
// versioned values plus secret references; credential values stay in process
// environment. Database-disabled standalone mode retains the legacy local
// JSON override for compatibility.
type CloudSettings struct {
	Provider string `json:"provider"` // gcp | aws | pve

	// Static remote builders (dispatch targets). Empty = provision on demand.
	RemoteBuilders []string `json:"remote_builders,omitempty"`

	// GCP
	GCPProject string `json:"gcp_project"`
	GCPRegion  string `json:"gcp_region"`
	GCPZone    string `json:"gcp_zone"`
	GCPKeyFile string `json:"gcp_key_file"`

	// AWS
	AWSRegion    string `json:"aws_region"`
	AWSZone      string `json:"aws_zone"`
	AWSAccessKey string `json:"aws_access_key"`
	AWSSecretKey string `json:"aws_secret_key,omitempty"`

	// PVE (Proxmox VE)
	PVEEndpoint    string   `json:"pve_endpoint"`
	PVENode        string   `json:"pve_node"` // node name, or "auto"
	PVENodes       []string `json:"pve_nodes,omitempty"`
	PVETokenID     string   `json:"pve_token_id"`
	PVETokenSecret string   `json:"pve_token_secret,omitempty"`
	PVEUsername    string   `json:"pve_username"`
	PVEPassword    string   `json:"pve_password,omitempty"`
	PVEInsecure    bool     `json:"pve_insecure"`
	PVEStorage     string   `json:"pve_storage"`
	PVENetwork     string   `json:"pve_network"`
	PVETemplate    string   `json:"pve_template"`
	PVECICustom    string   `json:"pve_cicustom"`
	// PVENameserver is pushed to build VMs via cloud-init so internal domains
	// (registry/mirror hosts) resolve; DHCP-provided DNS often lacks the zone.
	PVENameserver string `json:"pve_nameserver"`
	// PVEIPConfig and PVEGateway are operator-owned defaults for disposable
	// build VMs. Keep them out of client-controlled catalog machine specs so a
	// profile cannot silently claim a fixed address.
	PVEIPConfig string `json:"pve_ip_config"` // empty, "dhcp", or IPv4 CIDR
	PVEGateway  string `json:"pve_gateway"`   // required with a static CIDR

	// SSH deployment
	SSHKeyPath         string `json:"ssh_key_path"`
	SSHUser            string `json:"ssh_user"`
	SSHKnownHosts      string `json:"ssh_known_hosts,omitempty"`
	SSHInsecureHostKey bool   `json:"ssh_insecure_host_key"`

	// Mirror acceleration for build instances (all optional)
	GentooMirror   string `json:"gentoo_mirror"`
	PortageSyncURI string `json:"portage_sync_uri"`
	// PortageSyncMethod selects how build instances fetch the portage tree:
	// "webrsync" (snapshot tarball via GENTOO_MIRRORS, default) or "rsync"
	// (incremental via PortageSyncURI).
	PortageSyncMethod string `json:"portage_sync_method"`

	// MakeConfExtra is appended to the generated make.conf on build instances
	// (global USE, ACCEPT_LICENSE, FEATURES, ...).
	MakeConfExtra string `json:"make_conf_extra"`

	// BuildFeatures is appended to the native Gentoo root's make.conf FEATURES.
	BuildFeatures string `json:"build_features"`

	// BuildMode is retained for settings-file compatibility. New settings must
	// use "native-gentoo"; Docker is no longer a supported build backend.
	BuildMode string `json:"build_mode"`

	// Reachability / delivery
	ServerCallbackURL   string `json:"server_callback_url"`
	BuilderBinaryPath   string `json:"builder_binary_path,omitempty"`
	BuilderBinaryURL    string `json:"builder_binary_url,omitempty"`
	BuilderBinarySHA256 string `json:"builder_binary_sha256,omitempty"`

	InstanceTTLMinutes int `json:"instance_ttl_minutes"`

	// SkipVerifyInstall is retained only for decoding older settings files.
	// The server now requires install verification before publication and
	// normalizes this field to false.
	SkipVerifyInstall bool `json:"skip_verify_install"`

	// Artifact upload: when UploadURL is set, packages that already passed the
	// quarantine verification gate (plus the Packages index and signing
	// pubkey) are pushed to the internal mirror's artifact API.
	UploadURL      string `json:"upload_url"`
	UploadUser     string `json:"upload_user"`
	UploadPassword string `json:"upload_password,omitempty"`
	UploadDir      string `json:"upload_dir"`
}

// CloudSettingsFromServerConfig extracts the runtime-adjustable cloud settings
// from a loaded server configuration (the startup defaults).
func CloudSettingsFromServerConfig(cfg *ServerConfig) *CloudSettings {
	return &CloudSettings{
		Provider:            cfg.CloudProvider,
		RemoteBuilders:      cfg.RemoteBuilders,
		GCPProject:          cfg.CloudGCPProject,
		GCPRegion:           cfg.CloudGCPRegion,
		GCPZone:             cfg.CloudGCPZone,
		GCPKeyFile:          cfg.CloudGCPKeyFile,
		AWSRegion:           cfg.CloudAWSRegion,
		AWSZone:             cfg.CloudAWSZone,
		AWSAccessKey:        cfg.CloudAWSAccessKey,
		AWSSecretKey:        cfg.CloudAWSSecretKey,
		PVEEndpoint:         cfg.CloudPVEEndpoint,
		PVENode:             cfg.CloudPVENode,
		PVENodes:            cfg.CloudPVENodes,
		PVETokenID:          cfg.CloudPVETokenID,
		PVETokenSecret:      cfg.CloudPVETokenSecret,
		PVEUsername:         cfg.CloudPVEUsername,
		PVEPassword:         cfg.CloudPVEPassword,
		PVEInsecure:         cfg.CloudPVEInsecure,
		PVEStorage:          cfg.CloudPVEStorage,
		PVENetwork:          cfg.CloudPVENetwork,
		PVETemplate:         cfg.CloudPVETemplate,
		PVECICustom:         cfg.CloudPVECICustom,
		PVENameserver:       cfg.CloudPVENameserver,
		PVEIPConfig:         cfg.CloudPVEIPConfig,
		PVEGateway:          cfg.CloudPVEGateway,
		SSHKeyPath:          cfg.CloudSSHKeyPath,
		SSHUser:             cfg.CloudSSHUser,
		SSHKnownHosts:       cfg.CloudSSHKnownHosts,
		SSHInsecureHostKey:  cfg.CloudSSHInsecureHostKey,
		ServerCallbackURL:   cfg.ServerCallbackURL,
		BuilderBinaryPath:   cfg.CloudBuilderBinaryPath,
		BuilderBinaryURL:    cfg.CloudBuilderBinaryURL,
		BuilderBinarySHA256: cfg.CloudBuilderBinarySHA256,
		InstanceTTLMinutes:  cfg.CloudInstanceTTL,
		BuildMode:           "native-gentoo",
		UploadPassword:      os.Getenv("PORTAGE_UPLOAD_PASSWORD"),
	}
}

// ValidatePVEStaticNetwork validates the operator-owned build VM network
// default. A gateway is deliberately required for a static address: these VMs
// must reach the worker gateway and object store, and accepting a local-only
// address would turn a configuration typo into a late build timeout.
func (s *CloudSettings) ValidatePVEStaticNetwork() error {
	ipConfig := strings.TrimSpace(s.PVEIPConfig)
	gateway := strings.TrimSpace(s.PVEGateway)
	if ipConfig == "" || strings.EqualFold(ipConfig, "dhcp") {
		if gateway != "" {
			return fmt.Errorf("pve_gateway requires a static pve_ip_config IPv4 CIDR")
		}
		return nil
	}
	prefix, err := netip.ParsePrefix(ipConfig)
	if err != nil || !prefix.Addr().Is4() {
		return fmt.Errorf("pve_ip_config must be dhcp or an IPv4 CIDR")
	}
	if gateway == "" {
		return fmt.Errorf("pve_gateway is required with a static pve_ip_config")
	}
	gatewayAddr, err := netip.ParseAddr(gateway)
	if err != nil || !gatewayAddr.Is4() {
		return fmt.Errorf("pve_gateway must be an IPv4 address")
	}
	if !prefix.Contains(gatewayAddr) {
		return fmt.Errorf("pve_gateway must be inside the pve_ip_config subnet")
	}
	return nil
}

// Clone returns a deep copy (slices are the only reference fields).
func (s *CloudSettings) Clone() *CloudSettings {
	c := *s
	if s.PVENodes != nil {
		c.PVENodes = append([]string(nil), s.PVENodes...)
	}
	if s.RemoteBuilders != nil {
		c.RemoteBuilders = append([]string(nil), s.RemoteBuilders...)
	}
	return &c
}
