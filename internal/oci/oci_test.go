package oci

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestParseRef(t *testing.T) {
	for in, want := range map[string]string{
		"node:22-alpine":                    "registry-1.docker.io/library/node:22-alpine",
		"redis":                             "registry-1.docker.io/library/redis:latest",
		"bitnami/redis:7":                   "registry-1.docker.io/bitnami/redis:7",
		"docker.io/library/nginx":           "registry-1.docker.io/library/nginx:latest",
		"gcr.io/distroless/static:nonroot":  "gcr.io/distroless/static:nonroot",
		"localhost:5000/app":                "localhost:5000/app:latest",
		"ghcr.io/o/a@sha256:" + sixtyFour(): "ghcr.io/o/a@sha256:" + sixtyFour(),
	} {
		r, err := ParseRef(in)
		if err != nil || r.String() != want {
			t.Errorf("ParseRef(%q) = %v, %v; want %s", in, r, err, want)
		}
	}
	if _, err := ParseRef("bad ref"); err == nil {
		t.Error("expected error for ref with space")
	}
}

func sixtyFour() string { return "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" }

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func mkTar(t *testing.T, es []entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: mode, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	return &buf
}

func requireRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (chown, mknod, trusted xattrs)")
	}
}

func TestExtractCannotEscape(t *testing.T) {
	requireRoot(t)
	outside := t.TempDir()
	dir := t.TempDir()
	err := extractLayer(mkTar(t, []entry{
		{name: "evil", typ: tar.TypeSymlink, link: outside},
		{name: "evil/pwned", typ: tar.TypeReg, body: "x"},
	}), dir, 100000)
	if err == nil {
		t.Error("expected write through an escaping symlink to fail")
	}
	// "../" names are clamped to the layer root.
	if err := extractLayer(mkTar(t, []entry{{name: "../../up", typ: tar.TypeReg, body: "x"}}), dir, 100000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "up")); err != nil {
		t.Error("../ entry should land inside the layer")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("wrote outside the layer: %v", ents)
	}
}

func TestExtractFormatAndRoundTrip(t *testing.T) {
	requireRoot(t)
	dir := t.TempDir()
	err := extractLayer(mkTar(t, []entry{
		{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		{name: "etc/app.conf", typ: tar.TypeReg, body: "hello"},
		{name: "etc/hard", typ: tar.TypeLink, link: "etc/app.conf"},
		{name: "bin/su", typ: tar.TypeReg, body: "#!", mode: 0o4755},
		{name: "etc/.wh.gone", typ: tar.TypeReg},
		{name: "var/cache/.wh..wh..opq", typ: tar.TypeReg},
		{name: "dev/sda", typ: tar.TypeBlock},
	}), dir, 100000)
	if err != nil {
		t.Fatal(err)
	}
	st := stat(t, filepath.Join(dir, "etc/app.conf"))
	if st.Uid != 100000 || st.Gid != 100000 {
		t.Errorf("owner = %d:%d, want shifted to 100000", st.Uid, st.Gid)
	}
	if st.Nlink != 2 {
		t.Errorf("hardlink not preserved (nlink=%d)", st.Nlink)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "bin/su")); fi.Mode()&os.ModeSetuid == 0 {
		t.Error("setuid bit lost")
	}
	if wh := stat(t, filepath.Join(dir, "etc/gone")); wh.Mode&syscall.S_IFMT != syscall.S_IFCHR || wh.Rdev != 0 {
		t.Error("whiteout not converted to a 0/0 char device")
	}
	if !isOpaque(filepath.Join(dir, "var/cache")) {
		t.Error("opaque dir not marked")
	}
	if _, err := os.Lstat(filepath.Join(dir, "dev/sda")); err == nil {
		t.Error("device node should be dropped")
	}

	// Re-tar and re-extract: the result must match.
	s := &Store{idShift: 100000}
	var buf bytes.Buffer
	if err := s.writeLayerTar(dir, &buf); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	seen := map[string]*tar.Header{}
	for h, err := tr.Next(); err == nil; h, err = tr.Next() {
		seen[h.Name] = h
	}
	if h := seen["etc/app.conf"]; h == nil || h.Uid != 0 {
		t.Errorf("exported owner not unshifted: %+v", h)
	}
	for _, n := range []string{"etc/.wh.gone", "var/cache/.wh..wh..opq"} {
		if seen[n] == nil {
			t.Errorf("export missing %s", n)
		}
	}
	if h := seen["etc/hard"]; h == nil || h.Typeflag != tar.TypeLink {
		if h2 := seen["etc/app.conf"]; h2 == nil || h2.Typeflag != tar.TypeLink {
			t.Error("hardlink not exported as a link")
		}
	}
	dir2 := t.TempDir()
	if err := extractLayer(bytes.NewReader(buf.Bytes()), dir2, 100000); err != nil {
		t.Fatalf("re-extract: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir2, "etc/app.conf")); string(b) != "hello" {
		t.Errorf("round trip content = %q", b)
	}
}

func stat(t *testing.T, p string) *syscall.Stat_t {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t)
}
