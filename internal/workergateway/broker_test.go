package workergateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testIdentity(t *testing.T) Identity {
	t.Helper()
	return Identity{
		WorkerID: "worker-1", JobID: "job-1",
		AttemptID: "attempt-1", FenceToken: 7,
	}
}

func authenticatedRequest(t *testing.T, method, target string, body *bytes.Reader, identity Identity) *http.Request {
	t.Helper()
	uri, err := identity.URI()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, target, body)
	leaf := &x509.Certificate{URIs: []*url.URL{uri}}
	request.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf},
		VerifiedChains:   [][]*x509.Certificate{{leaf}},
	}
	return request
}

func TestIdentityURIRoundTrip(t *testing.T) {
	identity := testIdentity(t)
	uri, err := identity.URI()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseIdentityURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	if got != identity {
		t.Fatalf("identity mismatch: got %#v want %#v", got, identity)
	}
}

func TestBrokerDispatchRequiresExactIdentityAndCompletes(t *testing.T) {
	identity := testIdentity(t)
	broker := NewBroker(func(_ context.Context, got Identity) error {
		if got != identity {
			return fmt.Errorf("stale identity")
		}
		return nil
	})
	if err := broker.Register(identity); err != nil {
		t.Fatal(err)
	}
	type response struct {
		Value string `json:"value"`
	}
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var out response
		err := broker.Dispatch(ctx, identity, ActionVerify, map[string]string{"input": "x"}, &out)
		if err == nil && out.Value != "ok" {
			t.Errorf("unexpected response %#v", out)
		}
		result <- err
	}()

	recorder := httptest.NewRecorder()
	broker.Handler().ServeHTTP(recorder, authenticatedRequest(t, http.MethodGet, "/v1/pull", bytes.NewReader(nil), identity))
	if recorder.Code != http.StatusOK {
		t.Fatalf("pull status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var task Task
	if err := json.Unmarshal(recorder.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(response{Value: "ok"})
	completion, _ := json.Marshal(Completion{
		TaskID: task.ID, DeliveryFence: task.DeliveryFence, Payload: payload,
	})
	recorder = httptest.NewRecorder()
	broker.Handler().ServeHTTP(recorder,
		authenticatedRequest(t, http.MethodPost, "/v1/complete", bytes.NewReader(completion), identity))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("complete status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}

	stale := identity
	stale.FenceToken++
	recorder = httptest.NewRecorder()
	broker.Handler().ServeHTTP(recorder,
		authenticatedRequest(t, http.MethodGet, "/v1/pull", bytes.NewReader(nil), stale))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("stale identity status = %d", recorder.Code)
	}
}

func TestBrokerArtifactUploadIsDigestBoundAndOneShot(t *testing.T) {
	identity := testIdentity(t)
	broker := NewBroker(nil)
	if err := broker.Register(identity); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "cat", "pkg.gpkg.tar")
	uploadID, err := broker.PrepareUpload(identity, destination, 1024)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("artifact")
	digest := sha256.Sum256(data)
	request := authenticatedRequest(t, http.MethodPut, "/v1/uploads/"+uploadID, bytes.NewReader(data), identity)
	request.ContentLength = int64(len(data))
	request.Header.Set("X-Content-SHA256", hex.EncodeToString(digest[:]))
	recorder := httptest.NewRecorder()
	broker.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("upload status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("artifact bytes mismatch")
	}

	recorder = httptest.NewRecorder()
	broker.Handler().ServeHTTP(recorder, request.Clone(context.Background()))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("reused upload status = %d", recorder.Code)
	}
}

func TestBrokerArtifactUploadIsConfinedToConfiguredRoot(t *testing.T) {
	identity := testIdentity(t)
	broker := NewBroker(nil)
	if err := broker.Register(identity); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	broker.SetUploadRoot(root)

	inside := filepath.Join(root, "job", "pkg.gpkg.tar")
	if _, err := broker.PrepareUpload(identity, inside, 1024); err != nil {
		t.Fatalf("prepare confined upload: %v", err)
	}
	outside := filepath.Join(filepath.Dir(root), "outside.gpkg.tar")
	if _, err := broker.PrepareUpload(identity, outside, 1024); err == nil {
		t.Fatal("expected upload outside configured root to be rejected")
	}
	if _, err := broker.PrepareUpload(identity, root, 1024); err == nil {
		t.Fatal("expected upload root itself to be rejected")
	}
}

func TestRecoverCompletedUploadRequiresFencedGenerationDigest(t *testing.T) {
	data := []byte("artifact-generation")
	sum := sha256.Sum256(data)
	claim := UploadClaim{
		ID: uuid.NewString(), Destination: filepath.Join(t.TempDir(), "pkg.gpkg.tar"),
		Fence: 2, Digest: hex.EncodeToString(sum[:]), Size: int64(len(data)),
		Completed: true,
	}
	generation := durableUploadGeneration(claim)
	if err := os.WriteFile(generation, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverCompletedUpload(claim); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(claim.Destination); err != nil ||
		!bytes.Equal(got, data) {
		t.Fatalf("recovered destination=%q err=%v", got, err)
	}

	if err := os.WriteFile(
		claim.Destination, bytes.Repeat([]byte("x"), len(data)), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := recoverCompletedUpload(claim); err == nil {
		t.Fatal("same-size corrupt destination was accepted without its fenced generation")
	}
}

func TestBrokerRejectsNonMTLSRequest(t *testing.T) {
	broker := NewBroker(nil)
	recorder := httptest.NewRecorder()
	broker.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/pull", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d", recorder.Code)
	}
}
