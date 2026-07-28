// Package iam defines the authenticated control-plane identity and project
// authorization vocabulary. PostgreSQL remains the membership authority;
// bearer-token verification only establishes the external subject.
package iam

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	ModeLegacy = "legacy"
	ModeHybrid = "hybrid"
	ModeOIDC   = "oidc"

	RoleViewer     = "viewer"
	RoleDeveloper  = "developer"
	RoleMaintainer = "maintainer"
	RoleOwner      = "owner"
)

type contextKey struct{}

// ExternalIdentity is the cryptographically verified identity asserted by the
// configured OIDC issuer. Authorization never trusts role/project claims from
// the token; those assignments come from PostgreSQL.
type ExternalIdentity struct {
	ProviderID        string
	Issuer            string
	Subject           string
	PreferredUsername string
	DisplayName       string
	Email             string
	TokenHash         string
	ProviderSessionID string
	ProviderTokenID   string
	IssuedAt          time.Time
	AuthenticatedAt   time.Time
	ExpiresAt         time.Time
	ACR               string
	AMR               []string
}

// LogoutIdentity is a verified OpenID Connect Back-Channel Logout token. The
// raw signed token is never returned to persistence or audit code.
type LogoutIdentity struct {
	ProviderID        string
	Issuer            string
	Subject           string
	ProviderSessionID string
	ProviderTokenID   string
	IssuedAt          time.Time
	ExpiresAt         time.Time
}

// ProviderCapabilities is the security-relevant discovery metadata consumed
// by the server. It contains no provider credentials or dynamic authorization
// claims.
type ProviderCapabilities struct {
	ProviderID                  string `json:"provider_id"`
	Issuer                      string `json:"issuer"`
	BackchannelLogoutSupported  bool   `json:"backchannel_logout_supported"`
	BackchannelSessionSupported bool   `json:"backchannel_logout_session_supported"`
}

// Principal is the request identity after its external subject has been
// observed in PostgreSQL. Legacy API-key access is explicitly represented as
// a system administrator for migration and audit purposes.
type Principal struct {
	ProviderID        string    `json:"provider_id,omitempty"`
	SubjectID         string    `json:"subject_id,omitempty"`
	Issuer            string    `json:"issuer"`
	Subject           string    `json:"subject"`
	PreferredUsername string    `json:"preferred_username,omitempty"`
	DisplayName       string    `json:"display_name,omitempty"`
	Email             string    `json:"email,omitempty"`
	Authentication    string    `json:"authentication"`
	SystemAdmin       bool      `json:"system_admin"`
	SessionID         string    `json:"session_id,omitempty"`
	TokenIssuedAt     time.Time `json:"token_issued_at,omitempty"`
	TokenExpiresAt    time.Time `json:"token_expires_at,omitempty"`
	AuthenticatedAt   time.Time `json:"authenticated_at,omitempty"`
	ACR               string    `json:"acr,omitempty"`
	AMR               []string  `json:"amr,omitempty"`
	StepUp            bool      `json:"step_up"`
}

// ProjectAccess is a durable membership returned to API clients.
type ProjectAccess struct {
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	Role        string `json:"role"`
}

// IdentityVerifier verifies one provider credential for session exchange.
type IdentityVerifier interface {
	Verify(context.Context, string) (ExternalIdentity, error)
}

// Verifier validates one bearer JWT against an exact issuer and audience and
// supports the OIDC-specific logout protocol.
type Verifier interface {
	IdentityVerifier
	VerifyLogout(context.Context, string) (LogoutIdentity, error)
	Capabilities() ProviderCapabilities
}

// OIDCVerifier wraps one long-lived go-oidc verifier so JWKS are cached and
// refreshed by key ID instead of refetched for every request.
type OIDCVerifier struct {
	providerID   string
	issuer       string
	verifier     *oidc.IDTokenVerifier
	capabilities ProviderCapabilities
}

// NewOIDCVerifier discovers the provider and constructs an issuer/audience/
// expiry/signature-checking verifier. Plain HTTP issuers require an explicit
// trusted-LAN opt-in; that does not encrypt bearer tokens on the API hop.
func NewOIDCVerifier(
	ctx context.Context,
	issuer, audience string,
	allowInsecureHTTP bool,
) (*OIDCVerifier, error) {
	return NewOIDCProviderVerifier(ctx, "", issuer, audience, allowInsecureHTTP)
}

// NewOIDCProviderVerifier builds a verifier associated with a stable local
// provider ID. The ID selects configuration and UI only; issuer+subject remain
// the durable external identity key.
func NewOIDCProviderVerifier(
	ctx context.Context,
	providerID, issuer, audience string,
	allowInsecureHTTP bool,
) (*OIDCVerifier, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	audience = strings.TrimSpace(audience)
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, fmt.Errorf("OIDC issuer must be an HTTP(S) origin or path without credentials, query, or fragment")
	}
	if parsed.Scheme == "http" && !allowInsecureHTTP {
		return nil, fmt.Errorf("OIDC issuer uses HTTP; set OIDC_ALLOW_INSECURE_HTTP=true only on a trusted private network")
	}
	if audience == "" {
		return nil, fmt.Errorf("OIDC audience is required")
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(discoveryCtx, issuer)
	if err != nil {
		return nil, fmt.Errorf("discover OIDC provider: %w", err)
	}
	var discovery struct {
		BackchannelLogoutSupported  bool `json:"backchannel_logout_supported"`
		BackchannelSessionSupported bool `json:"backchannel_logout_session_supported"`
	}
	if err := provider.Claims(&discovery); err != nil {
		return nil, fmt.Errorf("decode OIDC provider metadata: %w", err)
	}
	return &OIDCVerifier{
		providerID: providerID, issuer: issuer,
		verifier: provider.VerifierContext(context.Background(), &oidc.Config{
			ClientID: audience,
		}),
		capabilities: ProviderCapabilities{
			ProviderID:                  providerID,
			Issuer:                      issuer,
			BackchannelLogoutSupported:  discovery.BackchannelLogoutSupported,
			BackchannelSessionSupported: discovery.BackchannelSessionSupported,
		},
	}, nil
}

// Capabilities returns the immutable discovery metadata captured during
// startup. Provider discovery is not performed on each request.
func (v *OIDCVerifier) Capabilities() ProviderCapabilities {
	return v.capabilities
}

// Verify validates the JWT and extracts identity-only claims.
func (v *OIDCVerifier) Verify(ctx context.Context, raw string) (ExternalIdentity, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ExternalIdentity{}, fmt.Errorf("bearer token is empty")
	}
	token, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return ExternalIdentity{}, fmt.Errorf("verify OIDC token: %w", err)
	}
	var claims struct {
		Subject           string   `json:"sub"`
		PreferredUsername string   `json:"preferred_username"`
		Name              string   `json:"name"`
		Email             string   `json:"email"`
		EmailVerified     bool     `json:"email_verified"`
		ProviderSessionID string   `json:"sid"`
		ProviderTokenID   string   `json:"jti"`
		AuthenticatedAt   int64    `json:"auth_time"`
		ACR               string   `json:"acr"`
		AMR               []string `json:"amr"`
	}
	if err := token.Claims(&claims); err != nil {
		return ExternalIdentity{}, fmt.Errorf("decode OIDC claims: %w", err)
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return ExternalIdentity{}, fmt.Errorf("OIDC token has no subject")
	}
	now := time.Now()
	if token.IssuedAt.IsZero() || token.IssuedAt.After(now.Add(2*time.Minute)) ||
		!token.Expiry.After(token.IssuedAt) {
		return ExternalIdentity{}, fmt.Errorf("OIDC token has an invalid issued-at lifetime")
	}
	if len(claims.ProviderSessionID) > 512 || len(claims.ProviderTokenID) > 512 ||
		len(claims.ACR) > 512 || len(claims.AMR) > 32 {
		return ExternalIdentity{}, fmt.Errorf("OIDC session claims exceed supported limits")
	}
	amr := make([]string, 0, len(claims.AMR))
	for _, method := range claims.AMR {
		method = strings.TrimSpace(method)
		if method == "" || len(method) > 128 {
			return ExternalIdentity{}, fmt.Errorf("OIDC amr claim is invalid")
		}
		amr = append(amr, method)
	}
	var authenticatedAt time.Time
	if claims.AuthenticatedAt > 0 {
		authenticatedAt = time.Unix(claims.AuthenticatedAt, 0).UTC()
		if authenticatedAt.After(now.Add(2*time.Minute)) || authenticatedAt.After(token.Expiry) {
			return ExternalIdentity{}, fmt.Errorf("OIDC auth_time claim is invalid")
		}
	}
	tokenDigest := sha256.Sum256([]byte(raw))
	email := strings.TrimSpace(claims.Email)
	if email != "" && !claims.EmailVerified {
		email = ""
	}
	return ExternalIdentity{
		ProviderID: v.providerID, Issuer: v.issuer, Subject: claims.Subject,
		PreferredUsername: strings.TrimSpace(claims.PreferredUsername),
		DisplayName:       strings.TrimSpace(claims.Name),
		Email:             email,
		TokenHash:         hex.EncodeToString(tokenDigest[:]),
		ProviderSessionID: strings.TrimSpace(claims.ProviderSessionID),
		ProviderTokenID:   strings.TrimSpace(claims.ProviderTokenID),
		IssuedAt:          token.IssuedAt.UTC(),
		AuthenticatedAt:   authenticatedAt,
		ExpiresAt:         token.Expiry.UTC(),
		ACR:               strings.TrimSpace(claims.ACR),
		AMR:               amr,
	}, nil
}

// VerifyLogout validates a signed OpenID Connect Back-Channel Logout token,
// including issuer, audience, signature, expiry, event type and nonce absence.
func (v *OIDCVerifier) VerifyLogout(
	ctx context.Context,
	raw string,
) (LogoutIdentity, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return LogoutIdentity{}, fmt.Errorf("logout token is empty")
	}
	token, err := v.verifier.VerifyLogout(ctx, raw)
	if err != nil {
		return LogoutIdentity{}, fmt.Errorf("verify OIDC logout token: %w", err)
	}
	subject := strings.TrimSpace(token.Subject)
	sessionID := strings.TrimSpace(token.SessionID)
	tokenID := strings.TrimSpace(token.TokenID)
	if tokenID == "" {
		return LogoutIdentity{}, fmt.Errorf("OIDC logout token has no jti")
	}
	if len(subject) > 512 || len(sessionID) > 512 || len(tokenID) > 512 {
		return LogoutIdentity{}, fmt.Errorf("OIDC logout claims exceed supported limits")
	}
	if token.IssuedAt.IsZero() || token.Expiry.IsZero() ||
		!token.Expiry.After(token.IssuedAt) {
		return LogoutIdentity{}, fmt.Errorf("OIDC logout token has an invalid lifetime")
	}
	return LogoutIdentity{
		ProviderID: v.providerID, Issuer: v.issuer, Subject: subject,
		ProviderSessionID: sessionID, ProviderTokenID: tokenID,
		IssuedAt: token.IssuedAt.UTC(), ExpiresAt: token.Expiry.UTC(),
	}, nil
}

// WithPrincipal attaches the authenticated identity to a request context.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, principal)
}

// PrincipalFromContext returns the authenticated request identity.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(contextKey{}).(Principal)
	return principal, ok
}

// RoleAtLeast compares only the four project roles. System-admin bypass is
// handled by the caller so it remains visible at the authorization boundary.
func RoleAtLeast(actual, required string) bool {
	rank := map[string]int{
		RoleViewer: 1, RoleDeveloper: 2, RoleMaintainer: 3, RoleOwner: 4,
	}
	return rank[actual] >= rank[required] && rank[required] > 0
}
