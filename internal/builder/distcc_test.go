package builder

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/distcc"
)

func builderDistCCPool() distcc.Pool {
	return distcc.Pool{
		Architecture: "amd64", CHOST: "x86_64-pc-linux-gnu",
		CompilerDigest:           "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ToolchainImageGeneration: "gentoo-g1",
		CPUFeatures:              []string{"avx2"}, NetworkZone: "build-a",
		ProjectTrustDomain: "project-a",
	}
}

func TestConfigureDistCCIsPackageScopedPumpDisabled(t *testing.T) {
	_, network, _ := net.ParseCIDR("10.44.0.0/24")
	policy, err := distcc.NewBuilderPolicy(distcc.BuilderPolicy{
		Enabled: true, Allowlist: []string{"sys-devel/llvm"},
		IsolatedNetworks: []*net.IPNet{network}, Architecture: "amd64",
		CHOST:           "x86_64-pc-linux-gnu",
		CompilerDigest:  builderDistCCPool().CompilerDigest,
		ImageGeneration: "gentoo-g1", CPUFeatures: []string{"avx2"},
		NetworkZone: "build-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	poolID, _ := builderDistCCPool().ID()
	lease := &distcc.Lease{
		ID: "lease-a", WorkerID: "worker-a", WorkerEndpoint: "10.44.0.9:3632",
		BuilderID: "builder-a", BuilderNetworkIdentity: "worker-cert:a",
		AttemptID: "attempt-a", AttemptFence: 5,
		Fence: 7, Slots: 2, PoolID: poolID, Pool: builderDistCCPool(),
		FallbackPolicy: distcc.FallbackLocal,
		ExpiresAt:      time.Now().Add(time.Minute),
	}
	root := t.TempDir()
	executor := NewBuildExecutorWithOptions(t.TempDir(), t.TempDir(), BuildOptions{
		Format: "gpkg", DistCCPolicy: policy,
	})
	bundle := &ConfigBundle{
		Config: &PortageConfig{},
		Packages: &BuildPackageSpec{Packages: []PackageSpec{
			{Atom: "sys-devel/llvm"}, {Atom: "dev-lang/rust"},
		}},
	}
	job := &BuildJob{ID: "job-a", Request: &LocalBuildRequest{
		ProjectID: lease.Pool.ProjectTrustDomain,
		AttemptID: lease.AttemptID, AttemptFence: lease.AttemptFence,
		CompileLease: lease,
	}}
	if err := executor.configureDistCC(root, bundle, job); err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(filepath.Join(root, "etc", "portage", "env", "portage-engine-distcc-lease"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), `FEATURES="-distcc -distcc-pump"`) ||
		strings.Contains(string(env), "DISTCC_PUMP") ||
		strings.Contains(string(env), lease.BuilderNetworkIdentity) {
		t.Fatalf("unsafe distcc env:\n%s", env)
	}
	for _, wrapper := range []string{"cc", "cxx"} {
		content, readErr := os.ReadFile(filepath.Join(root, "var", "lib", "portage-engine", "distcc-wrappers", wrapper))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !strings.Contains(string(content), `EBUILD_PHASE:-`) ||
			!strings.Contains(string(content), `= "-c"`) ||
			!strings.Contains(string(content), `DISTCC_FALLBACK=0`) ||
			!strings.Contains(string(content), `network_payload_bytes`) ||
			strings.Contains(string(content), "pump") {
			t.Fatalf("wrapper %s does not enforce compile-only/no-pump policy:\n%s", wrapper, content)
		}
	}
	mapping, err := os.ReadFile(filepath.Join(root, "etc", "portage", "package.env", "90-portage-engine-distcc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(mapping) != "sys-devel/llvm portage-engine-distcc-lease\n" {
		t.Fatalf("unexpected package mapping %q", mapping)
	}
	if !strings.Contains(job.Log, "pump disabled") {
		t.Fatalf("missing safe-policy log: %s", job.Log)
	}
}

func TestRecordDistCCReportAggregatesBoundedCompilerEvents(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "var", "lib", "portage-engine", "distcc-wrappers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "observations.tsv"), []byte(
		"remote\t\t100\t1\nremote\t\t250\t1\nlocal\t\t0\t1\nfallback\tconnect\t9\t1\ninvalid\tworker-a\t3\t1\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	job := &BuildJob{Request: &LocalBuildRequest{CompileLease: &distcc.Lease{ID: "lease-a"}}}
	(&BuildExecutor{}).recordDistCCReport(root, job)
	clone := job.Clone()
	value, ok := clone.Metadata[distcc.MetadataReportKey].(distcc.Report)
	if !ok {
		t.Fatalf("missing typed distcc report: %#v", clone.Metadata)
	}
	if len(value.Observations) != 3 {
		t.Fatalf("unbounded/invalid report events survived: %+v", value)
	}
	for _, observation := range value.Observations {
		if observation.Outcome == distcc.OutcomeRemote &&
			(observation.Count != 2 || observation.NetworkBytes != 350) {
			t.Fatalf("remote observations were not aggregated: %+v", observation)
		}
	}
}

func TestConfigureDistCCBlockedRejectsRequestWithoutEligiblePackage(t *testing.T) {
	policy, err := distcc.NewBuilderPolicy(distcc.BuilderPolicy{
		Enabled: true, Allowlist: []string{"sys-devel/llvm"},
		IsolatedNetworks: []*net.IPNet{{IP: net.IPv4(10, 44, 0, 0), Mask: net.CIDRMask(24, 32)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	executor := NewBuildExecutorWithOptions(t.TempDir(), t.TempDir(), BuildOptions{DistCCPolicy: policy})
	job := &BuildJob{Request: &LocalBuildRequest{CompileLease: &distcc.Lease{FallbackPolicy: distcc.FallbackBlocked}}}
	bundle := &ConfigBundle{Packages: &BuildPackageSpec{Packages: []PackageSpec{{Atom: "dev-lang/rust"}}}}
	if err := executor.configureDistCC(t.TempDir(), bundle, job); err == nil {
		t.Fatal("blocked policy silently compiled a non-eligible package locally")
	}
}

func TestConfigureDistCCUsesTheLeaseEligibleAtomsForVersionedBundleAtoms(t *testing.T) {
	_, network, _ := net.ParseCIDR("10.44.0.0/24")
	policy, err := distcc.NewBuilderPolicy(distcc.BuilderPolicy{
		Enabled: true, Allowlist: []string{"dev-qt/qtwebengine"},
		IsolatedNetworks: []*net.IPNet{network}, Architecture: "amd64",
		CHOST:           "x86_64-pc-linux-gnu",
		CompilerDigest:  builderDistCCPool().CompilerDigest,
		ImageGeneration: "gentoo-g1", CPUFeatures: []string{"avx2"},
		NetworkZone: "build-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	poolID, _ := builderDistCCPool().ID()
	lease := &distcc.Lease{
		ID: "lease-a", WorkerID: "worker-a", WorkerEndpoint: "10.44.0.9:3632",
		BuilderID: "builder-a", BuilderNetworkIdentity: "worker-cert:a",
		AttemptID: "attempt-a", AttemptFence: 5,
		Fence: 7, Slots: 2, PoolID: poolID, Pool: builderDistCCPool(),
		FallbackPolicy: distcc.FallbackBlocked,
		ExpiresAt:      time.Now().Add(time.Minute),
		EligibleAtoms:  []string{"dev-qt/qtwebengine"},
	}
	root := t.TempDir()
	executor := NewBuildExecutorWithOptions(t.TempDir(), t.TempDir(), BuildOptions{
		Format: "gpkg", DistCCPolicy: policy,
	})
	// The client's own bundle carries the versioned form the control plane
	// reserved a compile slot for.
	bundle := &ConfigBundle{
		Config: &PortageConfig{},
		Packages: &BuildPackageSpec{Packages: []PackageSpec{
			{Atom: "=dev-qt/qtwebengine-6.7.2"},
		}},
	}
	job := &BuildJob{ID: "job-a", Request: &LocalBuildRequest{
		ProjectID: lease.Pool.ProjectTrustDomain,
		AttemptID: lease.AttemptID, AttemptFence: lease.AttemptFence,
		CompileLease: lease,
	}}
	if err := executor.configureDistCC(root, bundle, job); err != nil {
		t.Fatal(err)
	}
	mapping, err := os.ReadFile(filepath.Join(root, "etc", "portage", "package.env", "90-portage-engine-distcc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(mapping) != "dev-qt/qtwebengine portage-engine-distcc-lease\n" {
		t.Fatalf("unexpected package mapping %q", mapping)
	}
}

func TestBuildEnvironmentCannotGloballyEnableDistCC(t *testing.T) {
	executor := NewBuildExecutor(t.TempDir(), t.TempDir())
	env := executor.buildEnvironment(
		PackageSpec{},
		&ConfigBundle{Config: &PortageConfig{Environment: map[string]string{
			"FEATURES": "distcc distcc-pump sandbox",
		}}},
		t.TempDir(), "",
	)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "distcc") || !strings.Contains(joined, "sandbox") {
		t.Fatalf("global FEATURES was not constrained: %s", joined)
	}
}
