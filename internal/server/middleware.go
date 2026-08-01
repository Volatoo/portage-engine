package server

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/slchris/portage-engine/internal/iam"
)

func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cache == nil || !strings.HasPrefix(r.URL.Path, "/api/v1/") ||
			(r.Method != http.MethodPost && r.Method != http.MethodPut &&
				r.Method != http.MethodPatch && r.Method != http.MethodDelete) {
			next.ServeHTTP(w, r)
			return
		}
		identity := stripPort(r.RemoteAddr)
		principal, authenticated := iam.PrincipalFromContext(r.Context())
		projectSelector := strings.TrimSpace(r.Header.Get("X-Project-ID"))
		if authenticated {
			identity = principal.SubjectID
			if identity == "" {
				identity = principal.Issuer + "#" + principal.Subject
			}
		}
		// Do not include the raw project selector in the bucket key: it has
		// not been authorized yet, so a caller could rotate fake selectors to
		// manufacture unbounded buckets. Durable per-project caps are enforced
		// later by PostgreSQL after project authorization.
		identity += "|" + r.URL.Path
		ctx, cancel := context.WithTimeout(r.Context(), 750*time.Millisecond)
		allowed, err := s.cache.Allow(ctx, identity,
			s.config.Cache.RateLimitPerMinute, s.config.Cache.RateLimitBurst)
		cancel()
		if err != nil {
			// Redis is acceleration, never the build correctness authority.
			// Fail open and let PostgreSQL idempotency/fencing protect writes.
			next.ServeHTTP(w, r)
			return
		}
		if !allowed {
			w.Header().Set("Retry-After", "1")
			if authenticated {
				s.auditRequest(r, principal, "request.rate_limit", "http_request",
					r.URL.Path, "", "denied", map[string]any{
						"project_selector": projectSelector,
						"limit_per_minute": s.config.Cache.RateLimitPerMinute,
						"burst":            s.config.Cache.RateLimitBurst,
					})
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit exceeded","code":"request_rate_limit"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticationRateLimitMiddleware bounds work performed before an identity
// has been authenticated. It uses a separate source-IP bucket from the
// authenticated subject bucket above, so invalid credentials cannot bypass
// protection while valid identities still receive their own write budget.
func (s *Server) authenticationRateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cache == nil || !authenticationRateLimitedRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		identity := "preauth|" + stripPort(r.RemoteAddr) + "|" + r.URL.Path
		ctx, cancel := context.WithTimeout(r.Context(), 750*time.Millisecond)
		allowed, err := s.cache.Allow(ctx, identity,
			s.config.Cache.RateLimitPerMinute, s.config.Cache.RateLimitBurst)
		cancel()
		if err != nil {
			// This layer mitigates abuse but is not an authorization or
			// correctness boundary. Preserve availability if Redis is down.
			next.ServeHTTP(w, r)
			return
		}
		if !allowed {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(
				`{"error":"authentication rate limit exceeded","code":"authentication_rate_limit"}`,
			))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func authenticationRateLimitedRequest(r *http.Request) bool {
	return r != nil &&
		r.Method != http.MethodOptions &&
		strings.HasPrefix(r.URL.Path, "/api/v1/") &&
		!publicControlPlanePath(r.URL.Path)
}

// responseWriter wraps http.ResponseWriter to capture the status code and bytes written.
type responseWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
	wroteHeader  bool
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK, // default if WriteHeader is never called
	}
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.statusCode = code
		rw.wroteHeader = true
		rw.ResponseWriter.WriteHeader(code)
	}
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.bytesWritten += n
	return n, err
}

// Hijack forwards to the underlying writer so WebSocket upgrades (web shell)
// work through the logging middleware.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	return h.Hijack()
}

// Flush forwards streaming responses such as SSE through the logging wrapper.
func (rw *responseWriter) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// requestIDMiddleware generates a unique request ID for each request and adds it
// to the response headers. If the client sends an X-Request-ID header, it is
// reused for request tracing across services (Server → Builder).
func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}

		// Set on response for client correlation
		w.Header().Set("X-Request-ID", requestID)

		// Store in request context via header so downstream handlers can read it
		r.Header.Set("X-Request-ID", requestID)

		next.ServeHTTP(w, r)
	})
}

// enhancedLoggingMiddleware provides structured request logging with:
// - Request ID for correlation
// - Response status code
// - Processing duration
// - Response body size
// - Goroutine count (for resource monitoring)
func (s *Server) enhancedLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap ResponseWriter to capture status code and bytes
		rw := newResponseWriter(w)

		// Process request
		next.ServeHTTP(rw, r)

		// Calculate duration
		duration := time.Since(start)
		requestID := r.Header.Get("X-Request-ID")

		// Update goroutine metric
		s.metrics.UpdateGoroutines(int64(runtime.NumGoroutine()))

		// Structured log output
		level := slog.LevelInfo
		if rw.statusCode >= 500 {
			level = slog.LevelError
		} else if rw.statusCode >= 400 {
			level = slog.LevelWarn
		}

		// Skip noisy health check logs unless they fail
		path := r.URL.Path
		if (path == "/health" || path == "/readyz" || path == "/livez") && rw.statusCode < 400 {
			return
		}

		slog.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", requestURIForLog(r),
			"status", rw.statusCode,
			"duration_ms", fmt.Sprintf("%.2f", float64(duration.Microseconds())/1000.0),
			"bytes", rw.bytesWritten,
			"remote", stripPort(r.RemoteAddr),
			"request_id", requestID,
		)
	})
}

func requestURIForLog(r *http.Request) string {
	if r == nil {
		return ""
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/iam/device/") {
		if r.URL.RawQuery != "" {
			return r.URL.Path + "?<redacted>"
		}
		return r.URL.Path
	}
	if !strings.HasPrefix(r.URL.Path, "/verify-binhost/") {
		return r.RequestURI
	}
	rest := strings.TrimPrefix(r.URL.Path, "/verify-binhost/")
	suffix := ""
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		suffix = rest[slash:]
	}
	return "/verify-binhost/<redacted>" + suffix
}

// stripPort removes the port from a remote address for cleaner logging.
func stripPort(addr string) string {
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		return addr[:idx]
	}
	return addr
}
