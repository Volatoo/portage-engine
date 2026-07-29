package binpkg

import (
	"bytes"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MergePackagesIndexes creates a complete Packages index by overlaying current
// job stanzas on the previously published generation. PATH is the immutable
// package identity, so an exact-path rebuild replaces only that stanza.
func MergePackagesIndexes(
	previous, current []byte,
	arch string,
	timestamp time.Time,
) ([]byte, error) {
	if strings.TrimSpace(arch) == "" || timestamp.IsZero() {
		return nil, fmt.Errorf("Packages merge requires architecture and timestamp")
	}
	merged := make(map[string]packagesStanza)
	for _, source := range [][]byte{previous, current} {
		if len(source) == 0 {
			continue
		}
		stanzas, err := parsePackagesStanzas(source)
		if err != nil {
			return nil, err
		}
		for _, stanza := range stanzas {
			merged[stanza.path] = stanza
		}
	}
	ordered := make([]packagesStanza, 0, len(merged))
	for _, stanza := range merged {
		ordered = append(ordered, stanza)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].cpv != ordered[j].cpv {
			return ordered[i].cpv < ordered[j].cpv
		}
		return ordered[i].path < ordered[j].path
	})
	var output bytes.Buffer
	fmt.Fprintf(&output, "ARCH: %s\n", arch)
	fmt.Fprintf(&output, "TIMESTAMP: %d\n", timestamp.UTC().Unix())
	output.WriteString("VERSION: 0\n")
	fmt.Fprintf(&output, "PACKAGES: %d\n\n", len(ordered))
	for _, stanza := range ordered {
		output.WriteString(stanza.document)
		output.WriteString("\n\n")
	}
	return output.Bytes(), nil
}

// LoadPackagesIndex refreshes the query/search projection from a trusted,
// digest-verified object generation without materializing every package.
func (s *Store) LoadPackagesIndex(document []byte, arch string) (int, error) {
	stanzas, err := parsePackagesStanzas(document)
	if err != nil {
		return 0, err
	}
	fresh := make(map[string][]*Package, len(stanzas))
	for _, stanza := range stanzas {
		fields, err := packagesFields(stanza.document)
		if err != nil {
			return 0, err
		}
		size, _ := strconv.ParseInt(fields["SIZE"], 10, 64)
		extra := make(map[string]string)
		for key, value := range fields {
			switch key {
			case "CPV", "PATH", "SIZE", "MTIME", "SHA1", "MD5":
			default:
				extra[key] = value
			}
		}
		entry := pkgEntry{
			cpv: stanza.cpv, path: stanza.path, size: size,
			sha1: fields["SHA1"], md5: fields["MD5"], extra: extra,
		}
		pkg := packageFromEntry(entry, arch)
		if pkg != nil {
			fresh[packageKey(pkg.Name, pkg.Arch)] =
				append(fresh[packageKey(pkg.Name, pkg.Arch)], pkg)
		}
	}
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	s.mu.Lock()
	s.packages = fresh
	s.mu.Unlock()
	return len(stanzas), nil
}

type packagesStanza struct {
	cpv      string
	path     string
	document string
}

func parsePackagesStanzas(document []byte) ([]packagesStanza, error) {
	if len(document) > 64<<20 {
		return nil, fmt.Errorf("Packages index exceeds merge limit")
	}
	normalized := strings.ReplaceAll(string(document), "\r\n", "\n")
	blocks := strings.Split(strings.TrimSpace(normalized), "\n\n")
	if len(blocks) == 0 {
		return nil, fmt.Errorf("Packages index is empty")
	}
	preamble, err := packagesFields(blocks[0])
	if err != nil || preamble["VERSION"] != "0" {
		return nil, fmt.Errorf("unsupported Packages preamble")
	}
	stanzas := make([]packagesStanza, 0, len(blocks)-1)
	seen := make(map[string]struct{})
	for _, block := range blocks[1:] {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		fields, err := packagesFields(block)
		if err != nil {
			return nil, err
		}
		cpv, relative := fields["CPV"], fields["PATH"]
		clean := path.Clean(strings.ReplaceAll(relative, "\\", "/"))
		if cpv == "" || relative == "" || clean != relative ||
			clean == "." || clean == ".." || strings.HasPrefix(clean, "../") ||
			strings.HasPrefix(clean, "/") {
			return nil, fmt.Errorf("Packages stanza has invalid CPV or PATH")
		}
		if _, err := strconv.ParseInt(fields["SIZE"], 10, 64); err != nil {
			return nil, fmt.Errorf("Packages stanza %q has invalid SIZE", relative)
		}
		if _, exists := seen[relative]; exists {
			return nil, fmt.Errorf("Packages index repeats PATH %q", relative)
		}
		seen[relative] = struct{}{}
		stanzas = append(stanzas, packagesStanza{
			cpv: cpv, path: relative, document: block,
		})
	}
	return stanzas, nil
}

func packagesFields(block string) (map[string]string, error) {
	fields := make(map[string]string)
	for _, line := range strings.Split(block, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("malformed Packages line")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate Packages field %q", key)
		}
		fields[key] = value
	}
	return fields, nil
}
