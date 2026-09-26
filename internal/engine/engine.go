// Package engine is the platform's native container engine: daemonless image
// builds (internal/dockerfile executed step by step in sandboxes, with a
// content-addressed layer cache), image pulls (internal/oci), and running and
// supervising apps (internal/sandbox). It replaces the Docker daemon.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"servd/platform/internal/oci"
	"servd/platform/internal/sandbox"
)

// Config configures the engine.
type Config struct {
	Root       string // state root, e.g. /var/lib/servd (must be traversable: 0711 up the tree)
	Bridge     string // bridge interface, e.g. "servd0"
	Subnet     string // sandbox subnet, e.g. "10.88.0.0/16"
	IDShift    int    // host uid container root maps to, e.g. 100000
	Mirror     string // optional Docker Hub pull-through mirror
	RuntimeBin string // "" = runc or crun from PATH
}

// Engine builds and runs images without a Docker daemon.
type Engine struct {
	root  string
	store *oci.Store
	sb    *sandbox.Runc

	mu      sync.Mutex
	backoff map[string]*restartState
	locks   map[string]*sync.Mutex // per app: Start/Stop/supervisor never interleave
}

// lock serializes lifecycle changes to one app.
func (e *Engine) lock(id string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.locks[id]
	if m == nil {
		m = &sync.Mutex{}
		e.locks[id] = m
	}
	return m
}

type restartState struct {
	next  time.Time
	delay time.Duration
}

// New initializes the engine: image store, network and sandbox backend.
func New(cfg Config) (*Engine, error) {
	if cfg.IDShift == 0 {
		cfg.IDShift = 100000
	}
	if err := os.MkdirAll(cfg.Root, 0o711); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.Root, 0o711); err != nil {
		return nil, err
	}
	store, err := oci.NewStore(filepath.Join(cfg.Root, "images"), cfg.IDShift, cfg.Mirror)
	if err != nil {
		return nil, err
	}
	network, err := sandbox.NewNetwork(cfg.Bridge, cfg.Subnet)
	if err != nil {
		return nil, err
	}
	sb, err := sandbox.NewRunc(filepath.Join(cfg.Root, "sandboxes"), network, cfg.IDShift, cfg.RuntimeBin)
	if err != nil {
		return nil, err
	}
	// Build sandboxes orphaned by a platform crash are never resumed.
	if specs, err := sb.Started(); err == nil {
		for _, s := range specs {
			if s.Build {
				_ = sb.Stop(s.ID)
			}
		}
	}
	return &Engine{root: cfg.Root, store: store, sb: sb, backoff: map[string]*restartState{}, locks: map[string]*sync.Mutex{}}, nil
}

// Pull fetches an image from its registry.
func (e *Engine) Pull(ctx context.Context, ref string) (*oci.Image, error) {
	return e.store.Pull(ctx, ref)
}

// Image returns a stored image.
func (e *Engine) Image(name string) (*oci.Image, error) { return e.store.Image(name) }

// RemoveImage forgets an image; its layers are reclaimed by Prune once unused.
func (e *Engine) RemoveImage(name string) error { return e.store.RemoveImage(name) }

// Export writes an image in OCI layout (for pushing or running elsewhere).
func (e *Engine) Export(name, dir string) error { return e.store.Export(name, dir) }

// RunOptions describes an app to start.
type RunOptions struct {
	ID     string            // sandbox id, e.g. "app-<service id>"
	Image  string            // stored image name
	Env    map[string]string // added to (and overriding) the image's env
	Limits sandbox.Limits
}

// Start runs an app in the background and returns its address on the
// platform bridge. An existing sandbox with the same id is replaced.
func (e *Engine) Start(ctx context.Context, o RunOptions) (string, error) {
	img, err := e.store.Image(o.Image)
	if err != nil {
		return "", fmt.Errorf("image %s: %w", o.Image, err)
	}
	args := append(append([]string(nil), img.Config.Entrypoint...), img.Config.Cmd...)
	if len(args) == 0 {
		return "", fmt.Errorf("image %s has no command", o.Image)
	}
	env := append([]string(nil), img.Config.Env...)
	keys := make([]string, 0, len(o.Env))
	for k := range o.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = setEnv(env, k, o.Env[k])
	}
	l := e.lock(o.ID)
	l.Lock()
	defer l.Unlock()
	_ = e.sb.Stop(o.ID)
	info, err := e.sb.Start(ctx, &sandbox.Spec{
		ID:      o.ID,
		Layers:  e.layerPaths(img.Layers),
		Args:    args,
		Env:     env,
		Cwd:     img.Config.WorkingDir,
		User:    img.Config.User,
		Network: true,
		Limits:  o.Limits,
	})
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	delete(e.backoff, o.ID)
	e.mu.Unlock()
	return info.IP, nil
}

// Stop terminates an app and releases its resources.
func (e *Engine) Stop(id string) error {
	l := e.lock(id)
	l.Lock()
	defer l.Unlock()
	err := e.sb.Stop(id)
	e.sb.Forget(id)
	return err
}

// Alive reports whether an app is running.
func (e *Engine) Alive(id string) bool { return e.sb.Alive(id) }

// Logs returns the last n lines of an app's output.
func (e *Engine) Logs(id string, n int) (string, error) {
	f, err := os.Open(e.sb.LogFile(id))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	// Read at most the last 256 KiB; plenty for n ≤ 1000 typical lines.
	const window = 256 << 10
	if fi, err := f.Stat(); err == nil && fi.Size() > window {
		_, _ = f.Seek(fi.Size()-window, io.SeekStart)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

// maxLogBytes bounds an app's log file; beyond it the log is truncated.
const maxLogBytes = 20 << 20

// Supervise keeps apps running until ctx ends: it restarts crashed apps
// (with exponential backoff, capped at a minute), re-creates apps whose
// sandboxes vanished with a host reboot, bounds log files, and prunes
// unused layers daily.
func (e *Engine) Supervise(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	lastPrune := time.Time{}
	for {
		e.superviseOnce(ctx)
		if time.Since(lastPrune) > 24*time.Hour {
			if n, err := e.Prune(7 * 24 * time.Hour); err == nil && n > 0 {
				log.Printf("engine: pruned %d unused layers", n)
			}
			lastPrune = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (e *Engine) superviseOnce(ctx context.Context) {
	specs, err := e.sb.Started()
	if err != nil {
		return
	}
	for _, s := range specs {
		if s.Build {
			continue // builds clean up after themselves; crash leftovers go at startup
		}
		l := e.lock(s.ID)
		if !l.TryLock() {
			continue // being (re)started or stopped right now
		}
		e.superviseApp(ctx, s)
		l.Unlock()
	}
}

func (e *Engine) superviseApp(ctx context.Context, s *sandbox.Spec) {
	if fi, err := os.Stat(e.sb.LogFile(s.ID)); err == nil && fi.Size() > maxLogBytes {
		_ = os.Truncate(e.sb.LogFile(s.ID), 0)
	}
	if e.sb.Alive(s.ID) {
		e.mu.Lock()
		delete(e.backoff, s.ID) // healthy again: next crash restarts at once
		e.mu.Unlock()
		return
	}
	if _, err := os.Stat(e.sb.LogFile(s.ID)); os.IsNotExist(err) {
		return // stopped and removed between listing and locking
	}
	e.mu.Lock()
	st := e.backoff[s.ID]
	if st == nil {
		st = &restartState{delay: time.Second}
		e.backoff[s.ID] = st
	}
	if time.Now().Before(st.next) {
		e.mu.Unlock()
		return
	}
	st.next = time.Now().Add(st.delay)
	if st.delay < time.Minute {
		st.delay *= 2
	}
	e.mu.Unlock()

	logTail, _ := e.Logs(s.ID, 3)
	log.Printf("engine: %s is not running; restarting (last output: %q)", s.ID, logTail)
	spec := *s
	if _, err := e.sb.Start(ctx, &spec); err != nil {
		log.Printf("engine: restart %s: %v", s.ID, err)
	}
}

// Prune deletes layers no stored image uses and nothing has touched for
// olderThan (recent unreferenced layers are kept as build cache).
func (e *Engine) Prune(olderThan time.Duration) (int, error) {
	used := map[string]bool{}
	imgs, err := filepath.Glob(filepath.Join(e.root, "images", "images", "*.json"))
	if err != nil {
		return 0, err
	}
	for _, p := range imgs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, id := range layerIDsFromJSON(b) {
			used[id] = true
		}
	}
	ents, err := os.ReadDir(filepath.Join(e.root, "images", "layers"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ent := range ents {
		if used[ent.Name()] {
			continue
		}
		p := e.store.LayerPath(ent.Name())
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > olderThan {
			if os.RemoveAll(p) == nil {
				n++
			}
		}
	}
	return n, nil
}

func layerIDsFromJSON(b []byte) []string {
	var img oci.Image
	if err := json.Unmarshal(b, &img); err != nil {
		return nil
	}
	return img.Layers
}

// --- overlay helpers ---

func mountOverlayRO(target string, lower []string) error {
	opts := "lowerdir=" + strings.Join(lower, ":")
	if len(opts) > unix.Getpagesize()-1 {
		return fmt.Errorf("too many layers to mount (%d)", len(lower))
	}
	return unix.Mount("overlay", target, "overlay", unix.MS_RDONLY, opts)
}

func unmount(target string) { _ = unix.Unmount(target, unix.MNT_DETACH) }
