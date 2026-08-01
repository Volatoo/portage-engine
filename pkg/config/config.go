// Package config provides configuration management for Portage Engine.
package config

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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

var identityProviderIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
var executorCapabilityPattern = regexp.MustCompile(
	`^[a-z][a-z0-9-]{0,31}:[a-zA-Z0-9][a-zA-Z0-9+._/@:-]{0,479}$`,
)
var executorZonePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var executorCapacityInstancePattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
)
var autoscaleProviderPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// IdentityProviderConfig describes one community-selectable sign-in provider.
// ClientSecretEnv names the environment variable populated by a deployment
// secret provider; the resolved secret is never serialized back to JSON.
type IdentityProviderConfig struct {
	ID                       string `json:"id"`
	Type                     string `json:"type"`
	DisplayName              string `json:"display_name"`
	IssuerURL                string `json:"issuer_url,omitempty"`
	Audience                 string `json:"audience,omitempty"`
	ClientID                 string `json:"client_id"`
	ClientSecretEnv          string `json:"client_secret_env,omitempty"`
	ClientSecret             string `json:"-"`
	RedirectURL              string `json:"redirect_url"`
	AuthorizationURL         string `json:"authorization_url,omitempty"`
	TokenURL                 string `json:"token_url,omitempty"`
	APIBaseURL               string `json:"api_base_url,omitempty"`
	AllowInsecureHTTP        bool   `json:"allow_insecure_http,omitempty"`
	BackchannelLogout        bool   `json:"backchannel_logout,omitempty"`
	BackchannelRequireSID    bool   `json:"backchannel_require_sid,omitempty"`
	BackchannelMaxAgeSeconds int    `json:"backchannel_max_age_seconds,omitempty"`
}

type identityProviderDocument struct {
	Providers []IdentityProviderConfig `json:"providers"`
}

const (
	DeploymentModeTrusted = "trusted"
	DeploymentModePublic  = "public"
)

// ServerConfig represents the server configuration.
type ServerConfig struct {
	DeploymentMode           string // trusted or public
	Port                     int
	ControlPlaneID           string
	RuntimeRole              string // control-plane (combined), api, or executor
	BinpkgPath               string
	MaxWorkers               int
	BuildMode                string
	StorageType              string
	StorageLocalDir          string
	StorageS3Bucket          string
	StorageS3Region          string
	StorageS3Prefix          string
	StorageS3Endpoint        string
	StorageS3UsePathStyle    bool
	StorageS3PublicBaseURL   string
	StorageS3AllowDelete     bool
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
	APIKey                             string   // API key for authenticating requests (empty = auth disabled)
	StepUpAPIKey                       string   // Independent legacy/hybrid credential for high-risk writes
	AuthMode                           string   // legacy, hybrid, or oidc
	OIDCIssuerURL                      string   // Exact issuer used for discovery and iss validation
	OIDCAudience                       string   // Required aud/client ID for control-plane bearer JWTs
	OIDCAdminSubjects                  []string // Exact OIDC subject IDs granted system-admin bootstrap access
	OIDCAllowInsecureHTTP              bool     // Trusted-LAN-only opt-in for an HTTP issuer
	OIDCSessionIdleMinutes             int      // Server-side inactivity timeout for observed bearer sessions
	OIDCSessionMaxMinutes              int      // Maximum accepted token/session lifetime
	OIDCStepUpMaxAgeMin                int      // Maximum auth_time age for sensitive writes
	OIDCStepUpAMRValues                []string // Optional accepted authentication-method references
	OIDCStepUpACRValues                []string // Optional accepted authentication-context classes
	DeviceAuthorizationVerificationURI string   // Browser page used to approve CLI device codes
	IdentityProvidersPath              string
	IdentityProviders                  []IdentityProviderConfig
	IdentityAdminSubjects              []string // provider-id:external-subject
	BuilderToken                       string   // Shared secret the server presents to remote builders (empty = no builder auth)
	CORSAllowedOrigins                 []string // Allowed CORS origins (empty = allow all for backward compatibility)
	MaxRequestBodyBytes                int64    // Maximum request body size in bytes (0 = default 10MB)
	CatalogPath                        string   // Server-owned profile/repository/image catalog JSON (empty = compatibility catalog)
	ImageFactoryStatusPath             string   // Optional read-only image-factory milestone/evidence status JSON
	// WorkerGateway is a dedicated mTLS listener for disposable workers. The
	// ordinary control-plane/UI listener may remain HTTP on a trusted LAN.
	WorkerGatewayEnabled        bool
	WorkerGatewayPort           int
	WorkerGatewayAdvertiseURL   string
	WorkerGatewayTLSCert        string
	WorkerGatewayTLSKey         string
	WorkerGatewayServerCA       string
	WorkerGatewayClientCA       string
	WorkerGatewayIssuerID       string
	WorkerGatewayIssuerProvider string
	WorkerGatewayIssuerCert     string
	WorkerGatewayIssuerKey      string
	WorkerGatewayVaultAddress   string
	WorkerGatewayVaultMount     string
	WorkerGatewayVaultRole      string
	WorkerGatewayVaultTokenPath string
	WorkerGatewayVaultNamespace string
	WorkerGatewayVaultServerCA  string
	WorkerGatewayVaultTimeout   int
	WorkerCertificateTTLMin     int
	// PhaseExecutorMode controls the explicit cutover from the legacy
	// whole-pipeline worker to independently leased durable phases. "shadow"
	// keeps creating non-runnable plans; "active" is PostgreSQL + outbound
	// worker only and never starts the legacy executor for the same attempt.
	PhaseExecutorMode    string
	ExecutorZones        []string // Execution zones this replica can reach; default is "default"
	ExecutorCapabilities []string // Exact capability-label override; empty derives labels from catalog/config
	// ExecutorCapacityInstanceID binds every worker slot in a persistent
	// executor process to one actuator-owned capacity instance UUID.
	ExecutorCapacityInstanceID string
	// Scheduler autoscaling can remain advisory or enqueue fenced actions.
	// Provider side effects are always executed by the independent actuator,
	// never by the scheduler transaction.
	SchedulerAutoscaleMode             string
	SchedulerAutoscaleMinSlots         int
	SchedulerAutoscaleMaxSlots         int
	SchedulerAutoscaleTargetReady      int
	SchedulerAutoscaleCooldownSeconds  int
	SchedulerAutoscaleScaleDownSeconds int
	SchedulerAutoscaleIntervalSeconds  int
	SchedulerAutoscaleProviderMaxSlots map[string]int
	SchedulerAutoscaleProviderLimitsOK bool
	// DistCC Alpha is an opt-in compile-only acceleration layer. Project
	// fairness/admission remains authoritative before compile-slot routing.
	DistCCAlphaEnabled             bool
	DistCCPackageAllowlist         []string
	DistCCCHOST                    string
	DistCCCompilerDigest           string
	DistCCToolchainImageGeneration string
	DistCCCPUFeatures              []string
	DistCCNetworkZone              string
	DistCCIsolatedNetworkCIDRs     []string
	DistCCSlotsPerJob              int
	DistCCLeaseSeconds             int
	DistCCWorkerFreshnessSeconds   int
	DistCCFallbackPolicy           string
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
	if c.RuntimeRole != "" && c.RuntimeRole != "control-plane" &&
		c.RuntimeRole != "api" && c.RuntimeRole != "executor" {
		warnings = append(
			warnings,
			"CONFIG: SERVER_RUNTIME_ROLE must be control-plane, api, or executor",
		)
	}
	if c.RuntimeRole == "executor" {
		if c.ControlPlaneID == "" {
			warnings = append(
				warnings,
				"CONFIG: executor role requires a stable CONTROL_PLANE_ID",
			)
		}
		if c.PhaseExecutorMode != "active" ||
			!c.Database.Enabled || !c.Database.Required ||
			!c.WorkerGatewayEnabled {
			warnings = append(
				warnings,
				"CONFIG: executor role requires active phase execution, required PostgreSQL, and Worker Gateway",
			)
		}
		if len(c.ExecutorCapabilities) == 0 {
			warnings = append(
				warnings,
				"CONFIG: executor role requires explicit EXECUTOR_CAPABILITIES for one immutable pool",
			)
		}
		if c.ExecutorCapacityInstanceID == "" {
			warnings = append(
				warnings,
				"CONFIG: executor role requires EXECUTOR_CAPACITY_INSTANCE_ID",
			)
		} else if !executorCapacityInstancePattern.MatchString(
			c.ExecutorCapacityInstanceID,
		) {
			warnings = append(
				warnings,
				"CONFIG: EXECUTOR_CAPACITY_INSTANCE_ID must be a lowercase UUID",
			)
		}
	} else if c.ExecutorCapacityInstanceID != "" {
		warnings = append(
			warnings,
			"CONFIG: EXECUTOR_CAPACITY_INSTANCE_ID is only valid for executor role",
		)
	}
	warnings = append(warnings, c.validateAuth()...)
	warnings = append(warnings, c.validateCore()...)
	warnings = append(warnings, c.validateDatabaseAndCache()...)
	warnings = append(warnings, c.validateWorkerGateway()...)
	return warnings
}

// ValidateStartup enforces deployment-boundary requirements that must stop the
// process instead of being emitted as advisory warnings. Trusted mode preserves
// the self-hosted/LAN compatibility surface. Public mode is intentionally
// strict: it is a readiness contract, not a shortcut for exposing the
// development Compose topology.
func (c *ServerConfig) ValidateStartup() error {
	mode := strings.ToLower(strings.TrimSpace(c.DeploymentMode))
	if mode == "" {
		mode = DeploymentModeTrusted
	}
	if mode != DeploymentModeTrusted && mode != DeploymentModePublic {
		return fmt.Errorf(
			"DEPLOYMENT_MODE must be %q or %q",
			DeploymentModeTrusted, DeploymentModePublic,
		)
	}
	if c.RuntimeRole == "executor" {
		if violations := c.validatePersistentExecutorBoundary(); len(violations) > 0 {
			return fmt.Errorf(
				"persistent executor configuration rejected: %s",
				strings.Join(violations, "; "),
			)
		}
	}
	if mode != DeploymentModePublic {
		return nil
	}

	violations := c.validatePublicRuntimeBoundary()
	violations = append(violations, c.validatePublicStorageBoundary()...)
	if c.RuntimeRole != "executor" {
		violations = append(violations, c.validatePublicAPIBoundary()...)
	}
	if len(violations) > 0 {
		return fmt.Errorf(
			"public deployment configuration rejected: %s",
			strings.Join(violations, "; "),
		)
	}
	return nil
}

func (c *ServerConfig) validatePersistentExecutorBoundary() []string {
	violations := make([]string, 0, 16)
	addViolation(&violations, strings.TrimSpace(c.ControlPlaneID) == "",
		"CONTROL_PLANE_ID must be stable and non-empty")
	addViolation(&violations, c.PhaseExecutorMode != "active",
		"PHASE_EXECUTOR_MODE must be active")
	addViolation(&violations, !c.Database.Enabled || !c.Database.Required,
		"PostgreSQL must be enabled and required")
	addViolation(&violations, !c.WorkerGatewayEnabled,
		"WORKER_GATEWAY_ENABLED must be true")
	addViolation(&violations, len(c.RemoteBuilders) != 0,
		"REMOTE_BUILDERS legacy push compatibility must be disabled")
	addViolation(&violations, strings.TrimSpace(c.ExecutorCapacityInstanceID) == "" ||
		!executorCapacityInstancePattern.MatchString(c.ExecutorCapacityInstanceID),
		"EXECUTOR_CAPACITY_INSTANCE_ID must be a lowercase UUID")
	addViolation(&violations, strings.TrimSpace(c.WorkerGatewayTLSKey) != "",
		"executor role must not receive WORKER_GATEWAY_TLS_KEY listener credentials")
	addViolation(&violations, c.executorGatewayTrustIncomplete(),
		"Worker Gateway advertise URL plus server/client CA bundles are required")
	addViolation(&violations,
		c.WorkerGatewayAdvertiseURL != "" &&
			validateHTTPSEndpoint(c.WorkerGatewayAdvertiseURL) != nil,
		"WORKER_GATEWAY_ADVERTISE_URL must be an absolute HTTPS URL")
	switch c.WorkerGatewayIssuerProvider {
	case "file":
		addViolation(&violations,
			c.WorkerGatewayIssuerCert == "" || c.WorkerGatewayIssuerKey == "",
			"file worker issuer requires its certificate and runtime-injected private key")
	case "vault":
		addViolation(&violations,
			c.WorkerGatewayVaultAddress == "" || c.WorkerGatewayVaultMount == "" ||
				c.WorkerGatewayVaultRole == "" || c.WorkerGatewayVaultTokenPath == "",
			"Vault worker issuer requires address, mount, role, and runtime token file")
	default:
		addViolation(&violations, true,
			"WORKER_GATEWAY_ISSUER_PROVIDER must be file or vault")
	}

	violations = append(
		violations, validatePersistentExecutorCapabilities(c.ExecutorCapabilities)...,
	)
	return violations
}

func validatePersistentExecutorCapabilities(capabilities []string) []string {
	var violations []string
	seen := make(map[string]struct{}, len(capabilities))
	required := map[string]int{
		"capacity-pool": 0,
		"provider":      0,
		"zone":          0,
		"arch":          0,
		"build-mode":    0,
		"profile":       0,
		"image":         0,
	}
	dimensions := make(map[string]string, len(required))
	phases := map[string]bool{
		"provision": false,
		"build":     false,
		"verify":    false,
		"publish":   false,
	}
	for _, capability := range capabilities {
		if !executorCapabilityPattern.MatchString(capability) {
			violations = append(violations,
				"EXECUTOR_CAPABILITIES contains invalid label "+capability)
			continue
		}
		if _, duplicate := seen[capability]; duplicate {
			violations = append(violations,
				"EXECUTOR_CAPABILITIES contains duplicate label "+capability)
			continue
		}
		seen[capability] = struct{}{}
		prefix, value, found := strings.Cut(capability, ":")
		if !found {
			continue
		}
		if _, exists := required[prefix]; exists {
			required[prefix]++
			dimensions[prefix] = value
		}
		if prefix == "phase" {
			if _, exists := phases[value]; !exists {
				violations = append(violations,
					"EXECUTOR_CAPABILITIES contains unsupported phase:"+value)
			} else {
				phases[value] = true
			}
		}
		if prefix == "capacity-instance" {
			violations = append(violations,
				"capacity-instance capability is derived only from SMBIOS")
		}
		if prefix == "image" {
			separator := strings.LastIndex(value, "@")
			if separator <= 0 || separator == len(value)-1 ||
				strings.Count(value, "@") != 1 {
				violations = append(violations,
					"image capability must bind image ID and generation as image:<id>@<generation>")
			}
		}
	}
	for _, prefix := range []string{
		"capacity-pool", "provider", "zone", "arch", "build-mode", "profile", "image",
	} {
		if required[prefix] != 1 {
			violations = append(violations, fmt.Sprintf(
				"EXECUTOR_CAPABILITIES must contain exactly one %s label", prefix,
			))
		}
	}
	for _, phase := range []string{"provision", "build", "verify", "publish"} {
		if !phases[phase] {
			violations = append(violations,
				"EXECUTOR_CAPABILITIES must contain phase:"+phase)
		}
	}
	if len(dimensions) == len(required) {
		parts := []string{
			dimensions["provider"], dimensions["zone"], dimensions["arch"],
			dimensions["build-mode"], dimensions["profile"],
		}
		imageID, imageGeneration, found := strings.Cut(
			dimensions["image"], "@",
		)
		if found && imageID != "" && imageGeneration != "" &&
			!strings.Contains(imageGeneration, "@") {
			parts = append(parts, imageID, imageGeneration)
			digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
			expectedPool := strings.Join(parts[:3], "-") + "-" +
				hex.EncodeToString(digest[:12])
			if dimensions["capacity-pool"] != expectedPool {
				violations = append(violations,
					"capacity-pool capability does not match its immutable dimensions")
			}
		}
	}
	return violations
}

func (c *ServerConfig) validatePublicRuntimeBoundary() []string {
	var violations []string
	addViolation(&violations, !c.Database.Enabled || !c.Database.Required,
		"PostgreSQL must be enabled and required")
	addViolation(&violations, strings.TrimSpace(c.CatalogPath) == "",
		"CATALOG_PATH must select an operator-owned immutable build catalog")
	addViolation(&violations, c.CloudPVEInsecure,
		"CLOUD_PVE_INSECURE must be false")
	addViolation(&violations, c.CloudSSHInsecureHostKey,
		"CLOUD_SSH_INSECURE_HOST_KEY must be false")
	addViolation(&violations, len(c.RemoteBuilders) != 0,
		"REMOTE_BUILDERS legacy push compatibility must be disabled")
	addViolation(&violations, c.BuilderToken != "",
		"BUILDER_TOKEN legacy shared credentials must be disabled")
	addViolation(&violations, c.RuntimeRole == "control-plane" || c.RuntimeRole == "",
		"SERVER_RUNTIME_ROLE must separate the public api from executor processes")
	return violations
}

func (c *ServerConfig) validatePublicStorageBoundary() []string {
	var violations []string
	addViolation(&violations, c.StorageType != "s3",
		"STORAGE_TYPE must be s3; shared local/NFS publication is not a public-service authority")
	addViolation(&violations, strings.TrimSpace(c.StorageS3Bucket) == "",
		"STORAGE_S3_BUCKET must select the artifact bucket")
	addViolation(&violations, strings.TrimSpace(c.StorageS3Region) == "",
		"STORAGE_S3_REGION must be explicit")
	addViolation(&violations, c.RuntimeRole == "executor" && !c.StorageS3AllowDelete,
		"executor STORAGE_S3_ALLOW_DELETE must be true for capability revocation; restrict its IAM DeleteObject grant to .quarantine/*")
	addViolation(&violations, c.RuntimeRole == "api" && c.StorageS3AllowDelete,
		"api STORAGE_S3_ALLOW_DELETE must be false; the public read path must not receive DeleteObject capability")
	endpoint := strings.TrimSpace(c.StorageS3Endpoint)
	addViolation(&violations, endpoint != "" && !validHTTPOrigin(endpoint),
		"STORAGE_S3_ENDPOINT must be an absolute HTTP(S) origin without credentials, path, query, or fragment")
	publicBaseURL := strings.TrimSpace(c.StorageS3PublicBaseURL)
	addViolation(&violations,
		publicBaseURL != "" && validateHTTPSEndpoint(publicBaseURL) != nil,
		"STORAGE_S3_PUBLIC_BASE_URL must be an absolute HTTPS URL")
	return violations
}

func (c *ServerConfig) validatePublicAPIBoundary() []string {
	var violations []string
	addViolation(&violations, c.RuntimeRole != "api",
		"the public listener process must use SERVER_RUNTIME_ROLE=api")
	addViolation(&violations, strings.TrimSpace(c.ControlPlaneID) == "",
		"CONTROL_PLANE_ID must be stable and non-empty")
	addViolation(&violations, c.hasProviderCredentials(),
		"the public api process must not receive provider or SSH credentials")
	addViolation(&violations, c.AuthMode != "oidc", "AUTH_MODE must be oidc")
	providerConfigured := len(c.IdentityProviders) > 0 ||
		(c.OIDCIssuerURL != "" && c.OIDCAudience != "")
	addViolation(&violations, !providerConfigured,
		"at least one OIDC or GitHub identity provider must be configured")
	addViolation(&violations,
		len(c.IdentityAdminSubjects) == 0 && len(c.OIDCAdminSubjects) == 0,
		"at least one immutable bootstrap administrator identity must be configured")
	addViolation(&violations, c.APIKey != "" || c.StepUpAPIKey != "",
		"legacy API_KEY and STEP_UP_API_KEY credentials must be unset")
	addViolation(&violations, !c.Cache.Enabled || !c.Cache.Required,
		"Redis must be enabled and required; edge rate limiting is still required independently")
	addViolation(&violations, len(c.CORSAllowedOrigins) == 0,
		"CORS_ALLOWED_ORIGINS must contain explicit HTTPS origins")
	for _, origin := range c.CORSAllowedOrigins {
		addViolation(&violations, !validPublicCORSOrigin(origin),
			"CORS_ALLOWED_ORIGINS must contain only absolute HTTPS origins without wildcards")
	}
	addViolation(&violations, c.OIDCAllowInsecureHTTP,
		"OIDC_ALLOW_INSECURE_HTTP must be false")
	addViolation(&violations,
		validateDeviceAuthorizationVerificationURI(
			c.DeviceAuthorizationVerificationURI, true,
		) != nil,
		"DEVICE_AUTHORIZATION_VERIFICATION_URI must be an absolute HTTPS /device URL without credentials, query, or fragment")
	for _, provider := range c.IdentityProviders {
		addViolation(&violations, provider.AllowInsecureHTTP,
			"identity provider "+provider.ID+" must not allow HTTP")
		if err := validateIdentityProvider(provider); err != nil {
			violations = append(violations,
				"identity provider "+provider.ID+": "+err.Error())
		}
	}
	addViolation(&violations, !c.GPGEnabled,
		"GPG_ENABLED must be true for the isolated signing queue")
	addViolation(&violations, c.GPGAutoCreate,
		"GPG_AUTO_CREATE must be false and an operator-approved release key must be selected")
	addViolation(&violations, strings.TrimSpace(c.GPGKeyID) == "",
		"GPG_KEY_ID must select the operator-approved release key")
	addViolation(&violations, !c.WorkerGatewayEnabled,
		"WORKER_GATEWAY_ENABLED must be true")
	addViolation(&violations, c.WorkerGatewayIssuerProvider != "vault",
		"WORKER_GATEWAY_ISSUER_PROVIDER must be vault")
	addViolation(&violations, c.PhaseExecutorMode != "active",
		"PHASE_EXECUTOR_MODE must be active")
	addViolation(&violations, c.MetricsEnabled && c.MetricsPassword == "",
		"METRICS_PASSWORD is required when metrics are enabled")
	return append(violations, c.validateWorkerGateway()...)
}

func (c *ServerConfig) hasProviderCredentials() bool {
	values := []string{
		c.CloudPVETokenID, c.CloudPVETokenSecret,
		c.CloudPVEUsername, c.CloudPVEPassword,
		c.CloudAliyunAK, c.CloudAliyunSK, c.CloudGCPKeyFile,
		c.CloudAWSAccessKey, c.CloudAWSSecretKey, c.CloudSSHKeyPath,
	}
	for _, value := range values {
		if value != "" {
			return true
		}
	}
	return false
}

func validPublicCORSOrigin(origin string) bool {
	parsed, err := url.Parse(strings.TrimSpace(origin))
	return err == nil && origin != "*" && parsed.Scheme == "https" &&
		parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" &&
		parsed.Fragment == ""
}

func addViolation(violations *[]string, condition bool, message string) {
	if condition {
		*violations = append(*violations, message)
	}
}

func (c *ServerConfig) validateAuth() []string {
	var warnings []string
	warnings = append(warnings, c.validateAuthMode()...)
	warnings = append(warnings, c.validateStepUpPolicy()...)
	warnings = append(warnings, c.validateOIDCSessionPolicy()...)
	if c.OIDCAllowInsecureHTTP {
		warnings = append(warnings, "SECURITY: OIDC issuer uses explicitly allowed HTTP; bearer tokens are safe only on a trusted private network")
	}
	return warnings
}

func (c *ServerConfig) validateAuthMode() []string {
	var warnings []string
	providerCount := len(c.IdentityProviders)
	if providerCount == 0 && c.OIDCIssuerURL != "" && c.OIDCAudience != "" {
		providerCount = 1
	}
	switch c.AuthMode {
	case "", "legacy":
		if c.APIKey == "" {
			warnings = append(warnings, "SECURITY: API_KEY is not set — all API endpoints are unauthenticated")
		}
	case "hybrid", "oidc":
		if !c.Database.Enabled || !c.Database.Required {
			warnings = append(warnings, "CONFIG: OIDC/hybrid auth requires DATABASE_ENABLED=true and DATABASE_REQUIRED=true")
		}
		if providerCount == 0 {
			warnings = append(warnings, "CONFIG: OIDC/hybrid auth requires AUTH_PROVIDERS_PATH or the legacy OIDC issuer/audience pair")
		}
		if c.AuthMode == "oidc" && len(c.IdentityAdminSubjects) == 0 &&
			len(c.OIDCAdminSubjects) == 0 {
			warnings = append(warnings, "CONFIG: OIDC mode requires at least one AUTH_ADMIN_IDENTITIES bootstrap administrator")
		}
		if c.AuthMode == "hybrid" && c.APIKey == "" {
			warnings = append(warnings, "CONFIG: hybrid auth requires API_KEY for the legacy administrator path")
		}
	default:
		warnings = append(warnings, "CONFIG: AUTH_MODE must be legacy, hybrid, or oidc")
	}
	for _, provider := range c.IdentityProviders {
		if err := validateIdentityProvider(provider); err != nil {
			warnings = append(warnings, "CONFIG: identity provider "+provider.ID+": "+err.Error())
		}
	}
	return warnings
}

func (c *ServerConfig) validateStepUpPolicy() []string {
	var warnings []string
	if c.AuthMode == "legacy" || c.AuthMode == "hybrid" {
		if c.APIKey != "" && c.StepUpAPIKey == "" {
			warnings = append(warnings, "SECURITY: STEP_UP_API_KEY is required for legacy administrator high-risk writes")
		} else if c.APIKey != "" && c.APIKey == c.StepUpAPIKey {
			warnings = append(warnings, "SECURITY: STEP_UP_API_KEY must differ from API_KEY")
		}
	}
	return warnings
}

func (c *ServerConfig) validateOIDCSessionPolicy() []string {
	var warnings []string
	if c.AuthMode == "hybrid" || c.AuthMode == "oidc" {
		if err := validateDeviceAuthorizationVerificationURI(
			c.DeviceAuthorizationVerificationURI, false,
		); err != nil {
			warnings = append(warnings,
				"CONFIG: DEVICE_AUTHORIZATION_VERIFICATION_URI "+err.Error())
		}
		if c.OIDCSessionIdleMinutes < 1 || c.OIDCSessionIdleMinutes > 7*24*60 {
			warnings = append(warnings, "CONFIG: OIDC_SESSION_IDLE_MINUTES must be in 1..10080")
		}
		if c.OIDCSessionMaxMinutes < 1 || c.OIDCSessionMaxMinutes > 30*24*60 {
			warnings = append(warnings, "CONFIG: OIDC_SESSION_MAX_MINUTES must be in 1..43200")
		}
		if c.OIDCStepUpMaxAgeMin < 1 || c.OIDCStepUpMaxAgeMin > 60 {
			warnings = append(warnings, "CONFIG: OIDC_STEP_UP_MAX_AGE_MINUTES must be in 1..60")
		}
	}
	return warnings
}

func (c *ServerConfig) validateCore() []string {
	var warnings []string
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
	return warnings
}

func (c *ServerConfig) validateDatabaseAndCache() []string {
	var warnings []string
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

func (c *ServerConfig) validateWorkerGateway() []string {
	var warnings []string
	warnings = append(warnings, c.validatePhaseExecutor()...)
	if !c.WorkerGatewayEnabled {
		return warnings
	}
	if !c.Database.Enabled || !c.Database.Required {
		warnings = append(warnings, "CONFIG: WORKER_GATEWAY_ENABLED requires DATABASE_ENABLED=true and DATABASE_REQUIRED=true")
	}
	if c.RuntimeRole == "executor" {
		if c.executorGatewayTrustIncomplete() {
			warnings = append(warnings, "CONFIG: executor requires Worker Gateway advertise URL plus server/client CA bundles")
		}
		if c.WorkerGatewayAdvertiseURL != "" &&
			validateHTTPSEndpoint(c.WorkerGatewayAdvertiseURL) != nil {
			warnings = append(warnings, "CONFIG: executor Worker Gateway advertise URL must be an absolute HTTPS URL")
		}
		if c.WorkerGatewayTLSKey != "" {
			warnings = append(warnings, "CONFIG: executor role must not receive Worker Gateway listener private keys")
		}
	} else {
		if c.WorkerGatewayPort <= 0 || c.WorkerGatewayPort > 65535 || c.WorkerGatewayPort == c.Port {
			warnings = append(warnings, "CONFIG: WORKER_GATEWAY_PORT must be a valid port distinct from SERVER_PORT")
		}
		if c.workerGatewayTLSIncomplete() {
			warnings = append(warnings, "CONFIG: worker gateway requires advertise URL, server TLS cert/key, and server/client CA bundles")
		}
	}
	switch c.WorkerGatewayIssuerProvider {
	case "file":
		if c.WorkerGatewayIssuerCert == "" ||
			c.WorkerGatewayIssuerKey == "" {
			warnings = append(warnings, "CONFIG: file worker issuer requires WORKER_GATEWAY_ISSUER_CERT and WORKER_GATEWAY_ISSUER_KEY")
		}
	case "vault":
		if c.WorkerGatewayVaultAddress == "" ||
			c.WorkerGatewayVaultMount == "" ||
			c.WorkerGatewayVaultRole == "" ||
			c.WorkerGatewayVaultTokenPath == "" {
			warnings = append(warnings, "CONFIG: Vault worker issuer requires address, mount, role, and token file")
		}
		if c.WorkerGatewayVaultTimeout <= 0 ||
			c.WorkerGatewayVaultTimeout > 60 {
			warnings = append(warnings, "CONFIG: WORKER_GATEWAY_VAULT_TIMEOUT_SECONDS must be in 1..60")
		}
	default:
		warnings = append(warnings, "CONFIG: WORKER_GATEWAY_ISSUER_PROVIDER must be file or vault")
	}
	if strings.TrimSpace(c.WorkerGatewayIssuerID) == "" ||
		len(c.WorkerGatewayIssuerID) > 128 {
		warnings = append(warnings, "CONFIG: WORKER_GATEWAY_ISSUER_ID must contain 1..128 characters")
	}
	if c.WorkerCertificateTTLMin <= 0 || c.WorkerCertificateTTLMin > 24*60 {
		warnings = append(warnings, "CONFIG: WORKER_CERTIFICATE_TTL_MINUTES must be in 1..1440")
	}
	return warnings
}

func (c *ServerConfig) validatePhaseExecutor() []string {
	var warnings []string
	if c.PhaseExecutorMode != "" && c.PhaseExecutorMode != "shadow" &&
		c.PhaseExecutorMode != "active" {
		warnings = append(warnings, "CONFIG: PHASE_EXECUTOR_MODE must be shadow or active")
	}
	if c.PhaseExecutorMode == "active" {
		if !c.Database.Enabled || !c.Database.Required {
			warnings = append(warnings, "CONFIG: active phase executor requires DATABASE_ENABLED=true and DATABASE_REQUIRED=true")
		}
		if !c.WorkerGatewayEnabled {
			warnings = append(warnings, "CONFIG: active phase executor requires WORKER_GATEWAY_ENABLED=true")
		}
		if len(c.RemoteBuilders) > 0 {
			warnings = append(warnings, "CONFIG: active phase executor does not support legacy REMOTE_BUILDERS")
		}
	}
	seenZones := make(map[string]struct{}, len(c.ExecutorZones))
	for _, zone := range c.ExecutorZones {
		if !executorZonePattern.MatchString(zone) {
			warnings = append(warnings, "CONFIG: EXECUTOR_ZONES entries must use lowercase stable IDs")
			break
		}
		if _, exists := seenZones[zone]; exists {
			warnings = append(warnings, "CONFIG: EXECUTOR_ZONES contains a duplicate")
			break
		}
		seenZones[zone] = struct{}{}
	}
	seenCapabilities := make(map[string]struct{}, len(c.ExecutorCapabilities))
	for _, capability := range c.ExecutorCapabilities {
		if !executorCapabilityPattern.MatchString(capability) {
			warnings = append(warnings, "CONFIG: EXECUTOR_CAPABILITIES contains an invalid label")
			break
		}
		if _, exists := seenCapabilities[capability]; exists {
			warnings = append(warnings, "CONFIG: EXECUTOR_CAPABILITIES contains a duplicate label")
			break
		}
		seenCapabilities[capability] = struct{}{}
	}
	if c.PhaseExecutorMode == "active" && len(c.ExecutorCapabilities) > 0 {
		hasPhase := false
		for _, capability := range c.ExecutorCapabilities {
			if strings.HasPrefix(capability, "phase:") {
				hasPhase = true
				break
			}
		}
		if !hasPhase {
			warnings = append(warnings, "CONFIG: explicit EXECUTOR_CAPABILITIES requires at least one phase label")
		}
	}
	if c.SchedulerAutoscaleMode != "off" &&
		c.SchedulerAutoscaleMode != "observe" &&
		c.SchedulerAutoscaleMode != "actuate" {
		warnings = append(
			warnings,
			"CONFIG: SCHEDULER_AUTOSCALE_MODE must be off, observe, or actuate",
		)
	}
	if c.SchedulerAutoscaleMode == "actuate" &&
		(!c.SchedulerAutoscaleProviderLimitsOK ||
			len(c.SchedulerAutoscaleProviderMaxSlots) == 0) {
		warnings = append(
			warnings,
			"CONFIG: actuate mode requires valid SCHEDULER_AUTOSCALE_PROVIDER_MAX_SLOTS entries",
		)
	}
	if c.SchedulerAutoscaleMinSlots < 0 ||
		c.SchedulerAutoscaleMaxSlots < c.SchedulerAutoscaleMinSlots ||
		c.SchedulerAutoscaleMaxSlots > 10000 ||
		c.SchedulerAutoscaleTargetReady <= 0 ||
		c.SchedulerAutoscaleTargetReady > 1000 {
		warnings = append(
			warnings,
			"CONFIG: scheduler autoscale slot bounds or target are invalid",
		)
	}
	if c.SchedulerAutoscaleCooldownSeconds < 0 ||
		c.SchedulerAutoscaleCooldownSeconds > 86400 ||
		c.SchedulerAutoscaleScaleDownSeconds < 0 ||
		c.SchedulerAutoscaleScaleDownSeconds > 7*86400 ||
		c.SchedulerAutoscaleIntervalSeconds < 5 ||
		c.SchedulerAutoscaleIntervalSeconds > 3600 {
		warnings = append(
			warnings,
			"CONFIG: scheduler autoscale timing is invalid",
		)
	}
	warnings = append(warnings, c.validateDistCC()...)
	return warnings
}

func (c *ServerConfig) validateDistCC() []string {
	if !c.DistCCAlphaEnabled {
		return nil
	}
	var warnings []string
	if !c.Database.Enabled || !c.Database.Required {
		warnings = append(warnings, "CONFIG: DISTCC_ALPHA_ENABLED requires PostgreSQL required mode")
	}
	if len(c.DistCCPackageAllowlist) == 0 || c.DistCCCHOST == "" ||
		c.DistCCCompilerDigest == "" || c.DistCCToolchainImageGeneration == "" ||
		c.DistCCNetworkZone == "" || len(c.DistCCIsolatedNetworkCIDRs) == 0 {
		warnings = append(warnings, "CONFIG: enabled distcc requires reviewed allowlist, exact toolchain dimensions, zone, and isolated network CIDR")
	}
	if c.DistCCFallbackPolicy != "local" && c.DistCCFallbackPolicy != "blocked" {
		warnings = append(warnings, "CONFIG: DISTCC_FALLBACK_POLICY must be local or blocked")
	}
	if c.DistCCSlotsPerJob < 1 || c.DistCCSlotsPerJob > 256 ||
		c.DistCCLeaseSeconds < 5 || c.DistCCLeaseSeconds > 3600 ||
		c.DistCCWorkerFreshnessSeconds < 5 || c.DistCCWorkerFreshnessSeconds > 600 {
		warnings = append(warnings, "CONFIG: distcc slot, lease, or heartbeat freshness bounds are invalid")
	}
	for _, cidr := range c.DistCCIsolatedNetworkCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			warnings = append(warnings, "CONFIG: DISTCC_ISOLATED_NETWORK_CIDRS contains an invalid CIDR")
			break
		}
	}
	return warnings
}

func (c *ServerConfig) workerGatewayTLSIncomplete() bool {
	return c.WorkerGatewayAdvertiseURL == "" || c.WorkerGatewayTLSCert == "" ||
		c.WorkerGatewayTLSKey == "" || c.WorkerGatewayServerCA == "" ||
		c.WorkerGatewayClientCA == "" ||
		c.WorkerGatewayIssuerID == "" || c.WorkerGatewayIssuerProvider == ""
}

func (c *ServerConfig) executorGatewayTrustIncomplete() bool {
	return c.WorkerGatewayAdvertiseURL == "" ||
		c.WorkerGatewayServerCA == "" || c.WorkerGatewayClientCA == "" ||
		c.WorkerGatewayIssuerID == "" || c.WorkerGatewayIssuerProvider == ""
}

// DashboardConfig represents the dashboard configuration.
type DashboardConfig struct {
	DeploymentMode        string
	Port                  int
	ServerURL             string
	ServerAPIKey          string // API key forwarded to the backend server (empty = none)
	ServerStepUpAPIKey    string // Separate high-risk-write key, forwarded only after local re-auth
	AuthEnabled           bool
	JWTSecret             string
	AdminUser             string // Username accepted by the login handler
	AdminPassword         string // Password accepted by the login handler
	TokenTTLMinutes       int    // Issued-token lifetime in minutes
	AllowAnonymous        bool
	CookieSecure          bool // Mark dashboard session/flow cookies Secure behind an HTTPS proxy
	OIDCEnabled           bool
	OIDCIssuerURL         string
	OIDCClientID          string
	OIDCClientSecret      string
	OIDCRedirectURL       string
	OIDCAllowInsecureHTTP bool
	IdentityProvidersPath string
	IdentityProviders     []IdentityProviderConfig
	MetricsEnabled        bool
	MetricsPort           string
	MetricsPassword       string
}

// Validate checks the dashboard configuration for common misconfigurations.
// Returns an error if a critical security issue is found.
func (c *DashboardConfig) Validate() error {
	if !validHTTPOrigin(c.ServerURL) {
		return fmt.Errorf("SERVER_URL must be an HTTP or HTTPS origin without credentials, path, query, or fragment")
	}
	providerLoginEnabled := c.OIDCEnabled || len(c.IdentityProviders) > 0
	if err := c.validateDashboardDeployment(providerLoginEnabled); err != nil {
		return err
	}
	if providerLoginEnabled && !c.AuthEnabled {
		return fmt.Errorf("identity provider login requires AUTH_ENABLED=true")
	}
	if !c.AuthEnabled {
		return nil
	}
	if err := c.validateDashboardSession(); err != nil {
		return err
	}
	if !c.AllowAnonymous && !providerLoginEnabled &&
		(c.AdminUser == "" || c.AdminPassword == "") {
		return fmt.Errorf(
			"SECURITY: ALLOW_ANONYMOUS is false but ADMIN_USER/ADMIN_PASSWORD are not set; " +
				"set credentials so operators can log in",
		)
	}
	if len(c.IdentityProviders) > 0 {
		for _, provider := range c.IdentityProviders {
			if err := validateIdentityProvider(provider); err != nil {
				return fmt.Errorf("identity provider %s: %w", provider.ID, err)
			}
		}
		return nil
	}
	if c.OIDCEnabled {
		return c.validateDashboardOIDC()
	}
	return nil
}

func (c *DashboardConfig) validateDashboardDeployment(providerLoginEnabled bool) error {
	mode := strings.ToLower(strings.TrimSpace(c.DeploymentMode))
	if mode == "" {
		mode = DeploymentModeTrusted
	}
	if mode != DeploymentModeTrusted && mode != DeploymentModePublic {
		return fmt.Errorf(
			"DEPLOYMENT_MODE must be %q or %q",
			DeploymentModeTrusted, DeploymentModePublic,
		)
	}
	if mode != DeploymentModePublic {
		return nil
	}

	var violations []string
	add := func(condition bool, message string) {
		if condition {
			violations = append(violations, message)
		}
	}
	add(!c.AuthEnabled, "AUTH_ENABLED must be true")
	add(c.AllowAnonymous, "ALLOW_ANONYMOUS must be false")
	add(!providerLoginEnabled, "at least one OIDC or GitHub identity provider is required")
	add(!c.CookieSecure, "COOKIE_SECURE must be true behind the public TLS edge")
	add(c.OIDCAllowInsecureHTTP, "OIDC_ALLOW_INSECURE_HTTP must be false")
	add(c.AdminUser != "" || c.AdminPassword != "",
		"local ADMIN_USER and ADMIN_PASSWORD login must be disabled")
	add(c.ServerAPIKey != "" || c.ServerStepUpAPIKey != "",
		"dashboard legacy server API keys must be unset")
	add(c.MetricsEnabled && c.MetricsPassword == "",
		"METRICS_PASSWORD is required when metrics are enabled")

	for _, provider := range c.IdentityProviders {
		add(provider.AllowInsecureHTTP,
			"identity provider "+provider.ID+" must not allow HTTP")
		redirect, err := url.Parse(provider.RedirectURL)
		add(err != nil || redirect.Scheme != "https",
			"identity provider "+provider.ID+" must use an HTTPS redirect URL")
	}
	if c.OIDCEnabled {
		issuer, issuerErr := url.Parse(c.OIDCIssuerURL)
		redirect, redirectErr := url.Parse(c.OIDCRedirectURL)
		add(issuerErr != nil || issuer.Scheme != "https",
			"OIDC_ISSUER_URL must use HTTPS")
		add(redirectErr != nil || redirect.Scheme != "https",
			"OIDC_REDIRECT_URL must use HTTPS")
	}

	if len(violations) > 0 {
		return fmt.Errorf(
			"public dashboard configuration rejected: %s",
			strings.Join(violations, "; "),
		)
	}
	return nil
}

func validateIdentityProvider(provider IdentityProviderConfig) error {
	if !identityProviderIDPattern.MatchString(provider.ID) {
		return fmt.Errorf("id must match %s", identityProviderIDPattern)
	}
	if strings.TrimSpace(provider.DisplayName) == "" ||
		len(strings.TrimSpace(provider.DisplayName)) > 80 {
		return fmt.Errorf("display_name is required and must not exceed 80 characters")
	}
	if provider.ClientID == "" || provider.RedirectURL == "" {
		return fmt.Errorf("client_id and redirect_url are required")
	}
	redirect, err := parseOIDCEndpoint(provider.RedirectURL)
	if err != nil {
		return fmt.Errorf("redirect_url must be an absolute HTTP(S) URL")
	}
	if !provider.AllowInsecureHTTP && insecureNonLoopback(redirect) {
		return fmt.Errorf("HTTP redirect requires allow_insecure_http=true outside loopback")
	}
	switch provider.Type {
	case "oidc":
		return validateOIDCProvider(provider)
	case "github":
		return validateGitHubProvider(provider)
	default:
		return fmt.Errorf("type must be oidc or github")
	}
}

func validateOIDCProvider(provider IdentityProviderConfig) error {
	if provider.IssuerURL == "" || provider.Audience == "" {
		return fmt.Errorf("OIDC requires issuer_url and audience")
	}
	issuer, err := parseOIDCEndpoint(provider.IssuerURL)
	if err != nil {
		return fmt.Errorf("issuer_url must be an absolute HTTP(S) URL")
	}
	if !provider.AllowInsecureHTTP && insecureNonLoopback(issuer) {
		return fmt.Errorf("HTTP issuer requires allow_insecure_http=true outside loopback")
	}
	if provider.BackchannelLogout &&
		(provider.BackchannelMaxAgeSeconds < 30 ||
			provider.BackchannelMaxAgeSeconds > 900) {
		return fmt.Errorf("backchannel_max_age_seconds must be in 30..900 when back-channel logout is enabled")
	}
	return nil
}

func validateGitHubProvider(provider IdentityProviderConfig) error {
	if provider.BackchannelLogout {
		return fmt.Errorf("GitHub OAuth does not support OIDC back-channel logout")
	}
	if provider.ClientSecret == "" {
		return fmt.Errorf("GitHub requires a populated client_secret_env")
	}
	endpoints := []struct {
		label string
		raw   string
	}{
		{"authorization_url", provider.AuthorizationURL},
		{"token_url", provider.TokenURL},
		{"api_base_url", provider.APIBaseURL},
	}
	for _, endpoint := range endpoints {
		if err := validateHTTPSEndpoint(endpoint.raw); err != nil {
			return fmt.Errorf("%s %w", endpoint.label, err)
		}
	}
	return nil
}

func validateHTTPSEndpoint(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	return nil
}

func validateDeviceAuthorizationVerificationURI(raw string, requireHTTPS bool) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/device" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("must be an absolute HTTP(S) /device URL without credentials, query, or fragment")
	}
	if requireHTTPS && parsed.Scheme != "https" {
		return fmt.Errorf("must use HTTPS")
	}
	return nil
}

// Validate enforces the fail-closed protocol and transport contract for one
// identity provider.
func (provider IdentityProviderConfig) Validate() error {
	return validateIdentityProvider(provider)
}

func validHTTPOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil &&
		(parsed.Scheme == "http" || parsed.Scheme == "https") &&
		parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" &&
		parsed.Fragment == "" && (parsed.Path == "" || parsed.Path == "/")
}

func (c *DashboardConfig) validateDashboardSession() error {
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
	return nil
}

func (c *DashboardConfig) validateDashboardOIDC() error {
	if c.OIDCIssuerURL == "" || c.OIDCClientID == "" || c.OIDCRedirectURL == "" {
		return fmt.Errorf("OIDC dashboard login requires OIDC_ISSUER_URL, OIDC_CLIENT_ID, and OIDC_REDIRECT_URL")
	}
	issuer, err := parseOIDCEndpoint(c.OIDCIssuerURL)
	if err != nil {
		return fmt.Errorf("OIDC_ISSUER_URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	redirect, err := parseOIDCEndpoint(c.OIDCRedirectURL)
	if err != nil {
		return fmt.Errorf("OIDC_REDIRECT_URL must be an absolute HTTP(S) URL")
	}
	if !c.OIDCAllowInsecureHTTP && insecureNonLoopback(issuer) {
		return fmt.Errorf("HTTP OIDC issuer requires OIDC_ALLOW_INSECURE_HTTP=true outside loopback")
	}
	if !c.OIDCAllowInsecureHTTP && insecureNonLoopback(redirect) {
		return fmt.Errorf("HTTP OIDC redirect requires OIDC_ALLOW_INSECURE_HTTP=true outside loopback")
	}
	if redirect.Scheme == "https" && !c.CookieSecure {
		return fmt.Errorf("HTTPS OIDC redirect requires COOKIE_SECURE=true")
	}
	return nil
}

func parseOIDCEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid OIDC endpoint")
	}
	return parsed, nil
}

func insecureNonLoopback(endpoint *url.URL) bool {
	return endpoint.Scheme == "http" &&
		endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "localhost"
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
	BuildFeatures          string
	StorageType            string
	StorageLocalDir        string
	StorageS3Bucket        string
	StorageS3Region        string
	StorageS3Prefix        string
	StorageS3Endpoint      string
	StorageS3UsePathStyle  bool
	StorageS3PublicBaseURL string
	StorageHTTPBase        string
	ServerURL              string
	// ServerAPIKey is the central server's API key, attached to registration
	// and heartbeat calls (required when the server sets API_KEY).
	ServerAPIKey string
	// AdvertiseURL is the URL this builder registers with the server (how the
	// server reaches it). Defaults to http://<hostname>:<port>.
	AdvertiseURL                   string
	PullEnabled                    bool
	WorkerGatewayURL               string
	WorkerTLSCert                  string
	WorkerTLSKey                   string
	WorkerTLSCA                    string
	NotifyConfig                   string
	MetricsEnabled                 bool
	MetricsPort                    string
	MetricsPassword                string
	DistCCAlphaEnabled             bool
	DistCCPackageAllowlist         []string
	DistCCCHOST                    string
	DistCCCompilerDigest           string
	DistCCToolchainImageGeneration string
	DistCCCPUFeatures              []string
	DistCCNetworkZone              string
	DistCCIsolatedNetworkCIDRs     []string

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
	if !c.PullEnabled && (c.Port <= 0 || c.Port > 65535) {
		warnings = append(warnings, fmt.Sprintf("CONFIG: BUILDER_PORT %d is invalid, must be 1-65535", c.Port))
	}
	if !c.PullEnabled && c.AuthToken == "" {
		warnings = append(warnings, "SECURITY: BUILDER_TOKEN is not set — the build endpoint is unauthenticated and allows arbitrary remote builds")
	}
	if c.PullEnabled && (c.WorkerGatewayURL == "" || c.WorkerTLSCert == "" ||
		c.WorkerTLSKey == "" || c.WorkerTLSCA == "") {
		warnings = append(warnings, "CONFIG: pull mode requires WORKER_GATEWAY_URL and worker TLS cert/key/CA paths")
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
	if c.DistCCAlphaEnabled {
		if len(c.DistCCPackageAllowlist) == 0 || c.DistCCCHOST == "" ||
			c.DistCCCompilerDigest == "" || c.DistCCToolchainImageGeneration == "" ||
			c.DistCCNetworkZone == "" || len(c.DistCCIsolatedNetworkCIDRs) == 0 {
			warnings = append(warnings, "CONFIG: enabled distcc builder requires reviewed allowlist, exact toolchain dimensions, zone, and isolated network CIDR")
		}
		for _, cidr := range c.DistCCIsolatedNetworkCIDRs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				warnings = append(warnings, "CONFIG: DISTCC_ISOLATED_NETWORK_CIDRS contains an invalid CIDR")
				break
			}
		}
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
	// #nosec G304 -- the operator explicitly supplies the config path on the
	// local command line; it is not derived from an HTTP request or tenant.
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
		DeploymentMode:                     DeploymentModeTrusted,
		Port:                               8080,
		RuntimeRole:                        "control-plane",
		BinpkgPath:                         "/var/cache/binpkgs",
		MaxWorkers:                         5,
		BuildMode:                          "remote",
		StorageType:                        "local",
		StorageLocalDir:                    "/var/cache/binpkgs",
		GPGEnabled:                         false,
		CloudProvider:                      "gcp",
		CloudGCPProject:                    "portage-engine",
		CloudGCPRegion:                     "us-central1",
		CloudGCPZone:                       "us-central1-a",
		WorkerGatewayPort:                  9443,
		WorkerGatewayIssuerID:              "portage-engine-local",
		WorkerGatewayIssuerProvider:        "file",
		WorkerGatewayVaultMount:            "pki",
		WorkerGatewayVaultRole:             "portage-worker",
		WorkerGatewayVaultTimeout:          15,
		WorkerCertificateTTLMin:            180,
		ExecutorZones:                      []string{"default"},
		SchedulerAutoscaleMode:             "observe",
		SchedulerAutoscaleMinSlots:         1,
		SchedulerAutoscaleMaxSlots:         64,
		SchedulerAutoscaleTargetReady:      2,
		SchedulerAutoscaleCooldownSeconds:  60,
		SchedulerAutoscaleScaleDownSeconds: 600,
		SchedulerAutoscaleIntervalSeconds:  15,
		SchedulerAutoscaleProviderLimitsOK: true,
		DistCCNetworkZone:                  "default",
		DistCCSlotsPerJob:                  1,
		DistCCLeaseSeconds:                 60,
		DistCCWorkerFreshnessSeconds:       45,
		DistCCFallbackPolicy:               "local",
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
	config.DeploymentMode = strings.ToLower(getEnvString(
		env, "DEPLOYMENT_MODE", config.DeploymentMode,
	))
	config.ControlPlaneID = getEnvString(env, "CONTROL_PLANE_ID", "")
	config.RuntimeRole = strings.ToLower(getEnvString(
		env, "SERVER_RUNTIME_ROLE", config.RuntimeRole,
	))
	config.BinpkgPath = getEnvString(env, "BINPKG_PATH", config.BinpkgPath)
	config.MaxWorkers = getEnvInt(env, "MAX_WORKERS", config.MaxWorkers)
	config.BuildMode = getEnvString(env, "BUILD_MODE", config.BuildMode)

	config.StorageType = getEnvString(env, "STORAGE_TYPE", config.StorageType)
	config.StorageLocalDir = getEnvString(env, "STORAGE_LOCAL_DIR", config.StorageLocalDir)
	config.StorageS3Bucket = getEnvString(env, "STORAGE_S3_BUCKET", "")
	config.StorageS3Region = getEnvString(env, "STORAGE_S3_REGION", "")
	config.StorageS3Prefix = getEnvString(env, "STORAGE_S3_PREFIX", "")
	config.StorageS3Endpoint = getEnvString(env, "STORAGE_S3_ENDPOINT", "")
	config.StorageS3UsePathStyle = getEnvBool(env, "STORAGE_S3_USE_PATH_STYLE", false)
	config.StorageS3PublicBaseURL = getEnvString(env, "STORAGE_S3_PUBLIC_BASE_URL", "")
	config.StorageS3AllowDelete = getEnvBool(env, "STORAGE_S3_ALLOW_DELETE", false)
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
	config.StepUpAPIKey = getEnvString(env, "STEP_UP_API_KEY", "")
	config.AuthMode = strings.ToLower(getEnvString(env, "AUTH_MODE", "legacy"))
	config.OIDCIssuerURL = getEnvString(env, "OIDC_ISSUER_URL", "")
	config.OIDCAudience = getEnvString(env, "OIDC_AUDIENCE", "")
	config.OIDCAdminSubjects = getEnvStringSlice(env, "OIDC_ADMIN_SUBJECTS", nil)
	config.OIDCAllowInsecureHTTP = getEnvBool(env, "OIDC_ALLOW_INSECURE_HTTP", false)
	config.OIDCSessionIdleMinutes = getEnvInt(env, "OIDC_SESSION_IDLE_MINUTES", 60)
	config.OIDCSessionMaxMinutes = getEnvInt(env, "OIDC_SESSION_MAX_MINUTES", 720)
	config.OIDCStepUpMaxAgeMin = getEnvInt(env, "OIDC_STEP_UP_MAX_AGE_MINUTES", 10)
	config.OIDCStepUpAMRValues = getEnvStringSlice(env, "OIDC_STEP_UP_AMR_VALUES", nil)
	config.OIDCStepUpACRValues = getEnvStringSlice(env, "OIDC_STEP_UP_ACR_VALUES", nil)
	config.DeviceAuthorizationVerificationURI = getEnvString(
		env, "DEVICE_AUTHORIZATION_VERIFICATION_URI", "",
	)
	config.IdentityProvidersPath = getEnvString(env, "AUTH_PROVIDERS_PATH", "")
	config.IdentityAdminSubjects = getEnvStringSlice(env, "AUTH_ADMIN_IDENTITIES", nil)
	if config.IdentityProvidersPath != "" {
		providers, err := loadIdentityProviders(config.IdentityProvidersPath, path)
		if err != nil {
			return nil, err
		}
		config.IdentityProviders = providers
	}
	config.BuilderToken = getEnvString(env, "BUILDER_TOKEN", "")
	config.CORSAllowedOrigins = getEnvStringSlice(env, "CORS_ALLOWED_ORIGINS", nil)
	config.MaxRequestBodyBytes = int64(getEnvInt(env, "MAX_REQUEST_BODY_BYTES", 10*1024*1024)) // Default 10MB
	config.DataDir = getEnvString(env, "DATA_DIR", "/var/lib/portage-engine/server")
	config.CatalogPath = getEnvString(env, "CATALOG_PATH", "")
	config.ImageFactoryStatusPath = getEnvString(env, "IMAGE_FACTORY_STATUS_PATH", "")
	config.WorkerGatewayEnabled = getEnvBool(env, "WORKER_GATEWAY_ENABLED", false)
	config.WorkerGatewayPort = getEnvInt(env, "WORKER_GATEWAY_PORT", config.WorkerGatewayPort)
	config.WorkerGatewayAdvertiseURL = getEnvString(env, "WORKER_GATEWAY_ADVERTISE_URL", "")
	config.WorkerGatewayTLSCert = getEnvString(env, "WORKER_GATEWAY_TLS_CERT", "")
	config.WorkerGatewayTLSKey = getEnvString(env, "WORKER_GATEWAY_TLS_KEY", "")
	config.WorkerGatewayServerCA = getEnvString(env, "WORKER_GATEWAY_SERVER_CA", "")
	config.WorkerGatewayClientCA = getEnvString(env, "WORKER_GATEWAY_CLIENT_CA", "")
	config.WorkerGatewayIssuerID = getEnvString(
		env, "WORKER_GATEWAY_ISSUER_ID", config.WorkerGatewayIssuerID,
	)
	config.WorkerGatewayIssuerProvider = strings.ToLower(getEnvString(
		env, "WORKER_GATEWAY_ISSUER_PROVIDER", config.WorkerGatewayIssuerProvider,
	))
	config.WorkerGatewayIssuerCert = getEnvString(env, "WORKER_GATEWAY_ISSUER_CERT", "")
	config.WorkerGatewayIssuerKey = getEnvString(env, "WORKER_GATEWAY_ISSUER_KEY", "")
	config.WorkerGatewayVaultAddress = getEnvString(env, "WORKER_GATEWAY_VAULT_ADDRESS", "")
	config.WorkerGatewayVaultMount = getEnvString(
		env, "WORKER_GATEWAY_VAULT_MOUNT", config.WorkerGatewayVaultMount,
	)
	config.WorkerGatewayVaultRole = getEnvString(
		env, "WORKER_GATEWAY_VAULT_ROLE", config.WorkerGatewayVaultRole,
	)
	config.WorkerGatewayVaultTokenPath = getEnvString(env, "WORKER_GATEWAY_VAULT_TOKEN_FILE", "")
	config.WorkerGatewayVaultNamespace = getEnvString(env, "WORKER_GATEWAY_VAULT_NAMESPACE", "")
	config.WorkerGatewayVaultServerCA = getEnvString(env, "WORKER_GATEWAY_VAULT_SERVER_CA", "")
	config.WorkerGatewayVaultTimeout = getEnvInt(
		env, "WORKER_GATEWAY_VAULT_TIMEOUT_SECONDS",
		config.WorkerGatewayVaultTimeout,
	)
	config.WorkerCertificateTTLMin = getEnvInt(env, "WORKER_CERTIFICATE_TTL_MINUTES", config.WorkerCertificateTTLMin)
	config.PhaseExecutorMode = strings.ToLower(getEnvString(env, "PHASE_EXECUTOR_MODE", "shadow"))
	config.ExecutorZones = getEnvStringSlice(env, "EXECUTOR_ZONES", config.ExecutorZones)
	config.ExecutorCapabilities = getEnvStringSlice(env, "EXECUTOR_CAPABILITIES", nil)
	config.ExecutorCapacityInstanceID = getEnvString(
		env, "EXECUTOR_CAPACITY_INSTANCE_ID", "",
	)
	config.SchedulerAutoscaleMode = strings.ToLower(getEnvString(
		env, "SCHEDULER_AUTOSCALE_MODE", config.SchedulerAutoscaleMode,
	))
	config.SchedulerAutoscaleMinSlots = getEnvInt(
		env, "SCHEDULER_AUTOSCALE_MIN_SLOTS", config.SchedulerAutoscaleMinSlots,
	)
	config.SchedulerAutoscaleMaxSlots = getEnvInt(
		env, "SCHEDULER_AUTOSCALE_MAX_SLOTS", config.SchedulerAutoscaleMaxSlots,
	)
	config.SchedulerAutoscaleTargetReady = getEnvInt(
		env, "SCHEDULER_AUTOSCALE_TARGET_READY_PER_SLOT",
		config.SchedulerAutoscaleTargetReady,
	)
	config.SchedulerAutoscaleCooldownSeconds = getEnvInt(
		env, "SCHEDULER_AUTOSCALE_COOLDOWN_SECONDS",
		config.SchedulerAutoscaleCooldownSeconds,
	)
	config.SchedulerAutoscaleScaleDownSeconds = getEnvInt(
		env, "SCHEDULER_AUTOSCALE_SCALE_DOWN_SECONDS",
		config.SchedulerAutoscaleScaleDownSeconds,
	)
	config.SchedulerAutoscaleIntervalSeconds = getEnvInt(
		env, "SCHEDULER_AUTOSCALE_INTERVAL_SECONDS",
		config.SchedulerAutoscaleIntervalSeconds,
	)
	config.SchedulerAutoscaleProviderMaxSlots,
		config.SchedulerAutoscaleProviderLimitsOK = parseProviderSlotLimits(
		getEnvString(env, "SCHEDULER_AUTOSCALE_PROVIDER_MAX_SLOTS", ""),
	)
	config.DistCCAlphaEnabled = getEnvBool(env, "DISTCC_ALPHA_ENABLED", false)
	config.DistCCPackageAllowlist = getEnvStringSlice(env, "DISTCC_PACKAGE_ALLOWLIST", nil)
	config.DistCCCHOST = getEnvString(env, "DISTCC_CHOST", "")
	config.DistCCCompilerDigest = getEnvString(env, "DISTCC_COMPILER_DIGEST", "")
	config.DistCCToolchainImageGeneration = getEnvString(env, "DISTCC_TOOLCHAIN_IMAGE_GENERATION", "")
	config.DistCCCPUFeatures = getEnvStringSlice(env, "DISTCC_CPU_FEATURES", nil)
	config.DistCCNetworkZone = getEnvString(env, "DISTCC_NETWORK_ZONE", config.DistCCNetworkZone)
	config.DistCCIsolatedNetworkCIDRs = getEnvStringSlice(env, "DISTCC_ISOLATED_NETWORK_CIDRS", nil)
	config.DistCCSlotsPerJob = getEnvInt(env, "DISTCC_SLOTS_PER_JOB", config.DistCCSlotsPerJob)
	config.DistCCLeaseSeconds = getEnvInt(env, "DISTCC_LEASE_SECONDS", config.DistCCLeaseSeconds)
	config.DistCCWorkerFreshnessSeconds = getEnvInt(env, "DISTCC_WORKER_FRESHNESS_SECONDS", config.DistCCWorkerFreshnessSeconds)
	config.DistCCFallbackPolicy = strings.ToLower(getEnvString(env, "DISTCC_FALLBACK_POLICY", config.DistCCFallbackPolicy))
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
		DeploymentMode:  DeploymentModeTrusted,
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
	config.DeploymentMode = strings.ToLower(getEnvString(
		env, "DEPLOYMENT_MODE", config.DeploymentMode,
	))
	config.ServerURL = getEnvString(env, "SERVER_URL", config.ServerURL)
	config.ServerAPIKey = getEnvString(env, "SERVER_API_KEY", "")
	config.ServerStepUpAPIKey = getEnvString(env, "SERVER_STEP_UP_API_KEY", "")
	config.AuthEnabled = getEnvBool(env, "AUTH_ENABLED", config.AuthEnabled)
	config.JWTSecret = getEnvString(env, "JWT_SECRET", config.JWTSecret)
	config.AdminUser = getEnvString(env, "ADMIN_USER", "")
	config.AdminPassword = getEnvString(env, "ADMIN_PASSWORD", "")
	config.TokenTTLMinutes = getEnvInt(env, "TOKEN_TTL_MINUTES", 720)
	config.AllowAnonymous = getEnvBool(env, "ALLOW_ANONYMOUS", config.AllowAnonymous)
	config.CookieSecure = getEnvBool(env, "COOKIE_SECURE", false)
	config.OIDCEnabled = getEnvBool(env, "OIDC_ENABLED", false)
	config.OIDCIssuerURL = getEnvString(env, "OIDC_ISSUER_URL", "")
	config.OIDCClientID = getEnvString(env, "OIDC_CLIENT_ID", "")
	config.OIDCClientSecret = getEnvString(env, "OIDC_CLIENT_SECRET", "")
	config.OIDCRedirectURL = getEnvString(env, "OIDC_REDIRECT_URL", "")
	config.OIDCAllowInsecureHTTP = getEnvBool(env, "OIDC_ALLOW_INSECURE_HTTP", false)
	config.IdentityProvidersPath = getEnvString(env, "AUTH_PROVIDERS_PATH", "")
	if config.IdentityProvidersPath != "" {
		providers, err := loadIdentityProviders(config.IdentityProvidersPath, path)
		if err != nil {
			return nil, err
		}
		config.IdentityProviders = providers
	}

	config.MetricsEnabled = getEnvBool(env, "METRICS_ENABLED", false)
	config.MetricsPort = getEnvString(env, "METRICS_PORT", "2112")
	config.MetricsPassword = getEnvString(env, "METRICS_PASSWORD", "")

	return config, nil
}

func loadIdentityProviders(rawPath, configPath string) ([]IdentityProviderConfig, error) {
	resolved := rawPath
	if !filepath.IsAbs(resolved) {
		base := filepath.Dir(configPath)
		if base == "." && configPath == "" {
			base = "."
		}
		resolved = filepath.Join(base, resolved)
	}
	// #nosec G304 -- the deployment operator explicitly selects this config file.
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read AUTH_PROVIDERS_PATH: %w", err)
	}
	var document identityProviderDocument
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode AUTH_PROVIDERS_PATH: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode AUTH_PROVIDERS_PATH: trailing JSON content")
	}
	if len(document.Providers) == 0 || len(document.Providers) > 32 {
		return nil, fmt.Errorf("AUTH_PROVIDERS_PATH must define 1..32 providers")
	}
	seen := make(map[string]struct{}, len(document.Providers))
	for index := range document.Providers {
		provider := &document.Providers[index]
		provider.ID = strings.TrimSpace(provider.ID)
		provider.Type = strings.ToLower(strings.TrimSpace(provider.Type))
		provider.DisplayName = strings.TrimSpace(provider.DisplayName)
		provider.IssuerURL = strings.TrimRight(strings.TrimSpace(provider.IssuerURL), "/")
		provider.Audience = strings.TrimSpace(provider.Audience)
		provider.ClientID = strings.TrimSpace(provider.ClientID)
		provider.ClientSecretEnv = strings.TrimSpace(provider.ClientSecretEnv)
		provider.RedirectURL = strings.TrimSpace(provider.RedirectURL)
		provider.AuthorizationURL = strings.TrimSpace(provider.AuthorizationURL)
		provider.TokenURL = strings.TrimSpace(provider.TokenURL)
		provider.APIBaseURL = strings.TrimRight(strings.TrimSpace(provider.APIBaseURL), "/")
		if provider.BackchannelLogout && provider.BackchannelMaxAgeSeconds == 0 {
			provider.BackchannelMaxAgeSeconds = 300
		}
		if _, exists := seen[provider.ID]; exists {
			return nil, fmt.Errorf("AUTH_PROVIDERS_PATH contains duplicate provider id %q", provider.ID)
		}
		seen[provider.ID] = struct{}{}
		if provider.ClientSecretEnv != "" {
			provider.ClientSecret = os.Getenv(provider.ClientSecretEnv)
		}
		if provider.Type == "github" {
			if provider.AuthorizationURL == "" {
				provider.AuthorizationURL = "https://github.com/login/oauth/authorize"
			}
			if provider.TokenURL == "" {
				provider.TokenURL = "https://github.com/login/oauth/access_token"
			}
			if provider.APIBaseURL == "" {
				provider.APIBaseURL = "https://api.github.com"
			}
		}
	}
	return document.Providers, nil
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
		DistCCNetworkZone:  "default",
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
	config.DistCCAlphaEnabled = getEnvBool(env, "DISTCC_ALPHA_ENABLED", false)
	config.DistCCPackageAllowlist = getEnvStringSlice(env, "DISTCC_PACKAGE_ALLOWLIST", nil)
	config.DistCCCHOST = getEnvString(env, "DISTCC_CHOST", "")
	config.DistCCCompilerDigest = getEnvString(env, "DISTCC_COMPILER_DIGEST", "")
	config.DistCCToolchainImageGeneration = getEnvString(env, "DISTCC_TOOLCHAIN_IMAGE_GENERATION", "")
	config.DistCCCPUFeatures = getEnvStringSlice(env, "DISTCC_CPU_FEATURES", nil)
	config.DistCCNetworkZone = getEnvString(env, "DISTCC_NETWORK_ZONE", config.DistCCNetworkZone)
	config.DistCCIsolatedNetworkCIDRs = getEnvStringSlice(env, "DISTCC_ISOLATED_NETWORK_CIDRS", nil)

	config.StorageType = getEnvString(env, "STORAGE_TYPE", config.StorageType)
	config.StorageLocalDir = getEnvString(env, "STORAGE_LOCAL_DIR", config.StorageLocalDir)
	config.StorageS3Bucket = getEnvString(env, "STORAGE_S3_BUCKET", "")
	config.StorageS3Region = getEnvString(env, "STORAGE_S3_REGION", "")
	config.StorageS3Prefix = getEnvString(env, "STORAGE_S3_PREFIX", "")
	config.StorageS3Endpoint = getEnvString(env, "STORAGE_S3_ENDPOINT", "")
	config.StorageS3UsePathStyle = getEnvBool(env, "STORAGE_S3_USE_PATH_STYLE", false)
	config.StorageS3PublicBaseURL = getEnvString(env, "STORAGE_S3_PUBLIC_BASE_URL", "")
	config.StorageHTTPBase = getEnvString(env, "STORAGE_HTTP_BASE", "")

	config.ServerURL = getEnvString(env, "SERVER_URL", "")
	config.ServerAPIKey = getEnvString(env, "SERVER_API_KEY", "")
	config.AdvertiseURL = getEnvString(env, "BUILDER_ADVERTISE_URL", "")
	config.PullEnabled = getEnvBool(env, "WORKER_PULL_ENABLED", false)
	config.WorkerGatewayURL = getEnvString(env, "WORKER_GATEWAY_URL", "")
	config.WorkerTLSCert = getEnvString(env, "WORKER_TLS_CERT", "")
	config.WorkerTLSKey = getEnvString(env, "WORKER_TLS_KEY", "")
	config.WorkerTLSCA = getEnvString(env, "WORKER_TLS_CA", "")
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

func parseProviderSlotLimits(raw string) (map[string]int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	limits := make(map[string]int)
	for _, entry := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(entry), ":", 2)
		if len(parts) != 2 {
			return nil, false
		}
		provider := strings.ToLower(strings.TrimSpace(parts[0]))
		limit, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || !autoscaleProviderPattern.MatchString(provider) ||
			limit <= 0 || limit > 10000 {
			return nil, false
		}
		if _, duplicate := limits[provider]; duplicate {
			return nil, false
		}
		limits[provider] = limit
	}
	return limits, len(limits) > 0
}
