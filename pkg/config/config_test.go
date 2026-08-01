package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadServerConfigImageFactoryStatusPath(t *testing.T) {
	t.Setenv("IMAGE_FACTORY_STATUS_PATH", "/run/portage-engine/image-factory-status.json")
	cfg, err := LoadServerConfig(filepath.Join(t.TempDir(), "missing.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImageFactoryStatusPath != "/run/portage-engine/image-factory-status.json" {
		t.Fatalf("ImageFactoryStatusPath = %q", cfg.ImageFactoryStatusPath)
	}
}

func TestLoadServerDatabaseConfig(t *testing.T) {
	t.Setenv("DATABASE_REQUIRED", "true")
	t.Setenv("PGHOST", "postgres.internal")
	t.Setenv("PGPORT", "5544")
	t.Setenv("PGDATABASE", "portage_test")
	t.Setenv("PGUSER", "portage_app")
	t.Setenv("PGPASSWORD", "p@ss:/?#[]")
	t.Setenv("PGSSLMODE", "require")
	t.Setenv("DATABASE_MAX_CONNS", "24")
	t.Setenv("DATABASE_MIN_CONNS", "3")

	cfg, err := LoadServerConfig(filepath.Join(t.TempDir(), "missing.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Database.Enabled || !cfg.Database.Required {
		t.Fatalf("required database must imply enabled: %+v", cfg.Database)
	}
	if cfg.Database.Host != "postgres.internal" || cfg.Database.Port != 5544 {
		t.Fatalf("unexpected database address: %+v", cfg.Database)
	}
	if cfg.Database.Password != "p@ss:/?#[]" || cfg.Database.SSLMode != "require" {
		t.Fatal("database password or SSL mode was not preserved")
	}
	if cfg.Database.MaxConns != 24 || cfg.Database.MinConns != 3 {
		t.Fatalf("unexpected pool limits: %+v", cfg.Database)
	}
}

func TestLoadServerRedisConfig(t *testing.T) {
	t.Setenv("REDIS_REQUIRED", "true")
	t.Setenv("REDIS_HOST", "redis.internal")
	t.Setenv("REDIS_PORT", "6380")
	t.Setenv("REDIS_PASSWORD", "redis-secret")
	t.Setenv("REDIS_TLS_ENABLED", "true")
	t.Setenv("REDIS_KEY_PREFIX", "pe-test")
	t.Setenv("RATE_LIMIT_PER_MINUTE", "90")
	t.Setenv("RATE_LIMIT_BURST", "12")
	cfg, err := LoadServerConfig(filepath.Join(t.TempDir(), "missing.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Cache.Enabled || !cfg.Cache.Required || cfg.Cache.Host != "redis.internal" ||
		cfg.Cache.Port != 6380 || cfg.Cache.Password != "redis-secret" ||
		!cfg.Cache.TLSEnabled || cfg.Cache.KeyPrefix != "pe-test" ||
		cfg.Cache.RateLimitPerMinute != 90 || cfg.Cache.RateLimitBurst != 12 {
		t.Fatalf("unexpected Redis config: %+v", cfg.Cache)
	}
}

func validPublicServerConfig() *ServerConfig {
	return &ServerConfig{
		DeploymentMode:                     DeploymentModePublic,
		RuntimeRole:                        "api",
		ControlPlaneID:                     "community-api-1",
		StorageType:                        "s3",
		StorageS3Bucket:                    "portage-engine-artifacts",
		StorageS3Region:                    "us-east-1",
		CatalogPath:                        "/etc/portage-engine/catalog.json",
		AuthMode:                           "oidc",
		DeviceAuthorizationVerificationURI: "https://build.example.test/device",
		CORSAllowedOrigins: []string{
			"https://build.example.test",
		},
		IdentityProviders: []IdentityProviderConfig{{
			ID:          "google",
			Type:        "oidc",
			DisplayName: "Google",
			IssuerURL:   "https://accounts.google.com",
			Audience:    "portage-engine",
			ClientID:    "portage-engine",
			RedirectURL: "https://build.example.test/auth/provider/google/callback",
		}},
		IdentityAdminSubjects: []string{"google:admin-subject"},
		Database: DatabaseConfig{
			Enabled: true, Required: true,
		},
		Cache: CacheConfig{
			Enabled: true, Required: true, Password: "redis-secret",
		},
		GPGEnabled:                         true,
		GPGAutoCreate:                      false,
		GPGKeyID:                           "0123456789ABCDEF",
		GPGPublicKeyPath:                   "/run/portage-engine/release.asc",
		WorkerGatewayEnabled:               true,
		WorkerGatewayPort:                  9443,
		WorkerGatewayAdvertiseURL:          "https://workers.example.test:9443",
		WorkerGatewayTLSCert:               "/run/pki/server.crt",
		WorkerGatewayTLSKey:                "/run/pki/server.key",
		WorkerGatewayServerCA:              "/run/pki/server-ca.crt",
		WorkerGatewayClientCA:              "/run/pki/client-ca.crt",
		WorkerGatewayIssuerID:              "community-vault",
		WorkerGatewayIssuerProvider:        "vault",
		WorkerGatewayVaultAddress:          "https://vault.internal:8200",
		WorkerGatewayVaultMount:            "pki",
		WorkerGatewayVaultRole:             "portage-worker",
		WorkerGatewayVaultTokenPath:        "/run/secrets/vault-token",
		WorkerGatewayVaultTimeout:          15,
		WorkerCertificateTTLMin:            180,
		PhaseExecutorMode:                  "active",
		MetricsEnabled:                     true,
		MetricsPassword:                    "metrics-secret",
		SchedulerAutoscaleMode:             "observe",
		SchedulerAutoscaleMinSlots:         1,
		SchedulerAutoscaleMaxSlots:         64,
		SchedulerAutoscaleTargetReady:      2,
		SchedulerAutoscaleCooldownSeconds:  60,
		SchedulerAutoscaleScaleDownSeconds: 600,
		SchedulerAutoscaleIntervalSeconds:  15,
		SchedulerAutoscaleProviderMaxSlots: map[string]int{},
		SchedulerAutoscaleProviderLimitsOK: true,
	}
}

func configurePersistentExecutor(cfg *ServerConfig) {
	cfg.RuntimeRole = "executor"
	cfg.ControlPlaneID = "capacity-executor-test"
	cfg.ExecutorCapacityInstanceID = "123e4567-e89b-12d3-a456-426614174000"
	cfg.ExecutorCapabilities = []string{
		"capacity-pool:pve-zone-a-amd64-db23fcaaaeb71219f0511397",
		"provider:pve",
		"zone:zone-a",
		"arch:amd64",
		"build-mode:native-gentoo",
		"profile:pe/amd64/base-v1",
		"image:pe/amd64/base@g17",
		"phase:provision",
		"phase:build",
		"phase:verify",
		"phase:publish",
	}
	// An executor consumes the gateway trust/issuer configuration but never
	// owns the API-side TLS listener private key.
	cfg.WorkerGatewayTLSCert = ""
	cfg.WorkerGatewayTLSKey = ""
}

func TestPublicServerDeploymentRejectsUnsafeCompatibilityDefaults(t *testing.T) {
	cfg := validPublicServerConfig()
	cfg.StorageType = "local"
	cfg.AuthMode = "hybrid"
	cfg.APIKey = "legacy-admin"
	cfg.CORSAllowedOrigins = nil
	cfg.WorkerGatewayIssuerProvider = "file"
	cfg.GPGAutoCreate = true
	cfg.RemoteBuilders = []string{"http://legacy-builder:9090"}
	cfg.MetricsPassword = ""

	err := cfg.ValidateStartup()
	if err == nil {
		t.Fatal("unsafe public deployment was accepted")
	}
	message := err.Error()
	for _, want := range []string{
		"STORAGE_TYPE must be s3",
		"AUTH_MODE must be oidc",
		"CORS_ALLOWED_ORIGINS",
		"WORKER_GATEWAY_ISSUER_PROVIDER must be vault",
		"GPG_AUTO_CREATE must be false",
		"REMOTE_BUILDERS",
		"METRICS_PASSWORD",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("public validation error %q does not contain %q", message, want)
		}
	}
}

func TestPublicServerDeploymentAcceptsHardenedBoundary(t *testing.T) {
	if err := validPublicServerConfig().ValidateStartup(); err != nil {
		t.Fatalf("valid public deployment was rejected: %v", err)
	}
}

func TestPublicExecutorRequiresQuarantineScopedDeleteClient(t *testing.T) {
	cfg := validPublicServerConfig()
	configurePersistentExecutor(cfg)
	if err := cfg.ValidateStartup(); err == nil ||
		!strings.Contains(err.Error(), "STORAGE_S3_ALLOW_DELETE") {
		t.Fatalf("public executor without revocation client error=%v", err)
	}
	cfg.StorageS3AllowDelete = true
	if err := cfg.ValidateStartup(); err != nil {
		t.Fatalf("public executor with lifecycle client was rejected: %v", err)
	}
}

func TestPersistentExecutorAcceptsOutboundGatewayWithoutListenerKey(t *testing.T) {
	cfg := validPublicServerConfig()
	cfg.DeploymentMode = DeploymentModeTrusted
	cfg.StorageType = "local"
	configurePersistentExecutor(cfg)
	if err := cfg.ValidateStartup(); err != nil {
		t.Fatalf("listener-free persistent executor was rejected: %v", err)
	}
	for _, warning := range cfg.Validate() {
		if strings.Contains(warning, "server TLS cert/key") ||
			strings.Contains(warning, "WORKER_GATEWAY_PORT") {
			t.Fatalf("executor was incorrectly required to own a listener: %s", warning)
		}
	}
}

func TestPersistentExecutorRejectsAmbiguousOrSpoofedCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		capability string
		want       string
	}{
		{
			name:       "second provider",
			capability: "provider:aws",
			want:       "exactly one provider",
		},
		{
			name:       "static capacity identity",
			capability: "capacity-instance:123e4567-e89b-12d3-a456-426614174000",
			want:       "derived only from SMBIOS",
		},
		{
			name:       "malformed extra label",
			capability: "broken label",
			want:       "contains invalid label",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validPublicServerConfig()
			cfg.DeploymentMode = DeploymentModeTrusted
			configurePersistentExecutor(cfg)
			cfg.ExecutorCapabilities = append(cfg.ExecutorCapabilities, test.capability)
			err := cfg.ValidateStartup()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ambiguous executor capability error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestPersistentExecutorRejectsListenerPrivateKey(t *testing.T) {
	cfg := validPublicServerConfig()
	cfg.DeploymentMode = DeploymentModeTrusted
	configurePersistentExecutor(cfg)
	cfg.WorkerGatewayTLSKey = "/run/pki/api-listener.key"
	err := cfg.ValidateStartup()
	if err == nil || !strings.Contains(err.Error(), "must not receive WORKER_GATEWAY_TLS_KEY") {
		t.Fatalf("executor listener key error=%v", err)
	}
}

func TestPersistentExecutorRejectsPoolHashDrift(t *testing.T) {
	cfg := validPublicServerConfig()
	cfg.DeploymentMode = DeploymentModeTrusted
	configurePersistentExecutor(cfg)
	cfg.ExecutorCapabilities[0] = "capacity-pool:pve-zone-a-amd64-000000000000000000000000"
	err := cfg.ValidateStartup()
	if err == nil || !strings.Contains(err.Error(), "does not match its immutable dimensions") {
		t.Fatalf("executor pool hash drift error=%v", err)
	}
}

func TestPublicAPIRejectsObjectDeleteCapability(t *testing.T) {
	cfg := validPublicServerConfig()
	cfg.StorageS3AllowDelete = true
	if err := cfg.ValidateStartup(); err == nil ||
		!strings.Contains(err.Error(), "public read path") {
		t.Fatalf("public api with object deletion capability error=%v", err)
	}
}

func TestTrustedServerDeploymentPreservesLANCompatibility(t *testing.T) {
	cfg := &ServerConfig{
		DeploymentMode:              DeploymentModeTrusted,
		StorageType:                 "local",
		AuthMode:                    "legacy",
		OIDCAllowInsecureHTTP:       true,
		CloudPVEInsecure:            true,
		CloudSSHInsecureHostKey:     true,
		WorkerGatewayIssuerProvider: "file",
	}
	if err := cfg.ValidateStartup(); err != nil {
		t.Fatalf("trusted deployment compatibility was rejected: %v", err)
	}
}

func TestLoadServerIAMSessionAndStepUpPolicy(t *testing.T) {
	t.Setenv("STEP_UP_API_KEY", "independent-key")
	t.Setenv("DEVICE_AUTHORIZATION_VERIFICATION_URI", "https://build.example.test/device")
	t.Setenv("OIDC_SESSION_IDLE_MINUTES", "45")
	t.Setenv("OIDC_SESSION_MAX_MINUTES", "480")
	t.Setenv("OIDC_STEP_UP_MAX_AGE_MINUTES", "7")
	t.Setenv("OIDC_STEP_UP_AMR_VALUES", "otp,hwk")
	t.Setenv("OIDC_STEP_UP_ACR_VALUES", "urn:example:mfa")
	cfg, err := LoadServerConfig(filepath.Join(t.TempDir(), "missing.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StepUpAPIKey != "independent-key" ||
		cfg.DeviceAuthorizationVerificationURI != "https://build.example.test/device" ||
		cfg.OIDCSessionIdleMinutes != 45 || cfg.OIDCSessionMaxMinutes != 480 ||
		cfg.OIDCStepUpMaxAgeMin != 7 ||
		len(cfg.OIDCStepUpAMRValues) != 2 ||
		len(cfg.OIDCStepUpACRValues) != 1 {
		t.Fatalf("unexpected IAM lifecycle config: %+v", cfg)
	}
}

func TestDeviceAuthorizationVerificationURIValidation(t *testing.T) {
	cfg := validPublicServerConfig()
	cfg.DeviceAuthorizationVerificationURI = "http://build.example.test/device"
	if err := cfg.ValidateStartup(); err == nil ||
		!strings.Contains(err.Error(), "DEVICE_AUTHORIZATION_VERIFICATION_URI") {
		t.Fatalf("public HTTP verification URI error=%v", err)
	}

	cfg = validPublicServerConfig()
	cfg.DeviceAuthorizationVerificationURI = "https://build.example.test/device?token=unsafe"
	if err := cfg.ValidateStartup(); err == nil ||
		!strings.Contains(err.Error(), "DEVICE_AUTHORIZATION_VERIFICATION_URI") {
		t.Fatalf("verification URI with query error=%v", err)
	}
}

func TestLoadServerSchedulerAutoscalePolicy(t *testing.T) {
	t.Setenv("SCHEDULER_AUTOSCALE_MODE", "actuate")
	t.Setenv("SCHEDULER_AUTOSCALE_MIN_SLOTS", "2")
	t.Setenv("SCHEDULER_AUTOSCALE_MAX_SLOTS", "48")
	t.Setenv("SCHEDULER_AUTOSCALE_TARGET_READY_PER_SLOT", "3")
	t.Setenv("SCHEDULER_AUTOSCALE_COOLDOWN_SECONDS", "90")
	t.Setenv("SCHEDULER_AUTOSCALE_SCALE_DOWN_SECONDS", "900")
	t.Setenv("SCHEDULER_AUTOSCALE_INTERVAL_SECONDS", "20")
	t.Setenv("SCHEDULER_AUTOSCALE_PROVIDER_MAX_SLOTS", "pve:32,gcp:16")
	cfg, err := LoadServerConfig(filepath.Join(t.TempDir(), "missing.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SchedulerAutoscaleMode != "actuate" ||
		cfg.SchedulerAutoscaleMinSlots != 2 ||
		cfg.SchedulerAutoscaleMaxSlots != 48 ||
		cfg.SchedulerAutoscaleTargetReady != 3 ||
		cfg.SchedulerAutoscaleCooldownSeconds != 90 ||
		cfg.SchedulerAutoscaleScaleDownSeconds != 900 ||
		cfg.SchedulerAutoscaleIntervalSeconds != 20 ||
		!cfg.SchedulerAutoscaleProviderLimitsOK ||
		cfg.SchedulerAutoscaleProviderMaxSlots["pve"] != 32 ||
		cfg.SchedulerAutoscaleProviderMaxSlots["gcp"] != 16 {
		t.Fatalf("unexpected scheduler autoscale config: %+v", cfg)
	}
}

func TestLoadAndValidatePersistentExecutorRole(t *testing.T) {
	instanceID := "123e4567-e89b-12d3-a456-426614174000"
	t.Setenv("SERVER_RUNTIME_ROLE", "executor")
	t.Setenv("CONTROL_PLANE_ID", "executor-"+instanceID)
	t.Setenv("EXECUTOR_CAPACITY_INSTANCE_ID", instanceID)
	t.Setenv("PHASE_EXECUTOR_MODE", "active")
	t.Setenv("DATABASE_ENABLED", "true")
	t.Setenv("DATABASE_REQUIRED", "true")
	t.Setenv("WORKER_GATEWAY_ENABLED", "true")
	t.Setenv(
		"EXECUTOR_CAPABILITIES",
		"capacity-pool:pve-default-amd64-test,provider:pve,zone:default,arch:amd64,build-mode:native-gentoo,profile:test/profile,image:test/image@g1,phase:provision,phase:build,phase:verify,phase:publish",
	)
	cfg, err := LoadServerConfig(filepath.Join(t.TempDir(), "missing.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeRole != "executor" ||
		cfg.ExecutorCapacityInstanceID != instanceID {
		t.Fatalf("executor identity was not loaded: %+v", cfg)
	}
	for _, warning := range cfg.Validate() {
		if strings.Contains(warning, "executor role requires") ||
			strings.Contains(warning, "EXECUTOR_CAPACITY_INSTANCE_ID") {
			t.Fatalf("valid executor role was rejected: %s", warning)
		}
	}
}

func TestValidatePersistentExecutorRejectsUnboundIdentity(t *testing.T) {
	cfg := &ServerConfig{
		RuntimeRole:                "executor",
		ControlPlaneID:             "executor-a",
		ExecutorCapacityInstanceID: "NOT-A-UUID",
		PhaseExecutorMode:          "active",
		Database: DatabaseConfig{
			Enabled: true, Required: true,
		},
		WorkerGatewayEnabled: true,
	}
	warnings := strings.Join(cfg.Validate(), "\n")
	if !strings.Contains(warnings, "lowercase UUID") {
		t.Fatalf("invalid capacity identity was accepted: %s", warnings)
	}
}

// TestLoadServerConfig tests loading server configuration.
func TestLoadServerConfig(t *testing.T) {
	tmpFile := "/tmp/test-server.conf"
	configData := `# Test server config
SERVER_PORT=9999
BINPKG_PATH=/test/binpkgs
MAX_WORKERS=10
BUILD_MODE=cloud
STORAGE_TYPE=s3
STORAGE_S3_BUCKET=test-bucket
STORAGE_S3_REGION=us-east-1
STORAGE_S3_ENDPOINT=http://minio.internal:9000
STORAGE_S3_USE_PATH_STYLE=true
STORAGE_S3_PUBLIC_BASE_URL=https://binpkgs.example.test
STORAGE_S3_ALLOW_DELETE=true
GPG_ENABLED=true
GPG_KEY_ID=ABCD1234
CLOUD_DEFAULT_PROVIDER=aws
REMOTE_BUILDERS=http://builder1:9090,http://builder2:9090
CATALOG_PATH=/etc/portage-engine/catalog.json
`

	if err := os.WriteFile(tmpFile, []byte(configData), 0600); err != nil {
		t.Fatalf("Failed to create test config: %v", err)
	}
	defer func() { _ = os.Remove(tmpFile) }()

	cfg, err := LoadServerConfig(tmpFile)
	if err != nil {
		t.Fatalf("LoadServerConfig failed: %v", err)
	}

	if cfg.Port != 9999 {
		t.Errorf("Expected Port=9999, got %d", cfg.Port)
	}

	if cfg.BinpkgPath != "/test/binpkgs" {
		t.Errorf("Expected BinpkgPath=/test/binpkgs, got %s", cfg.BinpkgPath)
	}

	if cfg.MaxWorkers != 10 {
		t.Errorf("Expected MaxWorkers=10, got %d", cfg.MaxWorkers)
	}

	if cfg.BuildMode != "cloud" {
		t.Errorf("Expected BuildMode=cloud, got %s", cfg.BuildMode)
	}

	if cfg.StorageType != "s3" {
		t.Errorf("Expected StorageType=s3, got %s", cfg.StorageType)
	}

	if cfg.StorageS3Bucket != "test-bucket" {
		t.Errorf("Expected StorageS3Bucket=test-bucket, got %s", cfg.StorageS3Bucket)
	}
	if cfg.StorageS3Region != "us-east-1" ||
		cfg.StorageS3Endpoint != "http://minio.internal:9000" ||
		!cfg.StorageS3UsePathStyle ||
		!cfg.StorageS3AllowDelete ||
		cfg.StorageS3PublicBaseURL != "https://binpkgs.example.test" {
		t.Errorf("Unexpected S3-compatible configuration: %+v", cfg)
	}

	if !cfg.GPGEnabled {
		t.Error("Expected GPGEnabled=true, got false")
	}

	if cfg.GPGKeyID != "ABCD1234" {
		t.Errorf("Expected GPGKeyID=ABCD1234, got %s", cfg.GPGKeyID)
	}

	if cfg.CloudProvider != "aws" {
		t.Errorf("Expected CloudProvider=aws, got %s", cfg.CloudProvider)
	}

	if len(cfg.RemoteBuilders) != 2 {
		t.Errorf("Expected 2 remote builders, got %d", len(cfg.RemoteBuilders))
	}

	if cfg.RemoteBuilders[0] != "http://builder1:9090" {
		t.Errorf("Expected first builder=http://builder1:9090, got %s", cfg.RemoteBuilders[0])
	}
	if cfg.CatalogPath != "/etc/portage-engine/catalog.json" {
		t.Errorf("Expected CatalogPath from config, got %q", cfg.CatalogPath)
	}
}

// TestLoadDashboardConfig tests loading dashboard configuration.
func TestLoadDashboardConfig(t *testing.T) {
	tmpFile := "/tmp/test-dashboard.conf"
	configData := `# Test dashboard config
DASHBOARD_PORT=7777
SERVER_URL=http://test-server:8080
AUTH_ENABLED=false
JWT_SECRET=test-secret
ALLOW_ANONYMOUS=false
`

	if err := os.WriteFile(tmpFile, []byte(configData), 0600); err != nil {
		t.Fatalf("Failed to create test config: %v", err)
	}
	defer func() { _ = os.Remove(tmpFile) }()

	cfg, err := LoadDashboardConfig(tmpFile)
	if err != nil {
		t.Fatalf("LoadDashboardConfig failed: %v", err)
	}

	if cfg.Port != 7777 {
		t.Errorf("Expected Port=7777, got %d", cfg.Port)
	}

	if cfg.ServerURL != "http://test-server:8080" {
		t.Errorf("Expected ServerURL=http://test-server:8080, got %s", cfg.ServerURL)
	}

	if cfg.AuthEnabled {
		t.Error("Expected AuthEnabled=false, got true")
	}

	if cfg.JWTSecret != "test-secret" {
		t.Errorf("Expected JWTSecret=test-secret, got %s", cfg.JWTSecret)
	}

	if cfg.AllowAnonymous {
		t.Error("Expected AllowAnonymous=false, got true")
	}
}

func TestDashboardConfigValidatesBackendOrigin(t *testing.T) {
	for _, valid := range []string{"http://server.internal:8080", "https://server.internal/"} {
		cfg := &DashboardConfig{ServerURL: valid}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want success", valid, err)
		}
	}
	for _, invalid := range []string{
		"",
		"file:///etc/passwd",
		"http://user:password@server.internal",
		"http://server.internal/api",
		"http://server.internal?target=other",
	} {
		cfg := &DashboardConfig{ServerURL: invalid}
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate(%q) succeeded, want origin validation error", invalid)
		}
	}
}

func TestServerConfigActivePhaseExecutorFailsClosedWithoutDurablePullMode(t *testing.T) {
	cfg := &ServerConfig{PhaseExecutorMode: "active"}
	warnings := strings.Join(cfg.Validate(), "\n")
	for _, expected := range []string{
		"active phase executor requires DATABASE_ENABLED=true",
		"active phase executor requires WORKER_GATEWAY_ENABLED=true",
	} {
		if !strings.Contains(warnings, expected) {
			t.Fatalf("warnings %q do not contain %q", warnings, expected)
		}
	}
	cfg.Database.Enabled, cfg.Database.Required = true, true
	cfg.WorkerGatewayEnabled = true
	cfg.WorkerGatewayPort, cfg.Port = 9443, 8080
	cfg.WorkerGatewayAdvertiseURL = "https://gateway.internal:9443"
	cfg.WorkerGatewayTLSCert = "server.crt"
	cfg.WorkerGatewayTLSKey = "server.key"
	cfg.WorkerGatewayServerCA = "ca.crt"
	cfg.WorkerGatewayClientCA = "ca.crt"
	cfg.WorkerGatewayIssuerCert = "issuer.crt"
	cfg.WorkerGatewayIssuerKey = "issuer.key"
	cfg.WorkerCertificateTTLMin = 180
	cfg.RemoteBuilders = []string{"legacy-builder:9090"}
	warnings = strings.Join(cfg.Validate(), "\n")
	if !strings.Contains(warnings,
		"active phase executor does not support legacy REMOTE_BUILDERS") {
		t.Fatalf("legacy dual-run warning missing: %q", warnings)
	}
}

func TestLoadServerExecutorCapabilityRouting(t *testing.T) {
	t.Setenv("EXECUTOR_ZONES", "lan-a,lan-b")
	t.Setenv(
		"EXECUTOR_CAPABILITIES",
		"phase:build,provider:pve,zone:lan-a,image:pe/base-g6@g6",
	)
	cfg, err := LoadServerConfig(filepath.Join(t.TempDir(), "missing.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.ExecutorZones, ",") != "lan-a,lan-b" ||
		strings.Join(cfg.ExecutorCapabilities, ",") !=
			"phase:build,provider:pve,zone:lan-a,image:pe/base-g6@g6" {
		t.Fatalf(
			"unexpected executor routing config: zones=%v capabilities=%v",
			cfg.ExecutorZones, cfg.ExecutorCapabilities,
		)
	}
	cfg.PhaseExecutorMode = "active"
	cfg.Database.Enabled, cfg.Database.Required = true, true
	cfg.WorkerGatewayEnabled = true
	cfg.WorkerGatewayPort, cfg.Port = 9443, 8080
	cfg.WorkerGatewayAdvertiseURL = "https://gateway.internal:9443"
	cfg.WorkerGatewayTLSCert, cfg.WorkerGatewayTLSKey = "server.crt", "server.key"
	cfg.WorkerGatewayServerCA, cfg.WorkerGatewayClientCA = "ca.crt", "ca.crt"
	cfg.WorkerGatewayIssuerCert, cfg.WorkerGatewayIssuerKey = "issuer.crt", "issuer.key"
	cfg.WorkerCertificateTTLMin = 180
	if warnings := strings.Join(cfg.Validate(), "\n"); strings.Contains(
		warnings, "EXECUTOR_",
	) {
		t.Fatalf("valid executor routing config produced routing warnings: %s", warnings)
	}

	cfg.ExecutorCapabilities = []string{"provider:pve"}
	if warnings := strings.Join(cfg.Validate(), "\n"); !strings.Contains(
		warnings, "requires at least one phase label",
	) {
		t.Fatalf("phase-less capability override was accepted: %q", warnings)
	}
}

func TestLoadServerVaultWorkerIssuer(t *testing.T) {
	t.Setenv("WORKER_GATEWAY_ENABLED", "true")
	t.Setenv("DATABASE_ENABLED", "true")
	t.Setenv("DATABASE_REQUIRED", "true")
	t.Setenv("WORKER_GATEWAY_ADVERTISE_URL", "https://gateway.internal:9443")
	t.Setenv("WORKER_GATEWAY_TLS_CERT", "server.crt")
	t.Setenv("WORKER_GATEWAY_TLS_KEY", "server.key")
	t.Setenv("WORKER_GATEWAY_SERVER_CA", "server-ca.crt")
	t.Setenv("WORKER_GATEWAY_CLIENT_CA", "worker-roots.pem")
	t.Setenv("WORKER_GATEWAY_ISSUER_PROVIDER", "vault")
	t.Setenv("WORKER_GATEWAY_ISSUER_ID", "community-vault")
	t.Setenv("WORKER_GATEWAY_VAULT_ADDRESS", "https://vault.internal:8200")
	t.Setenv("WORKER_GATEWAY_VAULT_MOUNT", "pki_workers")
	t.Setenv("WORKER_GATEWAY_VAULT_ROLE", "portage-worker")
	t.Setenv("WORKER_GATEWAY_VAULT_TOKEN_FILE", "/run/secrets/vault-token")
	t.Setenv("WORKER_GATEWAY_VAULT_NAMESPACE", "community")
	t.Setenv("WORKER_GATEWAY_VAULT_SERVER_CA", "/run/secrets/vault-ca.pem")
	t.Setenv("WORKER_GATEWAY_VAULT_TIMEOUT_SECONDS", "20")
	cfg, err := LoadServerConfig(filepath.Join(t.TempDir(), "missing.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerGatewayIssuerProvider != "vault" ||
		cfg.WorkerGatewayVaultMount != "pki_workers" ||
		cfg.WorkerGatewayVaultRole != "portage-worker" ||
		cfg.WorkerGatewayVaultTimeout != 20 {
		t.Fatalf("unexpected Vault worker issuer config: %+v", cfg)
	}
	warnings := strings.Join(cfg.Validate(), "\n")
	if strings.Contains(warnings, "Vault worker issuer requires") ||
		strings.Contains(warnings, "must be file or vault") {
		t.Fatalf("valid Vault worker issuer was rejected: %s", warnings)
	}
}

func TestDashboardConfigValidatesOIDCTransport(t *testing.T) {
	base := DashboardConfig{
		ServerURL:       "http://server.internal:8080",
		AuthEnabled:     true,
		JWTSecret:       "test-secret-that-is-at-least-32-chars-long",
		OIDCEnabled:     true,
		OIDCIssuerURL:   "http://idp.internal/realms/portage",
		OIDCClientID:    "portage-dashboard",
		OIDCRedirectURL: "http://dashboard.internal/auth/oidc/callback",
	}
	if err := base.Validate(); err == nil {
		t.Fatal("trusted-LAN HTTP OIDC succeeded without explicit opt-in")
	}
	base.OIDCAllowInsecureHTTP = true
	if err := base.Validate(); err != nil {
		t.Fatalf("explicit trusted-LAN HTTP OIDC opt-in failed: %v", err)
	}

	base.OIDCAllowInsecureHTTP = false
	base.OIDCIssuerURL = "https://idp.example.test/realms/portage"
	base.OIDCRedirectURL = "https://dashboard.example.test/auth/oidc/callback"
	if err := base.Validate(); err == nil {
		t.Fatal("HTTPS OIDC callback succeeded without Secure cookies")
	}
	base.CookieSecure = true
	if err := base.Validate(); err != nil {
		t.Fatalf("HTTPS OIDC config failed: %v", err)
	}

	base.AuthEnabled = false
	if err := base.Validate(); err == nil {
		t.Fatal("OIDC succeeded while dashboard authentication was disabled")
	}
}

func TestPublicDashboardDeploymentBoundary(t *testing.T) {
	cfg := DashboardConfig{
		DeploymentMode: DeploymentModePublic,
		ServerURL:      "http://portage-server:8080",
		AuthEnabled:    true,
		JWTSecret:      "test-secret-that-is-at-least-32-chars-long",
		AllowAnonymous: false,
		CookieSecure:   true,
		IdentityProviders: []IdentityProviderConfig{{
			ID:           "github",
			Type:         "github",
			DisplayName:  "GitHub",
			ClientID:     "github-client",
			ClientSecret: "github-secret",
			RedirectURL:  "https://build.example.test/auth/provider/github/callback",
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid public dashboard was rejected: %v", err)
	}

	cfg.AllowAnonymous = true
	cfg.AdminUser = "admin"
	cfg.CookieSecure = false
	err := cfg.Validate()
	if err == nil {
		t.Fatal("unsafe public dashboard was accepted")
	}
	for _, want := range []string{
		"ALLOW_ANONYMOUS must be false",
		"COOKIE_SECURE must be true",
		"local ADMIN_USER",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("public dashboard validation error %q does not contain %q", err, want)
		}
	}
}

func TestIdentityProviderConfigLoadsMultipleProviderTypes(t *testing.T) {
	dir := t.TempDir()
	providersPath := filepath.Join(dir, "providers.json")
	configPath := filepath.Join(dir, "server.conf")
	t.Setenv("TEST_GITHUB_OAUTH_SECRET", "github-test-secret")
	providers := `{
		"providers": [
			{
				"id": "google",
				"type": "oidc",
				"display_name": "Google",
				"issuer_url": "https://accounts.google.com",
				"audience": "google-client",
				"client_id": "google-client",
				"redirect_url": "https://dashboard.example.test/auth/provider/google/callback"
			},
			{
				"id": "github",
				"type": "github",
				"display_name": "GitHub",
				"client_id": "github-client",
				"client_secret_env": "TEST_GITHUB_OAUTH_SECRET",
				"redirect_url": "https://dashboard.example.test/auth/provider/github/callback"
			}
		]
	}`
	if err := os.WriteFile(providersPath, []byte(providers), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(
		"AUTH_MODE=hybrid\nDATABASE_ENABLED=true\nDATABASE_REQUIRED=true\n"+
			"AUTH_PROVIDERS_PATH=providers.json\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadServerConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.IdentityProviders) != 2 ||
		cfg.IdentityProviders[0].ID != "google" ||
		cfg.IdentityProviders[1].ClientSecret != "github-test-secret" ||
		cfg.IdentityProviders[1].APIBaseURL != "https://api.github.com" {
		t.Fatalf("identity providers = %+v", cfg.IdentityProviders)
	}
	if warnings := strings.Join(cfg.Validate(), "\n"); strings.Contains(
		warnings, "identity provider",
	) {
		t.Fatalf("valid provider config warnings = %s", warnings)
	}
}

// TestLoadBuilderConfig tests loading builder configuration.
func TestLoadBuilderConfig(t *testing.T) {
	tmpFile := "/tmp/test-builder.conf"
	configData := `# Test builder config
BUILDER_PORT=6666
BUILDER_WORKERS=8
NATIVE_JOB_POLICY=unsafe-reuse
BUILD_WORK_DIR=/custom/work
BUILD_ARTIFACT_DIR=/custom/artifacts
STORAGE_TYPE=http
STORAGE_HTTP_BASE=https://storage.test.com
NOTIFY_CONFIG=/path/to/notify.json
`

	if err := os.WriteFile(tmpFile, []byte(configData), 0600); err != nil {
		t.Fatalf("Failed to create test config: %v", err)
	}
	defer func() { _ = os.Remove(tmpFile) }()

	cfg, err := LoadBuilderConfig(tmpFile)
	if err != nil {
		t.Fatalf("LoadBuilderConfig failed: %v", err)
	}

	if cfg.Port != 6666 {
		t.Errorf("Expected Port=6666, got %d", cfg.Port)
	}

	if cfg.Workers != 8 {
		t.Errorf("Expected Workers=8, got %d", cfg.Workers)
	}

	if cfg.NativeJobPolicy != "unsafe-reuse" {
		t.Errorf("Expected NativeJobPolicy=unsafe-reuse, got %q", cfg.NativeJobPolicy)
	}

	if cfg.WorkDir != "/custom/work" {
		t.Errorf("Expected WorkDir=/custom/work, got %s", cfg.WorkDir)
	}

	if cfg.ArtifactDir != "/custom/artifacts" {
		t.Errorf("Expected ArtifactDir=/custom/artifacts, got %s", cfg.ArtifactDir)
	}

	if cfg.StorageType != "http" {
		t.Errorf("Expected StorageType=http, got %s", cfg.StorageType)
	}

	if cfg.StorageHTTPBase != "https://storage.test.com" {
		t.Errorf("Expected StorageHTTPBase=https://storage.test.com, got %s", cfg.StorageHTTPBase)
	}

	if cfg.NotifyConfig != "/path/to/notify.json" {
		t.Errorf("Expected NotifyConfig=/path/to/notify.json, got %s", cfg.NotifyConfig)
	}
}

// TestLoadConfigDefaults tests that default values are used when config file doesn't exist.
func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadServerConfig("/nonexistent/config.conf")
	if err != nil {
		t.Fatalf("LoadServerConfig failed: %v", err)
	}

	if cfg.Port != 8080 {
		t.Errorf("Expected default Port=8080, got %d", cfg.Port)
	}

	if cfg.MaxWorkers != 5 {
		t.Errorf("Expected default MaxWorkers=5, got %d", cfg.MaxWorkers)
	}
	if cfg.SchedulerAutoscaleMode != "observe" ||
		cfg.SchedulerAutoscaleMinSlots != 1 ||
		cfg.SchedulerAutoscaleMaxSlots != 64 {
		t.Fatalf("unexpected default autoscale policy: %+v", cfg)
	}
}

// TestLoadEnvFile tests the env file parsing.
func TestLoadEnvFile(t *testing.T) {
	tmpFile := "/tmp/test-env.conf"
	configData := `# Comment line
KEY1=value1

KEY2=value2

# Another comment
KEY3=value with spaces
EMPTY_KEY=
`

	if err := os.WriteFile(tmpFile, []byte(configData), 0600); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer func() { _ = os.Remove(tmpFile) }()

	env, err := loadEnvFile(tmpFile)
	if err != nil {
		t.Fatalf("loadEnvFile failed: %v", err)
	}

	if env["KEY1"] != "value1" {
		t.Errorf("Expected KEY1=value1, got %s", env["KEY1"])
	}

	if env["KEY2"] != "value2" {
		t.Errorf("Expected KEY2=value2, got %s", env["KEY2"])
	}

	if env["KEY3"] != "value with spaces" {
		t.Errorf("Expected KEY3='value with spaces', got %s", env["KEY3"])
	}

	if env["EMPTY_KEY"] != "" {
		t.Errorf("Expected EMPTY_KEY='', got %s", env["EMPTY_KEY"])
	}
}

// TestLoadBuilderConfigPortageSettings tests loading portage mirror configuration.
func TestLoadBuilderConfigPortageSettings(t *testing.T) {
	t.Run("portage mirror config", func(t *testing.T) {
		tmpFile := "/tmp/test-builder-portage.conf"
		configData := `SYNC_MIRROR=rsync://rsync.example.com/gentoo-portage
DISTFILES_MIRROR=https://mirrors.example.com/gentoo
PORTAGE_REPOS_PATH=/custom/repos
PORTAGE_CONF_PATH=/custom/portage
MAKE_CONF_PATH=/custom/make.conf
`

		if err := os.WriteFile(tmpFile, []byte(configData), 0600); err != nil {
			t.Fatalf("Failed to create test config: %v", err)
		}
		defer func() { _ = os.Remove(tmpFile) }()

		cfg, err := LoadBuilderConfig(tmpFile)
		if err != nil {
			t.Fatalf("LoadBuilderConfig failed: %v", err)
		}

		if cfg.SyncMirror != "rsync://rsync.example.com/gentoo-portage" {
			t.Errorf("Expected SyncMirror=rsync://rsync.example.com/gentoo-portage, got %s", cfg.SyncMirror)
		}

		if cfg.DistfilesMirror != "https://mirrors.example.com/gentoo" {
			t.Errorf("Expected DistfilesMirror=https://mirrors.example.com/gentoo, got %s", cfg.DistfilesMirror)
		}

		if cfg.PortageReposPath != "/custom/repos" {
			t.Errorf("Expected PortageReposPath=/custom/repos, got %s", cfg.PortageReposPath)
		}

		if cfg.PortageConfPath != "/custom/portage" {
			t.Errorf("Expected PortageConfPath=/custom/portage, got %s", cfg.PortageConfPath)
		}

		if cfg.MakeConfPath != "/custom/make.conf" {
			t.Errorf("Expected MakeConfPath=/custom/make.conf, got %s", cfg.MakeConfPath)
		}
	})

	t.Run("default paths", func(t *testing.T) {
		tmpFile := "/tmp/test-builder-defaults.conf"
		configData := `BUILDER_PORT=9090
`

		if err := os.WriteFile(tmpFile, []byte(configData), 0600); err != nil {
			t.Fatalf("Failed to create test config: %v", err)
		}
		defer func() { _ = os.Remove(tmpFile) }()

		cfg, err := LoadBuilderConfig(tmpFile)
		if err != nil {
			t.Fatalf("LoadBuilderConfig failed: %v", err)
		}

		// Default Gentoo paths
		if cfg.PortageReposPath != "/var/db/repos" {
			t.Errorf("Expected default PortageReposPath=/var/db/repos, got %s", cfg.PortageReposPath)
		}

		if cfg.PortageConfPath != "/etc/portage" {
			t.Errorf("Expected default PortageConfPath=/etc/portage, got %s", cfg.PortageConfPath)
		}

		if cfg.MakeConfPath != "/etc/portage/make.conf" {
			t.Errorf("Expected default MakeConfPath=/etc/portage/make.conf, got %s", cfg.MakeConfPath)
		}

		if cfg.NativeJobPolicy != "single-use" {
			t.Errorf("Expected default NativeJobPolicy=single-use, got %q", cfg.NativeJobPolicy)
		}
	})
}

func TestLoadBuilderConfigRejectsDockerBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "builder.conf")
	if err := os.WriteFile(path, []byte("USE_DOCKER=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBuilderConfig(path); err == nil || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("LoadBuilderConfig docker error = %v, want explicit removal error", err)
	}
}

// TestUnquoteEnvValue verifies surrounding quotes are stripped (#61).
func TestUnquoteEnvValue(t *testing.T) {
	cases := map[string]string{
		`"quoted"`:      "quoted",
		`'single'`:      "single",
		`plain`:         "plain",
		`"mismatch'`:    `"mismatch'`,
		`""`:            "",
		`"with spaces"`: "with spaces",
		`"a#b"`:         "a#b",
	}
	for in, want := range cases {
		if got := unquoteEnvValue(in); got != want {
			t.Errorf("unquoteEnvValue(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLoadServerConfigEnvWithoutFile verifies env vars are honored even when the
// config file is absent (#39).
func TestLoadServerConfigEnvWithoutFile(t *testing.T) {
	t.Setenv("SERVER_PORT", "9999")
	t.Setenv("API_KEY", "envkey")
	t.Setenv("CATALOG_PATH", "/srv/portage-engine/catalog.json")
	t.Setenv("CLOUD_PVE_USERNAME", "terraform-prov@pve")
	t.Setenv("CLOUD_PVE_PASSWORD", "runtime-only-password")
	t.Setenv("BUILD_MODE", "native-gentoo")

	cfg, err := LoadServerConfig("/nonexistent/path/server.conf")
	if err != nil {
		t.Fatalf("LoadServerConfig failed: %v", err)
	}
	if cfg.Port != 9999 {
		t.Errorf("env SERVER_PORT ignored when file missing: got Port=%d", cfg.Port)
	}
	if cfg.APIKey != "envkey" {
		t.Errorf("env API_KEY ignored when file missing: got %q", cfg.APIKey)
	}
	if cfg.CatalogPath != "/srv/portage-engine/catalog.json" {
		t.Errorf("env CATALOG_PATH ignored: got %q", cfg.CatalogPath)
	}
	if cfg.CloudPVEUsername != "terraform-prov@pve" || cfg.CloudPVEPassword != "runtime-only-password" {
		t.Fatal("headless PVE password authentication was not loaded from the process environment")
	}
	settings := CloudSettingsFromServerConfig(cfg)
	if settings.PVEUsername != cfg.CloudPVEUsername || settings.PVEPassword != cfg.CloudPVEPassword {
		t.Fatal("PVE password authentication was dropped from runtime cloud settings")
	}
	if settings.BuildMode != "native-gentoo" {
		t.Fatalf("build mode was dropped from runtime cloud settings: %q", settings.BuildMode)
	}
}

// TestLoadEnvFileStripsQuotes verifies quoted values in a conf file are unquoted.
func TestLoadEnvFileStripsQuotes(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.conf"
	content := "API_KEY=\"quoted-secret\"\nSERVER_PORT=8080\nDATA_DIR='single-quoted'\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	env, err := loadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if env["API_KEY"] != "quoted-secret" {
		t.Errorf("API_KEY = %q, want quoted-secret", env["API_KEY"])
	}
	if env["DATA_DIR"] != "single-quoted" {
		t.Errorf("DATA_DIR = %q, want single-quoted", env["DATA_DIR"])
	}
}
