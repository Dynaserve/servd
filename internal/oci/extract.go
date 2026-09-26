package oci

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// Overlayfs markers. Layers are stored in overlay's native format so they can
// be stacked as lowerdirs directly, with no per-container copying.
const (
	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh..wh..opq"
	opaqueXattr    = "trusted.overlay.opaque"
)

// extractLayer unpacks an (uncompressed) layer tar into dir, converting OCI
// whiteouts to overlayfs ones and shifting every owner by idShift so files
// owned by root in the image are owned by the unprivileged host id that
// container root maps to.
//
// Layers are untrusted input: every path goes through os.Root, so entries
// (including ones that traverse symlinks created by earlier entries) can
// never escape dir. Device nodes are dropped.
func extractLayer(r io.Reader, dir string, idShift int) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		if name == "" {
			continue // the layer root itself
		}
		parent, base := path.Split(name)
		parent = strings.TrimSuffix(parent, "/")
		if parent != "" {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}

		if base == opaqueMarker {
			if err := setOpaque(root, parent); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			continue
		}
		if strings.HasPrefix(base, whiteoutPrefix) {
			target := path.Join(parent, strings.TrimPrefix(base, whiteoutPrefix))
			_ = root.RemoveAll(target)
			if err := mknodIn(root, parent, path.Base(target), unix.S_IFCHR, 0); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			continue
		}

		// A later entry replaces an earlier one of a different kind.
		if fi, err := root.Lstat(name); err == nil && !(fi.IsDir() && hdr.Typeflag == tar.TypeDir) {
			if err := root.RemoveAll(name); err != nil {
				return err
			}
		}

		mode := hdr.FileInfo().Mode()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		case tar.TypeReg:
			f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
		case tar.TypeLink:
			target := strings.TrimPrefix(path.Clean("/"+hdr.Linkname), "/")
			if err := root.Link(target, name); err != nil {
				return fmt.Errorf("hardlink %s -> %s: %w", name, target, err)
			}
			continue // shares the target's inode, owner and mode
		case tar.TypeFifo:
			if err := mknodIn(root, parent, base, unix.S_IFIFO|uint32(mode.Perm()), 0); err != nil {
				return err
			}
		default:
			continue // devices and anything exotic are not allowed in app images
		}

		// Chown before chmod: chown clears setuid/setgid bits.
		if err := root.Lchown(name, hdr.Uid+idShift, hdr.Gid+idShift); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeSymlink {
			if err := root.Chmod(name, mode); err != nil {
				return err
			}
			_ = root.Chtimes(name, hdr.ModTime, hdr.ModTime)
		}
	}
}

// mknodIn creates a node named base inside parent (relative to root) without
// resolving base itself, so it cannot be redirected by a symlink.
func mknodIn(root *os.Root, parent, base string, mode uint32, dev int) error {
	if parent == "" {
		parent = "."
	}
	d, err := root.Open(parent)
	if err != nil {
		return err
	}
	defer d.Close()
	return unix.Mknodat(int(d.Fd()), base, mode, dev)
}

// setOpaque marks a directory (relative to root) as overlay-opaque, hiding
// the contents of the same directory in lower layers.
func setOpaque(root *os.Root, dir string) error {
	if dir == "" {
		dir = "."
	}
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return unix.Fsetxattr(int(d.Fd()), opaqueXattr, []byte("y"), 0)
}
