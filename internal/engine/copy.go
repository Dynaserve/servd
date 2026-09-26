package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// owner is a uid/gid pair on the host (already shifted), or keep=true to
// preserve the source's owner.
type owner struct {
	uid, gid int
	keep     bool
}

// copyTree copies srcPath (relative to src) to dstPath (relative to dst).
// Both sides go through os.Root, so neither a hostile repo nor a hostile
// build output can make us read or write outside them. Symlinks are copied
// as symlinks, never followed. skip filters source paths (relative to src).
func copyTree(src *os.Root, srcPath string, dst *os.Root, dstPath string, own owner, chmod fs.FileMode, skip func(string) bool) error {
	if skip != nil && srcPath != "." && skip(srcPath) {
		return nil
	}
	fi, err := src.Lstat(srcPath)
	if err != nil {
		return err
	}
	uid, gid := own.uid, own.gid
	if own.keep {
		st := fi.Sys().(*syscall.Stat_t)
		uid, gid = int(st.Uid), int(st.Gid)
	}
	mode := fi.Mode()
	if chmod != 0 {
		mode = mode&^fs.ModePerm | chmod
	}

	// Replace anything of a different kind at the destination.
	if dfi, err := dst.Lstat(dstPath); err == nil && !(dfi.IsDir() && fi.IsDir()) {
		if err := dst.RemoveAll(dstPath); err != nil {
			return err
		}
	}

	switch {
	case fi.IsDir():
		if err := dst.Mkdir(dstPath, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		d, err := src.Open(srcPath)
		if err != nil {
			return err
		}
		ents, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return err
		}
		for _, e := range ents {
			if err := copyTree(src, path.Join(srcPath, e.Name()), dst, path.Join(dstPath, e.Name()), own, chmod, skip); err != nil {
				return err
			}
		}
	case mode.IsRegular():
		in, err := src.Open(srcPath)
		if err != nil {
			return err
		}
		out, err := dst.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			in.Close()
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	case mode&fs.ModeSymlink != 0:
		target, err := src.Readlink(srcPath)
		if err != nil {
			return err
		}
		if err := dst.Symlink(target, dstPath); err != nil {
			return err
		}
		return dst.Lchown(dstPath, uid, gid)
	default:
		return nil // sockets, devices, fifos: not copied
	}
	if err := dst.Lchown(dstPath, uid, gid); err != nil {
		return err
	}
	if err := dst.Chmod(dstPath, mode); err != nil {
		return err
	}
	return dst.Chtimes(dstPath, fi.ModTime(), fi.ModTime())
}

// hashTree feeds srcPath's content (paths, modes, file bytes, link targets)
// into h, for cache keys. mtimes are excluded: a fresh clone has new ones.
func hashTree(src *os.Root, srcPath string, h hash.Hash, skip func(string) bool) error {
	if skip != nil && srcPath != "." && skip(srcPath) {
		return nil
	}
	fi, err := src.Lstat(srcPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(h, "%s\x00%o\x00", srcPath, fi.Mode())
	switch {
	case fi.IsDir():
		d, err := src.Open(srcPath)
		if err != nil {
			return err
		}
		ents, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return err
		}
		for _, e := range ents { // ReadDir is sorted
			if err := hashTree(src, path.Join(srcPath, e.Name()), h, skip); err != nil {
				return err
			}
		}
	case fi.Mode().IsRegular():
		f, err := src.Open(srcPath)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, f)
		f.Close()
		return err
	case fi.Mode()&fs.ModeSymlink != 0:
		t, err := src.Readlink(srcPath)
		if err != nil {
			return err
		}
		io.WriteString(h, t)
	}
	return nil
}

// ignoreMatcher implements the common .dockerignore forms: exact paths,
// globs, directory prefixes and "**/name".
func ignoreMatcher(patterns []string) func(string) bool {
	if len(patterns) == 0 {
		return nil
	}
	return func(rel string) bool {
		for _, p := range patterns {
			if strings.HasPrefix(p, "!") {
				continue // exceptions are not supported; keep the file
			}
			p = strings.TrimPrefix(p, "./")
			if strings.HasPrefix(p, "**/") {
				if ok, _ := path.Match(strings.TrimPrefix(p, "**/"), path.Base(rel)); ok {
					return true
				}
				continue
			}
			if ok, _ := path.Match(p, rel); ok {
				return true
			}
			if strings.HasPrefix(rel, p+"/") {
				return true
			}
		}
		return false
	}
}

// cleanRel turns a source path from a Dockerfile into a path relative to the
// build context, clamped inside it.
func cleanRel(p string) string {
	c := strings.TrimPrefix(path.Clean("/"+p), "/")
	if c == "" {
		return "."
	}
	return c
}

// ensureParents creates dstDir's missing ancestors in a layer, copying each
// directory's owner and mode from the image below (so e.g. /tmp keeps 1777)
// or defaulting to root 0755.
func ensureParents(layer *os.Root, lowers []string, dir string, rootUID int) error {
	if dir == "." || dir == "" {
		return nil
	}
	if err := ensureParents(layer, lowers, path.Dir(dir), rootUID); err != nil {
		return err
	}
	if fi, err := layer.Lstat(dir); err == nil {
		if fi.IsDir() {
			return nil
		}
		if err := layer.RemoveAll(dir); err != nil {
			return err
		}
	}
	mode, uid, gid := fs.FileMode(0o755), rootUID, rootUID
	if fi := lowerStat(lowers, dir); fi != nil && fi.IsDir() {
		st := fi.Sys().(*syscall.Stat_t)
		mode, uid, gid = fi.Mode(), int(st.Uid), int(st.Gid)
	}
	if err := layer.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := layer.Lchown(dir, uid, gid); err != nil {
		return err
	}
	return layer.Chmod(dir, mode)
}

// lowerStat finds rel in the topmost layer that has it (nil if absent or
// whited out).
func lowerStat(layers []string, rel string) fs.FileInfo {
	for i := len(layers) - 1; i >= 0; i-- {
		fi, err := os.Lstat(filepath.Join(layers[i], rel))
		if err != nil {
			continue
		}
		if fi.Mode()&fs.ModeCharDevice != 0 && fi.Sys().(*syscall.Stat_t).Rdev == 0 {
			return nil // whiteout
		}
		return fi
	}
	return nil
}

// readFromLayers reads a small file (e.g. etc/passwd) from the topmost layer
// that has it, through os.Root.
func readFromLayers(layers []string, rel string) string {
	for i := len(layers) - 1; i >= 0; i-- {
		r, err := os.OpenRoot(layers[i])
		if err != nil {
			continue
		}
		f, err := r.Open(rel)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(f, 1<<20))
			f.Close()
			r.Close()
			return string(b)
		}
		r.Close()
	}
	return ""
}

func key(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		io.WriteString(h, p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
