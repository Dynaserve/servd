package main

import "net/http"

// brandName is the public product name the platform advertises in the
// Server and X-Powered-By headers, overriding whatever the API or a proxied
// app would otherwise report.
const brandName = "Dynaserve"

// region is advertised in the X-Region header on every response. Defaults to
// "AU"; main overrides it from the REGION environment variable.
var region = "AU"

// withCORS allows the frontend (any origin, in this dev build) to call the API
// directly from the browser, including preflight requests.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Powered-By", brandName)
		h.Set("Server", brandName)
		h.Set("X-Region", region)
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Vary", "Origin")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-User")
		// Allow the browser to send the session cookie cross-origin (:3000 -> :8080).
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Access-Control-Max-Age", "600")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
