package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Buildpack detection: figure out how to containerize a cloned repo. If the
// repo ships its own Dockerfile we use it; otherwise we generate one for the
// detected stack.

type stack struct {
	name string
	port int
}

// detectAndPrepare inspects the repo at dir and ensures a Dockerfile exists,
// returning the framework name and the container port the app will listen on.
func detectAndPrepare(dir string) (framework string, port int, err error) {
	if fileExists(dir, "Dockerfile") {
		return "docker", detectExposedPort(dir), nil
	}

	switch {
	case hasNextDependency(dir):
		return write(dir, stack{"next.js", 3000}, nextDockerfile(dir))
	case fileExists(dir, "package.json"):
		return write(dir, stack{"node", 3000}, nodeDockerfile(dir))
	case fileExists(dir, "go.mod"):
		return write(dir, stack{"go", 8080}, goDockerfile)
	case fileExists(dir, "requirements.txt"), fileExists(dir, "pyproject.toml"):
		return write(dir, stack{"python", 8000}, pythonDockerfile)
	case fileExists(dir, "index.html"):
		return write(dir, stack{"static", 80}, staticDockerfile)
	default:
		return "", 0, fmt.Errorf("could not detect a supported stack (looked for Dockerfile, package.json, go.mod, requirements.txt, index.html)")
	}
}

func write(dir string, s stack, dockerfile string) (string, int, error) {
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", 0, fmt.Errorf("write Dockerfile: %w", err)
	}
	return s.name, s.port, nil
}

func fileExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// hasNextDependency reports whether package.json depends on Next.js.
func hasNextDependency(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return false
	}
	_, a := pkg.Dependencies["next"]
	_, d := pkg.DevDependencies["next"]
	return a || d
}

// installCmd picks the fastest install command for the lockfile present.
// `npm ci` / frozen lockfiles are much faster and reproducible; --prefer-offline
// reuses the BuildKit npm cache mount.
func installCmd(dir string) string {
	switch {
	case fileExists(dir, "yarn.lock"):
		return "yarn install --frozen-lockfile --prefer-offline || yarn install"
	case fileExists(dir, "pnpm-lock.yaml"):
		return "corepack enable && pnpm install --frozen-lockfile --prefer-offline || npm install"
	case fileExists(dir, "package-lock.json"):
		return "npm ci --prefer-offline --no-audit --no-fund || npm install --prefer-offline"
	default:
		return "npm install --prefer-offline --no-audit --no-fund"
	}
}

func nextDockerfile(dir string) string {
	return fmt.Sprintf(`# syntax=docker/dockerfile:1
FROM node:20-alpine
WORKDIR /app
COPY package*.json yarn.lock* pnpm-lock.yaml* ./
RUN --mount=type=cache,target=/root/.npm %s
COPY . .
RUN npm run build || yarn build || pnpm build
ENV PORT=3000
ENV HOSTNAME=0.0.0.0
EXPOSE 3000
CMD ["npm","run","start"]
`, installCmd(dir))
}

func nodeDockerfile(dir string) string {
	return fmt.Sprintf(`# syntax=docker/dockerfile:1
FROM node:20-alpine
WORKDIR /app
COPY package*.json yarn.lock* pnpm-lock.yaml* ./
RUN --mount=type=cache,target=/root/.npm %s
COPY . .
RUN npm run build --if-present
ENV PORT=3000
EXPOSE 3000
CMD ["npm","start"]
`, installCmd(dir))
}

// frameworkLabel is a friendly name for a detected stack, for the deploy log.
func frameworkLabel(fw string) string {
	switch fw {
	case "next.js":
		return "Next.js app"
	case "node":
		return "Node.js app"
	case "go":
		return "Go app"
	case "python":
		return "Python app"
	case "static":
		return "static site"
	case "docker":
		return "Dockerfile"
	default:
		return fw + " app"
	}
}

const goDockerfile = `FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.* ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 go build -o /app/server ./...

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /app/server /server
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/server"]
`

const pythonDockerfile = `FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt* ./
RUN pip install --no-cache-dir -r requirements.txt || true
COPY . .
ENV PORT=8000
EXPOSE 8000
CMD ["python","app.py"]
`

const staticDockerfile = `FROM nginx:alpine
COPY . /usr/share/nginx/html
EXPOSE 80
`

// detectExposedPort reads the first EXPOSE from a repo's own Dockerfile,
// defaulting to 3000 when none is declared.
func detectExposedPort(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		return 3000
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) >= 2 && strings.EqualFold(f[0], "EXPOSE") {
			var p int
			if _, err := fmt.Sscanf(f[1], "%d", &p); err == nil && p > 0 {
				return p
			}
		}
	}
	return 3000
}
