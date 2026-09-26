# Servd Platform API

The Servd control-plane API — a single Go binary, no external services. It
persists services to a JSON file (no MongoDB) and does not require Docker, so it
runs anywhere Go runs. It serves the REST API the frontend canvas consumes.

## Run

```bash
cd platform
go build -o platform .
./platform
```

Listens on `:8080` (all interfaces), so it is reachable over the LAN at
`http://<server-ip>:8080` — e.g. `http://172.20.10.2:8080`.

Config (environment):

| Var              | Default             | Meaning                                          |
|------------------|---------------------|--------------------------------------------------|
| `LISTEN_ADDR`    | `:8080`             | Bind address                                     |
| `MONGO_URI`      | *(unset)*           | If set, persist to MongoDB instead of a file     |
| `MONGO_DATABASE` | `dynaserve`         | Mongo database name (when `MONGO_URI` is set)    |
| `DATA_FILE`      | `./data/store.json` | JSON store path (used only when `DATABASE_URL` unset) |
| `PUBLIC_HOST`    | `localhost`         | Hostname advertised in `expose`/deploy URLs      |
| `SESSION_SECRET` | *(unset)*           | Shared with the frontend; when set, the platform verifies the session cookie (real auth). Unset = dev auth (trusts `X-User`). |
| `ENCRYPTION_KEY` | *(unset)*           | Exactly 32 bytes; enables storing users' GitHub tokens (AES-256-GCM) so the deploy engine can clone private repos. |
| `REGION`         | `AU`                | Advertised in the `X-Region` response header. |
| `GITHUB_APP_ID`  | *(unset)*           | GitHub App id; enables private-repo clones via installation tokens (preferred over the OAuth token). |
| `GITHUB_APP_PRIVATE_KEY` / `GITHUB_APP_PRIVATE_KEY_PATH` | *(unset)* | The App's private key (PEM inline, or a path to the `.pem`). Required when `GITHUB_APP_ID` is set. |

### Private repos (GitHub App)

Login stays on the OAuth flow. For private-repo access the platform uses a
GitHub **App** with least-privilege, per-repo, read-only installation tokens:

1. Register a GitHub App (Repository permissions → **Contents: Read-only**,
   **Metadata: Read-only**). Set its **Setup URL** to
   `<frontend>/github/setup` and enable "Redirect on update". Generate a
   private key.
2. Platform env: `GITHUB_APP_ID` + `GITHUB_APP_PRIVATE_KEY_PATH`.
   Frontend env: `NEXT_PUBLIC_GITHUB_APP_SLUG` (for the install link).
3. In the dashboard's repo picker, "Deploy a private repository" installs the
   App (user picks repos) → GitHub redirects to `/github/setup` → the platform
   stores the installation id → private `owner/repo` deploys clone with a
   freshly-minted installation token.

When the App is not configured, deploys fall back to the stored user OAuth
token (public repos only).

### Auth

With `SESSION_SECRET` set (same value as the frontend's), every `/api/v1`
request must carry a valid signed session — the `session` cookie (sent by the
browser) or an `Authorization: Bearer <token>`. The `X-User` header can no
longer impersonate. Without `SESSION_SECRET`, it falls back to trusting
`X-User` (local dev only).

### Storage backends

The platform picks its store at startup:

- **`MONGO_URI` set** → MongoDB. Services are stored in the `services`
  collection as `{ _id, user, workspace, service }`, indexed on user/workspace.
- **unset** → the zero-dependency JSON file store (`DATA_FILE`).

Run Mongo in Docker and the platform on the host (so `expose` still works):

```bash
docker compose up -d mongo
cd platform && MONGO_URI=mongodb://localhost:27017 ./platform
```

## Run in Docker

The platform is a static, stdlib-only binary, so the image is tiny (distroless)
and needs no MongoDB or Docker-in-Docker. Start Docker Desktop / your VM first,
then from the repo root:

```bash
docker compose up --build
```

Or by hand:

```bash
cd platform
docker build -t servd-platform .
docker run -d --name servd-platform -p 8080:8080 -v servd-data:/data servd-platform
```

- The API is reachable on the host at `http://localhost:8080` — which is what
  the frontend's `NEXT_PUBLIC_PLATFORM_URL` already points to. Nothing to change.
- The store persists in the `servd-data` named volume, so services survive
  restarts.
- Stop anything already bound to host `:8080` first (e.g. a `./platform` you ran
  directly) — only one process can hold the port.
- If you later containerise the frontend too, keep `NEXT_PUBLIC_PLATFORM_URL` a
  **host-reachable** URL (`http://localhost:8080`), because that fetch runs in
  the browser, not inside the container network.

## Auth (dev)

Requests are scoped to a user by the `X-User` header (the frontend sends the
signed-in GitHub login) and to a workspace by the URL. This build **trusts**
`X-User`; it is a dev bypass and must not be exposed publicly as-is. Production
should verify a signed session token instead.

## Endpoints

| Method | Path                                       | Purpose                          |
|--------|--------------------------------------------|----------------------------------|
| GET    | `/health`                                  | Liveness                         |
| GET    | `/api/v1/workspaces`                       | Dashboard workspace groupings    |
| GET    | `/api/v1/workspaces/{wid}/services`        | List a workspace's services      |
| PUT    | `/api/v1/workspaces/{wid}/services`        | Replace all (snapshot sync)      |
| POST   | `/api/v1/workspaces/{wid}/services`        | Create one service               |
| GET    | `/api/v1/services/{id}`                    | Get one service                  |
| PATCH  | `/api/v1/services/{id}`                    | Merge fields into a service      |
| DELETE | `/api/v1/services/{id}`                    | Delete a service                 |
| POST   | `/api/v1/services/{id}/deploy`             | Real deploy: clone → build → isolated container → URL |
| GET    | `/api/v1/services/{id}/logs`               | Live runtime logs of the container |
| POST   | `/api/v1/services/{id}/expose`             | Proxy a public URL to a running app |
| POST   | `/api/v1/services/{id}/unexpose`           | Stop the proxy                   |
| POST   | `/api/v1/github/token`                     | Store the caller's GitHub token (for private-repo clones) |

### Exposing a running app (reverse proxy)

`expose` points a public port at a locally-running app (e.g. a Next.js dev
server) so it's reachable at a real URL. Each exposed service gets its own port
(9000–9100) — a dedicated port, not a path prefix, so apps with absolute asset
paths like Next.js's `/_next/*` work with no rewriting.

```bash
# target is whatever address the app is actually listening on
curl -X POST localhost:8080/api/v1/services/{id}/expose \
  -H 'X-User: me' -H 'Content-Type: application/json' \
  -d '{"target":"http://localhost:3000"}'
# → { "url": "http://localhost:9000", ... }  ← open this
```

The public URL is bound to all interfaces, so it also works over the LAN
(`http://<server-ip>:9000`). Set `PUBLIC_HOST` to control the hostname returned
in `url`. Exposed services are re-proxied automatically on restart.

> Needs host network access and free ports, so run the platform **on the host**
> (not in the container) for this — the container publishes only `:8080` and
> can't reach the host's `localhost`.

CORS is open (dev) so the browser can call `:8080` directly.

## Postman

Import `servd-platform.postman_collection.json`. Set the collection variables
`baseUrl`, `user`, and `workspaceId`; run **Create service** first — it captures
the new `serviceId` for the get/patch/deploy/delete requests.

## Frontend wiring

The frontend reads `NEXT_PUBLIC_PLATFORM_URL` (default `http://localhost:8080`).
The workspace canvas loads services on mount (`GET`) and persists changes back
(debounced `PUT`), scoped by the user's GitHub login. See `frontend/src/lib/platform.ts`.
