package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore persists services in PostgreSQL. Each service is one row in the
// `services` table; the flexible frontend card lives in a JSONB `data` column,
// while `account` (the user) and `workspace` are first-class indexed columns
// for scoping and lookups.
type PostgresStore struct {
	pool *pgxpool.Pool
}

const schemaDDL = `
CREATE TABLE IF NOT EXISTS services (
    id         text        PRIMARY KEY,
    account    text        NOT NULL,
    workspace  text        NOT NULL,
    data       jsonb       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS services_account_workspace_idx ON services (account, workspace);
CREATE INDEX IF NOT EXISTS services_account_idx           ON services (account);

CREATE TABLE IF NOT EXISTS github_tokens (
    account    text        PRIMARY KEY,
    token      text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS github_installations (
    account         text        PRIMARY KEY,
    installation_id bigint      NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS workspaces (
    account     text        NOT NULL,
    identifier  text        NOT NULL,
    label       text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    icon        text        NOT NULL DEFAULT '',
    position    serial,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account, identifier)
);
`

// NewPostgresStore connects to Postgres, verifies the connection, and creates
// the schema if it does not exist.
func NewPostgresStore(url string) (*PostgresStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if _, err := pool.Exec(ctx, schemaDDL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &PostgresStore{pool: pool}, nil
}

func pgCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func (p *PostgresStore) ListServices(user, workspace string) ([]Service, error) {
	c, cancel := pgCtx()
	defer cancel()
	rows, err := p.pool.Query(c,
		`SELECT data FROM services WHERE account=$1 AND workspace=$2 ORDER BY created_at, id`,
		user, workspace)
	if err != nil {
		return nil, err
	}
	return scanServices(rows)
}

func (p *PostgresStore) ReplaceServices(user, workspace string, services []Service) ([]Service, error) {
	c, cancel := pgCtx()
	defer cancel()

	tx, err := p.pool.Begin(c)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(c) //nolint:errcheck // no-op after a successful commit

	// Preserve backend-owned deploy state from the current rows.
	existing := map[string]Service{}
	rows, err := tx.Query(c, `SELECT data FROM services WHERE account=$1 AND workspace=$2`, user, workspace)
	if err != nil {
		return nil, err
	}
	current, err := scanServices(rows)
	if err != nil {
		return nil, err
	}
	for _, svc := range current {
		existing[idOf(svc)] = svc
	}

	if _, err := tx.Exec(c, `DELETE FROM services WHERE account=$1 AND workspace=$2`, user, workspace); err != nil {
		return nil, err
	}
	for _, svc := range services {
		if idOf(svc) == "" {
			svc["id"] = newID()
		}
	}
	preserveBackendFields(services, existing)
	for _, svc := range services {
		raw, err := json.Marshal(svc)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(c,
			`INSERT INTO services (id, account, workspace, data) VALUES ($1,$2,$3,$4)`,
			idOf(svc), user, workspace, raw); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(c); err != nil {
		return nil, err
	}
	return services, nil
}

func (p *PostgresStore) CreateService(user, workspace string, svc Service) (Service, error) {
	c, cancel := pgCtx()
	defer cancel()
	if idOf(svc) == "" {
		svc["id"] = newID()
	}
	raw, err := json.Marshal(svc)
	if err != nil {
		return nil, err
	}
	if _, err := p.pool.Exec(c,
		`INSERT INTO services (id, account, workspace, data) VALUES ($1,$2,$3,$4)`,
		idOf(svc), user, workspace, raw); err != nil {
		return nil, err
	}
	return svc, nil
}

func (p *PostgresStore) GetService(user, id string) (Service, string, error) {
	c, cancel := pgCtx()
	defer cancel()
	var raw []byte
	var workspace string
	err := p.pool.QueryRow(c,
		`SELECT data, workspace FROM services WHERE id=$1 AND account=$2`, id, user).
		Scan(&raw, &workspace)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	svc, err := unmarshalService(raw)
	return svc, workspace, err
}

func (p *PostgresStore) PatchService(user, id string, patch map[string]any) (Service, error) {
	c, cancel := pgCtx()
	defer cancel()

	// id is immutable; drop it so the JSONB merge never rewrites it.
	clean := make(map[string]any, len(patch))
	for k, v := range patch {
		if k != "id" {
			clean[k] = v
		}
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return nil, err
	}

	// `data || patch` is Postgres's atomic shallow merge of top-level keys —
	// exactly the patch semantics the API promises.
	var out []byte
	err = p.pool.QueryRow(c,
		`UPDATE services SET data = data || $2::jsonb, updated_at = now()
		 WHERE id=$1 AND account=$3 RETURNING data`,
		id, raw, user).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return unmarshalService(out)
}

func (p *PostgresStore) DeleteService(user, id string) error {
	c, cancel := pgCtx()
	defer cancel()
	tag, err := p.pool.Exec(c, `DELETE FROM services WHERE id=$1 AND account=$2`, id, user)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PostgresStore) AllServices() ([]Service, error) {
	c, cancel := pgCtx()
	defer cancel()
	rows, err := p.pool.Query(c, `SELECT data FROM services`)
	if err != nil {
		return nil, err
	}
	return scanServices(rows)
}

// SetToken upserts a user's encrypted GitHub token.
func (p *PostgresStore) SetToken(user, encToken string) error {
	c, cancel := pgCtx()
	defer cancel()
	_, err := p.pool.Exec(c,
		`INSERT INTO github_tokens (account, token, updated_at) VALUES ($1,$2,now())
		 ON CONFLICT (account) DO UPDATE SET token=EXCLUDED.token, updated_at=now()`,
		user, encToken)
	return err
}

// GetToken returns a user's encrypted GitHub token, or "" if none.
func (p *PostgresStore) GetToken(user string) (string, error) {
	c, cancel := pgCtx()
	defer cancel()
	var token string
	err := p.pool.QueryRow(c, `SELECT token FROM github_tokens WHERE account=$1`, user).Scan(&token)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return token, err
}

// SetInstallation upserts a user's GitHub App installation id.
func (p *PostgresStore) SetInstallation(user string, installationID int64) error {
	c, cancel := pgCtx()
	defer cancel()
	_, err := p.pool.Exec(c,
		`INSERT INTO github_installations (account, installation_id, updated_at) VALUES ($1,$2,now())
		 ON CONFLICT (account) DO UPDATE SET installation_id=EXCLUDED.installation_id, updated_at=now()`,
		user, installationID)
	return err
}

// GetInstallation returns a user's installation id, or 0 if none.
func (p *PostgresStore) GetInstallation(user string) (int64, error) {
	c, cancel := pgCtx()
	defer cancel()
	var id int64
	err := p.pool.QueryRow(c, `SELECT installation_id FROM github_installations WHERE account=$1`, user).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// ListWorkspaces returns a user's workspaces, seeding defaults on first access.
func (p *PostgresStore) ListWorkspaces(user string) ([]Workspace, error) {
	c, cancel := pgCtx()
	defer cancel()

	rows, err := p.pool.Query(c,
		`SELECT identifier, label, description, icon FROM workspaces WHERE account=$1 ORDER BY position`, user)
	if err != nil {
		return nil, err
	}
	out, err := scanWorkspaces(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		for _, ws := range defaultWorkspaces {
			if err := p.CreateWorkspace(user, ws); err != nil {
				return nil, err
			}
		}
		return append([]Workspace(nil), defaultWorkspaces...), nil
	}
	return out, nil
}

// CreateWorkspace inserts a workspace for a user (no-op if identifier exists).
func (p *PostgresStore) CreateWorkspace(user string, ws Workspace) error {
	c, cancel := pgCtx()
	defer cancel()
	_, err := p.pool.Exec(c,
		`INSERT INTO workspaces (account, identifier, label, description, icon) VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (account, identifier) DO UPDATE SET label=EXCLUDED.label, description=EXCLUDED.description, icon=EXCLUDED.icon`,
		user, ws.Identifier, ws.Label, ws.Description, ws.Icon)
	return err
}

// DeleteWorkspace removes a workspace record for a user.
func (p *PostgresStore) DeleteWorkspace(user, identifier string) error {
	c, cancel := pgCtx()
	defer cancel()
	_, err := p.pool.Exec(c, `DELETE FROM workspaces WHERE account=$1 AND identifier=$2`, user, identifier)
	return err
}

func scanWorkspaces(rows pgx.Rows) ([]Workspace, error) {
	defer rows.Close()
	out := []Workspace{}
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(&w.Identifier, &w.Label, &w.Description, &w.Icon); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func scanServices(rows pgx.Rows) ([]Service, error) {
	defer rows.Close()
	out := []Service{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		svc, err := unmarshalService(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

func unmarshalService(raw []byte) (Service, error) {
	var svc Service
	if err := json.Unmarshal(raw, &svc); err != nil {
		return nil, err
	}
	return svc, nil
}
