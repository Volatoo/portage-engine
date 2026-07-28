package capacity

import (
	"testing"

	"github.com/slchris/portage-engine/internal/builder"
	"github.com/slchris/portage-engine/internal/catalog"
	"github.com/slchris/portage-engine/internal/iac"
)

func validPVEExecutorTemplate() PVEExecutorTemplate {
	return PVEExecutorTemplate{
		ImageID: "pe/amd64/base", ImageGeneration: "g1",
		Template:          "pe-persistent-executor-g1",
		BootstrapContract: PVEExecutorBootstrapDMIV1,
		EgressPolicy: catalog.EgressPolicy{
			ID: "egress/executor", Mode: catalog.EgressModeEnforce,
			Channel: "candidate", DNSResolvers: []string{"192.0.2.53"},
			Rules: []catalog.EgressRule{{
				ID: "control", Hosts: []string{"control.example.internal"},
				CIDRs: []string{"192.0.2.10/32"}, Protocol: "tcp",
				Ports: []int{443, 25432},
			}},
		},
	}
}

func TestNewPVEProviderRequiresPersistentExecutorContract(t *testing.T) {
	template := validPVEExecutorTemplate()
	template.BootstrapContract = "job-builder-v1"
	_, err := NewPVEProvider(PVEProviderConfig{
		Endpoint:     "https://pve.example.internal:8006",
		WorkspaceDir: t.TempDir(),
		Credentials: &iac.CloudCredentials{
			PVETokenID: "user@pve!capacity", PVETokenSecret: "test-secret",
		},
		Templates: []PVEExecutorTemplate{template},
	})
	if err == nil {
		t.Fatal("disposable job-builder template was accepted for capacity")
	}
}

func TestNewPVEProviderRejectsTemplateAuthorityOverrides(t *testing.T) {
	template := validPVEExecutorTemplate()
	template.Spec = map[string]string{"resource_name": "other-vm"}
	_, err := NewPVEProvider(PVEProviderConfig{
		Endpoint:     "https://pve.example.internal:8006",
		WorkspaceDir: t.TempDir(),
		Credentials: &iac.CloudCredentials{
			PVETokenID: "user@pve!capacity", PVETokenSecret: "test-secret",
		},
		Templates: []PVEExecutorTemplate{template},
	})
	if err == nil {
		t.Fatal("template overrode database-owned provider identity")
	}
}

func TestValidatePVEInstanceIdentityIsExact(t *testing.T) {
	claim := testClaim("scale-up")
	instance := &builder.CapacityInstance{
		ID:     "123e4567-e89b-12d3-a456-426614174000",
		PoolID: claim.Pool.ID, Provider: "pve",
		ProviderInstanceID: "portage-capacity-123e4567-e89b-12d3-a456-426614174000",
	}
	if err := validatePVEInstanceIdentity(claim, instance); err != nil {
		t.Fatalf("exact identity rejected: %v", err)
	}
	instance.ProviderInstanceID = "unrelated-vm"
	if err := validatePVEInstanceIdentity(claim, instance); err == nil {
		t.Fatal("unrelated PVE identity accepted")
	}
}
