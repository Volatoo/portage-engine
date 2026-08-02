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
	"unicode"

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
		{path: "/", name: "landing"},
		{path: "/overview", name: "overview"},
		{path: "/image-factory", name: "image-factory"},
		{path: "/build/job-4711", name: "build-detail", jobID: "job-4711"},
		{path: "/logs/job-4711", name: "logs", jobID: "job-4711"},
		{path: "/shell/i-0abc", name: "shell", instanceID: "i-0abc"},
		// Read aloud off another screen, so case and stray spaces are the
		// reader's rather than the code's.
		{path: "/device", query: "user_code=  wdjb-mjht ", name: "device", userCode: "WDJB-MJHT"},
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
		"/no-such-page",     // absent from the table
		"/overviewish",      // a prefix match on a route name is not that route
		"/static/ui/app.js", // an asset, served by the asset handler
		"/api/status",       // an endpoint, and not a page in either console
		// A parameter route with no parameter. The bundle spells these
		// /build/:jobID, /logs/:jobID and /shell/:instanceID, and react-router
		// matches none of them without a value — so serving the shell here is a
		// 200 that paints nothing.
		"/build/",
		"/logs/",
		"/shell/",
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
	public := []string{"/", "/login", "/packages", "/docs", "/status"}
	gated := []string{
		"/overview", "/builds", "/monitor", "/settings",
		"/image-factory", "/device", "/build/job-1", "/logs/job-1",
		"/shell/i-1",
	}
	// The same answer on every mount the same page is reachable through. A
	// reader following a link that was shared while the console lived at /ui, or
	// an operator who went to the old console on purpose, is looking at the page
	// this table calls public — and meeting a login redirect on the way to it is
	// the promotion having quietly gated something it did not move.
	for _, mount := range []string{"", legacyBase, consolePreviewBase} {
		for _, path := range public {
			if !consolePathIsPublic(mount + path) {
				t.Errorf("%s is gated but its old-console twin is public", mount+path)
			}
		}
		for _, path := range gated {
			if consolePathIsPublic(mount + path) {
				t.Errorf("%s is public but its old-console twin needs a session", mount+path)
			}
		}
	}
	// The mount points themselves are the landing page, which is public. Spelled
	// without the trailing slash because that is how a bookmark spells them.
	for _, path := range []string{legacyBase, consolePreviewBase} {
		if !consolePathIsPublic(path) {
			t.Errorf("%s is gated; it is the landing page of a console that serves it publicly", path)
		}
	}
	// A near miss on a mount point is not on it. "/legacyish" reduced to a
	// console address by string-trimming rather than by segment would become
	// "ish" and match nothing, which is the right answer for the wrong reason;
	// what matters is that it cannot become "/" and turn any path at all public.
	for _, path := range []string{"/legacyish", "/uixyz", "/legacy-notes", "/uidocs"} {
		if consolePathIsPublic(path) {
			t.Errorf("%s is public; it is not a page on any mount", path)
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
	request := httptest.NewRequest(http.MethodGet, "/overview", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})

	_, route, ok := matchConsoleRoute(request.URL.Path, request.URL.Query())
	if !ok {
		t.Fatal("/overview did not match a console route")
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
		request := httptest.NewRequest(http.MethodGet, "/overview", nil)
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

	request := httptest.NewRequest(http.MethodGet, "/login", nil)
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
	request := httptest.NewRequest(http.MethodGet, "/login", nil)
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
		request := httptest.NewRequest(http.MethodGet, "/", nil)
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
// It also pins the two sentences that have no counterpart in the old console,
// which rendered on the server and so never had to tell anyone what scripting
// off costs them. They shipped English in both languages for exactly that
// reason, and this test used to assert it. They are written now, so what it
// asserts is the opposite claim: a Chinese reader gets Chinese here. A reader
// whose browser will not run the bundle cannot be sent to any other surface to
// be told why the page is blank, so this is the one place where falling back to
// the source string reaches someone who has nowhere else to go.
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

	// Both sentences are translated, so both differ from their source string and
	// both are actually Chinese. Difference alone would be satisfied by an empty
	// string, which is the shape a lost value takes.
	for _, sentence := range []struct {
		field string
		zh    string
		en    string
	}{
		{"NeedsScript", zh.NeedsScript, en.NeedsScript},
		{"StillServed", zh.StillServed, en.StillServed},
	} {
		if sentence.zh == sentence.en {
			t.Errorf("the noscript's %s is one string for both languages; a Chinese reader "+
				"whose browser runs no script has no other surface to be told on",
				sentence.field)
		}
		if !strings.ContainsFunc(sentence.zh, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
			t.Errorf("the noscript's Chinese %s carries no Chinese: %q", sentence.field, sentence.zh)
		}
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
		lang    string
		other   string
		want    string
		refused string
	}{
		{
			name: "chinese reader", accept: "zh-CN,zh;q=0.9",
			lang: "zh", other: "en", want: "总览", refused: "Overview",
		},
		{
			name: "english reader", accept: "en-US",
			lang: "en", other: "zh", want: "Overview", refused: "总览",
		},
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
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
		// The sentences are translated too, so each reader gets their own and not
		// the other's. A noscript that lost them renders a list of links
		// introduced by nothing; one that renders the wrong language's renders an
		// explanation to a reader who cannot read it, on the one surface where
		// there is no bundle left to switch language with.
		text, other := webassets.NoScriptCopy(testCase.lang), webassets.NoScriptCopy(testCase.other)
		if !strings.Contains(shell, text.NeedsScript) {
			t.Errorf("%s: the shell does not say why the page is empty", testCase.name)
		}
		if strings.Contains(shell, other.NeedsScript) {
			t.Errorf("%s: the shell says why the page is empty in the other reader's language",
				testCase.name)
		}
	}
}

// TestNoScriptSendsAScriptlessReaderToTheConsoleThatNeedsNoScript reads the
// addresses in the rendered shell.
//
// Those five links were the top-level paths until this console took them. Left
// there, the element that exists to say "here is a page you can still read"
// offers a scripting-disabled reader five more copies of the blank page they are
// already looking at — and offers it in the one browser that will run nothing
// able to tell them so. The pages are under /legacy now, and that is where the
// links go.
func TestNoScriptSendsAScriptlessReaderToTheConsoleThatNeedsNoScript(t *testing.T) {
	dashboard := New(&config.DashboardConfig{})
	if dashboard.console == nil {
		t.Skip("console bundle not built")
	}
	recorder := httptest.NewRecorder()
	dashboard.Router().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	shell := recorder.Body.String()

	for _, page := range []string{"/overview", "/builds", "/packages", "/docs", "/status"} {
		if !strings.Contains(shell, `href="`+legacyBase+page+`"`) {
			t.Errorf("the noscript does not point at %s%s", legacyBase, page)
		}
		if strings.Contains(shell, `href="`+page+`"`) {
			t.Errorf("the noscript points at %s, which is this console — "+
				"the one thing the reader of that element cannot run", page)
		}
	}
}

func TestConsoleRoutesReportAnUnbuiltBundleInsteadOf404(t *testing.T) {
	dashboard := New(&config.DashboardConfig{})
	if dashboard.console != nil {
		t.Skip("the bundle is built in this working copy; the unbuilt path cannot be exercised")
	}
	for _, path := range []string{"/", "/static/ui/assets/index.js"} {
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

// promotedAddresses is one address per row of the route table, so a route added
// to the table arrives with an address that has to answer rather than with a
// list of paths someone remembered to extend.
func promotedAddresses() []string {
	addresses := make([]string, 0, len(consoleRoutes))
	for _, entry := range consoleRoutes {
		if entry.prefix {
			// The parameter is part of the address; without one the route is
			// deliberately a 404.
			addresses = append(addresses, entry.suffix+"a-parameter")
			continue
		}
		addresses = append(addresses, entry.suffix)
	}
	return addresses
}

// promotionRouter is a dashboard whose middleware lets every page be reached
// without a session, so a test about addresses is not also a test about
// authentication.
//
// AuthEnabled stays on because /login is a page only while there is something to
// log in to — with authentication off it redirects, in this console as in the
// one it replaces — and AllowAnonymous is what then lets the gated pages answer.
func promotionRouter(t *testing.T) (*Dashboard, http.Handler) {
	t.Helper()
	dashboard := New(&config.DashboardConfig{AuthEnabled: true, AllowAnonymous: true})
	return dashboard, dashboard.Router()
}

func TestConsoleServesTheShellForEveryRouteItNames(t *testing.T) {
	dashboard, router := promotionRouter(t)
	if dashboard.console == nil {
		t.Skip("console bundle not built")
	}
	for _, path := range promotedAddresses() {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("%s: status %d, want the console shell", path, recorder.Code)
			continue
		}
		if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type %q", path, got)
		}
		// The boot payload is what makes this the ported console rather than
		// merely some HTML: the old console renders no such element, and a
		// promotion that left the old handler on the path would satisfy every
		// assertion above it.
		if body := recorder.Body.String(); !strings.Contains(body, "__PE_BOOT__") {
			t.Errorf("%s: the response carries no boot payload, so it is not the ported console", path)
		}
	}
}

// TestPromotedRoutesResolveTheRouteTheyName checks that the shell coming back is
// the shell for that page. Serving one route's payload from every address is a
// console that renders the overview under nine different URLs, and every
// status-code assertion in this file would pass while it did.
// TestConsoleMovedRedirectNamesOnlyThisOrigin holds the /ui forward to a local
// address whatever it is handed.
//
// Trimming "/ui" off "/ui//elsewhere.example" leaves "//elsewhere.example",
// which is a protocol-relative URL and an open redirect the moment it reaches
// Location. Today no route matches it, so it never gets that far — but that is
// a fact about the route table rather than about this handler, and the table is
// edited. The handler composes its answer from the matched entry's own constant
// suffix and a re-encoded query instead of forwarding what it was given.
func TestConsoleMovedRedirectNamesOnlyThisOrigin(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		ServerURL: "http://127.0.0.1:1", AuthEnabled: true,
		JWTSecret: strings.Repeat("x", 32),
	})
	router := dashboard.Router()
	for _, target := range []string{
		"/ui/packages",
		"/ui/",
		"/ui//elsewhere.example",
		"/ui//elsewhere.example/packages",
		"/ui//attacker.tld",
		"/ui/packages?next=//evil.tld",
		"/ui/packages?next=https://evil.tld",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		location := recorder.Header().Get("Location")
		if location == "" {
			continue
		}
		if !strings.HasPrefix(location, "/") || strings.HasPrefix(location, "//") {
			t.Errorf("%s redirected to %q, which does not name this origin", target, location)
		}
		if parsed, err := url.Parse(location); err != nil || parsed.Host != "" {
			t.Errorf("%s redirected to %q, which carries a host", target, location)
		}
	}
}

func TestPromotedRoutesResolveTheRouteTheyName(t *testing.T) {
	dashboard, router := promotionRouter(t)
	if dashboard.console == nil {
		t.Skip("console bundle not built")
	}
	for index, entry := range consoleRoutes {
		path := promotedAddresses()[index]
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		// The payload is JSON in a script element, so the name is a quoted field
		// rather than a bare token.
		if want := `"name":"` + entry.name + `"`; !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("%s: the boot payload does not carry %s", path, want)
		}
	}
}

// legacyMarker is the stylesheet link every page in internal/dashboard/ui.go
// carries and no page of the ported console does. It is the cheapest honest
// answer to "is this the old console", and it is one the ported shell cannot
// accidentally satisfy: the bundle's own styles are hashed and live under the
// asset prefix.
var legacyMarker = regexp.MustCompile(`<link rel="stylesheet" href="/static/apple\.css\?v=`)

func TestLegacyConsoleStillAnswersUnderItsOwnMount(t *testing.T) {
	_, router := promotionRouter(t)
	for _, suffix := range promotedAddresses() {
		path := legacyBase + suffix
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("%s: status %d, want the page the old console serves", path, recorder.Code)
			continue
		}
		body := recorder.Body.String()
		if !legacyMarker.MatchString(body) {
			t.Errorf("%s: the response is not a page internal/dashboard/ui.go rendered", path)
		}
		if strings.Contains(body, "__PE_BOOT__") {
			t.Errorf("%s: the ported console answered here, so the fallback is gone", path)
		}
	}
}

// The mount point without its trailing slash is how a bookmark spells it, and
// how an operator types it. It has to reach the old console's landing page and
// not the new console's — which is where a bare-path registration in front of
// http.StripPrefix sends it, because the stripped path is empty and the mux
// cleans an empty path to "/".
func TestLegacyMountPointReachesTheOldConsoleAndNotTheNewOne(t *testing.T) {
	_, router := promotionRouter(t)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, legacyBase, nil))
	// Which redirect status net/http picks for a missing trailing slash is
	// net/http's business and has changed between releases; where it points is
	// ours.
	location := recorder.Header().Get("Location")
	if recorder.Code < 300 || recorder.Code >= 400 || location != legacyBase+"/" {
		t.Fatalf("%s answered %d to %q, want a redirect to %q",
			legacyBase, recorder.Code, location, legacyBase+"/")
	}
	followed := httptest.NewRecorder()
	router.ServeHTTP(followed, httptest.NewRequest(http.MethodGet, location, nil))
	if !legacyMarker.MatchString(followed.Body.String()) {
		t.Errorf("%s leads to %q, which is not a page internal/dashboard/ui.go rendered",
			legacyBase, location)
	}
}

func TestThePreviousConsoleAddressForwardsToTheNewOne(t *testing.T) {
	_, router := promotionRouter(t)
	for _, testCase := range []struct{ from, to string }{
		{from: consolePreviewBase, to: "/"},
		{from: consolePreviewBase + "/", to: "/"},
		{from: consolePreviewBase + "/overview", to: "/overview"},
		{from: consolePreviewBase + "/packages", to: "/packages"},
		{from: consolePreviewBase + "/build/job-1", to: "/build/job-1"},
		{from: consolePreviewBase + "/shell/i-1", to: "/shell/i-1"},
		// The query is the reader's, and on this route it is the whole point of
		// the address.
		{from: consolePreviewBase + "/device?user_code=ABCD-2345", to: "/device?user_code=ABCD-2345"},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, testCase.from, nil))
		if recorder.Code != http.StatusFound {
			t.Errorf("%s: status %d, want a forward to %s", testCase.from, recorder.Code, testCase.to)
			continue
		}
		if location := recorder.Header().Get("Location"); location != testCase.to {
			t.Errorf("%s forwards to %q, want %q", testCase.from, location, testCase.to)
		}
	}
}

// Location is the one header in this file an attacker would like to choose.
// Trimming "/ui" off "/ui//elsewhere.example" leaves a protocol-relative URL,
// which a browser reads as a host — so the forward resolves what is left
// against the route table before it sends anyone anywhere, and a path no route
// names is refused rather than forwarded.
//
// The handler is called directly: net/http's own path cleaning would rewrite
// the doubled slash before the mux dispatched, and a guard that is only ever
// exercised through something else's cleaning is a guard nobody has tested.
func TestThePreviousConsoleAddressForwardsNowhereItDoesNotName(t *testing.T) {
	dashboard := New(&config.DashboardConfig{})
	for _, path := range []string{
		consolePreviewBase + "//elsewhere.example",
		consolePreviewBase + "//elsewhere.example/overview",
		consolePreviewBase + "/no-such-page",
		consolePreviewBase + "/build/",
		consolePreviewBase + "xyz",
	} {
		recorder := httptest.NewRecorder()
		dashboard.handleConsoleMoved(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if location := recorder.Header().Get("Location"); location != "" {
			t.Errorf("%s forwards to %q; it names no console route", path, location)
		}
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, recorder.Code)
		}
	}
}

// TestPromotedAddressesRefuseWhatNeitherConsoleNames goes through the router
// rather than through matchConsoleRoute, because the console is the mux's
// catch-all now: every address in the deployment that nothing else claims lands
// in it, and answering those with a shell would turn every typo into a page.
func TestPromotedAddressesRefuseWhatNeitherConsoleNames(t *testing.T) {
	_, router := promotionRouter(t)
	for _, path := range []string{
		"/no-such-page",
		"/overviewish",
		"/build/",
		"/logs/",
		"/shell/",
		legacyBase + "/no-such-page",
		"/legacyish",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404 — neither console names it", path, recorder.Code)
		}
	}
}

// /login is a page and a credential endpoint at one address, and only the page
// half moved to the bundle. A promotion that routed the whole path at the
// console would answer a sign-in with a shell, which reaches the browser as a
// sign-in that silently does nothing.
func TestPromotedLoginStillTakesCredentialsWhereTheFormPostsThem(t *testing.T) {
	dashboard := New(&config.DashboardConfig{
		AuthEnabled: true, AdminUser: "operator", AdminPassword: "correct-horse",
		JWTSecret: "console-promotion-test-secret",
	})
	router := dashboard.Router()

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(`{"username":"operator","password":"correct-horse"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST /login: status %d, want a session", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"token"`) {
		t.Errorf("POST /login answered %q, which carries no session", recorder.Body.String())
	}

	// And the wrong credentials are still refused there, so the test above cannot
	// be satisfied by a handler that says yes to everything.
	refused := httptest.NewRecorder()
	router.ServeHTTP(refused, httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(`{"username":"operator","password":"wrong"}`)))
	if refused.Code != http.StatusUnauthorized {
		t.Errorf("POST /login with the wrong password: status %d, want 401", refused.Code)
	}
}

// TestBundleBasenameIsTheMountTheServerServesTheShellAt holds the router's
// basename against the path the shell is actually served from.
//
// This is the failure the promotion is one edit away from at all times, and it
// is invisible from either side alone: the server answers /overview with a 200
// and a whole shell, react-router strips a basename that is not in the location,
// matches no route in its table, and renders the not-found frame. Every test
// above this line would still pass. The route table check beside it compares the
// two vocabularies and cannot see this, because the paths agree — it is the
// prefix in front of them that does not.
func TestBundleBasenameIsTheMountTheServerServesTheShellAt(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "app", "routes.ts"))
	if err != nil {
		t.Fatalf("read the bundle route table: %v", err)
	}
	declaration := regexp.MustCompile(`export const CONSOLE_BASE = '([^']*)'`).
		FindSubmatch(source)
	if declaration == nil {
		t.Fatal("web/src/app/routes.ts declares no CONSOLE_BASE this test can read")
	}
	if got := string(declaration[1]); got != consoleBase {
		t.Errorf("the bundle's router strips %q off the location and the server serves the "+
			"shell at %q: every page renders the not-found frame", got, consoleBase)
	}
}
