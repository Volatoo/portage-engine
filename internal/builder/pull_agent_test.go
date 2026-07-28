package builder

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/workergateway"
	"github.com/slchris/portage-engine/pkg/config"
)

func issuePullTestCA(t *testing.T) ([]byte, []byte, *x509.Certificate, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pull-test-ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(private)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		certificate, private
}

func issuePullTestServerCertificate(
	t *testing.T,
	ca *x509.Certificate,
	caKey ed25519.PrivateKey,
) tls.Certificate {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "worker-gateway"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, public, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(private)
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func TestPullAgentStreamsArtifactOverRealMTLS(t *testing.T) {
	caPEM, caKeyPEM, ca, caKey := issuePullTestCA(t)
	identity := workergateway.Identity{
		WorkerID: "worker-test", JobID: "job-test",
		AttemptID: "attempt-test", FenceToken: 3,
	}
	workerCertPEM, workerKeyPEM, err := workergateway.IssueWorkerCertificate(
		caPEM, caKeyPEM, identity, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	broker := workergateway.NewBroker(nil)
	if err := broker.Register(identity); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(broker.Handler())
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{issuePullTestServerCertificate(t, ca, caKey)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    roots,
	}
	server.StartTLS()
	defer server.Close()

	temp := t.TempDir()
	certPath := filepath.Join(temp, "worker.crt")
	keyPath := filepath.Join(temp, "worker.key")
	caPath := filepath.Join(temp, "ca.crt")
	if err := os.WriteFile(certPath, workerCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, workerKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(temp, "artifacts")
	relative := "app-misc/jq/jq-1.8-1.gpkg.tar"
	source := filepath.Join(artifactDir, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(source), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("signed-package-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	localJobID := "local-job"
	local := &LocalBuilder{
		artifactDir: artifactDir,
		jobs: map[string]*BuildJob{
			localJobID: {ID: localJobID, Status: "success", Artifacts: []string{relative}},
		},
	}
	cfg := &config.BuilderConfig{
		WorkerGatewayURL: server.URL,
		WorkerTLSCert:    certPath, WorkerTLSKey: keyPath, WorkerTLSCA: caPath,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agentResult := make(chan error, 1)
	go func() { agentResult <- RunPullAgent(ctx, cfg, local) }()
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), 5*time.Second)
	if err := broker.WaitConnected(connectCtx, identity.WorkerID); err != nil {
		t.Fatal(err)
	}
	cancelConnect()

	destination := filepath.Join(temp, "quarantine", filepath.FromSlash(relative))
	uploadID, err := broker.PrepareUpload(identity, destination, 1024)
	if err != nil {
		t.Fatal(err)
	}
	dispatchCtx, cancelDispatch := context.WithTimeout(context.Background(), 5*time.Second)
	var result workergateway.CollectResult
	err = broker.Dispatch(dispatchCtx, identity, workergateway.ActionCollect,
		workergateway.CollectRequest{
			LocalJobID: localJobID, Relative: relative, UploadID: uploadID,
		}, &result)
	cancelDispatch()
	if err != nil {
		t.Fatal(err)
	}
	if result.Size != int64(len("signed-package-bytes")) || result.SHA256 == "" {
		t.Fatalf("unexpected upload result: %+v", result)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "signed-package-bytes" {
		t.Fatalf("destination contains %q", got)
	}
	cancel()
	select {
	case err := <-agentResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pull agent did not stop")
	}
}
