package tests

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/blake2b"

	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/internal/signing"
	"github.com/slchris/portage-engine/pkg/config"
)

// TestComposeSigningRoundTrip is deliberately opt-in: it writes a short-lived
// fixture to the running Compose PostgreSQL database and shared quarantine
// volume, then waits for the separately running portage-signer container.
func TestComposeSigningRoundTrip(t *testing.T) {
	if os.Getenv("PORTAGE_COMPOSE_SIGNING_SMOKE") != "1" {
		t.Skip("set PORTAGE_COMPOSE_SIGNING_SMOKE=1 inside portage-server")
	}
	cfg, err := config.LoadServerConfig("/nonexistent.conf")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db, err := persistence.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := persistence.NewJobRepository(db)

	jobID, attemptID, workerID := uuid.New(), uuid.New(), uuid.New()
	sourceToken := strings.ReplaceAll(uuid.NewString(), "-", "")
	relative := "app-misc/compose-signing-smoke-1.gpkg.tar"
	sourceRoot, err := signing.TokenRoot(cfg.BinpkgPath, sourceToken)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceRoot, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(source), 0o750); err != nil {
		t.Fatal(err)
	}
	writeUnsignedSmokeGPKG(t, source)
	digest, size := smokeDigest(t, source)

	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = db.Pool().Exec(cleanupCtx, "DELETE FROM build_jobs WHERE id = $1", jobID)
		_ = os.RemoveAll(sourceRoot)
	}()
	stableName := "compose-signing-smoke/" + workerID.String()
	if err := db.WithTx(ctx, pgx.TxOptions{}, func(q persistence.Querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO workers (id, stable_name, max_slots)
			VALUES ($1, $2, 1)
		`, workerID, stableName); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO build_jobs (id, package_atom, state, request, request_digest)
			VALUES ($1, 'app-misc/compose-signing-smoke', 'signing', '{}'::jsonb, $2)
		`, jobID, digest); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO build_attempts (id, job_id, attempt_no, state, worker_id, fence_token)
			VALUES ($1, $2, 1, 'signing', $3, 1)
		`, attemptID, jobID, workerID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO worker_leases (id, worker_id, attempt_id, fence_token, expires_at)
			VALUES ($1, $2, $3, 1, clock_timestamp() + interval '40 seconds')
		`, uuid.New(), workerID, attemptID); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `
			INSERT INTO artifacts (
				id, job_id, attempt_id, kind, state, digest, size_bytes, location
			) VALUES ($1, $2, $3, 'binpkg', 'verified_unsigned', $4, $5, $6)
		`, uuid.New(), jobID, attemptID, digest, size,
			"quarantine:"+sourceToken+"/"+relative)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	task, err := repo.EnqueueSigning(ctx, signing.Request{
		JobID: jobID.String(), AttemptID: attemptID.String(), AttemptFence: 1,
		LeaseOwner: stableName, SourceToken: sourceToken, Architecture: "amd64",
		Artifacts: []signing.Artifact{{
			RelativePath: relative, InputDigest: digest, InputSize: size,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	outputToken, err := signing.OutputToken(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	outputRoot, _ := signing.TokenRoot(cfg.BinpkgPath, outputToken)
	defer func() { _ = os.RemoveAll(outputRoot) }()

	for {
		current, getErr := repo.GetSigningTask(ctx, task.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		switch current.State {
		case "completed":
			if len(current.Artifacts) != 1 || current.SigningKeyID == "" {
				t.Fatalf("invalid completed signing task: %+v", current)
			}
			output := filepath.Join(outputRoot, filepath.FromSlash(relative))
			outputDigest, outputSize := smokeDigest(t, output)
			if outputDigest != current.Artifacts[0].OutputDigest ||
				outputSize != current.Artifacts[0].OutputSize {
				t.Fatalf("signed output does not match durable manifest")
			}
			assertSignedSmokeGPKG(t, output)
			if _, err := os.Stat(filepath.Join(outputRoot, "Packages")); err != nil {
				t.Fatalf("signed Packages index missing: %v", err)
			}
			return
		case "failed", "canceled":
			t.Fatalf("signing task ended in %s: %s", current.State, current.Error)
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for isolated signer")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func writeUnsignedSmokeGPKG(t *testing.T, path string) {
	t.Helper()
	prefix := "compose-signing-smoke-1"
	type smokeMember struct {
		name string
		data []byte
	}
	members := make([]smokeMember, 0, 4)
	members = append(members,
		smokeMember{prefix + "/gpkg-1", []byte("1\n")},
		smokeMember{prefix + "/metadata.tar", []byte("compose metadata")},
		smokeMember{prefix + "/image.tar", []byte("compose image")},
	)
	records := make([]string, 0, len(members))
	for _, member := range members {
		records = append(records, smokeManifestRecord(filepath.Base(member.name), member.data))
	}
	members = append(members, smokeMember{prefix + "/Manifest", []byte(strings.Join(records, "\n") + "\n")})
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	for _, member := range members {
		header := &tar.Header{Name: member.name, Mode: 0o644, Size: int64(len(member.data))}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(member.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func smokeManifestRecord(name string, data []byte) string {
	sha := sha512.Sum512(data)
	blake := blake2b.Sum512(data)
	return fmt.Sprintf("DATA %s %d SHA512 %s BLAKE2B %s", name, len(data),
		hex.EncodeToString(sha[:]), hex.EncodeToString(blake[:]))
}

func smokeDigest(t *testing.T, path string) (string, int64) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil)), size
}

func assertSignedSmokeGPKG(t *testing.T, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	names := map[string]bool{}
	manifestSigned := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(header.Name)
		names[name] = true
		if name == "Manifest" {
			manifestSigned = strings.Contains(string(data), "-----BEGIN PGP SIGNED MESSAGE-----")
		}
	}
	for _, name := range []string{"metadata.tar.sig", "image.tar.sig"} {
		if !names[name] {
			t.Fatalf("signed GPKG missing %s", name)
		}
	}
	if !manifestSigned {
		t.Fatal("signed GPKG Manifest is not clear-signed")
	}
}
