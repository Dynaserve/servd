// Package builder is the platform's in-house image builder. It inspects a
// source checkout, detects the stack (and the runtime version the repo asks
// for), and generates a small, cache-friendly Dockerfile that the host's
// Docker BuildKit then builds. Repos that ship their own Dockerfile are built
// as-is.
//
// It deliberately covers only the essentials: Node (incl. Next.js and static
// SPAs), Go, Python and plain static sites. Anything else should bring a
// Dockerfile.
package builder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Default runtime versions, used when a repo doesn't pin one.
const (
	defaultNode   = 22
	defaultGo     = "1.25"
	defaultPython = "3.12"
)

// Runtime images shared by several stacks.
const (
	staticImage = "nginxinc/nginx-unprivileged:alpine" // non-root, listens on 8080
	goRunImage  = "gcr.io/distroless/static-debian12:nonroot"
)

// Plan is the result of inspecting a repo: what it is, which port the built
// image listens on, and the Dockerfile to build it with.
type Plan struct {
	Stack string // "next.js", "node", "spa", "go", "python", "static" or "docker"
	Port  int    // container port the app listens on
	// Details are human-readable detection facts for the deploy log, e.g.
	// "Node 20 from .nvmrc" or "pnpm".
	Details []string
	// Dockerfile is the generated recipe; empty when the repo ships its own.
	Dockerfile string
}

// Detect inspects the checkout at dir and returns a build plan. buildEnv names
// the variables that will be available to install/build steps as BuildKit
// secrets (see FilterEnv); they never end up in image layers or history.
func Detect(dir string, buildEnv []string) (*Plan, error) {
	if exists(dir, "Dockerfile") {
		return &Plan{Stack: "docker", Port: exposedPort(dir)}, nil
	}
	mounts := secretMounts(buildEnv)
	switch {
	case exists(dir, "package.json"):
		return detectNode(dir, mounts)
	case exists(dir, "go.mod"):
		return detectGo(dir, mounts)
	case exists(dir, "requirements.txt"), exists(dir, "pyproject.toml"):
		return detectPython(dir, mounts)
	case exists(dir, "index.html"):
		return &Plan{Stack: "static", Port: 8080, Dockerfile: staticDockerfile}, nil
	}
	return nil, fmt.Errorf("could not detect a supported stack (looked for Dockerfile, package.json, go.mod, requirements.txt, pyproject.toml, index.html)")
}

// Write materializes a generated plan into dir: the Dockerfile, plus a
// .dockerignore when the repo has none. It is a no-op for repos that ship
// their own Dockerfile.
func (p *Plan) Write(dir string) error {
	if p.Dockerfile == "" {
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(p.Dockerfile), 0o644); err != nil {
		return fmt.Errorf("write Dockerfile: %w", err)
	}
	if !exists(dir, ".dockerignore") {
		if err := os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte(dockerignore), 0o644); err != nil {
			return fmt.Errorf("write .dockerignore: %w", err)
		}
	}
	return nil
}

// Label is a friendly description of the plan for the deploy log.
func (p *Plan) Label() string {
	name := map[string]string{
		"next.js": "Next.js app",
		"node":    "Node.js app",
		"spa":     "static web app",
		"go":      "Go app",
		"python":  "Python app",
		"static":  "static site",
		"docker":  "Dockerfile",
	}[p.Stack]
	if name == "" {
		name = p.Stack + " app"
	}
	if len(p.Details) > 0 {
		name += " (" + strings.Join(p.Details, ", ") + ")"
	}
	return name
}

// BaseImages are the images generated Dockerfiles use by default, worth
// pre-pulling so the first build of each stack starts warm.
func BaseImages() []string {
	return []string{
		fmt.Sprintf("node:%d-alpine", defaultNode),
		"golang:" + defaultGo + "-alpine",
		"python:" + defaultPython + "-slim",
		staticImage,
		goRunImage,
		"docker/dockerfile:1",
	}
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// buildDenied are variables that control the build toolchain itself rather
// than the app. A service's runtime env still gets them; builds do not, so a
// PATH or NODE_ENV=production set for runtime can't break install/build (the
// latter would make npm skip the devDependencies most builds need).
var buildDenied = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "SHELL": true, "PWD": true,
	"HOSTNAME": true, "TMPDIR": true, "PORT": true, "NODE_ENV": true,
}

// FilterEnv returns the subset of env usable as build-time variables: valid
// shell identifiers, minus toolchain-controlling names (see buildDenied) and
// LD_*/DOCKER_*/BUILDKIT_* prefixes.
func FilterEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		if !envKeyRe.MatchString(k) || buildDenied[k] ||
			strings.HasPrefix(k, "LD_") || strings.HasPrefix(k, "DOCKER_") || strings.HasPrefix(k, "BUILDKIT_") {
			continue
		}
		out[k] = v
	}
	return out
}

// Keys returns env's keys, sorted.
func Keys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// secretMounts renders RUN flags exposing each build secret as an env var of
// the same name for that step only. Returns "" or a string with a leading space.
func secretMounts(keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		if envKeyRe.MatchString(k) {
			fmt.Fprintf(&b, " --mount=type=secret,id=%s,env=%s", k, k)
		}
	}
	return b.String()
}

// shellCmd renders an exec-form CMD running cmd through sh, so $PORT expands.
func shellCmd(cmd string) string {
	b, _ := json.Marshal([]string{"sh", "-c", "exec " + cmd})
	return "CMD " + string(b)
}

const dockerignore = `.git
node_modules
.next
__pycache__
.venv
`

const staticDockerfile = `# syntax=docker/dockerfile:1
FROM ` + staticImage + `
COPY . /usr/share/nginx/html
EXPOSE 8080
`

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func readFile(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return string(b)
}

// exposedPort reads the first EXPOSE from a repo's own Dockerfile, defaulting
// to 3000 when none is declared.
func exposedPort(dir string) int {
	for _, line := range strings.Split(readFile(dir, "Dockerfile"), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.EqualFold(f[0], "EXPOSE") {
			var p int
			if _, err := fmt.Sscanf(f[1], "%d", &p); err == nil && p > 0 {
				return p
			}
		}
	}
	return 3000
}
