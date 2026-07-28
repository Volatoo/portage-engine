package iam

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHubVerifier validates an OAuth token against the exact OAuth application,
// not merely the /user endpoint. This prevents a token minted for an unrelated
// GitHub OAuth app from being exchanged for a Portage Engine session.
type GitHubVerifier struct {
	providerID   string
	clientID     string
	clientSecret string
	apiBaseURL   string
	httpClient   *http.Client
}

func NewGitHubVerifier(
	providerID, clientID, clientSecret, apiBaseURL string,
	httpClient *http.Client,
) (*GitHubVerifier, error) {
	providerID = strings.TrimSpace(providerID)
	clientID = strings.TrimSpace(clientID)
	apiBaseURL = strings.TrimRight(strings.TrimSpace(apiBaseURL), "/")
	if providerID == "" || clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("GitHub provider id, client id, and client secret are required")
	}
	parsed, err := url.Parse(apiBaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("GitHub API base URL is invalid")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &GitHubVerifier{
		providerID: providerID, clientID: clientID, clientSecret: clientSecret,
		apiBaseURL: apiBaseURL, httpClient: httpClient,
	}, nil
}

func (v *GitHubVerifier) Verify(ctx context.Context, raw string) (ExternalIdentity, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 4096 {
		return ExternalIdentity{}, fmt.Errorf("GitHub OAuth token is empty or too large")
	}
	body, err := json.Marshal(map[string]string{"access_token": raw})
	if err != nil {
		return ExternalIdentity{}, err
	}
	endpoint := v.apiBaseURL + "/applications/" + url.PathEscape(v.clientID) + "/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ExternalIdentity{}, fmt.Errorf("create GitHub token check: %w", err)
	}
	req.SetBasicAuth(v.clientID, v.clientSecret)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return ExternalIdentity{}, fmt.Errorf("check GitHub OAuth token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return ExternalIdentity{}, fmt.Errorf("GitHub OAuth token was rejected")
	}
	var result struct {
		CreatedAt time.Time  `json:"created_at"`
		ExpiresAt *time.Time `json:"expires_at"`
		User      struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"user"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := decoder.Decode(&result); err != nil {
		return ExternalIdentity{}, fmt.Errorf("decode GitHub token check: %w", err)
	}
	if result.User.ID <= 0 || strings.TrimSpace(result.User.Login) == "" {
		return ExternalIdentity{}, fmt.Errorf("GitHub token check returned no stable user")
	}
	now := time.Now().UTC()
	issuedAt := result.CreatedAt.UTC()
	if issuedAt.IsZero() || issuedAt.After(now.Add(2*time.Minute)) {
		issuedAt = now
	}
	expiresAt := now.Add(12 * time.Hour)
	if result.ExpiresAt != nil && result.ExpiresAt.After(now) {
		expiresAt = result.ExpiresAt.UTC()
	}
	digest := sha256.Sum256([]byte(raw))
	return ExternalIdentity{
		ProviderID:        v.providerID,
		Issuer:            "https://github.com",
		Subject:           strconv.FormatInt(result.User.ID, 10),
		PreferredUsername: strings.TrimSpace(result.User.Login),
		DisplayName:       strings.TrimSpace(result.User.Name),
		Email:             strings.TrimSpace(result.User.Email),
		TokenHash:         hex.EncodeToString(digest[:]),
		IssuedAt:          issuedAt,
		ExpiresAt:         expiresAt,
		AMR:               []string{"oauth"},
	}, nil
}
