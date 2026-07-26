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
