// Package signing implements the isolated, digest-bound GPKG signing boundary.
package signing

import (
	"archive/tar"
	"bytes"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"
)

// GPG signs and verifies Gentoo GPKG payloads with an isolated keyring.
type GPG struct {
	Home  string
	KeyID string
}

type gpkgMember struct {
	header *tar.Header
	data   []byte
}

// SignGPKG signs every payload member and replaces the package atomically.
func (g GPG) SignGPKG(path string) error {
	if g.Home == "" || g.KeyID == "" {
		return fmt.Errorf("GPG home and key ID are required")
	}
	members, prefix, err := readUnsignedGPKG(path)
	if err != nil {
		return err
	}

	var output []gpkgMember
	manifestRecords := make([]string, 0, len(members)*2)
	for _, member := range members {
		base := filepath.Base(member.header.Name)
		output = append(output, member)
		manifestRecords = append(manifestRecords, manifestRecord(base, member.data))
		if base == "gpkg-1" {
			continue
		}
		signature, err := g.sign(member.data, true)
		if err != nil {
			return fmt.Errorf("sign GPKG member %s: %w", base, err)
		}
		signatureHeader := &tar.Header{
			Name:    member.header.Name + ".sig",
			Mode:    0o644,
			Size:    int64(len(signature)),
			ModTime: time.Now().UTC(),
			Format:  tar.FormatPAX,
		}
		output = append(output, gpkgMember{header: signatureHeader, data: signature})
		manifestRecords = append(manifestRecords, manifestRecord(base+".sig", signature))
	}
	sort.SliceStable(output, func(i, j int) bool {
		// Keep each detached signature immediately after its payload.
		left := strings.TrimSuffix(output[i].header.Name, ".sig")
		right := strings.TrimSuffix(output[j].header.Name, ".sig")
		if left == right {
			return !strings.HasSuffix(output[i].header.Name, ".sig")
		}
		return indexOfMember(members, left) < indexOfMember(members, right)
	})
	manifestPlain := []byte(strings.Join(manifestRecords, "\n") + "\n")
	manifest, err := g.sign(manifestPlain, false)
	if err != nil {
		return fmt.Errorf("clear-sign GPKG Manifest: %w", err)
	}
	output = append(output, gpkgMember{header: &tar.Header{
		Name: prefix + "/Manifest", Mode: 0o644, Size: int64(len(manifest)),
		ModTime: time.Now().UTC(), Format: tar.FormatPAX,
	}, data: manifest})

	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".signing-*")
	if err != nil {
		return fmt.Errorf("create signed GPKG temporary file: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(0o640); err != nil {
		_ = temp.Close()
		return err
	}
	tw := tar.NewWriter(temp)
	for _, member := range output {
		header := *member.header
		header.Size = int64(len(member.data))
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""
		header.Format = tar.FormatPAX
		if err := tw.WriteHeader(&header); err != nil {
			_ = temp.Close()
			return fmt.Errorf("write signed GPKG header: %w", err)
		}
		if _, err := tw.Write(member.data); err != nil {
			_ = temp.Close()
			return fmt.Errorf("write signed GPKG member: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("close signed GPKG tar: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync signed GPKG: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close signed GPKG: %w", err)
	}
	if err := g.VerifyGPKG(tempName); err != nil {
		return fmt.Errorf("self-verify signed GPKG: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("atomically replace signed GPKG: %w", err)
	}
	return nil
}

func indexOfMember(members []gpkgMember, name string) int {
	for i := range members {
		if members[i].header.Name == name {
			return i
		}
	}
	return len(members)
}

func readUnsignedGPKG(path string) ([]gpkgMember, string, error) {
	file, err := os.Open(path) // #nosec G304 -- signer receives a validated quarantine path.
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = file.Close() }()
	tr := tar.NewReader(file)
	var members []gpkgMember
	prefix := ""
	seen := map[string]struct{}{}
	var manifest []byte
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read GPKG tar: %w", err)
		}
		if !header.FileInfo().Mode().IsRegular() {
			return nil, "", fmt.Errorf("GPKG member %q is not a regular file", header.Name)
		}
		clean := filepath.ToSlash(filepath.Clean(header.Name))
		parts := strings.Split(clean, "/")
		if clean != header.Name || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, "", fmt.Errorf("unsafe or invalid GPKG member %q", header.Name)
		}
		if prefix == "" {
			prefix = parts[0]
		} else if parts[0] != prefix {
			return nil, "", fmt.Errorf("GPKG members do not share one prefix")
		}
		if _, exists := seen[header.Name]; exists {
			return nil, "", fmt.Errorf("duplicate GPKG member %q", header.Name)
		}
		seen[header.Name] = struct{}{}
		if header.Size < 0 || header.Size > 16<<30 {
			return nil, "", fmt.Errorf("GPKG member %q has invalid size", header.Name)
		}
		data, err := io.ReadAll(io.LimitReader(tr, header.Size+1))
		if err != nil || int64(len(data)) != header.Size {
			return nil, "", fmt.Errorf("read GPKG member %q", header.Name)
		}
		if parts[1] == "Manifest" {
			manifest = data
			continue
		}
		if strings.HasSuffix(parts[1], ".sig") {
			return nil, "", fmt.Errorf("signer accepts only unsigned GPKG input")
		}
		copyHeader := *header
		members = append(members, gpkgMember{header: &copyHeader, data: data})
	}
	if prefix == "" || len(manifest) == 0 {
		return nil, "", fmt.Errorf("GPKG prefix or Manifest is missing")
	}
	if bytes.Contains(manifest, []byte("-----BEGIN PGP SIGNATURE-----")) {
		return nil, "", fmt.Errorf("signer accepts only unsigned GPKG input")
	}
	if err := verifyManifest(manifest, members); err != nil {
		return nil, "", err
	}
	hasVersion := false
	for _, member := range members {
		hasVersion = hasVersion || filepath.Base(member.header.Name) == "gpkg-1"
	}
	if !hasVersion {
		return nil, "", fmt.Errorf("GPKG version marker is missing")
	}
	return members, prefix, nil
}

// VerifyGPKG verifies every payload signature and the signed Manifest.
func (g GPG) VerifyGPKG(path string) error {
	file, err := os.Open(path) // #nosec G304 -- signer-owned output path.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	tr := tar.NewReader(file)
	members := map[string][]byte{}
	prefix := ""
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if !header.FileInfo().Mode().IsRegular() {
			return fmt.Errorf("signed GPKG member %q is not a regular file", header.Name)
		}
		clean := filepath.ToSlash(filepath.Clean(header.Name))
		parts := strings.Split(clean, "/")
		if clean != header.Name || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("invalid signed GPKG member %q", header.Name)
		}
		if prefix == "" {
			prefix = parts[0]
		} else if prefix != parts[0] {
			return fmt.Errorf("signed GPKG has multiple prefixes")
		}
		if _, exists := members[parts[1]]; exists {
			return fmt.Errorf("duplicate signed GPKG member %q", parts[1])
		}
		data, err := io.ReadAll(io.LimitReader(tr, header.Size+1))
		if err != nil || int64(len(data)) != header.Size {
			return fmt.Errorf("read signed GPKG member %q", header.Name)
		}
		members[parts[1]] = data
	}
	manifest, ok := members["Manifest"]
	if !ok {
		return fmt.Errorf("signed GPKG Manifest is missing")
	}
	plain, err := g.verifyClearSigned(manifest)
	if err != nil {
		return fmt.Errorf("verify GPKG Manifest signature: %w", err)
	}
	delete(members, "Manifest")
	list := make([]gpkgMember, 0, len(members))
	for name, data := range members {
		list = append(list, gpkgMember{header: &tar.Header{Name: prefix + "/" + name, Size: int64(len(data))}, data: data})
	}
	if err := verifyManifest(plain, list); err != nil {
		return err
	}
	if _, exists := members["gpkg-1.sig"]; exists {
		return fmt.Errorf("GPKG version marker must not have a signature")
	}
	for name := range members {
		if !strings.HasSuffix(name, ".sig") {
			continue
		}
		payload := strings.TrimSuffix(name, ".sig")
		if payload == "" {
			return fmt.Errorf("orphan GPKG signature %q", name)
		}
		if _, exists := members[payload]; !exists {
			return fmt.Errorf("orphan GPKG signature %q", name)
		}
	}
	for name, data := range members {
		if name == "gpkg-1" || strings.HasSuffix(name, ".sig") {
			continue
		}
		signature, exists := members[name+".sig"]
		if !exists {
			return fmt.Errorf("signature for GPKG member %q is missing", name)
		}
		if err := g.verifyDetached(data, signature); err != nil {
			return fmt.Errorf("verify GPKG member %q: %w", name, err)
		}
	}
	return nil
}

func manifestRecord(name string, data []byte) string {
	sha := sha512.Sum512(data)
	blake := blake2b.Sum512(data)
	return "DATA " + name + " " + strconv.FormatInt(int64(len(data)), 10) +
		" BLAKE2B " + hex.EncodeToString(blake[:]) +
		" SHA512 " + hex.EncodeToString(sha[:])
}

func verifyManifest(manifest []byte, members []gpkgMember) error {
	type record struct {
		size   int64
		sha512 string
		blake2 string
	}
	records := map[string]record{}
	for _, line := range strings.Split(strings.TrimSpace(string(manifest)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 7 || fields[0] != "DATA" {
			return fmt.Errorf("invalid GPKG Manifest record")
		}
		if _, exists := records[fields[1]]; exists {
			return fmt.Errorf("duplicate GPKG Manifest record %q", fields[1])
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return fmt.Errorf("invalid GPKG Manifest size for %q", fields[1])
		}
		hashes := map[string]string{
			fields[3]: strings.ToLower(fields[4]),
			fields[5]: strings.ToLower(fields[6]),
		}
		if len(hashes) != 2 || len(hashes["SHA512"]) != sha512.Size*2 ||
			len(hashes["BLAKE2B"]) != blake2b.Size*2 {
			return fmt.Errorf("invalid GPKG Manifest hashes for %q", fields[1])
		}
		if _, err := hex.DecodeString(hashes["SHA512"]); err != nil {
			return fmt.Errorf("invalid GPKG Manifest SHA512 for %q", fields[1])
		}
		if _, err := hex.DecodeString(hashes["BLAKE2B"]); err != nil {
			return fmt.Errorf("invalid GPKG Manifest BLAKE2B for %q", fields[1])
		}
		records[fields[1]] = record{
			size: size, sha512: hashes["SHA512"], blake2: hashes["BLAKE2B"],
		}
	}
	for _, member := range members {
		name := filepath.Base(member.header.Name)
		record, exists := records[name]
		sha := sha512.Sum512(member.data)
		blake := blake2b.Sum512(member.data)
		if !exists || record.size != int64(len(member.data)) ||
			record.sha512 != hex.EncodeToString(sha[:]) ||
			record.blake2 != hex.EncodeToString(blake[:]) {
			return fmt.Errorf("GPKG Manifest mismatch for %q", name)
		}
		delete(records, name)
	}
	for name := range records {
		return fmt.Errorf("GPKG Manifest references unknown member %q", name)
	}
	return nil
}

func (g GPG) sign(data []byte, detached bool) ([]byte, error) {
	mode := "--clearsign"
	if detached {
		mode = "--detach-sign"
	}
	args := []string{
		"--homedir", g.Home, "--batch", "--yes", "--no-tty",
		"--armor", "--digest-algo", "SHA512", "--local-user", g.KeyID,
		mode, "--output", "-",
	}
	command := exec.Command("gpg", args...)
	command.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("gpg signing failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("gpg produced an empty signature")
	}
	return stdout.Bytes(), nil
}

func (g GPG) verifyDetached(data, signature []byte) error {
	signatureFile, err := os.CreateTemp("", "portage-gpkg-signature-*.asc")
	if err != nil {
		return err
	}
	name := signatureFile.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := signatureFile.Write(signature); err != nil {
		_ = signatureFile.Close()
		return err
	}
	if err := signatureFile.Close(); err != nil {
		return err
	}
	command := exec.Command("gpg", "--homedir", g.Home, "--batch", "--status-fd", "1", "--verify", name, "-")
	command.Stdin = bytes.NewReader(data)
	var status, stderr bytes.Buffer
	command.Stdout, command.Stderr = &status, &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("gpg verification failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if !strings.Contains(status.String(), "[GNUPG:] VALIDSIG ") {
		return fmt.Errorf("gpg did not report a valid signature")
	}
	return nil
}

func (g GPG) verifyClearSigned(data []byte) ([]byte, error) {
	command := exec.Command("gpg", "--homedir", g.Home, "--batch", "--status-fd", "2", "--decrypt")
	command.Stdin = bytes.NewReader(data)
	var plain, status bytes.Buffer
	command.Stdout, command.Stderr = &plain, &status
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("gpg clear-sign verification failed: %w: %s", err, strings.TrimSpace(status.String()))
	}
	if !strings.Contains(status.String(), "[GNUPG:] VALIDSIG ") {
		return nil, fmt.Errorf("gpg did not report a valid clear-signature")
	}
	return plain.Bytes(), nil
}
