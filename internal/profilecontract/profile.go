// Package profilecontract validates an operator-selected Portage profile
// against the repositories already pinned on a builder image.
package profilecontract

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var componentPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+._/-]{0,255}$`)
var repoNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Parent names one exact parent line in a Portage profile's parent file.
type Parent struct {
	RepositoryName string `json:"repository_name"`
	ProfilePath    string `json:"profile_path"`
}

// Selection identifies the repository and relative profile selected by the
// server catalog. RepositoryRoot is an absolute builder path.
type Selection struct {
	RepositoryName     string   `json:"repository_name"`
	RepositoryRoot     string   `json:"repository_root"`
	ProfilePath        string   `json:"profile_path"`
	Parents            []Parent `json:"parents,omitempty"`
	VerifyExactParents bool     `json:"verify_exact_parents,omitempty"`
}

// Validate checks syntax independent of a filesystem.
func (s Selection) Validate() error {
	if !repoNamePattern.MatchString(s.RepositoryName) || !componentPattern.MatchString(s.ProfilePath) || strings.HasPrefix(s.ProfilePath, "/") {
		return fmt.Errorf("invalid profile repository or path")
	}
	for _, part := range strings.Split(s.ProfilePath, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid profile path %q", s.ProfilePath)
		}
	}
	if !filepath.IsAbs(s.RepositoryRoot) || filepath.Clean(s.RepositoryRoot) != s.RepositoryRoot {
		return fmt.Errorf("profile repository root must be an absolute clean path")
	}
	seen := make(map[string]struct{}, len(s.Parents))
	for _, parent := range s.Parents {
		if !repoNamePattern.MatchString(parent.RepositoryName) || !componentPattern.MatchString(parent.ProfilePath) || strings.HasPrefix(parent.ProfilePath, "/") {
			return fmt.Errorf("invalid parent profile")
		}
		line := parent.RepositoryName + ":" + parent.ProfilePath
		if _, duplicate := seen[line]; duplicate {
			return fmt.Errorf("duplicate parent profile %q", line)
		}
		seen[line] = struct{}{}
	}
	return nil
}

// Verify validates repository identity, profile confinement and exact parent
// lines, returning the canonical profile directory for make.profile.
func Verify(selection Selection) (string, error) {
	if err := selection.Validate(); err != nil {
		return "", err
	}
	repositoryRoot, err := filepath.EvalSymlinks(selection.RepositoryRoot)
	if err != nil {
		return "", fmt.Errorf("resolve profile repository: %w", err)
	}
	repoName, err := os.ReadFile(filepath.Join(repositoryRoot, "profiles", "repo_name")) // #nosec G304 -- repositoryRoot is operator-owned and validated above.
	if err != nil || strings.TrimSpace(string(repoName)) != selection.RepositoryName {
		return "", fmt.Errorf("profile repository identity does not match %q", selection.RepositoryName)
	}
	profilesRoot := filepath.Join(repositoryRoot, "profiles")
	profilesRoot, err = filepath.EvalSymlinks(profilesRoot)
	if err != nil {
		return "", fmt.Errorf("resolve profiles root: %w", err)
	}
	profileDir, err := filepath.EvalSymlinks(filepath.Join(profilesRoot, filepath.FromSlash(selection.ProfilePath)))
	if err != nil {
		return "", fmt.Errorf("resolve selected profile: %w", err)
	}
	relative, err := filepath.Rel(profilesRoot, profileDir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("selected profile escapes repository profiles root")
	}
	info, err := os.Stat(profileDir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("selected profile is not a directory")
	}
	if selection.VerifyExactParents {
		actualParents, err := ReadParents(filepath.Join(profileDir, "parent"))
		if err != nil {
			return "", err
		}
		expectedParents := make([]string, 0, len(selection.Parents))
		for _, parent := range selection.Parents {
			expectedParents = append(expectedParents, parent.RepositoryName+":"+parent.ProfilePath)
		}
		if !slices.Equal(actualParents, expectedParents) {
			return "", fmt.Errorf("profile parent chain mismatch: got %v, expected %v", actualParents, expectedParents)
		}
	}
	return profileDir, nil
}

// ReadParents returns non-empty, non-comment parent lines in source order.
func ReadParents(path string) ([]string, error) {
	file, err := os.Open(path) // #nosec G304 -- caller supplies a validated selected profile path.
	if err != nil {
		return nil, fmt.Errorf("read profile parent file: %w", err)
	}
	defer func() { _ = file.Close() }()
	return ParseParents(file)
}

// ParseParents parses a Portage parent file obtained through another
// transport, such as an ephemeral container exec call.
func ParseParents(reader io.Reader) ([]string, error) {
	parents := []string{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) > 512 || strings.ContainsAny(line, "\x00\r\t ") {
			return nil, fmt.Errorf("invalid profile parent line %q", line)
		}
		parents = append(parents, line)
		if len(parents) > 32 {
			return nil, fmt.Errorf("profile has too many parents")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read profile parent data: %w", err)
	}
	return parents, nil
}
