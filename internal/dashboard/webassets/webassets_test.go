package webassets

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// shellTemplate is the part of web/index.html this package's contract covers:
// the language on <html>, and the boot payload in a synchronous script ahead of
// the module script. The fixture keeps the ordering explicit so a test can
// assert it rather than assume it.
const shellTemplate = `<!doctype html>
<html lang="{{.HTMLLang}}" data-pe-lang="{{.Lang}}">
  <head>
    <script>
      window.__PE_BOOT__ = {{.Boot}};
    </script>
  </head>
  <body>
    <div id="root"></div>
    <script type="module" src="/static/ui/assets/index-abc123.js"></script>
  </body>
</html>
`

func builtBundle() fstest.MapFS {
	return fstest.MapFS{
		"bundle/dist/index.html":              {Data: []byte(shellTemplate)},
		"bundle/dist/assets/index-abc123.js":  {Data: []byte("export default 1;\n")},
		"bundle/dist/assets/index-def456.css": {Data: []byte(":root{}\n")},
		"bundle/dist/robots.txt":              {Data: []byte("User-agent: *\n")},
		"bundle/dist/.vite/manifest.json":     {Data: []byte("{}\n")},
		"bundle/.gitkeep":                     {Data: []byte("")},
	}
}

func loadBuilt(t *testing.T) *Console {
	t.Helper()
	console, err := loadFrom(builtBundle())
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	return console
}

func TestLoadReportsAnUnbuiltBundleRatherThanServingAnEmptyOne(t *testing.T) {
	_, err := loadFrom(fstest.MapFS{"bundle/.gitkeep": {Data: []byte("")}})
	if err == nil {
		t.Fatal("a tree with no index.html loaded successfully")
	}
	if !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("want ErrNotBuilt, got %v", err)
	}
}

func TestRenderIndexResolvesLanguageAndBootBeforeTheModuleScript(t *testing.T) {
	console := loadBuilt(t)
	recorder := httptest.NewRecorder()
	boot := Boot{
		Lang: "zh", HTMLLang: "zh-CN",
		AuthEnabled: true,
		IdentityProviders: []IdentityProvider{
			{ID: "authentik", DisplayName: "Authentik", SupportsStepUp: true},
		},
		Route:     Route{Name: "build-detail", Path: "/ui/build/job-1", JobID: "job-1"},
		AssetBase: "/static/ui/",
	}
	if err := console.RenderIndex(recorder, boot); err != nil {
		t.Fatalf("RenderIndex: %v", err)
	}
	page := recorder.Body.String()

	if !strings.Contains(page, `<html lang="zh-CN" data-pe-lang="zh">`) {
		t.Errorf("the shell did not carry the resolved language on <html>:\n%s", page)
	}
	bootAt := strings.Index(page, "__PE_BOOT__")
	moduleAt := strings.Index(page, `type="module"`)
	if bootAt < 0 || moduleAt < 0 || bootAt > moduleAt {
		t.Errorf("the boot payload must be readable before the bundle runs; boot at %d, module at %d",
			bootAt, moduleAt)
	}
	for _, want := range []string{
		`"lang":"zh"`, `"job_id":"job-1"`, `"auth_enabled":true`,
		`"id":"authentik"`, `"display_name":"Authentik"`, `"supports_step_up":true`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("boot payload is missing %s:\n%s", want, page)
		}
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("the shell is per-reader and must not be cached; Cache-Control %q", got)
	}
	if got := recorder.Header().Get("Vary"); !strings.Contains(got, "Accept-Language") {
		t.Errorf("a shared cache must not reuse one reader's language; Vary %q", got)
	}
}

func TestRenderIndexCannotBeTalkedOutOfItsScriptElement(t *testing.T) {
	console := loadBuilt(t)
	recorder := httptest.NewRecorder()
	hostile := `</script><script>window.stolen=1</script>`
	boot := Boot{Lang: "en", HTMLLang: "en", Route: Route{Name: "build-detail", JobID: hostile}}
	if err := console.RenderIndex(recorder, boot); err != nil {
		t.Fatalf("RenderIndex: %v", err)
	}
	page := recorder.Body.String()
	if strings.Contains(page, hostile) {
		t.Errorf("a job id closed the boot script:\n%s", page)
	}
	// The shell has exactly two script elements: the boot payload and the module.
	// A third closing tag means a value opened one, which is the whole attack.
	if closings := strings.Count(page, "</script>"); closings != 2 {
		t.Errorf("%d closing script tags, want 2:\n%s", closings, page)
	}
	// What the escaper does instead: the angle brackets survive escaped inside the
	// JSON string, so the bundle still reads back the id the server resolved.
	if !strings.Contains(page, `\u003c/script\u003e`) {
		t.Errorf("the hostile id did not survive as escaped data:\n%s", page)
	}
}

func TestBootPayloadEmitsEveryFieldSoTheBundleReadsOneShape(t *testing.T) {
	encoded, err := json.Marshal(Boot{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"lang", "html_lang", "auth_enabled", "oidc_enabled", "local_login_enabled",
		"identity_providers", "principal", "route", "asset_base",
	} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("boot payload omits %q; the bundle would have to branch on its absence", key)
		}
	}
	if string(decoded["principal"]) != "null" {
		t.Errorf("an unresolved principal must be null, got %s", decoded["principal"])
	}

	var route map[string]json.RawMessage
	if err := json.Unmarshal(decoded["route"], &route); err != nil {
		t.Fatalf("unmarshal route: %v", err)
	}
	for _, key := range []string{"name", "path", "job_id", "instance_id", "user_code"} {
		if _, ok := route[key]; !ok {
			t.Errorf("route omits %q", key)
		}
	}
}

func TestAssetHandlerCachesOnlyWhatTheFilenameEarns(t *testing.T) {
	handler := loadBuilt(t).AssetHandler("/static/ui/")
	cases := []struct {
		path  string
		cache string
	}{
		// The name carries a digest of the bytes, so the URL can never mean two
		// different files.
		{"/static/ui/assets/index-abc123.js", immutableCache},
		{"/static/ui/assets/index-def456.css", immutableCache},
		// Copied through verbatim: a stable name over changing bytes.
		{"/static/ui/robots.txt", revalidatedCache},
	}
	for _, testCase := range cases {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, testCase.path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status %d", testCase.path, recorder.Code)
		}
		if got := recorder.Header().Get("Cache-Control"); got != testCase.cache {
			t.Errorf("%s: Cache-Control %q, want %q", testCase.path, got, testCase.cache)
		}
		if recorder.Header().Get("ETag") == "" {
			t.Errorf("%s: no ETag, so a stale bookmark cannot revalidate", testCase.path)
		}
	}
}

func TestAssetHandlerNamesTheTypesTheBundleContains(t *testing.T) {
	handler := loadBuilt(t).AssetHandler("/static/ui/")
	// A container with no /etc/mime.types answers nothing for .js, and a bundle
	// served as application/octet-stream does not execute.
	cases := map[string]string{
		"/static/ui/assets/index-abc123.js":  "text/javascript; charset=utf-8",
		"/static/ui/assets/index-def456.css": "text/css; charset=utf-8",
	}
	for path, want := range cases {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if got := recorder.Header().Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type %q, want %q", path, got, want)
		}
	}
}

func TestAssetHandlerRefusesTheShellAndAnythingOutsideTheTree(t *testing.T) {
	handler := loadBuilt(t).AssetHandler("/static/ui/")
	for _, path := range []string{
		// The shell is a template; its unrendered bytes contain {{.Boot}}.
		"/static/ui/index.html",
		"/static/ui/../../../etc/passwd",
		"/static/ui/assets/",
		"/static/ui/assets/missing.js",
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://console"+path, nil)
		request.URL.Path = path
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, recorder.Code)
		}
	}
}

func TestAssetHandlerAnswersARevalidationWithoutResendingTheBytes(t *testing.T) {
	handler := loadBuilt(t).AssetHandler("/static/ui/")
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/static/ui/robots.txt", nil))
	etag := first.Header().Get("ETag")

	second := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/static/ui/robots.txt", nil)
	request.Header.Set("If-None-Match", etag)
	handler.ServeHTTP(second, request)
	if second.Code != http.StatusNotModified {
		t.Fatalf("status %d, want 304", second.Code)
	}
	if body, _ := io.ReadAll(second.Body); len(body) != 0 {
		t.Errorf("304 carried %d bytes of body", len(body))
	}
}

func TestAssetHandlerRefusesAWriteMethod(t *testing.T) {
	handler := loadBuilt(t).AssetHandler("/static/ui/")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/static/ui/assets/index-abc123.js", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", recorder.Code)
	}
}
