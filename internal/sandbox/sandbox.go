// Package sandbox runs untrusted code (customer builds and apps) in hardened,
// daemonless containers.
//
// Everything that decides how isolated a sandbox is lives here: the overlay
// rootfs, the user namespace that maps container root to an unprivileged host
// uid, the per-sandbox network namespace, capabilities, the seccomp filter
// and cgroup limits. The OCI runtime (runc/crun) only performs the final
// clone + pivot_root from the spec we generate.
//
// Callers talk to the Backend interface, which is deliberately high level
// (layers + process + limits + network rather than an OCI bundle) so a
// microVM backend (Firecracker: layers → virtio-blk, network → tap) can be
// added without touching them.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"

	"golang.org/x/sys/unix"
)

// Spec describes one sandbox.
type Spec struct {
	ID     string   `json:"id"`     // unique; [a-z0-9-], ≤ 40 chars
	Layers []string `json:"layers"` // read-only lower layer dirs, bottom → top
	// UpperDir, when set, receives the sandbox's filesystem changes and is
	// kept after Run returns (the builder turns it into a layer). Otherwise a
	// private, discarded upper is used. Must be on the same filesystem as
	// the backend's state directory.
	UpperDir string   `json:"upperDir,omitempty"`
	Args     []string `json:"args"`
	Env      []string `json:"env"`
	Cwd      string   `json:"cwd"`
	User     string   `json:"user,omitempty"` // "uid[:gid]" or a name from the image's /etc/passwd
	Hostname string   `json:"hostname,omitempty"`
	Mounts   []Mount  `json:"mounts,omitempty"`
	Limits   Limits   `json:"limits"`
	// Network gives the sandbox an address on the platform bridge (egress
	// via NAT). Without it the sandbox has loopback only.
	Network bool `json:"network"`
	// Build grants the capabilities package managers expect (chown, setuid…).
	// They are namespaced to the sandbox's user namespace, so they carry no
	// power over the host; long-running apps get none at all.
	Build bool `json:"build"`
}

// Mount is an extra bind mount, e.g. a build cache.
type Mount struct {
	Source   string `json:"source"` // host path
	Target   string `json:"target"` // path in the sandbox
	ReadOnly bool   `json:"readOnly"`
}

// Limits caps a sandbox's resources. Zero values mean the defaults.
type Limits struct {
	MemoryMB int     `json:"memoryMB"`
	CPUs     float64 `json:"cpus"`
	Pids     int     `json:"pids"`
}

// Info describes a started sandbox.
type Info struct {
	ID string
	IP string // address on the platform bridge; "" without Network
}

// Backend runs sandboxes.
type Backend interface {
	// Run executes spec to completion, streaming its output to out. A
	// non-zero exit is reported as an *ExitError.
	Run(ctx context.Context, spec *Spec, out io.Writer) error
	// Start launches spec in the background; its output goes to LogFile.
	Start(ctx context.Context, spec *Spec) (*Info, error)
	// Stop terminates a sandbox (SIGTERM, then SIGKILL) and frees everything
	// it held. Stopping an unknown sandbox is not an error.
	Stop(id string) error
	// Alive reports whether a started sandbox's process is running.
	Alive(id string) bool
	// LogFile is the path of a started sandbox's combined stdout/stderr.
	LogFile(id string) string
	// Started lists sandboxes with state on disk (for reconciling after a
	// platform restart), with the spec each was started from.
	Started() ([]*Spec, error)
}

// ExitError reports a sandboxed process that exited unsuccessfully.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit code %d", e.Code) }

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func validID(id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("invalid sandbox id %q", id)
	}
	return nil
}

// --- namespace pair ---
//
// Each sandbox gets a user namespace (container root → IDShift on the host)
// and a network namespace owned by it, created here rather than by the OCI
// runtime so the network can be wired up from the host first. Because the
// netns belongs to the sandbox's userns, the sandbox can mount its own /sys
// and set its own net sysctls, yet has no power over the host's network.

const nsHolderEnv = "SERVD_SANDBOX_NS_HOLDER"

func init() {
	// Re-exec'd namespace holder: stay alive until the parent has bind-mounted
	// our namespaces (it closes our stdin), then exit.
	if os.Getenv(nsHolderEnv) == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
}

// createNamespaces creates a userns+netns pair and pins them as files
// dir/user and dir/net. dir becomes a private mount point.
func createNamespaces(dir string, idShift, size int) error {
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return err
	}
	if err := unix.Mount(dir, dir, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind ns dir: %w", err)
	}
	if err := unix.Mount("", dir, "", unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make ns dir private: %w", err)
	}

	cmd := exec.Command("/proc/self/exe")
	cmd.Env = []string{nsHolderEnv + "=1"}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	idmap := []syscall.SysProcIDMap{{ContainerID: 0, HostID: idShift, Size: size}}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: idmap,
		GidMappings: idmap,
		Pdeathsig:   syscall.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("create namespaces: %w", err)
	}
	defer func() {
		stdin.Close()
		_ = cmd.Wait()
	}()
	for _, ns := range []string{"user", "net"} {
		target := filepath.Join(dir, ns)
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			return err
		}
		src := fmt.Sprintf("/proc/%d/ns/%s", cmd.Process.Pid, ns)
		if err := unix.Mount(src, target, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("pin %s namespace: %w", ns, err)
		}
	}
	return nil
}

// removeNamespaces unpins a namespace pair created by createNamespaces.
func removeNamespaces(dir string) {
	for _, ns := range []string{"user", "net"} {
		_ = unix.Unmount(filepath.Join(dir, ns), unix.MNT_DETACH)
	}
	_ = unix.Unmount(dir, unix.MNT_DETACH)
	_ = os.RemoveAll(dir)
}

func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
