// Package dashboard implements the web dashboard for monitoring build cluster.
package dashboard

import (
	"crypto/subtle"
	"embed"
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

	"github.com/slchris/portage-engine/pkg/config"
)

//go:embed assets/xterm.min.js assets/xterm.css
var xtermAssets embed.FS

// Dashboard represents the web dashboard.
type Dashboard struct {
	config     *config.DashboardConfig
	templates  *template.Template
	httpClient *http.Client
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
	template.Must(tmpl.New("overview").Parse(overviewHTML))
	template.Must(tmpl.New("builds").Parse(buildsPageHTML))
	template.Must(tmpl.New("build-detail").Parse(buildDetailHTML))
	template.Must(tmpl.New("logs").Parse(logsPageHTML))
	template.Must(tmpl.New("monitor").Parse(monitorHTML))
	template.Must(tmpl.New("image-factory").Parse(imageFactoryHTML))
	template.Must(tmpl.New("settings").Parse(settingsHTML))
	template.Must(tmpl.New("docs").Parse(docsHTML))
	template.Must(tmpl.New("shell").Parse(shellHTML))

	return &Dashboard{
		config:     cfg,
		templates:  tmpl,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// pageData is the payload every page template receives.
func (d *Dashboard) pageData(extra map[string]interface{}) map[string]interface{} {
	data := map[string]interface{}{
		"AuthEnabled": d.config.AuthEnabled,
	}
	for k, v := range extra {
		data[k] = v
	}
	return data
}

// renderPage executes a page template with the standard payload.
func (d *Dashboard) renderPage(w http.ResponseWriter, name string, extra map[string]interface{}) {
	if err := d.templates.ExecuteTemplate(w, name, d.pageData(extra)); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Template error (%s): %v", name, err)
	}
}

// Router returns the HTTP router for the dashboard.
func (d *Dashboard) Router() http.Handler {
	mux := http.NewServeMux()

	// Web interface
	mux.HandleFunc("/", d.handleLanding)
	mux.HandleFunc("/login", d.handleLoginRoute)
	mux.HandleFunc("/logout", d.handleLogout)
	mux.HandleFunc("/overview", d.handleOverview)
	mux.HandleFunc("/builds", d.handleBuildsPage)
	mux.HandleFunc("/build/", d.handleBuildDetail)
	mux.HandleFunc("/logs/", d.handleBuildLogs)
	mux.HandleFunc("/monitor", d.handleBuildersMonitor)
	mux.HandleFunc("/image-factory", d.handleImageFactoryPage)
	mux.HandleFunc("/settings", d.handleSettingsPage)
	mux.HandleFunc("/docs", d.handleDocs)

	// API endpoints
	mux.HandleFunc("/api/status", d.handleStatus)
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
	mux.HandleFunc("/api/builders/status", d.handleBuildersStatusAPI)
	mux.HandleFunc("/api/ledger/status", d.handleLedgerStatusAPI)
	mux.HandleFunc("/api/runtime-metadata/status", d.handleRuntimeMetadataStatusAPI)
	mux.HandleFunc("/api/cache/status", d.handleCacheStatusAPI)
	mux.HandleFunc("/api/events/jobs", d.handleJobEventsProxy)
	mux.HandleFunc("/api/image-factory/status", d.handleImageFactoryStatusProxy)

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

	mux.HandleFunc("/static/apple.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte(appleCSS))
	})
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
const sessionCookie = "pe_session"

// handleLanding serves the public landing page.
func (d *Dashboard) handleLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	d.renderPage(w, "landing", nil)
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
		d.renderPage(w, "login", nil)
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

	// #nosec G124 -- Secure follows the actual transport so deployments on the
	// explicitly supported trusted-LAN HTTP mode can still authenticate.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"token": token,
		"user":  creds.Username,
	})
}

// handleLogout clears the session cookie and returns to the landing page.
func (d *Dashboard) handleLogout(w http.ResponseWriter, r *http.Request) {
	// #nosec G124 -- Secure follows the actual transport for trusted-LAN HTTP.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleOverview serves the authed overview page.
func (d *Dashboard) handleOverview(w http.ResponseWriter, _ *http.Request) {
	d.renderPage(w, "overview", nil)
}

// handleSettingsPage serves the cloud settings management page.
func (d *Dashboard) handleSettingsPage(w http.ResponseWriter, _ *http.Request) {
	d.renderPage(w, "settings", nil)
}

func (d *Dashboard) handleImageFactoryPage(w http.ResponseWriter, _ *http.Request) {
	d.renderPage(w, "image-factory", nil)
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
	d.renderPage(w, "shell", map[string]interface{}{"InstanceID": instanceID})
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

// handleShellProxy bridges the browser's WebSocket to the server's SSH shell
// endpoint, attaching the server API key (browsers cannot send it on a WS).
func (d *Dashboard) handleShellProxy(w http.ResponseWriter, r *http.Request) {
	instanceID := r.URL.Query().Get("id")
	if instanceID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	serverWS := strings.Replace(d.config.ServerURL, "http://", "ws://", 1)
	serverWS = strings.Replace(serverWS, "https://", "wss://", 1)
	hdr := http.Header{}
	if d.config.ServerAPIKey != "" {
		hdr.Set("X-API-Key", d.config.ServerAPIKey)
	}
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
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.proxyServer(w, r, http.MethodGet, d.config.ServerURL+r.URL.Path)
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
	if d.config.ServerAPIKey != "" {
		req.Header.Set("X-API-Key", d.config.ServerAPIKey)
	}
	// #nosec G704 -- the request target is the validated backend origin above.
	resp, err := d.httpClient.Do(req)
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
func (d *Dashboard) handleStatus(w http.ResponseWriter, _ *http.Request) {
	// Query the server for current status
	status, err := d.fetchClusterStatus()
	if err != nil {
		writeBackendError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
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
	resp, err := d.serverGet(url)
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
	_, _ = io.Copy(w, resp.Body)
}

// handleInstances returns the list of active instances.
func (d *Dashboard) handleInstances(w http.ResponseWriter, _ *http.Request) {
	resp, err := d.serverGet(d.config.ServerURL + "/api/v1/instances")
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
func (d *Dashboard) handleBuildsPage(w http.ResponseWriter, _ *http.Request) {
	d.renderPage(w, "builds", nil)
}

// handleBuildDetail serves the build detail page.
func (d *Dashboard) handleBuildDetail(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimPrefix(r.URL.Path, "/build/")
	d.renderPage(w, "build-detail", map[string]interface{}{"JobID": jobID})
}

// handleBuildLogs serves the real-time build logs page.
func (d *Dashboard) handleBuildLogs(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimPrefix(r.URL.Path, "/logs/")
	d.renderPage(w, "logs", map[string]interface{}{"JobID": jobID})
}

// handleBuildDetailAPI returns detailed information about a specific build.
func (d *Dashboard) handleBuildDetailAPI(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		http.Error(w, "job_id required", http.StatusBadRequest)
		return
	}

	url := fmt.Sprintf("%s/api/v1/builds/status?job_id=%s", d.config.ServerURL, jobID)
	resp, err := d.serverGet(url)
	if err != nil {
		log.Printf("Failed to query build detail: %v", err)
		writeBackendError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	w.Header().Set("Content-Type", "application/json")
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
	resp, err := d.serverGet(url)
	if err != nil {
		log.Printf("Failed to query build logs: %v", err)
		writeBackendError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	w.Header().Set("Content-Type", "application/json")
	_, _ = io.Copy(w, resp.Body)
}

// handleBuildersMonitor serves the builders status monitor page.
func (d *Dashboard) handleBuildersMonitor(w http.ResponseWriter, _ *http.Request) {
	d.renderPage(w, "monitor", nil)
}

// handleDocs serves the documentation page.
func (d *Dashboard) handleDocs(w http.ResponseWriter, _ *http.Request) {
	d.renderPage(w, "docs", nil)
}

// fetchServerPublicKey retrieves the server's real GPG public key (armored).
func (d *Dashboard) fetchServerPublicKey() (string, error) {
	resp, err := d.serverGet(d.config.ServerURL + "/api/v1/gpg/public-key")
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
func (d *Dashboard) handlePublicKeyAPI(w http.ResponseWriter, _ *http.Request) {
	key, err := d.fetchServerPublicKey()
	if err != nil {
		writeBackendError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"public_key": key})
}

// handleDownloadKeyAPI serves the server's real GPG public key as a download.
func (d *Dashboard) handleDownloadKeyAPI(w http.ResponseWriter, _ *http.Request) {
	key, err := d.fetchServerPublicKey()
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
func (d *Dashboard) handleKeyInfoAPI(w http.ResponseWriter, _ *http.Request) {
	key, err := d.fetchServerPublicKey()
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
func (d *Dashboard) handleBuildersStatusAPI(w http.ResponseWriter, _ *http.Request) {
	url := fmt.Sprintf("%s/api/v1/builders/status", d.config.ServerURL)
	resp, err := d.serverGet(url)
	if err != nil {
		log.Printf("Failed to query builders status: %v", err)
		writeBackendError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	w.Header().Set("Content-Type", "application/json")
	_, _ = io.Copy(w, resp.Body)
}

// handleSchedulerStatus returns scheduler and task assignment status.
func (d *Dashboard) handleSchedulerStatus(w http.ResponseWriter, _ *http.Request) {
	url := fmt.Sprintf("%s/api/v1/scheduler/status", d.config.ServerURL)
	resp, err := d.serverGet(url)
	if err != nil {
		log.Printf("Failed to query scheduler status: %v", err)
		writeBackendError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	w.Header().Set("Content-Type", "application/json")
	_, _ = io.Copy(w, resp.Body)
}

// handleLedgerStatusAPI exposes the server's low-cardinality DB-1 ledger
// health without leaking request payloads, job IDs, or package names.
func (d *Dashboard) handleLedgerStatusAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp, err := d.serverGet(d.config.ServerURL + "/api/v1/ledger/status")
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
	resp, err := d.serverGet(d.config.ServerURL + "/api/v1/runtime-metadata/status")
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
	d.proxyServer(w, r, http.MethodGet, d.config.ServerURL+"/api/v1/events/jobs")
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
	resp, err := d.serverGet(infoURL)
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
	resp, err := d.serverGet(downloadURL)
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
func (d *Dashboard) fetchClusterStatus() (*ClusterStatus, error) {
	resp, err := d.serverGet(fmt.Sprintf("%s/api/v1/cluster/status", d.config.ServerURL))
	if err != nil {
		// Surface the outage to the caller instead of returning fabricated
		// "healthy" numbers that would mask a backend outage.
		log.Printf("Failed to fetch cluster status: %v", err)
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

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
		// Public: landing, login, logout, and static assets.
		if r.URL.Path == "/" || r.URL.Path == "/login" || r.URL.Path == "/logout" ||
			strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}

		token := extractBearer(r.Header.Get("Authorization"))
		if token == "" {
			if c, err := r.Cookie(sessionCookie); err == nil {
				token = c.Value
			}
		}

		// Allow anonymous access if enabled and no token was presented.
		if d.config.AllowAnonymous && token == "" {
			next.ServeHTTP(w, r)
			return
		}

		if token == "" || verifyToken(d.config.JWTSecret, token, time.Now()) != nil {
			if isPageRequest(r) {
				http.Redirect(w, r, "/login", http.StatusFound)
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
func (d *Dashboard) serverGet(url string) (*http.Response, error) {
	// #nosec G704 -- callers build this URL from the startup-validated
	// operator-controlled SERVER_URL and fixed API paths.
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if d.config.ServerAPIKey != "" {
		req.Header.Set("X-API-Key", d.config.ServerAPIKey)
	}
	// #nosec G704 -- the request target is the validated backend origin above.
	return d.httpClient.Do(req)
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

// loggingMiddleware provides request logging.
func (d *Dashboard) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %q %q", r.Method, r.RequestURI, r.RemoteAddr) // #nosec G706 -- values are safely quoted.
		next.ServeHTTP(w, r)
	})
}
