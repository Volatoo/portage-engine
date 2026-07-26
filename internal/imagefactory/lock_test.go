package imagefactory

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testObject(data []byte) InputObject {
	digest := sha256.Sum256(data)
	return InputObject{
		ID: "packer/proxmox", Kind: "packer-plugin", Path: "packer/plugin.bin",
		Platform: runtime.GOOS + "-" + runtime.GOARCH,
		SHA256:   hex.EncodeToString(digest[:]), Size: int64(len(data)), RequiredFor: []string{"base-systemd"},
	}
}

func testLock(object InputObject) *InputLock {
	return &InputLock{
		Version: 1, BundleID: "mirror/test", StrictOffline: true,
		AllowedHosts: []string{"mirror.internal"}, Objects: []InputObject{object},
	}
}

func TestPreflightVerifiesAndReportsAllObjects(t *testing.T) {
	root := t.TempDir()
	data := []byte("plugin")
	object := testObject(data)
	path := filepath.Join(root, object.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Preflight(root, testLock(object), "base-systemd")
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified != 1 || len(report.Missing) != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}

	missing := testObject([]byte("missing"))
	missing.ID = "seed/missing"
	missing.Kind = "seed"
	missing.Path = "seed/missing.qcow2"
	lock := testLock(object)
	lock.Objects = append(lock.Objects, missing)
	report, err = Preflight(root, lock, "base-systemd")
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified != 1 || len(report.Missing) != 1 || report.Missing[0].FallbackAllowed {
		t.Fatalf("missing set is wrong: %+v", report)
	}
}

func TestPreflightRejectsDigestMismatchAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	object := testObject([]byte("expected"))
	object.Path = "objects/value"
	path := filepath.Join(root, object.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("same-size"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Preflight(root, testLock(object), "base-systemd")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("digest mismatch accepted: %+v", report)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	report, err = Preflight(root, testLock(object), "base-systemd")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 || report.Missing[0].Reason != "object resolves outside the offline root" {
		t.Fatalf("symlink escape accepted: %+v", report)
	}
}

func TestInputLockRejectsUnsafeDefinition(t *testing.T) {
	object := testObject([]byte("x"))
	object.Path = "../escape"
	if err := testLock(object).Validate(); err == nil {
		t.Fatal("path traversal accepted")
	}
	object = testObject([]byte("x"))
	lock := testLock(object)
	lock.StrictOffline = false
	if err := lock.Validate(); err == nil {
		t.Fatal("public fallback accepted")
	}
}

func TestInputLockAcceptsContentAddressedServiceBinary(t *testing.T) {
	object := testObject([]byte("portage-builder"))
	object.ID = "service/portage-builder/linux-amd64"
	object.Kind = "service-binary"
	object.Path = "services/portage-builder-linux-amd64"
	object.Platform = "linux-amd64"
	object.Executable = true
	if err := testLock(object).Validate(); err != nil {
		t.Fatalf("content-addressed service binary rejected: %v", err)
	}
}

func TestTrackedPackerInputLockExampleIsStructurallyValid(t *testing.T) {
	if _, err := LoadInputLock(filepath.Join("..", "..", "image-factory", "offline", "inputs.lock.example.json")); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializeInputLockMeasuresFilesAndRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "factory", "runner.sh")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	draft := InputLock{Version: 1, BundleID: "mirror/test", StrictOffline: true, AllowedHosts: []string{"mirror.internal"}, Objects: []InputObject{{
		ID: "script/runner", Kind: "script", Path: "factory/runner.sh", SHA256: strings.Repeat("0", 64), Size: 0, Executable: true,
	}}}
	draftPath := writeTestJSON(t, root, "draft.json", draft)
	lock, err := MaterializeInputLock(draftPath, root)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Objects[0].Size != 10 || lock.Objects[0].SHA256 == strings.Repeat("0", 64) {
		t.Fatalf("object was not materialized: %+v", lock.Objects[0])
	}

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeInputLock(draftPath, root); err == nil {
		t.Fatal("symlinked draft object was accepted")
	}
}

func TestInputLockRequiresPackedTerraformProvider(t *testing.T) {
	object := testObject([]byte("provider zip"))
	object.ID = "terraform-provider/telmate-proxmox/3.0.2-rc04/linux-amd64"
	object.Kind = "terraform-provider"
	object.Platform = "linux-amd64"
	object.Path = "terraform/providers/registry.terraform.io/telmate/proxmox/terraform-provider-proxmox_3.0.2-rc04_linux_amd64.zip"
	object.Executable = false
	if err := testLock(object).Validate(); err != nil {
		t.Fatalf("packed Terraform provider rejected: %v", err)
	}

	for _, mutate := range []func(*InputObject){
		func(value *InputObject) {
			value.Path = "terraform/providers/registry.terraform.io/telmate/proxmox/3.0.2-rc04/linux_amd64/terraform-provider-proxmox_v3.0.2-rc04"
		},
		func(value *InputObject) { value.Executable = true },
		func(value *InputObject) {
			value.Path = "terraform/providers/registry.terraform.io/telmate/proxmox/terraform-provider-proxmox_3.0.2-rc04_darwin_arm64.zip"
		},
	} {
		invalid := object
		mutate(&invalid)
		if err := testLock(invalid).Validate(); err == nil {
			t.Fatalf("invalid Terraform provider accepted: %+v", invalid)
		}
	}
}

func TestInputLockRequiresPEMCAFile(t *testing.T) {
	object := testObject([]byte("certificate"))
	object.ID = "ca/internal-mirror"
	object.Kind = "ca-bundle"
	object.Path = "keys/internal-mirror.crt"
	object.Platform = ""
	if err := testLock(object).Validate(); err != nil {
		t.Fatalf("locked CA certificate rejected: %v", err)
	}
	object.Path = "keys/internal-mirror.pem"
	if err := testLock(object).Validate(); err == nil {
		t.Fatal("CA object without .crt suffix was accepted")
	}
	object.Path = "keys/internal-mirror.crt"
	object.Executable = true
	if err := testLock(object).Validate(); err == nil {
		t.Fatal("executable CA object was accepted")
	}
}

func TestPreflightDoesNotTreatGuestBinaryAsHostTool(t *testing.T) {
	root := t.TempDir()
	data := []byte("portage-builder")
	object := testObject(data)
	object.ID = "service/portage-builder/linux-amd64"
	object.Kind = "service-binary"
	object.Platform = "linux-amd64"
	object.Path = "services/portage-builder-linux-amd64"
	path := filepath.Join(root, object.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
	report, err := Preflight(root, testLock(object), "base-systemd")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 0 || report.Verified != 1 {
		t.Fatalf("guest service binary was compared with host runtime: %+v", report)
	}
}

func TestPreflightRejectsHostRuntimePlatformMismatch(t *testing.T) {
	root := t.TempDir()
	data := []byte("plugin")
	object := testObject(data)
	if object.Platform == "linux-amd64" {
		object.Platform = "darwin-arm64"
	} else {
		object.Platform = "linux-amd64"
	}
	path := filepath.Join(root, object.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
	report, err := Preflight(root, testLock(object), "base-systemd")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 || report.Missing[0].Reason == "" {
		t.Fatalf("platform mismatch accepted: %+v", report)
	}
}
