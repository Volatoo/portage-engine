package distcc

import (
	"net"
	"testing"
	"time"
)

func testPool() Pool {
	return Pool{
		Architecture: "amd64", CHOST: "x86_64-pc-linux-gnu",
		CompilerDigest:           "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ToolchainImageGeneration: "gentoo-2026-08-01-g1",
		CPUFeatures:              []string{"sse4.2", "avx2", "avx2"},
		NetworkZone:              "build-a", ProjectTrustDomain: "project-tenant-a",
	}
}

func TestPoolIDUsesEveryExactDimension(t *testing.T) {
	base := testPool()
	baseID, err := base.ID()
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := base.Normalize()
	if err != nil || len(normalized.CPUFeatures) != 2 || normalized.CPUFeatures[0] != "avx2" {
		t.Fatalf("normalized=%+v err=%v", normalized, err)
	}
	mutations := []func(*Pool){
		func(p *Pool) { p.Architecture = "arm64" },
		func(p *Pool) { p.CHOST = "aarch64-unknown-linux-gnu" },
		func(p *Pool) {
			p.CompilerDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		},
		func(p *Pool) { p.ToolchainImageGeneration = "gentoo-g2" },
		func(p *Pool) { p.CPUFeatures = []string{"avx2"} },
		func(p *Pool) { p.NetworkZone = "build-b" },
		func(p *Pool) { p.ProjectTrustDomain = "project-tenant-b" },
	}
	for index, mutate := range mutations {
		candidate := base
		candidate.CPUFeatures = append([]string(nil), base.CPUFeatures...)
		mutate(&candidate)
		id, idErr := candidate.ID()
		if idErr != nil {
			t.Fatalf("mutation %d: %v", index, idErr)
		}
		if id == baseID {
			t.Fatalf("mutation %d did not change the exact pool ID", index)
		}
	}
}

func TestBuilderPolicyRequiresAllowlistExactPoolAndIsolatedNetwork(t *testing.T) {
	_, network, _ := net.ParseCIDR("10.44.0.0/24")
	policy, err := NewBuilderPolicy(BuilderPolicy{
		Enabled: true, Allowlist: []string{"sys-devel/llvm"},
		IsolatedNetworks: []*net.IPNet{network}, Architecture: "amd64",
		CHOST:           "x86_64-pc-linux-gnu",
		CompilerDigest:  testPool().CompilerDigest,
		ImageGeneration: testPool().ToolchainImageGeneration,
		CPUFeatures:     []string{"avx2", "sse4.2"}, NetworkZone: "build-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	poolID, _ := testPool().ID()
	lease := &Lease{
		ID: "lease-a", WorkerID: "worker-a", WorkerEndpoint: "10.44.0.8:3632",
		BuilderID: "builder-a", BuilderNetworkIdentity: "spiffe://build/builder-a",
		AttemptID: "attempt-a", AttemptFence: 4,
		Fence: 1, Slots: 2, PoolID: poolID, Pool: testPool(),
		FallbackPolicy: FallbackLocal, ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := policy.Authorize(
		"sys-devel/llvm", lease.Pool.ProjectTrustDomain,
		lease.AttemptID, lease.AttemptFence, lease, time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	if err := policy.Authorize(
		"dev-lang/rust", lease.Pool.ProjectTrustDomain,
		lease.AttemptID, lease.AttemptFence, lease, time.Now(),
	); err == nil {
		t.Fatal("non-allowlisted Rust package was authorized")
	}
	outside := *lease
	outside.WorkerEndpoint = "192.0.2.10:3632"
	if err := policy.Authorize(
		"sys-devel/llvm", lease.Pool.ProjectTrustDomain,
		lease.AttemptID, lease.AttemptFence, &outside, time.Now(),
	); err == nil {
		t.Fatal("public/outside endpoint was authorized")
	}
	pump := *lease
	pump.Pump = true
	if err := policy.Authorize(
		"sys-devel/llvm", lease.Pool.ProjectTrustDomain,
		lease.AttemptID, lease.AttemptFence, &pump, time.Now(),
	); err == nil {
		t.Fatal("pump mode was authorized")
	}
	if err := policy.Authorize(
		"sys-devel/llvm", "project-tenant-b",
		lease.AttemptID, lease.AttemptFence, lease, time.Now(),
	); err == nil {
		t.Fatal("lease crossed the builder's claimed project trust domain")
	}
}

func TestObservationRejectsUnboundedLabels(t *testing.T) {
	if err := (Observation{Outcome: OutcomeRemote, Count: 3, NetworkBytes: 42}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Observation{Outcome: OutcomeFallback, FailureReason: "worker-id-123", Count: 1}).Validate(); err == nil {
		t.Fatal("unbounded failure label was accepted")
	}
}
