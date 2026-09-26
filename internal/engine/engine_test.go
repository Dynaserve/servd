package engine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Integration test: needs root, runc and registry access.
// Run with SERVD_INTEGRATION=1 go test ./internal/engine
func TestBuildAndRun(t *testing.T) {
	if os.Getenv("SERVD_INTEGRATION") != "1" || os.Geteuid() != 0 {
		t.Skip("set SERVD_INTEGRATION=1 and run as root")
	}
	root := "/var/lib/servd-engine-test"
	e, err := New(Config{Root: root, Bridge: "servdtest1", Subnet: "10.91.0.0/24", Mirror: os.Getenv("REGISTRY_MIRROR")})
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.MkdirTemp(root, "ctx-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(src)
	write := func(name, body string) {
		p := filepath.Join(src, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app/hello.txt", "v1\n")
	write("deps.txt", "left-pad\n")
	write("secret-leak.txt", "must be ignored\n")
	write(".dockerignore", "secret-leak.txt\n")
	df := `FROM alpine:3 AS build
WORKDIR /src
COPY deps.txt ./
RUN --mount=type=cache,target=/cache --mount=type=secret,id=TOKEN,env=TOKEN \
    test "$TOKEN" = "s3cr3t" && echo "installed $(cat deps.txt)" > /src/installed && date +%s%N > /cache/stamp
COPY . .
RUN cat app/hello.txt > /src/out.txt && ls /src > /src/listing

FROM alpine:3
COPY --from=build --chown=nobody:nobody /src/out.txt /srv/
COPY --from=build /src/installed /src/listing /srv/
COPY <<'EOF' /srv/show.sh
cat /srv/out.txt /srv/installed /srv/listing; echo "user=$(id -u)"; exec sleep 3600
EOF
CMD ["sh", "/srv/show.sh"]
`
	var lines []string
	build := func() {
		t.Helper()
		lines = nil
		_, err := e.Build(context.Background(), BuildOptions{
			ContextDir: src, Dockerfile: df, Tag: "test/app", CacheScope: "t",
			Secrets: map[string]string{"TOKEN": "s3cr3t"},
			Log:     func(l string) { lines = append(lines, l) },
		})
		if err != nil {
			t.Fatalf("build: %v\n%s", err, strings.Join(lines, "\n"))
		}
	}
	cached := func() int {
		n := 0
		for _, l := range lines {
			if strings.HasSuffix(l, "(cached)") {
				n++
			}
		}
		return n
	}

	build()
	img, err := e.Image("test/app")
	if err != nil {
		t.Fatal(err)
	}
	// The secret value must not be in any layer or the image config.
	for _, id := range img.Layers {
		filepath.WalkDir(e.store.LayerPath(id), func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if b, _ := os.ReadFile(p); strings.Contains(string(b), "s3cr3t") {
					t.Errorf("secret found in layer file %s", p)
				}
			}
			return nil
		})
	}
	if strings.Contains(strings.Join(img.Config.Env, " "), "s3cr3t") {
		t.Error("secret found in image env")
	}

	build() // unchanged: everything cached
	if got := cached(); got != 7 {
		t.Errorf("rebuild: %d cached steps, want 7\n%s", got, strings.Join(lines, "\n"))
	}
	write("app/hello.txt", "v2\n") // code change: install step stays cached
	build()
	for _, l := range lines {
		if strings.Contains(l, "test \"$TOKEN\"") && !strings.HasSuffix(l, "(cached)") {
			t.Errorf("install step re-ran after a code-only change: %s", l)
		}
	}

	if _, err := e.Start(context.Background(), RunOptions{ID: "test-app", Image: "test/app"}); err != nil {
		t.Fatal(err)
	}
	defer e.Stop("test-app")
	var out string
	for i := 0; i < 50 && !strings.Contains(out, "user="); i++ {
		time.Sleep(100 * time.Millisecond)
		out, _ = e.Logs("test-app", 20)
	}
	for _, want := range []string{"v2\n", "installed left-pad\n", "app\n", "user=0"} {
		if !strings.Contains(out, want) {
			t.Errorf("app output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret-leak") {
		t.Errorf(".dockerignore not applied:\n%s", out)
	}
	// --chown=nobody resolved from the image's /etc/passwd (uid 65534).
	fi, err := os.Stat(filepath.Join(e.store.LayerPath(img.Layers[len(img.Layers)-3]), "srv/out.txt"))
	if err == nil {
		if uid := fi.Sys().(*syscall.Stat_t).Uid; uid != 100000+65534 {
			t.Errorf("--chown=nobody: uid %d, want %d", uid, 100000+65534)
		}
	}
}
