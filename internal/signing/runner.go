package signing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/binpkg"
	artifactstorage "github.com/slchris/portage-engine/internal/storage"
)

// Runner claims digest-bound tasks and signs them outside the control plane.
type Runner struct {
	Queue      Queue
	BinpkgPath string
	Storage    artifactstorage.Storage
	ScratchDir string
	Owner      string
	Lease      time.Duration
	GPG        GPG
}

// Run processes signing tasks until the context is canceled.
func (r *Runner) Run(ctx context.Context) error {
	if r.Queue == nil || (r.BinpkgPath == "" && r.Storage == nil) ||
		r.Owner == "" || r.Lease <= 0 {
		return fmt.Errorf("signer queue, artifact source, owner, and lease are required")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		worked, err := r.RunOnce(ctx)
		if err != nil {
			return err
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce claims and processes at most one signing task.
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	task, err := r.Queue.ClaimSigning(claimCtx, r.Owner, r.Lease)
	cancel()
	if err != nil || task == nil {
		return false, err
	}
	started := time.Now()
	log.Printf("signing task claimed (task=%s job=%s attempt=%s fence=%d claim_fence=%d artifacts=%d)",
		task.ID, task.JobID, task.AttemptID, task.AttemptFence, task.ClaimFence, len(task.Artifacts))
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	heartbeatDone := make(chan error, 1)
	go r.renewLease(heartbeatCtx, stopHeartbeat, task, heartbeatDone)
	outputs, err := r.process(heartbeatCtx, task)
	stopHeartbeat()
	// A lost lease is the root cause of whatever process() reports after the
	// heartbeat aborts it, so the renewal error outranks the cancellation.
	if heartbeatErr := <-heartbeatDone; heartbeatErr != nil {
		err = heartbeatErr
	}
	resultCtx, resultCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer resultCancel()
	if err != nil {
		// OutputToken derives from the task ID alone, so a task re-claimed after
		// a lease expiry shares its output prefix with the previous claimant.
		// FailSigning is fenced on (owner, claim_fence): only a successful
		// transition proves this process still owned the claim, and therefore
		// that the prefix it is about to erase is not a successor's signed
		// generation. A lost race leaves the bytes for the current claimant.
		if failErr := r.Queue.FailSigning(resultCtx, task, err.Error()); failErr != nil {
			log.Printf("signing task failure was not recorded, leaving output to the current claimant (task=%s job=%s error=%v)",
				task.ID, task.JobID, failErr)
		} else if removeErr := r.removeOutput(task); removeErr != nil {
			log.Printf("signing output cleanup failed (task=%s job=%s error=%v)",
				task.ID, task.JobID, removeErr)
		}
		log.Printf("signing task failed (task=%s job=%s duration=%s error=%v)",
			task.ID, task.JobID, time.Since(started).Round(time.Millisecond), err)
		return true, nil
	}
	if err := r.Queue.CompleteSigning(resultCtx, task, r.GPG.KeyID, outputs); err != nil {
		return true, err
	}
	log.Printf("signing task completed (task=%s job=%s key=%s artifacts=%d duration=%s)",
		task.ID, task.JobID, r.GPG.KeyID, len(outputs), time.Since(started).Round(time.Millisecond))
	return true, nil
}

func (r *Runner) removeOutput(task *Task) error {
	token, err := OutputToken(task.ID)
	if err != nil {
		return err
	}
	if r.Storage != nil {
		return deleteQuarantineGeneration(r.Storage, token)
	}
	root, err := TokenRoot(r.BinpkgPath, token)
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}

func (r *Runner) renewLease(
	ctx context.Context,
	abort context.CancelFunc,
	task *Task,
	done chan<- error,
) {
	interval := r.Lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			renewCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := r.Queue.RenewSigning(renewCtx, task, r.Lease)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					// process() already returned and stopped the heartbeat; the
					// renewal failed on our own cancellation, not on a lost lease.
					done <- nil
					return
				}
				// Without the lease another claimant may already own this task's
				// output prefix, so stop signing now instead of racing it to the
				// capability manifest.
				abort()
				done <- fmt.Errorf("renew signing task lease: %w", err)
				return
			}
		}
	}
}

func (r *Runner) process(ctx context.Context, task *Task) ([]Artifact, error) {
	if r.Storage != nil {
		return r.processObject(ctx, task)
	}
	sourceRoot, err := TokenRoot(r.BinpkgPath, task.SourceToken)
	if err != nil {
		return nil, err
	}
	outputToken, err := OutputToken(task.ID)
	if err != nil {
		return nil, err
	}
	outputRoot, err := TokenRoot(r.BinpkgPath, outputToken)
	if err != nil {
		return nil, err
	}
	if sourceRoot == outputRoot {
		return nil, fmt.Errorf("signing source and output roots must differ")
	}
	return r.processRoots(ctx, task, sourceRoot, outputRoot, true)
}

func (r *Runner) processObject(ctx context.Context, task *Task) ([]Artifact, error) {
	outputToken, err := OutputToken(task.ID)
	if err != nil {
		return nil, err
	}
	scratch := r.ScratchDir
	if scratch == "" {
		scratch = os.TempDir()
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, err
	}
	sourceRoot, err := os.MkdirTemp(scratch, "signer-input-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(sourceRoot) }()
	outputRoot, err := os.MkdirTemp(scratch, "signer-output-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(outputRoot) }()
	for _, artifact := range task.Artifacts {
		key, keyErr := artifactstorage.QuarantineGenerationKey(
			task.SourceToken, artifact.RelativePath,
		)
		if keyErr != nil {
			return nil, keyErr
		}
		destination := filepath.Join(sourceRoot, filepath.FromSlash(artifact.RelativePath))
		if err := r.Storage.Download(key, destination); err != nil {
			return nil, fmt.Errorf("materialize signing input %q: %w", artifact.RelativePath, err)
		}
	}
	outputs, err := r.processRoots(ctx, task, sourceRoot, outputRoot, false)
	if err != nil {
		return nil, err
	}
	for _, output := range outputs {
		key, keyErr := artifactstorage.QuarantineGenerationKey(
			outputToken, output.RelativePath,
		)
		if keyErr != nil {
			return nil, keyErr
		}
		if err := r.Storage.Upload(
			filepath.Join(outputRoot, filepath.FromSlash(output.RelativePath)), key,
		); err != nil {
			return nil, fmt.Errorf("upload signed artifact %q: %w", output.RelativePath, err)
		}
	}
	packagesPath := filepath.Join(outputRoot, "Packages")
	packagesDigest, _, err := fileDigest(packagesPath)
	if err != nil {
		return nil, fmt.Errorf("digest signed Packages index: %w", err)
	}
	packagesKey, err := artifactstorage.QuarantineGenerationKey(outputToken, "Packages")
	if err != nil {
		return nil, err
	}
	if err := r.Storage.Upload(packagesPath, packagesKey); err != nil {
		return nil, fmt.Errorf("upload signed Packages index: %w", err)
	}
	manifestArtifacts := make([]artifactstorage.GenerationArtifact, len(outputs))
	for index, output := range outputs {
		manifestArtifacts[index] = artifactstorage.GenerationArtifact{
			RelativePath: output.RelativePath,
			SHA256:       output.OutputDigest,
			Size:         output.OutputSize,
		}
	}
	manifest := artifactstorage.QuarantineManifest{
		SchemaVersion:  artifactstorage.QuarantineManifestSchema,
		Token:          outputToken,
		Generation:     "signed",
		Architecture:   task.Architecture,
		PackagesSHA256: packagesDigest,
		Artifacts:      manifestArtifacts,
		ExpiresAt:      time.Now().UTC().Add(30 * time.Minute),
	}
	document, err := manifest.Marshal()
	if err != nil {
		return nil, err
	}
	capabilityKey, err := artifactstorage.QuarantineCapabilityKey(outputToken)
	if err != nil {
		return nil, err
	}
	// The capability manifest is what makes this generation consumable, so it is
	// the one write that must never happen after the lease is gone.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("signing aborted before capability activation: %w", err)
	}
	if err := artifactstorage.UploadBytes(
		r.Storage, capabilityKey, document, scratch,
	); err != nil {
		return nil, fmt.Errorf("activate signed object capability: %w", err)
	}
	return outputs, nil
}

func (r *Runner) processRoots(
	ctx context.Context,
	task *Task,
	sourceRoot, outputRoot string,
	activateLocalCapability bool,
) ([]Artifact, error) {
	if err := os.RemoveAll(outputRoot); err != nil {
		return nil, fmt.Errorf("clear prior signer output: %w", err)
	}
	if err := os.MkdirAll(outputRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create signer output root: %w", err)
	}
	cleanOutput := true
	defer func() {
		if cleanOutput {
			_ = os.RemoveAll(outputRoot)
		}
	}()

	maxOutputBytes := task.MaxOutputBytes
	if maxOutputBytes <= 0 {
		// Compatibility for tasks materialized before schema v12 and for
		// in-memory Queue implementations. PostgreSQL v12 always persists the
		// explicit attempt budget.
		maxOutputBytes = 32 << 30
	}
	outputs := make([]Artifact, 0, len(task.Artifacts))
	var outputBytes int64
	for _, input := range task.Artifacts {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("signing aborted: %w", err)
		}
		output, err := r.signArtifact(
			sourceRoot, outputRoot, input, maxOutputBytes-outputBytes,
		)
		if err != nil {
			return nil, err
		}
		outputBytes += output.OutputSize
		outputs = append(outputs, output)
	}
	if _, err := binpkg.NewStore(outputRoot).RegenerateIndex(task.Architecture); err != nil {
		return nil, fmt.Errorf("generate signed quarantine index: %w", err)
	}
	if activateLocalCapability {
		expires := time.Now().UTC().Add(30 * time.Minute)
		if err := os.WriteFile(filepath.Join(outputRoot, CapabilityMarkerName),
			[]byte(fmt.Sprintf("%d", expires.Unix())), 0o600); err != nil {
			return nil, fmt.Errorf("activate signed verification capability: %w", err)
		}
	}
	cleanOutput = false
	return outputs, nil
}

func deleteQuarantineGeneration(store artifactstorage.Storage, token string) error {
	prefix, err := artifactstorage.QuarantineGenerationPrefix(token)
	if err != nil {
		return err
	}
	keys, err := store.List(prefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			return fmt.Errorf("object store returned key outside signer output prefix")
		}
		if err := store.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) signArtifact(
	sourceRoot, outputRoot string,
	input Artifact,
	remainingBytes int64,
) (Artifact, error) {
	if err := ValidateArtifact(input, false); err != nil {
		return Artifact{}, err
	}
	source := filepath.Join(sourceRoot, filepath.FromSlash(input.RelativePath))
	if err := rejectSymlinkPath(sourceRoot, input.RelativePath); err != nil {
		return Artifact{}, fmt.Errorf("validate signing input %q: %w", input.RelativePath, err)
	}
	digest, size, err := fileDigest(source)
	if err != nil {
		return Artifact{}, fmt.Errorf("digest signing input %q: %w", input.RelativePath, err)
	}
	if digest != input.InputDigest || size != input.InputSize {
		return Artifact{}, fmt.Errorf(
			"signing input %q does not match its verified digest", input.RelativePath,
		)
	}
	if input.InputSize > remainingBytes {
		return Artifact{}, fmt.Errorf("signed output would exceed quarantine byte budget")
	}
	destination := filepath.Join(outputRoot, filepath.FromSlash(input.RelativePath))
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return Artifact{}, err
	}
	if err := copyRegularFile(source, destination); err != nil {
		return Artifact{}, err
	}
	if err := r.GPG.SignGPKG(destination); err != nil {
		return Artifact{}, err
	}
	outputDigest, outputSize, err := fileDigest(destination)
	if err != nil {
		return Artifact{}, err
	}
	if outputSize > remainingBytes {
		return Artifact{}, fmt.Errorf(
			"signed output exceeds quarantine byte budget: output=%d remaining=%d",
			outputSize, remainingBytes,
		)
	}
	return Artifact{
		RelativePath: input.RelativePath,
		InputDigest:  input.InputDigest,
		InputSize:    input.InputSize,
		OutputDigest: outputDigest,
		OutputSize:   outputSize,
	}, nil
}

func fileDigest(path string) (string, int64, error) {
	file, err := os.Open(path) // #nosec G304 -- caller supplies a validated quarantine path.
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("artifact is not a regular file")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func copyRegularFile(source, destination string) error {
	input, err := os.Open(source) // #nosec G304 -- validated quarantine source.
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("signing input is not a regular file")
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640) // #nosec G302,G304 -- signer/server share a dedicated group; destination is confined to validated quarantine storage.
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

// rejectSymlinkPath prevents a compromised or buggy builder from replacing a
// quarantine category or artifact with a symlink between verification and
// signing. Every component is checked with Lstat before the signer opens it.
func rejectSymlinkPath(root, relative string) error {
	current := filepath.Clean(root)
	rootInfo, err := os.Lstat(current)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("quarantine root is not a real directory")
	}
	for _, part := range strings.Split(filepath.FromSlash(relative), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink component is forbidden")
		}
	}
	return nil
}
