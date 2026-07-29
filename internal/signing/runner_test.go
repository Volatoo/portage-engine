package signing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	artifactstorage "github.com/slchris/portage-engine/internal/storage"
)

type testQueue struct {
	task      *Task
	completed []Artifact
	failed    string
}

func (q *testQueue) ClaimSigning(context.Context, string, time.Duration) (*Task, error) {
	task := q.task
	q.task = nil
	return task, nil
}

func (q *testQueue) CompleteSigning(_ context.Context, _ *Task, _ string, artifacts []Artifact) error {
	q.completed = artifacts
	return nil
}

func (q *testQueue) RenewSigning(context.Context, *Task, time.Duration) error {
	return nil
}

func (q *testQueue) FailSigning(_ context.Context, _ *Task, detail string) error {
	q.failed = detail
	return nil
}

func TestRunnerSignsDigestBoundGeneration(t *testing.T) {
	gpg := testGPG(t)
	root := t.TempDir()
	binpkgPath := filepath.Join(root, "binpkgs")
	sourceToken := strings.Repeat("a", 32)
	sourceRoot, err := TokenRoot(binpkgPath, sourceToken)
	if err != nil {
		t.Fatal(err)
	}
	relative := "app-misc/hello-2.12.2.gpkg.tar"
	source := filepath.Join(sourceRoot, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(source), 0o750); err != nil {
		t.Fatal(err)
	}
	writeUnsignedGPKG(t, source)
	digest, size, err := fileDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	task := &Task{
		ID: uuid.NewString(), JobID: uuid.NewString(), AttemptID: uuid.NewString(),
		AttemptFence: 1, State: "claimed", SourceToken: sourceToken,
		Architecture: "amd64", Owner: "signer-1", ClaimFence: 1,
		Artifacts: []Artifact{{RelativePath: relative, InputDigest: digest, InputSize: size}},
	}
	queue := &testQueue{task: task}
	runner := &Runner{
		Queue: queue, BinpkgPath: binpkgPath, Owner: "signer-1",
		Lease: time.Minute, GPG: gpg,
	}
	worked, err := runner.RunOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("RunOnce worked=%v err=%v", worked, err)
	}
	if queue.failed != "" || len(queue.completed) != 1 {
		t.Fatalf("signing result failed=%q completed=%+v", queue.failed, queue.completed)
	}
	outputToken, _ := OutputToken(task.ID)
	outputRoot, _ := TokenRoot(binpkgPath, outputToken)
	output := filepath.Join(outputRoot, filepath.FromSlash(relative))
	if err := gpg.VerifyGPKG(output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outputRoot, "Packages")); err != nil {
		t.Fatalf("signed Packages index missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputRoot, CapabilityMarkerName)); err != nil {
		t.Fatalf("signed capability marker missing: %v", err)
	}
	if current, _, err := fileDigest(source); err != nil || current != digest {
		t.Fatalf("unsigned source generation was mutated: digest=%s err=%v", current, err)
	}
}

func TestRunnerRejectsDigestMismatchAndCleansOutput(t *testing.T) {
	root := t.TempDir()
	binpkgPath := filepath.Join(root, "binpkgs")
	sourceToken := strings.Repeat("b", 32)
	sourceRoot, _ := TokenRoot(binpkgPath, sourceToken)
	relative := "app-misc/hello-2.12.2.gpkg.tar"
	source := filepath.Join(sourceRoot, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(source), 0o750); err != nil {
		t.Fatal(err)
	}
	writeUnsignedGPKG(t, source)
	_, size, _ := fileDigest(source)
	task := &Task{
		ID: uuid.NewString(), JobID: uuid.NewString(), AttemptID: uuid.NewString(),
		AttemptFence: 1, State: "claimed", SourceToken: sourceToken,
		Architecture: "amd64", Owner: "signer-1", ClaimFence: 1,
		Artifacts: []Artifact{{
			RelativePath: relative, InputDigest: strings.Repeat("0", 64), InputSize: size,
		}},
	}
	queue := &testQueue{task: task}
	runner := &Runner{
		Queue: queue, BinpkgPath: binpkgPath, Owner: "signer-1",
		Lease: time.Minute, GPG: GPG{Home: "unused", KeyID: "unused"},
	}
	worked, err := runner.RunOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("RunOnce worked=%v err=%v", worked, err)
	}
	if !strings.Contains(queue.failed, "does not match") || queue.completed != nil {
		t.Fatalf("unexpected result failed=%q completed=%+v", queue.failed, queue.completed)
	}
	outputToken, _ := OutputToken(task.ID)
	outputRoot, _ := TokenRoot(binpkgPath, outputToken)
	if _, err := os.Stat(outputRoot); !os.IsNotExist(err) {
		t.Fatalf("failed signer output was not cleaned: %v", err)
	}
}

func TestRunnerRejectsSymlinkedInput(t *testing.T) {
	root := t.TempDir()
	binpkgPath := filepath.Join(root, "binpkgs")
	sourceToken := strings.Repeat("c", 32)
	sourceRoot, _ := TokenRoot(binpkgPath, sourceToken)
	targetDir := filepath.Join(root, "outside")
	if err := os.MkdirAll(targetDir, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, "hello.gpkg.tar")
	writeUnsignedGPKG(t, target)
	digest, size, _ := fileDigest(target)
	if err := os.MkdirAll(sourceRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetDir, filepath.Join(sourceRoot, "app-misc")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	task := &Task{
		ID: uuid.NewString(), SourceToken: sourceToken, Architecture: "amd64",
		Owner: "signer-1", ClaimFence: 1,
		Artifacts: []Artifact{{
			RelativePath: "app-misc/hello.gpkg.tar", InputDigest: digest, InputSize: size,
		}},
	}
	queue := &testQueue{task: task}
	runner := &Runner{
		Queue: queue, BinpkgPath: binpkgPath, Owner: "signer-1",
		Lease: time.Minute, GPG: GPG{Home: "unused", KeyID: "unused"},
	}
	_, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(queue.failed, "symlink") {
		t.Fatalf("symlink input was not rejected: %q", queue.failed)
	}
}

func TestRunnerSignsObjectBackedGeneration(t *testing.T) {
	gpg := testGPG(t)
	root := t.TempDir()
	store, err := artifactstorage.NewLocalStorage(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	sourceToken := strings.Repeat("d", 32)
	relative := "app-misc/hello-2.12.2.gpkg.tar"
	source := filepath.Join(root, "unsigned.gpkg.tar")
	writeUnsignedGPKG(t, source)
	digest, size, err := fileDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey, _ := artifactstorage.QuarantineGenerationKey(sourceToken, relative)
	if err := store.Upload(source, sourceKey); err != nil {
		t.Fatal(err)
	}
	task := &Task{
		ID: uuid.NewString(), JobID: uuid.NewString(), AttemptID: uuid.NewString(),
		AttemptFence: 1, State: "claimed", SourceToken: sourceToken,
		Architecture: "amd64", Owner: "signer-1", ClaimFence: 1,
		Artifacts: []Artifact{{RelativePath: relative, InputDigest: digest, InputSize: size}},
	}
	queue := &testQueue{task: task}
	runner := &Runner{
		Queue: queue, Storage: store, ScratchDir: filepath.Join(root, "scratch"),
		Owner: "signer-1", Lease: time.Minute, GPG: gpg,
	}
	worked, err := runner.RunOnce(context.Background())
	if err != nil || !worked || queue.failed != "" || len(queue.completed) != 1 {
		t.Fatalf("worked=%v err=%v failed=%q completed=%+v",
			worked, err, queue.failed, queue.completed)
	}
	outputToken, _ := OutputToken(task.ID)
	outputKey, _ := artifactstorage.QuarantineGenerationKey(outputToken, relative)
	output := filepath.Join(root, "signed.gpkg.tar")
	if err := store.Download(outputKey, output); err != nil {
		t.Fatal(err)
	}
	if err := gpg.VerifyGPKG(output); err != nil {
		t.Fatal(err)
	}
	capabilityKey, _ := artifactstorage.QuarantineCapabilityKey(outputToken)
	document, err := artifactstorage.DownloadBytes(
		store, capabilityKey, filepath.Join(root, "read"), 1<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := artifactstorage.ParseQuarantineManifest(document, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Generation != "signed" || len(manifest.Artifacts) != 1 ||
		manifest.Artifacts[0].SHA256 != queue.completed[0].OutputDigest {
		t.Fatalf("unexpected signed object manifest: %#v", manifest)
	}
}
