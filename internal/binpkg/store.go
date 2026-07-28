// Package binpkg manages binary package storage and queries.
package binpkg

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Package represents a binary package.
type Package struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Arch         string            `json:"arch"`
	UseFlags     []string          `json:"use_flags"`
	Dependencies []string          `json:"dependencies"`
	Path         string            `json:"path"`
	Checksum     string            `json:"checksum"`
	Metadata     map[string]string `json:"metadata"`
}

// QueryRequest represents a package query request.
type QueryRequest struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Arch      string   `json:"arch"`
	UseFlags  []string `json:"use_flags"`
	ProfileID string   `json:"profile_id,omitempty"`
}

// QueryResponse represents a package query response.
type QueryResponse struct {
	Found       bool     `json:"found"`
	Package     *Package `json:"package,omitempty"`
	ProfileID   string   `json:"profile_id,omitempty"`
	BinhostPath string   `json:"binhost_path,omitempty"`
}

// Store manages an in-memory queryable view of the binary packages on disk
// (the PKGDIR served as a binhost). It is populated and kept fresh by
// RegenerateIndex, which scans the PKGDIR while (re)writing the Portage
// Packages index — one scan feeds both emerge clients and the JSON query API.
type Store struct {
	basePath string
	mu       sync.RWMutex
	packages map[string][]*Package // "name|arch" -> known versions

	// indexMu serializes on-disk index regeneration.
	indexMu sync.Mutex
}

// NewStore creates a new package store. The in-memory view starts empty and is
// filled by the first RegenerateIndex call (the server does this at startup
// and on a refresh interval).
func NewStore(basePath string) *Store {
	store := &Store{
		basePath: basePath,
		packages: make(map[string][]*Package),
	}

	// Create base path if it doesn't exist
	if err := os.MkdirAll(basePath, 0750); err != nil {
		fmt.Printf("Warning: failed to create base path: %v\n", err)
	}

	return store
}

// Query searches for a package matching the request. Version may be empty
// ("is any version of this package available?" — the natural binhost query),
// in which case the newest matching version is returned. Arch may be empty to
// match any architecture.
func (s *Store) Query(req *QueryRequest) (*Package, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best *Package
	for _, pkg := range s.candidatesLocked(req.Name, req.Arch) {
		if req.Version != "" && pkg.Version != req.Version {
			continue
		}
		if !useFlagsMatch(pkg.UseFlags, req.UseFlags) {
			continue
		}
		if best == nil || versionLess(best.Version, pkg.Version) {
			best = pkg
		}
	}
	return best, best != nil
}

// candidatesLocked returns the known packages for name, restricted to arch
// when non-empty. Callers must hold s.mu.
func (s *Store) candidatesLocked(name, arch string) []*Package {
	if arch != "" {
		return s.packages[packageKey(name, arch)]
	}
	var all []*Package
	for _, pkgs := range s.packages {
		if len(pkgs) > 0 && pkgs[0].Name == name {
			all = append(all, pkgs...)
		}
	}
	return all
}

// Add adds (or replaces) a package in the store.
func (s *Store) Add(pkg *Package) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := packageKey(pkg.Name, pkg.Arch)
	list := s.packages[key]
	for i, existing := range list {
		if existing.Version == pkg.Version {
			list[i] = pkg
			return nil
		}
	}
	s.packages[key] = append(list, pkg)
	return nil
}

// packageKey builds the lookup key for a package. The separator cannot appear
// in Gentoo package names or versions, so "foo-1"/"2-3" and "foo"/"1-2-3" no
// longer collide (they did with the old name-version-arch scheme).
func packageKey(name, arch string) string {
	return name + "|" + arch
}

// useFlagsMatch checks if USE flags are compatible: every requested flag must
// be enabled in the package.
func useFlagsMatch(pkgFlags, reqFlags []string) bool {
	if len(reqFlags) == 0 {
		return true
	}

	flagSet := make(map[string]bool, len(pkgFlags))
	for _, flag := range pkgFlags {
		flagSet[flag] = true
	}

	for _, flag := range reqFlags {
		if !flagSet[flag] {
			return false
		}
	}

	return true
}

// GetPath returns the on-disk path for a package. Packages ingested from the
// index carry their real relative path; the legacy layout is kept as a
// fallback for hand-constructed Package values.
func (s *Store) GetPath(pkg *Package) string {
	if pkg.Path != "" {
		return filepath.Join(s.basePath, pkg.Path)
	}
	return filepath.Join(s.basePath, pkg.Arch, fmt.Sprintf("%s-%s.tbz2", pkg.Name, pkg.Version))
}

// BasePath returns the on-disk PKGDIR this store manages. It is served to
// emerge clients as a binhost.
func (s *Store) BasePath() string {
	return s.basePath
}

// RegenerateIndex rebuilds the Portage "Packages" index from the binary
// packages currently on disk so that `emerge --getbinpkg` can consume this
// server as a binhost, and refreshes the in-memory query view from the same
// scan. arch is the default ARCH advertised in the index and assigned to
// scanned packages. Returns the number of packages indexed.
func (s *Store) RegenerateIndex(arch string) (int, error) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()

	return s.regenerateIndexLocked(arch)
}

func (s *Store) regenerateIndexLocked(arch string) (int, error) {
	entries, err := generateIndex(s.basePath, arch)
	if err != nil {
		return 0, err
	}

	fresh := make(map[string][]*Package, len(entries))
	for _, e := range entries {
		pkg := packageFromEntry(e, arch)
		if pkg == nil {
			continue
		}
		key := packageKey(pkg.Name, pkg.Arch)
		fresh[key] = append(fresh[key], pkg)
	}

	s.mu.Lock()
	s.packages = fresh
	s.mu.Unlock()

	return len(entries), nil
}

// PromoteStaged atomically copies a verified set of package files from a
// staging directory. Keeping the source until the durable publish-phase commit
// makes crash replay possible. Exact existing destinations are idempotent;
// different content at an immutable location is rejected.
func (s *Store) PromoteStaged(stagingRoot string, rels []string, arch string) ([]string, error) {
	if stagingRoot == "" || len(rels) == 0 {
		return nil, fmt.Errorf("staging root and artifacts are required")
	}

	cleanRels := make([]string, 0, len(rels))
	for _, rel := range rels {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("invalid staged artifact path %q", rel)
		}
		if slices.Contains(cleanRels, clean) {
			return nil, fmt.Errorf("duplicate staged artifact path %q", rel)
		}
		info, err := os.Lstat(filepath.Join(stagingRoot, filepath.FromSlash(clean)))
		if err != nil {
			return nil, fmt.Errorf("stat staged artifact %q: %w", rel, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("staged artifact %q is not a regular file", rel)
		}
		cleanRels = append(cleanRels, clean)
	}

	s.indexMu.Lock()
	defer s.indexMu.Unlock()

	type movedArtifact struct {
		rel     string
		dest    string
		created bool
	}
	moved := make([]movedArtifact, 0, len(cleanRels))
	rollback := func() {
		for i := len(moved) - 1; i >= 0; i-- {
			item := moved[i]
			if item.created {
				_ = os.Remove(item.dest)
			}
		}
	}

	for _, rel := range cleanRels {
		src := filepath.Join(stagingRoot, filepath.FromSlash(rel))
		dest := filepath.Join(s.basePath, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			rollback()
			return nil, fmt.Errorf("create publish directory for %q: %w", rel, err)
		}

		item := movedArtifact{rel: rel, dest: dest}
		if _, err := os.Lstat(dest); err == nil {
			equal, compareErr := sameRegularFileContent(src, dest)
			if compareErr != nil {
				rollback()
				return nil, fmt.Errorf("verify replayed artifact %q: %w", rel, compareErr)
			}
			if !equal {
				rollback()
				return nil, fmt.Errorf("published artifact %q already exists with different content", rel)
			}
			// Exact immutable destination means a prior publish reached the
			// filesystem before its database phase commit. Treat it as the
			// same idempotent publication generation.
			moved = append(moved, item)
			continue
		} else if !os.IsNotExist(err) {
			rollback()
			return nil, fmt.Errorf("inspect publish destination %q: %w", rel, err)
		}

		if err := copyFileAtomic(src, dest); err != nil {
			rollback()
			return nil, fmt.Errorf("promote artifact %q: %w", rel, err)
		}
		item.created = true
		moved = append(moved, item)
	}

	if _, err := s.regenerateIndexLocked(arch); err != nil {
		rollback()
		return nil, fmt.Errorf("regenerate Packages after promotion: %w", err)
	}

	paths := make([]string, 0, len(moved))
	for _, item := range moved {
		paths = append(paths, item.dest)
	}
	return paths, nil
}

func sameRegularFileContent(left, right string) (bool, error) {
	leftInfo, err := os.Lstat(left)
	if err != nil || !leftInfo.Mode().IsRegular() {
		return false, fmt.Errorf("staged source is unavailable or not regular")
	}
	rightInfo, err := os.Lstat(right)
	if err != nil || !rightInfo.Mode().IsRegular() {
		return false, fmt.Errorf("published destination is unavailable or not regular")
	}
	if leftInfo.Size() != rightInfo.Size() {
		return false, nil
	}
	leftDigest, err := fileSHA256(left)
	if err != nil {
		return false, err
	}
	rightDigest, err := fileSHA256(right)
	if err != nil {
		return false, err
	}
	return leftDigest == rightDigest, nil
}

func copyFileAtomic(source, destination string) error {
	input, err := os.Open(source) // #nosec G304 -- trusted staged path was confined and lstat-validated by PromoteStaged.
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".publish-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		_ = tmp.Close()
		if !success {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if _, err := io.Copy(tmp, input); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, destination); err != nil {
		return err
	}
	success = true
	return nil
}

func fileSHA256(name string) ([sha256.Size]byte, error) {
	var empty [sha256.Size]byte
	file, err := os.Open(name) // #nosec G304 -- caller passes confined, lstat-validated artifact paths.
	if err != nil {
		return empty, err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return empty, err
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

// packageFromEntry converts a scanned index entry into a queryable Package.
func packageFromEntry(e pkgEntry, arch string) *Package {
	name, version, ok := splitCPV(e.cpv)
	if !ok {
		return nil
	}
	var useFlags []string
	if use := e.extra["USE"]; use != "" {
		useFlags = strings.Fields(use)
	}
	checksum := ""
	if e.sha1 != "" {
		checksum = "sha1:" + e.sha1
	}
	return &Package{
		Name:     name,
		Version:  version,
		Arch:     arch,
		UseFlags: useFlags,
		Path:     e.path,
		Checksum: checksum,
		Metadata: e.extra,
	}
}

// splitCPV splits "category/name-version" into name and version. The version
// starts at the last hyphen that is directly followed by a digit (Gentoo
// package names cannot end in a version-like suffix, so this boundary is
// unambiguous; "-rN" revision suffixes stay with the version).
func splitCPV(cpv string) (name, version string, ok bool) {
	for i := len(cpv) - 2; i > 0; i-- {
		if cpv[i] == '-' && cpv[i+1] >= '0' && cpv[i+1] <= '9' {
			return cpv[:i], cpv[i+1:], true
		}
	}
	return "", "", false
}

// versionLess reports whether version a sorts before b, comparing dotted /
// dashed / underscored components numerically where possible. This is
// sufficient to pick the newest binpkg for a version-less query; it is not a
// full Portage vercmp.
func versionLess(a, b string) bool {
	sep := func(r rune) bool { return r == '.' || r == '-' || r == '_' }
	as, bs := strings.FieldsFunc(a, sep), strings.FieldsFunc(b, sep)
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aErr := strconv.Atoi(as[i])
		bn, bErr := strconv.Atoi(bs[i])
		switch {
		case aErr == nil && bErr == nil:
			if an != bn {
				return an < bn
			}
		default:
			if as[i] != bs[i] {
				return as[i] < bs[i]
			}
		}
	}
	return len(as) < len(bs)
}
