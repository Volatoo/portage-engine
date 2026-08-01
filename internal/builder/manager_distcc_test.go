package builder

import (
	"context"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/distcc"
	"github.com/slchris/portage-engine/pkg/config"
)

type compileSchedulerStub struct {
	request      distcc.ReservationRequest
	lease        *distcc.Lease
	released     bool
	observations []distcc.Observation
}

func (s *compileSchedulerStub) ReserveCompileSlots(
	_ context.Context, request distcc.ReservationRequest,
) (*distcc.Lease, error) {
	s.request = request
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

func (*compileSchedulerStub) HeartbeatCompileLease(
	context.Context, distcc.Lease, time.Duration, time.Duration,
) error {
	return nil
}

func (*compileSchedulerStub) CheckCompileLease(
	context.Context, distcc.Lease, time.Duration,
) error {
	return nil
}

func (s *compileSchedulerStub) RecordCompileObservation(
	_ context.Context, _ distcc.Lease, observation distcc.Observation,
) error {
	s.observations = append(s.observations, observation)
	return nil
}

func (s *compileSchedulerStub) ReleaseCompileLease(
	_ context.Context, _ distcc.Lease, _ string,
) error {
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
