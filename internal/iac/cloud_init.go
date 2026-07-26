// Package iac manages infrastructure provisioning using Terraform.
package iac

import (
	"strings"
)

// CloudInitConfig holds configuration for cloud instance initialization.
type CloudInitConfig struct {
	// Portage configuration
	PortageTreeSync bool   `json:"portage_tree_sync"`
	PortageMirror   string `json:"portage_mirror"`
	// PortageSyncURI overrides the gentoo repo sync-uri (rsync/git).
	PortageSyncURI    string `json:"portage_sync_uri"`
	PortageSyncMethod string `json:"portage_sync_method"`
	// MakeConfExtra is appended verbatim to the generated make.conf (dashboard
	// "build config" box: USE, ACCEPT_LICENSE, FEATURES, ...).
	MakeConfExtra     string `json:"make_conf_extra"`
	PortageBinpkgHost string `json:"portage_binpkg_host"`

	// Builder service configuration
	BuilderBinaryURL    string `json:"builder_binary_url"`
	BuilderBinarySHA256 string `json:"builder_binary_sha256"`
	BuilderPort         int    `json:"builder_port"`
	BuilderToken        string `json:"builder_token"`
	ServerCallbackURL   string `json:"server_callback_url"`
	InstanceID          string `json:"instance_id"`
	Architecture        string `json:"architecture"`

	// Data directories
	DataDir     string `json:"data_dir"`
	WorkDir     string `json:"work_dir"`
	ArtifactDir string `json:"artifact_dir"`

	// System configuration
	SwapSizeGB     int  `json:"swap_size_gb"`
	EnableFirewall bool `json:"enable_firewall"`

	// Extra packages to install
	ExtraPackages []string `json:"extra_packages"`
	// BuildFeatures is appended to the native Gentoo root's FEATURES.
	BuildFeatures string
}

// DefaultCloudInitConfig returns the default cloud initialization configuration.
func DefaultCloudInitConfig() *CloudInitConfig {
	return &CloudInitConfig{
		PortageTreeSync:     true,
		PortageMirror:       "https://distfiles.gentoo.org",
		PortageBinpkgHost:   "",
		BuilderBinaryURL:    "",
		BuilderBinarySHA256: "",
		BuilderPort:         9090,
		ServerCallbackURL:   "",
		InstanceID:          "",
		Architecture:        "amd64",
		DataDir:             "/var/lib/portage-engine",
		WorkDir:             "/var/tmp/portage-builds",
		ArtifactDir:         "/var/tmp/portage-artifacts",
		SwapSizeGB:          4,
		EnableFirewall:      true,
		ExtraPackages:       []string{},
	}
}

// shellSingleQuote wraps s in single quotes for safe use as a shell word,
// escaping any embedded single quotes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// heredocEscape escapes $, backtick, and backslash so a value embedded in an
// UNQUOTED heredoc lands literally instead of being shell-expanded.
func heredocEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "`", "\\`", `$`, `\$`)
	return r.Replace(s)
}

// GenerateCloudInitScript is retained for provider compatibility and now
// always emits the native Gentoo bootstrap. Docker builders were removed.
func GenerateCloudInitScript(config *CloudInitConfig) string {
	if config == nil {
		config = DefaultCloudInitConfig()
	}
	return GenerateGentooNativeScript(config)
}

// GenerateStartupScript returns the native bootstrap for providers that use a
// startup-script metadata field.
func GenerateStartupScript(config *CloudInitConfig) string {
	return GenerateCloudInitScript(config)
}

// GenerateUserData wraps the native bootstrap as cloud-config user data.
func GenerateUserData(config *CloudInitConfig) string {
	return "#cloud-config\nruncmd:\n  - |\n" + indentScript(GenerateCloudInitScript(config), "      ")
}

func indentScript(script, indent string) string {
	lines := strings.Split(script, "\n")
	for index := range lines {
		lines[index] = indent + lines[index]
	}
	return strings.Join(lines, "\n")
}
