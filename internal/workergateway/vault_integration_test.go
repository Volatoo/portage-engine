//go:build integration

package workergateway

import (
	"context"
	"crypto/x509"
	"os"
	"testing"
	"time"
)

func TestVaultIssuerIntegration(t *testing.T) {
	address := requiredIntegrationEnv(t, "PORTAGE_TEST_VAULT_ADDRESS")
	caPath := requiredIntegrationEnv(t, "PORTAGE_TEST_VAULT_CA")
	serverCAPath := requiredIntegrationEnv(t, "PORTAGE_TEST_VAULT_SERVER_CA")
	tokenPath := requiredIntegrationEnv(t, "PORTAGE_TEST_VAULT_TOKEN_FILE")
	issuer, err := NewVaultIssuer(VaultIssuerConfig{
		ID: "integration-vault", Address: address,
		Mount: "pki_workers", Role: "portage-worker",
		TokenPath: tokenPath, ServerCAPath: serverCAPath,
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("integration trust bundle contains no certificate")
	}
	trusted, err := NewTrustingIssuer(issuer, roots)
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{
		WorkerID: "vault-integration", JobID: "vault-integration",
		AttemptID: "vault-integration", FenceToken: 7,
	}
	issued, issueErr := trusted.Issue(context.Background(), identity, 5*time.Minute)
	if os.Getenv("PORTAGE_TEST_EXPECT_TRUST_FAILURE") == "true" {
		if issueErr == nil {
			clear(issued.KeyPEM)
			t.Fatal("unstaged Vault issuer generation was unexpectedly trusted")
		}
		if trusted.Status().Healthy {
			t.Fatal("trust rejection did not degrade issuer runtime status")
		}
		return
	}
	if issueErr != nil {
		t.Fatal(issueErr)
	}
	clear(issued.KeyPEM)
	if issued.Record.IssuerProvider != IssuerProviderVault ||
		issued.Record.IssuerID != "integration-vault" ||
		!trusted.Status().Healthy {
		t.Fatalf("unexpected successful issuer state: %+v", trusted.Status())
	}

	if os.Getenv("PORTAGE_TEST_TOKEN_RECOVERY") != "true" {
		return
	}
	validToken, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(validToken)
	if err := os.WriteFile(tokenPath, []byte("invalid-integration-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rejected, err := trusted.Issue(context.Background(), identity, 5*time.Minute); err == nil {
		clear(rejected.KeyPEM)
		t.Fatal("Vault accepted an invalid token")
	}
	degraded := trusted.Status()
	if degraded.Healthy || degraded.ConsecutiveFailures != 1 ||
		degraded.LastFailureAt == nil {
		t.Fatalf("issuer failure was not observable: %+v", degraded)
	}
	if err := os.WriteFile(tokenPath, validToken, 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := trusted.Issue(context.Background(), identity, 5*time.Minute)
	if err != nil {
		t.Fatalf("issuer did not recover after token restoration: %v", err)
	}
	clear(recovered.KeyPEM)
	status := trusted.Status()
	if !status.Healthy || status.ConsecutiveFailures != 0 ||
		status.LastSuccessAt == nil {
		t.Fatalf("issuer recovery was not observable: %+v", status)
	}
}

func requiredIntegrationEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Skipf("%s is not configured", name)
	}
	return value
}
