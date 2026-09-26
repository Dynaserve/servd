package builder

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type packageJSON struct {
	Main            string            `json:"main"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Engines         struct {
		Node string `json:"node"`
	} `json:"engines"`
	Workspaces json.RawMessage `json:"workspaces"`
}

// installNeedsSource reports whether installing dependencies needs the whole
// repo (workspaces, or lifecycle scripts like `postinstall: prisma generate`)
// rather than just the manifests.
func (p *packageJSON) installNeedsSource(dir string) bool {
	if len(p.Workspaces) > 0 || exists(dir, "pnpm-workspace.yaml") {
		return true
	}
	for _, s := range []string{"preinstall", "install", "postinstall", "prepare"} {
		if _, ok := p.Scripts[s]; ok {
			return true
		}
	}
	return false
}

// manifestFiles are copied before installing, so the install layer stays
// cached until one of them changes.
func manifestFiles(dir string) []string {
	var out []string
	for _, f := range []string{"package.json", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock",
		"pnpm-lock.yaml", ".npmrc", ".yarnrc", ".yarnrc.yml"} {
		if exists(dir, f) {
			out = append(out, f)
		}
	}
	return out
}

func (p *packageJSON) has(dep string) bool {
	_, a := p.Dependencies[dep]
	_, d := p.DevDependencies[dep]
	return a || d
}

type packageManager struct {
	name    string
	install string // install command, run with a cache mount
	cache   string // cache directory to mount
}

// corepack pins pnpm/yarn to the repo's "packageManager" version; Node 25+
// images no longer bundle it, so install it on demand.
const corepack = "(command -v corepack >/dev/null || npm i -g corepack) && corepack enable && "

// Recent pnpm fails installs whose dependencies have unapproved build scripts
// (strict-dep-builds); a deploy shouldn't, so that is relaxed to a warning
// (pnpm 11+ reads pnpm_config_*, older versions npm_config_*).
func detectPackageManager(dir string) packageManager {
	switch {
	case exists(dir, "pnpm-lock.yaml"):
		return packageManager{"pnpm", corepack + "pnpm_config_strict_dep_builds=false npm_config_strict_dep_builds=false pnpm install --frozen-lockfile", "/root/.local/share/pnpm/store"}
	case exists(dir, "yarn.lock"):
		return packageManager{"yarn", corepack + "yarn install --frozen-lockfile", "/usr/local/share/.cache/yarn"}
	case exists(dir, "package-lock.json"), exists(dir, "npm-shrinkwrap.json"):
		return packageManager{"npm", "npm ci --no-audit --no-fund", "/root/.npm"}
	default:
		return packageManager{"npm", "npm install --no-audit --no-fund", "/root/.npm"}
	}
}

// spaOutputs maps static-site build tools to the directory they emit.
var spaOutputs = []struct{ dep, out string }{
	{"vite", "dist"},
	{"astro", "dist"},
	{"@vue/cli-service", "dist"},
	{"react-scripts", "build"},
}

func detectNode(dir, mounts string) (*Plan, error) {
	var pkg packageJSON
	if err := json.Unmarshal([]byte(readFile(dir, "package.json")), &pkg); err != nil {
		return nil, fmt.Errorf("parse package.json: %w", err)
	}
	pm := detectPackageManager(dir)
	version, from := nodeVersion(dir, pkg.Engines.Node)
	details := []string{fmt.Sprintf("Node %d%s", version, from), pm.name}

	_, hasBuild := pkg.Scripts["build"]
	_, hasStart := pkg.Scripts["start"]

	// Install from the manifests alone when possible, so code-only changes
	// reuse the cached install layer.
	copyFirst, copyAfter := "COPY . .\n", ""
	if !pkg.installNeedsSource(dir) {
		copyFirst = "COPY " + strings.Join(manifestFiles(dir), " ") + " ./\n"
		copyAfter = "COPY . .\n"
	}
	head := fmt.Sprintf(`# syntax=docker/dockerfile:1
FROM node:%d-alpine AS build
WORKDIR /app
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0 NEXT_TELEMETRY_DISABLED=1
%sRUN --mount=type=cache,target=%s%s %s
%s`, version, copyFirst, pm.cache, mounts, pm.install, copyAfter)
	build := ""
	if hasBuild {
		build = fmt.Sprintf("RUN%s %s run build\n", mounts, pm.name)
	}

	// Next.js: build, then serve with `next start` (honours $PORT). Its
	// incremental build cache persists between deploys via a cache mount.
	if pkg.has("next") {
		if hasBuild {
			build = fmt.Sprintf("RUN --mount=type=cache,target=/app/.next/cache%s %s run build\n", mounts, pm.name)
		}
		start := pm.name + " start"
		if !hasStart {
			start = "./node_modules/.bin/next start"
		}
		return &Plan{Stack: "next.js", Port: 3000, Details: details, Dockerfile: head + build +
			"ENV NODE_ENV=production PORT=3000 HOSTNAME=0.0.0.0\nEXPOSE 3000\n" + shellCmd(start) + "\n"}, nil
	}

	// A static SPA (build script, no server): build it, serve the output
	// with nginx, falling back to index.html for client-side routes.
	if hasBuild && !hasStart {
		for _, s := range spaOutputs {
			if pkg.has(s.dep) {
				return &Plan{Stack: "spa", Port: 8080, Details: append(details, s.dep), Dockerfile: head + build + fmt.Sprintf(`
FROM %s
COPY --from=build /app/%s /usr/share/nginx/html
COPY <<'EOF' /etc/nginx/conf.d/default.conf
server {
    listen 8080;
    root /usr/share/nginx/html;
    location / { try_files $uri $uri/ /index.html; }
}
EOF
EXPOSE 8080
`, staticImage, s.out)}, nil
			}
		}
	}

	// Plain Node server.
	start := ""
	switch {
	case hasStart:
		start = pm.name + " start"
	case pkg.Main != "" && exists(dir, pkg.Main):
		start = "node " + pkg.Main
	default:
		for _, f := range []string{"server.js", "index.js", "app.js", "main.js"} {
			if exists(dir, f) {
				start = "node " + f
				break
			}
		}
	}
	if start == "" {
		return nil, fmt.Errorf(`don't know how to start this Node app: add a "start" script to package.json`)
	}
	return &Plan{Stack: "node", Port: 3000, Details: details, Dockerfile: head + build +
		"ENV NODE_ENV=production PORT=3000\nEXPOSE 3000\n" + shellCmd(start) + "\n"}, nil
}

var digitsRe = regexp.MustCompile(`\d+`)

// nodeVersion picks the Node major version from .nvmrc, .node-version or
// package.json engines, returning it and a " from <source>" suffix (empty when
// defaulted).
func nodeVersion(dir, engines string) (int, string) {
	for _, f := range []string{".nvmrc", ".node-version"} {
		if v := nodeMajor(readFile(dir, f)); v > 0 {
			return v, " from " + f
		}
	}
	if v := nodeMajor(engines); v > 0 {
		return v, " from engines"
	}
	return defaultNode, ""
}

// nodeMajor parses a version or range ("20", "v18.17.0", "^20", ">=18",
// "18 || 20") into a concrete major. Open-ended ranges resolve to at least the
// default. Returns 0 when nothing usable is found (e.g. "lts/*").
func nodeMajor(spec string) int {
	best := 0
	for _, alt := range strings.Split(strings.TrimSpace(spec), "||") {
		alt = strings.TrimSpace(alt)
		m := digitsRe.FindString(alt)
		if m == "" {
			continue
		}
		n, _ := strconv.Atoi(m)
		if strings.HasPrefix(alt, ">") && n < defaultNode {
			n = defaultNode
		}
		if n > best {
			best = n
		}
	}
	return best
}
