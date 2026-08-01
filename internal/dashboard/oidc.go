package dashboard

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/slchris/portage-engine/pkg/config"
)

const oidcFlowCookie = "pe_oidc_flow"

type oidcRuntime struct {
	id           string
	providerType string
	displayName  string
	verifier     *oidc.IDTokenVerifier
	oauth2       oauth2.Config
}

type identityProviderView struct {
	ID          string
	DisplayName string
	LoginURL    string
}

type oidcFlow struct {
	ProviderID string `json:"provider_id,omitempty"`
	State      string `json:"state"`
	Verifier   string `json:"verifier"`
	Nonce      string `json:"nonce"`
	Expires    int64  `json:"expires"`
	StepUp     bool   `json:"step_up,omitempty"`
	ReturnTo   string `json:"return_to,omitempty"`
}

type oidcSession struct {
	AccessToken string `json:"access_token,omitempty"`
	IDToken     string `json:"id_token,omitempty"`
	ProviderID  string `json:"provider_id,omitempty"`
	Expires     int64  `json:"expires"`
}

// Initialize discovers the configured OIDC provider once. It is deliberately
// separate from New so existing local-only tests can construct a dashboard
// without a network dependency.
func (d *Dashboard) Initialize(ctx context.Context) error {
	providers := d.config.IdentityProviders
	if len(providers) == 0 && d.config.OIDCEnabled {
		providers = []config.IdentityProviderConfig{{
			ID: "oidc", Type: "oidc", DisplayName: "Identity provider",
			IssuerURL:         d.config.OIDCIssuerURL,
			Audience:          d.config.OIDCClientID,
			ClientID:          d.config.OIDCClientID,
			ClientSecret:      d.config.OIDCClientSecret,
			RedirectURL:       d.config.OIDCRedirectURL,
			AllowInsecureHTTP: d.config.OIDCAllowInsecureHTTP,
		}}
	}
	if len(providers) == 0 {
		return nil
	}
	d.providers = make(map[string]*oidcRuntime, len(providers))
	for _, configured := range providers {
		runtime := &oidcRuntime{
			id: configured.ID, providerType: configured.Type,
			displayName: configured.DisplayName,
			oauth2: oauth2.Config{
				ClientID: configured.ClientID, ClientSecret: configured.ClientSecret,
				RedirectURL: configured.RedirectURL,
			},
		}
		switch configured.Type {
		case "oidc":
			provider, err := oidc.NewProvider(
				ctx, strings.TrimRight(configured.IssuerURL, "/"),
			)
			if err != nil {
				return fmt.Errorf("discover dashboard provider %s: %w", configured.ID, err)
			}
			runtime.verifier = provider.VerifierContext(
				context.Background(), &oidc.Config{ClientID: configured.Audience},
			)
			runtime.oauth2.Endpoint = provider.Endpoint()
			runtime.oauth2.Scopes = []string{
				oidc.ScopeOpenID, oidc.ScopeProfile, oidc.ScopeEmail,
			}
		case "github":
			runtime.oauth2.Endpoint = oauth2.Endpoint{
				AuthURL:  configured.AuthorizationURL,
				TokenURL: configured.TokenURL,
			}
			runtime.oauth2.Scopes = []string{"read:user", "user:email"}
		default:
			return fmt.Errorf("unsupported dashboard identity provider %q", configured.Type)
		}
		d.providers[configured.ID] = runtime
		if d.oidc == nil {
			d.oidc = runtime
		}
	}
	return nil
}

func randomURLToken() (string, error) {
	value := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (d *Dashboard) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	d.handleProviderStart(w, r, d.oidc)
}

func (d *Dashboard) handleIdentityProviderRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/auth/provider/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	provider := d.providers[parts[0]]
	switch parts[1] {
	case "start":
		d.handleProviderStart(w, r, provider)
	case "callback":
		d.handleProviderCallback(w, r, provider)
	default:
		http.NotFound(w, r)
	}
}

func (d *Dashboard) handleProviderStart(
	w http.ResponseWriter,
	r *http.Request,
	provider *oidcRuntime,
) {
	if r.Method != http.MethodGet || provider == nil {
		http.NotFound(w, r)
		return
	}
	state, err := randomURLToken()
	if err != nil {
		http.Error(w, "failed to start login", http.StatusInternalServerError)
		return
	}
	verifier, err := randomURLToken()
	if err != nil {
		http.Error(w, "failed to start login", http.StatusInternalServerError)
		return
	}
	nonce, err := randomURLToken()
	if err != nil {
		http.Error(w, "failed to start login", http.StatusInternalServerError)
		return
	}
	flow := oidcFlow{
		ProviderID: provider.id,
		State:      state, Verifier: verifier, Nonce: nonce,
		Expires:  time.Now().Add(10 * time.Minute).Unix(),
		StepUp:   r.URL.Query().Get("step_up") == "1",
		ReturnTo: safeReturnTo(r.URL.Query().Get("return_to")),
	}
	sealed, err := d.seal("oidc-flow", flow)
	if err != nil {
		http.Error(w, "failed to start login", http.StatusInternalServerError)
		return
	}
	// #nosec G124 -- HTTP is an explicit trusted-LAN mode; HttpOnly and SameSite remain enforced.
	http.SetCookie(w, &http.Cookie{
		Name: oidcFlowCookie, Value: sealed, Path: "/auth/",
		MaxAge: 600, HttpOnly: true, Secure: d.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
	options := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}
	if provider.providerType != "github" {
		options = append(options, oidc.Nonce(nonce))
	}
	if flow.StepUp && provider.providerType != "github" {
		options = append(options,
			oauth2.SetAuthURLParam("prompt", "login"),
			oauth2.SetAuthURLParam("max_age", "0"),
		)
	}
	http.Redirect(w, r, provider.oauth2.AuthCodeURL(state, options...), http.StatusFound)
}

func (d *Dashboard) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	d.handleProviderCallback(w, r, d.oidc)
}

func (d *Dashboard) handleProviderCallback(
	w http.ResponseWriter,
	r *http.Request,
	provider *oidcRuntime,
) {
	if r.Method != http.MethodGet || provider == nil {
		http.NotFound(w, r)
		return
	}
	cookie, err := r.Cookie(oidcFlowCookie)
	if err != nil {
		http.Error(w, "OIDC flow cookie is missing", http.StatusBadRequest)
		return
	}
	var flow oidcFlow
	if err := d.unseal("oidc-flow", cookie.Value, &flow); err != nil ||
		time.Now().Unix() >= flow.Expires ||
		(flow.ProviderID != "" && flow.ProviderID != provider.id) ||
		subtle.ConstantTimeCompare([]byte(flow.State), []byte(r.URL.Query().Get("state"))) != 1 {
		http.Error(w, "OIDC flow state is invalid or expired", http.StatusBadRequest)
		return
	}
	if providerError := strings.TrimSpace(r.URL.Query().Get("error")); providerError != "" {
		http.Error(w, "OIDC provider rejected login", http.StatusUnauthorized)
		return
	}
	token, err := provider.oauth2.Exchange(
		r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(flow.Verifier),
	)
	if err != nil {
		http.Error(w, "OIDC code exchange failed", http.StatusUnauthorized)
		return
	}
	credential := token.AccessToken
	if provider.providerType != "github" {
		rawIDToken, ok := token.Extra("id_token").(string)
		if !ok || rawIDToken == "" {
			http.Error(w, "OIDC provider returned no ID token", http.StatusUnauthorized)
			return
		}
		idToken, verifyErr := provider.verifier.Verify(r.Context(), rawIDToken)
		if verifyErr != nil ||
			subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(flow.Nonce)) != 1 {
			http.Error(w, "OIDC ID token verification failed", http.StatusUnauthorized)
			return
		}
		credential = rawIDToken
	}
	platformToken, expiresAt, err := d.exchangeProviderCredential(
		r.Context(), provider.id, credential,
	)
	if err != nil {
		http.Error(w, "identity exchange failed", http.StatusUnauthorized)
		return
	}
	sessionValue, err := d.seal("oidc-session", oidcSession{
		AccessToken: platformToken, ProviderID: provider.id,
		Expires: expiresAt.Unix(),
	})
	if err != nil {
		http.Error(w, "failed to establish session", http.StatusInternalServerError)
		return
	}
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge <= 0 {
		http.Error(w, "OIDC ID token is already expired", http.StatusUnauthorized)
		return
	}
	// #nosec G124 -- HTTP is an explicit trusted-LAN mode; HttpOnly and SameSite remain enforced.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "federated." + sessionValue, Path: "/",
		MaxAge: maxAge, HttpOnly: true, Secure: d.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
	// #nosec G124 -- HTTP is an explicit trusted-LAN mode; HttpOnly and SameSite remain enforced.
	http.SetCookie(w, &http.Cookie{
		Name: oidcFlowCookie, Value: "", Path: "/auth/",
		MaxAge: -1, HttpOnly: true, Secure: d.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
	target := "/overview"
	if flow.ReturnTo != "" {
		target = flow.ReturnTo
	}
	if flow.StepUp {
		target = "/settings?step_up=complete"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (d *Dashboard) exchangeProviderCredential(
	ctx context.Context,
	providerID, credential string,
) (string, time.Time, error) {
	body, err := json.Marshal(map[string]string{
		"provider_id": providerID, "credential": credential,
	})
	if err != nil {
		return "", time.Time{}, err
	}
	target := strings.TrimRight(d.config.ServerURL, "/") + "/api/v1/iam/exchange"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", time.Time{}, fmt.Errorf("control plane rejected identity exchange")
	}
	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	if err := decoder.Decode(&result); err != nil {
		return "", time.Time{}, err
	}
	if !strings.EqualFold(result.TokenType, "Bearer") ||
		!strings.HasPrefix(result.AccessToken, "pe1_") ||
		result.ExpiresIn < 1 || result.ExpiresIn > 30*24*60*60 {
		return "", time.Time{}, fmt.Errorf("control plane returned an invalid session")
	}
	return result.AccessToken, time.Now().Add(time.Duration(result.ExpiresIn) * time.Second), nil
}

func (d *Dashboard) seal(purpose string, value any) (string, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	key := sha256.Sum256([]byte("portage-engine:" + purpose + ":" + d.config.JWTSecret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, plain, []byte(purpose))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (d *Dashboard) unseal(purpose, encoded string, target any) error {
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	key := sha256.Sum256([]byte("portage-engine:" + purpose + ":" + d.config.JWTSecret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	if len(data) < aead.NonceSize() {
		return fmt.Errorf("sealed value is truncated")
	}
	nonce, ciphertext := data[:aead.NonceSize()], data[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, ciphertext, []byte(purpose))
	if err != nil {
		return err
	}
	return json.Unmarshal(plain, target)
}

func (d *Dashboard) oidcTokenFromSession(ctx context.Context, token string) (string, error) {
	if !strings.HasPrefix(token, "oidc.") && !strings.HasPrefix(token, "federated.") {
		return "", fmt.Errorf("not an OIDC dashboard session")
	}
	prefix := "oidc."
	if strings.HasPrefix(token, "federated.") {
		prefix = "federated."
	}
	var session oidcSession
	if err := d.unseal("oidc-session", strings.TrimPrefix(token, prefix), &session); err != nil {
		return "", err
	}
	if time.Now().Unix() >= session.Expires {
		return "", fmt.Errorf("OIDC dashboard session expired")
	}
	if session.AccessToken != "" {
		if !strings.HasPrefix(session.AccessToken, "pe1_") {
			return "", fmt.Errorf("dashboard platform session is invalid")
		}
		return session.AccessToken, nil
	}
	if d.oidc == nil || d.oidc.verifier == nil {
		return "", fmt.Errorf("legacy OIDC dashboard session is unavailable")
	}
	if _, err := d.oidc.verifier.Verify(ctx, session.IDToken); err != nil {
		return "", err
	}
	return session.IDToken, nil
}

func (d *Dashboard) verifyDashboardSession(ctx context.Context, token string) error {
	if strings.HasPrefix(token, "oidc.") || strings.HasPrefix(token, "federated.") {
		_, err := d.oidcTokenFromSession(ctx, token)
		return err
	}
	return verifyToken(d.config.JWTSecret, token, time.Now())
}

func (d *Dashboard) applyBackendAuth(incoming, outgoing *http.Request) {
	if cookie, err := incoming.Cookie(sessionCookie); err == nil {
		if token, err := d.oidcTokenFromSession(incoming.Context(), cookie.Value); err == nil {
			outgoing.Header.Set("Authorization", "Bearer "+token)
			return
		}
		if d.validLocalStepUp(incoming, cookie.Value) {
			outgoing.Header.Set("X-Step-Up-Key", d.config.ServerStepUpAPIKey)
		}
	}
	if d.config.ServerAPIKey != "" {
		outgoing.Header.Set("X-API-Key", d.config.ServerAPIKey)
	}
}
