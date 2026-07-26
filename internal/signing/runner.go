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
)

// Runner claims digest-bound tasks and signs them outside the control plane.
type Runner struct {
	Queue      Queue
	BinpkgPath string
	Owner      string
	Lease      time.Duration
	GPG        GPG
}

// Run processes signing tasks until the context is canceled.
func (r *Runner) Run(ctx context.Context) error {
	if r.Queue == nil || r.BinpkgPath == "" || r.Owner == "" || r.Lease <= 0 {
		return fmt.Errorf("signer queue, binpkg path, owner, and lease are required")
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
	go r.renewLease(heartbeatCtx, task, heartbeatDone)
	outputs, err := r.process(task)
	stopHeartbeat()
	if heartbeatErr := <-heartbeatDone; err == nil && heartbeatErr != nil {
		err = heartbeatErr
	}
	resultCtx, resultCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer resultCancel()
	if err != nil {
		r.removeOutput(task)
		_ = r.Queue.FailSigning(resultCtx, task, err.Error())
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

func (r *Runner) removeOutput(task *Task) {
	token, err := OutputToken(task.ID)
	if err != nil {
		return
	}
	root, err := TokenRoot(r.BinpkgPath, token)
	if err == nil {
		_ = os.RemoveAll(root)
	}
}

func (r *Runner) renewLease(ctx context.Context, task *Task, done chan<- error) {
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
				done <- fmt.Errorf("renew signing task lease: %w", err)
				return
			}
		}
	}
}

func (r *Runner) process(task *Task) ([]Artifact, error) {
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

	outputs := make([]Artifact, 0, len(task.Artifacts))
	for _, input := range task.Artifacts {
		if err := ValidateArtifact(input, false); err != nil {
			return nil, err
		}
		source := filepath.Join(sourceRoot, filepath.FromSlash(input.RelativePath))
		if err := rejectSymlinkPath(sourceRoot, input.RelativePath); err != nil {
			return nil, fmt.Errorf("validate signing input %q: %w", input.RelativePath, err)
		}
		digest, size, err := fileDigest(source)
		if err != nil {
			return nil, fmt.Errorf("digest signing input %q: %w", input.RelativePath, err)
		}
		if digest != input.InputDigest || size != input.InputSize {
			return nil, fmt.Errorf("signing input %q does not match its verified digest", input.RelativePath)
		}
		destination := filepath.Join(outputRoot, filepath.FromSlash(input.RelativePath))
		if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
			return nil, err
		}
		if err := copyRegularFile(source, destination); err != nil {
			return nil, err
		}
		if err := r.GPG.SignGPKG(destination); err != nil {
			return nil, err
		}
		outputDigest, outputSize, err := fileDigest(destination)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, Artifact{
			RelativePath: input.RelativePath,
			InputDigest:  input.InputDigest, InputSize: input.InputSize,
			OutputDigest: outputDigest, OutputSize: outputSize,
		})
	}
	if _, err := binpkg.NewStore(outputRoot).RegenerateIndex(task.Architecture); err != nil {
		return nil, fmt.Errorf("generate signed quarantine index: %w", err)
	}
	expires := time.Now().UTC().Add(30 * time.Minute)
	if err := os.WriteFile(filepath.Join(outputRoot, CapabilityMarkerName),
		[]byte(fmt.Sprintf("%d", expires.Unix())), 0o600); err != nil {
		return nil, fmt.Errorf("activate signed verification capability: %w", err)
	}
	cleanOutput = false
	return outputs, nil
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
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
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
