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
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// GitHubApp mints installation access tokens for a registered GitHub App, so
// the deploy engine can clone private repositories with least-privilege,
// per-installation, short-lived tokens (rather than a broad user OAuth token).
//
// It is optional: when GITHUB_APP_ID / GITHUB_APP_PRIVATE_KEY(_PATH) are unset,
// NewGitHubApp returns nil and the deploy engine falls back to the stored user
// OAuth token.
type GitHubApp struct {
	appID string
	key   *rsa.PrivateKey
	http  *http.Client
	mu    sync.Mutex
	cache map[int64]cachedToken // installationID -> token
}

type cachedToken struct {
	token   string
	expires time.Time
}

// NewGitHubApp loads the App from the environment, or returns (nil, nil) when
// it is not configured.
func NewGitHubApp() (*GitHubApp, error) {
	appID := os.Getenv("GITHUB_APP_ID")
	if appID == "" {
		return nil, nil
	}
	pemBytes, err := loadAppKey()
	if err != nil {
		return nil, err
	}
	key, err := parseRSAKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App private key: %w", err)
	}
	return &GitHubApp{
		appID: appID,
		key:   key,
		http:  &http.Client{Timeout: 15 * time.Second},
		cache: map[int64]cachedToken{},
	}, nil
}

func loadAppKey() ([]byte, error) {
	if path := os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH"); path != "" {
		return os.ReadFile(path)
	}
	if key := os.Getenv("GITHUB_APP_PRIVATE_KEY"); key != "" {
		return []byte(key), nil
	}
	return nil, fmt.Errorf("GITHUB_APP_ID set but no private key (set GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_PATH)")
}

func parseRSAKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA private key")
	}
	return rsaKey, nil
}

// appJWT mints a short-lived RS256 JWT authenticating as the App itself.
func (g *GitHubApp) appJWT() (string, error) {
	now := time.Now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": now.Add(-30 * time.Second).Unix(), // allow small clock drift
		"exp": now.Add(9 * time.Minute).Unix(),   // GitHub max is 10 minutes
		"iss": g.appID,
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := b64url(hb) + "." + b64url(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// InstallationToken returns a valid installation access token, minting a new one
// (and caching it) when the cached one is missing or near expiry.
func (g *GitHubApp) InstallationToken(installationID int64) (string, error) {
	g.mu.Lock()
	if c, ok := g.cache[installationID]; ok && time.Until(c.expires) > 2*time.Minute {
		g.mu.Unlock()
		return c.token, nil
	}
	g.mu.Unlock()

	jwt, err := g.appJWT()
	if err != nil {
		return "", err
	}
	url := "https://api.github.com/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := g.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github app: installation token request -> %d", resp.StatusCode)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}

	g.mu.Lock()
	g.cache[installationID] = cachedToken{token: out.Token, expires: out.ExpiresAt}
	g.mu.Unlock()
	return out.Token, nil
}

// RepoInstallationToken resolves the App installation that covers owner/repo
// and mints an installation token for it. This handles repos owned by any
// account (personal or org) the App is installed on, without needing to know
// the installation id ahead of time.
func (g *GitHubApp) RepoInstallationToken(owner, repo string) (string, error) {
	jwt, err := g.appJWT()
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/installation", owner, repo)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := g.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("no installation for %s/%s (status %d)", owner, repo, resp.StatusCode)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return g.InstallationToken(out.ID)
}

// Repo is the subset of a GitHub repository the picker needs.
type Repo struct {
	FullName    string  `json:"full_name"`
	Name        string  `json:"name"`
	Owner       owner   `json:"owner"`
	Description *string `json:"description"`
	Language    *string `json:"language"`
	PushedAt    string  `json:"pushed_at"`
	Private     bool    `json:"private"`
}

type owner struct {
	Login string `json:"login"`
}

// AccessibleRepos lists every repository the App can access for this user,
// across their installations (private and org repos included), newest-push
// first. When a user OAuth token is given it scopes to the user's own
// installations; otherwise it falls back to all of the App's installations.
func (g *GitHubApp) AccessibleRepos(userToken string) ([]Repo, error) {
	ids := g.userInstallationIDs(userToken)
	if len(ids) == 0 {
		ids = g.allInstallationIDs()
	}

	seen := map[string]bool{}
	var out []Repo
	for _, id := range ids {
		tok, err := g.InstallationToken(id)
		if err != nil {
			continue
		}
		for _, r := range g.installationRepos(tok) {
			if !seen[r.FullName] {
				seen[r.FullName] = true
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PushedAt > out[j].PushedAt })
	return out, nil
}

// userInstallationIDs returns the installation ids the user belongs to.
func (g *GitHubApp) userInstallationIDs(userToken string) []int64 {
	if userToken == "" {
		return nil
	}
	var body struct {
		Installations []struct {
			ID int64 `json:"id"`
		} `json:"installations"`
	}
	if err := g.ghGET("https://api.github.com/user/installations?per_page=100", "token "+userToken, &body); err != nil {
		return nil
	}
	ids := make([]int64, 0, len(body.Installations))
	for _, i := range body.Installations {
		ids = append(ids, i.ID)
	}
	return ids
}

// allInstallationIDs returns every installation of the App.
func (g *GitHubApp) allInstallationIDs() []int64 {
	jwt, err := g.appJWT()
	if err != nil {
		return nil
	}
	var arr []struct {
		ID int64 `json:"id"`
	}
	if err := g.ghGET("https://api.github.com/app/installations?per_page=100", "Bearer "+jwt, &arr); err != nil {
		return nil
	}
	ids := make([]int64, 0, len(arr))
	for _, i := range arr {
		ids = append(ids, i.ID)
	}
	return ids
}

// installationRepos lists the repositories an installation token can access.
func (g *GitHubApp) installationRepos(instToken string) []Repo {
	var repos []Repo
	for page := 1; page <= 5; page++ { // up to 500 repos
		var body struct {
			Repositories []Repo `json:"repositories"`
		}
		url := fmt.Sprintf("https://api.github.com/installation/repositories?per_page=100&page=%d", page)
		if err := g.ghGET(url, "token "+instToken, &body); err != nil {
			break
		}
		repos = append(repos, body.Repositories...)
		if len(body.Repositories) < 100 {
			break
		}
	}
	return repos
}

// ghGET performs an authenticated GitHub GET and decodes JSON into out.
func (g *GitHubApp) ghGET(url, auth string, out any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s -> %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
