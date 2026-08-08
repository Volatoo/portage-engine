package dashboard

import (
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/dashboard/webassets"
)

const (
	// consoleBase is where the ported console answers: the top level. Every
	// suffix in the route table below is therefore the address itself.
	//
	// The bundle's router basename (CONSOLE_BASE in web/src/app/routes.ts) has to
	// be this same string, because the basename is what react-router strips off
	// the browser's location before matching. A basename left at the /ui this
	// console was built at strips nothing off /overview, matches no route, and
	// renders the not-found frame on a page the server answered with a 200 —
	// which is a failure neither side can see alone.
	// TestBundleBasenameIsTheMountTheServerServesTheShellAt holds them together.
	consoleBase = "/"

	// legacyBase is where internal/dashboard/ui.go answers now that the ported
	// console owns the top-level paths.
	//
	// The old console is moved rather than deleted. The ported one has been read
	// page by page in a browser, but on one deployment — and a console is what an
	// operator opens to find out what is wrong, so losing it is the one failure
	// with no workaround. Keeping it mounted for a release makes backing the
	// promotion out an edit to the registrations in Router rather than a revert
	// of the port, and leaves the operator somewhere to go in the meantime.
	legacyBase = "/legacy"

	// consolePreviewBase is where the ported console was mounted while it was
	// built beside the old one. Links to it exist — in the deployment, in the
	// review that verified those ten pages, and possibly in a bookmark — so the
	// address forwards to the console's new home rather than 404ing at a reader
	// who followed a link that worked yesterday.
	consolePreviewBase = "/ui"

	// consoleAssetPrefix has to equal `base` in web/vite.config.ts, because that
	// is the string the build wrote into every asset URL inside index.html.
	// TestBuiltBundleIsMountedAtTheAssetPrefixTheServerServes checks the two
	// against each other once a build exists.
	//
	// It is deliberately not the console's own mount point: the shell is served
	// from every path in the route table, and the hashed tree has to sit
	// somewhere no route can ever grow into.
	consoleAssetPrefix = "/static/ui/"
)

// consoleRoute is one entry in the console's route vocabulary. The table below
// is the whole vocabulary: a path that is not in it does not reach the bundle,
// and every fact the server can state about a route — its name, whether it is
// public, what the URL carries — is stated here rather than re-derived from the
// path's spelling at each place that needs it.
type consoleRoute struct {
	name string

	// suffix is the path the route answers at. The console is mounted at the top
	// level, so this is the address itself; on the two other mounts — the old
	// console's /legacy and the /ui this console was built at — it is what is
	// left after the mount point is trimmed off.
	suffix string

	// prefix marks a route whose trailing segment is a parameter rather than
	// part of the path.
	prefix bool

	// public mirrors the community pages the old console serves without a
	// session. One table answers for both the router and the auth middleware, so
	// the two cannot come to disagree about which pages are public — which is
	// the failure mode where a page is reachable in one console and a redirect
	// loop in the other.
	public bool

	// capture writes what the URL carries into the payload, so a deep link
	// renders its frame without a round trip for an id already in the address
	// bar. A route with no parameters leaves this nil.
	capture func(tail string, query url.Values, route *webassets.Route)
}

func captureJobID(tail string, _ url.Values, route *webassets.Route) {
	route.JobID = tail
}

func captureInstanceID(tail string, _ url.Values, route *webassets.Route) {
	route.InstanceID = tail
}

// captureUserCode normalises exactly as the old device page does: the code is
// read aloud off another screen, so case and stray spaces are the reader's, not
// the code's.
func captureUserCode(_ string, query url.Values, route *webassets.Route) {
	route.UserCode = strings.ToUpper(strings.TrimSpace(query.Get("user_code")))
}

var consoleRoutes = []consoleRoute{
	{name: "landing", suffix: "/", public: true},
	{name: "login", suffix: "/login", public: true},
	{name: "packages", suffix: "/packages", public: true},
	{name: "docs", suffix: "/docs", public: true},
	{name: "status", suffix: "/status", public: true},
	{name: "device", suffix: "/device", capture: captureUserCode},
	{name: "overview", suffix: "/overview"},
	{name: "builds", suffix: "/builds"},
	{name: "monitor", suffix: "/monitor"},
	{name: "image-factory", suffix: "/image-factory"},
	{name: "settings", suffix: "/settings"},
	{name: "build-detail", suffix: "/build/", prefix: true, capture: captureJobID},
	{name: "logs", suffix: "/logs/", prefix: true, capture: captureJobID},
	{name: "shell", suffix: "/shell/", prefix: true, capture: captureInstanceID},
}

// matchConsoleRoute resolves a request path against the table. Exact entries are
// tried before prefix entries so a fixed path can never be swallowed by a prefix
// that happens to be shorter than it.
func matchConsoleRoute(suffix string, query url.Values) (consoleRoute, webassets.Route, bool) {
	resolved := webassets.Route{Path: suffix}
	for _, entry := range consoleRoutes {
		if entry.prefix || suffix != entry.suffix {
			continue
		}
		resolved.Name = entry.name
		if entry.capture != nil {
			entry.capture("", query, &resolved)
		}
		return entry, resolved, true
	}
	for _, entry := range consoleRoutes {
		if !entry.prefix || !strings.HasPrefix(suffix, entry.suffix) {
			continue
		}
		tail := strings.TrimPrefix(suffix, entry.suffix)
		if tail == "" {
			// The parameter is the route. /build/ carries no job id, and the
			// bundle's own table spells this path /build/:jobID, which matches
			// nothing without one — so serving the shell here answered three
			// paths with a router that matched no route and a blank page.
			// 404 is what the address actually is.
			continue
		}
		resolved.Name = entry.name
		entry.capture(tail, query, &resolved)
		return entry, resolved, true
	}
	return consoleRoute{}, webassets.Route{}, false
}

// mountSuffix returns the path under a mount point. "/legacy" and "/legacy/"
// are the same page; "/legacyish" is not under the mount at all, and saying so
// here means no caller has to think about it.
func mountSuffix(mount, path string) (string, bool) {
	if path == mount {
		return "/", true
	}
	if strings.HasPrefix(path, mount+"/") {
		return strings.TrimPrefix(path, mount), true
	}
	return "", false
}

// consoleAddress reduces a request path to the route it names, whichever mount
// it arrived on.
//
// Three addresses reach the same page: the console's own, the old console's
// under /legacy, and the /ui the console was built at and still forwards from.
// They are one vocabulary spelled three times, so one table answers for all
// three — otherwise a reader following a shared /ui/packages link meets a login
// redirect on the way to the page /packages serves without one, and the
// forwarding that was supposed to keep the old link working breaks it instead.
func consoleAddress(path string) string {
	for _, mount := range []string{legacyBase, consolePreviewBase} {
		if suffix, ok := mountSuffix(mount, path); ok {
			return suffix
		}
	}
	return path
}

// consolePathIsPublic reports whether a path names one of the community pages a
// reader may see without a session. authMiddleware asks this instead of
// carrying a second list of strings.
func consolePathIsPublic(path string) bool {
	entry, _, ok := matchConsoleRoute(consoleAddress(path), nil)
	return ok && entry.public
}

// handleConsole serves the shell for every console route. It is the mux's
// catch-all, so it is also what answers an address neither console names: the
// route table is the whole vocabulary, and a path outside it is a 404 rather
// than a shell whose router would match nothing.
func (d *Dashboard) handleConsole(w http.ResponseWriter, r *http.Request) {
	_, route, ok := matchConsoleRoute(r.URL.Path, r.URL.Query())
	if !ok {
		http.NotFound(w, r)
		return
	}
	if d.console == nil {
		writeConsoleNotBuilt(w)
		return
	}
	if err := d.console.RenderIndex(w, d.consoleBoot(r, route)); err != nil {
		// RenderIndex buffers the page, so an error here means nothing has been
		// written and a status is still ours to send.
		http.Error(w, "Failed to render console", http.StatusInternalServerError)
		log.Printf("Console shell error (%s): %v", route.Name, err)
	}
}

// handleConsoleLogin answers the one promoted path that is not only a page.
//
// /login is where the sign-in form posts its credentials — in this console and
// in the one it replaces, both of which spell that destination as a literal —
// so the POST stays exactly where it has always been and only the GET moves to
// the bundle. Handing the whole path to handleConsole would have answered a
// credential submission with a shell and left every deployment unable to sign
// anyone in.
func (d *Dashboard) handleConsoleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !d.config.AuthEnabled {
			// Nothing to log in to. Rendering the page anyway would offer a form
			// whose POST this dashboard answers with 404, which is what the old
			// console avoided here by the same redirect.
			http.Redirect(w, r, "/overview", http.StatusFound)
			return
		}
		d.handleConsole(w, r)
	case http.MethodPost:
		d.handleLoginSubmit(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleConsoleMoved forwards the address the console was built at.
//
// It resolves the trimmed path against the route table before sending anyone
// anywhere, which does two things. A path neither console names 404s here
// rather than by way of a redirect. And, because trimming "/ui" off
// "/ui//elsewhere.example" leaves a protocol-relative URL, the table is what
// keeps an attacker-chosen host out of Location.
//
// The redirect is temporary on purpose. The promotion is meant to be reversible
// by the route table rather than by a revert, and a permanent redirect is one a
// browser keeps honouring long after the route table stopped saying it.
func (d *Dashboard) handleConsoleMoved(w http.ResponseWriter, r *http.Request) {
	suffix, ok := mountSuffix(consolePreviewBase, r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	entry, _, matched := matchConsoleRoute(suffix, r.URL.Query())
	if !matched {
		http.NotFound(w, r)
		return
	}
	// Rebuilt from the table rather than forwarded: `suffix` is request path
	// with "/ui" trimmed off, and a trimmed path is still the caller's string.
	// Matching it proves a route accepted it, not that it is safe to put in
	// Location — "/ui//elsewhere.example" trims to a protocol-relative URL, and
	// the only reason it does not arrive here is that no route matches it, which
	// is a fact about today's table. Composing the answer out of the entry's own
	// constant suffix and an escaped parameter is a fact about this function.
	location := url.URL{Path: entry.suffix}
	if entry.prefix {
		location.Path += strings.TrimPrefix(suffix, entry.suffix)
	}
	// Re-encoded rather than forwarded, for the same reason as the path: what
	// arrives is the caller's spelling, and what leaves is this function's.
	// Only /device reads anything out of it, but a query dropped on the way to
	// the new address is a device code the reader has to type again.
	if r.URL.RawQuery != "" {
		location.RawQuery = r.URL.Query().Encode()
	}
	http.Redirect(w, r, location.String(), http.StatusFound)
}

// legacyRouter is the console in internal/dashboard/ui.go, moved aside.
//
// Its pages are registered at the addresses they were written for and reached
// through http.StripPrefix, because several of them read their own parameter
// off the path — handleBuildDetail trims "/build/" from it — and a second
// spelling of each route under the mount point would be a second place for that
// prefix to be wrong.
//
// Only pages are here. The old console's script calls /api/... and /logout by
// absolute path, and those are unmoved, so they keep answering the pages under
// this mount exactly as they answered them at the top level.
func (d *Dashboard) legacyRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", d.handleLanding)
	mux.HandleFunc("/login", d.handleLoginRoute)
	mux.HandleFunc("/device", d.handleDeviceAuthorizationPage)
	mux.HandleFunc("/overview", d.handleOverview)
	mux.HandleFunc("/builds", d.handleBuildsPage)
	mux.HandleFunc("/build/", d.handleBuildDetail)
	mux.HandleFunc("/logs/", d.handleBuildLogs)
	mux.HandleFunc("/monitor", d.handleBuildersMonitor)
	mux.HandleFunc("/image-factory", d.handleImageFactoryPage)
	mux.HandleFunc("/settings", d.handleSettingsPage)
	mux.HandleFunc("/packages", d.handlePackagesPage)
	mux.HandleFunc("/docs", d.handleDocs)
	mux.HandleFunc("/status", d.handlePublicStatusPage)
	mux.HandleFunc("/shell/", d.handleShellPage)
	return http.StripPrefix(legacyBase, mux)
}

// consoleAssetHandler serves the hashed bundle, or explains its absence.
func (d *Dashboard) consoleAssetHandler() http.Handler {
	if d.console == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeConsoleNotBuilt(w)
		})
	}
	return d.console.AssetHandler(consoleAssetPrefix)
}

// writeConsoleNotBuilt answers a request for a bundle this binary was compiled
// without. It names the command rather than 404ing, because the two look
// identical from a browser and only one of them is a build mistake.
func writeConsoleNotBuilt(w http.ResponseWriter) {
	http.Error(w,
		"console bundle not built: run `make web-build` before building this binary",
		http.StatusServiceUnavailable)
}

// consoleBoot assembles the payload the shell stamps into the page.
func (d *Dashboard) consoleBoot(r *http.Request, route webassets.Route) webassets.Boot {
	language := resolveLanguage(r)
	return webassets.Boot{
		Lang:              language,
		HTMLLang:          htmlLanguageTag(language),
		AuthEnabled:       d.config.AuthEnabled,
		OIDCEnabled:       d.oidcAvailable(),
		LocalLoginEnabled: d.config.AdminUser != "" && d.config.AdminPassword != "",
		IdentityProviders: d.consoleIdentityProviders(),
		Principal:         d.consolePrincipal(r),
		StepUpMethod:      d.stepUpMethod(r),
		Route:             route,
		AssetBase:         consoleAssetPrefix,
	}
}

// consoleIdentityProviders names the federated destinations this deployment can
// actually complete a sign-in through.
//
// It walks the configured list rather than the runtime map because a map has no
// order and the card would reshuffle its buttons between renders. A provider
// that failed discovery is absent from the runtime map and so is absent here:
// /auth/provider/<id>/start answers 404 for it, and an offered button that 404s
// is worse than no button.
//
// The list is empty for the pre-multi-provider configuration, whose one provider
// is synthesised at startup and is not in the configured list at all — so there
// is no name to offer and nothing to choose between. That is what leaves the
// bundle on /auth/oidc/start there, which is where pageData leaves the old
// console for the same reason.
func (d *Dashboard) consoleIdentityProviders() []webassets.IdentityProvider {
	// Never nil: the payload states one shape, and a JSON null would make the
	// absence of providers a second thing the bundle has to test for.
	providers := make([]webassets.IdentityProvider, 0, len(d.config.IdentityProviders))
	for _, provider := range d.config.IdentityProviders {
		if d.providers[provider.ID] == nil {
			continue
		}
		providers = append(providers, webassets.IdentityProvider{
			ID:          provider.ID,
			DisplayName: provider.DisplayName,
			// pageData drops a github button from a step-up card; the rule
			// travels here as a fact about the provider rather than as an
			// already-filtered list, because only one of the three pages that
			// build a step-up link is rendered by a request that says step_up.
			SupportsStepUp: provider.Type != "github",
		})
	}
	return providers
}

// consolePrincipal names the reader when the dashboard can do it without a
// network call, and says nothing when it cannot.
//
// A federated session's principal lives in the control plane, so naming it here
// would cost every page render an upstream round trip. Returning nil is what
// makes the bundle wait for /api/iam/me rather than render a name it guessed —
// and because the payload carries no capability either way, a destination that
// needs one stays hidden until IAM answers.
func (d *Dashboard) consolePrincipal(r *http.Request) *webassets.Principal {
	token := sessionToken(r)
	if token == "" || isFederatedSession(token) {
		return nil
	}
	claims, err := verifyTokenClaims(d.config.JWTSecret, token, time.Now())
	if err != nil {
		return nil
	}
	return &webassets.Principal{
		Subject:           claims.Subject,
		PreferredUsername: claims.Subject,
		Authentication:    "local-session",
	}
}
