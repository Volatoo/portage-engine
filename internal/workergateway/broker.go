package workergateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	maxCompletionBytes         = 4 << 20
	defaultPollWait            = 25 * time.Second
	defaultCommandLease        = 90 * time.Second
	defaultUploadLease         = 30 * time.Minute
	durableUploadCommitTimeout = 10 * time.Second
	// memoryUploadSuffix names the single generation the standalone path uses.
	// Without a database there is no fence to distinguish attempts.
	memoryUploadSuffix = ".uploading"
)

type ClaimChecker func(context.Context, Identity) error

// UploadObjectSink persists a verified durable upload in the shared object
// authority. relative is rooted at the Broker's upload root; source is opened
// from the fenced generation and is valid only for the duration of the call.
// Implementations must consume source synchronously and verify size/digest
// before returning success.
type UploadObjectSink func(relative string, source io.Reader, size int64, digest string) error

type pendingTask struct {
	result chan Completion
}

type uploadSlot struct {
	path     string
	maxBytes int64
	used     bool
}

type session struct {
	identity  Identity
	tasks     chan Task
	pending   map[string]*pendingTask
	uploads   map[string]*uploadSlot
	connected chan struct{}
	seenOnce  sync.Once
}

// Broker coordinates attempt-bound commands. PostgreSQL-backed deployments
// install a DurableStore, making every HTTP replica interchangeable; standalone
// mode retains the bounded in-memory compatibility path.
type Broker struct {
	mu           sync.Mutex
	sessions     map[string]*session
	checkClaim   ClaimChecker
	store        DurableStore
	longPollWait time.Duration
	commandLease time.Duration
	uploadLease  time.Duration
	uploadRoot   string
	uploadSink   UploadObjectSink
}

type RuntimeStatus struct {
	Authority            string `json:"authority"`
	RegisteredSessions   int    `json:"registered_sessions"`
	ConnectedSessions    int    `json:"connected_sessions"`
	PendingTasks         int    `json:"pending_tasks"`
	PendingUploads       int    `json:"pending_uploads"`
	ActiveIssuers        int    `json:"active_issuers"`
	DrainingIssuers      int    `json:"draining_issuers"`
	RevokedIssuers       int    `json:"revoked_issuers"`
	ActiveCertificates   int    `json:"active_certificates"`
	RevokedCertificates  int    `json:"revoked_certificates"`
	ExpiringCertificates int    `json:"expiring_certificates"`
}

func NewBroker(checker ClaimChecker) *Broker {
	return &Broker{
		sessions:     make(map[string]*session),
		checkClaim:   checker,
		longPollWait: defaultPollWait,
		commandLease: defaultCommandLease,
		uploadLease:  defaultUploadLease,
	}
}

// SetDurableStore switches the Broker to its multi-replica PostgreSQL
// authority. It is installed before the server begins accepting durable jobs.
func (b *Broker) SetDurableStore(store DurableStore) {
	b.mu.Lock()
	b.store = store
	b.mu.Unlock()
}

// SetUploadRoot confines every worker upload destination to a control-plane
// owned directory. Durable claims are revalidated against this boundary after
// they are read from PostgreSQL, so a corrupt row cannot become a filesystem
// write primitive. Until it is called the Broker has no spool boundary and
// refuses uploads outright: a deployment that forgets to configure one must
// lose artifact collection, not gain an unbounded write primitive.
func (b *Broker) SetUploadRoot(root string) {
	b.mu.Lock()
	clean := filepath.Clean(root)
	b.uploadRoot = clean
	uploadLease := b.uploadLease
	b.mu.Unlock()
	if strings.TrimSpace(root) != "" {
		// A process crash can leave a fenced receive generation behind. It is
		// never authoritative without its database row, and after the longest
		// upload lease no live request may still own it. WalkDir does not follow
		// planted symlink directories; removal is issued through os.Root below.
		_ = sweepExpiredUploadGenerations(clean, time.Now().Add(-uploadLease))
	}
}

// SetUploadObjectSink makes shared object storage the durable byte authority
// for PostgreSQL-backed uploads. The local upload root remains a confined,
// disposable receive buffer; a completed database row is only written after
// the sink has accepted the exact verified generation.
func (b *Broker) SetUploadObjectSink(sink UploadObjectSink) {
	b.mu.Lock()
	b.uploadSink = sink
	b.mu.Unlock()
}

func (b *Broker) Status() RuntimeStatus {
	b.mu.Lock()
	store := b.store
	if store != nil {
		b.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		status, err := store.WorkerGatewayStatus(ctx)
		if err == nil {
			return status
		}
		return RuntimeStatus{Authority: "postgresql-unavailable"}
	}
	defer b.mu.Unlock()
	status := RuntimeStatus{Authority: "memory", RegisteredSessions: len(b.sessions)}
	for _, session := range b.sessions {
		select {
		case <-session.connected:
			status.ConnectedSessions++
		default:
		}
		status.PendingTasks += len(session.pending)
		status.PendingUploads += len(session.uploads)
	}
	return status
}

func (b *Broker) Register(identity Identity) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	store := b.store
	b.mu.Unlock()
	if store != nil {
		if err := store.RegisterWorkerSession(context.Background(), identity); err != nil {
			return err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if existing, exists := b.sessions[identity.WorkerID]; exists {
		if existing.identity == identity {
			return nil
		}
		return fmt.Errorf("worker %q is already registered with another identity", identity.WorkerID)
	}
	b.sessions[identity.WorkerID] = &session{
		identity: identity, tasks: make(chan Task, 1),
		pending: make(map[string]*pendingTask), uploads: make(map[string]*uploadSlot),
		connected: make(chan struct{}),
	}
	return nil
}

// RegisterCertificate binds the freshly issued leaf to the durable attempt
// before bootstrap material is handed to the worker. Replays of the exact
// record are idempotent; a different leaf cannot replace an active session.
func (b *Broker) RegisterCertificate(
	identity Identity,
	record CertificateRecord,
) error {
	if err := record.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	_, existed := b.sessions[identity.WorkerID]
	b.mu.Unlock()
	if err := b.Register(identity); err != nil {
		return err
	}
	b.mu.Lock()
	store := b.store
	b.mu.Unlock()
	if store == nil {
		return nil
	}
	if err := store.RegisterWorkerCertificate(
		context.Background(), identity, record,
	); err != nil {
		if !existed {
			b.Unregister(identity.WorkerID)
		}
		return err
	}
	return nil
}

func (b *Broker) Unregister(workerID string) {
	b.mu.Lock()
	s := b.sessions[workerID]
	store := b.store
	uploadRoot := b.uploadRoot
	delete(b.sessions, workerID)
	if s == nil {
		b.mu.Unlock()
		return
	}
	uploads := make([]*uploadSlot, 0, len(s.uploads))
	for _, slot := range s.uploads {
		uploads = append(uploads, slot)
	}
	pendingTasks := make([]*pendingTask, 0, len(s.pending))
	for _, pending := range s.pending {
		pendingTasks = append(pendingTasks, pending)
	}
	s.uploads = make(map[string]*uploadSlot)
	s.pending = make(map[string]*pendingTask)
	b.mu.Unlock()
	if store != nil {
		_ = store.RevokeWorkerSession(
			context.Background(), s.identity, "worker session closed",
		)
	}
	for _, slot := range uploads {
		discardUploadGeneration(uploadRoot, slot.path, memoryUploadSuffix)
	}
	for _, pending := range pendingTasks {
		select {
		case pending.result <- Completion{Error: "worker session closed"}:
		default:
		}
	}
}

func (b *Broker) WaitConnected(ctx context.Context, workerID string) error {
	b.mu.Lock()
	s := b.sessions[workerID]
	store := b.store
	b.mu.Unlock()
	if s == nil {
		return fmt.Errorf("worker %q is not registered", workerID)
	}
	if store != nil {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			connected, err := store.WorkerSessionConnected(ctx, s.identity)
			if err != nil {
				return err
			}
			if connected {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.connected:
		return nil
	}
}

// Dispatch persists one stable command ID and waits for its terminal result.
// Durable delivery may be replayed with a higher fence, but the command ID is
// unchanged so executor-side idempotency can return the original BuildJob.
func (b *Broker) Dispatch(ctx context.Context, identity Identity, action string, request, response any) error {
	return b.DispatchID(ctx, identity, uuid.NewString(), action, request, response)
}

// DispatchID persists/reuses a caller-selected command ID. Phase executors
// derive it from the durable work item so replay waits for the same result
// instead of dispatching the side effect again.
func (b *Broker) DispatchID(
	ctx context.Context,
	identity Identity,
	commandID, action string,
	request, response any,
) error {
	task, err := b.prepareDispatch(ctx, identity, commandID, action, request)
	if err != nil {
		return err
	}
	b.mu.Lock()
	store := b.store
	b.mu.Unlock()
	if store != nil {
		return dispatchDurable(ctx, store, identity, task, response)
	}
	return b.dispatchMemory(ctx, identity, task, response)
}

func (b *Broker) prepareDispatch(
	ctx context.Context,
	identity Identity,
	commandID, action string,
	request any,
) (Task, error) {
	if action != ActionBuild && action != ActionVerify && action != ActionCollect {
		return Task{}, fmt.Errorf("unsupported worker action %q", action)
	}
	if _, err := uuid.Parse(commandID); err != nil {
		return Task{}, fmt.Errorf("invalid worker command id: %w", err)
	}
	if err := b.validateClaim(ctx, identity); err != nil {
		return Task{}, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return Task{}, fmt.Errorf("encode worker task: %w", err)
	}
	return Task{ID: commandID, Action: action, Payload: payload}, nil
}

func dispatchDurable(
	ctx context.Context,
	store DurableStore,
	identity Identity,
	task Task,
	response any,
) error {
	if err := store.EnqueueWorkerCommand(ctx, identity, task); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		completion, err := store.WorkerCommandResult(ctx, identity, task.ID)
		if err == nil {
			return decodeCompletion(completion, response)
		}
		if !errors.Is(err, ErrResultPending) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (b *Broker) dispatchMemory(
	ctx context.Context,
	identity Identity,
	task Task,
	response any,
) error {
	task.DeliveryFence = 1
	pending := &pendingTask{result: make(chan Completion, 1)}
	b.mu.Lock()
	s, err := b.sessionLocked(identity)
	if err == nil {
		s.pending[task.ID] = pending
	}
	b.mu.Unlock()
	if err != nil {
		return err
	}
	defer func() {
		b.mu.Lock()
		if current := b.sessions[identity.WorkerID]; current == s {
			delete(s.pending, task.ID)
		}
		b.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.tasks <- task:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case completion := <-pending.result:
		return decodeCompletion(&completion, response)
	}
}

func decodeCompletion(completion *Completion, response any) error {
	if completion.Error != "" {
		return errors.New(completion.Error)
	}
	if response == nil || len(completion.Payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(completion.Payload, response); err != nil {
		return fmt.Errorf("decode worker completion: %w", err)
	}
	return nil
}

// PrepareUpload creates a one-shot upload capability bound to identity. The
// destination is selected by trusted control-plane code, never by the worker.
func (b *Broker) PrepareUpload(identity Identity, destination string, maxBytes int64) (string, error) {
	return b.PrepareUploadID(identity, uuid.NewString(), destination, maxBytes)
}

// PrepareUploadID persists/reuses a stable upload generation selected by the
// active phase executor.
func (b *Broker) PrepareUploadID(
	identity Identity,
	uploadID, destination string,
	maxBytes int64,
) (string, error) {
	if maxBytes <= 0 {
		return "", fmt.Errorf("upload size limit must be positive")
	}
	if !canonicalUploadID(uploadID) {
		return "", fmt.Errorf("invalid upload id")
	}
	b.mu.Lock()
	store := b.store
	uploadRoot := b.uploadRoot
	_, err := b.sessionLocked(identity)
	b.mu.Unlock()
	if err != nil && store == nil {
		return "", err
	}
	confined, err := confinedUploadDestination(uploadRoot, destination)
	if err != nil {
		return "", err
	}
	if store != nil {
		if err := store.PrepareWorkerUpload(
			context.Background(), identity, uploadID, confined.relative, maxBytes,
		); err != nil {
			return "", err
		}
		return uploadID, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s, err := b.sessionLocked(identity)
	if err != nil {
		return "", err
	}
	s.uploads[uploadID] = &uploadSlot{path: confined.absolute, maxBytes: maxBytes}
	return uploadID, nil
}

// canonicalUploadID accepts only the exact spelling the control plane emits.
// uuid.Parse on its own also admits braced, URN-prefixed and dash-less forms,
// all of which match the same uuid column but reach the spool as a different
// filename, so a worker could pick the generation name for a capability the
// control plane created.
func canonicalUploadID(uploadID string) bool {
	parsed, err := uuid.Parse(uploadID)
	return err == nil && parsed.String() == uploadID
}

func (b *Broker) validateClaim(ctx context.Context, identity Identity) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	if b.checkClaim != nil {
		if err := b.checkClaim(ctx, identity); err != nil {
			return fmt.Errorf("worker claim rejected: %w", err)
		}
	}
	return nil
}

func (b *Broker) sessionLocked(identity Identity) (*session, error) {
	s := b.sessions[identity.WorkerID]
	if s == nil {
		return nil, fmt.Errorf("worker %q is not registered", identity.WorkerID)
	}
	if s.identity != identity {
		return nil, fmt.Errorf("worker identity does not match the registered attempt")
	}
	return s, nil
}

// Handler returns the worker-only HTTP surface. It must be mounted exclusively
// on an mTLS listener configured with RequireAndVerifyClientCert.
func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pull", b.handlePull)
	mux.HandleFunc("/v1/complete", b.handleComplete)
	mux.HandleFunc("/v1/uploads/", b.handleUpload)
	return mux
}

func (b *Broker) authenticated(r *http.Request) (Identity, *session, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return Identity{}, nil, fmt.Errorf("verified mTLS client certificate required")
	}
	cert := r.TLS.PeerCertificates[0]
	if len(cert.URIs) != 1 {
		return Identity{}, nil, fmt.Errorf("worker certificate requires exactly one URI SAN")
	}
	identity, err := ParseIdentityURI(cert.URIs[0])
	if err != nil {
		return Identity{}, nil, err
	}
	b.mu.Lock()
	store := b.store
	b.mu.Unlock()
	if store != nil {
		presented, err := CertificatePresentation(cert)
		if err != nil {
			return Identity{}, nil, err
		}
		if err := store.AuthorizeWorkerCertificate(
			r.Context(), identity, presented,
		); err != nil {
			return Identity{}, nil, fmt.Errorf("worker certificate rejected: %w", err)
		}
	}
	if err := b.validateClaim(r.Context(), identity); err != nil {
		return Identity{}, nil, err
	}
	b.mu.Lock()
	if store != nil {
		b.mu.Unlock()
		return identity, nil, nil
	}
	s, err := b.sessionLocked(identity)
	b.mu.Unlock()
	return identity, s, err
}

func (b *Broker) handlePull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, s, err := b.authenticated(r)
	if err != nil {
		http.Error(w, "worker identity rejected", http.StatusForbidden)
		return
	}
	b.mu.Lock()
	store := b.store
	commandLease := b.commandLease
	pollWait := b.longPollWait
	b.mu.Unlock()
	if store != nil {
		deadline := time.NewTimer(pollWait)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer deadline.Stop()
		defer ticker.Stop()
		for {
			task, claimErr := store.ClaimWorkerCommand(r.Context(), identity, commandLease)
			if claimErr == nil {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(task)
				return
			}
			if !errors.Is(claimErr, ErrNoWork) {
				http.Error(w, "worker command claim rejected", http.StatusConflict)
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-deadline.C:
				w.WriteHeader(http.StatusNoContent)
				return
			case <-ticker.C:
			}
		}
	}
	s.seenOnce.Do(func() { close(s.connected) })
	timer := time.NewTimer(pollWait)
	defer timer.Stop()
	select {
	case <-r.Context().Done():
		return
	case <-timer.C:
		w.WriteHeader(http.StatusNoContent)
	case task := <-s.tasks:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(task)
	}
}

func (b *Broker) handleComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, s, err := b.authenticated(r)
	if err != nil {
		http.Error(w, "worker identity rejected", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCompletionBytes)
	var completion Completion
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&completion); err != nil || strings.TrimSpace(completion.TaskID) == "" {
		http.Error(w, "invalid completion", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	store := b.store
	b.mu.Unlock()
	if store != nil {
		if err := store.CompleteWorkerCommand(r.Context(), identity, completion); err != nil {
			http.Error(w, "task is stale or unknown", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if completion.DeliveryFence != 1 {
		http.Error(w, "task delivery fence is stale", http.StatusConflict)
		return
	}
	b.mu.Lock()
	pending := s.pending[completion.TaskID]
	if pending != nil {
		delete(s.pending, completion.TaskID)
	}
	b.mu.Unlock()
	if pending == nil {
		http.Error(w, "task is stale or unknown", http.StatusConflict)
		return
	}
	select {
	case pending.result <- completion:
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "task already completed", http.StatusConflict)
	}
}

func (b *Broker) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, s, err := b.authenticated(r)
	if err != nil {
		http.Error(w, "worker identity rejected", http.StatusForbidden)
		return
	}
	uploadID := strings.TrimPrefix(r.URL.Path, "/v1/uploads/")
	if !canonicalUploadID(uploadID) {
		http.Error(w, "invalid upload id", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	store := b.store
	uploadLease := b.uploadLease
	b.mu.Unlock()
	if store != nil {
		b.handleDurableUploadRequest(
			w, r, store, identity, uploadID, uploadLease,
		)
		return
	}
	b.handleMemoryUpload(w, r, s, uploadID)
}

func (b *Broker) handleDurableUploadRequest(
	w http.ResponseWriter,
	r *http.Request,
	store DurableStore,
	identity Identity,
	uploadID string,
	uploadLease time.Duration,
) {
	claim, err := store.ClaimWorkerUpload(
		r.Context(), identity, uploadID, uploadLease,
	)
	if err != nil {
		http.Error(w, "upload is stale or unknown", http.StatusConflict)
		return
	}
	b.mu.Lock()
	uploadRoot := b.uploadRoot
	uploadSink := b.uploadSink
	b.mu.Unlock()
	target, err := openUploadTarget(uploadRoot, claim.Destination)
	if err != nil {
		_ = store.CancelWorkerUpload(
			r.Context(), identity, claim.ID, claim.Fence,
		)
		http.Error(w, "upload destination is outside the artifact spool", http.StatusConflict)
		return
	}
	defer target.close()
	claim.Destination = target.absolute
	if !claim.Completed {
		b.handleDurableUpload(w, r, store, identity, claim, target, uploadSink)
		return
	}
	// A completed row with an object sink means the shared object was committed
	// before the row. A different API replica is neither expected nor required
	// to have the disposable receive file used by the original request.
	if uploadSink == nil {
		if err := recoverCompletedUpload(target, claim); err != nil {
			http.Error(w, "recover committed artifact", http.StatusInternalServerError)
			return
		}
	} else {
		target.discardGenerations(claim.ID)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CollectResult{
		SHA256: claim.Digest, Size: claim.Size,
	})
}

func (b *Broker) handleMemoryUpload(
	w http.ResponseWriter,
	r *http.Request,
	s *session,
	uploadID string,
) {
	b.mu.Lock()
	slot := s.uploads[uploadID]
	if slot == nil || slot.used {
		b.mu.Unlock()
		http.Error(w, "upload is stale or unknown", http.StatusConflict)
		return
	}
	slot.used = true
	delete(s.uploads, uploadID)
	uploadRoot := b.uploadRoot
	b.mu.Unlock()

	target, err := openUploadTarget(uploadRoot, slot.path)
	if err != nil {
		http.Error(w, "upload destination is outside the artifact spool", http.StatusConflict)
		return
	}
	defer target.close()
	if r.ContentLength < 0 || r.ContentLength > slot.maxBytes {
		http.Error(w, "invalid artifact content length", http.StatusRequestEntityTooLarge)
		return
	}
	if err := target.prepareParent(); err != nil {
		http.Error(w, "prepare artifact destination", http.StatusInternalServerError)
		return
	}
	tmp := target.relative + memoryUploadSuffix
	file, err := target.create(tmp)
	if err != nil {
		http.Error(w, "create artifact destination", http.StatusInternalServerError)
		return
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(r.Body, slot.maxBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written > slot.maxBytes || written != r.ContentLength {
		_ = target.root.Remove(tmp)
		http.Error(w, "artifact upload failed", http.StatusBadRequest)
		return
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	provided := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Content-SHA256")))
	if len(provided) != sha256.Size*2 ||
		subtle.ConstantTimeCompare([]byte(provided), []byte(digest)) != 1 {
		_ = target.root.Remove(tmp)
		http.Error(w, "artifact digest mismatch", http.StatusUnprocessableEntity)
		return
	}
	if err := target.root.Rename(tmp, target.relative); err != nil {
		_ = target.root.Remove(tmp)
		http.Error(w, "commit artifact upload", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CollectResult{SHA256: digest, Size: written})
}

// confinedUploadDestination resolves an absolute trusted destination or a
// durable root-relative capability inside root. The durable form is portable
// across API and executor replicas whose local scratch roots differ. An
// unconfigured root is an error: the
// alternative — treating "no root" as "anywhere" — turns a deployment mistake
// into a filesystem write primitive for whichever worker happens to hold a
// live certificate.
func confinedUploadDestination(root, destination string) (confinedUpload, error) {
	if strings.TrimSpace(root) == "" {
		return confinedUpload{}, fmt.Errorf("worker upload root is not configured")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return confinedUpload{}, fmt.Errorf("resolve upload root: %w", err)
	}
	absoluteRoot = filepath.Clean(absoluteRoot)
	cleanDestination := filepath.Clean(destination)
	if destination == "" || cleanDestination == "." || cleanDestination == ".." ||
		cleanDestination != destination {
		return confinedUpload{}, fmt.Errorf("upload destination is not normalized")
	}
	absoluteDestination := cleanDestination
	if !filepath.IsAbs(cleanDestination) {
		if strings.HasPrefix(cleanDestination, ".."+string(filepath.Separator)) ||
			filepath.VolumeName(cleanDestination) != "" {
			return confinedUpload{}, fmt.Errorf("upload destination is outside configured root")
		}
		absoluteDestination = filepath.Join(absoluteRoot, cleanDestination)
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteDestination)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return confinedUpload{}, fmt.Errorf("upload destination is outside configured root")
	}
	return confinedUpload{
		root: absoluteRoot, absolute: absoluteDestination, relative: relative,
	}, nil
}

// confinedUpload is the lexical half of the confinement: an upload destination
// that provably spells out a location under the configured root.
type confinedUpload struct {
	root     string
	absolute string
	relative string
}

// uploadTarget is a spool destination that has been resolved once and is then
// operated on exclusively through an open handle on the root. The lexical
// prefix check above is necessary but not sufficient: it compares strings, and
// a symlink planted anywhere under the spool would satisfy it while sending the
// write somewhere else entirely. Going through *os.Root makes every component
// resolve under the root or fail, so the confinement survives a spool tree the
// build VM has managed to touch.
type uploadTarget struct {
	root     *os.Root
	absolute string
	relative string
}

func openUploadTarget(root, destination string) (*uploadTarget, error) {
	confined, err := confinedUploadDestination(root, destination)
	if err != nil {
		return nil, err
	}
	handle, err := os.OpenRoot(confined.root)
	if errors.Is(err, os.ErrNotExist) {
		// The spool root itself is operator configuration, not worker input,
		// and the code this replaced created it on the way to the destination.
		// Keep a first upload on a fresh data directory working.
		if mkdirErr := os.MkdirAll(confined.root, 0o750); mkdirErr != nil {
			return nil, fmt.Errorf("create upload root: %w", mkdirErr)
		}
		handle, err = os.OpenRoot(confined.root)
	}
	if err != nil {
		return nil, fmt.Errorf("open upload root: %w", err)
	}
	return &uploadTarget{
		root: handle, absolute: confined.absolute, relative: confined.relative,
	}, nil
}

func (t *uploadTarget) close() {
	if t != nil && t.root != nil {
		_ = t.root.Close()
	}
}

// create opens the generation file for writing. O_EXCL refuses an existing
// entry of any kind, a dangling symlink included, so the write can only ever
// land on a file this call has just created inside the root.
func (t *uploadTarget) create(name string) (*os.File, error) {
	return t.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func (t *uploadTarget) prepareParent() error {
	parent := filepath.Dir(t.relative)
	if parent == "." {
		return nil
	}
	return t.root.MkdirAll(parent, 0o750)
}

// discardGenerations removes fenced files belonging to one stable capability.
// A new fence is issued only after the prior lease is dead, and a completed
// object-backed replay needs no local generation at all.
func (t *uploadTarget) discardGenerations(uploadID string) {
	if !canonicalUploadID(uploadID) {
		return
	}
	parent := filepath.Dir(t.relative)
	directory, err := t.root.Open(parent)
	if err != nil {
		return
	}
	entries, readErr := directory.ReadDir(-1)
	_ = directory.Close()
	if readErr != nil {
		return
	}
	prefix := filepath.Base(t.relative) + ".uploading." + uploadID + "."
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		fence, err := strconv.ParseInt(strings.TrimPrefix(entry.Name(), prefix), 10, 64)
		if err != nil || fence < 1 {
			continue
		}
		_ = t.root.Remove(filepath.Join(parent, entry.Name()))
	}
}

func sweepExpiredUploadGenerations(root string, before time.Time) error {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	handle, err := os.OpenRoot(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	return fs.WalkDir(os.DirFS(absolute), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() || !uploadGenerationName(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(before) {
			return nil
		}
		_ = handle.Remove(name)
		return nil
	})
}

func uploadGenerationName(name string) bool {
	const marker = ".uploading."
	index := strings.LastIndex(name, marker)
	if index < 1 {
		return false
	}
	tail := strings.Split(name[index+len(marker):], ".")
	if len(tail) != 2 || !canonicalUploadID(tail[0]) {
		return false
	}
	fence, err := strconv.ParseInt(tail[1], 10, 64)
	return err == nil && fence > 0
}

// discardUploadGeneration removes an abandoned temporary generation. It is
// best-effort cleanup, so a destination that no longer resolves inside the root
// is simply left alone rather than chased with an unconfined os.Remove.
func discardUploadGeneration(root, destination, suffix string) {
	target, err := openUploadTarget(root, destination)
	if err != nil {
		return
	}
	defer target.close()
	_ = target.root.Remove(target.relative + suffix)
}

func (b *Broker) handleDurableUpload(
	w http.ResponseWriter,
	r *http.Request,
	store DurableStore,
	identity Identity,
	claim UploadClaim,
	target *uploadTarget,
	uploadSink UploadObjectSink,
) {
	if r.ContentLength < 0 || r.ContentLength > claim.MaxBytes {
		_ = store.CancelWorkerUpload(
			r.Context(), identity, claim.ID, claim.Fence,
		)
		http.Error(w, "invalid artifact content length", http.StatusRequestEntityTooLarge)
		return
	}
	if err := target.prepareParent(); err != nil {
		http.Error(w, "prepare artifact destination", http.StatusInternalServerError)
		return
	}
	tmp := target.relative + uploadGenerationSuffix(claim)
	target.discardGenerations(claim.ID)
	_ = target.root.Remove(tmp)
	file, err := target.create(tmp)
	if err != nil {
		http.Error(w, "create artifact destination", http.StatusInternalServerError)
		return
	}
	hash := sha256.New()
	written, copyErr := io.Copy(
		io.MultiWriter(file, hash), io.LimitReader(r.Body, claim.MaxBytes+1),
	)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written > claim.MaxBytes ||
		written != r.ContentLength {
		_ = target.root.Remove(tmp)
		_ = store.CancelWorkerUpload(
			r.Context(), identity, claim.ID, claim.Fence,
		)
		http.Error(w, "artifact upload failed", http.StatusBadRequest)
		return
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	provided := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Content-SHA256")))
	if len(provided) != sha256.Size*2 ||
		subtle.ConstantTimeCompare([]byte(provided), []byte(digest)) != 1 {
		_ = target.root.Remove(tmp)
		_ = store.CancelWorkerUpload(
			r.Context(), identity, claim.ID, claim.Fence,
		)
		http.Error(w, "artifact digest mismatch", http.StatusUnprocessableEntity)
		return
	}
	if uploadSink != nil {
		source, openErr := target.root.Open(tmp)
		if openErr != nil {
			_ = target.root.Remove(tmp)
			_ = store.CancelWorkerUpload(
				r.Context(), identity, claim.ID, claim.Fence,
			)
			http.Error(w, "open verified artifact generation", http.StatusInternalServerError)
			return
		}
		sinkErr := uploadSink(target.relative, source, written, digest)
		closeErr := source.Close()
		if sinkErr != nil || closeErr != nil {
			_ = target.root.Remove(tmp)
			_ = store.CancelWorkerUpload(
				r.Context(), identity, claim.ID, claim.Fence,
			)
			http.Error(w, "persist artifact upload", http.StatusInternalServerError)
			return
		}
	}
	commitContext := r.Context()
	commitCancel := func() {}
	if uploadSink != nil {
		// Once immutable object storage accepted the bytes, finish the small
		// PostgreSQL commit even if the worker disconnected while S3 was
		// acknowledging the request. A retry can then recover from the row.
		commitContext, commitCancel = context.WithTimeout(
			context.WithoutCancel(r.Context()), durableUploadCommitTimeout,
		)
	}
	defer commitCancel()
	if err := store.CompleteWorkerUpload(
		commitContext, identity, claim.ID, claim.Fence, digest, written,
	); err != nil {
		_ = target.root.Remove(tmp)
		http.Error(w, "upload fence is stale", http.StatusConflict)
		return
	}
	if uploadSink != nil {
		// Object storage owns the durable bytes; keeping a replica-local copy
		// would leak one scratch generation per completed build on API nodes.
		_ = target.root.Remove(tmp)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CollectResult{SHA256: digest, Size: written})
		return
	}
	if err := target.root.Rename(tmp, target.relative); err != nil {
		http.Error(w, "commit artifact upload", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CollectResult{SHA256: digest, Size: written})
}

// uploadGenerationSuffix names one fenced attempt at a destination. It is
// appended to a root-relative name, never joined as a path, so the fence and
// the canonical upload id can only extend the final component.
func uploadGenerationSuffix(claim UploadClaim) string {
	return fmt.Sprintf(".uploading.%s.%d", claim.ID, claim.Fence)
}

func recoverCompletedUpload(target *uploadTarget, claim UploadClaim) error {
	if uploadFileMatches(target, target.relative, claim) {
		return nil
	}
	tmp := target.relative + uploadGenerationSuffix(claim)
	if !uploadFileMatches(target, tmp, claim) {
		return fmt.Errorf("completed upload generation is missing or has the wrong digest")
	}
	if err := target.root.Rename(tmp, target.relative); err != nil {
		return err
	}
	return nil
}

func uploadFileMatches(target *uploadTarget, name string, claim UploadClaim) bool {
	file, err := target.root.Open(name)
	if err != nil {
		return false
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || size != claim.Size {
		return false
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	return len(claim.Digest) == sha256.Size*2 &&
		subtle.ConstantTimeCompare([]byte(actual), []byte(claim.Digest)) == 1
}
