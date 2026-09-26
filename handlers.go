package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type server struct {
	store         Storer
	proxy         *ProxyManager
	deployer      *Deployer // nil when docker is unavailable
	sessionSecret string    // shared with the frontend; empty = dev auth
	encKey        []byte    // 32 bytes; encrypts stored GitHub tokens
}

func (s *server) routes() http.Handler {
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

	return withCORS(authMiddleware(s.sessionSecret, mux))
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "servd-platform"})
}

func (s *server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	ws, err := s.store.ListWorkspaces(userFrom(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list workspaces")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": ws})
}

type createWorkspaceRequest struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
}

func (s *server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var req createWorkspaceRequest
	if err := decode(r, &req); err != nil || strings.TrimSpace(req.Label) == "" {
		writeError(w, http.StatusBadRequest, "label is required")
		return
	}
	user := userFrom(r)
	identifier := uniqueWorkspaceID(user, s.store, slugify(req.Label))
	if identifier == "" {
		writeError(w, http.StatusBadRequest, "label must contain letters or digits")
		return
	}
	icon := req.Icon
	if icon == "" {
		icon = "Folder"
	}
	ws := Workspace{Identifier: identifier, Label: strings.TrimSpace(req.Label), Description: req.Description, Icon: icon}
	if err := s.store.CreateWorkspace(user, ws); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create workspace")
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

func (s *server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	wid := r.PathValue("wid")

	// Tear down every service in the workspace before removing it.
	if services, err := s.store.ListServices(user, wid); err == nil {
		for _, svc := range services {
			id := idOf(svc)
			if s.deployer != nil {
				s.deployer.Stop(user, id)
			} else {
				s.proxy.Unexpose(id)
			}
			_ = s.store.DeleteService(user, id)
		}
	}
	if err := s.store.DeleteWorkspace(user, wid); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete workspace")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// slugify turns a label into a url-safe identifier.
func slugify(label string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(label) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// uniqueWorkspaceID appends a numeric suffix if the base slug is taken.
func uniqueWorkspaceID(user string, store Storer, base string) string {
	if base == "" {
		return ""
	}
	existing, _ := store.ListWorkspaces(user)
	taken := map[string]bool{}
	for _, ws := range existing {
		taken[ws.Identifier] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; i < 1000; i++ {
		candidate := base + "-" + strconv.Itoa(i)
		if !taken[candidate] {
			return candidate
		}
	}
	return base + "-" + newID()
}

func (s *server) listServices(w http.ResponseWriter, r *http.Request) {
	wid := r.PathValue("wid")
	services, err := s.store.ListServices(userFrom(r), wid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list services")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": services})
}

// replaceServices accepts either {"services":[...]} or a bare [...] array and
// overwrites the workspace's service list. This is the snapshot-sync endpoint
// the frontend canvas uses to persist its state.
func (s *server) replaceServices(w http.ResponseWriter, r *http.Request) {
	wid := r.PathValue("wid")
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	services, err := parseServices(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: expected an array of services or {\"services\":[...]}")
		return
	}
	saved, err := s.store.ReplaceServices(userFrom(r), wid, services)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist services")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": saved})
}

func (s *server) createService(w http.ResponseWriter, r *http.Request) {
	wid := r.PathValue("wid")
	var svc Service
	if err := decode(r, &svc); err != nil {
		writeError(w, http.StatusBadRequest, "invalid service body")
		return
	}
	created, err := s.store.CreateService(userFrom(r), wid, svc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create service")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *server) getService(w http.ResponseWriter, r *http.Request) {
	svc, ws, err := s.store.GetService(userFrom(r), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": svc, "workspace": ws})
}

func (s *server) patchService(w http.ResponseWriter, r *http.Request) {
	var patch map[string]any
	if err := decode(r, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid patch body")
		return
	}
	svc, err := s.store.PatchService(userFrom(r), r.PathValue("id"), patch)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update service")
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

func (s *server) deleteService(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id := r.PathValue("id")
	// Tear down any running container + proxy before removing the record.
	if s.deployer != nil {
		s.deployer.Stop(user, id)
	} else {
		s.proxy.Unexpose(id)
	}
	err := s.store.DeleteService(user, id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete service")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

type deployRequest struct {
	Trigger string `json:"trigger"`
}

func (s *server) deployService(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id := r.PathValue("id")
	var req deployRequest
	_ = decode(r, &req) // body is optional
	trigger := req.Trigger
	if trigger == "" {
		trigger = "Manual deploy"
	}
	svc, err := s.store.PatchService(user, id, map[string]any{
		"deployment": map[string]any{
			"id":        newID(),
			"startedAt": time.Now().UnixMilli(),
			"trigger":   trigger,
		},
		"status": "building",
		"logs":   []any{}, // fresh log stream for this deploy
	})
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start deploy")
		return
	}

	// Run the REAL pipeline (clone → build → isolated container → URL) when
	// docker is available; otherwise just record the deployment marker.
	if s.deployer != nil {
		go s.deployer.Deploy(user, svc)
	}
	writeJSON(w, http.StatusAccepted, svc)
}

type githubTokenRequest struct {
	Token string `json:"token"`
}

// storeGitHubToken saves the caller's GitHub OAuth token (encrypted) so the
// deploy engine can clone their private repositories.
func (s *server) storeGitHubToken(w http.ResponseWriter, r *http.Request) {
	if len(s.encKey) != 32 {
		writeError(w, http.StatusServiceUnavailable, "token storage not configured (set ENCRYPTION_KEY)")
		return
	}
	var req githubTokenRequest
	if err := decode(r, &req); err != nil || req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	enc, err := encrypt(s.encKey, req.Token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not secure token")
		return
	}
	if err := s.store.SetToken(userFrom(r), enc); err != nil {
		writeError(w, http.StatusInternalServerError, "could not store token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stored"})
}

type installationRequest struct {
	InstallationID int64 `json:"installationId"`
}

// storeInstallation records the caller's GitHub App installation id (set after
// they install the App and pick repos), enabling private-repo deploys.
func (s *server) storeInstallation(w http.ResponseWriter, r *http.Request) {
	var req installationRequest
	if err := decode(r, &req); err != nil || req.InstallationID == 0 {
		writeError(w, http.StatusBadRequest, "installationId is required")
		return
	}
	if err := s.store.SetInstallation(userFrom(r), req.InstallationID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not store installation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "connected"})
}

// listGitHubRepos returns the repositories the GitHub App can access for the
// caller (private and org repos included), for the repo picker. Returns an
// empty list when no App is configured, so the frontend can fall back to
// typing owner/repo.
func (s *server) listGitHubRepos(w http.ResponseWriter, r *http.Request) {
	if s.deployer == nil || s.deployer.githubApp == nil {
		writeJSON(w, http.StatusOK, map[string]any{"repos": []any{}})
		return
	}
	// The user's OAuth token, when present, scopes the list to their own
	// installations rather than all of the App's.
	var userToken string
	if len(s.encKey) == 32 {
		if enc, _ := s.store.GetToken(userFrom(r)); enc != "" {
			if t, err := decrypt(s.encKey, enc); err == nil {
				userToken = t
			}
		}
	}
	repos, err := s.deployer.githubApp.AccessibleRepos(userToken)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not list repositories")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}

// serviceLogs returns the recent stdout/stderr of a service's running
// container — the live runtime logs shown in the dashboard's Logs panel.
func (s *server) serviceLogs(w http.ResponseWriter, r *http.Request) {
	svc, _, err := s.store.GetService(userFrom(r), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	cid, _ := svc["containerId"].(string)
	if cid == "" || s.deployer == nil {
		writeJSON(w, http.StatusOK, map[string]any{"logs": ""})
		return
	}
	tail := 200
	if t := r.URL.Query().Get("tail"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n > 0 && n <= 1000 {
			tail = n
		}
	}
	out, _ := s.deployer.Logs(cid, tail)
	writeJSON(w, http.StatusOK, map[string]any{"logs": out})
}

type exposeRequest struct {
	Target string `json:"target"`
}

// exposeService starts a reverse proxy from a public port to a locally-running
// app (the `target`), so it can be reached at a real URL. The public URL and
// target are persisted on the service.
func (s *server) exposeService(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id := r.PathValue("id")

	var req exposeRequest
	if err := decode(r, &req); err != nil || req.Target == "" {
		writeError(w, http.StatusBadRequest, "target is required, e.g. {\"target\":\"http://localhost:3000\"}")
		return
	}
	if _, _, err := s.store.GetService(user, id); errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}

	url, err := s.proxy.Expose(id, req.Target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	svc, err := s.store.PatchService(user, id, map[string]any{
		"publicUrl":   url,
		"proxyTarget": req.Target,
		"exposed":     true,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "exposed but could not persist")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": url, "target": req.Target, "service": svc})
}

func (s *server) unexposeService(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id := r.PathValue("id")
	s.proxy.Unexpose(id)
	svc, err := s.store.PatchService(user, id, map[string]any{
		"exposed":   false,
		"publicUrl": "",
	})
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "unexposed", "service": svc})
}

// parseServices accepts a bare JSON array or an object with a "services" key.
func parseServices(body []byte) ([]Service, error) {
	var wrapped struct {
		Services []Service `json:"services"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Services != nil {
		return wrapped.Services, nil
	}
	var arr []Service
	if err := json.Unmarshal(body, &arr); err != nil {
		return nil, err
	}
	return arr, nil
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(v)
}
