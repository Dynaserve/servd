package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// FileStore is an in-memory, file-backed Storer, scoped by user and workspace.
// It is safe for concurrent use and persists every mutation to a JSON file so
// data survives restarts — no database required.
type FileStore struct {
	mu   sync.Mutex
	path string
	// data[user][workspace] = ordered list of services
	data map[string]map[string][]Service
	// tokens[user] = encrypted GitHub token
	tokens map[string]string
	// installs[user] = GitHub App installation id
	installs map[string]int64
	// spaces[user] = workspaces (nil until seeded)
	spaces map[string][]Workspace
}

// fileData is the on-disk shape of the file store.
type fileData struct {
	Services map[string]map[string][]Service `json:"services"`
	Tokens   map[string]string               `json:"tokens"`
	Installs map[string]int64                `json:"installs"`
	Spaces   map[string][]Workspace          `json:"spaces"`
}

// NewFileStore loads an existing store from path (if present) or starts empty.
func NewFileStore(path string) (*FileStore, error) {
	s := &FileStore{
		path:     path,
		data:     map[string]map[string][]Service{},
		tokens:   map[string]string{},
		installs: map[string]int64{},
		spaces:   map[string][]Workspace{},
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(b) > 0 {
		var fd fileData
		if err := json.Unmarshal(b, &fd); err != nil {
			return nil, err
		}
		if fd.Services != nil {
			s.data = fd.Services
		}
		if fd.Tokens != nil {
			s.tokens = fd.Tokens
		}
		if fd.Installs != nil {
			s.installs = fd.Installs
		}
		if fd.Spaces != nil {
			s.spaces = fd.Spaces
		}
	}
	return s, nil
}

// flush writes the current state to disk. Callers must hold s.mu.
func (s *FileStore) flush() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(fileData{Services: s.data, Tokens: s.tokens, Installs: s.installs, Spaces: s.spaces}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// SetToken stores a user's encrypted GitHub token.
func (s *FileStore) SetToken(user, encToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[user] = encToken
	return s.flush()
}

// GetToken returns a user's encrypted GitHub token, or "" if none.
func (s *FileStore) GetToken(user string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens[user], nil
}

// SetInstallation stores a user's GitHub App installation id.
func (s *FileStore) SetInstallation(user string, installationID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.installs[user] = installationID
	return s.flush()
}

// GetInstallation returns a user's installation id, or 0 if none.
func (s *FileStore) GetInstallation(user string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.installs[user], nil
}

// ListWorkspaces returns a user's workspaces, seeding defaults on first access.
func (s *FileStore) ListWorkspaces(user string) ([]Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spaces[user] == nil {
		seed := append([]Workspace(nil), DefaultWorkspaces...)
		s.spaces[user] = seed
		_ = s.flush()
	}
	out := make([]Workspace, len(s.spaces[user]))
	copy(out, s.spaces[user])
	return out, nil
}

// CreateWorkspace appends a workspace for a user.
func (s *FileStore) CreateWorkspace(user string, ws Workspace) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spaces[user] == nil {
		s.spaces[user] = append([]Workspace(nil), DefaultWorkspaces...)
	}
	s.spaces[user] = append(s.spaces[user], ws)
	return s.flush()
}

// DeleteWorkspace removes a workspace record for a user.
func (s *FileStore) DeleteWorkspace(user, identifier string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.spaces[user]
	for i, ws := range list {
		if ws.Identifier == identifier {
			s.spaces[user] = append(list[:i], list[i+1:]...)
			return s.flush()
		}
	}
	return nil
}

func (s *FileStore) userWorkspaces(user string) map[string][]Service {
	if s.data[user] == nil {
		s.data[user] = map[string][]Service{}
	}
	return s.data[user]
}

// ListServices returns the services in a workspace (never nil).
func (s *FileStore) ListServices(user, workspace string) ([]Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.userWorkspaces(user)[workspace]
	if out == nil {
		return []Service{}, nil
	}
	// Return a shallow copy so callers cannot mutate our slice header.
	cp := make([]Service, len(out))
	copy(cp, out)
	return cp, nil
}

// ReplaceServices overwrites a workspace's services (snapshot sync). Any
// service missing an id is assigned one.
func (s *FileStore) ReplaceServices(user, workspace string, services []Service) ([]Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Preserve backend-owned deploy state from the current services.
	existing := map[string]Service{}
	for _, svc := range s.userWorkspaces(user)[workspace] {
		existing[IDOf(svc)] = svc
	}
	for _, svc := range services {
		ensureID(svc)
	}
	preserveBackendFields(services, existing)

	s.userWorkspaces(user)[workspace] = services
	if err := s.flush(); err != nil {
		return nil, err
	}
	return services, nil
}

// CreateService appends a service to a workspace, assigning an id if absent.
func (s *FileStore) CreateService(user, workspace string, svc Service) (Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ensureID(svc)
	ws := s.userWorkspaces(user)
	ws[workspace] = append(ws[workspace], svc)
	if err := s.flush(); err != nil {
		return nil, err
	}
	return svc, nil
}

// GetService finds a service by id across all of a user's workspaces.
func (s *FileStore) GetService(user, id string) (Service, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ws, list := range s.userWorkspaces(user) {
		for _, svc := range list {
			if IDOf(svc) == id {
				return svc, ws, nil
			}
		}
	}
	return nil, "", ErrNotFound
}

// PatchService merges fields into an existing service and returns it.
func (s *FileStore) PatchService(user, id string, patch map[string]any) (Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, list := range s.userWorkspaces(user) {
		for _, svc := range list {
			if IDOf(svc) == id {
				for k, v := range patch {
					if k == "id" {
						continue // id is immutable
					}
					svc[k] = v
				}
				if err := s.flush(); err != nil {
					return nil, err
				}
				return svc, nil
			}
		}
	}
	return nil, ErrNotFound
}

// DeleteService removes a service by id.
func (s *FileStore) DeleteService(user, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws := s.userWorkspaces(user)
	for wsID, list := range ws {
		for i, svc := range list {
			if IDOf(svc) == id {
				ws[wsID] = append(list[:i], list[i+1:]...)
				return s.flush()
			}
		}
	}
	return ErrNotFound
}

// AllServices returns every service across all users and workspaces. Used on
// startup to restore reverse proxies for services that were exposed.
func (s *FileStore) AllServices() ([]Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Service
	for _, workspaces := range s.data {
		for _, list := range workspaces {
			out = append(out, list...)
		}
	}
	return out, nil
}
