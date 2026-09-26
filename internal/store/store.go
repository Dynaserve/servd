// Package store persists services, workspaces and per-user GitHub credentials.
package store

import (
	"errors"

	"servd/platform/internal/ids"
)

// Service is stored as an opaque JSON object so the store never has to track
// the frontend's evolving card shape. The only field the backend relies on is
// "id"; everything else is passed through untouched.
type Service = map[string]any

// ErrNotFound is returned when a service id does not exist for a user.
var ErrNotFound = errors.New("service not found")

// Storer is the persistence contract the API depends on. Two implementations
// exist: FileStore (JSON file, zero dependencies) and PostgresStore
// (PostgreSQL). The one used is chosen at startup by whether DATABASE_URL is set.
type Storer interface {
	// ListServices returns a workspace's services for a user (never nil).
	ListServices(user, workspace string) ([]Service, error)
	// ReplaceServices overwrites a workspace's services (snapshot sync).
	ReplaceServices(user, workspace string, services []Service) ([]Service, error)
	// CreateService appends a service, assigning an id if absent.
	CreateService(user, workspace string, svc Service) (Service, error)
	// GetService finds a service by id and returns it with its workspace.
	GetService(user, id string) (Service, string, error)
	// PatchService merges fields into a service (id is immutable).
	PatchService(user, id string, patch map[string]any) (Service, error)
	// DeleteService removes a service by id.
	DeleteService(user, id string) error
	// AllServices returns every service across all users and workspaces,
	// with its owner.
	AllServices() ([]Owned, error)

	// SetToken stores a user's (already-encrypted) GitHub token.
	SetToken(user, encToken string) error
	// GetToken returns a user's encrypted GitHub token, or "" if none.
	GetToken(user string) (string, error)

	// SetInstallation stores a user's GitHub App installation id.
	SetInstallation(user string, installationID int64) error
	// GetInstallation returns a user's installation id, or 0 if none.
	GetInstallation(user string) (int64, error)

	// ListWorkspaces returns a user's workspaces, seeding the defaults on first
	// access so a new user starts with something.
	ListWorkspaces(user string) ([]Workspace, error)
	// CreateWorkspace adds a workspace for a user.
	CreateWorkspace(user string, ws Workspace) error
	// DeleteWorkspace removes a workspace record for a user.
	DeleteWorkspace(user, identifier string) error
}

// Owned is a service together with the user it belongs to.
type Owned struct {
	User    string
	Service Service
}

// Workspace is a grouping of services on the dashboard.
type Workspace struct {
	Identifier  string `json:"identifier" bson:"identifier"`
	Label       string `json:"label" bson:"label"`
	Description string `json:"description" bson:"description"`
	Icon        string `json:"icon" bson:"icon"`
}

// DefaultWorkspaces seed a brand-new user's dashboard.
var DefaultWorkspaces = []Workspace{
	{"workflows", "Workflows", "Build & run the processes that move work forward.", "Route"},
	{"clients", "Clients", "Manage the people & accounts you serve.", "Users"},
	{"organizations", "Organizations", "Group clients & teams under their parent orgs.", "Building2"},
}

// backendOwnedKeys are fields the deploy engine owns. A client snapshot sync
// (ReplaceServices) must not overwrite them, or a debounced save from the
// dashboard would wipe live deploy state (status, URL, logs, container id…).
var backendOwnedKeys = []string{
	"status", "publicUrl", "localUrl", "hostPort", "containerId",
	"exposed", "proxyTarget", "framework", "logs", "hostname", "image", "runtime",
	"deployment",
}

// preserveBackendFields makes backend-owned keys of each incoming service
// come from the stored copy (looked up by id), never from the client: a sync
// keeps live deploy state intact, and a client can't forge it (e.g. point
// proxyTarget at another customer's app).
func preserveBackendFields(incoming []Service, existingByID map[string]Service) {
	for _, svc := range incoming {
		StripBackendFields(svc)
		old, ok := existingByID[IDOf(svc)]
		if !ok {
			continue
		}
		for _, k := range backendOwnedKeys {
			if v, present := old[k]; present {
				svc[k] = v
			}
		}
	}
}

// StripBackendFields removes keys only the platform may set from
// client-supplied service data.
func StripBackendFields(svc map[string]any) {
	for _, k := range backendOwnedKeys {
		delete(svc, k)
	}
}

// IDOf returns a service's id, or "" if it has none.
func IDOf(svc Service) string {
	if svc == nil {
		return ""
	}
	if id, ok := svc["id"].(string); ok {
		return id
	}
	return ""
}

// ensureID assigns a fresh id to a service that lacks one.
func ensureID(svc Service) {
	if IDOf(svc) == "" {
		svc["id"] = ids.New()
	}
}
