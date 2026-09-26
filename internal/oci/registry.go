package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Manifest media types we accept, newest first.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// registry is a minimal read-only registry client (anonymous bearer-token
// auth, which covers public images on Docker Hub, GHCR, GCR, Quay, …).
type registry struct {
	http *http.Client
	// mirror, when set, is tried first for Docker Hub images (e.g.
	// "mirror.gcr.io"), falling back to Docker Hub itself.
	mirror string

	mu     sync.Mutex
	tokens map[string]string // host+scope -> bearer token
}

func newRegistry(mirror string) *registry {
	return &registry{
		http:   &http.Client{Timeout: 10 * time.Minute},
		mirror: mirror,
		tokens: map[string]string{},
	}
}

// hosts returns the hosts to try for ref, in order.
func (r *registry) hosts(ref Ref) []string {
	if ref.Registry == dockerHub && r.mirror != "" {
		return []string{r.mirror, dockerHub}
	}
	return []string{ref.Registry}
}

// get fetches /v2/<repo>/<kind>/<reference> from the first host that has it.
func (r *registry) get(ctx context.Context, ref Ref, kind, reference, accept string) (*http.Response, error) {
	var lastErr error
	for _, host := range r.hosts(ref) {
		resp, err := r.getFrom(ctx, host, ref.Repo, kind, reference, accept)
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (r *registry) getFrom(ctx context.Context, host, repo, kind, reference, accept string) (*http.Response, error) {
	u := fmt.Sprintf("https://%s/v2/%s/%s/%s", host, repo, kind, reference)
	scope := "repository:" + repo + ":pull"
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		r.mu.Lock()
		tok := r.tokens[host+" "+scope]
		r.mu.Unlock()
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := r.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			challenge := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			tok, err := r.token(ctx, challenge, scope)
			if err != nil {
				return nil, fmt.Errorf("%s: auth: %w", host, err)
			}
			r.mu.Lock()
			r.tokens[host+" "+scope] = tok
			r.mu.Unlock()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("%s: GET %s/%s/%s: %s", host, repo, kind, reference, resp.Status)
		}
		return resp, nil
	}
	return nil, fmt.Errorf("%s: unauthorized", host)
}

// token performs the anonymous bearer-token dance for a WWW-Authenticate
// challenge such as: Bearer realm="https://auth.docker.io/token",service="registry.docker.io".
func (r *registry) token(ctx context.Context, challenge, scope string) (string, error) {
	scheme, params, ok := strings.Cut(challenge, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("unsupported auth challenge %q", challenge)
	}
	p := parseChallenge(params)
	if p["realm"] == "" {
		return "", fmt.Errorf("auth challenge has no realm")
	}
	q := url.Values{}
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	if p["scope"] != "" {
		scope = p["scope"]
	}
	q.Set("scope", scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p["realm"]+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint: %s", resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if body.Token != "" {
		return body.Token, nil
	}
	return body.AccessToken, nil
}

// parseChallenge parses `k="v",k2="v2"` auth parameters.
func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		k, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		k = strings.TrimSpace(strings.TrimLeft(k, ", "))
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				break
			}
			v, s = rest[1:end+1], rest[end+2:]
		} else {
			v, s, _ = strings.Cut(rest, ",")
		}
		out[strings.ToLower(k)] = v
	}
	return out
}
