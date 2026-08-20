package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func validCatalog() *Catalog {
	return &Catalog{
		Version: 1,
		Profiles: []ProfileDefinition{{
			ID: "pe/amd64/base-v1", Arch: "amd64",
			ProfilePath:         "portage-engine/amd64/23.0/systemd/base",
			BinhostPath:         "releases/amd64/binpackages/23.0/x86-64_pe-base-v1",
			ProfileRepositoryID: "pe-profiles",
			Parents:             []ProfileParentDefinition{{RepositoryID: "gentoo", ProfilePath: "default/linux/amd64/23.0/systemd"}},
			LegacyProfiles:      []string{"default/linux/amd64/23.0"},
			RepositoryIDs:       []string{"gentoo", "pe-profiles"}, ImageID: "image/base-g1",
			MirrorBundleID: "mirror/2026-07-22", DefaultResourceClass: "small",
			RequiredFeatures: []string{"binpkg-multi-instance", "sandbox", "userpriv"},
			EgressPolicyID:   "egress/internal",
			Default:          true, Channel: "stable",
		}},
		Repositories: []RepositoryDefinition{{
			ID: "gentoo", Name: "gentoo", Location: "/var/db/repos/gentoo",
			SyncType: "git", SyncURI: "https://git.internal/gentoo.git",
			Revision: "0123456789abcdef0123456789abcdef01234567", Channel: "stable",
		}, {
			ID: "pe-profiles", Name: "pe-profiles", Location: "/var/db/repos/pe-profiles",
			SyncType: "git", SyncURI: "https://git.internal/pe-profiles.git",
			Revision: "1123456789abcdef0123456789abcdef01234567", Channel: "stable",
		}},
		Images: []ImageManifest{{
			ID: "image/base-g1", ProfileID: "pe/amd64/base-v1", Generation: "g1", Provider: "pve",
			Arch: "amd64", BuildMode: "native-gentoo", Template: "pe-base-g1",
			Digest: testDigest, RootfsSource: "catalyst", RootfsManifestDigest: testDigest,
			PackageSetIDs: []string{"pe/runtime-v1", "pe/build-test-v1"}, PackageSetCatalogDigest: testDigest,
			Channel: "stable",
		}},
		MirrorBundles: []MirrorBundle{{ID: "mirror/2026-07-22", Digest: testDigest, CreatedAt: time.Now().UTC().Add(-time.Hour),
			FreshUntil: time.Now().UTC().Add(24 * time.Hour), AdvisoryWatermark: "2026-07-22T00:00:00Z", Channel: "stable"}},
		ResourceClasses: []ResourceClass{{
			ID: "small", MachineSpec: map[string]string{"cores": "2", "memory": "4096", "disk_size": "40"},
			MaxRuntimeMinutes: 60, CloudCostMicrounitsPerMinute: 1000,
		}},
		EgressPolicies: []EgressPolicy{{
			ID: "egress/internal", Mode: EgressModeEnforce, Channel: "stable",
			Rules: []EgressRule{{ID: "git", Hosts: []string{"git.internal"}, CIDRs: []string{"10.31.0.2/32"}, Protocol: "tcp", Ports: []int{443}}},
		}},
	}
}

func TestCatalogResolve(t *testing.T) {
	c := validCatalog()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	ctx, err := c.Resolve(ResolveRequest{LegacyProfile: "default/linux/amd64/23.0", Arch: "amd64"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if ctx.ProfileID != "pe/amd64/base-v1" || ctx.Template != "pe-base-g1" || ctx.Provider != "pve" {
		t.Fatalf("unexpected context: %+v", ctx)
	}
	if ctx.ExecutionZone != "default" {
		t.Fatalf("unexpected default execution zone: %q", ctx.ExecutionZone)
	}
	if ctx.BinhostPath != "releases/amd64/binpackages/23.0/x86-64_pe-base-v1" {
		t.Fatalf("unexpected binhost path: %q", ctx.BinhostPath)
	}
	if len(ctx.Repositories) != 2 || ctx.ProfileRepositoryName != "pe-profiles" || len(ctx.ProfileParents) != 1 {
		t.Fatalf("unexpected repositories: %+v", ctx.Repositories)
	}
	if ctx.MachineSpec["cores"] != "2" {
		t.Fatalf("resource class not resolved: %+v", ctx.MachineSpec)
	}
	if ctx.MaxRuntimeMinutes != 60 ||
		ctx.CloudCostMicrounitsPerMinute != 1000 {
		t.Fatalf("runtime/cost accounting contract not resolved: %+v", ctx)
	}
	if len(ctx.PackageSetIDs) != 2 || ctx.PackageSetCatalogDigest != testDigest {
		t.Fatalf("package sets not resolved: %+v", ctx)
	}
	if strings.Join(ctx.RequiredFeatures, " ") != "binpkg-multi-instance sandbox userpriv" {
		t.Fatalf("required Portage FEATURES not resolved: %+v", ctx.RequiredFeatures)
	}
	if ctx.EgressPolicy.ID != "egress/internal" || !strings.HasPrefix(ctx.EgressPolicyDigest, "sha256:") {
		t.Fatalf("egress policy not resolved: %+v", ctx)
	}
}

func TestCatalogResolveAcceptsLogicalRepositoryNameForRevisionScopedID(t *testing.T) {
	c := validCatalog()
	oldID := c.Repositories[0].ID
	newID := oldID + "/rev-" + c.Repositories[0].Revision
	c.Repositories[0].ID = newID
	c.Profiles[0].RepositoryIDs[0] = newID
	c.Profiles[0].Parents[0].RepositoryID = newID
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	context, err := c.Resolve(ResolveRequest{ProfileID: c.Profiles[0].ID, RepositoryIDs: []string{oldID}})
	if err != nil {
		t.Fatal(err)
	}
	if context.Repositories[0].ID != newID || context.Repositories[0].Name != oldID {
		t.Fatalf("logical repository name did not resolve to immutable ID: %+v", context.Repositories)
	}
}

func TestCatalogResolveRejectsUnregisteredAndMismatch(t *testing.T) {
	c := validCatalog()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}

	tests := []ResolveRequest{
		{ProfileID: "missing"},
		{ProfileID: "pe/amd64/base-v1", Arch: "arm64"},
		{ProfileID: "pe/amd64/base-v1", CloudProvider: "aws"},
		{ProfileID: "pe/amd64/base-v1", RepositoryIDs: []string{"evil"}},
		{ProfileID: "pe/amd64/base-v1", ResourceClass: "huge"},
	}
	for _, req := range tests {
		if _, err := c.Resolve(req); err == nil {
			t.Errorf("Resolve(%+v) unexpectedly succeeded", req)
		}
	}
}

func TestCatalogStableEntriesRequireTrustMetadata(t *testing.T) {
	c := validCatalog()
	c.Images[0].Digest = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "stable image") {
		t.Fatalf("Validate() error = %v, want stable image error", err)
	}
}

func TestCatalogAllowsIntegrityProtectedHTTPArtifactPlane(t *testing.T) {
	c := validCatalog()
	for index := range c.Repositories {
		c.Repositories[index].SyncURI = "http://10.31.0.2/git/" + c.Repositories[index].Name + ".bundle"
		c.Repositories[index].Digest = testDigest
	}
	c.EgressPolicies[0].Rules = []EgressRule{{
		ID: "git", Hosts: []string{"10.31.0.2"}, CIDRs: []string{"10.31.0.2/32"}, Protocol: "tcp", Ports: []int{80},
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("integrity-protected HTTP mirror was rejected: %v", err)
	}
	c = validCatalog()
	c.Repositories[0].SyncURI = "http://10.31.0.2/git/gentoo.bundle"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "snapshot digest") {
		t.Fatalf("HTTP mirror without a snapshot digest was accepted: %v", err)
	}
}

func TestCatalogResolveRejectsStaleBundle(t *testing.T) {
	c := validCatalog()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResolveAt(ResolveRequest{ProfileID: "pe/amd64/base-v1"}, c.MirrorBundles[0].FreshUntil); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expired mirror bundle was accepted: %v", err)
	}
}

func TestCatalogRejectsUnsafeExecutionMetadata(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Catalog)
	}{
		{name: "unsupported provider", mutate: func(c *Catalog) { c.Images[0].Provider = "unknown" }},
		{name: "unsafe execution zone", mutate: func(c *Catalog) { c.Images[0].ExecutionZone = "LAN/A" }},
		{name: "removed docker build mode", mutate: func(c *Catalog) { c.Images[0].BuildMode = "docker" }},
		{name: "oversized resource", mutate: func(c *Catalog) { c.ResourceClasses[0].MachineSpec["cores"] = "9999" }},
		{name: "missing runtime budget", mutate: func(c *Catalog) { c.ResourceClasses[0].MaxRuntimeMinutes = 0 }},
		{name: "missing cloud cost rate", mutate: func(c *Catalog) { c.ResourceClasses[0].CloudCostMicrounitsPerMinute = 0 }},
		{name: "abbreviated stable commit", mutate: func(c *Catalog) { c.Repositories[0].Revision = "0123456" }},
		{name: "stable rsync unsupported", mutate: func(c *Catalog) {
			c.Repositories[0].SyncType = "rsync"
			c.Repositories[0].SyncURI = "rsync://mirror.internal/gentoo"
			c.Repositories[0].Revision = ""
			c.Repositories[0].Digest = testDigest
		}},
		{name: "profile repository missing", mutate: func(c *Catalog) { c.Profiles[0].ProfileRepositoryID = "" }},
		{name: "binhost path traversal", mutate: func(c *Catalog) { c.Profiles[0].BinhostPath = "../binpkgs" }},
		{name: "binhost architecture mismatch", mutate: func(c *Catalog) { c.Profiles[0].BinhostPath = "releases/arm64/binpackages/23.0/arm64" }},
		{name: "profile parent drift unbound", mutate: func(c *Catalog) { c.Profiles[0].Parents[0].RepositoryID = "missing" }},
		{name: "two revisions of one repository", mutate: func(c *Catalog) {
			c.Repositories = append(c.Repositories, RepositoryDefinition{ID: "gentoo/other", Name: "gentoo", Location: "/var/db/repos/gentoo", SyncType: "git", SyncURI: "https://git.internal/gentoo.git", Revision: "2123456789abcdef0123456789abcdef01234567", Channel: "stable"})
			c.Profiles[0].RepositoryIDs = append(c.Profiles[0].RepositoryIDs, "gentoo/other")
		}},
		{name: "image profile mismatch", mutate: func(c *Catalog) { c.Images[0].ProfileID = "pe/amd64/other" }},
		{name: "missing required features", mutate: func(c *Catalog) { c.Profiles[0].RequiredFeatures = nil }},
		{name: "unsorted required features", mutate: func(c *Catalog) { c.Profiles[0].RequiredFeatures = []string{"userpriv", "sandbox"} }},
		{name: "duplicate required features", mutate: func(c *Catalog) { c.Profiles[0].RequiredFeatures = []string{"sandbox", "sandbox"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validCatalog()
			tt.mutate(c)
			if err := c.Validate(); err == nil {
				t.Fatal("unsafe catalog was accepted")
			}
		})
	}
}

func TestLoadStrictJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")
	data := `{"version":1,"profiles":[],"repositories":[],"images":[],"mirror_bundles":[],"unknown":true}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("Load() error = %v, want unknown field error", err)
	}
}

func TestExampleCatalogLoads(t *testing.T) {
	catalog, err := Load(filepath.Join("..", "..", "configs", "catalog.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.ResolveAt(ResolveRequest{ProfileID: "pe/amd64/glibc/systemd/base-v1"}, catalog.MirrorBundles[0].CreatedAt.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "not published") {
		t.Fatalf("candidate example catalog was accepted for a build: %v", err)
	}
}

func TestResolveRejectsCandidateChannel(t *testing.T) {
	c := validCatalog()
	c.Profiles[0].Channel = "candidate"
	c.Repositories[0].Channel = "candidate"
	c.Repositories[1].Channel = "candidate"
	c.Images[0].Channel = "candidate"
	c.MirrorBundles[0].Channel = "candidate"
	c.EgressPolicies[0].Channel = "candidate"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResolveAt(ResolveRequest{ProfileID: c.Profiles[0].ID}, c.MirrorBundles[0].CreatedAt.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "not published") {
		t.Fatalf("candidate profile was accepted for a build: %v", err)
	}
}

func TestCompatibilityCatalog(t *testing.T) {
	c := NewCompatibility(CompatibilityOptions{
		Provider: "pve", BuildMode: "native-gentoo", Template: "gentoo-template",
		GentooSyncType: "git", GentooSyncURI: "https://git.internal/gentoo.git",
	})
	ctx, err := c.Resolve(ResolveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if ctx.ProfileID != "compat/amd64/default" || ctx.Template != "gentoo-template" {
		t.Fatalf("unexpected compatibility context: %+v", ctx)
	}
}
