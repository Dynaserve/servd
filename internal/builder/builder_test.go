package builder

import (
	"servd/platform/internal/dockerfile"

	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repo writes files (name -> content) into a temp dir and returns it.
func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		env   []string
		stack string
		port  int
		want  []string // substrings of the Dockerfile
		label string   // substring of Label()
	}{
		{
			name:  "own Dockerfile",
			files: map[string]string{"Dockerfile": "FROM x\nEXPOSE 5000\n", "package.json": "{}"},
			stack: "docker", port: 5000,
		},
		{
			name: "next.js with pnpm and .nvmrc",
			files: map[string]string{
				"package.json":   `{"scripts":{"build":"next build","start":"next start"},"dependencies":{"next":"15"}}`,
				"pnpm-lock.yaml": "", ".nvmrc": "v20.11.0\n",
			},
			env:   []string{"NEXT_PUBLIC_API"},
			stack: "next.js", port: 3000,
			want: []string{"FROM node:20-alpine", "pnpm install --frozen-lockfile", "--mount=type=cache,target=/app/.next/cache", "pnpm run build",
				"--mount=type=secret,id=NEXT_PUBLIC_API,env=NEXT_PUBLIC_API", `"exec pnpm start"`, "HOSTNAME=0.0.0.0"},
			label: "Node 20 from .nvmrc, pnpm",
		},
		{
			name: "vite SPA served by nginx",
			files: map[string]string{
				"package.json":      `{"scripts":{"build":"vite build"},"devDependencies":{"vite":"6"}}`,
				"package-lock.json": "",
			},
			stack: "spa", port: 8080,
			want: []string{"COPY package.json package-lock.json ./\nRUN", "npm ci", "COPY . .\nRUN", "npm run build", "COPY --from=build /app/dist", "try_files", "listen 8080"},
		},
		{
			name:  "postinstall hook installs with full source",
			files: map[string]string{"package.json": `{"scripts":{"postinstall":"prisma generate","start":"node s.js"}}`},
			stack: "node", port: 3000,
			want: []string{"COPY . .\nRUN --mount=type=cache"},
		},
		{
			name:  "node server from main, engines range",
			files: map[string]string{"package.json": `{"main":"srv.js","engines":{"node":">=18"}}`, "srv.js": ""},
			stack: "node", port: 3000,
			want: []string{"FROM node:22-alpine", "npm install", `"exec node srv.js"`},
		},
		{
			name: "go with single cmd and newer go.mod",
			files: map[string]string{
				"go.mod":          "module example.com/app\n\ngo 1.99.1\n",
				"cmd/api/main.go": "package main\n",
				"internal/x/x.go": "package x\n",
			},
			stack: "go", port: 8080,
			want: []string{"FROM golang:1.99-alpine", "-o /out/app ./cmd/api", "distroless"},
		},
		{
			name:  "go root main, old go.mod uses default toolchain",
			files: map[string]string{"go.mod": "module m\n\ngo 1.16\n", "main.go": "// x\npackage main\n"},
			stack: "go", port: 8080,
			want: []string{"FROM golang:" + defaultGo + "-alpine", "-o /out/app ."},
		},
		{
			name:  "fastapi with uvicorn",
			files: map[string]string{"requirements.txt": "fastapi\nuvicorn[standard]\n", "main.py": "", ".python-version": "3.11.9\n"},
			stack: "python", port: 8000,
			want: []string{"FROM python:3.11-slim", "COPY requirements.txt ./\nRUN", "pip install -r requirements.txt", "uvicorn main:app --host 0.0.0.0 --port $PORT"},
		},
		{
			name: "django with gunicorn",
			files: map[string]string{
				"requirements.txt": "Django\ngunicorn\n", "manage.py": "", "mysite/wsgi.py": "",
			},
			stack: "python", port: 8000,
			want: []string{"gunicorn mysite.wsgi:application --bind 0.0.0.0:$PORT"},
		},
		{
			name: "procfile wins, pyproject version",
			files: map[string]string{
				"pyproject.toml": "[project]\nrequires-python = \">=3.13\"\n", "Procfile": "web: hypercorn app:app -b 0.0.0.0:$PORT\n",
			},
			stack: "python", port: 8000,
			want: []string{"FROM python:3.13-slim", "pip install .", "exec hypercorn app:app"},
		},
		{
			name:  "static site",
			files: map[string]string{"index.html": "<h1>hi</h1>"},
			stack: "static", port: 8080,
			want: []string{"nginx-unprivileged"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Detect(repo(t, tt.files), tt.env)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if p.Stack != tt.stack || p.Port != tt.port {
				t.Errorf("got stack=%q port=%d, want %q %d", p.Stack, p.Port, tt.stack, tt.port)
			}
			for _, w := range tt.want {
				if !strings.Contains(p.Dockerfile, w) {
					t.Errorf("Dockerfile missing %q:\n%s", w, p.Dockerfile)
				}
			}
			if !strings.Contains(p.Label(), tt.label) {
				t.Errorf("Label() = %q, want it to contain %q", p.Label(), tt.label)
			}
		})
	}
}

func TestDetectErrors(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"nothing":            {"README.md": ""},
		"node without start": {"package.json": `{}`},
		"ambiguous go cmds":  {"go.mod": "module m\n", "cmd/a/main.go": "package main", "cmd/b/main.go": "package main"},
		"python no entry":    {"requirements.txt": "requests"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Detect(repo(t, files), nil); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestWriteKeepsExistingDockerignore(t *testing.T) {
	dir := repo(t, map[string]string{"index.html": "", ".dockerignore": "custom\n"})
	p, err := Detect(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Write(dir); err != nil {
		t.Fatal(err)
	}
	if got := readFile(dir, ".dockerignore"); got != "custom\n" {
		t.Errorf(".dockerignore overwritten: %q", got)
	}
	if readFile(dir, "Dockerfile") != p.Dockerfile {
		t.Error("Dockerfile not written")
	}
}

func TestFilterEnv(t *testing.T) {
	got := FilterEnv(map[string]string{"OK_1": "a", "1BAD": "b", "has space": "c", "_x": "d",
		"PATH": "/x", "NODE_ENV": "production", "LD_PRELOAD": "x", "DOCKER_HOST": "x"})
	if len(got) != 2 || got["OK_1"] != "a" || got["_x"] != "d" {
		t.Errorf("FilterEnv = %v", got)
	}
	if m := secretMounts([]string{"A", "bad key"}); m != " --mount=type=secret,id=A,env=A" {
		t.Errorf("secretMounts = %q", m)
	}
}

func TestNodeMajor(t *testing.T) {
	for spec, want := range map[string]int{
		"20": 20, "v18.17.0": 18, "^20.1": 20, ">=18": defaultNode, ">=24": 24, "18 || 20": 20, "lts/*": 0, "": 0,
	} {
		if got := nodeMajor(spec); got != want {
			t.Errorf("nodeMajor(%q) = %d, want %d", spec, got, want)
		}
	}
}

// Every Dockerfile the detector writes must be executable by the native
// builder, so it must parse.
func TestGeneratedDockerfilesParse(t *testing.T) {
	for _, files := range []map[string]string{
		{"package.json": `{"scripts":{"build":"next build","start":"next start"},"dependencies":{"next":"15"}}`, "pnpm-lock.yaml": ""},
		{"package.json": `{"scripts":{"build":"vite build"},"devDependencies":{"vite":"6"}}`},
		{"package.json": `{"scripts":{"start":"node s.js","postinstall":"x"}}`},
		{"go.mod": "module m\n", "main.go": "package main\n"},
		{"requirements.txt": "fastapi\nuvicorn\n", "main.py": ""},
		{"pyproject.toml": "", "app.py": ""},
		{"index.html": ""},
	} {
		p, err := Detect(repo(t, files), []string{"SECRET_ONE"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dockerfile.Parse(p.Dockerfile, nil); err != nil {
			t.Errorf("%s: %v\n%s", p.Stack, err, p.Dockerfile)
		}
	}
}
