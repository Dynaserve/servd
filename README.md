# Servd Platform API

The Servd control-plane API — a single Go binary. It persists services to a
JSON file (or PostgreSQL when `DATABASE_URL` is set) and does not require
Docker, so it runs anywhere Go runs. It serves the REST API the frontend canvas
consumes.

## Layout

```
cmd/servd/          entrypoint: config from env, wiring, graceful shutdown
internal/
  api/              HTTP routes, handlers, auth + CORS middleware
  store/            Storer interface; JSON-file and PostgreSQL backends
  deploy/           deploy pipeline: source → build → container → URL
                    (build-log cleanup, port allocation)
  builder/          in-house image builder: detects the stack, writes a Dockerfile
  docker/           thin wrapper over the docker CLI
  proxy/            per-service reverse proxies on dedicated ports
  github/           GitHub App: installation tokens, repo listing
  session/          verifies the frontend's signed session token
  secret/           AES-256-GCM encryption for tokens at rest
  brand/            Server / X-Powered-By / X-Region response headers
  ids/              random id generation
docs/               Postman collection
```

## Run

```bash
go build -o platform ./cmd/servd
./platform
```

Or `./dev.sh`, which loads `.env`, rebuilds, and frees `:8080` first.

Listens on `:8080` (all interfaces), so it is reachable over the LAN at
`http://<server-ip>:8080` — e.g. `http://172.20.10.2:8080`.

Config (environment):

| Var              | Default             | Meaning                                          |
|------------------|---------------------|--------------------------------------------------|
| `LISTEN_ADDR`    | `:8080`             | Bind address                                     |
| `DATABASE_URL`   | *(unset)*           | If set (or `POSTGRES_URL`), persist to PostgreSQL instead of a file |
| `DATA_FILE`      | `./data/store.json` | JSON store path (used only when `DATABASE_URL` unset) |
| `PUBLIC_HOST`    | `localhost`         | Hostname advertised in `expose`/deploy URLs      |
| `SESSION_SECRET` | *(unset)*           | Shared with the frontend; when set, the platform verifies the session cookie (real auth). Unset = dev auth (trusts `X-User`). |
| `ENCRYPTION_KEY` | *(unset)*           | Exactly 32 bytes; enables storing users' GitHub tokens (AES-256-GCM) so the deploy engine can clone private repos. |
| `REGION`         | `AU`                | Advertised in the `X-Region` response header. |
| `APPS_NETWORK`   | `servd-apps`        | Docker bridge network deployed apps run on. |
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

### Builds

Deploys from git are built by the in-house builder (`internal/builder`), no
external build tool needed — just the host's Docker (23+, BuildKit). A repo
that ships a `Dockerfile` is built as-is; otherwise the builder detects the
stack and generates a small Dockerfile with BuildKit cache mounts:

| Stack | Detected by | Version from | Serves |
|-------|-------------|--------------|--------|
| Next.js | `next` dependency | `.nvmrc`, `.node-version`, `engines.node` (default 22) | `start` script / `next start` on 3000 |
| Static SPA | `build` script, no `start`, and `vite` / `astro` / `@vue/cli-service` / `react-scripts` | as above | build output via unprivileged nginx on 8080, SPA fallback |
| Node | `package.json` | as above | `start` script, `main`, or `server.js`/`index.js`/… on 3000 |
| Go | `go.mod` | `go` directive if newer than 1.25 | root or single `cmd/*` main package, distroless, 8080 |
| Python | `requirements.txt` / `pyproject.toml` | `.python-version`, `runtime.txt`, `requires-python` (default 3.12) | Procfile `web:`, Django, uvicorn, gunicorn or `python main.py` on 8000 |
| Static | `index.html` | — | unprivileged nginx on 8080 |

npm, pnpm and yarn are picked from the lockfile. A service's env vars are
passed to install/build steps as BuildKit secrets, so values like
`NEXT_PUBLIC_*` work at build time without ending up in image layers or
history. Toolchain variables (`PATH`, `HOME`, `NODE_ENV`, `PORT`, `LD_*`, …)
are kept out of builds; the running container still gets them.

### Auth

With `SESSION_SECRET` set (same value as the frontend's), every `/api/v1`
request must carry a valid signed session — the `session` cookie (sent by the
browser) or an `Authorization: Bearer <token>`. The `X-User` header can no
longer impersonate. Without `SESSION_SECRET`, it falls back to trusting
`X-User` (local dev only).

### Storage backends

The platform picks its store at startup:

- **`DATABASE_URL` set** → PostgreSQL. Each service is a row in `services`
  (JSONB `data`, indexed on account/workspace); the schema is created on start.
- **unset** → the zero-dependency JSON file store (`DATA_FILE`).

Run Postgres in Docker and the platform on the host (so `expose` still works):

```bash
docker run -d --name servd-pg -e POSTGRES_PASSWORD=dev -p 5432:5432 postgres:16
DATABASE_URL=postgres://postgres:dev@localhost:5432/postgres ./platform
```

## Run in Docker

The platform is a static, pure-Go binary, so the image is tiny (distroless)
and needs no database or Docker-in-Docker. Start Docker Desktop / your VM first,
then from the repo root:

```bash
docker compose up --build
```

Or by hand:

```bash
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
| POST   | `/api/v1/workspaces`                       | Create a workspace               |
| DELETE | `/api/v1/workspaces/{wid}`                 | Delete a workspace and tear down its services |
| POST   | `/api/v1/github/token`                     | Store the caller's GitHub token (for private-repo clones) |
| POST   | `/api/v1/github/installation`              | Store the caller's GitHub App installation id |
| GET    | `/api/v1/github/repos`                     | Repos the GitHub App can access (repo picker) |

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

Import `docs/servd-platform.postman_collection.json`. Set the collection variables
`baseUrl`, `user`, and `workspaceId`; run **Create service** first — it captures
the new `serviceId` for the get/patch/deploy/delete requests.

## Frontend wiring

The frontend reads `NEXT_PUBLIC_PLATFORM_URL` (default `http://localhost:8080`).
The workspace canvas loads services on mount (`GET`) and persists changes back
(debounced `PUT`), scoped by the user's GitHub login. See `frontend/src/lib/platform.ts`.
