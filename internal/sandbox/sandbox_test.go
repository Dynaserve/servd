package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"servd/platform/internal/oci"
)

// Integration tests: need root, runc and network access to pull an image.
// Run with SERVD_INTEGRATION=1 go test ./internal/sandbox
func setup(t *testing.T) (*Runc, []string) {
	t.Helper()
	if os.Getenv("SERVD_INTEGRATION") != "1" || os.Geteuid() != 0 {
		t.Skip("set SERVD_INTEGRATION=1 and run as root")
	}
	base := os.Getenv("SERVD_TEST_DIR")
	if base == "" {
		base = "/var/lib/servd-test"
	}
	if err := os.MkdirAll(base, 0o711); err != nil {
		t.Fatal(err)
	}
	store, err := oci.NewStore(filepath.Join(base, "images"), 100000, os.Getenv("REGISTRY_MIRROR"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := store.Resolve(context.Background(), "node:22-alpine", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var layers []string
	for _, id := range img.Layers {
		layers = append(layers, store.LayerPath(id))
	}
	network, err := NewNetwork("servdtest0", "10.89.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRunc(filepath.Join(base, "sandboxes"), network, 100000, "")
	if err != nil {
		t.Fatal(err)
	}
	return r, layers
}

func run(t *testing.T, r *Runc, s *Spec) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := r.Run(context.Background(), s, &out)
	return out.String(), err
}

func TestRunIsolation(t *testing.T) {
	r, layers := setup(t)
	upper := filepath.Join(r.state, "test-upper")
	os.RemoveAll(upper)
	defer os.RemoveAll(upper)

	start := time.Now()
	out, err := run(t, r, &Spec{
		ID: "t-run", Layers: layers, UpperDir: upper, Limits: Limits{MemoryMB: 128, Pids: 64},
		Args: []string{"sh", "-c", `
			echo uid=$(id -u); grep CapEff /proc/self/status; cat /proc/self/uid_map | tr -s ' '
			echo hello > /made-here
			cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null || cat /sys/fs/cgroup/memory.max
			node -e "try{require('child_process').execSync('unshare -U true',{stdio:'pipe'});console.log('unshare=allowed')}catch(e){console.log('unshare=denied')}"
			exit 7`},
	})
	t.Logf("run took %s\n%s", time.Since(start), out)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 7 {
		t.Fatalf("want exit code 7, got %v", err)
	}
	for _, want := range []string{"uid=0", "CapEff:\t0000000000000000", "0 100000 65536", "134217728", "unshare=denied"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	fi, err := os.Stat(filepath.Join(upper, "made-here"))
	if err != nil {
		t.Fatalf("upper dir did not capture the write: %v", err)
	}
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != 100000 {
		t.Errorf("file written by container root is host uid %d, want 100000", st.Uid)
	}
	if _, err := os.Stat(r.dir("t-run")); !os.IsNotExist(err) {
		t.Error("sandbox state not cleaned up")
	}
}

func TestNetworkIsolation(t *testing.T) {
	r, layers := setup(t)
	server := func(id string) *Info {
		info, err := r.Start(context.Background(), &Spec{
			ID: id, Layers: layers, Network: true, Limits: Limits{MemoryMB: 128, CPUs: 0.5, Pids: 64},
			Args: []string{"node", "-e", `require('http').createServer((q,s)=>s.end('hi from ` + id + `')).listen(8080)`},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Stop(id); r.Forget(id) })
		return info
	}
	a, b := server("t-net-a"), server("t-net-b")

	// Host (the platform proxy) can reach a sandbox.
	var body string
	for i := 0; i < 50 && body == ""; i++ {
		if resp, err := http.Get("http://" + a.IP + ":8080/"); err == nil {
			bb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(bb)
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if body != "hi from t-net-a" {
		t.Fatalf("host -> sandbox: got %q", body)
	}

	// A host service on the bridge gateway, which sandboxes must not reach.
	ln, err := net.Listen("tcp", r.net.Gateway().String()+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("host")) }))

	probe := fmt.Sprintf(`
		try_get() { wget -q -T 3 -O - "$1" >/dev/null 2>&1 && echo "$2=reachable" || echo "$2=blocked"; }
		try_get http://%s:8080/ peer
		try_get http://%s/ host
		try_get http://169.254.169.254/ metadata
		nslookup registry.npmjs.org >/dev/null 2>&1 && echo internet=reachable || echo internet=blocked`, b.IP, ln.Addr().String())
	out, err := run(t, r, &Spec{ID: "t-net-probe", Layers: layers, Network: true, Args: []string{"sh", "-c", probe}})
	t.Log(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"peer=blocked", "host=blocked", "metadata=blocked", "internet=reachable"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %s", want)
		}
	}
}
