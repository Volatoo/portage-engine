package builder

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/distcc"
	"github.com/slchris/portage-engine/pkg/config"
)

type compileSchedulerStub struct {
	mu             sync.Mutex
	request        distcc.ReservationRequest
	lease          *distcc.Lease
	released       bool
	observations   []distcc.Observation
	heartbeats     int
	heartbeatFails int
	checkErr       error
}

func (s *compileSchedulerStub) ReserveCompileSlots(
	_ context.Context, request distcc.ReservationRequest,
) (*distcc.Lease, error) {
	s.mu.Lock()
	s.request = request
	s.mu.Unlock()
	if s.lease == nil {
		return nil, nil
	}
	lease := *s.lease
	lease.Pool = request.Pool
	lease.PoolID, _ = request.Pool.ID()
	lease.BuilderID = request.BuilderID
	lease.BuilderNetworkIdentity = request.BuilderNetworkIdentity
	lease.AttemptID = request.AttemptID
	lease.AttemptFence = request.AttemptFence
	lease.FallbackPolicy = request.FallbackPolicy
	lease.ExpiresAt = time.Now().Add(time.Minute)
	return &lease, nil
}

func (s *compileSchedulerStub) HeartbeatCompileLease(
	context.Context, distcc.Lease, time.Duration, time.Duration,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeats++
	if s.heartbeatFails > 0 {
		s.heartbeatFails--
		return errors.New("compile lease heartbeat rejected by expiry or fence")
	}
	return nil
}

func (s *compileSchedulerStub) CheckCompileLease(
	context.Context, distcc.Lease, time.Duration,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkErr
}

func (s *compileSchedulerStub) RecordCompileObservation(
	_ context.Context, _ distcc.Lease, observation distcc.Observation,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observations = append(s.observations, observation)
	return nil
}

func (s *compileSchedulerStub) ReleaseCompileLease(
	_ context.Context, _ distcc.Lease, _ string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = true
	return nil
}

func distCCManagerConfig(fallback string) *config.ServerConfig {
	return &config.ServerConfig{
		CloudProvider: "pve", BuildMode: "native-gentoo",
		DistCCAlphaEnabled:             true,
		DistCCPackageAllowlist:         []string{"sys-devel/llvm"},
		DistCCCHOST:                    "x86_64-pc-linux-gnu",
		DistCCCompilerDigest:           "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DistCCToolchainImageGeneration: "gentoo-g1",
		DistCCCPUFeatures:              []string{"avx2"}, DistCCNetworkZone: "build-a",
		DistCCIsolatedNetworkCIDRs: []string{"10.44.0.0/24"},
		DistCCSlotsPerJob:          2, DistCCLeaseSeconds: 60,
		DistCCWorkerFreshnessSeconds: 45, DistCCFallbackPolicy: fallback,
	}
}

func TestManagerCompileLeaseRunsAfterProjectClaimAndReleases(t *testing.T) {
	cfg := distCCManagerConfig(distcc.FallbackLocal)
	manager := NewManager(cfg)
	stub := &compileSchedulerStub{lease: &distcc.Lease{
		ID: "lease-a", WorkerID: "compile-a", WorkerEndpoint: "10.44.0.8:3632",
		Fence: 3, Slots: 2,
	}}
	manager.compileQueue = stub
	manager.jobs["job-a"] = &BuildStatus{
		JobID: "job-a", ProjectID: "11111111-1111-4111-8111-111111111111",
		AttemptID: "22222222-2222-4222-8222-222222222222", FenceToken: 7,
	}
	request := &BuildRequest{
		ProjectID:   "11111111-1111-4111-8111-111111111111",
		PackageName: "sys-devel/llvm", Arch: "amd64",
	}
	lease, finish, err := manager.beginCompileLease(
		"job-a", "builder-a", "10.44.0.21", request,
	)
	if err != nil || lease == nil {
		t.Fatalf("lease=%+v err=%v", lease, err)
	}
	if stub.request.ProjectID != request.ProjectID ||
		stub.request.AttemptID != manager.jobs["job-a"].AttemptID ||
		stub.request.AttemptFence != manager.jobs["job-a"].FenceToken ||
		stub.request.Pool.ProjectTrustDomain != request.ProjectID ||
		stub.request.Slots != 2 {
		t.Fatalf("reservation crossed project/fairness boundary: %+v", stub.request)
	}
	manager.recordCompileReport("job-a", lease, map[string]interface{}{
		distcc.MetadataReportKey: distcc.Report{Observations: []distcc.Observation{{
			Outcome: distcc.OutcomeRemote, Count: 4, NetworkBytes: 8192,
		}}},
	})
	finish("")
	if !stub.released {
		t.Fatal("compile lease was not released")
	}
	if len(stub.observations) != 2 ||
		stub.observations[0].Outcome != distcc.OutcomeHit ||
		stub.observations[1].Outcome != distcc.OutcomeRemote ||
		stub.observations[1].NetworkBytes != 8192 {
		t.Fatalf("reservation/compiler telemetry was not persisted: %+v", stub.observations)
	}
}

func TestManagerCompileLeaseShipsNormalizedBundleEligibility(t *testing.T) {
	cfg := distCCManagerConfig(distcc.FallbackBlocked)
	cfg.DistCCPackageAllowlist = []string{"dev-qt/qtwebengine"}
	manager := NewManager(cfg)
	stub := &compileSchedulerStub{lease: &distcc.Lease{
		ID: "lease-a", WorkerID: "compile-a", WorkerEndpoint: "10.44.0.8:3632",
		Fence: 3, Slots: 2,
	}}
	manager.compileQueue = stub
	manager.jobs["job-a"] = &BuildStatus{
		JobID: "job-a", ProjectID: "11111111-1111-4111-8111-111111111111",
		AttemptID: "22222222-2222-4222-8222-222222222222", FenceToken: 7,
	}
	request := &BuildRequest{
		ProjectID:   "11111111-1111-4111-8111-111111111111",
		PackageName: "dev-qt/qtwebengine", Arch: "amd64",
		ConfigBundle: &ConfigBundle{Packages: &BuildPackageSpec{
			Packages: []PackageSpec{
				{Atom: "=dev-qt/qtwebengine-6.7.2"}, {Atom: "dev-lang/rust"},
			},
		}},
	}
	lease, finish, err := manager.beginCompileLease(
		"job-a", "builder-a", "10.44.0.21", request,
	)
	if err != nil || lease == nil {
		t.Fatalf("lease=%+v err=%v", lease, err)
	}
	defer finish("")
	if len(lease.EligibleAtoms) != 1 || lease.EligibleAtoms[0] != "dev-qt/qtwebengine" {
		t.Fatalf("lease eligibility = %v", lease.EligibleAtoms)
	}

	// A bundle whose atoms are all outside the reviewed allowlist must not
	// consume a compile slot the builder can never use.
	request.ConfigBundle.Packages.Packages = []PackageSpec{{Atom: "dev-lang/rust"}}
	lease, _, err = manager.beginCompileLease(
		"job-a", "builder-a", "10.44.0.21", request,
	)
	if err != nil || lease != nil {
		t.Fatalf("ineligible bundle reserved a slot: lease=%+v err=%v", lease, err)
	}
}

func TestManagerCompilePoolDerivationFailureHonoursFallbackPolicy(t *testing.T) {
	for _, test := range []struct {
		fallback  string
		wantError bool
	}{
		{distcc.FallbackLocal, false}, {distcc.FallbackBlocked, true},
	} {
		cfg := distCCManagerConfig(test.fallback)
		// An operator typo in the compiler digest only produces a config
		// warning today, so it reaches pool derivation at build time.
		cfg.DistCCCompilerDigest = "gcc-13.2.0"
		manager := NewManager(cfg)
		manager.compileQueue = &compileSchedulerStub{}
		manager.jobs["job-a"] = &BuildStatus{
			JobID: "job-a", ProjectID: "11111111-1111-4111-8111-111111111111",
			AttemptID: "22222222-2222-4222-8222-222222222222", FenceToken: 7,
		}
		lease, _, err := manager.beginCompileLease(
			"job-a", "builder-a", "10.44.0.21", &BuildRequest{
				ProjectID:   "11111111-1111-4111-8111-111111111111",
				PackageName: "sys-devel/llvm", Arch: "amd64",
			},
		)
		if (err != nil) != test.wantError || lease != nil {
			t.Fatalf("fallback=%s lease=%+v err=%v", test.fallback, lease, err)
		}
	}
}

func TestManagerCompileEndpointRequiresReviewedIsolatedNetwork(t *testing.T) {
	cfg := distCCManagerConfig(distcc.FallbackBlocked)
	cfg.DistCCIsolatedNetworkCIDRs = nil
	manager := NewManager(cfg)
	manager.compileQueue = &compileSchedulerStub{lease: &distcc.Lease{
		ID: "lease-a", WorkerID: "compile-a", WorkerEndpoint: "203.0.113.9:3632",
		Fence: 3, Slots: 2,
	}}
	manager.jobs["job-a"] = &BuildStatus{
		JobID: "job-a", ProjectID: "11111111-1111-4111-8111-111111111111",
		AttemptID: "22222222-2222-4222-8222-222222222222", FenceToken: 7,
	}
	lease, _, err := manager.beginCompileLease(
		"job-a", "builder-a", "10.44.0.21", &BuildRequest{
			ProjectID:   "11111111-1111-4111-8111-111111111111",
			PackageName: "sys-devel/llvm", Arch: "amd64",
		},
	)
	if err == nil || lease != nil {
		t.Fatalf("empty isolated CIDR list authorized a public endpoint: lease=%+v err=%v", lease, err)
	}
}

func TestManagerCompileHeartbeatSurvivesTransientRejection(t *testing.T) {
	manager := NewManager(distCCManagerConfig(distcc.FallbackLocal))
	stub := &compileSchedulerStub{heartbeatFails: 2}
	manager.compileQueue = stub
	t.Cleanup(manager.Shutdown)
	lease := distcc.Lease{ID: "lease-a", WorkerID: "compile-a", Fence: 3, Slots: 2}
	stop := make(chan struct{})
	if !manager.renewCompileLease(lease, 5*time.Second, time.Second, stop) {
		t.Fatal("two transient heartbeat rejections lost the lease")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.heartbeats != 3 {
		t.Fatalf("heartbeat attempts = %d", stub.heartbeats)
	}
}

func TestManagerCompileOutputFenceHonoursFallbackPolicy(t *testing.T) {
	for _, test := range []struct {
		fallback   string
		wantReason string
		wantError  bool
	}{
		{distcc.FallbackLocal, "lease-expired", false},
		{distcc.FallbackBlocked, "lease-fenced", true},
	} {
		manager := NewManager(distCCManagerConfig(test.fallback))
		stub := &compileSchedulerStub{
			checkErr: errors.New("compile lease is expired, stale, or fenced"),
		}
		manager.compileQueue = stub
		manager.jobs["job-a"] = &BuildStatus{JobID: "job-a"}
		reason, err := manager.checkCompileOutput("job-a", &distcc.Lease{
			ID: "lease-a", FallbackPolicy: test.fallback,
		})
		if reason != test.wantReason || (err != nil) != test.wantError {
			t.Fatalf("fallback=%s reason=%q err=%v", test.fallback, reason, err)
		}
		manager.Shutdown()
	}
}

func TestManagerCompileCapacityPolicyLocalOrBlocked(t *testing.T) {
	for _, test := range []struct {
		fallback  string
		wantError bool
	}{
		{distcc.FallbackLocal, false}, {distcc.FallbackBlocked, true},
	} {
		manager := NewManager(distCCManagerConfig(test.fallback))
		manager.compileQueue = &compileSchedulerStub{}
		manager.jobs["job-a"] = &BuildStatus{
			JobID: "job-a", ProjectID: "11111111-1111-4111-8111-111111111111",
			AttemptID: "22222222-2222-4222-8222-222222222222", FenceToken: 7,
		}
		lease, _, err := manager.beginCompileLease(
			"job-a", "builder-a", "10.44.0.21", &BuildRequest{
				ProjectID:   "11111111-1111-4111-8111-111111111111",
				PackageName: "sys-devel/llvm", Arch: "amd64",
			},
		)
		if (err != nil) != test.wantError || lease != nil {
			t.Fatalf("fallback=%s lease=%+v err=%v", test.fallback, lease, err)
		}
	}
}

// supersededClaimScheduler answers the attempt-fence question the way the
// ledger does once another attempt has taken the job over.
type supersededClaimScheduler struct{ phaseGateScheduler }

func (*supersededClaimScheduler) CheckClaim(context.Context, *BuildStatus) error {
	return errors.New("durable claim fence is not active")
}

// TestManagerCompileOutputFenceSeparatesExpiryFromSupersession pins the two
// cases the authorization read collapses into one SQL answer. Local fallback
// was reviewed for a compile reservation that lapsed under an attempt this
// replica still owns; it was never a licence to accept output produced after
// another attempt took the job, which would make the output fence advisory.
func TestManagerCompileOutputFenceSeparatesExpiryFromSupersession(t *testing.T) {
	for _, test := range []struct {
		name             string
		scheduler        DurableScheduler
		wantReason       string
		wantError        bool
		wantObservations int
	}{
		{"live attempt", &phaseGateScheduler{}, "lease-expired", false, 1},
		{"superseded attempt", &supersededClaimScheduler{}, "lease-fenced", true, 0},
	} {
		manager := NewManager(distCCManagerConfig(distcc.FallbackLocal))
		stub := &compileSchedulerStub{
			checkErr: errors.New("compile lease is expired, stale, or fenced"),
		}
		manager.compileQueue = stub
		manager.scheduler = test.scheduler
		manager.jobs["job-a"] = &BuildStatus{
			JobID: "job-a", Status: "building",
			AttemptID: "22222222-2222-4222-8222-222222222222", FenceToken: 7,
		}
		reason, err := manager.checkCompileOutput("job-a", &distcc.Lease{
			ID: "lease-a", FallbackPolicy: distcc.FallbackLocal,
		})
		if reason != test.wantReason || (err != nil) != test.wantError {
			t.Fatalf("%s: reason=%q err=%v", test.name, reason, err)
		}
		stub.mu.Lock()
		observations := len(stub.observations)
		stub.mu.Unlock()
		if observations != test.wantObservations {
			t.Fatalf("%s: recorded %d fallback observations", test.name, observations)
		}
		manager.Shutdown()
	}
}
