package api

import (
	"net/http"

	"servd/platform/internal/secret"
)

type githubTokenRequest struct {
	Token string `json:"token"`
}

// storeGitHubToken saves the caller's GitHub OAuth token (encrypted) so the
// deploy engine can clone their private repositories.
func (s *Server) storeGitHubToken(w http.ResponseWriter, r *http.Request) {
	if len(s.encKey) != 32 {
		writeError(w, http.StatusServiceUnavailable, "token storage not configured (set ENCRYPTION_KEY)")
		return
	}
	var req githubTokenRequest
	if err := decode(r, &req); err != nil || req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	enc, err := secret.Encrypt(s.encKey, req.Token)
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
func (s *Server) storeInstallation(w http.ResponseWriter, r *http.Request) {
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
func (s *Server) listGitHubRepos(w http.ResponseWriter, r *http.Request) {
	if s.deployer == nil || s.deployer.GitHubApp() == nil {
		writeJSON(w, http.StatusOK, map[string]any{"repos": []any{}})
		return
	}
	// The user's OAuth token, when present, scopes the list to their own
	// installations rather than all of the App's.
	var userToken string
	if len(s.encKey) == 32 {
		if enc, _ := s.store.GetToken(userFrom(r)); enc != "" {
			if t, err := secret.Decrypt(s.encKey, enc); err == nil {
				userToken = t
			}
		}
	}
	repos, err := s.deployer.GitHubApp().AccessibleRepos(userToken)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not list repositories")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}
