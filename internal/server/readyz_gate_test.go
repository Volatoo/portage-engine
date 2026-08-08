package server

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slchris/portage-engine/pkg/config"
)

// readyzGateTestVocabulary is every string the probe may publish. A reason that
// is not on this list has to be added here first, which is the review step.
var readyzGateTestVocabulary = map[string]bool{
	readyzReasons.storage:      true,
	readyzReasons.database:     true,
	readyzReasons.projection:   true,
	readyzReasons.ledgerWrite:  true,
	readyzReasons.notCompared:  true,
	readyzReasons.inconsistent: true,
	readyzReasons.metadata:     true,
}

// probeVerdicts is the other half of what a probe body may contain: the
// verdict itself, which carries no information beyond the status code.
var probeVerdicts = map[string]bool{"ready": true, "not ready": true, "alive": true}

// TestReadyzRefusalNamesTheSubsystemWithoutNamingTheDatabase is the behavioural
// half of the minimal-health gate: an unreachable database must not turn the
// anonymous probe into a topology disclosure.
func TestReadyzRefusalNamesTheSubsystemWithoutNamingTheDatabase(t *testing.T) {
	const private = "failed to connect to `user=portage database=ops host=10.44.0.7`: connection refused"

	s := New(&config.ServerConfig{
		BinpkgPath: t.TempDir(), MaxWorkers: 1,
		Database: config.DatabaseConfig{Enabled: true, Required: true},
	})
	defer s.builder.Shutdown()
	s.databaseInitErr = private

	recorder := httptest.NewRecorder()
	s.handleReadyz(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable database: got %d, want 503", recorder.Code)
	}
	body := recorder.Body.String()
	for _, secret := range []string{"10.44.0.7", "user=portage", "database=ops", private} {
		if strings.Contains(body, secret) {
			t.Fatalf("readyz published %q to an anonymous caller: %s", secret, body)
		}
	}
	var answer map[string]string
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decode readyz body %q: %v", body, err)
	}
	if answer["reason"] != readyzReasons.database {
		t.Fatalf("reason = %q, want %q", answer["reason"], readyzReasons.database)
	}
	if !readyzGateTestVocabulary[answer["reason"]] {
		t.Fatalf("reason %q is outside the published vocabulary", answer["reason"])
	}
}

// TestLivezPublishesNothingButLiveness keeps the second probe minimal too. It
// answers whether the process is running, and a process that is running has
// nothing else it needs to say without a credential.
func TestLivezPublishesNothingButLiveness(t *testing.T) {
	s := New(&config.ServerConfig{BinpkgPath: t.TempDir(), MaxWorkers: 1})
	defer s.builder.Shutdown()

	recorder := httptest.NewRecorder()
	s.handleLivez(recorder, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("livez: got %d, want 200", recorder.Code)
	}
	var answer map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode livez body %q: %v", recorder.Body.String(), err)
	}
	for key := range answer {
		switch key {
		case "status", "uptime", "version":
		default:
			t.Fatalf("livez published %q=%v without a credential", key, answer[key])
		}
	}
}

// TestReadyzCanOnlyEncodeTheReviewedVocabulary is the structural half, and the
// half that survives someone adding a branch.
//
// The behavioural test above can only cover the failures it can reach from a
// unit test; this one reads the handler itself and refuses any response body
// whose reason is not a readyzReasons field, the refusal helper's own reason
// parameter, or a literal already in the vocabulary. `databaseHealth.Reason`
// and `err.Error()` are both rejected by construction, which is what the two
// of them being there for a release was.
func TestReadyzCanOnlyEncodeTheReviewedVocabulary(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	guarded := map[string]bool{
		"handleReadyz": true, "handleLivez": true, "refuseReadiness": true,
		// The function that derives the published reason, not just the ones
		// that publish it. refuseReadiness encodes its `reason` parameter, and
		// three of its four call sites hand it a readyzReasons field the switch
		// below can read — the fourth calls this, so without it here the whole
		// ledger branch reached the response body unexamined and returning
		// `lastError` instead of a vocabulary field kept every case green.
		"readyzLedgerReason": true,
	}
	seen := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !guarded[fn.Name.Name] {
			continue
		}
		seen[fn.Name.Name] = true
		for _, argument := range encodedBodies(fn) {
			assertReviewedBody(t, fset, fn.Name.Name, argument)
		}
		assertNoRawBodyWrite(t, fset, fn)
		if fn.Name.Name == "readyzLedgerReason" {
			assertReturnsVocabulary(t, fset, fn)
		}
	}
	for name := range guarded {
		if !seen[name] {
			t.Fatalf("%s is no longer in server.go; move this gate with it", name)
		}
	}
	assertRefusalReasons(t, fset, file)
}

// assertRefusalReasons checks the argument, not just the parameter.
//
// refuseReadiness publishes whatever it is given, so the guarantee is only as
// good as its call sites: every reason must be a readyzReasons field or a call
// to a function this gate also guards.
func assertRefusalReasons(t *testing.T, fset *token.FileSet, file *ast.File) {
	t.Helper()
	found := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "refuseReadiness" || len(call.Args) < 2 {
			return true
		}
		found++
		// (w, reason, detail): the writer is first, so the reason is second.
		switch reason := call.Args[1].(type) {
		case *ast.SelectorExpr:
			if ident, ok := reason.X.(*ast.Ident); !ok || ident.Name != "readyzReasons" {
				t.Errorf("refusal at %s names %s, not a readyzReasons field",
					fset.Position(reason.Pos()), exprText(fset, reason))
			}
		case *ast.CallExpr:
			callee, ok := reason.Fun.(*ast.Ident)
			if !ok || callee.Name != "readyzLedgerReason" {
				t.Errorf("refusal at %s derives its reason from an unguarded call",
					fset.Position(reason.Pos()))
			}
		default:
			t.Errorf("refusal at %s computes its reason (%T)",
				fset.Position(call.Args[1].Pos()), call.Args[1])
		}
		return true
	})
	if found == 0 {
		t.Fatal("no refuseReadiness call site was found; move this gate with it")
	}
}

// assertReturnsVocabulary refuses a reason that is anything but a reviewed word.
func assertReturnsVocabulary(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl) {
	t.Helper()
	returns := 0
	ast.Inspect(fn, func(node ast.Node) bool {
		statement, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, result := range statement.Results {
			returns++
			switch value := result.(type) {
			case *ast.SelectorExpr:
				if ident, ok := value.X.(*ast.Ident); ok && ident.Name == "readyzReasons" {
					continue
				}
			case *ast.BasicLit:
				if readyzGateTestVocabulary[strings.Trim(value.Value, `"`)] {
					continue
				}
			}
			t.Errorf("%s returns a %T at %s; only readyzReasons fields may reach the body",
				fn.Name.Name, result, fset.Position(result.Pos()))
		}
		return true
	})
	if returns == 0 {
		t.Fatalf("%s returns nothing; move this gate with it", fn.Name.Name)
	}
}

// assertNoRawBodyWrite closes the door the Encode scan leaves open: a handler
// that writes the body with anything else publishes text this gate never reads.
func assertNoRawBodyWrite(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl) {
	t.Helper()
	// The writer is matched by name, taken from the signature, so the scan
	// tracks a rename. Header().Set and WriteHeader carry a content type and a
	// status code rather than a body and are not listed.
	writer := responseWriterName(fn)
	ast.Inspect(fn, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		banned := false
		switch receiver.Name {
		case writer:
			banned = selector.Sel.Name == "Write" || selector.Sel.Name == "WriteString"
		case "fmt":
			banned = strings.HasPrefix(selector.Sel.Name, "Fprint")
		case "http":
			banned = selector.Sel.Name == "Error"
		case "io":
			banned = selector.Sel.Name == "WriteString"
		}
		if !banned {
			return true
		}
		t.Errorf("%s writes the body with %s.%s at %s; the probe body must go "+
			"through Encode so this gate can read it",
			fn.Name.Name, receiver.Name, selector.Sel.Name, fset.Position(call.Pos()))
		return true
	})
}

// responseWriterName returns the name fn gives its http.ResponseWriter, or ""
// when it takes none.
func responseWriterName(fn *ast.FuncDecl) string {
	if fn.Type.Params == nil {
		return ""
	}
	for _, field := range fn.Type.Params.List {
		selector, ok := field.Type.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "ResponseWriter" {
			continue
		}
		if len(field.Names) > 0 {
			return field.Names[0].Name
		}
	}
	return ""
}

// encodedBodies collects every value handed to an Encode call inside fn.
func encodedBodies(fn *ast.FuncDecl) []ast.Expr {
	var bodies []ast.Expr
	ast.Inspect(fn, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Encode" {
			return true
		}
		bodies = append(bodies, call.Args...)
		return true
	})
	return bodies
}

// assertReviewedBody fails unless every value in the encoded map literal is a
// reviewed constant rather than something derived from an internal error.
func assertReviewedBody(t *testing.T, fset *token.FileSet, fn string, body ast.Expr) {
	t.Helper()
	literal, ok := body.(*ast.CompositeLit)
	if !ok {
		t.Fatalf("%s encodes %T at %s; probe bodies must be map literals",
			fn, body, fset.Position(body.Pos()))
	}
	for _, element := range literal.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			t.Fatalf("%s encodes a non-keyed element at %s",
				fn, fset.Position(element.Pos()))
		}
		switch value := pair.Value.(type) {
		case *ast.BasicLit:
			// A literal is only allowed if a reviewer already saw it: the
			// vocabulary, or the two words the probe answers with.
			text := strings.Trim(value.Value, `"`)
			if !readyzGateTestVocabulary[text] && !probeVerdicts[text] {
				t.Fatalf("%s publishes unreviewed literal %q at %s",
					fn, text, fset.Position(value.Pos()))
			}
		case *ast.SelectorExpr:
			ident, ok := value.X.(*ast.Ident)
			if !ok || ident.Name != "readyzReasons" {
				t.Fatalf("%s publishes %s at %s; only readyzReasons fields may be published",
					fn, exprText(fset, value), fset.Position(value.Pos()))
			}
		case *ast.Ident:
			// refuseReadiness publishes its own reason parameter, which every
			// caller supplies from readyzReasons.
			if fn != "refuseReadiness" || value.Name != "reason" {
				t.Fatalf("%s publishes identifier %q at %s",
					fn, value.Name, fset.Position(value.Pos()))
			}
		default:
			t.Fatalf("%s publishes a %T at %s; probe bodies may not be computed",
				fn, pair.Value, fset.Position(pair.Value.Pos()))
		}
	}
}

func exprText(fset *token.FileSet, expr ast.Expr) string {
	return fset.Position(expr.Pos()).String()
}
