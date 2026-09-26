package api

import (
	"encoding/json"
	"io"
	"net/http"
)

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

// decode reads a JSON request body (capped at 8 MiB) into v.
func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(v)
}
