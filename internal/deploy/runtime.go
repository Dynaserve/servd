package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"servd/platform/internal/builder"
	"servd/platform/internal/docker"
	"servd/platform/internal/engine"
	"servd/platform/internal/sandbox"
)

// Runtime is where images are built and apps run: the native engine (the
// default) or a Docker daemon.
type Runtime interface {
	Name() string
	// Pull fetches a prebuilt image and returns the port it likely serves on.
	Pull(ctx context.Context, image string) (port int, err error)
	// Build builds dir into an image named tag, following plan.
	Build(ctx context.Context, b BuildRequest) error
	// Run (re)starts an app and returns the address the proxy should target.
	Run(ctx context.Context, r RunRequest) (addr string, err error)
	// Stop removes an app.
	Stop(ctx context.Context, name string)
	// Logs returns the last lines of an app's output.
	Logs(ctx context.Context, name string, tail int) (string, error)
	// RemoveImage deletes a built image no longer in use.
	RemoveImage(ctx context.Context, name string)
	// Prewarm fetches common base images in the background.
	Prewarm()
}

// BuildRequest describes one image build.
type BuildRequest struct {
	Dir        string
	Tag        string
	Plan       *builder.Plan
	Env        map[string]string // build-time variables (BuildKit secrets)
	CacheScope string            // isolates build caches per service
	OnLine     func(string)
}

// RunRequest describes one app to run.
type RunRequest struct {
	Name     string
	Image    string
	Port     int
	Env      map[string]string
	MemoryMB int
	CPUs     float64
	Pids     int
}

// --- native engine ---

type nativeRuntime struct{ eng *engine.Engine }

// NewNativeRuntime wraps the daemonless engine.
func NewNativeRuntime(eng *engine.Engine) Runtime { return &nativeRuntime{eng: eng} }

func (n *nativeRuntime) Name() string { return "native" }

func (n *nativeRuntime) Pull(ctx context.Context, image string) (int, error) {
	img, err := n.eng.Pull(ctx, image)
	if err != nil {
		return 0, err
	}
	for p := range img.Config.ExposedPorts {
		var port int
		if _, err := fmt.Sscanf(p, "%d", &port); err == nil && port > 0 {
			return port, nil
		}
	}
	return docker.KnownImagePort(image), nil
}

func (n *nativeRuntime) Build(ctx context.Context, b BuildRequest) error {
	df := b.Plan.Dockerfile
	if df == "" { // the repo's own Dockerfile
		raw, err := os.ReadFile(filepath.Join(b.Dir, "Dockerfile"))
		if err != nil {
			return err
		}
		df = string(raw)
	}
	_, err := n.eng.Build(ctx, engine.BuildOptions{
		ContextDir: b.Dir,
		Dockerfile: df,
		Tag:        b.Tag,
		Secrets:    b.Env,
		CacheScope: b.CacheScope,
		Log:        b.OnLine,
	})
	return err
}

func (n *nativeRuntime) Run(ctx context.Context, r RunRequest) (string, error) {
	env := map[string]string{"PORT": fmt.Sprint(r.Port)}
	for k, v := range r.Env {
		env[k] = v
	}
	ip, err := n.eng.Start(ctx, engine.RunOptions{
		ID:     r.Name,
		Image:  r.Image,
		Env:    env,
		Limits: sandbox.Limits{MemoryMB: r.MemoryMB, CPUs: r.CPUs, Pids: r.Pids},
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d", ip, r.Port), nil
}

func (n *nativeRuntime) Stop(_ context.Context, name string) { _ = n.eng.Stop(name) }

func (n *nativeRuntime) Logs(_ context.Context, name string, tail int) (string, error) {
	return n.eng.Logs(name, tail)
}

func (n *nativeRuntime) RemoveImage(_ context.Context, name string) { _ = n.eng.RemoveImage(name) }

func (n *nativeRuntime) Prewarm() {
	go func() {
		for _, img := range builder.BaseImages() {
			if strings.HasPrefix(img, "docker/dockerfile") {
				continue // BuildKit frontend; the native builder doesn't need it
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			_, _ = n.eng.Pull(ctx, img)
			cancel()
		}
	}()
}

// --- Docker daemon ---

type dockerRuntime struct {
	dc    *docker.Client
	ports *portAllocator

	mu       sync.Mutex
	assigned map[string]int // app name -> host port
}

// NewDockerRuntime wraps a Docker daemon. Apps are published on loopback
// host ports 7000–7999, which the proxy targets.
func NewDockerRuntime(dc *docker.Client) Runtime {
	return &dockerRuntime{dc: dc, ports: newPortAllocator(7000, 7999), assigned: map[string]int{}}
}

func (d *dockerRuntime) Name() string { return "docker" }

func (d *dockerRuntime) Pull(ctx context.Context, image string) (int, error) {
	if out, err := d.dc.Pull(ctx, image); err != nil {
		return 0, fmt.Errorf("%v: %s", err, lastLines(out, 10))
	}
	return d.dc.ImagePort(ctx, image), nil
}

func (d *dockerRuntime) Build(ctx context.Context, b BuildRequest) error {
	if err := b.Plan.Write(b.Dir); err != nil {
		return err
	}
	return d.dc.BuildStream(ctx, b.Dir, b.Tag, b.Env, func(raw string) {
		if line, ok := cleanBuildLine(raw); ok {
			b.OnLine(line)
		}
	})
}

func (d *dockerRuntime) Run(ctx context.Context, r RunRequest) (string, error) {
	d.Stop(ctx, r.Name)
	hostPort, err := d.ports.allocate()
	if err != nil {
		return "", err
	}
	_, err = d.dc.RunSecure(ctx, docker.RunSpec{
		Name: r.Name, Image: r.Image, HostPort: hostPort, ContainerPort: r.Port,
		Env: r.Env, MemoryMB: r.MemoryMB, CPUs: fmt.Sprint(r.CPUs), PidsLimit: r.Pids,
	})
	if err != nil {
		d.ports.release(hostPort)
		return "", err
	}
	d.mu.Lock()
	d.assigned[r.Name] = hostPort
	d.mu.Unlock()
	return fmt.Sprintf("127.0.0.1:%d", hostPort), nil
}

func (d *dockerRuntime) Stop(ctx context.Context, name string) {
	_ = d.dc.Stop(ctx, name)
	d.mu.Lock()
	if p, ok := d.assigned[name]; ok {
		d.ports.release(p)
		delete(d.assigned, name)
	}
	d.mu.Unlock()
}

func (d *dockerRuntime) Logs(ctx context.Context, name string, tail int) (string, error) {
	return d.dc.Logs(ctx, name, tail)
}

func (d *dockerRuntime) RemoveImage(ctx context.Context, name string) {
	_ = d.dc.RemoveImage(ctx, name)
}

func (d *dockerRuntime) Prewarm() {
	go func() {
		for _, img := range builder.BaseImages() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			_, _ = d.dc.Pull(ctx, img)
			cancel()
		}
	}()
}
