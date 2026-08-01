package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/slchris/portage-engine/internal/iam"
	"github.com/slchris/portage-engine/internal/persistence"
	"github.com/slchris/portage-engine/pkg/config"
)

var projectNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

const (
	deviceAuthorizationTTL      = 10 * time.Minute
	deviceAuthorizationInterval = 5
	deviceGrantType             = "urn:ietf:params:oauth:grant-type:device_code"
)

const deviceUserCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

type deviceAuthorizationStore interface {
	CreateDeviceAuthorization(
		context.Context, string, string, int, int,
	) (bool, error)
	DecideDeviceAuthorization(
		context.Context, string, iam.Principal, bool,
		persistence.IAMSessionPolicy,
	) (persistence.DeviceAuthorization, error)
	PollDeviceAuthorization(
		context.Context, string, string, persistence.IAMSessionPolicy,
	) (persistence.DeviceAuthorizationPoll, error)
}

func (s *Server) initIAM() error {
	s.identityVerifiers = make(map[string]iam.IdentityVerifier)
	s.oidcVerifiers = make(map[string]iam.Verifier)
	s.providerIssuers = make(map[string]string)
	s.providerConfigs = make(map[string]config.IdentityProviderConfig)
	s.identityAdmins = make(map[string]struct{})
	if s.database != nil {
		s.iamRepository = persistence.NewIAMRepository(s.database)
		s.deviceAuthorizations = s.iamRepository
	}
	mode := s.authMode()
	if mode == iam.ModeLegacy {
		return nil
	}
	if s.iamRepository == nil {
		return fmt.Errorf("%s authentication requires a compatible PostgreSQL IAM schema", mode)
	}
	providers, legacyProvider := s.configuredIdentityProviders()
	for _, provider := range providers {
		if err := s.initializeIdentityProvider(provider, legacyProvider); err != nil {
			return err
		}
	}
	return s.configureIdentityAdmins(providers)
}

func (s *Server) configuredIdentityProviders() ([]config.IdentityProviderConfig, bool) {
	providers := append([]config.IdentityProviderConfig(nil), s.config.IdentityProviders...)
	if len(providers) > 0 {
		return providers, false
	}
	return []config.IdentityProviderConfig{{
		ID: "oidc", Type: "oidc", DisplayName: "OpenID Connect",
		IssuerURL: s.config.OIDCIssuerURL, Audience: s.config.OIDCAudience,
		ClientID:          s.config.OIDCAudience,
		AllowInsecureHTTP: s.config.OIDCAllowInsecureHTTP,
	}}, true
}

func (s *Server) initializeIdentityProvider(
	provider config.IdentityProviderConfig,
	legacyProvider bool,
) error {
	if !legacyProvider {
		if err := provider.Validate(); err != nil {
			return fmt.Errorf("identity provider %s: %w", provider.ID, err)
		}
	}
	s.providerConfigs[provider.ID] = provider
	if provider.Type == "github" {
		verifier, err := iam.NewGitHubVerifier(
			provider.ID, provider.ClientID, provider.ClientSecret,
			provider.APIBaseURL, nil,
		)
		if err != nil {
			return fmt.Errorf("initialize identity provider %s: %w", provider.ID, err)
		}
		s.identityVerifiers[provider.ID] = verifier
		s.providerIssuers[provider.ID] = "https://github.com"
		return nil
	}
	verifier, err := iam.NewOIDCProviderVerifier(
		context.Background(), provider.ID, provider.IssuerURL,
		provider.Audience, provider.AllowInsecureHTTP,
	)
	if err != nil {
		return fmt.Errorf("initialize identity provider %s: %w", provider.ID, err)
	}
	if err := validateBackchannelCapabilities(provider, verifier.Capabilities()); err != nil {
		return err
	}
	s.identityVerifiers[provider.ID] = verifier
	s.oidcVerifiers[provider.ID] = verifier
	s.providerIssuers[provider.ID] = verifier.Capabilities().Issuer
	if legacyProvider {
		s.oidcVerifier = verifier
	}
	return nil
}

func validateBackchannelCapabilities(
	provider config.IdentityProviderConfig,
	capabilities iam.ProviderCapabilities,
) error {
	if provider.BackchannelLogout && !capabilities.BackchannelLogoutSupported {
		return fmt.Errorf(
			"identity provider %s does not advertise OIDC back-channel logout",
			provider.ID,
		)
	}
	if provider.BackchannelRequireSID && !capabilities.BackchannelSessionSupported {
		return fmt.Errorf(
			"identity provider %s does not advertise session-bound back-channel logout",
			provider.ID,
		)
	}
	return nil
}

func (s *Server) configureIdentityAdmins(
	providers []config.IdentityProviderConfig,
) error {
	for _, configured := range s.config.IdentityAdminSubjects {
		providerID, subject, ok := strings.Cut(strings.TrimSpace(configured), ":")
		issuer := s.providerIssuers[providerID]
		if !ok || issuer == "" || strings.TrimSpace(subject) == "" {
			return fmt.Errorf("AUTH_ADMIN_IDENTITIES entry %q must be provider-id:subject", configured)
		}
		s.identityAdmins[identityAdminKey(issuer, subject)] = struct{}{}
	}
	if len(providers) == 1 {
		issuer := s.providerIssuers[providers[0].ID]
		for _, subject := range s.config.OIDCAdminSubjects {
			subject = strings.TrimSpace(subject)
			if subject != "" {
				s.identityAdmins[identityAdminKey(issuer, subject)] = struct{}{}
			}
		}
	}
	return nil
}

func identityAdminKey(issuer, subject string) string {
	return strings.TrimSpace(issuer) + "\x00" + strings.TrimSpace(subject)
}

func (s *Server) authMode() string {
	mode := strings.ToLower(strings.TrimSpace(s.config.AuthMode))
	if mode == "" {
		return iam.ModeLegacy
	}
	return mode
}

func publicControlPlanePath(path string) bool {
	return path == "/readyz" || path == "/livez" ||
		path == "/metrics" || path == "/metrics/prometheus" ||
		path == "/api/v1/iam/exchange" ||
		path == "/api/v1/iam/device/authorization" ||
		path == "/api/v1/iam/device/token" ||
		backchannelProviderID(path) != "" ||
		path == "/api/v1/binhosts" ||
		path == "/api/v1/gpg/public-key" ||
		path == "/api/v1/public/packages" ||
		path == "/api/v1/public/status" ||
		strings.HasPrefix(path, "/binpkgs/") ||
		strings.HasPrefix(path, "/verify-binhost/")
}

func backchannelProviderID(path string) string {
	const prefix = "/api/v1/iam/providers/"
	const suffix = "/backchannel-logout"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return ""
	}
	providerID := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if providerID == "" || strings.Contains(providerID, "/") {
		return ""
	}
	return providerID
}

func systemAdminPath(path string) bool {
	prefixes := []string{
		"/api/v1/settings/", "/api/v1/instances", "/api/v1/builders/",
		"/api/v1/scheduler/", "/api/v1/worker-gateway/",
		"/api/v1/ledger/", "/api/v1/image-factory/", "/api/v1/runtime-metadata/",
		"/api/v1/cache/", "/api/v1/artifacts/", "/api/v1/heartbeat",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return path == "/api/v1/builds/cleanup-failed" ||
		path == "/api/v1/gpg/generate" ||
		path == "/health"
}

func stepUpRequired(r *http.Request) bool {
	if r == nil || r.Method == http.MethodOptions {
		return false
	}
	// The web shell is a GET, but it opens an interactive SSH session on a
	// build instance whose default account is root. It is the one read-method
	// route that outranks every write below, so it is decided before the
	// read-method exemption.
	if r.URL.Path == "/api/v1/instances/shell" {
		return true
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return false
	}
	switch r.URL.Path {
	case "/api/v1/projects":
		return r.Method == http.MethodPost
	case "/api/v1/projects/members":
		return r.Method == http.MethodPut || r.Method == http.MethodDelete
	case "/api/v1/projects/policy":
		return r.Method == http.MethodPut
	case "/api/v1/builds/delete", "/api/v1/iam/sessions/revoke-all":
		return true
	case "/api/v1/iam/device/decision":
		// Approving a CLI device code mints a second, independently expiring
		// session credential. It must not cost less than revoking one.
		return true
	case "/api/v1/heartbeat":
		return false
	default:
		return systemAdminPath(r.URL.Path)
	}
}

func (s *Server) authenticateRequest(r *http.Request) (iam.Principal, error) {
	mode := s.authMode()
	apiKey := strings.TrimSpace(r.Header.Get("X-API-Key"))
	bearer := extractBearerToken(r.Header.Get("Authorization"))

	legacyAllowed := mode == iam.ModeLegacy || mode == iam.ModeHybrid
	if legacyAllowed && s.config.APIKey != "" {
		candidate := apiKey
		if candidate == "" {
			candidate = bearer
		}
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(s.config.APIKey)) == 1 {
			return iam.Principal{
				Issuer: "portage-engine", Subject: "legacy-api-key",
				Authentication: "legacy-api-key", SystemAdmin: true,
			}, nil
		}
	}

	// Database-disabled standalone compatibility mode preserves the historical
	// unauthenticated behavior only when AUTH_MODE remains explicitly legacy.
	if mode == iam.ModeLegacy && s.config.APIKey == "" {
		return iam.Principal{
			Issuer: "portage-engine", Subject: "anonymous-compatibility",
			Authentication: "anonymous-compatibility", SystemAdmin: true,
		}, nil
	}
	if bearer == "" || s.iamRepository == nil {
		return iam.Principal{}, fmt.Errorf("valid bearer identity required")
	}
	if strings.HasPrefix(bearer, "pe1_") {
		policy := s.iamSessionPolicy()
		authorizeCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		principal, err := s.iamRepository.AuthorizePlatformSession(
			authorizeCtx, bearer, policy,
		)
		cancel()
		if err != nil {
			return iam.Principal{}, err
		}
		principal.ProviderID = s.providerIDForIssuer(principal.Issuer)
		principal.SystemAdmin = s.isIdentityAdmin(principal.Issuer, principal.Subject)
		principal.StepUp = s.oidcStepUpSatisfied(principal, time.Now())
		return principal, nil
	}
	if s.oidcVerifier == nil {
		return iam.Principal{}, fmt.Errorf("external provider token must be exchanged for a Portage Engine session")
	}
	identity, err := s.oidcVerifier.Verify(r.Context(), bearer)
	if err != nil {
		return iam.Principal{}, err
	}
	return s.authenticateOIDCIdentity(r.Context(), identity)
}

func (s *Server) providerIDForIssuer(issuer string) string {
	match := ""
	for providerID, candidate := range s.providerIssuers {
		if candidate != issuer {
			continue
		}
		if match != "" {
			return ""
		}
		match = providerID
	}
	return match
}

func (s *Server) authenticateOIDCIdentity(
	ctx context.Context,
	identity iam.ExternalIdentity,
) (iam.Principal, error) {
	systemAdmin := s.isIdentityAdmin(identity.Issuer, identity.Subject)
	authorizeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	principal, err := s.iamRepository.ObserveSubject(authorizeCtx, identity, systemAdmin)
	if err != nil {
		return iam.Principal{}, err
	}
	principal, err = s.iamRepository.AuthorizeSession(
		authorizeCtx, principal, identity, s.iamSessionPolicy(),
	)
	if err != nil {
		return iam.Principal{}, err
	}
	principal.StepUp = s.oidcStepUpSatisfied(principal, time.Now())
	return principal, nil
}

func (s *Server) iamSessionPolicy() persistence.IAMSessionPolicy {
	idleMinutes := s.config.OIDCSessionIdleMinutes
	if idleMinutes <= 0 {
		idleMinutes = 60
	}
	maxMinutes := s.config.OIDCSessionMaxMinutes
	if maxMinutes <= 0 {
		maxMinutes = 720
	}
	return persistence.IAMSessionPolicy{
		IdleTimeout: time.Duration(idleMinutes) * time.Minute,
		MaxLifetime: time.Duration(maxMinutes) * time.Minute,
	}
}

func (s *Server) isIdentityAdmin(issuer, subject string) bool {
	_, ok := s.identityAdmins[identityAdminKey(issuer, subject)]
	return ok
}

func (s *Server) oidcStepUpSatisfied(principal iam.Principal, now time.Time) bool {
	if (principal.Authentication != "oidc" &&
		principal.Authentication != "federated-session") ||
		principal.AuthenticatedAt.IsZero() ||
		principal.AuthenticatedAt.After(now.Add(2*time.Minute)) {
		return false
	}
	maxAge := time.Duration(s.config.OIDCStepUpMaxAgeMin) * time.Minute
	if maxAge <= 0 {
		maxAge = 10 * time.Minute
	}
	if now.Sub(principal.AuthenticatedAt) > maxAge {
		return false
	}
	if !matchesAuthenticationContext(principal.AMR, s.config.OIDCStepUpAMRValues) ||
		!matchesAuthenticationContext([]string{principal.ACR}, s.config.OIDCStepUpACRValues) {
		return false
	}
	return true
}

func matchesAuthenticationContext(actual, required []string) bool {
	if len(required) == 0 {
		return true
	}
	for _, candidate := range actual {
		for _, accepted := range required {
			if subtle.ConstantTimeCompare(
				[]byte(strings.TrimSpace(candidate)),
				[]byte(strings.TrimSpace(accepted)),
			) == 1 {
				return true
			}
		}
	}
	return false
}

func (s *Server) authorizeStepUp(
	r *http.Request,
	principal iam.Principal,
) (iam.Principal, error) {
	if principal.Authentication == "oidc" ||
		principal.Authentication == "federated-session" {
		if principal.StepUp {
			return principal, nil
		}
		raw := extractBearerToken(r.Header.Get("X-Step-Up-Authorization"))
		if raw == "" {
			return iam.Principal{}, fmt.Errorf("fresh federated authentication is required")
		}
		stepRequest := r.Clone(r.Context())
		stepRequest.Header = r.Header.Clone()
		stepRequest.Header.Set("Authorization", "Bearer "+raw)
		elevated, err := s.authenticateRequest(stepRequest)
		if err != nil || elevated.Issuer != principal.Issuer ||
			elevated.Subject != principal.Subject {
			return iam.Principal{}, fmt.Errorf("step-up token does not match the authenticated subject")
		}
		if !elevated.StepUp {
			return iam.Principal{}, fmt.Errorf("OIDC step-up requirements were not satisfied")
		}
		return elevated, nil
	}
	if principal.Authentication == "legacy-api-key" {
		candidate := strings.TrimSpace(r.Header.Get("X-Step-Up-Key"))
		if s.config.StepUpAPIKey == "" ||
			subtle.ConstantTimeCompare([]byte(candidate), []byte(s.config.StepUpAPIKey)) != 1 ||
			subtle.ConstantTimeCompare([]byte(s.config.APIKey), []byte(s.config.StepUpAPIKey)) == 1 {
			return iam.Principal{}, fmt.Errorf("independent legacy step-up credential is required")
		}
		principal.StepUp = true
		return principal, nil
	}
	return iam.Principal{}, fmt.Errorf("this authentication method cannot perform step-up")
}

func extractBearerToken(header string) string {
	scheme, token, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func (s *Server) handleIAMExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.iamRepository == nil || len(s.identityVerifiers) == 0 {
		writeIAMError(w, http.StatusServiceUnavailable, "identity exchange is unavailable")
		return
	}
	var request struct {
		ProviderID string `json:"provider_id"`
		Credential string `json:"credential"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeIAMError(w, http.StatusBadRequest, "invalid identity exchange request")
		return
	}
	request.ProviderID = strings.TrimSpace(request.ProviderID)
	request.Credential = strings.TrimSpace(request.Credential)
	verifier := s.identityVerifiers[request.ProviderID]
	if verifier == nil || request.Credential == "" || len(request.Credential) > 12<<10 {
		writeIAMError(w, http.StatusUnauthorized, "identity provider credential was rejected")
		return
	}
	verifyCtx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	identity, err := verifier.Verify(verifyCtx, request.Credential)
	cancel()
	if err != nil {
		writeIAMError(w, http.StatusUnauthorized, "identity provider credential was rejected")
		return
	}
	rawSession, principal, err := s.issueFederatedSession(r.Context(), identity)
	if err != nil {
		writeIAMError(w, http.StatusServiceUnavailable, "session authority is unavailable")
		return
	}
	s.auditRequest(r, principal, "iam.session.exchange", "iam_session",
		principal.SessionID, "", "success",
		map[string]any{"provider_id": identity.ProviderID})
	expiresIn := int64(time.Until(principal.TokenExpiresAt).Seconds())
	if expiresIn < 1 {
		writeIAMError(w, http.StatusUnauthorized, "identity provider credential is expired")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": rawSession,
		"token_type":   "Bearer",
		"expires_in":   expiresIn,
		"provider_id":  identity.ProviderID,
	})
}

func (s *Server) handleIAMDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	verificationURI := strings.TrimSpace(s.config.DeviceAuthorizationVerificationURI)
	if s.authMode() == iam.ModeLegacy || s.deviceAuthorizations == nil ||
		verificationURI == "" {
		writeDeviceOAuthError(w, http.StatusServiceUnavailable,
			"temporarily_unavailable", "device authorization is unavailable", 0)
		return
	}
	parsedVerificationURI, err := url.Parse(verificationURI)
	if err != nil || parsedVerificationURI.Host == "" ||
		parsedVerificationURI.User != nil || parsedVerificationURI.RawQuery != "" ||
		parsedVerificationURI.Fragment != "" || parsedVerificationURI.Path != "/device" ||
		(parsedVerificationURI.Scheme != "https" && parsedVerificationURI.Scheme != "http") {
		writeDeviceOAuthError(w, http.StatusServiceUnavailable,
			"temporarily_unavailable", "device authorization is unavailable", 0)
		return
	}

	var rawDeviceCode, userCode string
	for attempt := 0; attempt < 5; attempt++ {
		rawDeviceCode, err = randomDeviceCode()
		if err != nil {
			break
		}
		userCode, err = randomDeviceUserCode()
		if err != nil {
			break
		}
		digest := sha256.Sum256([]byte(rawDeviceCode))
		createCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		created, createErr := s.deviceAuthorizations.CreateDeviceAuthorization(
			createCtx, fmt.Sprintf("%x", digest[:]), userCode,
			int(deviceAuthorizationTTL/time.Second),
			deviceAuthorizationInterval,
		)
		cancel()
		if createErr != nil {
			err = createErr
			break
		}
		if created {
			err = nil
			break
		}
		rawDeviceCode, userCode = "", ""
	}
	if err != nil || rawDeviceCode == "" || userCode == "" {
		writeDeviceOAuthError(w, http.StatusServiceUnavailable,
			"temporarily_unavailable", "device authorization is unavailable", 0)
		return
	}
	completeURI := *parsedVerificationURI
	query := completeURI.Query()
	query.Set("user_code", userCode)
	completeURI.RawQuery = query.Encode()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"device_code":               rawDeviceCode,
		"user_code":                 userCode,
		"verification_uri":          parsedVerificationURI.String(),
		"verification_uri_complete": completeURI.String(),
		"expires_in":                int64(deviceAuthorizationTTL / time.Second),
		"interval":                  deviceAuthorizationInterval,
	})
}

func (s *Server) handleIAMDeviceDecision(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, ok := iam.PrincipalFromContext(r.Context())
	if !ok || principal.Authentication != "federated-session" ||
		principal.SubjectID == "" || principal.SessionID == "" ||
		s.deviceAuthorizations == nil {
		writeIAMError(w, http.StatusForbidden,
			"an authenticated federated platform session is required")
		return
	}
	var request struct {
		UserCode string `json:"user_code"`
		Decision string `json:"decision"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil ||
		decoder.Decode(&struct{}{}) != io.EOF {
		writeIAMError(w, http.StatusBadRequest, "invalid device authorization decision")
		return
	}
	userCode, ok := normalizeDeviceUserCode(request.UserCode)
	decision := strings.ToLower(strings.TrimSpace(request.Decision))
	if !ok || (decision != "approve" && decision != "deny") {
		writeIAMError(w, http.StatusBadRequest, "invalid device authorization decision")
		return
	}
	decisionCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	device, err := s.deviceAuthorizations.DecideDeviceAuthorization(
		decisionCtx, userCode, principal, decision == "approve", s.iamSessionPolicy(),
	)
	cancel()
	if err != nil {
		s.auditRequest(r, principal, "iam.device.decision", "iam_device_authorization",
			"", "", "denied", map[string]any{"decision": decision})
		writeIAMError(w, http.StatusBadRequest,
			"device code is invalid, expired, or already decided")
		return
	}
	s.auditRequest(r, principal, "iam.device.decision", "iam_device_authorization",
		device.ID, "", "success", map[string]any{"decision": decision})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": device.Status,
	})
}

func (s *Server) handleIAMDeviceToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.authMode() == iam.ModeLegacy || s.deviceAuthorizations == nil {
		writeDeviceOAuthError(w, http.StatusServiceUnavailable,
			"temporarily_unavailable", "device authorization is unavailable", 0)
		return
	}
	mediaType := strings.ToLower(strings.TrimSpace(
		strings.Split(r.Header.Get("Content-Type"), ";")[0],
	))
	if mediaType != "application/x-www-form-urlencoded" {
		writeDeviceOAuthError(w, http.StatusBadRequest, "invalid_request",
			"form-encoded device token request is required", 0)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil ||
		len(r.PostForm) != 2 ||
		len(r.PostForm["grant_type"]) != 1 ||
		len(r.PostForm["device_code"]) != 1 ||
		r.PostForm.Get("grant_type") != deviceGrantType {
		writeDeviceOAuthError(w, http.StatusBadRequest, "invalid_request",
			"device_code and the device grant_type are required", 0)
		return
	}
	rawDeviceCode := strings.TrimSpace(r.PostForm.Get("device_code"))
	if rawDeviceCode == "" || len(rawDeviceCode) > 256 {
		writeDeviceOAuthError(w, http.StatusBadRequest, "expired_token",
			"device code is invalid or expired", 0)
		return
	}
	deviceDigest := sha256.Sum256([]byte(rawDeviceCode))
	rawAccessToken, err := randomPlatformSession()
	if err != nil {
		writeDeviceOAuthError(w, http.StatusServiceUnavailable,
			"temporarily_unavailable", "session authority is unavailable", 0)
		return
	}
	accessDigest := sha256.Sum256([]byte(rawAccessToken))
	pollCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	result, err := s.deviceAuthorizations.PollDeviceAuthorization(
		pollCtx, fmt.Sprintf("%x", deviceDigest[:]),
		fmt.Sprintf("%x", accessDigest[:]), s.iamSessionPolicy(),
	)
	cancel()
	if err != nil {
		writeDeviceOAuthError(w, http.StatusServiceUnavailable,
			"temporarily_unavailable", "session authority is unavailable", 0)
		return
	}
	switch result.Status {
	case persistence.DeviceAuthorizationPending:
		writeDeviceOAuthError(w, http.StatusBadRequest, "authorization_pending",
			"authorization is still pending", result.IntervalSeconds)
		return
	case persistence.DeviceAuthorizationSlowDown:
		writeDeviceOAuthError(w, http.StatusBadRequest, "slow_down",
			"polling too quickly", result.IntervalSeconds)
		return
	case persistence.DeviceAuthorizationDenied:
		writeDeviceOAuthError(w, http.StatusBadRequest, "access_denied",
			"the user denied the request", 0)
		return
	case persistence.DeviceAuthorizationExpired:
		writeDeviceOAuthError(w, http.StatusBadRequest, "expired_token",
			"device code is invalid, expired, or already consumed", 0)
		return
	case persistence.DeviceAuthorizationApproved:
	default:
		writeDeviceOAuthError(w, http.StatusServiceUnavailable,
			"temporarily_unavailable", "session authority is unavailable", 0)
		return
	}
	principal := result.Principal
	principal.ProviderID = s.providerIDForIssuer(principal.Issuer)
	principal.SystemAdmin = s.isIdentityAdmin(principal.Issuer, principal.Subject)
	principal.StepUp = s.oidcStepUpSatisfied(principal, time.Now())
	expiresIn := int64(time.Until(principal.TokenExpiresAt).Seconds())
	if expiresIn < 1 {
		// The database already atomically consumed the code and created this
		// session. Never turn a sub-second truncation into a lost response that
		// tempts the client to replay; authorization still checks expires_at.
		expiresIn = 1
	}
	s.auditRequest(r, principal, "iam.device.consume", "iam_session",
		principal.SessionID, "", "success", nil)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": rawAccessToken,
		"token_type":   "Bearer",
		"expires_in":   expiresIn,
	})
}

func randomDeviceCode() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "ped1_" + base64.RawURLEncoding.EncodeToString(value), nil
}

func randomPlatformSession() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "pe1_" + base64.RawURLEncoding.EncodeToString(value), nil
}

func randomDeviceUserCode() (string, error) {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	for index := range value {
		value[index] = deviceUserCodeAlphabet[int(value[index])&31]
	}
	return string(value[:4]) + "-" + string(value[4:]), nil
}

func normalizeDeviceUserCode(raw string) (string, bool) {
	compact := strings.NewReplacer("-", "", " ", "").Replace(
		strings.ToUpper(strings.TrimSpace(raw)),
	)
	if len(compact) != 8 {
		return "", false
	}
	for _, character := range compact {
		if !strings.ContainsRune(deviceUserCodeAlphabet, character) {
			return "", false
		}
	}
	return compact[:4] + "-" + compact[4:], true
}

func writeDeviceOAuthError(
	w http.ResponseWriter, status int, code, description string, interval int,
) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	if interval > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", interval))
	}
	w.WriteHeader(status)
	payload := map[string]any{"error": code, "error_description": description}
	if interval > 0 {
		payload["interval"] = interval
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Server) handleIAMProviderLifecycle(w http.ResponseWriter, r *http.Request) {
	providerID := backchannelProviderID(r.URL.Path)
	provider := s.providerConfigs[providerID]
	verifier := s.oidcVerifiers[providerID]
	if providerID == "" || !provider.BackchannelLogout || verifier == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mediaType := strings.ToLower(strings.TrimSpace(
		strings.Split(r.Header.Get("Content-Type"), ";")[0],
	))
	if mediaType != "application/x-www-form-urlencoded" {
		writeIAMError(w, http.StatusBadRequest, "logout token form is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeIAMError(w, http.StatusBadRequest, "invalid logout token form")
		return
	}
	values := r.PostForm["logout_token"]
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		writeIAMError(w, http.StatusBadRequest, "exactly one logout_token is required")
		return
	}
	verifyCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	logout, err := verifier.VerifyLogout(verifyCtx, values[0])
	cancel()
	if err != nil {
		writeIAMError(w, http.StatusBadRequest, "logout token was rejected")
		return
	}
	now := time.Now().UTC()
	maxAge := time.Duration(provider.BackchannelMaxAgeSeconds) * time.Second
	if maxAge <= 0 {
		maxAge = 5 * time.Minute
	}
	if logout.IssuedAt.After(now.Add(2*time.Minute)) ||
		now.Sub(logout.IssuedAt) > maxAge ||
		(provider.BackchannelRequireSID && logout.ProviderSessionID == "") {
		writeIAMError(w, http.StatusBadRequest, "logout token is outside provider policy")
		return
	}
	applyCtx, applyCancel := context.WithTimeout(r.Context(), 3*time.Second)
	result, err := s.iamRepository.ApplyBackchannelLogout(applyCtx, logout)
	applyCancel()
	if err != nil {
		writeIAMError(w, http.StatusServiceUnavailable, "session authority is unavailable")
		return
	}
	principal := iam.Principal{
		ProviderID: providerID, Issuer: logout.Issuer,
		Subject: logout.Subject, Authentication: "oidc-backchannel",
	}
	s.auditRequest(r, principal, "iam.session.backchannel_logout",
		"identity_provider", providerID, "", "success", map[string]any{
			"duplicate":        result.Duplicate,
			"revoked_sessions": result.RevokedSessions,
			"target":           map[bool]string{true: "sid", false: "subject"}[logout.ProviderSessionID != ""],
		})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) issueFederatedSession(
	ctx context.Context,
	identity iam.ExternalIdentity,
) (string, iam.Principal, error) {
	now := time.Now().UTC()
	policy := s.iamSessionPolicy()
	expiresAt := now.Add(policy.MaxLifetime)
	if !identity.ExpiresAt.IsZero() && identity.ExpiresAt.Before(expiresAt) {
		expiresAt = identity.ExpiresAt
	}
	if !expiresAt.After(now) {
		return "", iam.Principal{}, fmt.Errorf("provider credential is expired")
	}
	observeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	principal, err := s.iamRepository.ObserveSubject(
		observeCtx, identity, s.isIdentityAdmin(identity.Issuer, identity.Subject),
	)
	cancel()
	if err != nil {
		return "", iam.Principal{}, err
	}
	raw, err := randomPlatformSession()
	if err != nil {
		return "", iam.Principal{}, fmt.Errorf("generate platform session: %w", err)
	}
	digest := sha256.Sum256([]byte(raw))
	sessionIdentity := identity
	sessionIdentity.TokenHash = fmt.Sprintf("%x", digest[:])
	sessionIdentity.IssuedAt = now
	sessionIdentity.ExpiresAt = expiresAt
	authorizeCtx, authorizeCancel := context.WithTimeout(ctx, 3*time.Second)
	principal, err = s.iamRepository.AuthorizeSession(
		authorizeCtx, principal, sessionIdentity, policy,
	)
	authorizeCancel()
	if err != nil {
		return "", iam.Principal{}, err
	}
	principal.ProviderID = identity.ProviderID
	principal.Authentication = "federated-session"
	principal.StepUp = s.oidcStepUpSatisfied(principal, now)
	return raw, principal, nil
}

func writeIAMError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (s *Server) auditRequest(
	r *http.Request,
	principal iam.Principal,
	action, resourceType, resourceID, projectID, outcome string,
	detail map[string]any,
) {
	if s.iamRepository == nil {
		return
	}
	sourceIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	requestID := r.Header.Get("X-Request-ID")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.iamRepository.RecordAudit(ctx, persistence.AuditRecord{
		Principal: principal, Action: action, ResourceType: resourceType,
		ResourceID: resourceID, ProjectID: projectID, RequestID: requestID,
		SourceIP: sourceIP, Outcome: outcome, Detail: detail,
	}); err != nil {
		s.metrics.IncHTTPRequestErrors()
	}
}

func (s *Server) authorizeProject(
	r *http.Request,
	requiredRole string,
) (iam.Principal, iam.ProjectAccess, error) {
	principal, ok := iam.PrincipalFromContext(r.Context())
	if !ok {
		if s.iamRepository == nil {
			principal = iam.Principal{
				Issuer: "portage-engine", Subject: "direct-handler-compatibility",
				Authentication: "direct-handler-compatibility", SystemAdmin: true,
			}
		} else {
			return iam.Principal{}, iam.ProjectAccess{}, fmt.Errorf("request has no authenticated principal")
		}
	}
	if s.iamRepository == nil {
		return principal, iam.ProjectAccess{}, nil
	}
	selector := strings.TrimSpace(r.Header.Get("X-Project-ID"))
	if selector == "" {
		selector = strings.TrimSpace(r.URL.Query().Get("project_id"))
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	project, err := s.iamRepository.ResolveProject(ctx, principal, selector, requiredRole)
	return principal, project, err
}

func (s *Server) authorizeJob(
	r *http.Request,
	statusProjectID, requiredRole string,
) (iam.Principal, error) {
	principal, project, err := s.authorizeProject(r, requiredRole)
	if err != nil {
		return principal, err
	}
	if project.ProjectID != "" && statusProjectID != "" && project.ProjectID != statusProjectID {
		return principal, fmt.Errorf("job is not in the selected project")
	}
	return principal, nil
}

func (s *Server) handleIAMMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, ok := iam.PrincipalFromContext(r.Context())
	if !ok {
		writeIAMError(w, http.StatusUnauthorized, "authenticated identity required")
		return
	}
	var projects []iam.ProjectAccess
	if s.iamRepository != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		var err error
		projects, err = s.iamRepository.ListProjects(ctx, principal)
		cancel()
		if err != nil {
			writeIAMError(w, http.StatusServiceUnavailable, "identity store unavailable")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"principal": principal, "projects": projects, "auth_mode": s.authMode(),
		"identity_providers": s.identityProviderStatus(),
	})
}

func (s *Server) identityProviderStatus() []map[string]any {
	providerIDs := make([]string, 0, len(s.providerConfigs))
	for providerID := range s.providerConfigs {
		providerIDs = append(providerIDs, providerID)
	}
	sort.Strings(providerIDs)
	result := make([]map[string]any, 0, len(providerIDs))
	for _, providerID := range providerIDs {
		provider := s.providerConfigs[providerID]
		status := map[string]any{
			"id": provider.ID, "type": provider.Type,
			"display_name":               provider.DisplayName,
			"backchannel_logout_enabled": provider.BackchannelLogout,
		}
		if verifier := s.oidcVerifiers[providerID]; verifier != nil {
			status["capabilities"] = verifier.Capabilities()
		}
		result = append(result, status)
	}
	return result
}

func (s *Server) handleIAMSessions(w http.ResponseWriter, r *http.Request) {
	principal, ok := iam.PrincipalFromContext(r.Context())
	if !ok || s.iamRepository == nil || principal.SubjectID == "" {
		writeIAMError(w, http.StatusServiceUnavailable, "federated session authority is unavailable")
		return
	}
	targetSubject := strings.TrimSpace(r.URL.Query().Get("subject_id"))
	if targetSubject == "" {
		targetSubject = principal.SubjectID
	}
	switch r.Method {
	case http.MethodGet:
		if targetSubject != principal.SubjectID && !principal.SystemAdmin {
			writeIAMError(w, http.StatusForbidden, "system administrator role required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		sessions, err := s.iamRepository.ListSessions(ctx, targetSubject)
		cancel()
		if err != nil {
			writeIAMError(w, http.StatusServiceUnavailable, "session authority is unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessions": sessions, "current_session_id": principal.SessionID,
		})
	case http.MethodDelete:
		sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
		if sessionID == "" {
			sessionID = principal.SessionID
		}
		if _, err := uuid.Parse(sessionID); err != nil {
			writeIAMError(w, http.StatusBadRequest, "valid session_id is required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		session, err := s.iamRepository.RevokeSession(
			ctx, principal, sessionID, "user_requested", false,
		)
		cancel()
		if err != nil && principal.SystemAdmin && !principal.StepUp {
			if elevated, stepErr := s.authorizeStepUp(r, principal); stepErr == nil {
				principal = elevated
			}
		}
		if err != nil && principal.SystemAdmin && principal.StepUp {
			ctx, cancel = context.WithTimeout(r.Context(), 3*time.Second)
			session, err = s.iamRepository.RevokeSession(
				ctx, principal, sessionID, "administrator_revoked", true,
			)
			cancel()
		}
		if err != nil {
			writeIAMError(w, http.StatusForbidden, err.Error())
			return
		}
		s.auditRequest(r, principal, "iam.session.revoke", "iam_session",
			session.ID, "", "success", map[string]any{"subject_id": session.SubjectID})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(session)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleIAMRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, ok := iam.PrincipalFromContext(r.Context())
	if !ok || s.iamRepository == nil || principal.SubjectID == "" {
		writeIAMError(w, http.StatusServiceUnavailable, "federated session authority is unavailable")
		return
	}
	var request struct {
		SubjectID string `json:"subject_id"`
		Reason    string `json:"reason"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeIAMError(w, http.StatusBadRequest, "invalid session revocation request")
		return
	}
	request.SubjectID = strings.TrimSpace(request.SubjectID)
	if request.SubjectID == "" {
		request.SubjectID = principal.SubjectID
	}
	if _, err := uuid.Parse(request.SubjectID); err != nil || len(request.Reason) > 1024 {
		writeIAMError(w, http.StatusBadRequest, "subject_id or reason is invalid")
		return
	}
	other := request.SubjectID != principal.SubjectID
	if other && !principal.SystemAdmin {
		writeIAMError(w, http.StatusForbidden, "system administrator role required")
		return
	}
	if !principal.StepUp {
		writeIAMError(w, http.StatusPreconditionRequired, "fresh step-up authentication required")
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		reason = "revoke_all_requested"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	count, err := s.iamRepository.RevokeAllSessions(
		ctx, principal, request.SubjectID, reason, other,
	)
	cancel()
	if err != nil {
		writeIAMError(w, http.StatusConflict, err.Error())
		return
	}
	s.auditRequest(r, principal, "iam.sessions.revoke_all", "iam_subject",
		request.SubjectID, "", "success", map[string]any{"revoked_sessions": count})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"subject_id": request.SubjectID, "revoked_sessions": count,
	})
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	principal, ok := iam.PrincipalFromContext(r.Context())
	if !ok || s.iamRepository == nil {
		writeIAMError(w, http.StatusServiceUnavailable, "project authority is unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		projects, err := s.iamRepository.ListProjects(ctx, principal)
		cancel()
		if err != nil {
			writeIAMError(w, http.StatusServiceUnavailable, "project authority is unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"projects": projects})
	case http.MethodPost:
		if !principal.SystemAdmin {
			s.auditRequest(r, principal, "project.create", "project", "", "", "denied", nil)
			writeIAMError(w, http.StatusForbidden, "system administrator role required")
			return
		}
		var request struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeIAMError(w, http.StatusBadRequest, "invalid project request")
			return
		}
		request.Name = strings.TrimSpace(request.Name)
		request.Description = strings.TrimSpace(request.Description)
		if !projectNamePattern.MatchString(request.Name) || len(request.Description) > 4096 {
			writeIAMError(w, http.StatusBadRequest, "project name or description is invalid")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		project, err := s.iamRepository.CreateProject(ctx, request.Name, request.Description)
		cancel()
		if err != nil {
			writeIAMError(w, http.StatusConflict, "project could not be created")
			return
		}
		s.auditRequest(r, principal, "project.create", "project", project.ID,
			project.ID, "success", map[string]any{"name": project.Name})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(project)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleProjectMembers(w http.ResponseWriter, r *http.Request) {
	principal, project, err := s.authorizeProject(r, iam.RoleOwner)
	if err != nil {
		s.auditRequest(r, principal, "project.members.authorize", "project", "",
			"", "denied", nil)
		writeIAMError(w, http.StatusForbidden, err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		members, err := s.iamRepository.ListMembers(ctx, project.ProjectID)
		cancel()
		if err != nil {
			writeIAMError(w, http.StatusServiceUnavailable, "membership authority is unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"project": project, "members": members,
		})
	case http.MethodPut:
		var request struct {
			Issuer  string `json:"issuer"`
			Subject string `json:"subject"`
			Role    string `json:"role"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeIAMError(w, http.StatusBadRequest, "invalid membership request")
			return
		}
		request.Issuer = strings.TrimRight(strings.TrimSpace(request.Issuer), "/")
		request.Subject = strings.TrimSpace(request.Subject)
		request.Role = strings.TrimSpace(request.Role)
		if request.Issuer != strings.TrimRight(s.config.OIDCIssuerURL, "/") ||
			request.Subject == "" || len(request.Subject) > 512 ||
			!iam.RoleAtLeast(request.Role, iam.RoleViewer) {
			writeIAMError(w, http.StatusBadRequest, "issuer, subject, or role is invalid")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		member, err := s.iamRepository.PutMembership(
			ctx, project.ProjectID, request.Issuer, request.Subject, request.Role,
			principal.SubjectID,
		)
		cancel()
		if err != nil {
			writeIAMError(w, http.StatusConflict, "membership could not be updated")
			return
		}
		s.auditRequest(r, principal, "project.membership.put", "iam_subject",
			member.SubjectID, project.ProjectID, "success",
			map[string]any{"role": member.Role, "subject": member.Subject})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(member)
	case http.MethodDelete:
		subjectID := strings.TrimSpace(r.URL.Query().Get("subject_id"))
		if _, err := uuid.Parse(subjectID); err != nil {
			writeIAMError(w, http.StatusBadRequest, "valid subject_id is required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		err := s.iamRepository.DeleteMembership(ctx, project.ProjectID, subjectID)
		cancel()
		if err != nil {
			writeIAMError(w, http.StatusNotFound, "membership not found")
			return
		}
		s.auditRequest(r, principal, "project.membership.delete", "iam_subject",
			subjectID, project.ProjectID, "success", nil)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"deleted": subjectID})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleProjectPolicy(w http.ResponseWriter, r *http.Request) {
	requiredRole := iam.RoleViewer
	if r.Method == http.MethodPut {
		requiredRole = iam.RoleOwner
	}
	principal, project, err := s.authorizeProject(r, requiredRole)
	if err != nil {
		s.auditRequest(r, principal, "project.policy.authorize", "project", "",
			"", "denied", nil)
		writeIAMError(w, http.StatusForbidden, err.Error())
		return
	}
	if s.iamRepository == nil {
		writeIAMError(w, http.StatusServiceUnavailable, "project policy authority is unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		policy, err := s.iamRepository.GetProjectPolicy(ctx, project.ProjectID)
		cancel()
		if err != nil {
			writeIAMError(w, http.StatusServiceUnavailable, "project policy is unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(policy)
	case http.MethodPut:
		var request persistence.ProjectPolicyUpdate
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeIAMError(w, http.StatusBadRequest, "invalid project policy request")
			return
		}
		if err := validateProjectPolicyUpdate(request); err != nil {
			writeIAMError(w, http.StatusBadRequest, "project policy limits or version are invalid")
			return
		}
		updatedBy := principal.Authentication
		if principal.SubjectID != "" {
			updatedBy = principal.SubjectID
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		policy, err := s.iamRepository.UpdateProjectPolicy(
			ctx, project.ProjectID, request, updatedBy,
		)
		cancel()
		if err != nil {
			s.auditRequest(r, principal, "project.policy.update", "project",
				project.ProjectID, project.ProjectID, "failure",
				map[string]any{"reason": err.Error(), "expected_version": request.Version})
			writeIAMError(w, http.StatusConflict, err.Error())
			return
		}
		s.auditRequest(r, principal, "project.policy.update", "project",
			project.ProjectID, project.ProjectID, "success",
			map[string]any{
				"version": policy.Version, "suspended": policy.Suspended,
				"priority_weight":                 policy.PriorityWeight,
				"starvation_threshold_seconds":    policy.StarvationThresholdSeconds,
				"max_queued_jobs":                 policy.MaxQueuedJobs,
				"max_active_jobs":                 policy.MaxActiveJobs,
				"max_daily_submissions":           policy.MaxDailySubmissions,
				"max_active_vcpus":                policy.MaxActiveVCPUs,
				"max_active_memory_mib":           policy.MaxActiveMemoryMiB,
				"max_active_disk_gib":             policy.MaxActiveDiskGiB,
				"max_artifact_bytes_per_job":      policy.MaxArtifactBytesPerJob,
				"max_daily_build_seconds":         policy.MaxDailyBuildSeconds,
				"max_daily_cloud_cost_microunits": policy.MaxDailyCloudCostMicrounits,
				"max_failures_per_hour":           policy.MaxFailuresPerHour,
				"abuse_cooldown_seconds":          policy.AbuseCooldownSeconds,
				"clear_abuse_suspension":          request.ClearAbuseSuspension,
				"max_claimed_attempts":            policy.MaxClaimedAttempts,
				"max_provision_attempts":          policy.MaxProvisionAttempts,
				"max_build_attempts":              policy.MaxBuildAttempts,
				"max_verify_attempts":             policy.MaxVerifyAttempts,
				"max_publish_attempts":            policy.MaxPublishAttempts,
			})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(policy)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func validateProjectPolicyUpdate(request persistence.ProjectPolicyUpdate) error {
	if request.PriorityWeight < 0 || request.PriorityWeight > 1000 ||
		request.StarvationThresholdSeconds < 0 ||
		request.StarvationThresholdSeconds > 86400 ||
		(request.StarvationThresholdSeconds > 0 &&
			request.StarvationThresholdSeconds < 30) {
		return fmt.Errorf("project fairness policy is outside the supported range")
	}
	ranges := []struct {
		value int64
		min   int64
		max   int64
	}{
		{request.Version, 1, 1<<63 - 1},
		{int64(request.MaxQueuedJobs), 1, 100000},
		{int64(request.MaxActiveJobs), 1, 10000},
		{int64(request.MaxDailySubmissions), 1, 1000000},
		{int64(request.MaxActiveVCPUs), 1, 1280000},
		{int64(request.MaxActiveMemoryMiB), 1, 1048576000},
		{int64(request.MaxActiveDiskGiB), 1, 40960000},
		{request.MaxArtifactBytesPerJob, 1, 1 << 40},
		{request.MaxDailyBuildSeconds, 1, 365 * 24 * 60 * 60},
		{request.MaxDailyCloudCostMicrounits, 1, 1_000_000_000_000_000},
		{int64(request.MaxFailuresPerHour), 1, 100000},
		{int64(request.AbuseCooldownSeconds), 60, 7 * 24 * 60 * 60},
	}
	for _, allowed := range ranges {
		if allowed.value < allowed.min || allowed.value > allowed.max {
			return fmt.Errorf("project policy limit is outside the supported range")
		}
	}
	if !validPhaseCaps(request) {
		return fmt.Errorf("project policy limit is outside the supported range")
	}
	return nil
}

func validPhaseCaps(request persistence.ProjectPolicyUpdate) bool {
	for _, limit := range []int{
		request.MaxClaimedAttempts,
		request.MaxProvisionAttempts,
		request.MaxBuildAttempts,
		request.MaxVerifyAttempts,
		request.MaxPublishAttempts,
	} {
		if limit <= 0 || limit > 10000 {
			return false
		}
	}
	return true
}
