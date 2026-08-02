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
	// consoleBase is where the ported console is mounted while ui.go still owns
	// the top-level paths. Both serve at once on purpose: the old console stays
	// the operator's console until the port is finished, and mounting the new
	// one beside it is what makes a ported page comparable against the page it
	// replaces, on the same deployment, without a flag day.
	consoleBase = "/ui"

	// consoleAssetPrefix has to equal `base` in web/vite.config.ts, because that
	// is the string the build wrote into every asset URL inside index.html.
	// TestBuiltBundleIsMountedAtTheAssetPrefixTheServerServes checks the two
	// against each other once a build exists.
	consoleAssetPrefix = "/static/ui/"
)

// consoleRoute is one entry in the console's route vocabulary. The table below
// is the whole vocabulary: a path that is not in it does not reach the bundle,
// and every fact the server can state about a route — its name, whether it is
// public, what the URL carries — is stated here rather than re-derived from the
// path's spelling at each place that needs it.
type consoleRoute struct {
	name string

	// suffix is the path under consoleBase.
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
func matchConsoleRoute(path string, query url.Values) (consoleRoute, webassets.Route, bool) {
	suffix, ok := consoleSuffix(path)
	if !ok {
		return consoleRoute{}, webassets.Route{}, false
	}
	resolved := webassets.Route{Path: path}
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
			// The parameter is the route. /ui/build/ carries no job id, and the
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

// consoleSuffix returns the path under consoleBase. "/ui" and "/ui/" are the
// same route; "/uixyz" is not a console path at all, and saying so here means no
// caller has to think about it.
func consoleSuffix(path string) (string, bool) {
	if path == consoleBase {
		return "/", true
	}
	if strings.HasPrefix(path, consoleBase+"/") {
		return strings.TrimPrefix(path, consoleBase), true
	}
	return "", false
}

// consolePathIsPublic reports whether a console path mirrors one of the old
// console's community pages. authMiddleware asks this instead of carrying a
// second list of strings.
func consolePathIsPublic(path string) bool {
	entry, _, ok := matchConsoleRoute(path, nil)
	return ok && entry.public
}

// handleConsole serves the shell for every console route.
func (d *Dashboard) handleConsole(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == consoleBase {
		// One canonical form, so a bookmark and a link do not produce two cache
		// entries for the same page.
		http.Redirect(w, r, consoleBase+"/", http.StatusMovedPermanently)
		return
	}
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
