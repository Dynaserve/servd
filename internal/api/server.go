// Package api serves the platform's REST API.
package api

import (
	"net/http"

	"servd/platform/internal/deploy"
	"servd/platform/internal/proxy"
	"servd/platform/internal/store"
)

// Server holds the dependencies the HTTP handlers use.
type Server struct {
	store         store.Storer
	proxy         *proxy.Manager
	deployer      *deploy.Deployer // nil when docker is unavailable
	sessionSecret string           // shared with the frontend; empty = dev auth
	encKey        []byte           // 32 bytes; encrypts stored GitHub tokens
	region        string           // advertised in the X-Region response header
}

// Config wires a Server.
type Config struct {
	Store         store.Storer
	Proxy         *proxy.Manager
	Deployer      *deploy.Deployer // optional
	SessionSecret string
	EncKey        []byte
	Region        string
}

// New builds a Server from cfg.
func New(cfg Config) *Server {
	return &Server{
		store:         cfg.Store,
		proxy:         cfg.Proxy,
		deployer:      cfg.Deployer,
		sessionSecret: cfg.SessionSecret,
		encKey:        cfg.EncKey,
		region:        cfg.Region,
	}
}

// Routes returns the API handler with CORS and auth applied.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /api/v1/workspaces", s.listWorkspaces)
	mux.HandleFunc("POST /api/v1/workspaces", s.createWorkspace)
	mux.HandleFunc("DELETE /api/v1/workspaces/{wid}", s.deleteWorkspace)

	mux.HandleFunc("GET /api/v1/workspaces/{wid}/services", s.listServices)
	mux.HandleFunc("PUT /api/v1/workspaces/{wid}/services", s.replaceServices)
	mux.HandleFunc("POST /api/v1/workspaces/{wid}/services", s.createService)

	mux.HandleFunc("GET /api/v1/services/{id}", s.getService)
	mux.HandleFunc("PATCH /api/v1/services/{id}", s.patchService)
	mux.HandleFunc("DELETE /api/v1/services/{id}", s.deleteService)
	mux.HandleFunc("POST /api/v1/services/{id}/deploy", s.deployService)
	mux.HandleFunc("GET /api/v1/services/{id}/logs", s.serviceLogs)
	mux.HandleFunc("POST /api/v1/services/{id}/expose", s.exposeService)
	mux.HandleFunc("POST /api/v1/services/{id}/unexpose", s.unexposeService)

	mux.HandleFunc("POST /api/v1/github/token", s.storeGitHubToken)
	mux.HandleFunc("POST /api/v1/github/installation", s.storeInstallation)
	mux.HandleFunc("GET /api/v1/github/repos", s.listGitHubRepos)

	return withCORS(s.region, authMiddleware(s.sessionSecret, mux))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "servd-platform"})
}
