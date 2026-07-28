package workergateway

import (
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVaultIssuerSignsAttemptBoundCSR(t *testing.T) {
	caPEM, caKeyPEM := testCA(t)
	caBlock, _ := pem.Decode(caPEM)
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(caKeyPEM)
	signer, err := parsePrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/v1/pki/sign/portage-worker" ||
			request.Header.Get("X-Vault-Token") != "test-vault-token" ||
			request.Header.Get("X-Vault-Namespace") != "community" {
			http.Error(writer, "unexpected request", http.StatusForbidden)
			return
		}
		var input struct {
			CSR     string `json:"csr"`
			URISANs string `json:"uri_sans"`
			TTL     string `json:"ttl"`
			Common  string `json:"common_name"`
			Format  string `json:"format"`
			Exclude bool   `json:"exclude_cn_from_sans"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			http.Error(writer, "invalid JSON", http.StatusBadRequest)
			return
		}
		csrBlock, _ := pem.Decode([]byte(input.CSR))
		if csrBlock == nil {
			http.Error(writer, "invalid CSR", http.StatusBadRequest)
			return
		}
		csr, err := x509.ParseCertificateRequest(csrBlock.Bytes)
		if err != nil || csr.CheckSignature() != nil ||
			len(csr.URIs) != 1 || csr.URIs[0].String() != input.URISANs ||
			input.Common != "portage-worker" || input.Format != "pem" ||
			!input.Exclude {
			http.Error(writer, "CSR contract rejected", http.StatusBadRequest)
			return
		}
		ttl, err := time.ParseDuration(input.TTL)
		if err != nil {
			http.Error(writer, "invalid TTL", http.StatusBadRequest)
			return
		}
		now := time.Now().UTC()
		template := &x509.Certificate{
			SerialNumber: big.NewInt(42),
			Subject:      csr.Subject,
			NotBefore:    now.Add(-time.Minute),
			NotAfter:     now.Add(ttl),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			URIs:         csr.URIs,
		}
		der, err := x509.CreateCertificate(
			rand.Reader, template, ca, csr.PublicKey, signer,
		)
		if err != nil {
			http.Error(writer, "sign failure", http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"data": map[string]any{
				"certificate": string(pem.EncodeToMemory(
					&pem.Block{Type: "CERTIFICATE", Bytes: der},
				)),
				"issuing_ca": string(caPEM),
				"ca_chain":   []string{string(caPEM)},
			},
		})
	}))
	defer server.Close()

	root := t.TempDir()
	tokenPath := filepath.Join(root, "vault-token")
	if err := os.WriteFile(tokenPath, []byte("test-vault-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	serverCAPath := filepath.Join(root, "vault-server-ca.pem")
	if err := os.WriteFile(serverCAPath, pem.EncodeToMemory(
		&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw},
	), 0o644); err != nil {
		t.Fatal(err)
	}
	issuer, err := NewVaultIssuer(VaultIssuerConfig{
		ID: "community-vault", Address: server.URL,
		Mount: "pki", Role: "portage-worker",
		TokenPath: tokenPath, Namespace: "community",
		ServerCAPath: serverCAPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	trusted, err := NewTrustingIssuer(issuer, roots)
	if err != nil {
		t.Fatal(err)
	}
	identity := testIdentity(t)
	issued, err := trusted.Issue(
		t.Context(), identity, 15*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Record.IssuerProvider != IssuerProviderVault ||
		issued.Record.IssuerID != "community-vault" ||
		len(issued.KeyPEM) == 0 {
		t.Fatalf("unexpected Vault issuance: %+v", issued.Record)
	}
	certificates, err := parseCertificatePEMChain(issued.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, _ := pem.Decode(issued.KeyPEM)
	key, err := x509.ParsePKCS8PrivateKey(keyPEM.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	signerKey, ok := key.(crypto.Signer)
	if !ok || !publicKeysEqual(certificates[0].PublicKey, signerKey.Public()) {
		t.Fatal("Vault-issued leaf does not match returned worker private key")
	}
}

func TestVaultIssuerRejectsUnsafeTokenPermissions(t *testing.T) {
	root := t.TempDir()
	tokenPath := filepath.Join(root, "vault-token")
	if err := os.WriteFile(tokenPath, []byte("token"), 0o640); err != nil {
		t.Fatal(err)
	}
	issuer, err := NewVaultIssuer(VaultIssuerConfig{
		ID: "community-vault", Address: "https://vault.example.test",
		Mount: "pki", Role: "portage-worker", TokenPath: tokenPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Issue(
		t.Context(), testIdentity(t), 15*time.Minute,
	); err == nil {
		t.Fatal("group-readable Vault token was accepted")
	}
}

func TestTrustingIssuerRejectsUnstagedGeneration(t *testing.T) {
	root := t.TempDir()
	certPath := filepath.Join(root, "issuer.crt")
	keyPath := filepath.Join(root, "issuer.key")
	firstCert, firstKey := testCA(t)
	if err := os.WriteFile(certPath, firstCert, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, firstKey, 0o600); err != nil {
		t.Fatal(err)
	}
	firstBlock, _ := pem.Decode(firstCert)
	firstCA, err := x509.ParseCertificate(firstBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(firstCA)
	trusted, err := NewTrustingIssuer(
		NewFileIssuer("rotation-test", certPath, keyPath), roots,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trusted.Issue(
		t.Context(), testIdentity(t), 15*time.Minute,
	); err != nil {
		t.Fatal(err)
	}
	secondCert, secondKey := testCA(t)
	if err := os.WriteFile(certPath, secondCert, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, secondKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := trusted.Issue(
		t.Context(), testIdentity(t), 15*time.Minute,
	); err == nil {
		t.Fatal("unstaged issuer generation escaped listener trust roots")
	}
}
