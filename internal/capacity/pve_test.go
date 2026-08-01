package capacity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func TestNewPVEProviderRejectsPartialCredentials(t *testing.T) {
	_, err := NewPVEProvider(PVEProviderConfig{
		Endpoint:     "https://pve.example.internal:8006",
		WorkspaceDir: t.TempDir(),
		Credentials: &iac.CloudCredentials{
			PVETokenID: "user@pve!capacity",
		},
		Templates: []PVEExecutorTemplate{validPVEExecutorTemplate()},
	})
	if err == nil {
		t.Fatal("partial PVE credential pair was accepted")
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

func TestPVEProvisionSpecBindsDatabaseIdentityAndStartsStopped(t *testing.T) {
	template := validPVEExecutorTemplate()
	provider, err := NewPVEProvider(PVEProviderConfig{
		Endpoint:     "https://pve.example.internal:8006",
		Node:         "pve01",
		Storage:      "ceph-vm",
		Network:      "vmbr0",
		WorkspaceDir: t.TempDir(),
		Credentials: &iac.CloudCredentials{
			PVETokenID: "user@pve!capacity", PVETokenSecret: "test-secret",
		},
		Templates: []PVEExecutorTemplate{template},
	})
	if err != nil {
		t.Fatal(err)
	}
	instance := &builder.CapacityInstance{
		ID:                 "123e4567-e89b-12d3-a456-426614174000",
		ProviderInstanceID: "portage-capacity-123e4567-e89b-12d3-a456-426614174000",
	}
	spec := provider.provisionSpec(template, instance)
	for key, want := range map[string]string{
		"resource_name": instance.ProviderInstanceID,
		"smbios_uuid":   instance.ID,
		"template":      template.Template,
		"start_stopped": "true",
		"tags":          "portage-capacity,persistent-executor",
	} {
		if spec[key] != want {
			t.Errorf("spec[%q]=%q, want %q", key, spec[key], want)
		}
	}
}

func TestPVEDeleteMissingWorkspaceRequiresProviderAbsenceReadback(t *testing.T) {
	var readback bool
	var authorization string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/cluster/resources" ||
			r.URL.Query().Get("type") != "vm" {
			http.NotFound(w, r)
			return
		}
		authorization = r.Header.Get("Authorization")
		readback = true
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer server.Close()

	provider, err := NewPVEProvider(PVEProviderConfig{
		Endpoint: server.URL, Insecure: true, WorkspaceDir: t.TempDir(),
		Credentials: &iac.CloudCredentials{
			PVETokenID: "user@pve!capacity", PVETokenSecret: "test-secret",
		},
		Templates: []PVEExecutorTemplate{validPVEExecutorTemplate()},
	})
	if err != nil {
		t.Fatal(err)
	}
	instanceID := "123e4567-e89b-12d3-a456-426614174000"
	instance := &builder.CapacityInstance{
		ID:                 instanceID,
		Provider:           "pve",
		ProviderInstanceID: "portage-capacity-" + instanceID,
		RemoteStateRef:     filepath.Join(t.TempDir(), "removed-workspace"),
		Attributes:         map[string]string{"node": "pve01"},
	}
	if err := provider.Delete(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	if !readback {
		t.Fatal("missing Terraform state was acknowledged without PVE absence readback")
	}
	if authorization != "PVEAPIToken=user@pve!capacity=test-secret" {
		t.Fatalf("unexpected PVE authorization %q", authorization)
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
