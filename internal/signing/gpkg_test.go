package signing

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testGPG(t *testing.T) GPG {
	t.Helper()
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg is not installed")
	}
	home, err := os.MkdirTemp("/tmp", "pe-gpg-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", home, "--kill", "gpg-agent").Run()
		_ = os.RemoveAll(home)
	})
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := "Portage Engine Test <signer-test@portage-engine.invalid>"
	command := exec.Command("gpg", "--homedir", home, "--batch", "--passphrase", "",
		"--quick-generate-key", identity, "ed25519", "sign", "0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate test GPG key: %v: %s", err, output)
	}
	command = exec.Command("gpg", "--homedir", home, "--batch", "--with-colons",
		"--list-secret-keys", identity)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var keyID string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) > 4 && fields[0] == "sec" {
			keyID = fields[4]
			break
		}
	}
	if keyID == "" {
		t.Fatal("test GPG key ID is missing")
	}
	return GPG{Home: home, KeyID: keyID}
}

func writeUnsignedGPKG(t *testing.T, path string) {
	t.Helper()
	prefix := "hello-2.12.2"
	members := make([]gpkgMember, 0, 4)
	members = append(members,
		gpkgMember{header: &tar.Header{Name: prefix + "/gpkg-1", Mode: 0o644}, data: []byte("1\n")},
		gpkgMember{header: &tar.Header{Name: prefix + "/metadata.tar", Mode: 0o644}, data: []byte("metadata payload")},
		gpkgMember{header: &tar.Header{Name: prefix + "/image.tar", Mode: 0o644}, data: []byte("image payload")},
	)
	records := make([]string, 0, len(members))
	for _, member := range members {
		records = append(records, manifestRecord(filepath.Base(member.header.Name), member.data))
	}
	members = append(members, gpkgMember{
		header: &tar.Header{Name: prefix + "/Manifest", Mode: 0o644},
		data:   []byte(strings.Join(records, "\n") + "\n"),
	})
	writeGPKGMembers(t, path, members)
}

func writeGPKGMembers(t *testing.T, path string, members []gpkgMember) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	for _, member := range members {
		header := *member.header
		header.Size = int64(len(member.data))
		header.ModTime = time.Unix(1, 0)
		if err := writer.WriteHeader(&header); err != nil {
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

func readGPKGMembers(t *testing.T, path string) []gpkgMember {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	var result []gpkgMember
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
		copyHeader := *header
		result = append(result, gpkgMember{header: &copyHeader, data: data})
	}
	return result
}

func TestSignAndVerifyGPKG(t *testing.T) {
	gpg := testGPG(t)
	path := filepath.Join(t.TempDir(), "hello-2.12.2.gpkg.tar")
	writeUnsignedGPKG(t, path)

	if err := gpg.SignGPKG(path); err != nil {
		t.Fatal(err)
	}
	if err := gpg.VerifyGPKG(path); err != nil {
		t.Fatal(err)
	}
	members := readGPKGMembers(t, path)
	names := map[string]bool{}
	for _, member := range members {
		names[filepath.Base(member.header.Name)] = true
		if filepath.Base(member.header.Name) == "Manifest" &&
			!bytes.Contains(member.data, []byte("-----BEGIN PGP SIGNED MESSAGE-----")) {
			t.Fatal("Manifest is not clear-signed")
		}
	}
	for _, required := range []string{"gpkg-1", "metadata.tar", "metadata.tar.sig", "image.tar", "image.tar.sig", "Manifest"} {
		if !names[required] {
			t.Errorf("signed GPKG is missing %s", required)
		}
	}
	if names["gpkg-1.sig"] {
		t.Fatal("gpkg-1 must not be signed")
	}
	if _, _, err := readUnsignedGPKG(path); err == nil {
		t.Fatal("signed GPKG was accepted as unsigned input")
	}
}

func TestReadUnsignedGPKGAcceptsEitherManifestHashOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hello.gpkg.tar")
	writeUnsignedGPKG(t, path)

	members := readGPKGMembers(t, path)
	for index := range members {
		if filepath.Base(members[index].header.Name) != "Manifest" {
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(members[index].data)), "\n")
		for lineIndex, line := range lines {
			fields := strings.Fields(line)
			lines[lineIndex] = strings.Join([]string{
				fields[0], fields[1], fields[2],
				fields[5], fields[6], fields[3], fields[4],
			}, " ")
		}
		members[index].data = []byte(strings.Join(lines, "\n") + "\n")
	}
	writeGPKGMembers(t, path, members)

	if _, _, err := readUnsignedGPKG(path); err != nil {
		t.Fatalf("valid SHA512-first Manifest was rejected: %v", err)
	}
}

func TestVerifyGPKGRejectsTamperedPayload(t *testing.T) {
	gpg := testGPG(t)
	path := filepath.Join(t.TempDir(), "hello.gpkg.tar")
	writeUnsignedGPKG(t, path)
	if err := gpg.SignGPKG(path); err != nil {
		t.Fatal(err)
	}
	members := readGPKGMembers(t, path)
	for index := range members {
		if filepath.Base(members[index].header.Name) == "image.tar" {
			members[index].data[0] ^= 0xff
		}
	}
	writeGPKGMembers(t, path, members)
	if err := gpg.VerifyGPKG(path); err == nil {
		t.Fatal("tampered signed GPKG passed verification")
	}
}

func TestVerifyGPKGRejectsOrphanSignature(t *testing.T) {
	gpg := testGPG(t)
	path := filepath.Join(t.TempDir(), "hello.gpkg.tar")
	writeUnsignedGPKG(t, path)
	if err := gpg.SignGPKG(path); err != nil {
		t.Fatal(err)
	}
	members := readGPKGMembers(t, path)
	prefix := strings.SplitN(members[0].header.Name, "/", 2)[0]
	var payloadMembers []gpkgMember
	for _, member := range members {
		if filepath.Base(member.header.Name) != "Manifest" {
			payloadMembers = append(payloadMembers, member)
		}
	}
	orphan := gpkgMember{
		header: &tar.Header{Name: prefix + "/orphan.sig", Mode: 0o644},
		data:   []byte("not attached to a payload"),
	}
	payloadMembers = append(payloadMembers, orphan)
	records := make([]string, 0, len(payloadMembers))
	for _, member := range payloadMembers {
		records = append(records, manifestRecord(filepath.Base(member.header.Name), member.data))
	}
	signedManifest, err := gpg.sign([]byte(strings.Join(records, "\n")+"\n"), false)
	if err != nil {
		t.Fatal(err)
	}
	payloadMembers = append(payloadMembers, gpkgMember{
		header: &tar.Header{Name: prefix + "/Manifest", Mode: 0o644},
		data:   signedManifest,
	})
	writeGPKGMembers(t, path, payloadMembers)

	if err := gpg.VerifyGPKG(path); err == nil || !strings.Contains(err.Error(), "orphan") {
		t.Fatalf("orphan signature error = %v", err)
	}
}

func TestVerifyGPKGRejectsManifestOnlyMember(t *testing.T) {
	gpg := testGPG(t)
	path := filepath.Join(t.TempDir(), "hello.gpkg.tar")
	writeUnsignedGPKG(t, path)
	if err := gpg.SignGPKG(path); err != nil {
		t.Fatal(err)
	}
	members := readGPKGMembers(t, path)
	var payloadMembers []gpkgMember
	var records []string
	prefix := ""
	for _, member := range members {
		if prefix == "" {
			prefix = strings.SplitN(member.header.Name, "/", 2)[0]
		}
		if filepath.Base(member.header.Name) == "Manifest" {
			continue
		}
		payloadMembers = append(payloadMembers, member)
		records = append(records, manifestRecord(filepath.Base(member.header.Name), member.data))
	}
	records = append(records, manifestRecord("ghost.sig", []byte("not in the archive")))
	signedManifest, err := gpg.sign([]byte(strings.Join(records, "\n")+"\n"), false)
	if err != nil {
		t.Fatal(err)
	}
	payloadMembers = append(payloadMembers, gpkgMember{
		header: &tar.Header{Name: prefix + "/Manifest", Mode: 0o644},
		data:   signedManifest,
	})
	writeGPKGMembers(t, path, payloadMembers)

	if err := gpg.VerifyGPKG(path); err == nil || !strings.Contains(err.Error(), "unknown member") {
		t.Fatalf("manifest-only member error = %v", err)
	}
}
