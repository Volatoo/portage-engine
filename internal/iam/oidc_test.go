package iam

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOIDCVerifierDiscoverySignatureAudienceAndClaims(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": issuer.URL, "jwks_uri": issuer.URL + "/keys",
				"id_token_signing_alg_values_supported": []string{"RS256"},
				"backchannel_logout_supported":          true,
				"backchannel_logout_session_supported":  true,
			})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "iam-test",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer issuer.Close()

	if _, err := NewOIDCVerifier(context.Background(), issuer.URL, "portage-engine", false); err == nil ||
		!strings.Contains(err.Error(), "OIDC issuer uses HTTP") {
		t.Fatalf("HTTP issuer was not rejected without opt-in: %v", err)
	}
	verifier, err := NewOIDCVerifier(context.Background(), issuer.URL, "portage-engine", true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	token := signTestJWT(t, key, map[string]any{
		"iss": issuer.URL, "sub": "alice", "aud": "portage-engine",
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
		"preferred_username": "alice", "name": "Alice Example",
		"email": "alice@example.test", "email_verified": true,
		"sid": "provider-session", "jti": "token-id",
		"auth_time": now.Add(-2 * time.Minute).Unix(),
		"acr":       "urn:test:mfa", "amr": []string{"pwd", "otp"},
	})
	identity, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Subject != "alice" || identity.PreferredUsername != "alice" ||
		identity.Email != "alice@example.test" || identity.Issuer != issuer.URL ||
		identity.TokenHash == "" || identity.ProviderSessionID != "provider-session" ||
		identity.ProviderTokenID != "token-id" || identity.ACR != "urn:test:mfa" ||
		len(identity.AMR) != 2 || identity.AuthenticatedAt.IsZero() {
		t.Fatalf("identity = %+v", identity)
	}
	if strings.Contains(identity.TokenHash, token) || len(identity.TokenHash) != 64 {
		t.Fatalf("token hash is not a redacted SHA-256: %q", identity.TokenHash)
	}

	wrongAudience := signTestJWT(t, key, map[string]any{
		"iss": issuer.URL, "sub": "alice", "aud": "some-other-api",
		"exp": now.Add(time.Hour).Unix(),
	})
	if _, err := verifier.Verify(context.Background(), wrongAudience); err == nil {
		t.Fatal("wrong audience token was accepted")
	}
	futureAuthentication := signTestJWT(t, key, map[string]any{
		"iss": issuer.URL, "sub": "alice", "aud": "portage-engine",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"auth_time": now.Add(10 * time.Minute).Unix(),
	})
	if _, err := verifier.Verify(context.Background(), futureAuthentication); err == nil {
		t.Fatal("future auth_time token was accepted")
	}
	if capabilities := verifier.Capabilities(); !capabilities.BackchannelLogoutSupported ||
		!capabilities.BackchannelSessionSupported {
		t.Fatalf("provider capabilities = %+v", capabilities)
	}
	logoutToken := signTestJWT(t, key, map[string]any{
		"iss": issuer.URL, "sub": "alice", "sid": "provider-session",
		"aud": "portage-engine", "iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(), "jti": "logout-1",
		"events": map[string]any{
			"http://schemas.openid.net/event/backchannel-logout": map[string]any{},
		},
	})
	logout, err := verifier.VerifyLogout(context.Background(), logoutToken)
	if err != nil || logout.Subject != "alice" ||
		logout.ProviderSessionID != "provider-session" ||
		logout.ProviderTokenID != "logout-1" {
		t.Fatalf("verified logout = %+v, %v", logout, err)
	}
	withNonce := signTestJWT(t, key, map[string]any{
		"iss": issuer.URL, "sid": "provider-session", "aud": "portage-engine",
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"jti": "logout-2", "nonce": "not-allowed",
		"events": map[string]any{
			"http://schemas.openid.net/event/backchannel-logout": map[string]any{},
		},
	})
	if _, err := verifier.VerifyLogout(context.Background(), withNonce); err == nil {
		t.Fatal("logout token containing nonce was accepted")
	}
}

func signTestJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	encode := func(value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(data)
	}
	input := encode(map[string]string{
		"alg": "RS256", "kid": "iam-test", "typ": "JWT",
	}) + "." + encode(claims)
	sum := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s.%s", input, base64.RawURLEncoding.EncodeToString(signature))
}
