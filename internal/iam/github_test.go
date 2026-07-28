package iam

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGitHubVerifierBindsTokenToConfiguredOAuthApp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/applications/client-123/token" ||
			r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		clientID, secret, ok := r.BasicAuth()
		if !ok || clientID != "client-123" || secret != "secret-456" {
			http.Error(w, "bad app credentials", http.StatusUnauthorized)
			return
		}
		var request map[string]string
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request["access_token"] != "github-user-token" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created_at": time.Now().Add(-time.Minute),
			"expires_at": time.Now().Add(time.Hour),
			"user": map[string]any{
				"id": 88442211, "login": "octocat",
				"name": "The Octocat", "email": "octocat@example.test",
			},
		})
	}))
	defer server.Close()

	verifier, err := NewGitHubVerifier(
		"github", "client-123", "secret-456", server.URL, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := verifier.Verify(t.Context(), "github-user-token")
	if err != nil {
		t.Fatal(err)
	}
	if identity.ProviderID != "github" ||
		identity.Issuer != "https://github.com" ||
		identity.Subject != "88442211" ||
		identity.PreferredUsername != "octocat" ||
		len(identity.TokenHash) != 64 ||
		strings.Contains(identity.TokenHash, "github-user-token") {
		t.Fatalf("GitHub identity = %+v", identity)
	}
	if _, err := verifier.Verify(t.Context(), "other-app-token"); err == nil {
		t.Fatal("token rejected by the configured OAuth app was accepted")
	}
}
