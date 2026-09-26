package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// newID returns a random 16-char hex id for services and deployments.
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// userFrom returns the caller's authenticated identity, set by authMiddleware.
// It is the verified session user (secure mode) or the X-User header (dev).
func userFrom(r *http.Request) string {
	if u, ok := r.Context().Value(userCtxKey).(string); ok && u != "" {
		return u
	}
	return "public"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
