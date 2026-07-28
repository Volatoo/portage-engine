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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	maxCompletionBytes  = 4 << 20
	defaultPollWait     = 25 * time.Second
	defaultCommandLease = 90 * time.Second
	defaultUploadLease  = 30 * time.Minute
)

type ClaimChecker func(context.Context, Identity) error

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
// write primitive.
func (b *Broker) SetUploadRoot(root string) {
	b.mu.Lock()
	b.uploadRoot = filepath.Clean(root)
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
		_ = os.Remove(slot.path + ".uploading")
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
	if _, err := uuid.Parse(uploadID); err != nil {
		return "", fmt.Errorf("invalid upload id: %w", err)
	}
	b.mu.Lock()
	store := b.store
	uploadRoot := b.uploadRoot
	_, err := b.sessionLocked(identity)
	b.mu.Unlock()
	if err != nil && store == nil {
		return "", err
	}
	destination, err = confinedUploadDestination(uploadRoot, destination)
	if err != nil {
		return "", err
	}
	if store != nil {
		if err := store.PrepareWorkerUpload(
			context.Background(), identity, uploadID, destination, maxBytes,
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
	s.uploads[uploadID] = &uploadSlot{path: destination, maxBytes: maxBytes}
	return uploadID, nil
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
	if uploadID == "" || strings.Contains(uploadID, "/") {
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
	b.mu.Unlock()
	claim.Destination, err = confinedUploadDestination(uploadRoot, claim.Destination)
	if err != nil {
		_ = store.CancelWorkerUpload(
			r.Context(), identity, claim.ID, claim.Fence,
		)
		http.Error(w, "upload destination is outside the artifact spool", http.StatusConflict)
		return
	}
	if !claim.Completed {
		b.handleDurableUpload(w, r, store, identity, claim)
		return
	}
	if err := recoverCompletedUpload(claim); err != nil {
		http.Error(w, "recover committed artifact", http.StatusInternalServerError)
		return
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

	destination, err := confinedUploadDestination(uploadRoot, slot.path)
	if err != nil {
		http.Error(w, "upload destination is outside the artifact spool", http.StatusConflict)
		return
	}
	if r.ContentLength < 0 || r.ContentLength > slot.maxBytes {
		http.Error(w, "invalid artifact content length", http.StatusRequestEntityTooLarge)
		return
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil { // #nosec G703 -- destination passed the configured spool-root check.
		http.Error(w, "prepare artifact destination", http.StatusInternalServerError)
		return
	}
	tmp := destination + ".uploading"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304,G703 -- destination passed the configured spool-root check.
	if err != nil {
		http.Error(w, "create artifact destination", http.StatusInternalServerError)
		return
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(r.Body, slot.maxBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written > slot.maxBytes || written != r.ContentLength {
		_ = os.Remove(tmp) // #nosec G703 -- tmp is derived from the confined destination.
		http.Error(w, "artifact upload failed", http.StatusBadRequest)
		return
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	provided := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Content-SHA256")))
	if len(provided) != sha256.Size*2 ||
		subtle.ConstantTimeCompare([]byte(provided), []byte(digest)) != 1 {
		_ = os.Remove(tmp) // #nosec G703 -- tmp is derived from the confined destination.
		http.Error(w, "artifact digest mismatch", http.StatusUnprocessableEntity)
		return
	}
	if err := os.Rename(tmp, destination); err != nil { // #nosec G703 -- both paths are confined to the configured spool root.
		_ = os.Remove(tmp) // #nosec G703 -- tmp is derived from the confined destination.
		http.Error(w, "commit artifact upload", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CollectResult{SHA256: digest, Size: written})
}

func confinedUploadDestination(root, destination string) (string, error) {
	absoluteDestination, err := filepath.Abs(destination)
	if err != nil {
		return "", fmt.Errorf("resolve upload destination: %w", err)
	}
	absoluteDestination = filepath.Clean(absoluteDestination)
	if strings.TrimSpace(root) == "" {
		return absoluteDestination, nil
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve upload root: %w", err)
	}
	absoluteRoot = filepath.Clean(absoluteRoot)
	relative, err := filepath.Rel(absoluteRoot, absoluteDestination)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("upload destination is outside configured root")
	}
	return absoluteDestination, nil
}

func (b *Broker) handleDurableUpload(
	w http.ResponseWriter,
	r *http.Request,
	store DurableStore,
	identity Identity,
	claim UploadClaim,
) {
	if r.ContentLength < 0 || r.ContentLength > claim.MaxBytes {
		_ = store.CancelWorkerUpload(
			r.Context(), identity, claim.ID, claim.Fence,
		)
		http.Error(w, "invalid artifact content length", http.StatusRequestEntityTooLarge)
		return
	}
	if err := os.MkdirAll(filepath.Dir(claim.Destination), 0o750); err != nil {
		http.Error(w, "prepare artifact destination", http.StatusInternalServerError)
		return
	}
	tmp := durableUploadGeneration(claim)
	_ = os.Remove(tmp)
	file, err := os.OpenFile(
		tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600,
	) // #nosec G304 -- destination is selected by trusted control-plane code.
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
		_ = os.Remove(tmp)
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
		_ = os.Remove(tmp)
		_ = store.CancelWorkerUpload(
			r.Context(), identity, claim.ID, claim.Fence,
		)
		http.Error(w, "artifact digest mismatch", http.StatusUnprocessableEntity)
		return
	}
	if err := store.CompleteWorkerUpload(
		r.Context(), identity, claim.ID, claim.Fence, digest, written,
	); err != nil {
		_ = os.Remove(tmp)
		http.Error(w, "upload fence is stale", http.StatusConflict)
		return
	}
	if err := os.Rename(tmp, claim.Destination); err != nil {
		http.Error(w, "commit artifact upload", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CollectResult{SHA256: digest, Size: written})
}

func durableUploadGeneration(claim UploadClaim) string {
	return fmt.Sprintf("%s.uploading.%s.%d", claim.Destination, claim.ID, claim.Fence)
}

func recoverCompletedUpload(claim UploadClaim) error {
	if uploadFileMatches(claim.Destination, claim) {
		return nil
	}
	tmp := durableUploadGeneration(claim)
	if !uploadFileMatches(tmp, claim) {
		return fmt.Errorf("completed upload generation is missing or has the wrong digest")
	}
	if err := os.Rename(tmp, claim.Destination); err != nil {
		return err
	}
	return nil
}

func uploadFileMatches(path string, claim UploadClaim) bool {
	file, err := os.Open(path) // #nosec G304 -- path is a server-created upload capability.
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
