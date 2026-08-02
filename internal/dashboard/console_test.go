package dashboard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/slchris/portage-engine/internal/dashboard/webassets"
	"github.com/slchris/portage-engine/pkg/config"
)

func TestConsoleRouteTableResolvesTheParamsTheServerAlreadyKnows(t *testing.T) {
	cases := []struct {
		path       string
		query      string
		name       string
		jobID      string
		instanceID string
		userCode   string
	}{
		{path: "/ui", name: "landing"},
		{path: "/ui/", name: "landing"},
		{path: "/ui/overview", name: "overview"},
		{path: "/ui/image-factory", name: "image-factory"},
		{path: "/ui/build/job-4711", name: "build-detail", jobID: "job-4711"},
		{path: "/ui/logs/job-4711", name: "logs", jobID: "job-4711"},
		{path: "/ui/shell/i-0abc", name: "shell", instanceID: "i-0abc"},
		// Read aloud off another screen, so case and stray spaces are the
		// reader's rather than the code's.
		{path: "/ui/device", query: "user_code=  wdjb-mjht ", name: "device", userCode: "WDJB-MJHT"},
	}
	for _, testCase := range cases {
		query, err := url.ParseQuery(testCase.query)
		if err != nil {
			t.Fatalf("%s: parse query: %v", testCase.path, err)
		}
		_, route, ok := matchConsoleRoute(testCase.path, query)
		if !ok {
			t.Errorf("%s did not match any console route", testCase.path)
			continue
		}
		if route.Name != testCase.name {
			t.Errorf("%s: name %q, want %q", testCase.path, route.Name, testCase.name)
		}
		if route.JobID != testCase.jobID {
			t.Errorf("%s: job id %q, want %q", testCase.path, route.JobID, testCase.jobID)
		}
		if route.InstanceID != testCase.instanceID {
			t.Errorf("%s: instance id %q, want %q",
				testCase.path, route.InstanceID, testCase.instanceID)
		}
		if route.UserCode != testCase.userCode {
			t.Errorf("%s: user code %q, want %q", testCase.path, route.UserCode, testCase.userCode)
		}
	}
}

func TestConsoleRouteTableRefusesWhatItDoesNotName(t *testing.T) {
	for _, path := range []string{
		"/overview",         // the old console owns this one
		"/uixyz",            // a prefix match on the mount point is not a console path
		"/ui/no-such-page",  // absent from the table
		"/static/ui/app.js", // an asset, served by the asset handler
		// A parameter route with no parameter. The bundle spells these
		// /build/:jobID, /logs/:jobID and /shell/:instanceID, and react-router
		// matches none of them without a value — so serving the shell here is a
		// 200 that paints nothing.
		"/ui/build/",
		"/ui/logs/",
		"/ui/shell/",
	} {
		if _, _, ok := matchConsoleRoute(path, nil); ok {
			t.Errorf("%s matched a console route", path)
		}
	}
}

// bundleRoute is one row of the router's own table, as web/src/app/routes.ts
// spells it.
type bundleRoute struct {
	name   string
	suffix string
	prefix bool
	public bool
}

var (
	bundleTableBlock = regexp.MustCompile(
		`(?s)export const CONSOLE_ROUTES[^\[]*\[(.*?)\n\];`)
	bundleRouteName   = regexp.MustCompile(`\bname:\s*'([^']*)'`)
	bundleRoutePath   = regexp.MustCompile(`\bpath:\s*'([^']*)'`)
	bundleRoutePublic = regexp.MustCompile(`\bpublic:\s*(true|false)`)
)

// splitTopLevelObjects returns the text of each brace-delimited object at depth
// one. Splitting on '{' would cut `nav: { labelKey: … }` in half, and every row
// in the table has one.
func splitTopLevelObjects(block string) []string {
	var objects []string
	depth, start := 0, 0
	for index, character := range block {
		switch character {
		case '{':
			if depth == 0 {
				start = index + 1
			}
			depth++
		case '}':
			depth--
			if depth == 0 {
				objects = append(objects, block[start:index])
			}
		}
	}
	return objects
}

// readBundleRoutes reads the router's table out of the TypeScript source.
//
// The two tables are one vocabulary spelled twice, and until this ran nothing
// noticed them drifting: a path the server serves the shell for and the bundle
// does not name paints nothing, and a path the bundle names and the server does
// not 404s before the bundle is ever fetched. Neither is visible from the side
// that has the bug. Parsing TypeScript with regular expressions is ugly; it is
// also the only way to compare the two in a test that needs no Node toolchain,
// and the shape it depends on — one quoted `name` and one quoted `path` per row
// — is the shape a router table has.
func readBundleRoutes(t *testing.T) map[string]bundleRoute {
	t.Helper()
	source, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "app", "routes.ts"))
	if err != nil {
		t.Fatalf("read the bundle route table: %v", err)
	}
	block := bundleTableBlock.FindSubmatch(source)
	if block == nil {
		t.Fatal("web/src/app/routes.ts declares no CONSOLE_ROUTES array this test can read")
	}
	routes := make(map[string]bundleRoute)
	for _, object := range splitTopLevelObjects(string(block[1])) {
		name := bundleRouteName.FindStringSubmatch(object)
		path := bundleRoutePath.FindStringSubmatch(object)
		if name == nil || path == nil {
			t.Fatalf("a route in the bundle table has no name or no path:\n%s", object)
		}
		public := bundleRoutePublic.FindStringSubmatch(object)
		route := bundleRoute{name: name[1], public: public != nil && public[1] == "true"}
		// react-router spells a parameter `/:jobID`; this table spells the same
		// route as the prefix in front of it.
		if parameter := strings.Index(path[1], "/:"); parameter >= 0 {
			route.suffix, route.prefix = path[1][:parameter+1], true
		} else {
			route.suffix = path[1]
		}
		routes[route.name] = route
	}
	return routes
}

func TestConsoleRouteTableMatchesTheBundleRouter(t *testing.T) {
	bundle := readBundleRoutes(t)
	for _, entry := range consoleRoutes {
		found, ok := bundle[entry.name]
		if !ok {
			t.Errorf("the server serves the shell for %q, which the bundle router does not name: "+
				"every request for it paints a blank page", entry.name)
			continue
		}
		want := bundleRoute{
			name: entry.name, suffix: entry.suffix, prefix: entry.prefix, public: entry.public,
		}
		if found != want {
			t.Errorf("route %q is %+v here and %+v in web/src/app/routes.ts", entry.name, want, found)
		}
		delete(bundle, entry.name)
	}
	for name, route := range bundle {
		t.Errorf("the bundle router names %q (%s), which this table does not serve: "+
			"the address 404s before the bundle is ever fetched", name, route.suffix)
	}
}

func TestConsolePublicPagesMirrorTheOldConsole(t *testing.T) {
	// The old console serves these five without a session; the ported one has to
	// agree, or a reader following a shared link lands in a login redirect on one
	// and a page on the other.
	public := map[string]bool{
		"/ui/":         true,
		"/ui/login":    true,
		"/ui/packages": true,
		"/ui/docs":     true,
		"/ui/status":   true,
	}
	gated := []string{
		"/ui/overview", "/ui/builds", "/ui/monitor", "/ui/settings",
		"/ui/image-factory", "/ui/device", "/ui/build/job-1", "/ui/logs/job-1",
		"/ui/shell/i-1",
	}
	for path := range public {
		if !consolePathIsPublic(path) {
			t.Errorf("%s is gated but its old-console twin is public", path)
		}
	}
	for _, path := range gated {
		if consolePathIsPublic(path) {
			t.Errorf("%s is public but its old-console twin needs a session", path)
		}
	}
}

func TestConsoleBootCarriesIdentityAndNeverAuthority(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		AuthEnabled: true,
		JWTSecret:   "console-boot-test-secret",
		AdminUser:   "operator",
		// AdminPassword is empty, so local login is off. A payload that reported
		// it on would render a sign-in form nothing answers.
	})

	token, err := signToken(dashboard.config.JWTSecret, "operator", time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/ui/overview", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})

	_, route, ok := matchConsoleRoute(request.URL.Path, request.URL.Query())
	if !ok {
		t.Fatal("/ui/overview did not match a console route")
	}
	boot := dashboard.consoleBoot(request, route)

	if boot.Principal == nil {
		t.Fatal("a valid local session should be named without a round trip")
	}
	if boot.Principal.Subject != "operator" {
		t.Errorf("subject %q, want operator", boot.Principal.Subject)
	}
	if boot.Principal.Authentication != "local-session" {
		t.Errorf("authentication %q, want local-session", boot.Principal.Authentication)
	}
	if boot.LocalLoginEnabled {
		t.Error("local login reported enabled with no admin password configured")
	}
	if boot.AssetBase != consoleAssetPrefix {
		t.Errorf("asset base %q, want %q", boot.AssetBase, consoleAssetPrefix)
	}
	if !boot.AuthEnabled {
		t.Error("auth is enabled on this deployment and the payload says otherwise")
	}
}

func TestConsoleBootLeavesAnUnprovableSessionUnnamed(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		AuthEnabled: true,
		JWTSecret:   "console-boot-test-secret",
	})
	expired, err := signToken(dashboard.config.JWTSecret, "operator",
		time.Now().Add(-2*time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}

	cases := map[string]string{
		"no session":             "",
		"expired local session":  expired,
		"forged local session":   "not.a.token",
		"federated session":      "oidc.opaque-session-handle",
		"federated legacy alias": "federated.opaque-session-handle",
	}
	for name, token := range cases {
		request := httptest.NewRequest(http.MethodGet, "/ui/overview", nil)
		if token != "" {
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		}
		_, route, _ := matchConsoleRoute(request.URL.Path, request.URL.Query())
		if principal := dashboard.consoleBoot(request, route).Principal; principal != nil {
			// Naming a principal the server cannot prove is what turns a
			// capability check into a guess. Nil is the bundle's instruction to
			// ask IAM and keep gated destinations hidden until it answers.
			t.Errorf("%s produced a principal: %+v", name, principal)
		}
	}
}

func TestConsoleBootNamesEveryProviderTheDeploymentCanSignInThrough(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		AuthEnabled: true,
		JWTSecret:   "console-boot-test-secret",
		IdentityProviders: []config.IdentityProviderConfig{
			{ID: "authentik", Type: "oidc", DisplayName: "Authentik"},
			{ID: "google", Type: "oidc", DisplayName: "Google"},
			{ID: "github", Type: "github", DisplayName: "GitHub"},
			// Configured but never discovered — Initialize left it out of the
			// runtime map, so /auth/provider/broken/start answers 404.
			{ID: "broken", Type: "oidc", DisplayName: "Broken"},
		},
	})
	dashboard.providers = map[string]*oidcRuntime{
		"authentik": {}, "google": {}, "github": {},
	}

	request := httptest.NewRequest(http.MethodGet, "/ui/login", nil)
	_, route, _ := matchConsoleRoute(request.URL.Path, request.URL.Query())
	boot := dashboard.consoleBoot(request, route)

	if !boot.OIDCEnabled {
		t.Fatal("a deployment with three live providers reported no federated sign-in")
	}
	// Configuration order, because a map has none and a card that reshuffles its
	// buttons between renders is a card the reader has to re-read every time.
	want := []webassets.IdentityProvider{
		{ID: "authentik", DisplayName: "Authentik", SupportsStepUp: true},
		{ID: "google", DisplayName: "Google", SupportsStepUp: true},
		// prompt=login is not a thing a GitHub OAuth app has, so a step-up card
		// must not offer it.
		{ID: "github", DisplayName: "GitHub", SupportsStepUp: false},
	}
	if len(boot.IdentityProviders) != len(want) {
		t.Fatalf("payload carries %+v, want %+v", boot.IdentityProviders, want)
	}
	for i, provider := range boot.IdentityProviders {
		if provider != want[i] {
			t.Errorf("provider %d is %+v, want %+v", i, provider, want[i])
		}
	}
}

func TestConsoleBootLeavesTheLegacySingleProviderUnnamed(t *testing.T) {
	// The pre-multi-provider configuration: one provider, synthesised at startup
	// and absent from the configured list. There is no display name to put on a
	// button and nothing to choose between, so the payload names nobody and the
	// bundle offers the unnamed destination — which is what pageData does with
	// the same configuration.
	dashboard := New(&config.DashboardConfig{
		AuthEnabled: true,
		JWTSecret:   "console-boot-test-secret",
		OIDCEnabled: true,
	})
	request := httptest.NewRequest(http.MethodGet, "/ui/login", nil)
	_, route, _ := matchConsoleRoute(request.URL.Path, request.URL.Query())
	boot := dashboard.consoleBoot(request, route)

	if !boot.OIDCEnabled {
		t.Error("legacy single-OIDC deployment reported no federated sign-in")
	}
	if len(boot.IdentityProviders) != 0 {
		t.Errorf("legacy deployment named %+v; it has no per-provider routes",
			boot.IdentityProviders)
	}
	// Never nil: the payload states one shape, and a JSON null would be a second
	// absence for the bundle to test for.
	if boot.IdentityProviders == nil {
		t.Error("the provider list is nil rather than empty")
	}
}

func TestConsoleBootResolvesLanguageTheSameWayTheOldConsoleDoes(t *testing.T) {
	dashboard := New(&config.DashboardConfig{})
	cases := []struct {
		name     string
		cookie   string
		accept   string
		lang     string
		htmlLang string
	}{
		{name: "explicit choice wins", cookie: "zh", accept: "en-US", lang: "zh", htmlLang: "zh-CN"},
		{name: "browser preference", accept: "zh-CN,zh;q=0.9", lang: "zh", htmlLang: "zh-CN"},
		{name: "neither", lang: "en", htmlLang: "en"},
		{name: "unsupported", accept: "de-DE,de;q=0.9", lang: "en", htmlLang: "en"},
	}
	for _, testCase := range cases {
		request := httptest.NewRequest(http.MethodGet, "/ui/", nil)
		if testCase.cookie != "" {
			request.AddCookie(&http.Cookie{Name: languageCookie, Value: testCase.cookie})
		}
		if testCase.accept != "" {
			request.Header.Set("Accept-Language", testCase.accept)
		}
		_, route, _ := matchConsoleRoute(request.URL.Path, request.URL.Query())
		boot := dashboard.consoleBoot(request, route)
		if boot.Lang != testCase.lang || boot.HTMLLang != testCase.htmlLang {
			t.Errorf("%s: lang %q/%q, want %q/%q",
				testCase.name, boot.Lang, boot.HTMLLang, testCase.lang, testCase.htmlLang)
		}
	}
}

// TestNoScriptNamesThePagesTheWayTheOldConsoleDoes holds the shell's <noscript>
// copy against the catalogue it was taken from.
//
// The element exists for the reader whose browser will never run the bundle, so
// the bundle's message catalogue cannot reach it and the server picks the words.
// That is a second place words are chosen, and a second place is where a
// language quietly diverges: two Chinese words for one page is the failure this
// forecloses. webassets cannot read the catalogue itself — the dependency runs
// the other way — so the five names are transcribed there and compared here,
// which is the same arrangement the two route tables and the status vocabulary
// are already held in.
//
// It also pins what is deliberately not translated. The two sentences have no
// counterpart in the old console, which rendered on the server and so never had
// to tell anyone what scripting off costs them. English in both languages is the
// honest answer there and is what this asserts, so that "the noscript is English
// only" cannot be closed by writing a Chinese sentence nobody translated.
func TestNoScriptNamesThePagesTheWayTheOldConsoleDoes(t *testing.T) {
	english := make(map[string]string, len(consoleNav))
	for _, entry := range consoleNav {
		english[entry.key] = entry.en
	}

	zh, en := webassets.NoScriptCopy("zh"), webassets.NoScriptCopy("en")
	for _, named := range []struct {
		key string
		zh  string
		en  string
	}{
		{"nav.overview", zh.Overview, en.Overview},
		{"nav.builds", zh.Builds, en.Builds},
		{"nav.packages", zh.Packages, en.Packages},
		{"nav.docs", zh.Docs, en.Docs},
		{"nav.status", zh.Status, en.Status},
	} {
		if want := zhCatalogue[named.key]; named.zh != want {
			t.Errorf("%s: the noscript says %q and this console says %q",
				named.key, named.zh, want)
		}
		if want := english[named.key]; named.en != want {
			t.Errorf("%s: the noscript says %q and this console says %q",
				named.key, named.en, want)
		}
	}

	if zh.NeedsScript != en.NeedsScript || zh.StillServed != en.StillServed {
		t.Error("the two noscript sentences have a Chinese value; ui.go has no equivalent " +
			"sentence for either, so any value here was written rather than translated")
	}
	// An unresolved language is English, which is where resolveLanguage lands
	// for the same input.
	if webassets.NoScriptCopy("") != en {
		t.Error("an unresolved language renders something other than the source strings")
	}
}

// TestConsoleShellNoScriptSpeaksTheReaderLanguage reads the rendered shell,
// because everything above this line would still pass if index.html carried the
// English literals it used to and never named the template action.
func TestConsoleShellNoScriptSpeaksTheReaderLanguage(t *testing.T) {
	dashboard := New(&config.DashboardConfig{})
	if dashboard.console == nil {
		t.Skip("console bundle not built")
	}
	for _, testCase := range []struct {
		name    string
		accept  string
		want    string
		refused string
	}{
		{name: "chinese reader", accept: "zh-CN,zh;q=0.9", want: "总览", refused: "Overview"},
		{name: "english reader", accept: "en-US", want: "Overview", refused: "总览"},
	} {
		request := httptest.NewRequest(http.MethodGet, "/ui/", nil)
		request.Header.Set("Accept-Language", testCase.accept)
		recorder := httptest.NewRecorder()
		dashboard.Router().ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status %d", testCase.name, recorder.Code)
		}
		shell := recorder.Body.String()
		if !strings.Contains(shell, testCase.want) {
			t.Errorf("%s: the shell does not name %q", testCase.name, testCase.want)
		}
		if strings.Contains(shell, testCase.refused) {
			t.Errorf("%s: the shell names %q, which is the other reader's language",
				testCase.name, testCase.refused)
		}
		// The sentences carry no translation, so they are the same in both. A
		// noscript that lost them renders a list of links introduced by nothing.
		if !strings.Contains(shell, webassets.NoScriptCopy("en").NeedsScript) {
			t.Errorf("%s: the shell does not say why the page is empty", testCase.name)
		}
	}
}

func TestConsoleRoutesReportAnUnbuiltBundleInsteadOf404(t *testing.T) {
	dashboard := New(&config.DashboardConfig{})
	if dashboard.console != nil {
		t.Skip("the bundle is built in this working copy; the unbuilt path cannot be exercised")
	}
	for _, path := range []string{"/ui/", "/static/ui/assets/index.js"} {
		recorder := httptest.NewRecorder()
		dashboard.Router().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status %d, want 503", path, recorder.Code)
		}
	}
}

// TestBuiltBundleIsMountedAtTheAssetPrefixTheServerServes reads what npm
// actually produced. `base` in web/vite.config.ts and consoleAssetPrefix here
// are two spellings of one fact, and nothing else would notice them drifting
// apart until a browser asked for an asset that is not there.
//
// It lives in this package rather than beside the loader on purpose: the whole
// point is to compare the built shell against the constant the server really
// mounts the tree at. A copy of that string in the test would make the check
// agree with itself and with nothing else.
//
// It skips when the bundle has not been built, which is the normal state of a
// working copy — so CI asserts this case by name rather than trusting a green
// run. See the console job in .github/workflows/ci.yml.
func TestBuiltBundleIsMountedAtTheAssetPrefixTheServerServes(t *testing.T) {
	console, err := webassets.Load()
	// Only the unbuilt case is a skip. A shell that will not parse, or a tree
	// that will not read, is a failure: skipping those is a gate that reports
	// nothing precisely when there is something to report.
	if errors.Is(err, webassets.ErrNotBuilt) {
		t.Skip("console bundle not built")
	}
	if err != nil {
		t.Fatalf("webassets.Load: %v", err)
	}
	shell, err := console.IndexHTML()
	if err != nil {
		t.Fatalf("IndexHTML: %v", err)
	}

	served := make(map[string]bool, len(console.AssetPaths()))
	for _, name := range console.AssetPaths() {
		served[name] = true
	}

	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(consoleAssetPrefix) + `([^"]+)"`)
	references := pattern.FindAllStringSubmatch(shell, -1)
	if len(references) == 0 {
		t.Fatalf("the built shell references nothing under %s, so vite `base` and "+
			"consoleAssetPrefix have drifted apart:\n%s", consoleAssetPrefix, shell)
	}
	// Checked in this direction on purpose: a bundle may contain chunks the shell
	// never names, because the entry imports them. A URL in the shell that
	// nothing serves is a blank page.
	for _, reference := range references {
		if !served[reference[1]] {
			t.Errorf("the shell asks for %s%s, which is not in the bundle",
				consoleAssetPrefix, reference[1])
		}
	}
}

func TestConsoleServesTheShellForEveryRouteItNames(t *testing.T) {
	dashboard := New(&config.DashboardConfig{})
	if dashboard.console == nil {
		t.Skip("console bundle not built")
	}
	for _, path := range []string{"/ui/", "/ui/overview", "/ui/build/job-1", "/ui/shell/i-1"} {
		recorder := httptest.NewRecorder()
		dashboard.Router().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type %q", path, got)
		}
	}
}
