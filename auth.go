package main

import (
	"context"
	"net/http"
	"strings"
)

type ctxKey int

const userCtxKey ctxKey = iota

// authMiddleware authenticates every /api/v1 request and stashes the caller's
// identity in the request context. Two modes:
//
//   - secure (SESSION_SECRET set): require a valid signed session, from the
//     `session` cookie or an Authorization: Bearer token. The X-User header is
//     ignored — identity comes only from the verified session.
//   - dev (SESSION_SECRET empty): trust the X-User header (or "public").
//
// /health is always open; CORS preflight is handled before this runs.
func authMiddleware(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || !strings.HasPrefix(r.URL.Path, "/api/v1/") {
			next.ServeHTTP(w, r)
			return
		}

		user, ok := resolveUser(secret, r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// resolveUser returns the authenticated user login and whether auth succeeded.
func resolveUser(secret string, r *http.Request) (string, bool) {
	if secret == "" {
		// Dev mode: trust the header.
		if u := r.Header.Get("X-User"); u != "" {
			return u, true
		}
		return "public", true
	}

	token := sessionToken(r)
	if token == "" {
		return "", false
	}
	u, err := verifySession(secret, token)
	if err != nil {
		return "", false
	}
	return u.Login, true
}

// sessionToken pulls the session token from the cookie or a Bearer header.
func sessionToken(r *http.Request) string {
	if c, err := r.Cookie("session"); err == nil && c.Value != "" {
		return c.Value
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}
