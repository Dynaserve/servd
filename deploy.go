package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Deployer runs the real deploy pipeline: clone a repo, containerize it, run it
// as a hardened isolated container, health-check it, and route a public URL to
// it via the platform proxy. Status and logs are persisted on the service.
type Deployer struct {
	store     Storer
	docker    *dockerctl
	proxy     *ProxyManager
	ports     *portAllocator
	encKey    []byte     // decrypts stored GitHub tokens (32 bytes), may be empty
	githubApp *GitHubApp // optional; mints installation tokens for private repos
	railpack  *railpack  // optional; preferred builder (better detection + caching)
}

// portAllocator hands out unique host ports for app containers. It tracks
// assignments in memory and skips any port a process is already listening on
// (a running container), which a plain bind check can miss because Go's
// net.Listen sets SO_REUSEADDR.
type portAllocator struct {
	mu    sync.Mutex
	start int
	end   int
	next  int
	used  map[int]bool
}

func newPortAllocator(start, end int) *portAllocator {
	return &portAllocator{start: start, end: end, next: start, used: map[int]bool{}}
}

func (a *portAllocator) allocate() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	span := a.end - a.start + 1
	for i := 0; i < span; i++ {
		p := a.next
		a.next++
		if a.next > a.end {
			a.next = a.start
		}
		if a.used[p] {
			continue
		}
		if portListening(p) {
			a.used[p] = true // a container already holds it; remember to skip
			continue
		}
		a.used[p] = true
		return p, nil
	}
	return 0, fmt.Errorf("no free host ports in %d-%d", a.start, a.end)
}

func (a *portAllocator) release(p int) {
	if p == 0 {
		return
	}
	a.mu.Lock()
	delete(a.used, p)
	a.mu.Unlock()
}

// portListening reports whether something is accepting connections on the port.
func portListening(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// deploy resource defaults (per container).
const (
	appMemoryMB  = 512
	appCPUs      = "0.5"
	appPidsLimit = 256
	buildTimeout = 15 * time.Minute
)

// NewDeployer wires the deployer and ensures the isolated app network exists.
func NewDeployer(store Storer, docker *dockerctl, proxy *ProxyManager, encKey []byte, githubApp *GitHubApp) *Deployer {
	return &Deployer{
		store:     store,
		docker:    docker,
		proxy:     proxy,
		ports:     newPortAllocator(7000, 7999),
		encKey:    encKey,
		githubApp: githubApp,
		railpack:  newRailpack(docker),
	}
}

// baseImages are pre-pulled on startup so the first deploy of each stack skips
// the cold base-image download (often the slowest part of a fresh build).
var baseImages = []string{
	"node:20-alpine",
	"golang:1.23-alpine",
	"python:3.12-slim",
	"nginx:alpine",
	"gcr.io/distroless/static-debian12:nonroot",
	"docker/dockerfile:1",
}

// Prewarm pulls the common base images in the background so builds start warm.
func (d *Deployer) Prewarm() {
	go func() {
		for _, img := range baseImages {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			_, _ = d.docker.pull(ctx, img)
			cancel()
		}
	}()
}

// Logs returns the recent logs of a running container.
func (d *Deployer) Logs(containerID string, tail int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return d.docker.logs(ctx, containerID, tail)
}

// cloneToken returns a token for cloning source. Preference order:
//  1. a GitHub App installation token resolved from the repo's owner
//     (least-privilege, read-only, works for any account the App is on),
//  2. the user's stored installation (set via the install callback),
//  3. the user's stored OAuth token (public repos, or scopes permitting).
//
// Empty string means an unauthenticated clone (fine for public repos).
func (d *Deployer) cloneToken(user, source string) string {
	if d.githubApp != nil {
		if owner, repo := parseOwnerRepo(source); owner != "" && repo != "" {
			if tok, err := d.githubApp.RepoInstallationToken(owner, repo); err == nil && tok != "" {
				return tok
			}
		}
		if instID, err := d.store.GetInstallation(user); err == nil && instID != 0 {
			if tok, err := d.githubApp.InstallationToken(instID); err == nil && tok != "" {
				return tok
			}
		}
	}
	if len(d.encKey) != 32 {
		return ""
	}
	enc, err := d.store.GetToken(user)
	if err != nil || enc == "" {
		return ""
	}
	tok, err := decrypt(d.encKey, enc)
	if err != nil {
		return ""
	}
	return tok
}

// imageSource decides whether a service source is a prebuilt container image
// (e.g. "mongo:7", "redis", "ghcr.io/org/app:tag") rather than a git repo.
// A bare "owner/repo" and any http(s) URL are treated as repos.
func imageSource(source string) (string, bool) {
	s := strings.TrimSpace(source)
	if s == "" || strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return "", false
	}
	// A tag or digest marks an image ("name:tag", "name@sha256:…").
	if strings.Contains(s, ":") || strings.Contains(s, "@") {
		return s, true
	}
	// "owner/repo" (exactly one slash, no tag) is a GitHub repo.
	if strings.Count(s, "/") == 1 {
		return "", false
	}
	// A single word ("redis") or a registry path ("ghcr.io/org/app") is an image.
	return s, true
}

// knownImagePort maps common images to the port they listen on, used when the
// image declares no EXPOSE.
func knownImagePort(image string) int {
	base := image
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.IndexAny(base, ":@"); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "mongo", "mongodb":
		return 27017
	case "postgres", "postgresql":
		return 5432
	case "mysql", "mariadb":
		return 3306
	case "redis":
		return 6379
	case "rabbitmq":
		return 5672
	case "nginx", "httpd", "caddy":
		return 80
	default:
		return 3000
	}
}

// parseOwnerRepo extracts owner and repo from a service source such as
// "owner/repo" or "https://github.com/owner/repo(.git)".
func parseOwnerRepo(source string) (owner, repo string) {
	s := strings.TrimSpace(source)
	s = strings.TrimPrefix(s, "https://github.com/")
	s = strings.TrimPrefix(s, "http://github.com/")
	s = strings.TrimSuffix(s, ".git")
	parts := strings.Split(s, "/")
	if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1]
	}
	return "", ""
}

// Deploy runs the full pipeline for a service. It is meant to be called in a
// goroutine; progress is written to the service's status/logs as it goes.
func (d *Deployer) Deploy(user string, svc Service) {
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()

	id := idOf(svc)
	name := "servd-app-" + id
	source, _ := svc["source"].(string)
	branch, _ := svc["branch"].(string)

	d.setStatus(user, id, "building", info("Deployment started"))

	// The image (tag) to run and the port it listens on come from one of two
	// paths: a prebuilt container image (e.g. "mongo:7") is pulled directly; a
	// git repo is cloned and built.
	var tag, framework string
	var cport int

	if image, ok := imageSource(source); ok {
		framework = "image"
		d.log(user, id, info("Pulling image "+image))
		if out, err := d.docker.pull(ctx, image); err != nil {
			d.log(user, id, errline(lastLines(out, 10)))
			d.fail(user, id, "could not pull image "+image)
			return
		}
		tag = image
		cport = d.docker.imagePort(ctx, image)
		d.log(user, id, success(fmt.Sprintf("Image ready (port %d)", cport)))
	} else {
		repo, err := repoURL(source)
		if err != nil {
			d.fail(user, id, err.Error())
			return
		}
		dir, err := os.MkdirTemp("", "servd-deploy-*")
		if err != nil {
			d.fail(user, id, "workspace error: "+err.Error())
			return
		}
		defer os.RemoveAll(dir)

		if branch != "" {
			d.log(user, id, info(fmt.Sprintf("Cloning %s (branch %s)", repo, branch)))
		} else {
			d.log(user, id, info("Cloning "+repo))
		}
		if err := gitClone(ctx, repo, branch, d.cloneToken(user, source), dir); err != nil {
			d.fail(user, id, "clone failed: "+err.Error())
			return
		}
		d.log(user, id, success("Cloned"))

		tag = fmt.Sprintf("servd/%s:%d", id, time.Now().Unix())
		env := stringMap(svc["envVars"])
		buildStart := time.Now()

		// Prefer Railpack (auto-detects the stack + version, caches per service);
		// fall back to the Dockerfile buildpack when it's unavailable or the repo
		// ships its own Dockerfile.
		if d.railpack != nil && !fileExists(dir, "Dockerfile") {
			framework, cport = "railpack", 3000
			d.log(user, id, info("Building with Railpack (auto-detect)…"))
			if err := d.railpackBuildWithLogs(ctx, user, id, dir, tag, env); err != nil {
				d.fail(user, id, "build failed — see the build output above")
				return
			}
		} else {
			fw, port, err := detectAndPrepare(dir)
			if err != nil {
				d.fail(user, id, err.Error())
				return
			}
			framework, cport = fw, port
			d.log(user, id, success(fmt.Sprintf("Detected %s — will serve on port %d", frameworkLabel(fw), port)))
			d.log(user, id, info("Building image (BuildKit)…"))
			if err := d.dockerBuildWithLogs(ctx, user, id, dir, tag); err != nil {
				d.fail(user, id, "build failed — see the build output above")
				return
			}
		}
		d.log(user, id, success(fmt.Sprintf("Image built in %s", time.Since(buildStart).Round(time.Second))))
	}

	// Replace any previous container, then run the new one (hardened).
	_ = d.docker.stop(ctx, name)
	d.ports.release(intOf(svc["hostPort"])) // free this service's previous port on redeploy
	hostPort, err := d.ports.allocate()
	if err != nil {
		d.fail(user, id, err.Error())
		return
	}
	d.setStatus(user, id, "deploying", info("Starting isolated container"))

	env := stringMap(svc["envVars"])
	cid, err := d.docker.runSecure(ctx, runSpec{
		Name: name, Image: tag, HostPort: hostPort, ContainerPort: cport,
		Env: env, MemoryMB: appMemoryMB, CPUs: appCPUs, PidsLimit: appPidsLimit,
	})
	if err != nil {
		d.ports.release(hostPort)
		d.fail(user, id, "run failed: "+err.Error())
		return
	}

	// 5. Health check: wait for the app to accept connections — unless the
	// service opts out (for apps that don't serve on a port, or boot slowly).
	if boolOf(svc["skipHealthCheck"]) {
		d.log(user, id, info("Health check skipped (per service setting)"))
		time.Sleep(2 * time.Second) // brief grace for the container to come up
	} else if !waitListening(hostPort, 60*time.Second) {
		logs, _ := d.docker.logs(ctx, cid, 12)
		d.log(user, id, errline(logs))
		_ = d.docker.stop(ctx, name)
		d.ports.release(hostPort)
		d.fail(user, id, "health check failed: app did not start listening on port "+strconv.Itoa(cport))
		return
	} else {
		d.log(user, id, success("Health check passed"))
	}

	// 6. Route a public URL to it via the proxy.
	url, err := d.proxy.Expose(id, fmt.Sprintf("http://127.0.0.1:%d", hostPort))
	if err != nil {
		d.fail(user, id, "expose failed: "+err.Error())
		return
	}

	// 7. Persist the running state. localUrl is the container's own host port
	// (127.0.0.1) for direct local testing; publicUrl is the proxied address.
	localURL := fmt.Sprintf("http://localhost:%d", hostPort)
	_, _ = d.store.PatchService(user, id, map[string]any{
		"status":      "running",
		"framework":   framework,
		"containerId": cid,
		"hostPort":    hostPort,
		"localUrl":    localURL,
		"publicUrl":   url,
		"proxyTarget": fmt.Sprintf("http://127.0.0.1:%d", hostPort),
		"exposed":     true,
	})
	d.log(user, id, success("Live at "+url+" (local "+localURL+")"))
}

// Stop tears down a service's running container and proxy.
func (d *Deployer) Stop(user, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if svc, _, err := d.store.GetService(user, id); err == nil {
		d.ports.release(intOf(svc["hostPort"]))
	}
	_ = d.docker.stop(ctx, "servd-app-"+id)
	d.proxy.Unexpose(id)
	_, _ = d.store.PatchService(user, id, map[string]any{
		"status": "stopped", "exposed": false, "publicUrl": "", "localUrl": "",
	})
}

// --- helpers ---

func (d *Deployer) setStatus(user, id, status string, line map[string]any) {
	d.store.PatchService(user, id, map[string]any{"status": status}) //nolint:errcheck
	d.log(user, id, line)
}

func (d *Deployer) fail(user, id, reason string) {
	d.store.PatchService(user, id, map[string]any{"status": "failed"}) //nolint:errcheck
	d.log(user, id, errline("Error: "+reason))
}

// maxLogLines caps how many log lines a service keeps (verbose build output can
// be large, so this is generous).
const maxLogLines = 800

// log appends a single log line to the service.
func (d *Deployer) log(user, id string, line map[string]any) {
	d.appendLogs(user, id, []map[string]any{line})
}

// appendLogs appends several log lines in one store write (chronological order).
func (d *Deployer) appendLogs(user, id string, lines []map[string]any) {
	if len(lines) == 0 {
		return
	}
	svc, _, err := d.store.GetService(user, id)
	if err != nil {
		return
	}
	var logs []any
	if existing, ok := svc["logs"].([]any); ok {
		logs = existing
	}
	for _, l := range lines {
		logs = append(logs, l)
	}
	if len(logs) > maxLogLines {
		logs = logs[len(logs)-maxLogLines:]
	}
	d.store.PatchService(user, id, map[string]any{"logs": logs}) //nolint:errcheck
}

// streamBuild runs a build (via `run`, which streams raw output lines to the
// callback it's given) while batching those lines into the service log every
// 700ms, so verbose output stays cheap to persist.
func (d *Deployer) streamBuild(user, id string, run func(onLine func(string)) error) error {
	var (
		mu      sync.Mutex
		pending []map[string]any
	)
	flush := func() {
		mu.Lock()
		batch := pending
		pending = nil
		mu.Unlock()
		d.appendLogs(user, id, batch)
	}

	ticker := time.NewTicker(700 * time.Millisecond)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				flush()
			case <-stop:
				return
			}
		}
	}()

	err := run(func(line string) {
		mu.Lock()
		pending = append(pending, logLine(buildLevel(line), line))
		mu.Unlock()
	})

	ticker.Stop()
	close(stop)
	flush() // final flush of anything buffered
	return err
}

// dockerBuildWithLogs builds from a generated Dockerfile, streaming cleaned output.
func (d *Deployer) dockerBuildWithLogs(ctx context.Context, user, id, dir, tag string) error {
	return d.streamBuild(user, id, func(onLine func(string)) error {
		return d.docker.buildStream(ctx, dir, tag, func(raw string) {
			if line, ok := cleanBuildLine(raw); ok {
				onLine(line)
			}
		})
	})
}

// railpackBuildWithLogs builds from source with Railpack, streaming cleaned output.
func (d *Deployer) railpackBuildWithLogs(ctx context.Context, user, id, dir, tag string, env map[string]string) error {
	return d.streamBuild(user, id, func(onLine func(string)) error {
		return d.railpack.build(ctx, dir, tag, "servd-"+id, env, func(raw string) {
			if line, ok := cleanBuildLine(raw); ok {
				onLine(line)
			}
		})
	})
}

var (
	bkStep    = regexp.MustCompile(`^#\d+\s+`)        // "#10 "
	bkElapsed = regexp.MustCompile(`^\d+\.\d+\s+`)    // "6.358 "
	bkMount   = regexp.MustCompile(`^--mount=\S+\s+`) // "--mount=type=cache,... "
)

// cleanBuildLine turns raw BuildKit --progress=plain output into a readable
// log line, dropping internal noise (layer resolution, cache/transfer status,
// progress spam) and keeping real command output. Returns ok=false to skip.
func cleanBuildLine(raw string) (string, bool) {
	line := bkStep.ReplaceAllString(raw, "")    // drop "#10 "
	line = bkElapsed.ReplaceAllString(line, "") // drop elapsed "6.358 "
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", false
	}
	lower := strings.ToLower(trimmed)

	// BuildKit internals and status lines — noise.
	for _, p := range []string{
		"done", "cached", "sha256:", "transferring", "load build", "load metadata",
		"load .dockerignore", "load remote", "resolve ", "extracting", "naming to",
		"exporting", "writing image", "preparing", "building with", "=> ", "auth ",
		"[internal]", "importing cache", "sending tarball", "docker-image://",
		"unpacking to", "view build details", "verifying explicit fetch",
	} {
		if strings.HasPrefix(lower, p) {
			return "", false
		}
	}
	// Package-manager progress spam and box-drawing chrome.
	switch {
	case strings.Contains(trimmed, "Progress: resolved"),
		strings.HasPrefix(trimmed, "+"),
		strings.HasPrefix(trimmed, "│"),
		strings.HasPrefix(trimmed, "╭"),
		strings.HasPrefix(trimmed, "╰"),
		strings.HasPrefix(lower, "downloading"):
		return "", false
	}

	// Step headers: "[stage-0 4/6] RUN <cmd>" → show only real commands as "$ cmd".
	if strings.HasPrefix(trimmed, "[") {
		if i := strings.Index(trimmed, "] "); i >= 0 {
			rest := trimmed[i+2:]
			if cmd := strings.TrimPrefix(rest, "RUN "); cmd != rest {
				cmd = bkMount.ReplaceAllString(cmd, "") // hide the cache-mount flag
				return "$ " + cmd, true
			}
			return "", false // COPY / WORKDIR / FROM are noise
		}
		return "", false
	}
	return trimmed, true
}

// buildLevel classifies a build-output line for colouring in the logs.
func buildLevel(line string) string {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "error") || strings.Contains(l, "failed") || strings.Contains(l, "npm err"):
		return "error"
	case strings.Contains(l, "warn"):
		return "warning"
	default:
		return "info"
	}
}

func logLine(level, msg string) map[string]any {
	return map[string]any{
		"id":      newID(),
		"at":      time.Now().UnixMilli(),
		"level":   level,
		"message": msg,
	}
}

func info(m string) map[string]any    { return logLine("info", m) }
func success(m string) map[string]any { return logLine("success", m) }
func errline(m string) map[string]any { return logLine("error", m) }

// repoURL turns a service source into a cloneable https URL. Supports
// "owner/repo", full github URLs, and any https git URL. Container image
// sources are rejected (the deploy pipeline builds from source).
func repoURL(source string) (string, error) {
	s := strings.TrimSpace(source)
	if s == "" {
		return "", fmt.Errorf("no source set: pick a GitHub repository first")
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s, nil
	}
	// bare owner/repo
	if strings.Count(s, "/") == 1 && !strings.Contains(s, ":") {
		return "https://github.com/" + s + ".git", nil
	}
	return "", fmt.Errorf("unsupported source %q: use owner/repo or an https git URL", source)
}

func gitClone(ctx context.Context, repo, branch, token, dir string) error {
	cloneURL := repo
	// Inject the user's token for private github.com repos over https.
	if token != "" && strings.HasPrefix(repo, "https://") {
		cloneURL = "https://x-access-token:" + token + "@" + strings.TrimPrefix(repo, "https://")
	}

	args := []string{"clone", "--depth", "1"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, cloneURL, dir)
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
	if err != nil {
		safe := string(out)
		if token != "" {
			safe = strings.ReplaceAll(safe, token, "***") // never leak the token
		}
		return fmt.Errorf("%v: %s", err, lastLines(safe, 4))
	}
	return nil
}

// waitListening polls a local TCP port until something accepts, or times out.
func waitListening(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(150 * time.Millisecond) // poll fast so ready apps go live instantly
	}
	return false
}

// boolOf coerces a stored field to bool (handles the JSON bool type).
func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

// intOf coerces a stored numeric field (int or JSON float64) to int.
func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func stringMap(v any) map[string]string {
	out := map[string]string{}
	// service env vars are stored as [{key,value}] by the frontend.
	if arr, ok := v.([]any); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				k, _ := m["key"].(string)
				val, _ := m["value"].(string)
				if k != "" {
					out[k] = val
				}
			}
		}
	}
	return out
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
