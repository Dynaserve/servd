package oci

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
)

// Export writes an image to dir in the standard OCI image layout, so it can be
// pushed to any registry or run by any OCI runtime (docker, containerd, …).
// Layers are re-tarred from their unpacked form: overlay whiteouts become OCI
// ".wh." entries and owners are shifted back to their in-image ids.
func (s *Store) Export(name, dir string) error {
	img, err := s.Image(name)
	if err != nil {
		return err
	}
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		return err
	}

	cfg := v1.Image{
		Platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH},
		Config:   img.Config,
		RootFS:   v1.RootFS{Type: "layers"},
		Created:  &img.Created,
	}
	man := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest}
	for _, id := range img.Layers {
		desc, diffID, err := s.exportLayer(id, blobs)
		if err != nil {
			return fmt.Errorf("layer %s: %w", id[:12], err)
		}
		man.Layers = append(man.Layers, desc)
		cfg.RootFS.DiffIDs = append(cfg.RootFS.DiffIDs, diffID)
	}
	if man.Config, err = writeJSONBlob(blobs, v1.MediaTypeImageConfig, cfg); err != nil {
		return err
	}
	mdesc, err := writeJSONBlob(blobs, v1.MediaTypeImageManifest, man)
	if err != nil {
		return err
	}
	// Name the image the way both OCI tools and Docker/containerd look for it.
	full, tag := name, "latest"
	if ref, err := ParseRef(name); err == nil {
		tag = ref.Reference
		host := ref.Registry
		if host == dockerHub {
			host = "docker.io"
		}
		full = host + "/" + ref.Repo + ":" + tag
	}
	mdesc.Annotations = map[string]string{v1.AnnotationRefName: tag, "io.containerd.image.name": full}
	mdesc.Platform = &cfg.Platform
	index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{mdesc}}
	if err := writeJSON(filepath.Join(dir, "index.json"), index); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, v1.ImageLayoutFile), v1.ImageLayout{Version: v1.ImageLayoutVersion})
}

// exportLayer writes one layer as a gzipped tar blob, returning its
// descriptor and uncompressed digest (diffID).
func (s *Store) exportLayer(id, blobs string) (v1.Descriptor, digest.Digest, error) {
	tmp, err := os.CreateTemp(blobs, "layer-*")
	if err != nil {
		return v1.Descriptor{}, "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	blobHash, diffHash := sha256.New(), sha256.New()
	counter := &countWriter{w: io.MultiWriter(tmp, blobHash)}
	zw := gzip.NewWriter(counter)
	if err := s.writeLayerTar(s.LayerPath(id), io.MultiWriter(zw, diffHash)); err != nil {
		return v1.Descriptor{}, "", err
	}
	if err := zw.Close(); err != nil {
		return v1.Descriptor{}, "", err
	}
	if err := tmp.Close(); err != nil {
		return v1.Descriptor{}, "", err
	}
	d := digest.NewDigest(digest.SHA256, blobHash)
	if err := os.Rename(tmp.Name(), filepath.Join(blobs, d.Encoded())); err != nil {
		return v1.Descriptor{}, "", err
	}
	return v1.Descriptor{MediaType: v1.MediaTypeImageLayerGzip, Digest: d, Size: counter.n},
		digest.NewDigest(digest.SHA256, diffHash), nil
}

// writeLayerTar serializes an unpacked (overlay-format) layer directory.
func (s *Store) writeLayerTar(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	links := map[uint64]string{} // inode -> first path, for hardlinks
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		st := fi.Sys().(*syscall.Stat_t)

		// Overlay whiteout (0/0 char device) -> OCI ".wh.<name>".
		if fi.Mode()&fs.ModeCharDevice != 0 && st.Rdev == 0 {
			return tw.WriteHeader(&tar.Header{Name: filepath.Join(filepath.Dir(rel), whiteoutPrefix+fi.Name()), Typeflag: tar.TypeReg, Mode: 0o600})
		}

		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		hdr.Uid, hdr.Gid = s.unshift(int(st.Uid)), s.unshift(int(st.Gid))
		hdr.Uname, hdr.Gname = "", ""
		if fi.IsDir() {
			hdr.Name += "/"
		}
		if fi.Mode().IsRegular() && st.Nlink > 1 {
			if first, ok := links[st.Ino]; ok {
				hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeLink, first, 0
			} else {
				links[st.Ino] = rel
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.IsDir() && isOpaque(p) {
			if err := tw.WriteHeader(&tar.Header{Name: filepath.Join(rel, opaqueMarker), Typeflag: tar.TypeReg, Mode: 0o600}); err != nil {
				return err
			}
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func (s *Store) unshift(id int) int {
	if id >= s.idShift {
		return id - s.idShift
	}
	return id
}

func isOpaque(dir string) bool {
	buf := make([]byte, 1)
	n, err := unix.Lgetxattr(dir, opaqueXattr, buf)
	return err == nil && n == 1 && buf[0] == 'y'
}

func writeJSONBlob(blobs, mediaType string, v any) (v1.Descriptor, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return v1.Descriptor{}, err
	}
	d := digest.FromBytes(b)
	if err := os.WriteFile(filepath.Join(blobs, d.Encoded()), b, 0o644); err != nil {
		return v1.Descriptor{}, err
	}
	return v1.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(b))}, nil
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
