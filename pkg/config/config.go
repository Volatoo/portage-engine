// Package config provides configuration management for Portage Engine.
package config

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// insecureJWTSecrets is a list of well-known insecure JWT secrets that must
// never be used in production.
var insecureJWTSecrets = []string{
	"change-me-in-production",
	"changeme",
	"secret",
	"jwt-secret",
	"your-secret-here",
	"",
}

// ServerConfig represents the server configuration.
type ServerConfig struct {
	Port                     int
	ControlPlaneID           string
	BinpkgPath               string
	MaxWorkers               int
	BuildMode                string
	StorageType              string
	StorageLocalDir          string
	StorageS3Bucket          string
	StorageS3Region          string
	StorageS3Prefix          string
	StorageHTTPBase          string
	GPGEnabled               bool
	GPGKeyID                 string
	GPGKeyPath               string
	GPGAutoCreate            bool   // Auto-create GPG key if not exists
	GPGKeyName               string // Name for auto-generated key
	GPGKeyEmail              string // Email for auto-generated key
	GPGHome                  string // Custom GNUPGHOME directory
	GPGPublicKeyPath         string // Path to export public key
	SignerWaitTimeoutSeconds int
	CloudProvider            string
	CloudAliyunRegion        string
	CloudAliyunZone          string
	CloudAliyunAK            string
	CloudAliyunSK            string
	CloudGCPProject          string
	CloudGCPRegion           string
	CloudGCPZone             string
	CloudGCPKeyFile          string
	CloudGCPMachineType      string
	CloudGCPDiskSizeGB       int
	CloudGCPDiskType         string
	CloudGCPImageFamily      string
	CloudGCPImageProject     string
	CloudGCPNetwork          string
	CloudGCPSubnetwork       string
	CloudGCPPreemptible      bool
	CloudGCPStateDir         string
	CloudGCPAllowedIPs       []string
	CloudInstanceTTL         int // Instance TTL in minutes, 0 means no auto-termination
	CloudAWSRegion           string
	CloudAWSZone             string
	CloudAWSAccessKey        string
	CloudAWSSecretKey        string
	// PVE (Proxmox VE) configuration
	CloudPVEEndpoint    string   // PVE API endpoint (e.g., https://pve.example.com:8006)
	CloudPVENode        string   // Default PVE node name
	CloudPVETokenID     string   // API token ID (user@realm!tokenname)
	CloudPVETokenSecret string   // API token secret
	CloudPVEUsername    string   // Alternative password-auth user
	CloudPVEPassword    string   // Alternative password-auth secret
	CloudPVEInsecure    bool     // Skip TLS verification
	CloudPVEStorage     string   // Default storage pool
	CloudPVENetwork     string   // Default network bridge
	CloudPVETemplate    string   // Default VM template
	CloudPVENodes       []string // Candidate nodes for automatic placement (CLOUD_PVE_NODE=auto)
	CloudPVEAllowedIPs  []string // Allowed IP ranges for firewall
	CloudSSHKeyPath     string
	CloudSSHUser        string
	// SSH host-key verification for freshly provisioned instances: a
	// known_hosts file for real verification, or an explicit opt-in to skip
	// verification (MITM risk — LAN/throwaway instances only). With neither,
	// first-connection SSH to a new instance fails closed.
	CloudSSHKnownHosts      string
	CloudSSHInsecureHostKey bool
	ServerCallbackURL       string
	// Builder binary delivery for cloud instances: a local linux binary scp'd
	// during deployment, or a URL the instance downloads from (path wins).
	CloudBuilderBinaryPath   string
	CloudBuilderBinaryURL    string
	CloudBuilderBinarySHA256 string
	RemoteBuilders           []string
	// Security settings
	APIKey                 string   // API key for authenticating requests (empty = auth disabled)
	BuilderToken           string   // Shared secret the server presents to remote builders (empty = no builder auth)
	CORSAllowedOrigins     []string // Allowed CORS origins (empty = allow all for backward compatibility)
	MaxRequestBodyBytes    int64    // Maximum request body size in bytes (0 = default 10MB)
	CatalogPath            string   // Server-owned profile/repository/image catalog JSON (empty = compatibility catalog)
	ImageFactoryStatusPath string   // Optional read-only image-factory milestone/evidence status JSON
	// Data persistence
	DataDir  string // Directory for persisting legacy server state (empty = /var/lib/portage-engine/server)
	Database DatabaseConfig
	Cache    CacheConfig

	MetricsEnabled  bool
	MetricsPort     string
	MetricsPassword string
}

// DatabaseConfig controls the PostgreSQL control-plane connection. When
// enabled, PostgreSQL is the authoritative job/scheduler/runtime store; the
// JSON store is only used by database-disabled standalone compatibility mode.
type DatabaseConfig struct {
	Enabled               bool
	Required              bool
	URL                   string
	Host                  string
	Port                  int
	Name                  string
	User                  string
	Password              string
	SSLMode               string
	MaxConns              int
	MinConns              int
	ConnectTimeoutSeconds int
	HealthTimeoutSeconds  int
}

// CacheConfig controls Redis-backed ephemeral coordination. PostgreSQL remains
// the correctness authority; Redis accelerates wakeups, presence, rate limits,
// and live event fan-out.
type CacheConfig struct {
	Enabled            bool
	Required           bool
	Host               string
	Port               int
	Password           string
	DB                 int
	TLSEnabled         bool
	KeyPrefix          string
	RateLimitPerMinute int
	RateLimitBurst     int
}

// Validate checks the server configuration for common misconfigurations.
func (c *ServerConfig) Validate() []string {
	var warnings []string

	if c.APIKey == "" {
		warnings = append(warnings, "SECURITY: API_KEY is not set — all API endpoints are unauthenticated")
	}
	if len(c.CORSAllowedOrigins) == 0 {
		warnings = append(warnings, "SECURITY: CORS_ALLOWED_ORIGINS is not set — defaulting to allow all origins (*)")
	}
	if c.Port <= 0 || c.Port > 65535 {
		warnings = append(warnings, fmt.Sprintf("CONFIG: SERVER_PORT %d is invalid, must be 1-65535", c.Port))
	}
	if c.MaxWorkers <= 0 {
		warnings = append(warnings, "CONFIG: MAX_WORKERS must be > 0")
	}
	if c.Database.Required && !c.Database.Enabled {
		warnings = append(warnings, "CONFIG: DATABASE_REQUIRED requires DATABASE_ENABLED")
	}
	if c.GPGEnabled && (!c.Database.Enabled || !c.Database.Required) {
		warnings = append(warnings, "CONFIG: GPG_ENABLED requires DATABASE_ENABLED=true and DATABASE_REQUIRED=true for the isolated signing queue")
	}
	if c.GPGEnabled && c.GPGPublicKeyPath == "" {
		warnings = append(warnings, "CONFIG: GPG_ENABLED requires GPG_PUBLIC_KEY_PATH for isolated signer public-key distribution")
	}
	if c.Database.Enabled && c.Database.URL == "" && c.Database.SSLMode == "disable" {
		warnings = append(warnings, "SECURITY: PGSSLMODE=disable sends database traffic without TLS; use only on a trusted private network")
	}
	if c.Database.Enabled && (c.Database.MaxConns <= 0 || c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns) {
		warnings = append(warnings, "CONFIG: database pool requires 0 <= DATABASE_MIN_CONNS <= DATABASE_MAX_CONNS")
	}
	if c.Cache.Required && !c.Cache.Enabled {
		warnings = append(warnings, "CONFIG: REDIS_REQUIRED requires REDIS_ENABLED")
	}
	if c.Cache.Enabled && (c.Cache.Port <= 0 || c.Cache.Port > 65535) {
		warnings = append(warnings, "CONFIG: REDIS_PORT must be in 1-65535")
	}
	if c.Cache.Enabled && c.Cache.Password == "" {
		warnings = append(warnings, "SECURITY: Redis has no password configured; bind it to a private network and enable authentication")
	}

	return warnings
}

// DashboardConfig represents the dashboard configuration.
type DashboardConfig struct {
	Port            int
	ServerURL       string
	ServerAPIKey    string // API key forwarded to the backend server (empty = none)
	AuthEnabled     bool
	JWTSecret       string
	AdminUser       string // Username accepted by the login handler
	AdminPassword   string // Password accepted by the login handler
	TokenTTLMinutes int    // Issued-token lifetime in minutes
	AllowAnonymous  bool
	MetricsEnabled  bool
	MetricsPort     string
	MetricsPassword string
}

// Validate checks the dashboard configuration for common misconfigurations.
// Returns an error if a critical security issue is found.
func (c *DashboardConfig) Validate() error {
	serverURL, err := url.Parse(c.ServerURL)
	if err != nil || (serverURL.Scheme != "http" && serverURL.Scheme != "https") ||
		serverURL.Host == "" || serverURL.User != nil || serverURL.RawQuery != "" ||
		serverURL.Fragment != "" || (serverURL.Path != "" && serverURL.Path != "/") {
		return fmt.Errorf("SERVER_URL must be an HTTP or HTTPS origin without credentials, path, query, or fragment")
	}
	if c.AuthEnabled {
		for _, insecure := range insecureJWTSecrets {
			if c.JWTSecret == insecure {
				return fmt.Errorf(
					"SECURITY: JWT_SECRET is set to a well-known insecure value %q. "+
						"Please set a strong, unique secret (at least 32 characters) in your configuration",
					c.JWTSecret,
				)
			}
		}
		if len(c.JWTSecret) < 32 {
			return fmt.Errorf(
				"SECURITY: JWT_SECRET is too short (%d chars). Use at least 32 characters for security",
				len(c.JWTSecret),
			)
		}
		// When anonymous access is disabled, the login handler must be able to
		// authenticate a real operator; otherwise the dashboard is unreachable.
		if !c.AllowAnonymous {
			if c.AdminUser == "" || c.AdminPassword == "" {
				return fmt.Errorf(
					"SECURITY: ALLOW_ANONYMOUS is false but ADMIN_USER/ADMIN_PASSWORD are not set; " +
						"set credentials so operators can log in",
				)
			}
		}
	}
	return nil
}

// BuilderConfig represents the builder configuration.
type BuilderConfig struct {
	Port         int
	AuthToken    string // Shared secret required on build/job endpoints (empty = auth disabled)
	Workers      int
	InstanceID   string
	Architecture string
	// NativeJobPolicy controls reuse of the native Gentoo root. "single-use"
	// (default) persistently drains the builder as soon as one BuildJob is
	// accepted; only an external VM/snapshot/rootfs reset may make it clean
	// again. "unsafe-reuse" preserves the legacy mutable-host behavior and is
	// intended only for disposable development environments.
	NativeJobPolicy    string
	WorkDir            string
	ArtifactDir        string
	DataDir            string
	PersistenceEnabled bool
	RetentionDays      int
	// BinpkgFormat selects the binary package format Portage produces: "gpkg"
	// (modern) or "xpak" (legacy .tbz2, deprecated). Builders always emit
	// unsigned packages; portage-signer owns OpenPGP signing.
	BinpkgFormat string
	// BuildFeatures is appended to the native build root's make.conf FEATURES.
	BuildFeatures   string
	StorageType     string
	StorageLocalDir string
	StorageS3Bucket string
	StorageS3Region string
	StorageS3Prefix string
	StorageHTTPBase string
	ServerURL       string
	// ServerAPIKey is the central server's API key, attached to registration
	// and heartbeat calls (required when the server sets API_KEY).
	ServerAPIKey string
	// AdvertiseURL is the URL this builder registers with the server (how the
	// server reaches it). Defaults to http://<hostname>:<port>.
	AdvertiseURL    string
	NotifyConfig    string
	MetricsEnabled  bool
	MetricsPort     string
	MetricsPassword string

	// Portage mirror settings.
	SyncMirror      string // Mirror URL for portage sync (rsync or git)
	DistfilesMirror string // Mirror URL for distfiles download

	// Portage paths on the native Gentoo build root.
	PortageReposPath string // Path to portage repos (default: /var/db/repos)
	PortageConfPath  string // Path to portage config (default: /etc/portage)
	MakeConfPath     string // Path to make.conf (default: /etc/portage/make.conf)
}

// Validate checks the builder configuration for common misconfigurations.
func (c *BuilderConfig) Validate() []string {
	var warnings []string

	if c.Workers <= 0 {
		warnings = append(warnings, "CONFIG: BUILDER_WORKERS must be > 0")
	}
	if c.Port <= 0 || c.Port > 65535 {
		warnings = append(warnings, fmt.Sprintf("CONFIG: BUILDER_PORT %d is invalid, must be 1-65535", c.Port))
	}
	if c.AuthToken == "" {
		warnings = append(warnings, "SECURITY: BUILDER_TOKEN is not set — the build endpoint is unauthenticated and allows arbitrary remote builds")
	}
	switch c.NativeJobPolicy {
	case "", "single-use":
	case "unsafe-reuse":
		warnings = append(warnings, "SECURITY: native unsafe-reuse allows package/VDB/postinst state to leak across jobs; use single-use plus an external snapshot/VM reset")
	default:
		warnings = append(warnings, fmt.Sprintf("CONFIG: NATIVE_JOB_POLICY %q is invalid; use single-use or unsafe-reuse", c.NativeJobPolicy))
	}
	if c.WorkDir == "" {
		warnings = append(warnings, "CONFIG: BUILD_WORK_DIR is not set")
	}
	if c.ArtifactDir == "" {
		warnings = append(warnings, "CONFIG: BUILD_ARTIFACT_DIR is not set")
	}

	return warnings
}

// unquoteEnvValue strips a single matching pair of surrounding single or double
// quotes from a config value, so a quoted secret/path is not silently corrupted
// by the literal quotes. Unquoted values (and mismatched quotes) are returned
// unchanged.
func unquoteEnvValue(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// loadEnvFile loads key=value pairs from a .conf file.
func loadEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	env := make(map[string]string)
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Split on first =
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := unquoteEnvValue(strings.TrimSpace(parts[1]))
			env[key] = value
		}
	}

	return env, scanner.Err()
}

// getEnvString gets string value from env map with fallback to system env.
// getEnvString resolves a config value with conventional precedence:
// process environment > config file > built-in default. (The file used to win
// over the environment, which made container/env-only overrides silently
// no-ops whenever a conf file was present.)
func getEnvString(env map[string]string, key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	if val, ok := env[key]; ok && val != "" {
		return val
	}
	return defaultValue
}

// getEnvInt gets int value from env map with fallback to system env.
func getEnvInt(env map[string]string, key string, defaultValue int) int {
	val := getEnvString(env, key, "")
	if val == "" {
		return defaultValue
	}
	if i, err := strconv.Atoi(val); err == nil {
		return i
	}
	return defaultValue
}

// getEnvBool gets bool value from env map with fallback to system env.
func getEnvBool(env map[string]string, key string, defaultValue bool) bool {
	val := getEnvString(env, key, "")
	if val == "" {
		return defaultValue
	}
	val = strings.ToLower(val)
	return val == "true" || val == "1" || val == "yes"
}

// LoadServerConfig loads server configuration from a file.
func LoadServerConfig(path string) (*ServerConfig, error) {
	// Set defaults
	config := &ServerConfig{
		Port:            8080,
		BinpkgPath:      "/var/cache/binpkgs",
		MaxWorkers:      5,
		BuildMode:       "remote",
		StorageType:     "local",
		StorageLocalDir: "/var/cache/binpkgs",
		GPGEnabled:      false,
		CloudProvider:   "gcp",
		CloudGCPProject: "portage-engine",
		CloudGCPRegion:  "us-central1",
		CloudGCPZone:    "us-central1-a",
	}

	// If the config file is missing, still honor environment variables (the
	// get* helpers fall back to os.Getenv). Only defaults + env apply.
	env := map[string]string{}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Printf("Config file not found, using defaults + environment: %s\n", path)
	} else {
		loaded, err := loadEnvFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
		env = loaded
	}

	config.Port = getEnvInt(env, "SERVER_PORT", config.Port)
	config.ControlPlaneID = getEnvString(env, "CONTROL_PLANE_ID", "")
	config.BinpkgPath = getEnvString(env, "BINPKG_PATH", config.BinpkgPath)
	config.MaxWorkers = getEnvInt(env, "MAX_WORKERS", config.MaxWorkers)
	config.BuildMode = getEnvString(env, "BUILD_MODE", config.BuildMode)

	config.StorageType = getEnvString(env, "STORAGE_TYPE", config.StorageType)
	config.StorageLocalDir = getEnvString(env, "STORAGE_LOCAL_DIR", config.StorageLocalDir)
	config.StorageS3Bucket = getEnvString(env, "STORAGE_S3_BUCKET", "")
	config.StorageS3Region = getEnvString(env, "STORAGE_S3_REGION", "")
	config.StorageS3Prefix = getEnvString(env, "STORAGE_S3_PREFIX", "")
	config.StorageHTTPBase = getEnvString(env, "STORAGE_HTTP_BASE", "")

	config.GPGEnabled = getEnvBool(env, "GPG_ENABLED", config.GPGEnabled)
	config.GPGKeyID = getEnvString(env, "GPG_KEY_ID", "")
	config.GPGKeyPath = getEnvString(env, "GPG_KEY_PATH", "")
	config.GPGAutoCreate = getEnvBool(env, "GPG_AUTO_CREATE", true)
	config.GPGKeyName = getEnvString(env, "GPG_KEY_NAME", "Portage Engine")
	config.GPGKeyEmail = getEnvString(env, "GPG_KEY_EMAIL", "portage@localhost")
	config.GPGHome = getEnvString(env, "GPG_HOME", "/var/lib/portage-engine/gpg")
	config.GPGPublicKeyPath = getEnvString(env, "GPG_PUBLIC_KEY_PATH", "/var/lib/portage-engine/gpg/public.asc")
	config.SignerWaitTimeoutSeconds = getEnvInt(env, "SIGNER_WAIT_TIMEOUT_SECONDS", 600)

	config.CloudProvider = getEnvString(env, "CLOUD_DEFAULT_PROVIDER", config.CloudProvider)
	config.CloudAliyunRegion = getEnvString(env, "CLOUD_ALIYUN_REGION", "cn-hangzhou")
	config.CloudAliyunZone = getEnvString(env, "CLOUD_ALIYUN_ZONE", "cn-hangzhou-a")
	config.CloudAliyunAK = getEnvString(env, "CLOUD_ALIYUN_ACCESS_KEY", "")
	config.CloudAliyunSK = getEnvString(env, "CLOUD_ALIYUN_SECRET_KEY", "")
	config.CloudGCPProject = getEnvString(env, "CLOUD_GCP_PROJECT", config.CloudGCPProject)
	config.CloudGCPRegion = getEnvString(env, "CLOUD_GCP_REGION", config.CloudGCPRegion)
	config.CloudGCPZone = getEnvString(env, "CLOUD_GCP_ZONE", config.CloudGCPZone)
	config.CloudGCPKeyFile = getEnvString(env, "CLOUD_GCP_KEY_FILE", "")
	config.CloudGCPMachineType = getEnvString(env, "CLOUD_GCP_MACHINE_TYPE", "n1-standard-4")
	config.CloudGCPDiskSizeGB = getEnvInt(env, "CLOUD_GCP_DISK_SIZE_GB", 100)
	config.CloudGCPDiskType = getEnvString(env, "CLOUD_GCP_DISK_TYPE", "pd-ssd")
	config.CloudGCPImageFamily = getEnvString(env, "CLOUD_GCP_IMAGE_FAMILY", "ubuntu-2204-lts")
	config.CloudGCPImageProject = getEnvString(env, "CLOUD_GCP_IMAGE_PROJECT", "ubuntu-os-cloud")
	config.CloudGCPNetwork = getEnvString(env, "CLOUD_GCP_NETWORK", "default")
	config.CloudGCPSubnetwork = getEnvString(env, "CLOUD_GCP_SUBNETWORK", "")
	config.CloudGCPPreemptible = getEnvBool(env, "CLOUD_GCP_PREEMPTIBLE", false)
	config.CloudGCPStateDir = getEnvString(env, "CLOUD_GCP_STATE_DIR", "")
	if allowedIPs := getEnvString(env, "CLOUD_GCP_ALLOWED_IPS", ""); allowedIPs != "" {
		config.CloudGCPAllowedIPs = strings.Split(allowedIPs, ",")
		for i := range config.CloudGCPAllowedIPs {
			config.CloudGCPAllowedIPs[i] = strings.TrimSpace(config.CloudGCPAllowedIPs[i])
		}
	}
	config.CloudInstanceTTL = getEnvInt(env, "CLOUD_INSTANCE_TTL", 60) // Default 60 minutes
	config.CloudAWSRegion = getEnvString(env, "CLOUD_AWS_REGION", "us-east-1")
	config.CloudAWSZone = getEnvString(env, "CLOUD_AWS_ZONE", "us-east-1a")
	config.CloudAWSAccessKey = getEnvString(env, "CLOUD_AWS_ACCESS_KEY", "")
	config.CloudAWSSecretKey = getEnvString(env, "CLOUD_AWS_SECRET_KEY", "")

	// PVE (Proxmox VE) configuration
	config.CloudPVEEndpoint = getEnvString(env, "CLOUD_PVE_ENDPOINT", "")
	config.CloudPVENode = getEnvString(env, "CLOUD_PVE_NODE", "pve")
	config.CloudPVETokenID = getEnvString(env, "CLOUD_PVE_TOKEN_ID", "")
	config.CloudPVETokenSecret = getEnvString(env, "CLOUD_PVE_TOKEN_SECRET", "")
	config.CloudPVEUsername = getEnvString(env, "CLOUD_PVE_USERNAME", "")
	config.CloudPVEPassword = getEnvString(env, "CLOUD_PVE_PASSWORD", "")
	config.CloudPVEInsecure = getEnvBool(env, "CLOUD_PVE_INSECURE", false)
	config.CloudPVEStorage = getEnvString(env, "CLOUD_PVE_STORAGE", "local-lvm")
	config.CloudPVENetwork = getEnvString(env, "CLOUD_PVE_NETWORK", "vmbr0")
	config.CloudPVETemplate = getEnvString(env, "CLOUD_PVE_TEMPLATE", "")
	if nodes := getEnvString(env, "CLOUD_PVE_NODES", ""); nodes != "" {
		config.CloudPVENodes = strings.Split(nodes, ",")
		for i := range config.CloudPVENodes {
			config.CloudPVENodes[i] = strings.TrimSpace(config.CloudPVENodes[i])
		}
	}
	if allowedIPs := getEnvString(env, "CLOUD_PVE_ALLOWED_IPS", ""); allowedIPs != "" {
		config.CloudPVEAllowedIPs = strings.Split(allowedIPs, ",")
		for i := range config.CloudPVEAllowedIPs {
			config.CloudPVEAllowedIPs[i] = strings.TrimSpace(config.CloudPVEAllowedIPs[i])
		}
	}

	config.CloudSSHKeyPath = getEnvString(env, "CLOUD_SSH_KEY_PATH", "")
	config.CloudSSHUser = getEnvString(env, "CLOUD_SSH_USER", "root")
	config.CloudSSHKnownHosts = getEnvString(env, "CLOUD_SSH_KNOWN_HOSTS", "")
	config.CloudSSHInsecureHostKey = getEnvBool(env, "CLOUD_SSH_INSECURE_HOST_KEY", false)
	config.ServerCallbackURL = getEnvString(env, "SERVER_CALLBACK_URL", "")
	config.CloudBuilderBinaryPath = getEnvString(env, "CLOUD_BUILDER_BINARY_PATH", "")
	config.CloudBuilderBinaryURL = getEnvString(env, "CLOUD_BUILDER_BINARY_URL", "")
	config.CloudBuilderBinarySHA256 = getEnvString(env, "CLOUD_BUILDER_BINARY_SHA256", "")

	config.MetricsEnabled = getEnvBool(env, "METRICS_ENABLED", false)
	config.MetricsPort = getEnvString(env, "METRICS_PORT", "2112")
	config.MetricsPassword = getEnvString(env, "METRICS_PASSWORD", "")

	// Parse remote builders
	if builders := getEnvString(env, "REMOTE_BUILDERS", ""); builders != "" {
		config.RemoteBuilders = strings.Split(builders, ",")
		for i := range config.RemoteBuilders {
			config.RemoteBuilders[i] = strings.TrimSpace(config.RemoteBuilders[i])
		}
	}

	// Security settings
	config.APIKey = getEnvString(env, "API_KEY", "")
	config.BuilderToken = getEnvString(env, "BUILDER_TOKEN", "")
	config.CORSAllowedOrigins = getEnvStringSlice(env, "CORS_ALLOWED_ORIGINS", nil)
	config.MaxRequestBodyBytes = int64(getEnvInt(env, "MAX_REQUEST_BODY_BYTES", 10*1024*1024)) // Default 10MB
	config.DataDir = getEnvString(env, "DATA_DIR", "/var/lib/portage-engine/server")
	config.CatalogPath = getEnvString(env, "CATALOG_PATH", "")
	config.ImageFactoryStatusPath = getEnvString(env, "IMAGE_FACTORY_STATUS_PATH", "")
	config.Database.Enabled = getEnvBool(env, "DATABASE_ENABLED", false)
	config.Database.Required = getEnvBool(env, "DATABASE_REQUIRED", false)
	if config.Database.Required {
		config.Database.Enabled = true
	}
	config.Database.URL = getEnvString(env, "DATABASE_URL", "")
	config.Database.Host = getEnvString(env, "PGHOST", "127.0.0.1")
	config.Database.Port = getEnvInt(env, "PGPORT", 5432)
	config.Database.Name = getEnvString(env, "PGDATABASE", "portage_engine")
	config.Database.User = getEnvString(env, "PGUSER", "portage")
	config.Database.Password = getEnvString(env, "PGPASSWORD", "")
	config.Database.SSLMode = getEnvString(env, "PGSSLMODE", "verify-full")
	config.Database.MaxConns = getEnvInt(env, "DATABASE_MAX_CONNS", 10)
	config.Database.MinConns = getEnvInt(env, "DATABASE_MIN_CONNS", 1)
	config.Database.ConnectTimeoutSeconds = getEnvInt(env, "DATABASE_CONNECT_TIMEOUT_SECONDS", 10)
	config.Database.HealthTimeoutSeconds = getEnvInt(env, "DATABASE_HEALTH_TIMEOUT_SECONDS", 2)
	config.Cache.Enabled = getEnvBool(env, "REDIS_ENABLED", false)
	config.Cache.Required = getEnvBool(env, "REDIS_REQUIRED", false)
	if config.Cache.Required {
		config.Cache.Enabled = true
	}
	config.Cache.Host = getEnvString(env, "REDIS_HOST", "127.0.0.1")
	config.Cache.Port = getEnvInt(env, "REDIS_PORT", 6379)
	config.Cache.Password = getEnvString(env, "REDIS_PASSWORD", "")
	config.Cache.DB = getEnvInt(env, "REDIS_DB", 0)
	config.Cache.TLSEnabled = getEnvBool(env, "REDIS_TLS_ENABLED", false)
	config.Cache.KeyPrefix = getEnvString(env, "REDIS_KEY_PREFIX", "portage-engine")
	config.Cache.RateLimitPerMinute = getEnvInt(env, "RATE_LIMIT_PER_MINUTE", 120)
	config.Cache.RateLimitBurst = getEnvInt(env, "RATE_LIMIT_BURST", 30)

	return config, nil
}

// LoadDashboardConfig loads dashboard configuration from a file.
func LoadDashboardConfig(path string) (*DashboardConfig, error) {
	// Set defaults
	config := &DashboardConfig{
		Port:            8081,
		ServerURL:       "http://localhost:8080",
		AuthEnabled:     true,
		JWTSecret:       "change-me-in-production",
		TokenTTLMinutes: 720,
		AllowAnonymous:  true,
	}

	// If the config file is missing, still honor environment variables.
	env := map[string]string{}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Printf("Config file not found, using defaults + environment: %s\n", path)
	} else {
		loaded, err := loadEnvFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
		env = loaded
	}

	config.Port = getEnvInt(env, "DASHBOARD_PORT", config.Port)
	config.ServerURL = getEnvString(env, "SERVER_URL", config.ServerURL)
	config.ServerAPIKey = getEnvString(env, "SERVER_API_KEY", "")
	config.AuthEnabled = getEnvBool(env, "AUTH_ENABLED", config.AuthEnabled)
	config.JWTSecret = getEnvString(env, "JWT_SECRET", config.JWTSecret)
	config.AdminUser = getEnvString(env, "ADMIN_USER", "")
	config.AdminPassword = getEnvString(env, "ADMIN_PASSWORD", "")
	config.TokenTTLMinutes = getEnvInt(env, "TOKEN_TTL_MINUTES", 720)
	config.AllowAnonymous = getEnvBool(env, "ALLOW_ANONYMOUS", config.AllowAnonymous)

	config.MetricsEnabled = getEnvBool(env, "METRICS_ENABLED", false)
	config.MetricsPort = getEnvString(env, "METRICS_PORT", "2112")
	config.MetricsPassword = getEnvString(env, "METRICS_PASSWORD", "")

	return config, nil
}

// LoadBuilderConfig loads builder configuration from a file.
func LoadBuilderConfig(path string) (*BuilderConfig, error) {
	// Set defaults
	config := &BuilderConfig{
		Port:               9090,
		Workers:            2,
		NativeJobPolicy:    "single-use",
		WorkDir:            "/var/tmp/portage-builds",
		ArtifactDir:        "/var/tmp/portage-artifacts",
		DataDir:            "/var/lib/portage-engine",
		PersistenceEnabled: true,
		RetentionDays:      7,
		BinpkgFormat:       "gpkg",
		StorageType:        "local",
		StorageLocalDir:    "/var/binpkgs",
		// Portage defaults
		PortageReposPath: "/var/db/repos",
		PortageConfPath:  "/etc/portage",
		MakeConfPath:     "/etc/portage/make.conf",
	}

	// If the config file is missing, still honor environment variables.
	env := map[string]string{}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Printf("Config file not found, using defaults + environment: %s\n", path)
	} else {
		loaded, err := loadEnvFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
		env = loaded
	}

	config.Port = getEnvInt(env, "BUILDER_PORT", config.Port)
	config.AuthToken = getEnvString(env, "BUILDER_TOKEN", "")
	config.Workers = getEnvInt(env, "BUILDER_WORKERS", config.Workers)
	config.InstanceID = getEnvString(env, "INSTANCE_ID", "")
	config.Architecture = getEnvString(env, "ARCHITECTURE", "")
	if getEnvBool(env, "USE_DOCKER", false) {
		return nil, fmt.Errorf("USE_DOCKER=true is no longer supported; deploy the builder in a disposable native Gentoo root or VM")
	}
	config.NativeJobPolicy = getEnvString(env, "NATIVE_JOB_POLICY", config.NativeJobPolicy)
	config.WorkDir = getEnvString(env, "BUILD_WORK_DIR", config.WorkDir)
	config.ArtifactDir = getEnvString(env, "BUILD_ARTIFACT_DIR", config.ArtifactDir)
	config.DataDir = getEnvString(env, "DATA_DIR", config.DataDir)
	config.PersistenceEnabled = getEnvBool(env, "PERSISTENCE_ENABLED", config.PersistenceEnabled)
	config.RetentionDays = getEnvInt(env, "RETENTION_DAYS", config.RetentionDays)

	config.BinpkgFormat = getEnvString(env, "BINPKG_FORMAT", config.BinpkgFormat)
	config.BuildFeatures = getEnvString(env, "BUILD_FEATURES", "")

	config.StorageType = getEnvString(env, "STORAGE_TYPE", config.StorageType)
	config.StorageLocalDir = getEnvString(env, "STORAGE_LOCAL_DIR", config.StorageLocalDir)
	config.StorageS3Bucket = getEnvString(env, "STORAGE_S3_BUCKET", "")
	config.StorageS3Region = getEnvString(env, "STORAGE_S3_REGION", "")
	config.StorageS3Prefix = getEnvString(env, "STORAGE_S3_PREFIX", "")
	config.StorageHTTPBase = getEnvString(env, "STORAGE_HTTP_BASE", "")

	config.ServerURL = getEnvString(env, "SERVER_URL", "")
	config.ServerAPIKey = getEnvString(env, "SERVER_API_KEY", "")
	config.AdvertiseURL = getEnvString(env, "BUILDER_ADVERTISE_URL", "")
	config.NotifyConfig = getEnvString(env, "NOTIFY_CONFIG", "")

	// Portage mirror settings
	config.SyncMirror = getEnvString(env, "SYNC_MIRROR", config.SyncMirror)
	config.DistfilesMirror = getEnvString(env, "DISTFILES_MIRROR", config.DistfilesMirror)

	// Portage path settings
	config.PortageReposPath = getEnvString(env, "PORTAGE_REPOS_PATH", config.PortageReposPath)
	config.PortageConfPath = getEnvString(env, "PORTAGE_CONF_PATH", config.PortageConfPath)
	config.MakeConfPath = getEnvString(env, "MAKE_CONF_PATH", config.MakeConfPath)

	config.MetricsEnabled = getEnvBool(env, "METRICS_ENABLED", false)
	config.MetricsPort = getEnvString(env, "METRICS_PORT", "2112")
	config.MetricsPassword = getEnvString(env, "METRICS_PASSWORD", "")

	return config, nil
}

// getEnvStringSlice reads a comma-separated string from the env map and returns
// it as a trimmed slice. Returns defaultValue if the key is empty.
func getEnvStringSlice(env map[string]string, key string, defaultValue []string) []string {
	raw := getEnvString(env, key, "")
	if raw == "" {
		return defaultValue
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	if len(result) == 0 {
		return defaultValue
	}
	return result
}
