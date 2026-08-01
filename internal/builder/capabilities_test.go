package builder

import (
	"reflect"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/pkg/config"
)

func TestPhaseCapabilityRequirementsAreExactAndStable(t *testing.T) {
	request := &BuildRequest{ResolvedContext: &catalog.ResolvedBuildContext{
		Provider: "pve", ExecutionZone: "lan-a", Arch: "amd64",
		BuildMode: "native-gentoo", ProfileID: "pe/amd64/base-v1",
		ImageID: "pe/base-g6", ImageGeneration: "g6",
		EgressPolicy:       catalog.EgressPolicy{ID: "egress/build"},
		EgressPolicyDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}
	first, err := PhaseCapabilityRequirements(request, "build")
	if err != nil {
		t.Fatal(err)
	}
	second, err := PhaseCapabilityRequirements(request, "build")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("requirements are unstable: %v != %v", first, second)
	}
	for _, required := range []string{
		"phase:build", "provider:pve", "zone:lan-a", "arch:amd64",
		"profile:pe/amd64/base-v1", "image:pe/base-g6@g6",
	} {
		if !containsCapability(first, required) {
			t.Fatalf("missing capability %q in %v", required, first)
		}
	}
	poolID, err := CapacityPoolID(request.ResolvedContext)
	if err != nil {
		t.Fatal(err)
	}
	if !containsCapability(first, "capacity-pool:"+poolID) {
		t.Fatalf("missing stable capacity pool in %v", first)
	}
}

func TestCapacityPoolIDPersistentExecutorBuildVector(t *testing.T) {
	poolID, err := CapacityPoolID(&catalog.ResolvedBuildContext{
		Provider: "pve", ExecutionZone: "zone-a", Arch: "amd64",
		BuildMode: "native-gentoo", ProfileID: "pe/amd64/base-v1",
		ImageID: "pe/amd64/base", ImageGeneration: "g17",
	})
	if err != nil {
		t.Fatal(err)
	}
	const expected = "pve-zone-a-amd64-db23fcaaaeb71219f0511397"
	if poolID != expected {
		t.Fatalf("capacity pool ID=%q, want build-script vector %q", poolID, expected)
	}
}

func TestNormalizeExecutorCapabilitiesRejectsMalformedLabels(t *testing.T) {
	for _, labels := range [][]string{
		nil,
		{"phase"},
		{"phase:build with-space"},
		{"Phase:build"},
	} {
		if _, err := NormalizeExecutorCapabilities(labels); err == nil {
			t.Fatalf("accepted malformed labels %v", labels)
		}
	}
}

func TestResolveExecutorCapabilitiesUsesProviderAndZone(t *testing.T) {
	manager := NewManager(&config.ServerConfig{
		MaxWorkers:    0,
		CloudProvider: "pve",
		ExecutorZones: []string{"default"},
	})
	manager.SetBuildCatalog(catalog.NewCompatibility(catalog.CompatibilityOptions{
		Provider: "pve", BuildMode: "native-gentoo", Template: "9000",
	}))
	labels, err := manager.resolveExecutorCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"phase:provision", "phase:build", "phase:verify", "phase:publish",
		"zone:default", "provider:pve", "profile:compat/amd64/default",
		"image:compat/current-image@current",
	} {
		if !containsCapability(labels, expected) {
			t.Fatalf("auto capabilities are missing %q: %v", expected, labels)
		}
	}
	pools, err := manager.resolveCapacityPools()
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 ||
		!containsCapability(labels, "capacity-pool:"+pools[0].ID) ||
		!containsCapability(pools[0].Selector, "capacity-pool:"+pools[0].ID) {
		t.Fatalf("capacity pools=%+v labels=%v", pools, labels)
	}

	manager.config.ExecutorZones = []string{"isolated"}
	if _, err := manager.resolveExecutorCapabilities(); err == nil {
		t.Fatal("provider image outside the configured zone was accepted")
	}

	manager.config.ExecutorCapabilities = []string{
		"phase:verify", "zone:isolated", "provider:pve",
	}
	override, err := manager.resolveExecutorCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	expectedOverride := []string{"phase:verify", "provider:pve", "zone:isolated"}
	if !reflect.DeepEqual(override, expectedOverride) {
		t.Fatalf("explicit capability override = %v, want %v", override, expectedOverride)
	}
}

func TestResolveExecutorCapabilitiesUsesRuntimeProvider(t *testing.T) {
	manager := NewManager(&config.ServerConfig{
		MaxWorkers:    0,
		CloudProvider: "gcp",
		ExecutorZones: []string{"default"},
	})
	runtimeSettings := config.CloudSettings{
		Provider: "pve", BuildMode: "native-gentoo",
	}
	manager.UpdateCloudSettings(&runtimeSettings)
	manager.SetBuildCatalog(catalog.NewCompatibility(catalog.CompatibilityOptions{
		Provider: "pve", BuildMode: "native-gentoo", Template: "9000",
	}))
	labels, err := manager.resolveExecutorCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	if !containsCapability(labels, "provider:pve") {
		t.Fatalf("runtime provider capability missing: %v", labels)
	}
}

func TestShadowExecutorAdvertisesResolvedCapabilities(t *testing.T) {
	scheduler := &phaseGateScheduler{claimCaps: make(chan []string, 1)}
	manager := NewManager(&config.ServerConfig{
		MaxWorkers:        1,
		CloudProvider:     "pve",
		PhaseExecutorMode: "shadow",
		ExecutorZones:     []string{"default"},
	})
	manager.SetBuildCatalog(catalog.NewCompatibility(catalog.CompatibilityOptions{
		Provider: "pve", BuildMode: "native-gentoo", Template: "9000",
	}))
	manager.SetJobLedger(scheduler)
	if err := manager.StartWorkers(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Shutdown)

	select {
	case capabilities := <-scheduler.claimCaps:
		for _, expected := range []string{
			"phase:provision", "provider:pve", "zone:default",
			"profile:compat/amd64/default",
		} {
			if !containsCapability(capabilities, expected) {
				t.Fatalf(
					"shadow claim is missing %q: %v", expected, capabilities,
				)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shadow executor did not attempt a capability-bound claim")
	}
}

func TestStartWorkersReportsCapabilityFailureAndStaysRetryable(t *testing.T) {
	scheduler := &phaseGateScheduler{claimCaps: make(chan []string, 1)}
	manager := NewManager(&config.ServerConfig{
		MaxWorkers:        1,
		CloudProvider:     "pve",
		PhaseExecutorMode: "shadow",
		// No catalog profile resolves in this zone, so capability resolution
		// fails exactly the way a mistyped EXECUTOR_ZONES does in production.
		ExecutorZones: []string{"nowhere"},
	})
	manager.SetBuildCatalog(catalog.NewCompatibility(catalog.CompatibilityOptions{
		Provider: "pve", BuildMode: "native-gentoo", Template: "9000",
	}))
	manager.SetJobLedger(scheduler)
	t.Cleanup(manager.Shutdown)

	if err := manager.StartWorkers(); err == nil {
		t.Fatal("an executor that never started reported success")
	}
	if _, err := manager.SubmitBuild(&BuildRequest{
		PackageName: "app-misc/hello", Arch: "amd64",
	}); err == nil {
		t.Fatal("a build was accepted with no executor polling for it")
	}
	if len(manager.GetJobsSnapshot()) != 0 {
		t.Fatal("a build that nobody can claim reached the projection")
	}

	// A later start must still be able to succeed once the operator fixes the
	// configuration; the failed attempt must not have consumed the start.
	manager.config.ExecutorZones = []string{"default"}
	if err := manager.StartWorkers(); err != nil {
		t.Fatalf("the failed start was not retryable: %v", err)
	}
	select {
	case <-scheduler.claimCaps:
	case <-time.After(2 * time.Second):
		t.Fatal("the retried start did not run the executor")
	}
}

func TestSeparatedRuntimeRoleResponsibilities(t *testing.T) {
	tests := []struct {
		role      string
		admission bool
		phases    bool
	}{
		{role: "control-plane", admission: true, phases: true},
		{role: "api", admission: true, phases: false},
		{role: "executor", admission: false, phases: true},
	}
	for _, test := range tests {
		t.Run(test.role, func(t *testing.T) {
			if got := runtimeRunsAdmission(test.role); got != test.admission {
				t.Fatalf("runtimeRunsAdmission(%q)=%t, want %t",
					test.role, got, test.admission)
			}
			if got := runtimeRunsPhaseExecution(test.role); got != test.phases {
				t.Fatalf("runtimeRunsPhaseExecution(%q)=%t, want %t",
					test.role, got, test.phases)
			}
		})
	}
}

func containsCapability(labels []string, expected string) bool {
	for _, label := range labels {
		if label == expected {
			return true
		}
	}
	return false
}
