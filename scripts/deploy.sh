#!/usr/bin/env bash
# Deploy Portage Engine to remote server
# Usage: ./scripts/deploy.sh [options]

set -e

# Configuration
REMOTE_HOST="${DEPLOY_HOST:-10.42.0.1}"
REMOTE_USER="${DEPLOY_USER:-chris}"
SERVICE_USER="${SERVICE_USER:-portage}"
INSTALL_DIR="/opt/portage-engine"
CONFIG_DIR="/etc/portage-engine"
LOG_DIR="/var/log/portage-engine"
DATA_DIR="/var/lib/portage-engine"
BINPKG_DIR="/var/cache/binpkgs"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check if we can connect to remote host
check_connection() {
    log_info "Checking connection to ${REMOTE_USER}@${REMOTE_HOST}..."
    if ! ssh -o ConnectTimeout=5 "${REMOTE_USER}@${REMOTE_HOST}" "echo 'Connection OK'" > /dev/null 2>&1; then
        log_error "Cannot connect to ${REMOTE_USER}@${REMOTE_HOST}"
        log_error "Please check SSH connection and try again"
        exit 1
    fi
    log_info "Connection established"
}

# Build binaries
build_binaries() {
    log_info "Building binaries for Linux amd64 with static linking..."
    export CGO_ENABLED=0
    export GOOS=linux
    export GOARCH=amd64
    make build
    log_info "Build complete"
}

# Create remote directories
create_remote_dirs() {
    log_info "Creating remote directories..."
    ssh "${REMOTE_USER}@${REMOTE_HOST}" "sudo bash -c '
        mkdir -p ${INSTALL_DIR}/{bin,configs}
        mkdir -p ${CONFIG_DIR}
        mkdir -p ${LOG_DIR}
        mkdir -p ${DATA_DIR}/{server,dashboard,builder}
        mkdir -p ${BINPKG_DIR}
        mkdir -p /var/tmp/portage-builds
        mkdir -p /var/tmp/portage-artifacts

        # Create portage user if not exists
        if ! id ${SERVICE_USER} >/dev/null 2>&1; then
            useradd -r -s /bin/bash -d ${DATA_DIR} -c \"Portage Engine Service User\" ${SERVICE_USER}
        fi

        # Set ownership
        chown -R ${SERVICE_USER}:${SERVICE_USER} ${LOG_DIR}
        chown -R ${SERVICE_USER}:${SERVICE_USER} ${DATA_DIR}
        chown -R ${SERVICE_USER}:${SERVICE_USER} ${BINPKG_DIR}
        chown -R ${SERVICE_USER}:${SERVICE_USER} /var/tmp/portage-builds
        chown -R ${SERVICE_USER}:${SERVICE_USER} /var/tmp/portage-artifacts
        chmod 755 ${INSTALL_DIR}
        chmod 755 ${CONFIG_DIR}
    '"
    log_info "Remote directories created"
}

# Upload binaries
upload_binaries() {
    log_info "Uploading binaries..."
    scp bin/portage-server bin/portage-dashboard bin/portage-builder \
        "${REMOTE_USER}@${REMOTE_HOST}:/tmp/"

    ssh "${REMOTE_USER}@${REMOTE_HOST}" "sudo bash -c '
        mv /tmp/portage-server ${INSTALL_DIR}/bin/
        mv /tmp/portage-dashboard ${INSTALL_DIR}/bin/
        mv /tmp/portage-builder ${INSTALL_DIR}/bin/
        chmod +x ${INSTALL_DIR}/bin/*
    '"
    log_info "Binaries uploaded"
}

# Upload configurations
upload_configs() {
    log_info "Uploading configurations..."
    scp configs/server.conf configs/dashboard.conf configs/builder.conf \
        "${REMOTE_USER}@${REMOTE_HOST}:/tmp/"

    ssh "${REMOTE_USER}@${REMOTE_HOST}" "sudo bash -c '
        mv /tmp/server.conf ${CONFIG_DIR}/
        mv /tmp/dashboard.conf ${CONFIG_DIR}/
        mv /tmp/builder.conf ${CONFIG_DIR}/
        chmod 644 ${CONFIG_DIR}/*.conf
    '"
    log_info "Configurations uploaded"
}

# Upload management script
upload_management_script() {
    log_info "Uploading management script..."
    scp scripts/portage-ctl.sh "${REMOTE_USER}@${REMOTE_HOST}:/tmp/"

    ssh "${REMOTE_USER}@${REMOTE_HOST}" "sudo bash -c '
        mv /tmp/portage-ctl.sh /usr/local/bin/portage-ctl
        chmod +x /usr/local/bin/portage-ctl
    '"
    log_info "Management script uploaded to /usr/local/bin/portage-ctl"
}

# Create systemd service files
create_systemd_services() {
    log_info "Creating systemd service files..."

    # Server service
    ssh "${REMOTE_USER}@${REMOTE_HOST}" "sudo bash -c 'cat > /etc/systemd/system/portage-server.service << \"EOF\"
[Unit]
Description=Portage Engine Server
After=network.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
WorkingDirectory=${DATA_DIR}/server
ExecStart=${INSTALL_DIR}/bin/portage-server -config ${CONFIG_DIR}/server.conf
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal

# Security settings
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${LOG_DIR} ${DATA_DIR} ${BINPKG_DIR}

[Install]
WantedBy=multi-user.target
EOF
'"

    # Dashboard service
    ssh "${REMOTE_USER}@${REMOTE_HOST}" "sudo bash -c 'cat > /etc/systemd/system/portage-dashboard.service << \"EOF\"
[Unit]
Description=Portage Engine Dashboard
After=network.target portage-server.service

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
WorkingDirectory=${DATA_DIR}/dashboard
ExecStart=${INSTALL_DIR}/bin/portage-dashboard -config ${CONFIG_DIR}/dashboard.conf
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal

# Security settings
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${LOG_DIR} ${DATA_DIR}

[Install]
WantedBy=multi-user.target
EOF
'"

    # Builder service
    # Native Portage writes job workspaces and PKGDIR under /var/tmp.
    ssh "${REMOTE_USER}@${REMOTE_HOST}" "sudo bash -c 'cat > /etc/systemd/system/portage-builder.service << \"EOF\"
[Unit]
Description=Portage Engine Builder
After=network.target portage-server.service

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
WorkingDirectory=${DATA_DIR}/builder
ExecStart=${INSTALL_DIR}/bin/portage-builder -config ${CONFIG_DIR}/builder.conf
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal

# Security settings
NoNewPrivileges=true
PrivateTmp=false
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${LOG_DIR} ${DATA_DIR} ${BINPKG_DIR} /var/tmp

[Install]
WantedBy=multi-user.target
EOF
'"

    log_info "Systemd service files created"
}

# Reload systemd and enable services
enable_services() {
    log_info "Enabling services..."
    ssh "${REMOTE_USER}@${REMOTE_HOST}" "sudo bash -c '
        systemctl daemon-reload
        systemctl enable portage-server.service
        systemctl enable portage-dashboard.service
        systemctl enable portage-builder.service
    '"
    log_info "Services enabled"
}

# Start services
start_services() {
    log_info "Starting services..."
    ssh "${REMOTE_USER}@${REMOTE_HOST}" "sudo bash -c '
        systemctl start portage-server.service
        sleep 2
        systemctl start portage-dashboard.service
        systemctl start portage-builder.service
    '"
    log_info "Services started"
}

# Check service status
check_status() {
    log_info "Checking service status..."
    ssh "${REMOTE_USER}@${REMOTE_HOST}" "sudo systemctl status portage-server.service portage-dashboard.service portage-builder.service --no-pager" || true
}

# Show service URLs
show_urls() {
    echo ""
    log_info "=========================================="
    log_info "Deployment Complete!"
    log_info "=========================================="
    echo ""
    log_info "Service URLs:"
    log_info "  Server:    http://${REMOTE_HOST}:8080"
    log_info "  Dashboard: http://${REMOTE_HOST}:3000"
    log_info "  Builder:   http://${REMOTE_HOST}:9090"
    echo ""
    log_info "Service Management:"
    log_info "  Status:  sudo systemctl status portage-{server,dashboard,builder}"
    log_info "  Start:   sudo systemctl start portage-{server,dashboard,builder}"
    log_info "  Stop:    sudo systemctl stop portage-{server,dashboard,builder}"
    log_info "  Restart: sudo systemctl restart portage-{server,dashboard,builder}"
    log_info "  Logs:    sudo journalctl -u portage-server -f"
    echo ""
}

# Main deployment flow
main() {
    log_info "Starting deployment to ${REMOTE_HOST}..."

    check_connection
    build_binaries
    create_remote_dirs
    upload_binaries
    upload_configs
    upload_management_script
    create_systemd_services
    enable_services
    start_services
    sleep 3
    check_status
    show_urls
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --host)
            REMOTE_HOST="$2"
            shift 2
            ;;
        --user)
            REMOTE_USER="$2"
            shift 2
            ;;
        --service-user)
            SERVICE_USER="$2"
            shift 2
            ;;
        --help)
            echo "Usage: $0 [options]"
            echo ""
            echo "Options:"
            echo "  --host HOST          Remote host (default: 10.42.0.1)"
            echo "  --user USER          SSH user (default: chris)"
            echo "  --service-user USER  Service user (default: portage)"
            echo "  --help               Show this help message"
            exit 0
            ;;
        *)
            log_error "Unknown option: $1"
            exit 1
            ;;
    esac
done

# Run deployment
main
