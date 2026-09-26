package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"servd/platform/internal/deploy"
	"servd/platform/internal/ids"
	"servd/platform/internal/proxy"
	"servd/platform/internal/store"
)

func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
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
func (s *Server) replaceServices(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) createService(w http.ResponseWriter, r *http.Request) {
	wid := r.PathValue("wid")
	var svc store.Service
	if err := decode(r, &svc); err != nil {
		writeError(w, http.StatusBadRequest, "invalid service body")
		return
	}
	store.StripBackendFields(svc)
	created, err := s.store.CreateService(userFrom(r), wid, svc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create service")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) getService(w http.ResponseWriter, r *http.Request) {
	svc, ws, err := s.store.GetService(userFrom(r), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": svc, "workspace": ws})
}

func (s *Server) patchService(w http.ResponseWriter, r *http.Request) {
	var patch map[string]any
	if err := decode(r, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid patch body")
		return
	}
	store.StripBackendFields(patch)
	svc, err := s.store.PatchService(userFrom(r), r.PathValue("id"), patch)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update service")
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

func (s *Server) deleteService(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id := r.PathValue("id")
	// Only the owner's own service is touched: check before tearing down.
	if _, _, err := s.store.GetService(user, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	s.teardown(user, id)
	err := s.store.DeleteService(user, id)
	if errors.Is(err, store.ErrNotFound) {
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

func (s *Server) deployService(w http.ResponseWriter, r *http.Request) {
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
			"id":        ids.New(),
			"startedAt": time.Now().UnixMilli(),
			"trigger":   trigger,
		},
		"status": "building",
		"logs":   []any{}, // fresh log stream for this deploy
	})
	if errors.Is(err, store.ErrNotFound) {
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

// serviceLogs returns the recent stdout/stderr of a service's running
// container — the live runtime logs shown in the dashboard's Logs panel.
func (s *Server) serviceLogs(w http.ResponseWriter, r *http.Request) {
	svc, _, err := s.store.GetService(userFrom(r), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
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
func (s *Server) exposeService(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id := r.PathValue("id")

	// On a public server (domain mode) apps are exposed by their deploys only:
	// an arbitrary target would let a customer proxy the internet to another
	// customer's app or to internal addresses.
	if s.proxy.DomainMode() {
		writeError(w, http.StatusForbidden, "apps are made public automatically when deployed")
		return
	}
	var req exposeRequest
	if err := decode(r, &req); err != nil || req.Target == "" {
		writeError(w, http.StatusBadRequest, "target is required, e.g. {\"target\":\"http://localhost:3000\"}")
		return
	}
	current, _, err := s.store.GetService(user, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}

	exp, err := s.proxy.Expose(deploy.ServiceRoute(user, current, req.Target))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	url := exp.URL
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

func (s *Server) unexposeService(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id := r.PathValue("id")
	if _, _, err := s.store.GetService(user, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	s.proxy.Unexpose(deploy.Key(user, id))
	svc, err := s.store.PatchService(user, id, map[string]any{
		"exposed":   false,
		"publicUrl": "",
	})
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "unexposed", "service": svc})
}

// teardown stops a service's app, takes it offline and frees its custom
// domains. Callers have checked that user owns the service.
func (s *Server) teardown(user, id string) {
	if s.deployer != nil {
		s.deployer.Stop(user, id)
	} else {
		s.proxy.Unexpose(deploy.Key(user, id))
	}
	s.proxy.Release(deploy.Key(user, id))
}

type domainsRequest struct {
	Domains []string `json:"domains"`
}

// setDomains attaches the customer's own domains to a service. Each domain
// needs a DNS record (CNAME to the service's hostname, or A to this server);
// HTTPS certificates are issued automatically on the first visit. A running
// service picks the change up immediately.
func (s *Server) setDomains(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id := r.PathValue("id")
	if !s.proxy.DomainMode() {
		writeError(w, http.StatusConflict, "custom domains need APPS_DOMAIN to be configured")
		return
	}
	var req domainsRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "expected {\"domains\":[\"www.example.com\"]}")
		return
	}
	if len(req.Domains) > 20 {
		writeError(w, http.StatusBadRequest, "at most 20 domains per service")
		return
	}
	domains := []any{}
	for _, d := range req.Domains {
		nd, err := proxy.NormalizeDomain(d)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		domains = append(domains, nd)
	}
	svc, err := s.store.PatchService(user, id, map[string]any{"domains": domains})
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save domains")
		return
	}
	resp := map[string]any{"domains": domains, "rejected": map[string]string{}, "service": svc}
	if exposed, _ := svc["exposed"].(bool); exposed {
		if target, _ := svc["proxyTarget"].(string); target != "" {
			exp, err := s.proxy.Expose(deploy.ServiceRoute(user, svc, target))
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			resp["rejected"] = exp.Rejected
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseServices accepts a bare JSON array or an object with a "services" key.
func parseServices(body []byte) ([]store.Service, error) {
	var wrapped struct {
		Services []store.Service `json:"services"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Services != nil {
		return wrapped.Services, nil
	}
	var arr []store.Service
	if err := json.Unmarshal(body, &arr); err != nil {
		return nil, err
	}
	return arr, nil
}
