// Package deploy runs the deploy pipeline: clone a repo (or pull an image),
// build it, run it as a hardened container and route a public URL to it.
package deploy

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"servd/platform/internal/builder"
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
	rt        Runtime
	proxy     *proxy.Manager
	encKey    []byte      // decrypts stored GitHub tokens (32 bytes), may be empty
	githubApp *github.App // optional; mints installation tokens for private repos
}

// deploy resource defaults (per app).
const (
	appMemoryMB  = 512
	appCPUs      = 0.5
	appPidsLimit = 256
	buildTimeout = 15 * time.Minute
)

// New wires the deployer onto a runtime (native engine or Docker).
func New(st store.Storer, rt Runtime, px *proxy.Manager, encKey []byte, githubApp *github.App) *Deployer {
	return &Deployer{
		store:     st,
		rt:        rt,
		proxy:     px,
		encKey:    encKey,
		githubApp: githubApp,
	}
}

// GitHubApp returns the configured GitHub App, or nil.
func (d *Deployer) GitHubApp() *github.App { return d.githubApp }

// Runtime returns the runtime builds and apps use.
func (d *Deployer) Runtime() Runtime { return d.rt }

// Prewarm pulls common base images in the background, so the first deploy of
// each stack skips the cold download.
func (d *Deployer) Prewarm() { d.rt.Prewarm() }

// Logs returns the recent output of a service's running app.
func (d *Deployer) Logs(name string, tail int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return d.rt.Logs(ctx, name, tail)
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
	// Each deploy runs under a fresh name next to the previous version, which
	// keeps serving until the new one is healthy (zero-downtime, and a failed
	// deploy never takes the site down).
	key := Key(user, id)
	prev, prevImage := ownedApp(svc, key)
	name := appName(key) + "-" + strconv.FormatInt(time.Now().Unix()%1e7, 36)
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
		port, err := d.rt.Pull(ctx, image)
		if err != nil {
			d.log(user, id, errline(err.Error()))
			d.fail(user, id, "could not pull image "+image)
			return
		}
		tag, cport = image, port
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

		tag = fmt.Sprintf("servd/%s:%d", key, time.Now().Unix())
		buildEnv := builder.FilterEnv(stringMap(svc["envVars"]))
		plan, err := builder.Detect(dir, builder.Keys(buildEnv))
		if err != nil {
			d.fail(user, id, err.Error())
			return
		}
		framework, cport = plan.Stack, plan.Port
		d.log(user, id, success(fmt.Sprintf("Detected %s — will serve on port %d", plan.Label(), plan.Port)))
		d.log(user, id, info(fmt.Sprintf("Building image (%s builder)…", d.rt.Name())))
		buildStart := time.Now()
		if err := d.buildWithLogs(ctx, user, id, BuildRequest{Dir: dir, Tag: tag, Plan: plan, Env: buildEnv, CacheScope: key}); err != nil {
			d.log(user, id, errline(err.Error()))
			d.fail(user, id, "build failed — see the build output above")
			return
		}
		d.log(user, id, success(fmt.Sprintf("Image built in %s", time.Since(buildStart).Round(time.Second))))
	}

	// Start the new version (replacing any previous one), hardened.
	d.setStatus(user, id, "deploying", info("Starting isolated container"))
	addr, err := d.rt.Run(ctx, RunRequest{
		Name: name, Image: tag, Port: cport, Env: stringMap(svc["envVars"]),
		MemoryMB: appMemoryMB, CPUs: appCPUs, Pids: appPidsLimit,
	})
	if err != nil {
		d.fail(user, id, "run failed: "+err.Error()+keptNote(prev))
		return
	}

	// 5. Health check: wait for the app to accept connections — unless the
	// service opts out (for apps that don't serve on a port, or boot slowly).
	if boolOf(svc["skipHealthCheck"]) {
		d.log(user, id, info("Health check skipped (per service setting)"))
		time.Sleep(2 * time.Second) // brief grace for the container to come up
	} else if !waitListening(addr, 60*time.Second) {
		logs, _ := d.rt.Logs(ctx, name, 12)
		d.log(user, id, errline(logs))
		d.rt.Stop(ctx, name)
		d.fail(user, id, "health check failed: app did not start listening on port "+strconv.Itoa(cport)+keptNote(prev))
		return
	} else {
		d.log(user, id, success("Health check passed"))
	}

	// 6. Route public traffic to it (zero-downtime switch).
	exp, err := d.proxy.Expose(ServiceRoute(user, svc, "http://"+addr))
	if err != nil {
		d.rt.Stop(ctx, name)
		d.fail(user, id, "expose failed: "+err.Error()+keptNote(prev))
		return
	}
	for dom, why := range exp.Rejected {
		d.log(user, id, errline(fmt.Sprintf("Custom domain %s not attached: %s", dom, why)))
	}

	// 7. Persist the running state. localUrl is the app's own address (reachable
	// from the platform host) for direct testing; publicUrl is the public one.
	localURL := "http://" + addr
	_, _ = d.store.PatchService(user, id, map[string]any{
		"status":      "running",
		"framework":   framework,
		"containerId": name,
		"runtime":     d.rt.Name(),
		"image":       builtImage(tag, framework),
		"localUrl":    localURL,
		"publicUrl":   exp.URL,
		"hostname":    exp.Hostname,
		"proxyTarget": localURL,
		"exposed":     true,
	})
	d.log(user, id, success("Live at "+exp.URL))
	for _, dom := range exp.Domains {
		d.log(user, id, success("Also at "+schemeOf(exp.URL)+dom))
	}

	// Retire the previous version (and its image) now that traffic has moved.
	if prev != "" && prev != name {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			d.rt.Stop(ctx, prev)
			if prevImage != "" && prevImage != tag {
				d.rt.RemoveImage(ctx, prevImage)
			}
		}()
	}
}

// builtImage is the image name to clean up on the next deploy: only images we
// built, never a prebuilt image the user asked for (e.g. "redis:7").
func builtImage(tag, framework string) string {
	if framework == "image" {
		return ""
	}
	return tag
}

// schemeOf returns "https://" or "http://" to match url.
func schemeOf(url string) string {
	if strings.HasPrefix(url, "https://") {
		return "https://"
	}
	return "http://"
}

// keptNote tells the user their previous version is still up.
func keptNote(prev string) string {
	if prev == "" {
		return ""
	}
	return " (the previous version is still serving)"
}

// ownedApp returns the service's recorded app and image names, but only if
// they belong to key: stored fields are never trusted to name another
// tenant's app or image.
func ownedApp(svc store.Service, key string) (app, image string) {
	if a, _ := svc["containerId"].(string); strings.HasPrefix(a, appName(key)+"-") {
		app = a
	}
	if i, _ := svc["image"].(string); strings.HasPrefix(i, "servd/"+key+":") {
		image = i
	}
	return app, image
}

// appName is the container/sandbox name of a service's app.
func appName(serviceID string) string { return "servd-app-" + serviceID }

// Stop tears down a service's running container and proxy.
func (d *Deployer) Stop(user, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if svc, _, err := d.store.GetService(user, id); err == nil {
		if name, _ := ownedApp(svc, Key(user, id)); name != "" {
			d.rt.Stop(ctx, name)
		}
	}
	d.proxy.Unexpose(Key(user, id))
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
