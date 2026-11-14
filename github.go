package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// GitHubClient authenticates as a GitHub App to clone repos and post commit
// statuses. One App can be installed on several owners; the installation for
// each repo is looked up on first use and cached.
// If not configured (PUNCHCARD_GITHUB_APP_ID unset) it is nil and all calls are no-ops.
type GitHubClient struct {
	appID      string
	privateKey *rsa.PrivateKey
	httpClient *http.Client

	mu            sync.Mutex
	installations map[string]int64 // "owner/repo" -> installation ID
	tokens        map[int64]cachedToken
}

type cachedToken struct {
	token string
	exp   time.Time
}

// newGitHubClient builds a client from environment/credentials.
// Returns nil without error if PUNCHCARD_GITHUB_APP_ID is not set,
// so the GitHub App is opt-in (local dev falls back to SSH).
func newGitHubClient() (*GitHubClient, error) {
	appID := os.Getenv("PUNCHCARD_GITHUB_APP_ID")
	if appID == "" {
		return nil, nil
	}

	keyPEM := readSecret("GITHUB_APP_PRIVATE_KEY")
	if keyPEM == "" {
		return nil, fmt.Errorf("PUNCHCARD_GITHUB_APP_ID is set but GITHUB_APP_PRIVATE_KEY secret is missing")
	}

	key, err := parseRSAKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("invalid GITHUB_APP_PRIVATE_KEY: %w", err)
	}

	slog.Info("GitHub App configured", "app_id", appID)
	return &GitHubClient{
		appID:         appID,
		privateKey:    key,
		httpClient:    &http.Client{Timeout: 15 * time.Second},
		installations: make(map[string]int64),
		tokens:        make(map[int64]cachedToken),
	}, nil
}

// SetCommitStatus posts a commit status to GitHub.
// state is one of: "pending", "success", "failure", "error".
// repoFullName is "owner/repo". targetURL, if set, is linked from the check.
func (c *GitHubClient) SetCommitStatus(repoFullName, sha, state, description, targetURL string) {
	if c == nil {
		return
	}
	token, err := c.Token(repoFullName)
	if err != nil {
		slog.Warn("GitHub status skipped: could not get installation token", "err", err)
		return
	}

	body := map[string]string{
		"state":       state,
		"description": description,
		"context":     "punchcard/ci",
	}
	if targetURL != "" {
		body["target_url"] = targetURL
	}
	payload, _ := json.Marshal(body)

	url := fmt.Sprintf("https://api.github.com/repos/%s/statuses/%s", repoFullName, sha)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		slog.Warn("GitHub status skipped: could not build request", "err", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		slog.Warn("GitHub status failed", "repo", repoFullName, "sha", shortSHA(sha), "err", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		slog.Warn("GitHub status failed", "repo", repoFullName, "sha", shortSHA(sha), "status", resp.StatusCode, "body", string(body))
		return
	}

	slog.Info("GitHub status posted", "repo", repoFullName, "sha", shortSHA(sha), "state", state)
}

// Token returns an installation access token that can act on repoFullName.
func (c *GitHubClient) Token(repoFullName string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id, ok := c.installations[repoFullName]
	if !ok {
		var inst struct {
			ID int64 `json:"id"`
		}
		if err := c.appRequest(http.MethodGet, "/repos/"+repoFullName+"/installation", http.StatusOK, &inst); err != nil {
			return "", fmt.Errorf("finding App installation for %s: %w", repoFullName, err)
		}
		id = inst.ID
		c.installations[repoFullName] = id
	}

	if t, ok := c.tokens[id]; ok && time.Now().Before(t.exp.Add(-5*time.Minute)) {
		return t.token, nil
	}

	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", id)
	if err := c.appRequest(http.MethodPost, path, http.StatusCreated, &result); err != nil {
		return "", fmt.Errorf("creating installation token: %w", err)
	}
	c.tokens[id] = cachedToken{token: result.Token, exp: result.ExpiresAt}
	return result.Token, nil
}

// appRequest calls the GitHub API authenticated as the App itself (JWT) and
// decodes the JSON response into out.
func (c *GitHubClient) appRequest(method, path string, wantStatus int, out any) error {
	jwt, err := c.signJWT()
	if err != nil {
		return fmt.Errorf("signing JWT: %w", err)
	}
	req, err := http.NewRequest(method, "https://api.github.com"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != wantStatus {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub returned %d: %s", resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// signJWT produces a RS256-signed JWT for authenticating as the GitHub App.
func (c *GitHubClient) signJWT() (string, error) {
	now := time.Now()
	header := b64j(map[string]string{"alg": "RS256", "typ": "JWT"})
	payload := b64j(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(), // 60s clock skew buffer
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": c.appID,
	})

	signing := header + "." + payload
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.privateKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func b64j(v any) string {
	data, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(data)
}

func parseRSAKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// Try PKCS8 as fallback
		k, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, err // return the original error
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("key is not RSA")
		}
		return rk, nil
	}
	return key, nil
}
