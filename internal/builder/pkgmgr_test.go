// Package builder provides package building functionality.
package builder

import (
	"strings"
	"testing"

	"github.com/slchris/portage-engine/pkg/config"
)

func TestNewPackageManager(t *testing.T) {
	cfg := &config.BuilderConfig{
		PortageReposPath: "/var/db/repos",
		PortageConfPath:  "/etc/portage",
	}
	pm := NewPackageManager(cfg)
	if pm.Name() != "portage" {
		t.Errorf("NewPackageManager() name = %v, want portage", pm.Name())
	}
}

func TestGentooPackageManager_Commands(t *testing.T) {
	cfg := &config.BuilderConfig{
		PortageReposPath: "/var/db/repos",
		PortageConfPath:  "/etc/portage",
	}
	pm := NewGentooPackageManager(cfg)

	t.Run("install command", func(t *testing.T) {
		cmd := pm.InstallCommand("app-misc/hello", nil)
		if len(cmd) < 3 {
			t.Error("install command should have at least 3 elements")
		}
		if cmd[0] != "emerge" {
			t.Errorf("first element should be 'emerge', got %v", cmd[0])
		}
		if cmd[len(cmd)-1] != "app-misc/hello" {
			t.Errorf("last element should be package name, got %v", cmd[len(cmd)-1])
		}
	})

	t.Run("build command", func(t *testing.T) {
		cmd := pm.BuildCommand("app-misc/hello", nil)
		if cmd[0] != "emerge" {
			t.Errorf("first element should be 'emerge', got %v", cmd[0])
		}
		hasBuildPkg := false
		for _, arg := range cmd {
			if arg == "--buildpkg" {
				hasBuildPkg = true
				break
			}
		}
		if !hasBuildPkg {
			t.Error("build command should contain --buildpkg flag")
		}
	})

	t.Run("search command", func(t *testing.T) {
		cmd := pm.SearchCommand("hello")
		if cmd[0] != "emerge" {
			t.Errorf("first element should be 'emerge', got %v", cmd[0])
		}
		if cmd[1] != "--search" {
			t.Errorf("second element should be '--search', got %v", cmd[1])
		}
	})

	t.Run("update command", func(t *testing.T) {
		cmd := pm.UpdateCommand()
		if cmd[0] != "emerge" {
			t.Errorf("first element should be 'emerge', got %v", cmd[0])
		}
	})

	t.Run("artifact extension", func(t *testing.T) {
		ext := pm.ArtifactExtension()
		if ext != ".gpkg.tar" {
			t.Errorf("artifact extension should be '.gpkg.tar', got %v", ext)
		}
	})
}

func TestGentooPackageManager_EnvVars(t *testing.T) {
	t.Run("with mirrors configured", func(t *testing.T) {
		cfg := &config.BuilderConfig{
			DistfilesMirror: "https://mirrors.example.com/gentoo",
			SyncMirror:      "rsync://rsync.example.com/gentoo-portage",
		}
		pm := NewGentooPackageManager(cfg)

		envVars := pm.GetEnvVars(cfg)

		if envVars["GENTOO_MIRRORS"] != "https://mirrors.example.com/gentoo" {
			t.Errorf("GENTOO_MIRRORS not set correctly, got %v", envVars["GENTOO_MIRRORS"])
		}
		if envVars["PORTAGE_SYNC_URI"] != "rsync://rsync.example.com/gentoo-portage" {
			t.Errorf("PORTAGE_SYNC_URI not set correctly, got %v", envVars["PORTAGE_SYNC_URI"])
		}
	})

	t.Run("without mirrors configured", func(t *testing.T) {
		cfg := &config.BuilderConfig{}
		pm := NewGentooPackageManager(cfg)

		envVars := pm.GetEnvVars(cfg)

		if _, ok := envVars["GENTOO_MIRRORS"]; ok {
			t.Error("GENTOO_MIRRORS should not be set when not configured")
		}
	})
}

func TestGentooPackageManager_BuildCommandWithOptions(t *testing.T) {
	cfg := &config.BuilderConfig{}
	pm := NewGentooPackageManager(cfg)

	options := []string{"--oneshot", "--update"}
	cmd := pm.BuildCommand("app-misc/hello", options)

	cmdStr := strings.Join(cmd, " ")
	if !strings.Contains(cmdStr, "--oneshot") {
		t.Error("command should contain --oneshot option")
	}
	if !strings.Contains(cmdStr, "--update") {
		t.Error("command should contain --update option")
	}
	if !strings.Contains(cmdStr, "--buildpkg") {
		t.Error("command should contain --buildpkg flag")
	}
}

func TestGentooPackageManager_GetArtifactPaths(t *testing.T) {
	cfg := &config.BuilderConfig{}
	pm := NewGentooPackageManager(cfg)

	paths := pm.GetArtifactPaths()
	if len(paths) == 0 {
		t.Error("expected at least one artifact path")
	}
	if paths[0] != "/var/cache/binpkgs" {
		t.Errorf("expected /var/cache/binpkgs, got %v", paths[0])
	}
}
