package binpkg

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"crypto/md5"  // #nosec G501 -- MD5 is part of the Portage binpkg index format, not used for security.
	"crypto/sha1" // #nosec G505 -- SHA1 is part of the Portage binpkg index format, not used for security.
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// binhost.go generates a Portage-consumable "Packages" index for a PKGDIR so a
// stock `emerge --getbinpkg` can use this server as a binary host.
//
// The index format is Portage's bintree index (VERSION 0): an RFC822-ish
// preamble of key: value lines, a blank line, then one stanza per package. See
// portage's pym/portage/dbapi/bintree.py for the authoritative format.

// pkgEntry is the metadata for a single binary package in the index.
type pkgEntry struct {
	cpv   string            // category/package-version, e.g. dev-lang/python-3.11.0
	path  string            // path relative to PKGDIR
	size  int64             // file size in bytes
	mtime int64             // file mtime (unix seconds)
	sha1  string            // hex SHA1 of the file
	md5   string            // hex MD5 of the file
	extra map[string]string // metadata extracted from the package (SLOT, USE, KEYWORDS, *DEPEND, ...)
}

// preamble keys that come from the package metadata and should be emitted in a
// stable order. Any other extracted keys are appended alphabetically.
var orderedMetaKeys = []string{
	"BUILD_ID", "BUILD_TIME", "CHOST", "DEFINED_PHASES", "DEPEND", "DESCRIPTION",
	"EAPI", "IUSE", "KEYWORDS", "LICENSE", "PROVIDES", "RDEPEND", "REQUIRES",
	"SLOT", "USE",
}

// GenerateIndex scans pkgDir for binary packages and writes a valid Packages
// index at pkgDir/Packages. arch is the default ARCH advertised in the preamble
// (e.g. "amd64"); it may be empty. It returns the number of packages indexed.
func GenerateIndex(pkgDir, arch string) (int, error) {
	entries, err := generateIndex(pkgDir, arch)
	return len(entries), err
}

// generateIndex does the work of GenerateIndex and returns the scanned
// entries, so Store.RegenerateIndex can refresh its in-memory query view from
// the same single scan.
func generateIndex(pkgDir, arch string) ([]pkgEntry, error) {
	entries, err := scanPackages(pkgDir)
	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].cpv < entries[j].cpv })

	var buf bytes.Buffer
	writeLine := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&buf, "%s: %s\n", k, v)
		}
	}

	// Preamble.
	writeLine("ARCH", arch)
	writeLine("TIMESTAMP", fmt.Sprintf("%d", time.Now().Unix()))
	writeLine("VERSION", "0")
	writeLine("PACKAGES", fmt.Sprintf("%d", len(entries)))
	buf.WriteString("\n")

	// One stanza per package.
	for _, e := range entries {
		writeLine("CPV", e.cpv)

		// Metadata keys in a stable order, then any remaining keys sorted.
		emitted := map[string]bool{}
		for _, k := range orderedMetaKeys {
			if v, ok := e.extra[k]; ok {
				writeLine(k, v)
				emitted[k] = true
			}
		}
		remaining := make([]string, 0, len(e.extra))
		for k := range e.extra {
			if !emitted[k] {
				remaining = append(remaining, k)
			}
		}
		sort.Strings(remaining)
		for _, k := range remaining {
			writeLine(k, e.extra[k])
		}

		writeLine("MD5", e.md5)
		writeLine("SHA1", e.sha1)
		writeLine("MTIME", fmt.Sprintf("%d", e.mtime))
		writeLine("SIZE", fmt.Sprintf("%d", e.size))
		writeLine("PATH", e.path)
		buf.WriteString("\n")
	}

	// Write atomically: temp file + rename, so a concurrent emerge never reads a
	// half-written index.
	tmp := filepath.Join(pkgDir, ".Packages.tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil { // #nosec G306 -- index must be world-readable for a binhost.
		return nil, fmt.Errorf("write temp index: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(pkgDir, "Packages")); err != nil {
		return nil, fmt.Errorf("rename index: %w", err)
	}

	return entries, nil
}

// scanPackages walks pkgDir and returns an entry for every binary package.
func scanPackages(pkgDir string) ([]pkgEntry, error) {
	var entries []pkgEntry

	err := filepath.Walk(pkgDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		name := info.Name()
		isGpkg := strings.HasSuffix(name, ".gpkg.tar")
		isTbz2 := strings.HasSuffix(name, ".tbz2") || strings.HasSuffix(name, ".xpak")
		if !isGpkg && !isTbz2 {
			return nil
		}

		rel, err := filepath.Rel(pkgDir, path)
		if err != nil {
			return err
		}

		e := pkgEntry{
			path:  filepath.ToSlash(rel),
			size:  info.Size(),
			mtime: info.ModTime().Unix(),
			cpv:   cpvFromPath(rel, isGpkg),
			extra: map[string]string{},
		}
		// A gpkg filename is <PF>-<BUILD_ID>.gpkg.tar; record the build id so the
		// index disambiguates rebuilds (metadata extraction fills it too when it
		// succeeds, but compressed metadata.tar can defeat that).
		if isGpkg {
			if bid := gpkgBuildID(rel); bid != "" {
				e.extra["BUILD_ID"] = bid
			}
		}

		sha, md, herr := fileHashes(path)
		if herr != nil {
			return fmt.Errorf("hash %s: %w", rel, herr)
		}
		e.sha1, e.md5 = sha, md

		// Best-effort metadata extraction. If it fails, the entry is still valid
		// with a filename-derived CPV; emerge can fetch it but with less metadata.
		if meta := extractMetadata(path, isGpkg); meta != nil {
			for k, v := range meta {
				if isNonIndexMetaKey(k) {
					continue // binary blobs / redundant keys must not enter the text index
				}
				e.extra[k] = v
			}
			if cpv := composeCPV(meta); cpv != "" {
				e.cpv = cpv
			}
		}

		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// cpvFromPath derives category/package-version from the file path relative to
// PKGDIR. Modern gpkg layout is category/package/package-version-BUILDID.gpkg.tar;
// legacy layout is category/package-version.tbz2. For gpkg the trailing
// -<BUILD_ID> is stripped — it is NOT part of the CPV (portage rejects e.g.
// "app-misc/screenfetch-3.9.9-1" as InvalidData).
func cpvFromPath(rel string, isGpkg bool) string {
	rel = filepath.ToSlash(rel)
	base := filepath.Base(rel)
	base = strings.TrimSuffix(base, ".gpkg.tar")
	base = strings.TrimSuffix(base, ".tbz2")
	base = strings.TrimSuffix(base, ".xpak")
	if isGpkg {
		base = stripBuildID(base)
	}

	parts := strings.Split(rel, "/")
	if len(parts) >= 2 {
		// category/[package/]…-version.ext
		return parts[0] + "/" + base
	}
	return base
}

// stripBuildID removes a gpkg filename's trailing "-<digits>" build id, leaving
// PN-PV[-rN]. A package revision is "-rN" (has the 'r'), so a bare trailing
// "-<digits>" is unambiguously the build id.
func stripBuildID(pf string) string {
	i := strings.LastIndexByte(pf, '-')
	if i <= 0 || i == len(pf)-1 {
		return pf
	}
	last := pf[i+1:]
	for _, c := range last {
		if c < '0' || c > '9' {
			return pf // not a bare integer -> not a build id (e.g. version tail)
		}
	}
	return pf[:i]
}

// gpkgBuildID returns the trailing "-<digits>" build id of a gpkg filename, or
// "" if none.
func gpkgBuildID(rel string) string {
	base := filepath.Base(filepath.ToSlash(rel))
	base = strings.TrimSuffix(base, ".gpkg.tar")
	i := strings.LastIndexByte(base, '-')
	if i <= 0 || i == len(base)-1 {
		return ""
	}
	last := base[i+1:]
	for _, c := range last {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return last
}

// isNonIndexMetaKey reports whether a gpkg/xpak metadata key must be kept out of
// the Packages index: binary blobs (ENVIRONMENT.BZ2), the full ebuild source
// (<PF>.EBUILD), the installed-file list (CONTENTS), and keys the index emits
// itself from the file (SIZE/MD5/SHA1/MTIME/CPV/PATH). Emitting these would
// corrupt the plain-text index (raw bytes) or duplicate/override real values.
func isNonIndexMetaKey(k string) bool {
	switch k {
	case "ENVIRONMENT.BZ2", "ENVIRONMENT", "CONTENTS", "NEEDED", "NEEDED.ELF.2",
		"SIZE", "MD5", "SHA1", "MTIME", "CPV", "PATH", "COUNTER":
		return true
	}
	return strings.HasSuffix(k, ".EBUILD")
}

// composeCPV builds a CPV from extracted metadata if CATEGORY/PF are present.
func composeCPV(meta map[string]string) string {
	cat := meta["CATEGORY"]
	pf := meta["PF"]
	if cat != "" && pf != "" {
		return cat + "/" + pf
	}
	return ""
}

// fileHashes returns the hex SHA1 and MD5 of a file.
func fileHashes(path string) (sha1Hex, md5Hex string, err error) {
	f, err := os.Open(path) // #nosec G304 -- path comes from walking the operator-configured PKGDIR.
	if err != nil {
		return "", "", err
	}
	defer func() { _ = f.Close() }()

	h1 := sha1.New() // #nosec G401 -- format-mandated, not a security control.
	h5 := md5.New()  // #nosec G401 -- format-mandated, not a security control.
	if _, err := io.Copy(io.MultiWriter(h1, h5), f); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(h1.Sum(nil)), hex.EncodeToString(h5.Sum(nil)), nil
}

// extractMetadata pulls Portage metadata (SLOT, USE, KEYWORDS, *DEPEND, ...)
// from a binary package. Returns nil if it cannot be read. A panic while parsing
// a single (possibly corrupt) package is recovered and treated as "no metadata"
// so one bad file cannot crash the binhost index refresher goroutine.
func extractMetadata(path string, isGpkg bool) (meta map[string]string) {
	defer func() {
		if r := recover(); r != nil {
			meta = nil
		}
	}()
	if isGpkg {
		return extractGpkgMetadata(path)
	}
	return extractXpakMetadata(path)
}

// extractGpkgMetadata reads the metadata.tar member of a .gpkg.tar archive and
// returns each metadata/<KEY> file as a preamble key. It also records whether
// the package carries an OpenPGP signature (a .sig member) as the "SIGNED" key.
func extractGpkgMetadata(path string) map[string]string {
	f, err := os.Open(path) // #nosec G304 -- operator-configured PKGDIR.
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var meta map[string]string
	signed := false

	outer := tar.NewReader(f)
	for {
		hdr, err := outer.Next()
		if err != nil {
			break
		}
		name := filepath.Base(hdr.Name)
		if strings.HasSuffix(name, ".sig") {
			signed = true
			continue
		}
		if meta == nil && strings.HasPrefix(name, "metadata.tar") {
			r, cleanup, derr := decompressReader(outer, name)
			if derr == nil {
				func() {
					defer cleanup()
					meta = readMetadataTar(r)
				}()
			}
		}
	}

	if meta == nil {
		if signed {
			return map[string]string{"SIGNED": "1"}
		}
		return nil
	}
	if signed {
		meta["SIGNED"] = "1"
	}
	return meta
}

// noopCleanup is returned by codecs that hold no releasable resources.
func noopCleanup() {}

// decompressReader wraps r with a decompressor selected by the member name's
// extension. The caller must invoke the returned cleanup after consuming the
// reader. Supported formats cover Portage's common BINPKG_COMPRESS values;
// lzma/lz4 remain unsupported and fall back to filename-derived metadata.
func decompressReader(r io.Reader, name string) (io.Reader, func(), error) {
	switch {
	case strings.HasSuffix(name, ".tar"):
		return r, noopCleanup, nil
	case strings.HasSuffix(name, ".gz"):
		gr, err := gzip.NewReader(r)
		if err != nil {
			return nil, noopCleanup, err
		}
		return gr, func() { _ = gr.Close() }, nil
	case strings.HasSuffix(name, ".bz2"):
		return bzip2.NewReader(r), noopCleanup, nil
	case strings.HasSuffix(name, ".zst"), strings.HasSuffix(name, ".zstd"):
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, noopCleanup, err
		}
		return zr, zr.Close, nil
	case strings.HasSuffix(name, ".xz"):
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, noopCleanup, err
		}
		return xr, noopCleanup, nil
	default:
		return nil, noopCleanup, fmt.Errorf("unsupported metadata compression: %s", name)
	}
}

// readMetadataTar reads a nested metadata tar stream, returning metadata/<KEY>
// files as a map of KEY -> single-line value.
func readMetadataTar(r io.Reader) map[string]string {
	meta := map[string]string{}
	inner := tar.NewReader(r)
	for {
		hdr, err := inner.Next()
		if err != nil {
			break
		}
		dir, key := filepath.Split(filepath.Clean(hdr.Name))
		if !strings.HasSuffix(strings.TrimSuffix(dir, "/"), "metadata") {
			continue
		}
		data, err := io.ReadAll(inner)
		if err != nil {
			continue
		}
		if v := singleLine(data); v != "" {
			meta[strings.ToUpper(key)] = v
		}
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// extractXpakMetadata parses the trailing XPAK segment of a legacy .tbz2 binary
// package. The XPAK format appends an index + data blob and an 8-byte trailer
// "XPAKSTOP" preceded by the offset back to "XPAKPACK".
func extractXpakMetadata(path string) map[string]string {
	data, err := os.ReadFile(path) // #nosec G304 -- operator-configured PKGDIR.
	if err != nil {
		return nil
	}
	const stop = "STOP"
	const start = "XPAKPACK"
	if len(data) < 16 || string(data[len(data)-4:]) != stop {
		return nil
	}
	// The last 12 bytes are: <4-byte offset><8-byte "XPAKSTOP"> in some variants;
	// the canonical trailer is "XPAKSTOP" (8) preceded by a big-endian uint32
	// giving the size of the xpak blob.
	trailer := data[len(data)-8:]
	if string(trailer) != "XPAKSTOP" {
		return nil
	}
	xpakSize := binary.BigEndian.Uint32(data[len(data)-12 : len(data)-8])
	blobStart := len(data) - 8 - int(xpakSize)
	if blobStart < 0 || blobStart+len(start) > len(data) {
		return nil
	}
	blob := data[blobStart : len(data)-8]
	if len(blob) < len(start) || string(blob[:len(start)]) != start {
		return nil
	}
	return parseXpakBlob(blob)
}

// parseXpakBlob parses an XPAK blob (starting with "XPAKPACK") into a metadata
// map of KEY -> single-line value.
//
// The blob comes from a file on disk and its length fields are attacker- or
// corruption-controlled, so every slice is bounds-checked; a malformed blob
// yields nil rather than panicking (which, running inside the binhost index
// refresher goroutine, would crash the whole server).
func parseXpakBlob(blob []byte) map[string]string {
	const start = "XPAKPACK"
	p := len(start)
	if p+8 > len(blob) {
		return nil
	}
	indexLen := int(binary.BigEndian.Uint32(blob[p : p+4]))
	dataLen := int(binary.BigEndian.Uint32(blob[p+4 : p+8]))
	p += 8
	// Bounds-check the declared index/data section sizes before slicing.
	if indexLen < 0 || dataLen < 0 || p+indexLen+dataLen > len(blob) {
		return nil
	}
	index := blob[p : p+indexLen]
	dataSection := blob[p+indexLen : p+indexLen+dataLen]

	meta := map[string]string{}
	ip := 0
	// Each index entry is: namelen(4) + name(namelen) + dataoffset(4) +
	// datalen(4). There is NO per-entry checksum, so an entry consumes
	// 4 + namelen + 8 bytes.
	for ip+4 <= len(index) {
		nameLen := int(binary.BigEndian.Uint32(index[ip : ip+4]))
		ip += 4
		if nameLen < 0 || ip+nameLen+8 > len(index) {
			break
		}
		name := string(index[ip : ip+nameLen])
		ip += nameLen
		off := int(binary.BigEndian.Uint32(index[ip : ip+4]))
		length := int(binary.BigEndian.Uint32(index[ip+4 : ip+8]))
		ip += 8
		if off < 0 || length < 0 || off+length > len(dataSection) {
			continue
		}
		if v := singleLine(dataSection[off : off+length]); v != "" {
			meta[strings.ToUpper(strings.TrimSpace(name))] = v
		}
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// singleLine collapses metadata file contents (which may be multi-line) into a
// single space-separated line, as the Packages index format requires.
func singleLine(b []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	var parts []string
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}
