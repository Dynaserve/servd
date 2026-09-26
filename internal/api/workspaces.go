package api

import (
	"net/http"
	"strconv"
	"strings"

	"servd/platform/internal/ids"
	"servd/platform/internal/store"
)

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
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
	ws := store.Workspace{Identifier: identifier, Label: strings.TrimSpace(req.Label), Description: req.Description, Icon: icon}
	if err := s.store.CreateWorkspace(user, ws); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create workspace")
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	wid := r.PathValue("wid")

	// Tear down every service in the workspace before removing it.
	if services, err := s.store.ListServices(user, wid); err == nil {
		for _, svc := range services {
			id := store.IDOf(svc)
			s.teardown(user, id)
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
func uniqueWorkspaceID(user string, st store.Storer, base string) string {
	if base == "" {
		return ""
	}
	existing, _ := st.ListWorkspaces(user)
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
	return base + "-" + ids.New()
}
