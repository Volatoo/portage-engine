package iac

import (
	"fmt"
	"strings"
)

// cloud_init_gentoo.go generates the deployment script for a NATIVE Gentoo VM
// (cloned from the Gentoo cloud-init template) — no Docker. It configures
// make.conf for the build and installs the builder in native mode. The node is
// an unsigned trust domain and never receives the binhost signing key.

// GenerateGentooNativeScript returns the bootstrap script for a native Gentoo
// build node. The builder binary is either staged by deployBuilder or fetched
// from BuilderBinaryURL with a required SHA-256 check.
func GenerateGentooNativeScript(config *CloudInitConfig) string {
	arch := config.Architecture
	if arch == "" {
		arch = "amd64"
	}
	var sb strings.Builder

	sb.WriteString(`#!/bin/bash
set -euo pipefail
log() { echo "[gentoo-native] $*"; }
export DEBIAN_FRONTEND=noninteractive

log "Configuring native Gentoo build node..."
mkdir -p /etc/portage-engine /var/log/portage-engine /var/tmp/portage-builds /var/tmp/portage-artifacts /var/lib/portage-engine
`)

	if config.BuilderBinaryURL != "" {
		fmt.Fprintf(&sb, `
log "Downloading builder binary..."
mkdir -p /opt/portage-builder
builder_tmp=$(mktemp /tmp/portage-builder.XXXXXX)
curl -fsSL -o "$builder_tmp" %s
printf '%%s  %%s\n' %s "$builder_tmp" | sha256sum -c -
install -m 0755 "$builder_tmp" /opt/portage-builder/portage-builder
rm -f "$builder_tmp"
log "Builder binary downloaded and verified"
`, shellSingleQuote(config.BuilderBinaryURL), shellSingleQuote(config.BuilderBinarySHA256))
	}

	// make.conf: mirror + binhost + build FEATURES. The template already sets
	// GENTOO_MIRRORS/profile; we append build-farm settings idempotently.
	binhost := ""
	if config.PortageBinpkgHost != "" {
		binhost = config.PortageBinpkgHost
	} else if config.ServerCallbackURL != "" {
		binhost = strings.TrimRight(config.ServerCallbackURL, "/") + "/binpkgs"
	}
	fmt.Fprintf(&sb, `
# --- Portage Engine build settings (idempotent) ---
sed -i '/# PE-BUILD-BEGIN/,/# PE-BUILD-END/d' /etc/portage/make.conf 2>/dev/null || true
cat >> /etc/portage/make.conf <<'MAKECONF'
# PE-BUILD-BEGIN
FEATURES="${FEATURES} buildpkg"
`)
	if config.PortageMirror != "" {
		fmt.Fprintf(&sb, "GENTOO_MIRRORS=%s\n", heredocEscape(config.PortageMirror))
	}
	if binhost != "" {
		fmt.Fprintf(&sb, "PORTAGE_BINHOST=%s\n", heredocEscape(binhost))
	}
	if config.MakeConfExtra != "" {
		sb.WriteString(heredocEscape(config.MakeConfExtra) + "\n")
	}
	sb.WriteString("# PE-BUILD-END\nMAKECONF\n")
	if config.PortageSyncURI != "" {
		syncType := "rsync"
		if strings.HasPrefix(config.PortageSyncURI, "http://") ||
			strings.HasPrefix(config.PortageSyncURI, "https://") ||
			strings.HasPrefix(config.PortageSyncURI, "git://") {
			syncType = "git"
		}
		fmt.Fprintf(&sb, `
mkdir -p /etc/portage/repos.conf
cat > /etc/portage/repos.conf/portage-engine.conf <<'REPOSCONF'
[DEFAULT]
main-repo = gentoo
[gentoo]
location = /var/db/repos/gentoo
sync-type = %s
sync-uri = %s
REPOSCONF
`, syncType, heredocEscape(config.PortageSyncURI))
	}

	// builder.conf (native-only mode and native Portage paths).
	tokenLine := ""
	if config.BuilderToken != "" {
		tokenLine = fmt.Sprintf("BUILDER_TOKEN=%s\n", heredocEscape(config.BuilderToken))
	}
	pullLines := ""
	if config.WorkerPullEnabled {
		pullLines = fmt.Sprintf(
			"WORKER_PULL_ENABLED=true\nWORKER_GATEWAY_URL=%s\n"+
				"WORKER_TLS_CERT=/etc/portage-engine/tls/worker.crt\n"+
				"WORKER_TLS_KEY=/etc/portage-engine/tls/worker.key\n"+
				"WORKER_TLS_CA=/etc/portage-engine/tls/ca.crt\n",
			heredocEscape(config.WorkerGatewayURL),
		)
	}
	fmt.Fprintf(&sb, `
log "Writing builder configuration (native mode)..."
INSTANCE_ID_VAL=$(hostname)
cat > /etc/portage-engine/builder.conf <<BUILDERCONF
BUILDER_PORT=%d
INSTANCE_ID=${INSTANCE_ID_VAL}
ARCHITECTURE=%s
NATIVE_JOB_POLICY=single-use
BUILD_WORK_DIR=/var/tmp/portage-builds
BUILD_ARTIFACT_DIR=/var/tmp/portage-artifacts
DATA_DIR=/var/lib/portage-engine
PERSISTENCE_ENABLED=true
RETENTION_DAYS=7
SERVER_URL=%s
%s%sPORTAGE_REPOS_PATH=/var/db/repos
PORTAGE_CONF_PATH=/etc/portage
MAKE_CONF_PATH=/etc/portage/make.conf
BUILDERCONF

`, config.BuilderPort, heredocEscape(arch), heredocEscape(config.ServerCallbackURL), tokenLine, pullLines)

	// systemd unit (no docker dependency).
	sb.WriteString(`log "Installing systemd service..."
cat > /etc/systemd/system/portage-builder.service <<'SERVICEUNIT'
[Unit]
Description=Portage Builder Service (native)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/portage-engine/builder.conf
ExecStart=/opt/portage-builder/portage-builder
Restart=always
RestartSec=10
StandardOutput=append:/var/log/portage-engine/builder.log
StandardError=append:/var/log/portage-engine/builder.log

[Install]
WantedBy=multi-user.target
SERVICEUNIT

systemctl daemon-reload
if [ -x /opt/portage-builder/portage-builder ]; then
    log "Starting builder service..."
    systemctl enable portage-builder
    systemctl restart portage-builder
    log "Native Gentoo builder started"
else
    log "ERROR: builder binary missing at /opt/portage-builder/portage-builder"
    exit 1
fi
`)
	return sb.String()
}
