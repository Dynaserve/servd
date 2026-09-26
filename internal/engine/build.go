package engine

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"

	"servd/platform/internal/dockerfile"
	"servd/platform/internal/ids"
	"servd/platform/internal/oci"
	"servd/platform/internal/sandbox"
)

// BuildOptions configures one image build.
type BuildOptions struct {
	ContextDir string // untrusted source checkout
	Dockerfile string // Dockerfile text
	Tag        string // name to store the image under
	// Secrets are exposed to RUN steps that mount them (id → value). Their
	// values change cache keys (so NEXT_PUBLIC_* edits rebuild) but are never
	// written into layers or image config.
	Secrets map[string]string
	// CacheScope isolates RUN cache mounts (npm, pip, go …) per tenant, so one
	// customer's build can never poison another's package cache.
	CacheScope string
	Limits     sandbox.Limits
	Log        func(string)
}

// baseMaxAge is how long a resolved base image is trusted before its tag is
// re-checked with the registry.
const baseMaxAge = time.Hour

// stageState is a stage's image as the build progresses.
type stageState struct {
	layers []string // layer ids, bottom → top
	cfg    v1.ImageConfig
	key    string // content key of everything so far (chains into step keys)
}

// Build executes a Dockerfile without a Docker daemon: each filesystem step
// becomes a layer keyed by the content that produced it, so unchanged steps
// are skipped instantly on the next build.
func (e *Engine) Build(ctx context.Context, o BuildOptions) (*oci.Image, error) {
	log := o.Log
	if log == nil {
		log = func(string) {}
	}
	stages, err := dockerfile.Parse(o.Dockerfile, nil)
	if err != nil {
		return nil, err
	}
	names := map[string]int{}
	for i, st := range stages {
		if st.Name != "" {
			names[st.Name] = i
		}
	}
	needed := neededStages(stages, names)

	// Resolve every base image up front, in parallel.
	bases := map[string]*oci.Image{}
	var basesMu sync.Mutex
	want := map[string]bool{}
	g, gctx := errgroup.WithContext(ctx)
	for i, st := range stages {
		if !needed[i] {
			continue
		}
		refs := []string{st.From}
		for _, in := range st.Instr {
			if in.Op == "COPY" && in.From != "" {
				refs = append(refs, in.From)
			}
		}
		for _, ref := range refs {
			if _, isStage := names[strings.ToLower(ref)]; isStage || ref == "scratch" {
				continue
			}
			if _, n := strconv.Atoi(ref); n == nil {
				continue // COPY --from=<stage index>
			}
			if want[ref] {
				continue
			}
			want[ref] = true
			g.Go(func() error {
				img, err := e.store.Resolve(gctx, ref, baseMaxAge)
				if err != nil {
					return fmt.Errorf("base image %s: %w", ref, err)
				}
				basesMu.Lock()
				bases[ref] = img
				basesMu.Unlock()
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	ignore := ignoreMatcher(dockerfile.ReadIgnore(o.ContextDir))
	ctxRoot, err := os.OpenRoot(o.ContextDir)
	if err != nil {
		return nil, err
	}
	defer ctxRoot.Close()

	states := make([]*stageState, len(stages))
	total := 0
	for i, st := range stages {
		if needed[i] {
			total += len(st.Instr)
		}
	}
	step := 0
	for i, st := range stages {
		if !needed[i] {
			continue
		}
		s := &stageState{}
		if j, ok := names[strings.ToLower(st.From)]; ok && j < i {
			cp := *states[j]
			cp.layers = append([]string(nil), states[j].layers...)
			cp.cfg.Env = append([]string(nil), states[j].cfg.Env...)
			s = &cp
		} else if st.From != "scratch" {
			base := bases[st.From]
			s.layers = append([]string(nil), base.Layers...)
			s.cfg = base.Config
			s.key = key("from", st.From, strings.Join(base.Layers, ","))
		} else {
			s.key = key("scratch")
		}
		for _, in := range st.Instr {
			step++
			prefix := fmt.Sprintf("[%d/%d] ", step, total)
			if err := e.step(ctx, o, s, in, states, names, bases, ctxRoot, ignore, prefix, log); err != nil {
				return nil, fmt.Errorf("%s: %w", in.Raw, err)
			}
		}
		states[i] = s
	}

	final := states[len(stages)-1]
	img := &oci.Image{Name: o.Tag, Config: final.cfg, Layers: final.layers, Created: time.Now()}
	return img, e.store.SaveImage(img)
}

// neededStages marks the final stage and everything it depends on.
func neededStages(stages []dockerfile.Stage, names map[string]int) map[int]bool {
	need := map[int]bool{}
	var visit func(int)
	visit = func(i int) {
		if need[i] {
			return
		}
		need[i] = true
		if j, ok := names[strings.ToLower(stages[i].From)]; ok && j < i {
			visit(j)
		}
		for _, in := range stages[i].Instr {
			if j, ok := names[in.From]; ok && in.From != "" && j < i {
				visit(j)
			} else if n, err := strconv.Atoi(in.From); err == nil && n < i {
				visit(n)
			}
		}
	}
	visit(len(stages) - 1)
	return need
}

func (e *Engine) step(ctx context.Context, o BuildOptions, s *stageState, in dockerfile.Instr,
	states []*stageState, names map[string]int, bases map[string]*oci.Image,
	ctxRoot *os.Root, ignore func(string) bool, prefix string, log func(string)) error {

	switch in.Op {
	case "ENV":
		for _, p := range in.Pairs {
			s.cfg.Env = setEnv(s.cfg.Env, p[0], p[1])
		}
	case "ARG":
		// Build args only influence expansion (done by the parser) and keys.
	case "WORKDIR":
		wd := in.Value
		if !path.IsAbs(wd) {
			wd = path.Join(orRoot(s.cfg.WorkingDir), wd)
		}
		s.cfg.WorkingDir = wd
	case "USER":
		s.cfg.User = in.Value
	case "CMD":
		s.cfg.Cmd = in.Args
	case "ENTRYPOINT":
		s.cfg.Entrypoint = in.Args
		s.cfg.Cmd = nil // as Docker: a new ENTRYPOINT resets CMD
	case "EXPOSE":
		if s.cfg.ExposedPorts == nil {
			s.cfg.ExposedPorts = map[string]struct{}{}
		}
		for _, p := range strings.Fields(in.Value) {
			if !strings.Contains(p, "/") {
				p += "/tcp"
			}
			s.cfg.ExposedPorts[p] = struct{}{}
		}
	case "RUN":
		return e.run(ctx, o, s, in, prefix, log)
	case "COPY":
		return e.copy(o, s, in, states, names, bases, ctxRoot, ignore, prefix, log)
	}
	s.key = key(s.key, in.Raw, strings.Join(s.cfg.Env, "\n"), s.cfg.WorkingDir, s.cfg.User)
	return nil
}

func (e *Engine) run(ctx context.Context, o BuildOptions, s *stageState, in dockerfile.Instr, prefix string, log func(string)) error {
	secretEnv := []string{}
	secretHash := sha256.New()
	for _, sec := range in.Secrets {
		if v, ok := o.Secrets[sec.ID]; ok {
			secretEnv = append(secretEnv, sec.Env+"="+v)
			fmt.Fprintf(secretHash, "%s=%s\x00", sec.ID, v)
		}
	}
	k := key(s.key, "run", strings.Join(in.Args, "\x01"), strings.Join(s.cfg.Env, "\n"),
		s.cfg.WorkingDir, s.cfg.User, strings.Join(in.Caches, ","), hex.EncodeToString(secretHash.Sum(nil)))
	cmdLine := strings.Join(in.Args, " ")
	if in.Shell {
		cmdLine = in.Args[2]
	}
	if e.store.HasLayer(k) {
		e.store.Touch(k)
		log(prefix + "$ " + cmdLine + "  (cached)")
		s.layers, s.key = append(s.layers, k), k
		return nil
	}
	log(prefix + "$ " + cmdLine)

	upper, err := e.store.TempDir("run-")
	if err != nil {
		return err
	}
	var mounts []sandbox.Mount
	for _, target := range in.Caches {
		dir, err := e.cacheDir(o.CacheScope, target)
		if err != nil {
			os.RemoveAll(upper)
			return err
		}
		mounts = append(mounts, sandbox.Mount{Source: dir, Target: target})
	}
	lim := o.Limits
	if lim.MemoryMB == 0 {
		lim = sandbox.Limits{MemoryMB: 4096, CPUs: 2, Pids: 4096}
	}
	start := time.Now()
	out := lineWriter(func(l string) { log(l) })
	err = e.sb.Run(ctx, &sandbox.Spec{
		ID:       "build-" + ids.New(),
		Layers:   e.layerPaths(s.layers),
		UpperDir: upper,
		Args:     in.Args,
		Env:      append(append([]string(nil), s.cfg.Env...), secretEnv...),
		Cwd:      s.cfg.WorkingDir,
		User:     s.cfg.User,
		Mounts:   mounts,
		Network:  true,
		Build:    true,
		Limits:   lim,
	}, out)
	out.Close()
	if err != nil {
		os.RemoveAll(upper)
		return err
	}
	if err := e.store.CommitLayer(upper, k); err != nil {
		return err
	}
	log(fmt.Sprintf("%s  done in %s", prefix, time.Since(start).Round(100*time.Millisecond)))
	s.layers, s.key = append(s.layers, k), k
	return nil
}

func (e *Engine) copy(o BuildOptions, s *stageState, in dockerfile.Instr, states []*stageState,
	names map[string]int, bases map[string]*oci.Image, ctxRoot *os.Root, ignore func(string) bool, prefix string, log func(string)) error {

	dst := in.Dst
	trailing := strings.HasSuffix(dst, "/") || dst == "."
	if !path.IsAbs(dst) {
		dst = path.Join(orRoot(s.cfg.WorkingDir), dst)
	}
	if trailing && !strings.HasSuffix(dst, "/") {
		dst += "/"
	}
	var chmod fs.FileMode
	if in.Chmod != "" {
		n, err := strconv.ParseUint(in.Chmod, 8, 32)
		if err != nil {
			return fmt.Errorf("bad --chmod %q", in.Chmod)
		}
		chmod = fs.FileMode(n)
	}
	own, err := e.chownTo(in.Chown, s.layers)
	if err != nil {
		return err
	}

	// Where the files come from, and a content key for them.
	var src *os.Root
	var srcKey string
	var cleanup func()
	var skip func(string) bool
	switch {
	case in.Heredoc != nil:
		srcKey = key("heredoc", in.Heredoc.Body)
	case in.From != "":
		var layers []string
		if j, ok := names[in.From]; ok {
			layers, srcKey = states[j].layers, states[j].key
		} else if n, err := strconv.Atoi(in.From); err == nil && n < len(states) && states[n] != nil {
			layers, srcKey = states[n].layers, states[n].key
		} else if img := bases[in.From]; img != nil {
			layers, srcKey = img.Layers, key(img.Layers...)
		} else {
			return fmt.Errorf("unknown --from %q", in.From)
		}
		if in.Chown == "" {
			own.keep = true
		}
		k := key(s.key, "copy", in.Raw, srcKey, strconv.Itoa(own.uid), in.Chmod)
		if e.store.HasLayer(k) {
			e.store.Touch(k)
			log(prefix + in.Raw + "  (cached)")
			s.layers, s.key = append(s.layers, k), k
			return nil
		}
		view, done, err := e.mountView(layers)
		if err != nil {
			return err
		}
		cleanup = done
		if src, err = os.OpenRoot(view); err != nil {
			done()
			return err
		}
	default:
		src, skip = ctxRoot, ignore
		h := sha256.New()
		for _, p := range in.Src {
			if err := hashTree(ctxRoot, cleanRel(p), h, skip); err != nil {
				if matches, _ := globRoot(ctxRoot, p); len(matches) > 0 {
					for _, m := range matches {
						if err := hashTree(ctxRoot, m, h, skip); err != nil {
							return err
						}
					}
					continue
				}
				return fmt.Errorf("source %s: %w", p, err)
			}
		}
		srcKey = hex.EncodeToString(h.Sum(nil))
	}
	if cleanup != nil {
		defer cleanup()
		defer src.Close()
	}

	k := key(s.key, "copy", in.Raw, srcKey, strconv.Itoa(own.uid), in.Chmod)
	if e.store.HasLayer(k) {
		e.store.Touch(k)
		log(prefix + in.Raw + "  (cached)")
		s.layers, s.key = append(s.layers, k), k
		return nil
	}
	log(prefix + in.Raw)

	dir, err := e.store.TempDir("copy-")
	if err != nil {
		return err
	}
	if err := e.fillCopyLayer(dir, s, in, src, dst, own, chmod, skip); err != nil {
		os.RemoveAll(dir)
		return err
	}
	if err := e.store.CommitLayer(dir, k); err != nil {
		return err
	}
	s.layers, s.key = append(s.layers, k), k
	return nil
}

func (e *Engine) fillCopyLayer(dir string, s *stageState, in dockerfile.Instr, src *os.Root, dst string, own owner, chmod fs.FileMode, skip func(string) bool) error {
	layer, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer layer.Close()
	if err := os.Chown(dir, e.store.IDShift(), e.store.IDShift()); err != nil {
		return err
	}
	lowers := e.layerPaths(s.layers)
	rootUID := e.store.IDShift()
	dstRel := cleanRel(dst)
	dstIsDir := strings.HasSuffix(dst, "/") || len(in.Src) > 1

	if in.Heredoc != nil {
		if err := ensureParents(layer, lowers, path.Dir(dstRel), rootUID); err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if chmod != 0 {
			mode = chmod
		}
		f, err := layer.OpenFile(dstRel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, in.Heredoc.Body)
		f.Close()
		if err != nil {
			return err
		}
		if err := layer.Chmod(dstRel, mode); err != nil {
			return err
		}
		return layer.Lchown(dstRel, own.uid, own.gid)
	}

	var sources []string
	for _, p := range in.Src {
		rel := cleanRel(p)
		if _, err := src.Lstat(rel); err == nil {
			sources = append(sources, rel)
			continue
		}
		matches, _ := globRoot(src, p)
		sources = append(sources, matches...)
	}
	if len(sources) == 0 {
		return fmt.Errorf("no source files matched %v", in.Src)
	}
	if len(sources) > 1 {
		dstIsDir = true
	}
	for _, rel := range sources {
		fi, err := src.Lstat(rel)
		if err != nil {
			return err
		}
		target := dstRel
		if fi.IsDir() {
			// Directory sources copy their contents into the destination.
			if err := ensureParents(layer, lowers, dstRel, rootUID); err != nil {
				return err
			}
			ents, err := readDir(src, rel)
			if err != nil {
				return err
			}
			for _, name := range ents {
				if err := copyTree(src, path.Join(rel, name), layer, path.Join(dstRel, name), own, chmod, skip); err != nil {
					return err
				}
			}
			continue
		}
		if dstIsDir {
			target = path.Join(dstRel, path.Base(rel))
		}
		if err := ensureParents(layer, lowers, path.Dir(target), rootUID); err != nil {
			return err
		}
		if err := copyTree(src, rel, layer, target, own, chmod, skip); err != nil {
			return err
		}
	}
	return nil
}

func readDir(r *os.Root, rel string) ([]string, error) {
	d, err := r.Open(rel)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// globRoot expands a glob pattern against paths in r (one directory level
// of wildcards, which covers patterns like package*.json and src/*.py).
func globRoot(r *os.Root, pattern string) ([]string, error) {
	rel := cleanRel(pattern)
	dir, base := path.Split(rel)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = "."
	}
	names, err := readDir(r, dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		if ok, _ := path.Match(base, n); ok {
			out = append(out, path.Join(dir, n))
		}
	}
	return out, nil
}

// chownTo resolves --chown=user[:group] against the stage's own passwd/group.
func (e *Engine) chownTo(spec string, layers []string) (owner, error) {
	shift := e.store.IDShift()
	if spec == "" {
		return owner{uid: shift, gid: shift}, nil
	}
	u, g, hasGroup := strings.Cut(spec, ":")
	paths := e.layerPaths(layers)
	uid, err := lookupID(u, readFromLayers(paths, "etc/passwd"))
	if err != nil {
		return owner{}, err
	}
	gid := uid
	if hasGroup {
		if gid, err = lookupID(g, readFromLayers(paths, "etc/group")); err != nil {
			return owner{}, err
		}
	}
	return owner{uid: uid + shift, gid: gid + shift}, nil
}

func lookupID(name, table string) (int, error) {
	if n, err := strconv.Atoi(name); err == nil {
		return n, nil
	}
	for _, line := range strings.Split(table, "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 3 && f[0] == name {
			return strconv.Atoi(f[2])
		}
	}
	return 0, fmt.Errorf("unknown user or group %q", name)
}

// mountView mounts a read-only merged view of layers, for COPY --from.
func (e *Engine) mountView(layers []string) (string, func(), error) {
	paths := e.layerPaths(layers)
	if len(paths) == 1 {
		return paths[0], func() {}, nil
	}
	if len(paths) == 0 {
		dir, err := e.store.TempDir("empty-")
		return dir, func() { os.RemoveAll(dir) }, err
	}
	mnt, err := e.store.TempDir("view-")
	if err != nil {
		return "", nil, err
	}
	lower := make([]string, 0, len(paths))
	for i := len(paths) - 1; i >= 0; i-- {
		lower = append(lower, paths[i])
	}
	if err := mountOverlayRO(mnt, lower); err != nil {
		os.RemoveAll(mnt)
		return "", nil, err
	}
	return mnt, func() { unmount(mnt); os.RemoveAll(mnt) }, nil
}

func (e *Engine) layerPaths(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = e.store.LayerPath(id)
	}
	return out
}

// cacheDir returns the host directory backing a RUN cache mount, private to
// scope and owned by the sandbox's root.
func (e *Engine) cacheDir(scope, target string) (string, error) {
	if scope == "" {
		scope = "default"
	}
	dir := filepath.Join(e.root, "cache", key(scope)[:16], key(target)[:16])
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if err := os.Chown(dir, e.store.IDShift(), e.store.IDShift()); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func setEnv(env []string, k, v string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, k+"=") {
			out = append(out, e)
		}
	}
	return append(out, k+"="+v)
}

func orRoot(wd string) string {
	if wd == "" {
		return "/"
	}
	return wd
}

// lineWriter adapts streamed output to a per-line callback.
type lineWriterT struct {
	pw   *io.PipeWriter
	done chan struct{}
}

func lineWriter(fn func(string)) *lineWriterT {
	pr, pw := io.Pipe()
	lw := &lineWriterT{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(lw.done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			fn(sc.Text())
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	return lw
}

func (l *lineWriterT) Write(p []byte) (int, error) { return l.pw.Write(p) }

func (l *lineWriterT) Close() {
	l.pw.Close()
	<-l.done
}
