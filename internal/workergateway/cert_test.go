package workergateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testCA(t *testing.T) ([]byte, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "worker-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func TestIssueWorkerCertificateBindsAttemptIdentity(t *testing.T) {
	caCert, caKey := testCA(t)
	identity := testIdentity(t)
	certPEM, keyPEM, err := IssueWorkerCertificate(caCert, caKey, identity, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(keyPEM) == 0 {
		t.Fatal("worker key is empty")
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("worker certificate PEM is invalid")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.URIs) != 1 {
		t.Fatalf("URI SAN count = %d", len(cert.URIs))
	}
	got, err := ParseIdentityURI(cert.URIs[0])
	if err != nil {
		t.Fatal(err)
	}
	if got != identity {
		t.Fatalf("identity mismatch: got %#v want %#v", got, identity)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("unexpected EKU: %#v", cert.ExtKeyUsage)
	}
}

func TestIssueWorkerCertificateRejectsLongTTL(t *testing.T) {
	caCert, caKey := testCA(t)
	if _, _, err := IssueWorkerCertificate(caCert, caKey, testIdentity(t), 25*time.Hour); err == nil {
		t.Fatal("long-lived worker certificate was accepted")
	}
}

func TestFileIssuerReloadsRotatedGenerationAndProtectsKey(t *testing.T) {
	root := t.TempDir()
	certPath := filepath.Join(root, "issuer.crt")
	keyPath := filepath.Join(root, "issuer.key")
	writeIssuer := func() {
		t.Helper()
		cert, key := testCA(t)
		if err := os.WriteFile(certPath, cert, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, key, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeIssuer()
	issuer := NewFileIssuer("test-file-issuer", certPath, keyPath)
	first, err := issuer.Issue(
		context.Background(), testIdentity(t), 15*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(first.CertPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	presented, err := CertificatePresentation(leaf)
	if err != nil || presented.Fingerprint != first.Record.Fingerprint ||
		presented.Serial != first.Record.Serial {
		t.Fatalf("presentation=%+v record=%+v err=%v", presented, first.Record, err)
	}

	writeIssuer()
	second, err := issuer.Issue(
		context.Background(), testIdentity(t), 15*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.Record.IssuerID != first.Record.IssuerID ||
		second.Record.IssuerFingerprint == first.Record.IssuerFingerprint {
		t.Fatalf("file rotation was not observed: first=%+v second=%+v",
			first.Record, second.Record)
	}
	if err := os.Chmod(keyPath, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Issue(
		context.Background(), testIdentity(t), 15*time.Minute,
	); err == nil {
		t.Fatal("group-readable issuer key was accepted")
	}
}
