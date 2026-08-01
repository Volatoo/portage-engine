package tests

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// yamlSchemaBlock returns the indented body of one `components.schemas` entry.
func yamlSchemaBlock(t *testing.T, document, name string) string {
	t.Helper()
	header := "\n    " + name + ":\n"
	start := strings.Index(document, header)
	if start < 0 {
		t.Fatalf("openapi schema %q is missing", name)
	}
	remainder := document[start+len(header):]
	for index, line := range strings.Split(remainder, "\n") {
		if strings.HasPrefix(line, "    ") || strings.TrimSpace(line) == "" {
			continue
		}
		_ = index
		break
	}
	end := regexp.MustCompile(`(?m)^ {0,4}\S`).FindStringIndex(remainder)
	if end == nil {
		return remainder
	}
	return remainder[:end[0]]
}

// The spec is the contract consumers code against, and the project's own client
// reads profile_id. A field name that only the spec believes in is a bug report
// waiting to happen, so bind the schema to the struct tags that produce it.
func TestOpenAPIBinhostSchemaMatchesTheServerSerialization(t *testing.T) {
	document := readRepositoryFile(t, "docs/openapi.yaml")
	source := readRepositoryFile(t, "internal/server/server.go")

	structStart := strings.Index(source, "type binhostProfile struct {")
	if structStart < 0 {
		t.Fatal("binhostProfile is no longer declared in internal/server/server.go")
	}
	structBody := source[structStart:]
	end := strings.Index(structBody, "\n}")
	if end < 0 {
		t.Fatal("binhostProfile has no closing brace at column 0")
	}
	structBody = structBody[:end]

	var fields, alwaysPresent []string
	for _, match := range regexp.MustCompile("`json:\"([^\"]+)\"`").
		FindAllStringSubmatch(structBody, -1) {
		parts := strings.Split(match[1], ",")
		if parts[0] == "-" {
			continue
		}
		fields = append(fields, parts[0])
		if !strings.Contains(match[1], ",omitempty") {
			alwaysPresent = append(alwaysPresent, parts[0])
		}
	}
	if len(fields) == 0 {
		t.Fatal("binhostProfile declares no JSON fields")
	}

	block := yamlSchemaBlock(t, document, "Binhost")
	properties := regexp.MustCompile(`(?m)^        ([a-z_]+):`).FindAllStringSubmatch(block, -1)
	documented := make([]string, 0, len(properties))
	for _, match := range properties {
		documented = append(documented, match[1])
	}
	sort.Strings(fields)
	sort.Strings(documented)
	if strings.Join(fields, ",") != strings.Join(documented, ",") {
		t.Errorf("openapi Binhost properties=%v, server serialises %v", documented, fields)
	}

	requiredMatch := regexp.MustCompile(`required: \[([^\]]+)\]`).FindStringSubmatch(block)
	if requiredMatch == nil {
		t.Fatal("openapi Binhost schema declares no required list")
	}
	requiredNames := strings.Split(requiredMatch[1], ",")
	required := make([]string, 0, len(requiredNames))
	for _, name := range requiredNames {
		required = append(required, strings.TrimSpace(name))
	}
	sort.Strings(required)
	sort.Strings(alwaysPresent)
	if strings.Join(required, ",") != strings.Join(alwaysPresent, ",") {
		t.Errorf("openapi Binhost required=%v, server always emits %v", required, alwaysPresent)
	}
}

// The handler answers a disabled-signing deployment with 404, not 503.
func TestOpenAPIDocumentsEveryGPGPublicKeyStatus(t *testing.T) {
	document := readRepositoryFile(t, "docs/openapi.yaml")
	handler := readRepositoryFile(t, "internal/server/handlers_artifact.go")

	start := strings.Index(handler, "func (s *Server) handleGPGPublicKey(")
	if start < 0 {
		t.Fatal("handleGPGPublicKey is no longer declared")
	}
	body := handler[start:]
	bodyEnd := strings.Index(body, "\n}")
	if bodyEnd < 0 {
		t.Fatal("handleGPGPublicKey has no closing brace at column 0")
	}
	body = body[:bodyEnd]

	statuses := map[string]string{
		"http.StatusNotFound":           `"404"`,
		"http.StatusServiceUnavailable": `"503"`,
	}
	// The spec section is delimited by the next path key, so both ends have to
	// exist before slicing; a renamed path would otherwise panic here instead of
	// reporting which end of the range went missing.
	pathStart := strings.Index(document, "  /api/v1/gpg/public-key:")
	if pathStart < 0 {
		t.Fatal("openapi spec no longer documents /api/v1/gpg/public-key")
	}
	section := document[pathStart:]
	sectionEnd := strings.Index(section, "\n  /api/v1/iam/")
	if sectionEnd < 0 {
		t.Fatal("openapi spec no longer lists an /api/v1/iam/ path after /api/v1/gpg/public-key")
	}
	section = section[:sectionEnd]
	for constant, documented := range statuses {
		if strings.Contains(body, constant) && !strings.Contains(section, documented) {
			t.Errorf("handleGPGPublicKey returns %s but the spec omits %s", constant, documented)
		}
	}
}
