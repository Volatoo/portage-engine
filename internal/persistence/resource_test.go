package persistence

import (
	"testing"

	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/internal/catalog"
)

func TestResourceVectorFromResolvedRequest(t *testing.T) {
	request := &builder.BuildRequest{
		ResourceClass: "medium",
		MachineSpec: map[string]string{
			"cores": "4", "memory": "8192", "disk_size": "50",
		},
	}
	got, err := resourceVectorFromRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if got != (resourceVector{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 50}) {
		t.Fatalf("resource vector=%+v", got)
	}
}

func TestResourceVectorUsesResolvedContextAndRejectsPartialClass(t *testing.T) {
	request := &builder.BuildRequest{
		ResourceClass: "small",
		ResolvedContext: &catalog.ResolvedBuildContext{
			MachineSpec: map[string]string{
				"cores": "2", "memory": "4096", "disk_size": "40",
			},
		},
	}
	got, err := resourceVectorFromRequest(request)
	if err != nil || got.DiskGiB != 40 {
		t.Fatalf("resolved resource vector=%+v err=%v", got, err)
	}
	request.ResolvedContext.MachineSpec = map[string]string{
		"cores": "2", "memory": "4096",
	}
	if _, err := resourceVectorFromRequest(request); err == nil {
		t.Fatal("partial catalog resource was accepted")
	}
}

func TestLegacyResourceVectorIsConservativeCompatibilityClass(t *testing.T) {
	got, err := resourceVectorFromRequest(&builder.BuildRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got != (resourceVector{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 50}) {
		t.Fatalf("legacy resource vector=%+v", got)
	}
}

func TestReservationPhase(t *testing.T) {
	tests := map[string]string{
		"claimed": "claimed", "provisioning": "provision", "deploying": "provision",
		"forwarding": "build", "building": "build", "collecting": "verify",
		"verifying": "verify", "signing": "publish", "publishing": "publish",
		"queued": "",
	}
	for state, want := range tests {
		if got := reservationPhase(state); got != want {
			t.Errorf("reservationPhase(%q)=%q, want %q", state, got, want)
		}
	}
}

func TestCheckResourceRequestLimitsIsDimensionSpecific(t *testing.T) {
	request := &builder.BuildRequest{
		ResourceClass: "large",
		MachineSpec: map[string]string{
			"cores": "8", "memory": "16384", "disk_size": "100",
		},
	}
	err := checkResourceRequestLimits(request, 4, 32768, 200)
	admission, ok := builder.AsAdmissionError(err)
	if !ok || admission.Code != "vcpu_request_exceeds_limit" ||
		admission.Limit != 4 || admission.Used != 8 {
		t.Fatalf("vCPU admission=%+v err=%v", admission, err)
	}
	err = checkResourceRequestLimits(request, 16, 8192, 200)
	admission, ok = builder.AsAdmissionError(err)
	if !ok || admission.Code != "memory_request_exceeds_limit" {
		t.Fatalf("memory admission=%+v err=%v", admission, err)
	}
	err = checkResourceRequestLimits(request, 16, 32768, 50)
	admission, ok = builder.AsAdmissionError(err)
	if !ok || admission.Code != "disk_request_exceeds_limit" {
		t.Fatalf("disk admission=%+v err=%v", admission, err)
	}
}
