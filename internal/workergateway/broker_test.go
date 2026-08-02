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
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// testLeaf carries DER bytes and a serial because the durable path fingerprints
// the presented leaf, even though httptest never performs a real handshake.
func testLeaf(t *testing.T, identity Identity) *x509.Certificate {
	t.Helper()
	uri, err := identity.URI()
	if err != nil {
		t.Fatal(err)
	}
	return &x509.Certificate{
		URIs:         []*url.URL{uri},
		Raw:          []byte("worker-leaf-der/" + identity.WorkerID),
		SerialNumber: big.NewInt(42),
	}
}

func authenticatedRequest(t *testing.T, method, target string, body *bytes.Reader, identity Identity) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, body)
	leaf := testLeaf(t, identity)
	request.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf},
		VerifiedChains:   [][]*x509.Certificate{{leaf}},
	}
	return request
}

func testCertificateRecord(t *testing.T, presented PresentedCertificate) CertificateRecord {
	t.Helper()
	now := time.Now().UTC()
	record := CertificateRecord{
		Fingerprint: presented.Fingerprint, Serial: presented.Serial,
		IssuerID: "test-issuer", IssuerProvider: IssuerProviderFile,
		IssuerFingerprint: strings.Repeat("b", 64), IssuerSubject: "worker-test-ca",
		IssuerSerial:    "01",
		IssuerNotBefore: now.Add(-time.Hour), IssuerNotAfter: now.Add(time.Hour),
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(30 * time.Minute),
	}
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	return record
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
	spool := t.TempDir()
	broker.SetUploadRoot(spool)
	destination := filepath.Join(spool, "cat", "pkg.gpkg.tar")
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

// durableAttemptBroker wires a Broker to the in-memory authority with one
// authorized leaf, which is what a live build VM holds while its attempt runs.
func durableAttemptBroker(t *testing.T, identity Identity) (*Broker, *memoryDurableStore) {
	t.Helper()
	store := newMemoryDurableStore()
	broker := NewBroker(nil)
	broker.SetDurableStore(store)
	presented, err := CertificatePresentation(testLeaf(t, identity))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.RegisterCertificate(
		identity, testCertificateRecord(t, presented),
	); err != nil {
		t.Fatal(err)
	}
	return broker, store
}

func putArtifact(
	t *testing.T, broker *Broker, identity Identity, uploadID string, data []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	sum := sha256.Sum256(data)
	request := authenticatedRequest(
		t, http.MethodPut, "/v1/uploads/"+uploadID, bytes.NewReader(data), identity,
	)
	request.ContentLength = int64(len(data))
	request.Header.Set("X-Content-SHA256", hex.EncodeToString(sum[:]))
	recorder := httptest.NewRecorder()
	broker.Handler().ServeHTTP(recorder, request)
	return recorder
}

// A build VM runs untrusted ebuild code and picks the artifact names in its
// completion payload; the control plane joins each one onto the job's staging
// directory. A Broker with no configured spool therefore has nothing left
// standing between "../../.." in that name and the filesystem, so it must
// refuse both the capability and any PUT that tries to redeem one.
func TestBrokerWithoutUploadRootRefusesWorkerSteeredDestination(t *testing.T) {
	identity := testIdentity(t)
	broker, store := durableAttemptBroker(t, identity)

	victim := t.TempDir()
	spool := t.TempDir()
	staging := filepath.Join(spool, "0f9c2ac1")
	// The name the compromised worker reported, joined the way the collector
	// joins it. It resolves cleanly out of the staging tree and out of the spool.
	escaped := filepath.Join(staging, "..", "..", filepath.Base(victim),
		".portage-engine-capability")
	if filepath.Dir(escaped) != victim {
		t.Fatalf("fixture does not escape into %q: %q", victim, escaped)
	}

	uploadID := uuid.NewString()
	if _, err := broker.PrepareUploadID(identity, uploadID, escaped, 1024); err == nil {
		t.Fatal("a Broker without a spool root minted a capability outside every root")
	}

	// Even if the row already exists — written by a replica that did have a
	// root, or by anything else with database access — the rootless replica
	// serving the PUT must not carry it to the filesystem.
	store.uploads[uploadID] = &UploadClaim{
		ID: uploadID, Destination: escaped, MaxBytes: 1024,
	}
	recorder := putArtifact(t, broker, identity, uploadID, []byte("payload"))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("rootless upload status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(escaped); !os.IsNotExist(err) {
		t.Fatalf("rootless upload committed outside every spool: %v", err)
	}
}

func TestBrokerWithoutUploadRootRefusesStandaloneUpload(t *testing.T) {
	identity := testIdentity(t)
	broker := NewBroker(nil)
	if err := broker.Register(identity); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "pkg.gpkg.tar")
	if _, err := broker.PrepareUpload(identity, destination, 1024); err == nil {
		t.Fatal("standalone Broker without a spool root minted an unconfined capability")
	}
	// Under the working directory, specifically. An unset root resolves to ".",
	// so every containment question a rootless Broker is asked becomes "is this
	// under the directory the control plane happens to have been started in" —
	// which a destination like this one answers yes to. Refusing an unset root
	// outright is the only reading of it that does not depend on where the
	// process was launched.
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(working, "pe-rootless-upload-probe.gpkg.tar")
	if _, err := broker.PrepareUpload(identity, inside, 1024); err == nil {
		t.Fatalf("a rootless Broker accepted %s, which is confined to nothing but the "+
			"directory this process was started in", inside)
	}
}

// plantSpoolSymlink builds a spool whose "app-misc" category is a symlink
// pointing out of it, and returns the destination the collector would compute
// for an artifact in that category. The lexical prefix check is a string
// comparison, so this destination satisfies it while resolving elsewhere.
func plantSpoolSymlink(t *testing.T, absolute bool) (spool, victim, destination string) {
	t.Helper()
	victim = t.TempDir()
	spool = t.TempDir()
	target := victim
	if !absolute {
		relative, err := filepath.Rel(spool, victim)
		if err != nil {
			t.Fatal(err)
		}
		target = relative
	}
	if err := os.Symlink(target, filepath.Join(spool, "app-misc")); err != nil {
		t.Fatal(err)
	}
	destination = filepath.Join(spool, "app-misc", "jq-1.8-1.gpkg.tar")
	if _, err := confinedUploadDestination(spool, destination); err != nil {
		t.Fatalf("fixture must satisfy the lexical check to be interesting: %v", err)
	}
	return spool, victim, destination
}

func assertSpoolSymlinkWasNotFollowed(
	t *testing.T, recorder *httptest.ResponseRecorder, victim string,
) {
	t.Helper()
	if recorder.Code == http.StatusOK {
		t.Fatalf("upload through a spool symlink succeeded: %s", recorder.Body.String())
	}
	entries, err := os.ReadDir(victim)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink target was written to: entries=%v err=%v", entries, err)
	}
}

// The spool is a directory on a shared filesystem, not a private namespace.
// A symlink planted anywhere under it satisfies the prefix check while sending
// the write somewhere else, so the write itself has to refuse to leave.
func TestBrokerDurableUploadDoesNotFollowSymlinkOutOfSpool(t *testing.T) {
	for name, absolute := range map[string]bool{
		"relative-symlink": false, "absolute-symlink": true,
	} {
		t.Run(name, func(t *testing.T) {
			identity := testIdentity(t)
			broker, _ := durableAttemptBroker(t, identity)
			spool, victim, destination := plantSpoolSymlink(t, absolute)
			broker.SetUploadRoot(spool)
			uploadID, err := broker.PrepareUploadID(
				identity, uuid.NewString(), destination, 1024,
			)
			if err != nil {
				t.Fatal(err)
			}
			assertSpoolSymlinkWasNotFollowed(
				t, putArtifact(t, broker, identity, uploadID, []byte("payload")), victim,
			)
		})
	}
}

func TestBrokerStandaloneUploadDoesNotFollowSymlinkOutOfSpool(t *testing.T) {
	for name, absolute := range map[string]bool{
		"relative-symlink": false, "absolute-symlink": true,
	} {
		t.Run(name, func(t *testing.T) {
			identity := testIdentity(t)
			broker := NewBroker(nil)
			if err := broker.Register(identity); err != nil {
				t.Fatal(err)
			}
			spool, victim, destination := plantSpoolSymlink(t, absolute)
			broker.SetUploadRoot(spool)
			uploadID, err := broker.PrepareUpload(identity, destination, 1024)
			if err != nil {
				t.Fatal(err)
			}
			assertSpoolSymlinkWasNotFollowed(
				t, putArtifact(t, broker, identity, uploadID, []byte("payload")), victim,
			)
		})
	}
}

// PostgreSQL matches a uuid column against every spelling of the same value,
// so the id in the request line has to be pinned to the one form the control
// plane emits before it can become part of a filename.
func TestBrokerUploadIDMustBeCanonicalBeforeTheStoreIsConsulted(t *testing.T) {
	identity := testIdentity(t)
	broker, store := durableAttemptBroker(t, identity)
	spool := t.TempDir()
	broker.SetUploadRoot(spool)
	uploadID, err := broker.PrepareUploadID(
		identity, uuid.NewString(), filepath.Join(spool, "pkg.gpkg.tar"), 1024,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{
		"{" + uploadID + "}",
		"urn:uuid:" + uploadID,
		strings.ToUpper(uploadID),
		strings.ReplaceAll(uploadID, "-", ""),
	} {
		recorder := putArtifact(t, broker, identity, spelling, []byte("payload"))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("upload id %q status = %d", spelling, recorder.Code)
		}
	}
	claim, err := store.ClaimWorkerUpload(
		context.Background(), identity, uploadID, time.Minute,
	)
	if err != nil || claim.Fence != 1 {
		t.Fatalf("alternate spellings consumed a generation: claim=%+v err=%v", claim, err)
	}
}

func TestRecoverCompletedUploadRequiresFencedGenerationDigest(t *testing.T) {
	data := []byte("artifact-generation")
	sum := sha256.Sum256(data)
	spool := t.TempDir()
	claim := UploadClaim{
		ID: uuid.NewString(), Destination: filepath.Join(spool, "pkg.gpkg.tar"),
		Fence: 2, Digest: hex.EncodeToString(sum[:]), Size: int64(len(data)),
		Completed: true,
	}
	target, err := openUploadTarget(spool, claim.Destination)
	if err != nil {
		t.Fatal(err)
	}
	defer target.close()
	generation := claim.Destination + uploadGenerationSuffix(claim)
	if err := os.WriteFile(generation, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverCompletedUpload(target, claim); err != nil {
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
	if err := recoverCompletedUpload(target, claim); err == nil {
		t.Fatal("same-size corrupt destination was accepted without its fenced generation")
	}
}

// completedUploadReplay is one artifact driven all the way through the durable
// upload path, so the store holds a row that is genuinely completed carrying the
// digest and size the broker itself recorded. A worker repeats its PUT whenever
// it loses the response, and that retry is served entirely from the row: the
// body is never read again, and the only thing the spool is touched with is the
// destination string recorded at prepare time. The spool is a shared directory
// the build VM also writes to, so between the commit and the retry that string
// can come to mean something else.
type completedUploadReplay struct {
	broker      *Broker
	identity    Identity
	uploadID    string
	victim      string
	destination string
	generation  string
	outside     string
	data        []byte
}

func newCompletedUploadReplay(t *testing.T) *completedUploadReplay {
	t.Helper()
	identity := testIdentity(t)
	broker, _ := durableAttemptBroker(t, identity)
	spool, victim := t.TempDir(), t.TempDir()
	broker.SetUploadRoot(spool)
	destination := filepath.Join(spool, "app-misc", "jq-1.8-1.gpkg.tar")
	uploadID, err := broker.PrepareUploadID(
		identity, uuid.NewString(), destination, 1024,
	)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("jq-1.8-1 gpkg bytes")
	if recorder := putArtifact(t, broker, identity, uploadID, data); recorder.Code != http.StatusOK {
		t.Fatalf("first upload status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	return &completedUploadReplay{
		broker: broker, identity: identity, uploadID: uploadID,
		victim: victim, destination: destination,
		generation: destination + uploadGenerationSuffix(UploadClaim{
			ID: uploadID, Fence: 1,
		}),
		outside: filepath.Join(victim, "worker-controlled.gpkg.tar"),
		data:    data,
	}
}

// rewindToFencedGeneration puts the spool back the way a replica that died
// between CompleteWorkerUpload and the rename would have left it: the row is
// committed, the bytes are still under the fenced generation name, and the
// destination name is free. Finishing that commit is what the replay is for.
func (f *completedUploadReplay) rewindToFencedGeneration(t *testing.T) {
	t.Helper()
	if err := os.Rename(f.destination, f.generation); err != nil {
		t.Fatal(err)
	}
}

// plantWorkerControlledCopy writes the committed bytes to a file outside the
// spool. It is a copy of what the worker uploaded, so it satisfies the digest
// and the size the row recorded — a worker picks the artifact bytes, so it
// knows in advance which content any check against the row will accept.
func (f *completedUploadReplay) plantWorkerControlledCopy(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.outside, f.data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *completedUploadReplay) replay(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return putArtifact(t, f.broker, f.identity, f.uploadID, f.data)
}

// The retry of a completed upload must finish the commit with the generation
// that is inside the spool, whatever the destination name has been made to
// point at in the meantime. Renaming onto the planted link replaces it, so the
// artifact the collector later reads is the one the broker hashed, and stays
// that way after the worker changes the file the link pointed to.
func TestBrokerCompletedUploadReplayCommitsInsideSpoolOverPlantedSymlink(t *testing.T) {
	fixture := newCompletedUploadReplay(t)
	fixture.rewindToFencedGeneration(t)
	fixture.plantWorkerControlledCopy(t)
	if err := os.Symlink(fixture.outside, fixture.destination); err != nil {
		t.Fatal(err)
	}

	recorder := fixture.replay(t)
	var result CollectResult
	sum := sha256.Sum256(fixture.data)
	if recorder.Code != http.StatusOK ||
		json.Unmarshal(recorder.Body.Bytes(), &result) != nil ||
		result.SHA256 != hex.EncodeToString(sum[:]) ||
		result.Size != int64(len(fixture.data)) {
		t.Fatalf("replay status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	info, err := os.Lstat(fixture.destination)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("replay left the destination pointing out of the spool: mode=%v err=%v",
			info.Mode(), err)
	}
	// Matching bytes once is not the property under test. The destination has to
	// name a file inside the spool, so swapping what the link pointed at must be
	// invisible to everything that reads the artifact afterwards.
	if err := os.WriteFile(
		fixture.outside, []byte("swapped after the broker vouched for it"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(fixture.destination)
	if err != nil || !bytes.Equal(got, fixture.data) {
		t.Fatalf("committed artifact = %q err=%v, want the bytes the broker hashed", got, err)
	}
}

// With no generation left in the spool there is nothing the replay can honestly
// recover, and the only file that answers to the destination is the copy the
// worker kept outside. Reporting that upload as committed would hand the
// collector a path whose contents the worker can still change at will, so the
// replay has to fail instead of reading through the link.
func TestBrokerCompletedUploadReplayRefusesBytesOutsideSpool(t *testing.T) {
	fixture := newCompletedUploadReplay(t)
	if err := os.Remove(fixture.destination); err != nil {
		t.Fatal(err)
	}
	fixture.plantWorkerControlledCopy(t)
	if err := os.Symlink(fixture.outside, fixture.destination); err != nil {
		t.Fatal(err)
	}

	if recorder := fixture.replay(t); recorder.Code == http.StatusOK {
		t.Fatalf("replay vouched for bytes it could only reach by leaving the spool: %s",
			recorder.Body.String())
	}
	if got, err := os.ReadFile(fixture.outside); err != nil || !bytes.Equal(got, fixture.data) {
		t.Fatalf("replay wrote through the link: %q err=%v", got, err)
	}
}

// The escape does not have to be the last component. A worker that swaps the
// whole category directory for a link, and leaves a generation with the right
// digest on the other side of it, turns the recovery rename into a write
// outside the spool — the destination still spells out a path under the root
// the entire time.
func TestBrokerCompletedUploadReplayDoesNotCommitThroughSymlinkedCategory(t *testing.T) {
	fixture := newCompletedUploadReplay(t)
	fixture.rewindToFencedGeneration(t)
	planted := filepath.Join(fixture.victim, filepath.Base(fixture.generation))
	if err := os.Rename(fixture.generation, planted); err != nil {
		t.Fatal(err)
	}
	category := filepath.Dir(fixture.destination)
	if err := os.RemoveAll(category); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fixture.victim, category); err != nil {
		t.Fatal(err)
	}

	recorder := fixture.replay(t)
	committed := filepath.Join(fixture.victim, filepath.Base(fixture.destination))
	if _, err := os.Stat(committed); !os.IsNotExist(err) {
		t.Fatalf("replay wrote the artifact outside the spool at %s: %v", committed, err)
	}
	if got, err := os.ReadFile(planted); err != nil || !bytes.Equal(got, fixture.data) {
		t.Fatalf("replay moved the planted generation: %q err=%v", got, err)
	}
	if recorder.Code == http.StatusOK {
		t.Fatalf("replay committed through a symlinked category: %s", recorder.Body.String())
	}
}

type durableCommand struct {
	identity   Identity
	task       Task
	completion *Completion
}

// memoryDurableStore stands in for the PostgreSQL authority so the multi-replica
// broker path — certificate authorization, fenced command delivery, and fenced
// uploads — is exercised without a database. It keeps the same fencing rules the
// SQL statements enforce, not their storage.
type memoryDurableStore struct {
	mu           sync.Mutex
	sessions     map[Identity]bool
	connected    map[Identity]bool
	certificates map[string]CertificateRecord
	commands     map[string]*durableCommand
	uploads      map[string]*UploadClaim
	authorizeErr error
	canceled     int
}

func newMemoryDurableStore() *memoryDurableStore {
	return &memoryDurableStore{
		sessions: make(map[Identity]bool), connected: make(map[Identity]bool),
		certificates: make(map[string]CertificateRecord),
		commands:     make(map[string]*durableCommand),
		uploads:      make(map[string]*UploadClaim),
	}
}

func (s *memoryDurableStore) rejectCertificates(err error) {
	s.mu.Lock()
	s.authorizeErr = err
	s.mu.Unlock()
}

func (s *memoryDurableStore) activeLocked(identity Identity) error {
	if !s.sessions[identity] {
		return fmt.Errorf("worker session %q is not active", identity.WorkerID)
	}
	return nil
}

func (s *memoryDurableStore) RegisterWorkerSession(_ context.Context, identity Identity) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[identity] = true
	return nil
}

func (s *memoryDurableStore) RegisterWorkerCertificate(
	_ context.Context, identity Identity, record CertificateRecord,
) error {
	if err := record.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.activeLocked(identity); err != nil {
		return err
	}
	s.certificates[record.Fingerprint] = record
	return nil
}

func (s *memoryDurableStore) AuthorizeWorkerCertificate(
	_ context.Context, identity Identity, presented PresentedCertificate,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.authorizeErr != nil {
		return s.authorizeErr
	}
	if err := s.activeLocked(identity); err != nil {
		return err
	}
	record, registered := s.certificates[presented.Fingerprint]
	if !registered || record.Serial != presented.Serial {
		return fmt.Errorf("presented certificate is not an active worker leaf")
	}
	return nil
}

func (s *memoryDurableStore) RevokeWorkerSession(_ context.Context, identity Identity, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, identity)
	delete(s.connected, identity)
	return nil
}

func (s *memoryDurableStore) TouchWorkerSession(_ context.Context, identity Identity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.activeLocked(identity); err != nil {
		return err
	}
	s.connected[identity] = true
	return nil
}

func (s *memoryDurableStore) WorkerSessionConnected(_ context.Context, identity Identity) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected[identity], nil
}

func (s *memoryDurableStore) EnqueueWorkerCommand(_ context.Context, identity Identity, task Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.activeLocked(identity); err != nil {
		return err
	}
	if existing, known := s.commands[task.ID]; known {
		if existing.task.Action != task.Action {
			return fmt.Errorf("stable command id was reused with a different request")
		}
		return nil
	}
	s.commands[task.ID] = &durableCommand{identity: identity, task: task}
	return nil
}

func (s *memoryDurableStore) ClaimWorkerCommand(
	_ context.Context, identity Identity, lease time.Duration,
) (*Task, error) {
	if lease <= 0 {
		return nil, fmt.Errorf("worker command lease must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.activeLocked(identity); err != nil {
		return nil, err
	}
	// The claim is what proves the worker is polling, exactly as the PostgreSQL
	// claim sets connected_at even when it finds no work.
	s.connected[identity] = true
	for _, command := range s.commands {
		if command.identity != identity || command.completion != nil ||
			command.task.DeliveryFence > 0 {
			continue
		}
		command.task.DeliveryFence++
		delivered := command.task
		return &delivered, nil
	}
	return nil, ErrNoWork
}

func (s *memoryDurableStore) CompleteWorkerCommand(
	_ context.Context, identity Identity, completion Completion,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.activeLocked(identity); err != nil {
		return err
	}
	command, known := s.commands[completion.TaskID]
	if !known || command.identity != identity ||
		command.task.DeliveryFence != completion.DeliveryFence {
		return ErrStaleFence
	}
	command.completion = &completion
	return nil
}

func (s *memoryDurableStore) WorkerCommandResult(
	_ context.Context, identity Identity, taskID string,
) (*Completion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	command, known := s.commands[taskID]
	if !known || command.identity != identity {
		return nil, fmt.Errorf("worker command %q is unknown", taskID)
	}
	if command.completion == nil {
		return nil, ErrResultPending
	}
	result := *command.completion
	return &result, nil
}

func (s *memoryDurableStore) PrepareWorkerUpload(
	_ context.Context, identity Identity, uploadID, destination string, maxBytes int64,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.activeLocked(identity); err != nil {
		return err
	}
	if existing, known := s.uploads[uploadID]; known {
		if existing.Destination != destination || existing.MaxBytes != maxBytes {
			return fmt.Errorf("stable upload id was reused with different terms")
		}
		return nil
	}
	s.uploads[uploadID] = &UploadClaim{
		ID: uploadID, Destination: destination, MaxBytes: maxBytes,
	}
	return nil
}

func (s *memoryDurableStore) ClaimWorkerUpload(
	_ context.Context, identity Identity, uploadID string, lease time.Duration,
) (UploadClaim, error) {
	if lease <= 0 {
		return UploadClaim{}, fmt.Errorf("worker upload lease must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.activeLocked(identity); err != nil {
		return UploadClaim{}, err
	}
	upload, known := s.uploads[uploadID]
	if !known {
		return UploadClaim{}, fmt.Errorf("worker upload %q is unknown", uploadID)
	}
	if !upload.Completed {
		upload.Fence++
	}
	return *upload, nil
}

func (s *memoryDurableStore) CompleteWorkerUpload(
	_ context.Context, identity Identity, uploadID string,
	fence int64, digest string, size int64,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.activeLocked(identity); err != nil {
		return err
	}
	upload, known := s.uploads[uploadID]
	if !known || upload.Fence != fence {
		return ErrStaleFence
	}
	upload.Completed, upload.Digest, upload.Size = true, digest, size
	return nil
}

func (s *memoryDurableStore) CancelWorkerUpload(
	_ context.Context, _ Identity, uploadID string, fence int64,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	upload, known := s.uploads[uploadID]
	if !known || upload.Fence != fence {
		return ErrStaleFence
	}
	s.canceled++
	return nil
}

func (s *memoryDurableStore) WorkerGatewayStatus(context.Context) (RuntimeStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return RuntimeStatus{
		Authority: "postgresql", RegisteredSessions: len(s.sessions),
		ConnectedSessions: len(s.connected), ActiveCertificates: len(s.certificates),
	}, nil
}

func TestBrokerDurableStoreDeliversCommandAndUpload(t *testing.T) {
	identity := testIdentity(t)
	store := newMemoryDurableStore()
	broker := NewBroker(nil)
	broker.SetDurableStore(store)
	presented, err := CertificatePresentation(testLeaf(t, identity))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.RegisterCertificate(
		identity, testCertificateRecord(t, presented),
	); err != nil {
		t.Fatal(err)
	}
	if status := broker.Status(); status.Authority != "postgresql" ||
		status.RegisteredSessions != 1 || status.ActiveCertificates != 1 {
		t.Fatalf("durable status = %+v", status)
	}

	type response struct {
		Value string `json:"value"`
	}
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var out response
		err := broker.Dispatch(ctx, identity, ActionBuild, map[string]string{"input": "x"}, &out)
		if err == nil && out.Value != "ok" {
			t.Errorf("unexpected response %#v", out)
		}
		result <- err
	}()

	recorder := httptest.NewRecorder()
	broker.Handler().ServeHTTP(recorder,
		authenticatedRequest(t, http.MethodGet, "/v1/pull", bytes.NewReader(nil), identity))
	if recorder.Code != http.StatusOK {
		t.Fatalf("durable pull status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var task Task
	if err := json.Unmarshal(recorder.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.Action != ActionBuild || task.DeliveryFence != 1 {
		t.Fatalf("durable delivery = %+v", task)
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWait()
	if err := broker.WaitConnected(waitCtx, identity.WorkerID); err != nil {
		t.Fatalf("durable poll was not observed as connected: %v", err)
	}

	payload, _ := json.Marshal(response{Value: "ok"})
	stale, _ := json.Marshal(Completion{
		TaskID: task.ID, DeliveryFence: task.DeliveryFence + 1, Payload: payload,
	})
	recorder = httptest.NewRecorder()
	broker.Handler().ServeHTTP(recorder,
		authenticatedRequest(t, http.MethodPost, "/v1/complete", bytes.NewReader(stale), identity))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("stale durable completion status = %d", recorder.Code)
	}
	completion, _ := json.Marshal(Completion{
		TaskID: task.ID, DeliveryFence: task.DeliveryFence, Payload: payload,
	})
	recorder = httptest.NewRecorder()
	broker.Handler().ServeHTTP(recorder,
		authenticatedRequest(t, http.MethodPost, "/v1/complete", bytes.NewReader(completion), identity))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("durable completion status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("durable dispatch did not observe its completion")
	}

	spool := t.TempDir()
	broker.SetUploadRoot(spool)
	destination := filepath.Join(spool, "cat", "pkg.gpkg.tar")
	uploadID, err := broker.PrepareUploadID(identity, uuid.NewString(), destination, 1024)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("durable-artifact")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	upload := func() *httptest.ResponseRecorder {
		request := authenticatedRequest(
			t, http.MethodPut, "/v1/uploads/"+uploadID, bytes.NewReader(data), identity,
		)
		request.ContentLength = int64(len(data))
		request.Header.Set("X-Content-SHA256", digest)
		recorder := httptest.NewRecorder()
		broker.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	recorder = upload()
	if recorder.Code != http.StatusOK {
		t.Fatalf("durable upload status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got, err := os.ReadFile(destination); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("committed artifact = %q err=%v", got, err)
	}

	// A retried PUT must return the recorded result instead of writing again:
	// the completed claim is recovered, not re-uploaded.
	recorder = upload()
	var replay CollectResult
	if recorder.Code != http.StatusOK ||
		json.Unmarshal(recorder.Body.Bytes(), &replay) != nil ||
		replay.SHA256 != digest || replay.Size != int64(len(data)) {
		t.Fatalf("upload replay status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	broker.Unregister(identity.WorkerID)
	recorder = httptest.NewRecorder()
	broker.Handler().ServeHTTP(recorder,
		authenticatedRequest(t, http.MethodGet, "/v1/pull", bytes.NewReader(nil), identity))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("revoked durable session status = %d", recorder.Code)
	}
}

func TestBrokerDurableUploadCancelsClaimOnDigestMismatch(t *testing.T) {
	identity := testIdentity(t)
	store := newMemoryDurableStore()
	broker := NewBroker(nil)
	broker.SetDurableStore(store)
	presented, err := CertificatePresentation(testLeaf(t, identity))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.RegisterCertificate(
		identity, testCertificateRecord(t, presented),
	); err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()
	broker.SetUploadRoot(spool)
	destination := filepath.Join(spool, "pkg.gpkg.tar")
	uploadID, err := broker.PrepareUploadID(identity, uuid.NewString(), destination, 1024)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("durable-artifact")
	wrong := sha256.Sum256([]byte("something-else"))
	request := authenticatedRequest(
		t, http.MethodPut, "/v1/uploads/"+uploadID, bytes.NewReader(data), identity,
	)
	request.ContentLength = int64(len(data))
	request.Header.Set("X-Content-SHA256", hex.EncodeToString(wrong[:]))
	recorder := httptest.NewRecorder()
	broker.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("digest mismatch status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if store.canceled != 1 {
		t.Fatalf("rejected upload did not release its fenced claim: canceled=%d", store.canceled)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("mismatched artifact was committed: %v", err)
	}
	if _, err := os.Stat(destination + uploadGenerationSuffix(UploadClaim{
		ID: uploadID, Fence: 1,
	})); !os.IsNotExist(err) {
		t.Fatalf("rejected upload generation was left behind: %v", err)
	}
}

func TestBrokerDurableStoreRejectsUnauthorizedCertificate(t *testing.T) {
	identity := testIdentity(t)
	store := newMemoryDurableStore()
	broker := NewBroker(nil)
	broker.SetDurableStore(store)
	presented, err := CertificatePresentation(testLeaf(t, identity))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.RegisterCertificate(
		identity, testCertificateRecord(t, presented),
	); err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()
	broker.SetUploadRoot(spool)
	destination := filepath.Join(spool, "pkg.gpkg.tar")
	uploadID, err := broker.PrepareUploadID(identity, uuid.NewString(), destination, 1024)
	if err != nil {
		t.Fatal(err)
	}
	store.rejectCertificates(fmt.Errorf("worker certificate is revoked"))

	data := []byte("durable-artifact")
	sum := sha256.Sum256(data)
	for _, probe := range []struct {
		method, target string
		body           []byte
	}{
		{http.MethodGet, "/v1/pull", nil},
		{http.MethodPost, "/v1/complete", []byte(`{"task_id":"t","delivery_fence":1}`)},
		{http.MethodPut, "/v1/uploads/" + uploadID, data},
	} {
		request := authenticatedRequest(
			t, probe.method, probe.target, bytes.NewReader(probe.body), identity,
		)
		request.ContentLength = int64(len(probe.body))
		request.Header.Set("X-Content-SHA256", hex.EncodeToString(sum[:]))
		recorder := httptest.NewRecorder()
		broker.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s %s status = %d body=%s",
				probe.method, probe.target, recorder.Code, recorder.Body.String())
		}
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("revoked worker committed an artifact: %v", err)
	}
	// A rejected request must not have consumed an upload generation either.
	claim, err := store.ClaimWorkerUpload(
		context.Background(), identity, uploadID, time.Minute,
	)
	if err != nil || claim.Fence != 1 {
		t.Fatalf("rejected upload advanced the fence: claim=%+v err=%v", claim, err)
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
