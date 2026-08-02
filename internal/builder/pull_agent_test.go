package builder

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
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

func TestPullAgentTransferClientHasNoWholeRequestDeadline(t *testing.T) {
	_, clients := pullTestClients(t)
	if clients.control.Timeout != 40*time.Second {
		t.Fatalf("control client timeout = %s", clients.control.Timeout)
	}
	// Client.Timeout also covers the request body write, so a multi-hundred-MB
	// artifact PUT must not carry one.
	if clients.transfer.Timeout != 0 {
		t.Fatalf("transfer client carries a whole-request deadline of %s", clients.transfer.Timeout)
	}
	transport, ok := clients.transfer.Transport.(*http.Transport)
	if !ok || transport.ResponseHeaderTimeout == 0 || transport.IdleConnTimeout == 0 {
		t.Fatalf("transfer transport is unbounded: %#v", clients.transfer.Transport)
	}
}

// TestPullAgentTransportsBoundConnectionEstablishment covers what
// ResponseHeaderTimeout cannot: it does not start until the request body has
// finished writing, and the HTTP/2 transport ignores it entirely, so a
// blackholed dial or a stalled handshake would otherwise be bounded by nothing
// but the agent's process-lifetime context.
func TestPullAgentTransportsBoundConnectionEstablishment(t *testing.T) {
	_, clients := pullTestClients(t)
	for name, client := range map[string]*http.Client{
		"control": clients.control, "transfer": clients.transfer,
	} {
		transport, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s client does not use a *http.Transport", name)
		}
		if transport.DialContext == nil {
			t.Fatalf("%s transport has no bounded dialer; a blackholed dial parks client.Do forever", name)
		}
		if transport.TLSHandshakeTimeout != pullTLSHandshakeTimeout {
			t.Fatalf("%s transport TLS handshake timeout = %s, want %s",
				name, transport.TLSHandshakeTimeout, pullTLSHandshakeTimeout)
		}
		// A dialer that answers is the only proof the field is wired to a real
		// one rather than to a func that ignores its bounds.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		connection, err := transport.DialContext(
			context.Background(), "tcp", listener.Addr().String(),
		)
		if err != nil {
			t.Fatalf("%s transport dialer refused a live listener: %v", name, err)
		}
		_ = connection.Close()
		_ = listener.Close()
	}
	if pullDialTimeout <= 0 {
		t.Fatal("the shared pull dialer has no connect timeout")
	}
}

// TestPullAgentTaskContextBoundsOnlyTheLeasedUpload pins the per-task deadline
// to the one task that streams over the network. RunPullAgent's context is
// cancelled at process shutdown only, so without this an artifact PUT has no
// deadline at all — while a build legitimately runs for hours and must not
// acquire one.
func TestPullAgentTaskContextBoundsOnlyTheLeasedUpload(t *testing.T) {
	collect, cancelCollect := pullTaskContext(context.Background(), workergateway.ActionCollect)
	defer cancelCollect()
	deadline, ok := collect.Deadline()
	if !ok {
		t.Fatal("a collect task inherits no upload deadline")
	}
	if remaining := time.Until(deadline); remaining > pullUploadLease ||
		remaining < pullUploadLease-time.Minute {
		t.Fatalf("collect deadline is %s away, want the %s upload lease", remaining, pullUploadLease)
	}
	for _, action := range []string{workergateway.ActionBuild, workergateway.ActionVerify} {
		ctx, cancel := pullTaskContext(context.Background(), action)
		if _, bounded := ctx.Deadline(); bounded {
			cancel()
			t.Fatalf("%s task carries a wall-clock deadline; long compiles would be killed", action)
		}
		cancel()
	}
}

func pullTestClients(t *testing.T) (string, *pullClients) {
	t.Helper()
	caPEM, caKeyPEM, _, _ := issuePullTestCA(t)
	certPEM, keyPEM, err := workergateway.IssueWorkerCertificate(
		caPEM, caKeyPEM, workergateway.Identity{
			WorkerID: "worker-test", JobID: "job-test",
			AttemptID: "attempt-test", FenceToken: 1,
		}, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	for name, document := range map[string][]byte{
		"worker.crt": certPEM, "worker.key": keyPEM, "ca.crt": caPEM,
	} {
		if err := os.WriteFile(filepath.Join(temp, name), document, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	baseURL, clients, err := pullHTTPClients(&config.BuilderConfig{
		WorkerGatewayURL: "https://gateway.example:8443",
		WorkerTLSCert:    filepath.Join(temp, "worker.crt"),
		WorkerTLSKey:     filepath.Join(temp, "worker.key"),
		WorkerTLSCA:      filepath.Join(temp, "ca.crt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return baseURL, clients
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

	// The gateway refuses uploads until it has a spool boundary, exactly as the
	// manager configures one from BINPKG_PATH before any quarantine exists.
	spool := filepath.Join(temp, "quarantine")
	broker.SetUploadRoot(spool)
	destination := filepath.Join(spool, filepath.FromSlash(relative))
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

// TestOpenArtifactConfinesDeliveryToTheArtifactDirectory drives the attack that
// the recorded-artifact allowlist cannot see. The list is built by walking a
// directory the build's own ebuild code writes into as root and is reloaded
// verbatim from jobs.json, so it can name a path that leaves the tree either
// lexically or through a symlink the build planted. Both ways an artifact
// leaves a worker go through this call.
func TestOpenArtifactConfinesDeliveryToTheArtifactDirectory(t *testing.T) {
	root := t.TempDir()
	artifactDir := filepath.Join(root, "artifacts")
	if err := os.MkdirAll(filepath.Join(artifactDir, "app-misc"), 0o750); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "signing-key.asc")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY MATERIAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	genuine := filepath.Join(artifactDir, "app-misc", "jq-1.8.2-1.gpkg.tar")
	if err := os.WriteFile(genuine, []byte("package bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(artifactDir, "app-misc", "leak-1.gpkg.tar")
	if err := os.Symlink(secret, planted); err != nil {
		t.Fatal(err)
	}
	local := &LocalBuilder{artifactDir: artifactDir}

	for name, path := range map[string]string{
		"symlink out of the tree": planted,
		"lexical traversal":       filepath.Join(artifactDir, "..", "signing-key.asc"),
		"absolute path":           secret,
		"the artifact directory":  artifactDir,
	} {
		t.Run(name, func(t *testing.T) {
			file, _, err := local.OpenArtifact(path)
			if err == nil {
				contents, _ := io.ReadAll(file)
				_ = file.Close()
				t.Fatalf("OpenArtifact(%q) succeeded and returned %q", path, contents)
			}
		})
	}

	file, info, err := local.OpenArtifact(genuine)
	if err != nil {
		t.Fatalf("OpenArtifact rejected a genuine artifact: %v", err)
	}
	defer func() { _ = file.Close() }()
	if info.Size() != int64(len("package bytes")) {
		t.Fatalf("genuine artifact size = %d", info.Size())
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "package bytes" {
		t.Fatalf("genuine artifact contents = %q", contents)
	}
}
