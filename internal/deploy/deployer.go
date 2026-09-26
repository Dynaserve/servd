// Package deploy runs the deploy pipeline: clone a repo (or pull an image),
// build it, run it as a hardened container and route a public URL to it.
package deploy

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"servd/platform/internal/docker"
	"servd/platform/internal/github"
	"servd/platform/internal/proxy"
	"servd/platform/internal/secret"
	"servd/platform/internal/store"
)

// Deployer runs the real deploy pipeline: clone a repo, containerize it, run it
// as a hardened isolated container, health-check it, and route a public URL to
// it via the platform proxy. Status and logs are persisted on the service.
type Deployer struct {
	store     store.Storer
	docker    *docker.Client
	proxy     *proxy.Manager
	ports     *portAllocator
	encKey    []byte      // decrypts stored GitHub tokens (32 bytes), may be empty
	githubApp *github.App // optional; mints installation tokens for private repos
	railpack  *railpack   // optional; preferred builder (better detection + caching)
}

// deploy resource defaults (per container).
const (
	appMemoryMB  = 512
	appCPUs      = "0.5"
	appPidsLimit = 256
	buildTimeout = 15 * time.Minute
)

// New wires the deployer. The docker client's network must already exist.
func New(st store.Storer, dc *docker.Client, px *proxy.Manager, encKey []byte, githubApp *github.App) *Deployer {
	return &Deployer{
		store:     st,
		docker:    dc,
		proxy:     px,
		ports:     newPortAllocator(7000, 7999),
		encKey:    encKey,
		githubApp: githubApp,
		railpack:  newRailpack(),
	}
}

// GitHubApp returns the configured GitHub App, or nil.
func (d *Deployer) GitHubApp() *github.App { return d.githubApp }

// UsesRailpack reports whether builds go through Railpack rather than the
// Dockerfile buildpack.
func (d *Deployer) UsesRailpack() bool { return d.railpack != nil }

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
			_, _ = d.docker.Pull(ctx, img)
			cancel()
		}
	}()
}

// Logs returns the recent logs of a running container.
func (d *Deployer) Logs(containerID string, tail int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return d.docker.Logs(ctx, containerID, tail)
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
	tok, err := secret.Decrypt(d.encKey, enc)
	if err != nil {
		return ""
	}
	return tok
}

// Deploy runs the full pipeline for a service. It is meant to be called in a
// goroutine; progress is written to the service's status/logs as it goes.
func (d *Deployer) Deploy(user string, svc store.Service) {
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()

	id := store.IDOf(svc)
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
		if out, err := d.docker.Pull(ctx, image); err != nil {
			d.log(user, id, errline(lastLines(out, 10)))
			d.fail(user, id, "could not pull image "+image)
			return
		}
		tag = image
		cport = d.docker.ImagePort(ctx, image)
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
	_ = d.docker.Stop(ctx, name)
	d.ports.release(intOf(svc["hostPort"])) // free this service's previous port on redeploy
	hostPort, err := d.ports.allocate()
	if err != nil {
		d.fail(user, id, err.Error())
		return
	}
	d.setStatus(user, id, "deploying", info("Starting isolated container"))

	env := stringMap(svc["envVars"])
	cid, err := d.docker.RunSecure(ctx, docker.RunSpec{
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
		logs, _ := d.docker.Logs(ctx, cid, 12)
		d.log(user, id, errline(logs))
		_ = d.docker.Stop(ctx, name)
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
	_ = d.docker.Stop(ctx, "servd-app-"+id)
	d.proxy.Unexpose(id)
	_, _ = d.store.PatchService(user, id, map[string]any{
		"status": "stopped", "exposed": false, "publicUrl": "", "localUrl": "",
	})
}

func (d *Deployer) setStatus(user, id, status string, line map[string]any) {
	d.store.PatchService(user, id, map[string]any{"status": status}) //nolint:errcheck
	d.log(user, id, line)
}

func (d *Deployer) fail(user, id, reason string) {
	d.store.PatchService(user, id, map[string]any{"status": "failed"}) //nolint:errcheck
	d.log(user, id, errline("Error: "+reason))
}
