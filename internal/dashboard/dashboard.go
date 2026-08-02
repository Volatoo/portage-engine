// Package dashboard implements the web dashboard for monitoring build cluster.
package dashboard

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/slchris/portage-engine/internal/dashboard/webassets"
	"github.com/slchris/portage-engine/pkg/config"
)

//go:embed assets/xterm.min.js assets/xterm.css
var xtermAssets embed.FS

// appleCSSDigest is derived from the compiled-in stylesheet, so it changes
// exactly when the bytes do and stays stable across restarts and replicas
// (a per-process random tag would break revalidation behind a load balancer).
// It names the bytes for both consumers: the ETag below and the ?v= every
// page links the stylesheet with.
var appleCSSDigest = func() string {
	digest := sha256.Sum256([]byte(appleCSS))
	return hex.EncodeToString(digest[:16])
}()

var appleCSSETag = `"` + appleCSSDigest + `"`

// Dashboard represents the web dashboard.
type Dashboard struct {
	config       *config.DashboardConfig
	templates    *template.Template
	httpClient   *http.Client
	streamClient *http.Client
	oidc         *oidcRuntime
	providers    map[string]*oidcRuntime
	// console is the ported frontend, compiled in from web/dist. It is nil when
	// this binary was built without running the frontend build, which is a
	// normal state for `go build ./...` on a working copy and never a normal
	// state for a release; the routes under /ui say so rather than 404ing.
	console *webassets.Console
}

// ClusterStatus represents the overall cluster status.
type ClusterStatus struct {
	ActiveBuilds    int       `json:"active_builds"`
	QueuedBuilds    int       `json:"queued_builds"`
	ActiveInstances int       `json:"active_instances"`
	TotalBuilds     int       `json:"total_builds"`
	SuccessRate     float64   `json:"success_rate"`
	LastUpdated     time.Time `json:"last_updated"`
}

// New creates a new Dashboard instance.
func New(cfg *config.DashboardConfig) *Dashboard {
	tmpl := template.Must(template.New("landing").Parse(landingHTML))
	template.Must(tmpl.New("login").Parse(loginHTML))
	template.Must(tmpl.New("device").Parse(deviceAuthorizationHTML))
	template.Must(tmpl.New("overview").Parse(overviewHTML))
	template.Must(tmpl.New("builds").Parse(buildsPageHTML))
	template.Must(tmpl.New("build-detail").Parse(buildDetailHTML))
	template.Must(tmpl.New("logs").Parse(logsPageHTML))
	template.Must(tmpl.New("monitor").Parse(monitorHTML))
	template.Must(tmpl.New("image-factory").Parse(imageFactoryHTML))
	template.Must(tmpl.New("settings").Parse(settingsHTML))
	template.Must(tmpl.New("packages").Parse(packagesHTML))
	template.Must(tmpl.New("docs").Parse(docsHTML))
	template.Must(tmpl.New("status").Parse(statusHTML))
	template.Must(tmpl.New("shell").Parse(shellHTML))

	// The ported console is loaded once, not per request: it is compiled into
	// this binary, so a failure here is a build-time fact and repeating the
	// attempt on every request would only repeat the same answer.
	console, err := webassets.Load()
	if err != nil {
		log.Printf("Console bundle unavailable, /ui will report it: %v", err)
	}

	return &Dashboard{
		config:    cfg,
		templates: tmpl,
		console:   console,
		// httpClient carries control-plane JSON, where a wedged backend must
		// not pin a browser request open. streamClient carries the two bodies
		// with no bounded length — binary-package downloads and the job event
		// stream. Client.Timeout is a whole-request deadline, not an idle one,
		// so the 10s ceiling truncates a package download mid-transfer (the
		// server hashes the file before the first byte) and tears down an
		// EventSource every 10 seconds; those two are bounded by the caller's
		// request context instead.
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		streamClient: &http.Client{},
	}
}

// pageData is the payload every page template receives.
func (d *Dashboard) pageData(r *http.Request, extra map[string]interface{}) map[string]interface{} {
	providers := make([]identityProviderView, 0, len(d.providers))
	stepUp, _ := extra["StepUp"].(bool)
	returnTo, _ := extra["ReturnTo"].(string)
	for _, provider := range d.config.IdentityProviders {
		if stepUp && provider.Type == "github" {
			continue
		}
		if d.providers[provider.ID] != nil {
			loginURL := "/auth/provider/" + provider.ID + "/start"
			loginURL = loginURLWithContext(loginURL, stepUp, returnTo)
			providers = append(providers, identityProviderView{
				ID: provider.ID, DisplayName: provider.DisplayName,
				LoginURL: loginURL,
			})
		}
	}
	if len(providers) == 0 && d.legacySingleOIDC() {
		providers = append(providers, identityProviderView{
			ID: "oidc", DisplayName: "Identity provider",
			LoginURL: loginURLWithContext("/auth/oidc/start", stepUp, returnTo),
		})
	}
	language := resolveLanguage(r)
	data := map[string]interface{}{
		"AuthEnabled":       d.config.AuthEnabled,
		"OIDCEnabled":       len(providers) > 0,
		"IdentityProviders": providers,
		"LocalLoginEnabled": d.config.AdminUser != "" && d.config.AdminPassword != "",
		// Lang / HTMLLang let the template emit the right strings and <html
		// lang> in the first paint, instead of shipping English and letting
		// client-side i18n rewrite it a frame later.
		"Lang":     language,
		"HTMLLang": htmlLanguageTag(language),
	}
	for k, v := range extra {
		data[k] = v
	}
	return data
}

// legacySingleOIDC reports the pre-multi-provider configuration: one identity
// provider, unnamed, reachable at /auth/oidc/start.
func (d *Dashboard) legacySingleOIDC() bool {
	return d.oidc != nil || d.config.OIDCEnabled
}

// oidcAvailable reports whether any federated sign-in exists, for callers that
// want the boolean without assembling the provider list. pageData above is the
// one caller that needs the list itself; both answers come from these same two
// facts, so a deployment cannot offer a provider on one console and not the
// other.
func (d *Dashboard) oidcAvailable() bool {
	for _, provider := range d.config.IdentityProviders {
		if d.providers[provider.ID] != nil {
			return true
		}
	}
	return d.legacySingleOIDC()
}

func loginURLWithContext(path string, stepUp bool, returnTo string) string {
	query := url.Values{}
	if stepUp {
		query.Set("step_up", "1")
	}
	if returnTo != "" {
		query.Set("return_to", returnTo)
	}
	if encoded := query.Encode(); encoded != "" {
		return path + "?" + encoded
	}
	return path
}

func safeReturnTo(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || raw == "" || !strings.HasPrefix(raw, "/") ||
		strings.HasPrefix(raw, "//") || parsed.IsAbs() || parsed.Host != "" {
		return ""
	}
	return parsed.RequestURI()
}

// renderPage executes a page template with the standard payload.
func (d *Dashboard) renderPage(
	w http.ResponseWriter, r *http.Request, name string, extra map[string]interface{},
) {
	// The rendered language depends on the request, so a shared cache must not
	// hand a Chinese page to the next English reader.
	w.Header().Add("Vary", "Cookie, Accept-Language")
	if err := d.templates.ExecuteTemplate(w, name, d.pageData(r, extra)); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Template error (%s): %v", name, err)
	}
}

// languageCookie records the reader's explicit language choice so the server
// can pick it before the first byte of HTML. It is deliberately not HttpOnly:
// the page's own toggle reads and writes it, and it carries no authority.
const languageCookie = "pe_lang"

// resolveLanguage picks the page language server-side: an explicit choice
// (cookie) first, then what the browser asked for, then English.
func resolveLanguage(r *http.Request) string {
	if r == nil {
		return "en"
	}
	if cookie, err := r.Cookie(languageCookie); err == nil {
		if language := normalizeLanguage(cookie.Value); language != "" {
			return language
		}
	}
	if language := normalizeLanguage(r.Header.Get("Accept-Language")); language != "" {
		return language
	}
	return "en"
}

// normalizeLanguage reduces a cookie value or an Accept-Language list to one of
// the two languages ui.go ships strings for, or "" when neither is requested.
func normalizeLanguage(raw string) string {
	for _, tag := range strings.Split(raw, ",") {
		tag = strings.ToLower(strings.TrimSpace(strings.SplitN(tag, ";", 2)[0]))
		switch {
		case tag == "zh" || strings.HasPrefix(tag, "zh-"):
			return "zh"
		case tag == "en" || strings.HasPrefix(tag, "en-"):
			return "en"
		}
	}
	return ""
}

// htmlLanguageTag maps the internal language key to the BCP 47 tag that belongs
// in <html lang>.
func htmlLanguageTag(language string) string {
	if language == "zh" {
		return "zh-CN"
	}
	return "en"
}

// handleLanguagePreference persists the reader's language choice. The toggle
// posts here so the next navigation is already rendered in that language; the
// endpoint touches no backend and is public for the same reason the pages it
// serves are.
func (d *Dashboard) handleLanguagePreference(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var preference struct {
		Lang string `json:"lang"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&preference); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	language := normalizeLanguage(preference.Lang)
	if language == "" {
		http.Error(w, "unsupported language", http.StatusBadRequest)
		return
	}
	// #nosec G124 -- HTTP is an explicit trusted-LAN mode; this cookie is a
	// display preference and is readable by the page's toggle by design.
	http.SetCookie(w, &http.Cookie{
		Name: languageCookie, Value: language, Path: "/",
		MaxAge: int((365 * 24 * time.Hour).Seconds()),
		Secure: d.secureCookie(r), SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"lang": language})
}

// Router returns the HTTP router for the dashboard.
func (d *Dashboard) Router() http.Handler {
	mux := http.NewServeMux()

	// Web interface
	mux.HandleFunc("/", d.handleLanding)
	mux.HandleFunc("/login", d.handleLoginRoute)
	mux.HandleFunc("/logout", d.handleLogout)
	mux.HandleFunc("/device", d.handleDeviceAuthorizationPage)
	mux.HandleFunc("/auth/oidc/start", d.handleOIDCStart)
	mux.HandleFunc("/auth/oidc/callback", d.handleOIDCCallback)
	mux.HandleFunc("/auth/provider/", d.handleIdentityProviderRoute)
	mux.HandleFunc("/auth/step-up", d.handleLocalStepUp)
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

	// API endpoints
	mux.HandleFunc("/api/status", d.handleStatus)
	mux.HandleFunc("/api/public/binhosts", d.handlePublicBinhosts)
	mux.HandleFunc("/api/public/packages", d.handlePublicPackages)
	mux.HandleFunc("/api/public/status", d.handlePublicStatus)
	mux.HandleFunc("/api/preferences/language", d.handleLanguagePreference)
	mux.HandleFunc("/api/settings/cloud", d.handleCloudSettingsProxy)
	mux.HandleFunc("/api/settings/cloud/test", d.handleCloudSettingsTestProxy)
	mux.HandleFunc("/api/builds", d.handleBuilds)
	mux.HandleFunc("/api/builds/submit", d.handleBuildSubmitProxy)
	mux.HandleFunc("/api/builds/delete", d.handleBuildDeleteProxy)
	mux.HandleFunc("/api/builds/cancel", d.handleBuildCancelProxy)
	mux.HandleFunc("/api/builds/retry", d.handleBuildRetryProxy)
	mux.HandleFunc("/api/builds/cleanup-failed", d.handleBuildsCleanupFailedProxy)
	mux.HandleFunc("/api/builds/detail", d.handleBuildDetailAPI)
	mux.HandleFunc("/api/builds/logs", d.handleBuildLogsAPI)
	mux.HandleFunc("/api/instances", d.handleInstances)
	mux.HandleFunc("/api/scheduler/status", d.handleSchedulerStatus)
	mux.HandleFunc("/api/worker-gateway/status", d.handleWorkerGatewayStatus)
	mux.HandleFunc("/api/worker-gateway/identities", d.handleWorkloadIdentityInventory)
	mux.HandleFunc("/api/builders/status", d.handleBuildersStatusAPI)
	mux.HandleFunc("/api/ledger/status", d.handleLedgerStatusAPI)
	mux.HandleFunc("/api/runtime-metadata/status", d.handleRuntimeMetadataStatusAPI)
	mux.HandleFunc("/api/cache/status", d.handleCacheStatusAPI)
	mux.HandleFunc("/api/events/jobs", d.handleJobEventsProxy)
	mux.HandleFunc("/api/image-factory/status", d.handleImageFactoryStatusProxy)
	mux.HandleFunc("/api/iam/me", d.handleIAMMeProxy)
	mux.HandleFunc("/api/iam/sessions", d.handleIAMSessionsProxy)
	mux.HandleFunc("/api/iam/sessions/revoke-all", d.handleIAMRevokeAllProxy)
	mux.HandleFunc("/api/iam/device/decision", d.handleIAMDeviceDecisionProxy)
	mux.HandleFunc("/api/projects/policy", d.handleProjectPolicyProxy)

	// Key management endpoints
	mux.HandleFunc("/api/gpg/status", d.handleGPGStatusProxy)
	mux.HandleFunc("/api/keys/public", d.handlePublicKeyAPI)
	mux.HandleFunc("/api/keys/download", d.handleDownloadKeyAPI)
	mux.HandleFunc("/api/keys/info", d.handleKeyInfoAPI)

	// Artifact download endpoints (proxy through server)
	mux.HandleFunc("/api/artifacts/download/", d.handleArtifactDownload)
	mux.HandleFunc("/api/artifacts/info/", d.handleArtifactInfo)

	// Static files
	// Binhost artifact downloads (proxied so the operator can fetch artifacts
	// from the detail page without direct server access).
	mux.HandleFunc("/binpkgs/", d.handleBinpkgProxy)

	// Web shell: page + websocket bridge to the server's SSH session.
	mux.HandleFunc("/shell/", d.handleShellPage)
	mux.HandleFunc("/api/shell", d.handleShellProxy)
	mux.HandleFunc("/api/shell/preflight", d.handleShellPreflight)
	mux.HandleFunc("/static/xterm.js", func(w http.ResponseWriter, _ *http.Request) {
		data, _ := xtermAssets.ReadFile("assets/xterm.min.js")
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/static/xterm.css", func(w http.ResponseWriter, _ *http.Request) {
		data, _ := xtermAssets.ReadFile("assets/xterm.css")
		w.Header().Set("Content-Type", "text/css")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(data)
	})

	mux.HandleFunc("/static/apple.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		// Every page requests this with a ?v= query, so the URL itself is the
		// cache key and the bytes behind it never change: immutable here, a
		// bumped ?v= on the page when the stylesheet changes. The ETag still
		// answers a revalidation (an old ?v= in a bookmark) with a 304.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("ETag", appleCSSETag)
		http.ServeContent(w, r, "apple.css", time.Time{}, strings.NewReader(appleCSS))
	})
	// The ported console. It is mounted beside the old one rather than over it:
	// /ui/... serves the Vite bundle's shell, /static/ui/... serves its hashed
	// assets. The longer pattern wins in ServeMux, so /static/ui/ is answered
	// here and never falls through to the on-disk static handler below.
	mux.Handle(consoleAssetPrefix, d.consoleAssetHandler())
	mux.HandleFunc(consoleBase, d.handleConsole)
	mux.HandleFunc(consoleBase+"/", d.handleConsole)

	mux.HandleFunc("/static/", d.handleStatic)

	// Apply middleware
	var handler http.Handler = mux
	if d.config.AuthEnabled {
		handler = d.authMiddleware(handler)
	}
	handler = d.loggingMiddleware(handler)

	return handler
}

// sessionCookie carries the signed JWT so plain page navigations (which never
// send an Authorization header) authenticate too. HttpOnly keeps it away from
// page scripts; API fetches send it automatically (same-origin).
const (
	sessionCookie = "pe_session"
	stepUpCookie  = "pe_step_up"
)

type localStepUpSession struct {
	SessionDigest string `json:"session_digest"`
	Expires       int64  `json:"expires"`
}

// handleLanding serves the public landing page.
func (d *Dashboard) handleLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	d.renderPage(w, r, "landing", nil)
}

// handleLoginRoute renders the login page (GET) and issues a session (POST).
func (d *Dashboard) handleLoginRoute(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !d.config.AuthEnabled {
			// Nothing to log in to — go straight to the console.
			http.Redirect(w, r, "/overview", http.StatusFound)
			return
		}
		d.renderPage(w, r, "login", map[string]interface{}{
			"StepUp":   r.URL.Query().Get("step_up") == "1",
			"ReturnTo": safeReturnTo(r.URL.Query().Get("return_to")),
		})
	case http.MethodPost:
		d.handleLoginSubmit(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleLoginSubmit validates operator credentials, issues a signed JWT, and
// sets it both as an HttpOnly session cookie (for page navigation) and in the
// response body (for API clients).
func (d *Dashboard) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	// When authentication is disabled there is nothing to log in to.
	if !d.config.AuthEnabled {
		http.Error(w, "authentication is disabled", http.StatusNotFound)
		return
	}

	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Constant-time credential check against the configured admin account.
	userOK := subtle.ConstantTimeCompare([]byte(creds.Username), []byte(d.config.AdminUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(creds.Password), []byte(d.config.AdminPassword)) == 1
	if d.config.AdminUser == "" || d.config.AdminPassword == "" || !userOK || !passOK {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	ttl := time.Duration(d.config.TokenTTLMinutes) * time.Minute
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	token, err := signToken(d.config.JWTSecret, creds.Username, time.Now(), ttl)
	if err != nil {
		http.Error(w, "failed to issue token", http.StatusInternalServerError)
		return
	}

	// #nosec G124 -- Secure follows direct TLS or the explicit reverse-proxy
	// COOKIE_SECURE setting; trusted-LAN HTTP remains an operator opt-in.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   d.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"token":       token,
		"user":        creds.Username,
		"redirect_to": safeReturnTo(r.URL.Query().Get("return_to")),
	})
}

func (d *Dashboard) handleDeviceAuthorizationPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	d.renderPage(w, r, "device", map[string]interface{}{
		"UserCode": strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("user_code"))),
	})
}

func (d *Dashboard) handleIAMDeviceDecisionProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	d.proxyServer(w, r, http.MethodPost,
		strings.TrimRight(d.config.ServerURL, "/")+"/api/v1/iam/device/decision")
}

// handleLogout clears the session cookie and returns to the landing page.
func (d *Dashboard) handleLogout(w http.ResponseWriter, r *http.Request) {
	d.revokeBackendOIDCSession(r)
	// #nosec G124 -- Secure follows direct TLS or COOKIE_SECURE.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   d.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
	// #nosec G124 -- HTTP is an explicit trusted-LAN mode; HttpOnly and SameSite remain enforced.
	http.SetCookie(w, &http.Cookie{
		Name: stepUpCookie, Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, Secure: d.secureCookie(r),
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (d *Dashboard) handleLocalStepUp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || d.config.ServerStepUpAPIKey == "" {
		http.Error(w, "local step-up is unavailable", http.StatusNotFound)
		return
	}
	session, err := r.Cookie(sessionCookie)
	if err != nil || strings.HasPrefix(session.Value, "oidc.") ||
		strings.HasPrefix(session.Value, "federated.") {
		http.Error(w, "local administrator session required", http.StatusUnauthorized)
		return
	}
	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credentials); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	userOK := subtle.ConstantTimeCompare(
		[]byte(credentials.Username), []byte(d.config.AdminUser),
	) == 1
	passOK := subtle.ConstantTimeCompare(
		[]byte(credentials.Password), []byte(d.config.AdminPassword),
	) == 1
	if d.config.AdminUser == "" || d.config.AdminPassword == "" ||
		!userOK || !passOK {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	expires := time.Now().Add(10 * time.Minute)
	digest := sha256.Sum256([]byte(session.Value))
	value, err := d.seal("local-step-up", localStepUpSession{
		SessionDigest: hex.EncodeToString(digest[:]), Expires: expires.Unix(),
	})
	if err != nil {
		http.Error(w, "failed to establish step-up session", http.StatusInternalServerError)
		return
	}
	// #nosec G124 -- HTTP is an explicit trusted-LAN mode; HttpOnly and SameSite remain enforced.
	http.SetCookie(w, &http.Cookie{
		Name: stepUpCookie, Value: value, Path: "/",
		MaxAge: 600, HttpOnly: true, Secure: d.secureCookie(r),
		SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"step_up": true, "expires_at": expires.UTC(),
	})
}

func (d *Dashboard) validLocalStepUp(r *http.Request, sessionValue string) bool {
	if d.config.ServerStepUpAPIKey == "" {
		return false
	}
	cookie, err := r.Cookie(stepUpCookie)
	if err != nil {
		return false
	}
	var elevated localStepUpSession
	if err := d.unseal("local-step-up", cookie.Value, &elevated); err != nil ||
		time.Now().Unix() >= elevated.Expires {
		return false
	}
	digest := sha256.Sum256([]byte(sessionValue))
	return subtle.ConstantTimeCompare(
		[]byte(elevated.SessionDigest),
		[]byte(hex.EncodeToString(digest[:])),
	) == 1
}

func (d *Dashboard) revokeBackendOIDCSession(r *http.Request) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || (!strings.HasPrefix(cookie.Value, "oidc.") &&
		!strings.HasPrefix(cookie.Value, "federated.")) {
		return
	}
	token, err := d.oidcTokenFromSession(r.Context(), cookie.Value)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx, http.MethodDelete,
		strings.TrimRight(d.config.ServerURL, "/")+"/api/v1/iam/sessions",
		nil,
	)
	if err != nil {
		return
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := d.httpClient.Do(request)
	if err == nil {
		_ = response.Body.Close()
	}
}

func (d *Dashboard) secureCookie(r *http.Request) bool {
	return d.config.CookieSecure || r.TLS != nil
}

// handleOverview serves the authed overview page.
func (d *Dashboard) handleOverview(w http.ResponseWriter, r *http.Request) {
	d.renderPage(w, r, "overview", nil)
}

// handleSettingsPage serves the cloud settings management page.
func (d *Dashboard) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	d.renderPage(w, r, "settings", nil)
}

func (d *Dashboard) handleImageFactoryPage(w http.ResponseWriter, r *http.Request) {
	d.renderPage(w, r, "image-factory", nil)
}

func (d *Dashboard) handleImageFactoryStatusProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(w, r, http.MethodGet, d.config.ServerURL+"/api/v1/image-factory/status")
}

// handleCloudSettingsProxy forwards GET/PUT /api/settings/cloud to the server.
func (d *Dashboard) handleCloudSettingsProxy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPut:
		d.proxyServer(w, r, r.Method, d.config.ServerURL+"/api/v1/settings/cloud")
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleBuildSubmitProxy forwards a build submission to the server (used by
// the settings page's full-pipeline test build). It targets the bundle-less
// request-build endpoint: /api/v1/builds/submit requires a full Portage
// ConfigBundle, which a quick UI-triggered test build doesn't carry.
func (d *Dashboard) handleBuildSubmitProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(w, r, http.MethodPost, d.config.ServerURL+"/api/v1/packages/request-build")
}

// handleBuildDeleteProxy forwards a finished-job deletion to the server.
func (d *Dashboard) handleBuildDeleteProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(w, r, http.MethodDelete, d.config.ServerURL+"/api/v1/builds/delete?job_id="+url.QueryEscape(r.URL.Query().Get("job_id")))
}

// handleBuildCancelProxy requests cooperative cancellation. The database
// removes the active lease immediately, so a stale executor cannot publish.
func (d *Dashboard) handleBuildCancelProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target := d.config.ServerURL + "/api/v1/builds/cancel?job_id=" +
		url.QueryEscape(r.URL.Query().Get("job_id"))
	if reason := strings.TrimSpace(r.URL.Query().Get("reason")); reason != "" {
		target += "&reason=" + url.QueryEscape(reason)
	}
	d.proxyServer(w, r, http.MethodPost, target)
}

// handleBuildRetryProxy creates a new fenced attempt without changing job
// identity or losing the original attempt audit trail.
func (d *Dashboard) handleBuildRetryProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(w, r, http.MethodPost, d.config.ServerURL+"/api/v1/builds/retry?job_id="+
		url.QueryEscape(r.URL.Query().Get("job_id")))
}

// handleBuildsCleanupFailedProxy forwards the bulk failed-job cleanup.
func (d *Dashboard) handleBuildsCleanupFailedProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(w, r, http.MethodPost, d.config.ServerURL+"/api/v1/builds/cleanup-failed")
}

// handleGPGStatusProxy forwards the signing status query.
func (d *Dashboard) handleGPGStatusProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(w, r, http.MethodGet, d.config.ServerURL+"/api/v1/gpg/status")
}

// handleShellPage renders the web-shell page for an instance.
func (d *Dashboard) handleShellPage(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimPrefix(r.URL.Path, "/shell/")
	d.renderPage(w, r, "shell", map[string]interface{}{"InstanceID": instanceID})
}

var shellUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Same-origin only: the session cookie is SameSite=Lax (not sent on
	// cross-site WS handshakes), but reject foreign Origins outright so a
	// hostile page can never ride an authenticated shell session.
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return false
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	},
}

// shellStepUpState reports whether the control plane would accept a shell
// handshake from this caller right now and, when it would not, which
// authentication fixes it ("federated", "local") or that none can
// ("unavailable"). A refused WebSocket handshake reaches page script as a bare
// error event with no status, so the same decision has to be answerable over
// plain HTTP before the socket is opened.
func (d *Dashboard) shellStepUpState(r *http.Request) (bool, string) {
	session := sessionToken(r)
	if isFederatedSession(session) {
		// Only the control plane can answer for a federated principal: its
		// step-up is a property of the platform token's own auth_time, AMR and
		// ACR, none of which the dashboard ever sees.
		resp, err := d.serverGet(
			strings.TrimRight(d.config.ServerURL, "/")+"/api/v1/iam/me", r,
		)
		if err != nil {
			return false, "federated"
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false, "federated"
		}
		var identity struct {
			Principal struct {
				StepUp bool `json:"step_up"`
			} `json:"principal"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).
			Decode(&identity); err != nil {
			return false, "federated"
		}
		return identity.Principal.StepUp, "federated"
	}
	// A local session travels upstream as SERVER_API_KEY, whose step-up is the
	// independent SERVER_STEP_UP_API_KEY the dashboard attaches for ten minutes
	// after an administrator re-enters their password. Without that key, without
	// the credentials to re-enter, or without a session to bind the elevation
	// to, no request this dashboard makes can ever satisfy the route: say so,
	// rather than leaving the reader with a socket that closes silently.
	if d.config.ServerStepUpAPIKey == "" || session == "" ||
		d.config.AdminUser == "" || d.config.AdminPassword == "" {
		return false, "unavailable"
	}
	return d.validLocalStepUp(r, session), "local"
}

// handleShellPreflight lets the shell page establish the step-up credential
// before it opens the terminal socket, and answers in the shape the page's
// elevation branch reads.
func (d *Dashboard) handleShellPreflight(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	satisfied, method := d.shellStepUpState(r)
	if satisfied {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"step_up": true, "method": method,
		})
		return
	}
	code, message := "step_up_required", "fresh step-up authentication required"
	if method == "unavailable" {
		code = "step_up_unavailable"
		message = "this dashboard holds no step-up credential for the web shell"
	}
	w.WriteHeader(http.StatusPreconditionRequired)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": message, "code": code, "method": method,
	})
}

// handleShellProxy bridges the browser's WebSocket to the server's SSH shell
// endpoint, attaching the caller's credential (browsers cannot set headers on
// a WS handshake). The credential mapping is the shared one, so the step-up
// the control plane demands on this route travels with the handshake; a
// federated session that fails to resolve is refused rather than widened to
// the legacy admin key.
func (d *Dashboard) handleShellProxy(w http.ResponseWriter, r *http.Request) {
	instanceID := r.URL.Query().Get("id")
	if instanceID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	serverWS := strings.Replace(d.config.ServerURL, "http://", "ws://", 1)
	serverWS = strings.Replace(serverWS, "https://", "wss://", 1)
	hdr := http.Header{}
	d.applyBackendAuthHeaders(r, hdr)
	upstream, resp, err := websocket.DefaultDialer.Dial(serverWS+"/api/v1/instances/shell?id="+url.QueryEscape(instanceID), hdr)
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		status := http.StatusBadGateway
		if resp != nil {
			status = resp.StatusCode
		}
		http.Error(w, "shell unavailable: "+err.Error(), status)
		return
	}
	defer func() { _ = upstream.Close() }()

	client, err := shellUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()

	done := make(chan struct{}, 2)
	pipe := func(dst, src *websocket.Conn) {
		for {
			mt, data, err := src.ReadMessage()
			if err != nil {
				break
			}
			if err := dst.WriteMessage(mt, data); err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go pipe(upstream, client)
	go pipe(client, upstream)
	<-done
}

// handleBinpkgProxy streams a binhost artifact through the dashboard.
func (d *Dashboard) handleBinpkgProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyPublicRead(w, r, strings.TrimRight(d.config.ServerURL, "/")+r.URL.Path)
}

// handleCloudSettingsTestProxy forwards POST /api/settings/cloud/test.
func (d *Dashboard) handleCloudSettingsTestProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(w, r, http.MethodPost, d.config.ServerURL+"/api/v1/settings/cloud/test")
}

// proxyServer forwards a request (with body) to the backend server, attaching
// the server API key, and relays status + body back honestly.
func (d *Dashboard) proxyServer(w http.ResponseWriter, r *http.Request, method, url string) {
	d.proxyServerVia(d.httpClient, w, r, method, url)
}

// proxyServerVia is proxyServer with an explicit client, so endpoints whose
// response body is open-ended (SSE) can opt out of the control-plane deadline.
func (d *Dashboard) proxyServerVia(
	client *http.Client, w http.ResponseWriter, r *http.Request, method, url string,
) {
	// #nosec G704 -- url is assembled only from the startup-validated,
	// operator-controlled SERVER_URL and fixed API paths.
	req, err := http.NewRequestWithContext(r.Context(), method, url, r.Body)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	d.applyBackendAuth(r, req)
	if projectID := strings.TrimSpace(r.Header.Get("X-Project-ID")); projectID != "" {
		req.Header.Set("X-Project-ID", projectID)
	}
	// #nosec G704 -- the request target is the validated backend origin above.
	resp, err := client.Do(req)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = io.Copy(flushingWriter{writer: w, flusher: flusher}, resp.Body)
			return
		}
	}
	_, _ = io.Copy(w, resp.Body)
}

type flushingWriter struct {
	writer  io.Writer
	flusher http.Flusher
}

func (w flushingWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.flusher.Flush()
	return n, err
}

// handleStatus returns the cluster status.
func (d *Dashboard) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Query the server for current status
	status, err := d.fetchClusterStatus(r)
	if err != nil {
		writeBackendError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (d *Dashboard) handlePublicPackages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := url.Values{}
	for _, name := range []string{"q", "profile_id", "arch", "limit", "offset"} {
		if value := r.URL.Query().Get(name); value != "" {
			query.Set(name, value)
		}
	}
	target := strings.TrimRight(d.config.ServerURL, "/") + "/api/v1/public/packages"
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	d.proxyPublicRead(w, r, target)
}

func (d *Dashboard) handlePublicBinhosts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyPublicRead(
		w, r,
		strings.TrimRight(d.config.ServerURL, "/")+"/api/v1/binhosts",
	)
}

func (d *Dashboard) handlePublicStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyPublicRead(
		w, r,
		strings.TrimRight(d.config.ServerURL, "/")+"/api/v1/public/status",
	)
}

// proxyPublicRead calls one of the backend's explicitly public read-only
// endpoints without forwarding cookies, bearer tokens, API keys, or project
// identity.
func (d *Dashboard) proxyPublicRead(w http.ResponseWriter, r *http.Request, target string) {
	// #nosec G704 -- target is assembled from the startup-validated backend
	// origin and fixed public paths in the handlers above.
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	// #nosec G704 -- see target construction above. This path streams whole
	// binary packages through /binpkgs/, so it uses the deadline-free client.
	resp, err := d.streamClient.Do(req)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, name := range []string{
		"Content-Type", "Content-Length", "Content-Disposition",
		"Cache-Control", "ETag", "Last-Modified",
	} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// The status line is already on the wire, so a short body is the only
	// remaining signal: log it, and leave the forwarded Content-Length unmet so
	// net/http closes the connection instead of letting a truncated package
	// look like a complete download.
	if copied, copyErr := io.Copy(w, resp.Body); copyErr != nil {
		log.Printf("Public proxy body truncated after %d bytes for %q: %v", // #nosec G706 -- value is safely quoted.
			copied, target, copyErr)
	}
}

// handleBuilds returns the list of builds from the server.
func (d *Dashboard) handleBuilds(w http.ResponseWriter, r *http.Request) {
	// Get limit parameter (default 50, max 200)
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = parsed
			if limit > 200 {
				limit = 200
			}
		}
	}

	// Query the server for build list. On failure, report the outage honestly
	// rather than fabricating sample builds (which would hide a real outage).
	url := fmt.Sprintf("%s/api/v1/builds/list?limit=%d", d.config.ServerURL, limit)
	resp, err := d.serverGet(url, r)
	if err != nil {
		log.Printf("Failed to query builds: %v", err)
		writeBackendError(w, err)
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// Forward the response from server
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleInstances returns the list of active instances.
func (d *Dashboard) handleInstances(w http.ResponseWriter, r *http.Request) {
	resp, err := d.serverGet(d.config.ServerURL+"/api/v1/instances", r)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleBuildsPage serves the builds list page.
func (d *Dashboard) handleBuildsPage(w http.ResponseWriter, r *http.Request) {
	d.renderPage(w, r, "builds", nil)
}

// handleBuildDetail serves the build detail page.
func (d *Dashboard) handleBuildDetail(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimPrefix(r.URL.Path, "/build/")
	d.renderPage(w, r, "build-detail", map[string]interface{}{"JobID": jobID})
}

// handleBuildLogs serves the real-time build logs page.
func (d *Dashboard) handleBuildLogs(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimPrefix(r.URL.Path, "/logs/")
	d.renderPage(w, r, "logs", map[string]interface{}{"JobID": jobID})
}

// handleBuildDetailAPI returns detailed information about a specific build.
func (d *Dashboard) handleBuildDetailAPI(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "job_id required", http.StatusBadRequest)
		return
	}

	url := fmt.Sprintf("%s/api/v1/builds/status?job_id=%s", d.config.ServerURL, jobID)
	resp, err := d.serverGet(url, r)
	if err != nil {
		log.Printf("Failed to query build detail: %v", err)
		writeBackendError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleBuildLogsAPI returns build logs.
func (d *Dashboard) handleBuildLogsAPI(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "job_id required", http.StatusBadRequest)
		return
	}

	url := fmt.Sprintf("%s/api/v1/builds/logs?job_id=%s", d.config.ServerURL, jobID)
	resp, err := d.serverGet(url, r)
	if err != nil {
		log.Printf("Failed to query build logs: %v", err)
		writeBackendError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleBuildersMonitor serves the builders status monitor page.
func (d *Dashboard) handleBuildersMonitor(w http.ResponseWriter, r *http.Request) {
	d.renderPage(w, r, "monitor", nil)
}

// handleDocs serves the documentation page.
func (d *Dashboard) handleDocs(w http.ResponseWriter, r *http.Request) {
	d.renderPage(w, r, "docs", nil)
}

func (d *Dashboard) handlePackagesPage(w http.ResponseWriter, r *http.Request) {
	d.renderPage(w, r, "packages", nil)
}

func (d *Dashboard) handlePublicStatusPage(w http.ResponseWriter, r *http.Request) {
	d.renderPage(w, r, "status", nil)
}

// fetchServerPublicKey retrieves the server's real GPG public key (armored).
func (d *Dashboard) fetchServerPublicKey(r *http.Request) (string, error) {
	resp, err := d.serverGet(d.config.ServerURL+"/api/v1/gpg/public-key", r)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	key, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(key), nil
}

// handlePublicKeyAPI returns the server's real GPG public key.
func (d *Dashboard) handlePublicKeyAPI(w http.ResponseWriter, r *http.Request) {
	key, err := d.fetchServerPublicKey(r)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"public_key": key})
}

// handleDownloadKeyAPI serves the server's real GPG public key as a download.
func (d *Dashboard) handleDownloadKeyAPI(w http.ResponseWriter, r *http.Request) {
	key, err := d.fetchServerPublicKey(r)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/pgp-keys")
	w.Header().Set("Content-Disposition", `attachment; filename="portage-engine.asc"`)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(key)))
	_, _ = w.Write([]byte(key))
}

// handleKeyInfoAPI returns whether a signing key is available on the server.
// It reports the real key's presence rather than fabricated metadata.
func (d *Dashboard) handleKeyInfoAPI(w http.ResponseWriter, r *http.Request) {
	key, err := d.fetchServerPublicKey(r)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"available":  key != "",
		"format":     "OpenPGP (armored)",
		"public_key": key,
		"usage":      "Package signing and verification",
	})
}

// handleBuildersStatusAPI returns builders status from the server.
func (d *Dashboard) handleBuildersStatusAPI(w http.ResponseWriter, r *http.Request) {
	url := fmt.Sprintf("%s/api/v1/builders/status", d.config.ServerURL)
	resp, err := d.serverGet(url, r)
	if err != nil {
		log.Printf("Failed to query builders status: %v", err)
		writeBackendError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleSchedulerStatus returns scheduler and task assignment status.
func (d *Dashboard) handleSchedulerStatus(w http.ResponseWriter, r *http.Request) {
	url := fmt.Sprintf("%s/api/v1/scheduler/status", d.config.ServerURL)
	resp, err := d.serverGet(url, r)
	if err != nil {
		log.Printf("Failed to query scheduler status: %v", err)
		writeBackendError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (d *Dashboard) handleWorkerGatewayStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(w, r, http.MethodGet, d.config.ServerURL+"/api/v1/worker-gateway/status")
}

func (d *Dashboard) handleWorkloadIdentityInventory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(
		w, r, http.MethodGet,
		d.config.ServerURL+"/api/v1/worker-gateway/identities",
	)
}

// handleLedgerStatusAPI exposes the server's low-cardinality DB-1 ledger
// health without leaking request payloads, job IDs, or package names.
func (d *Dashboard) handleLedgerStatusAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp, err := d.serverGet(d.config.ServerURL+"/api/v1/ledger/status", r)
	if err != nil {
		log.Printf("Failed to query job ledger status: %v", err)
		writeBackendError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (d *Dashboard) handleRuntimeMetadataStatusAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp, err := d.serverGet(d.config.ServerURL+"/api/v1/runtime-metadata/status", r)
	if err != nil {
		log.Printf("Failed to query runtime metadata status: %v", err)
		writeBackendError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (d *Dashboard) handleCacheStatusAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(w, r, http.MethodGet, d.config.ServerURL+"/api/v1/cache/status")
}

func (d *Dashboard) handleJobEventsProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target := d.config.ServerURL + "/api/v1/events/jobs"
	if projectID := strings.TrimSpace(r.URL.Query().Get("project_id")); projectID != "" {
		target += "?project_id=" + url.QueryEscape(projectID)
	}
	// The event stream never ends on its own: it runs until the browser closes
	// the EventSource, so it cannot share the deadline-bearing client.
	d.proxyServerVia(d.streamClient, w, r, http.MethodGet, target)
}

// handleStatic serves static files.
func (d *Dashboard) handleStatic(w http.ResponseWriter, r *http.Request) {
	// Define the static files root directory
	staticRoot := "./static"

	// Extract the requested path and remove the /static/ prefix
	requestPath := strings.TrimPrefix(r.URL.Path, "/static/")

	// Clean the path to prevent directory traversal attacks
	requestPath = filepath.Clean(requestPath)

	// Prevent accessing files outside the static directory
	// Check for any attempt to traverse up (..)
	if strings.Contains(requestPath, "..") || strings.HasPrefix(requestPath, "/") {
		http.Error(w, "Forbidden", http.StatusForbidden)
		log.Printf("Blocked path traversal attempt: %q", r.URL.Path) // #nosec G706 -- value is safely quoted.
		return
	}

	// Construct the full file path
	fullPath := filepath.Join(staticRoot, requestPath)

	// Verify the resolved path is still within staticRoot
	absStaticRoot, err := filepath.Abs(staticRoot)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		log.Printf("Failed to resolve static root: %v", err)
		return
	}

	absFullPath, err := filepath.Abs(fullPath)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		log.Printf("Failed to resolve file path: %v", err)
		return
	}

	// Ensure the file is within the static directory
	if !strings.HasPrefix(absFullPath, absStaticRoot) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		log.Printf("Blocked access outside static directory: %q -> %q", r.URL.Path, absFullPath) // #nosec G706 -- values are safely quoted.
		return
	}

	// Serve the file
	http.ServeFile(w, r, fullPath)
}

// handleArtifactInfo returns artifact metadata for a job (proxied through server).
func (d *Dashboard) handleArtifactInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract job ID from URL
	jobID := strings.TrimPrefix(r.URL.Path, "/api/artifacts/info/")
	if jobID == "" {
		http.Error(w, "Job ID required", http.StatusBadRequest)
		return
	}

	// Proxy request to server
	infoURL := fmt.Sprintf("%s/api/v1/artifacts/info/%s", d.config.ServerURL, jobID)
	resp, err := d.serverGet(infoURL, r)
	if err != nil {
		log.Printf("Failed to get artifact info: %v", err)
		http.Error(w, fmt.Sprintf("Failed to contact server: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		http.Error(w, string(body), resp.StatusCode)
		return
	}

	// Forward response
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.Copy(w, resp.Body)
}

// handleArtifactDownload proxies artifact download requests to the server.
func (d *Dashboard) handleArtifactDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract job ID from URL
	jobID := strings.TrimPrefix(r.URL.Path, "/api/artifacts/download/")
	if jobID == "" {
		http.Error(w, "Job ID required", http.StatusBadRequest)
		return
	}

	// Proxy request to server
	downloadURL := fmt.Sprintf("%s/api/v1/artifacts/download/%s", d.config.ServerURL, jobID)
	resp, err := d.serverGet(downloadURL, r)
	if err != nil {
		log.Printf("Failed to download artifact: %v", err)
		http.Error(w, fmt.Sprintf("Failed to contact server: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		http.Error(w, string(body), resp.StatusCode)
		return
	}

	// Forward headers (Content-Type, Content-Disposition, Content-Length)
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Stream the file
	_, _ = io.Copy(w, resp.Body)
}

// fetchClusterStatus fetches cluster status from the server.
func (d *Dashboard) fetchClusterStatus(r *http.Request) (*ClusterStatus, error) {
	resp, err := d.serverGet(fmt.Sprintf("%s/api/v1/cluster/status", d.config.ServerURL), r)
	if err != nil {
		// Surface the outage to the caller instead of returning fabricated
		// "healthy" numbers that would mask a backend outage.
		log.Printf("Failed to fetch cluster status: %v", err)
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("server returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var status ClusterStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}

	return &status, nil
}

// authMiddleware verifies the session on every request except the public
// pages (landing, login, static assets). The token is taken from the
// Authorization header (API clients) or the session cookie (browser page
// navigation). Unauthenticated page requests are redirected to the login page;
// API requests get a plain 401.
func (d *Dashboard) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public: community pages and their intentionally narrow read APIs.
		if r.URL.Path == "/" || r.URL.Path == "/login" || r.URL.Path == "/logout" ||
			r.URL.Path == "/packages" || r.URL.Path == "/docs" ||
			r.URL.Path == "/status" ||
			r.URL.Path == "/api/public/binhosts" ||
			r.URL.Path == "/api/public/packages" ||
			r.URL.Path == "/api/public/status" ||
			// A display preference on pages that are themselves public.
			r.URL.Path == "/api/preferences/language" ||
			r.URL.Path == "/auth/oidc/start" || r.URL.Path == "/auth/oidc/callback" ||
			strings.HasPrefix(r.URL.Path, "/auth/provider/") ||
			strings.HasPrefix(r.URL.Path, "/binpkgs/") ||
			strings.HasPrefix(r.URL.Path, "/static/") ||
			// The ported console's community pages. The answer comes from the
			// route table in console.go, so this list and that one cannot come
			// to disagree about which pages a reader without a session may see.
			consolePathIsPublic(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		token := sessionToken(r)

		// Allow anonymous access if enabled and no token was presented.
		if d.config.AllowAnonymous && token == "" {
			next.ServeHTTP(w, r)
			return
		}

		if token == "" || d.verifyDashboardSession(r.Context(), token) != nil {
			if isPageRequest(r) {
				returnTo := safeReturnTo(r.URL.RequestURI())
				target := "/login"
				if returnTo != "" {
					target += "?return_to=" + url.QueryEscape(returnTo)
				}
				http.Redirect(w, r, target, http.StatusFound)
				return
			}
			http.Error(w, "Unauthorized: invalid or expired token", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isPageRequest reports whether the request is a browser page navigation (as
// opposed to a JSON API call), so auth failures can redirect instead of 401.
func isPageRequest(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		!strings.HasPrefix(r.URL.Path, "/api/") &&
		strings.Contains(r.Header.Get("Accept"), "text/html")
}

// writeBackendError reports that the backend server is unreachable/unhealthy as
// an honest HTTP 502, instead of returning fabricated data that would make an
// outage look like a healthy cluster.
func writeBackendError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   "backend server unreachable",
		"details": err.Error(),
	})
}

// serverGet issues a GET to the backend server, attaching the configured
// server API key so the dashboard works against a secured server.
func (d *Dashboard) serverGet(url string, incoming ...*http.Request) (*http.Response, error) {
	ctx := context.Background()
	if len(incoming) > 0 && incoming[0] != nil {
		ctx = incoming[0].Context()
	}
	// #nosec G704 -- callers build this URL from the startup-validated,
	// operator-controlled SERVER_URL and fixed API paths.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if len(incoming) > 0 && incoming[0] != nil {
		d.applyBackendAuth(incoming[0], req)
	} else if d.config.ServerAPIKey != "" {
		req.Header.Set("X-API-Key", d.config.ServerAPIKey)
	}
	if len(incoming) > 0 && incoming[0] != nil {
		if projectID := strings.TrimSpace(incoming[0].Header.Get("X-Project-ID")); projectID != "" {
			req.Header.Set("X-Project-ID", projectID)
		}
	}
	// #nosec G704 -- the request target is the validated backend origin above.
	return d.httpClient.Do(req)
}

func (d *Dashboard) handleIAMMeProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(w, r, http.MethodGet, d.config.ServerURL+"/api/v1/iam/me")
}

func (d *Dashboard) handleProjectPolicyProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(w, r, r.Method, d.config.ServerURL+"/api/v1/projects/policy")
}

func (d *Dashboard) handleIAMSessionsProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target := d.config.ServerURL + "/api/v1/iam/sessions"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	d.proxyServer(w, r, r.Method, target)
}

func (d *Dashboard) handleIAMRevokeAllProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(
		w, r, r.Method,
		d.config.ServerURL+"/api/v1/iam/sessions/revoke-all",
	)
}

// extractBearer returns the token from an "Authorization: Bearer <token>"
// header, or the raw header value if it has no Bearer prefix (backward compat).
func extractBearer(header string) string {
	if header == "" {
		return ""
	}
	if strings.HasPrefix(header, "Bearer ") {
		return strings.TrimPrefix(header, "Bearer ")
	}
	return header
}

// sessionToken resolves the caller's dashboard session, header before cookie.
// Everything that decides what to send upstream must resolve it through here:
// when authMiddleware accepted a header-borne session but the credential
// mapper only looked at the cookie, the request authenticated as a user and
// then travelled on as SERVER_API_KEY, which the control plane maps to a
// system administrator.
func sessionToken(r *http.Request) string {
	if token := extractBearer(r.Header.Get("Authorization")); token != "" {
		return token
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		return cookie.Value
	}
	return ""
}

// isFederatedSession reports whether a session carries a control-plane
// principal of its own. Such a session must always travel upstream as that
// principal's bearer — never as SERVER_API_KEY, which would promote any
// signed-in viewer to the legacy system administrator.
func isFederatedSession(token string) bool {
	return strings.HasPrefix(token, "oidc.") || strings.HasPrefix(token, "federated.")
}

// loggingMiddleware provides request logging.
func (d *Dashboard) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI := r.RequestURI
		if r.URL.Path == "/device" || strings.Contains(r.URL.RawQuery, "user_code") {
			requestURI = r.URL.Path + "?<redacted>"
		}
		log.Printf("%s %q %q", r.Method, requestURI, r.RemoteAddr) // #nosec G706 -- values are safely quoted.
		next.ServeHTTP(w, r)
	})
}
