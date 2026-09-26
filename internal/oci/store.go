package oci

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
)

// Image is a locally stored image: its runtime config plus the IDs of its
// unpacked layers, bottom to top.
type Image struct {
	Name    string         `json:"name"`
	Config  v1.ImageConfig `json:"config"`
	Layers  []string       `json:"layers"`
	Created time.Time      `json:"created"`
}

// Store keeps images and unpacked layers under a root directory:
//
//	layers/<id>/        one directory per layer, in overlayfs format
//	images/<hash>.json  image records
type Store struct {
	root    string
	idShift int
	reg     *registry

	mu      sync.Mutex
	layerMu map[string]*sync.Mutex
}

// NewStore opens (creating if needed) a store at root. idShift is the host
// uid/gid that container root maps to; layers are stored pre-shifted by it.
// mirror optionally names a Docker Hub pull-through mirror (e.g. mirror.gcr.io).
func NewStore(root string, idShift int, mirror string) (*Store, error) {
	for _, d := range []string{"layers", "images", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o711); err != nil {
			return nil, err
		}
	}
	// Leftovers from interrupted pulls or builds.
	_ = os.RemoveAll(filepath.Join(root, "tmp"))
	_ = os.MkdirAll(filepath.Join(root, "tmp"), 0o711)
	return &Store{root: root, idShift: idShift, reg: newRegistry(mirror), layerMu: map[string]*sync.Mutex{}}, nil
}

// IDShift is the host id container root maps to.
func (s *Store) IDShift() int { return s.idShift }

// LayerPath returns the directory of an unpacked layer.
func (s *Store) LayerPath(id string) string { return filepath.Join(s.root, "layers", id) }

// HasLayer reports whether a layer is present.
func (s *Store) HasLayer(id string) bool {
	_, err := os.Stat(s.LayerPath(id))
	return err == nil
}

// Touch marks a layer as recently used (a build cache hit), so Prune keeps it.
func (s *Store) Touch(id string) {
	now := time.Now()
	_ = os.Chtimes(s.LayerPath(id), now, now)
}

// TempDir creates a scratch directory on the store's filesystem, so it can be
// committed as a layer with a cheap rename.
func (s *Store) TempDir(pattern string) (string, error) {
	return os.MkdirTemp(filepath.Join(s.root, "tmp"), pattern)
}

// CommitLayer atomically moves a finished directory into the store as layer
// id. If the layer already exists (a concurrent build made it), dir is removed.
func (s *Store) CommitLayer(dir, id string) error {
	if err := os.Rename(dir, s.LayerPath(id)); err != nil {
		if s.HasLayer(id) {
			return os.RemoveAll(dir)
		}
		return err
	}
	return nil
}

// SaveImage records an image under its name.
func (s *Store) SaveImage(img *Image) error {
	b, err := json.Marshal(img)
	if err != nil {
		return err
	}
	p := s.imagePath(img.Name)
	if err := os.WriteFile(p+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// Image loads a stored image by name.
func (s *Store) Image(name string) (*Image, error) {
	b, err := os.ReadFile(s.imagePath(name))
	if err != nil {
		return nil, err
	}
	var img Image
	return &img, json.Unmarshal(b, &img)
}

// RemoveImage deletes an image record (its layers stay, as others may share them).
func (s *Store) RemoveImage(name string) error {
	err := os.Remove(s.imagePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) imagePath(name string) string {
	h := sha256.Sum256([]byte(name))
	return filepath.Join(s.root, "images", hex.EncodeToString(h[:16])+".json")
}

func (s *Store) lockLayer(id string) func() {
	s.mu.Lock()
	m := s.layerMu[id]
	if m == nil {
		m = &sync.Mutex{}
		s.layerMu[id] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

type manifest struct {
	MediaType string          `json:"mediaType"`
	Config    v1.Descriptor   `json:"config"`
	Layers    []v1.Descriptor `json:"layers"`
	Manifests []v1.Descriptor `json:"manifests"` // set for indexes / manifest lists
}

// Pull fetches an image (and any layers not already present) from its
// registry. Layers download and unpack in parallel. When the registry is
// unreachable, a previously pulled copy is returned instead.
func (s *Store) Pull(ctx context.Context, name string) (*Image, error) {
	ref, err := ParseRef(name)
	if err != nil {
		return nil, err
	}
	img, err := s.pull(ctx, ref, name)
	if err != nil {
		if cached, cerr := s.Image(name); cerr == nil {
			return cached, nil
		}
		return nil, err
	}
	return img, nil
}

// Resolve returns a stored image if it was pulled within maxAge, skipping the
// registry round trip entirely; otherwise it pulls. Builds use this so hot
// base images cost nothing.
func (s *Store) Resolve(ctx context.Context, name string, maxAge time.Duration) (*Image, error) {
	if img, err := s.Image(name); err == nil && time.Since(img.Created) < maxAge && s.hasLayers(img) {
		return img, nil
	}
	return s.Pull(ctx, name)
}

func (s *Store) hasLayers(img *Image) bool {
	for _, id := range img.Layers {
		if !s.HasLayer(id) {
			return false
		}
	}
	return true
}

func (s *Store) pull(ctx context.Context, ref Ref, name string) (*Image, error) {
	m, err := s.manifest(ctx, ref, ref.Reference)
	if err != nil {
		return nil, err
	}
	if len(m.Manifests) > 0 {
		d, err := pickPlatform(m.Manifests)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if m, err = s.manifest(ctx, ref, d.Digest.String()); err != nil {
			return nil, err
		}
	}

	var cfg v1.Image
	if err := s.fetchJSON(ctx, ref, m.Config.Digest, &cfg); err != nil {
		return nil, fmt.Errorf("image config: %w", err)
	}

	img := &Image{Name: name, Config: cfg.Config, Created: time.Now()}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for _, l := range m.Layers {
		id := l.Digest.Encoded()
		img.Layers = append(img.Layers, id)
		g.Go(func() error { return s.pullLayer(gctx, ref, l) })
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return img, s.SaveImage(img)
}

func (s *Store) manifest(ctx context.Context, ref Ref, reference string) (*manifest, error) {
	resp, err := s.reg.get(ctx, ref, "manifests", reference, manifestAccept)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

func (s *Store) fetchJSON(ctx context.Context, ref Ref, d digest.Digest, out any) error {
	resp, err := s.reg.get(ctx, ref, "blobs", d.String(), "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	v := d.Verifier()
	b, err := io.ReadAll(io.TeeReader(io.LimitReader(resp.Body, 16<<20), v))
	if err != nil {
		return err
	}
	if !v.Verified() {
		return fmt.Errorf("digest mismatch for %s", d)
	}
	return json.Unmarshal(b, out)
}

// pullLayer downloads, verifies and unpacks one layer, unless present.
func (s *Store) pullLayer(ctx context.Context, ref Ref, l v1.Descriptor) error {
	id := l.Digest.Encoded()
	defer s.lockLayer(id)()
	if s.HasLayer(id) {
		return nil
	}
	resp, err := s.reg.get(ctx, ref, "blobs", l.Digest.String(), "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	v := l.Digest.Verifier()
	var body io.Reader = io.TeeReader(resp.Body, v)
	switch {
	case strings.HasSuffix(l.MediaType, "gzip"):
		zr, err := gzip.NewReader(body)
		if err != nil {
			return fmt.Errorf("layer %s: %w", id[:12], err)
		}
		defer zr.Close()
		body = zr
	case strings.HasSuffix(l.MediaType, ".tar"):
	default:
		return fmt.Errorf("layer %s: unsupported media type %s", id[:12], l.MediaType)
	}

	dir, err := s.TempDir("pull-")
	if err != nil {
		return err
	}
	if err := extractLayer(body, dir, s.idShift); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("layer %s: %w", id[:12], err)
	}
	// Drain trailing bytes so the digest covers the whole blob.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil || !v.Verified() {
		os.RemoveAll(dir)
		return fmt.Errorf("layer %s: digest verification failed", id[:12])
	}
	return s.CommitLayer(dir, id)
}

// pickPlatform chooses the linux manifest for this host's architecture.
func pickPlatform(ds []v1.Descriptor) (v1.Descriptor, error) {
	for _, d := range ds {
		if p := d.Platform; p != nil && p.OS == "linux" && p.Architecture == runtime.GOARCH {
			return d, nil
		}
	}
	return v1.Descriptor{}, fmt.Errorf("no linux/%s image", runtime.GOARCH)
}
