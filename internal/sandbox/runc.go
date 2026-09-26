package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// idRange is the number of ids mapped into each sandbox's user namespace.
const idRange = 65536

// Runc is the default Backend: our own bundle (rootfs, namespaces, network,
// spec) executed by an OCI runtime binary (runc or crun).
type Runc struct {
	bin     string // runtime binary
	root    string // runtime state dir (tmpfs; lost on reboot)
	state   string // our per-sandbox dirs (persistent)
	net     *Network
	idShift int
}

// NewRunc creates the backend. stateDir holds per-sandbox state and must be
// on the same filesystem as any Spec.UpperDir; every directory above it must
// be traversable (o+x) since sandbox root is an unprivileged host uid. bin is
// the OCI runtime ("" finds runc or crun on PATH).
func NewRunc(stateDir string, network *Network, idShift int, bin string) (*Runc, error) {
	if bin == "" {
		for _, cand := range []string{"runc", "crun"} {
			if p, err := exec.LookPath(cand); err == nil {
				bin = p
				break
			}
		}
	}
	if bin == "" {
		return nil, errors.New("no OCI runtime found (install runc or crun)")
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("the sandbox backend must run as root")
	}
	if err := os.MkdirAll(stateDir, 0o711); err != nil {
		return nil, err
	}
	if err := os.Chmod(stateDir, 0o711); err != nil {
		return nil, err
	}
	r := &Runc{bin: bin, root: "/run/servd/runtime", state: stateDir, net: network, idShift: idShift}
	// Sandboxes that outlived a platform restart keep their addresses.
	if network != nil {
		if ents, err := os.ReadDir(stateDir); err == nil {
			for _, e := range ents {
				network.Reserve(net.ParseIP(r.ip(e.Name())))
			}
		}
	}
	return r, nil
}

func (r *Runc) dir(id string) string { return filepath.Join(r.state, id) }

// LogFile implements Backend.
func (r *Runc) LogFile(id string) string { return filepath.Join(r.dir(id), "log") }

// Run implements Backend.
func (r *Runc) Run(ctx context.Context, spec *Spec, out io.Writer) error {
	if err := r.prepare(ctx, spec); err != nil {
		return err
	}
	defer r.cleanup(spec.ID)

	cmd := exec.CommandContext(ctx, r.bin, "--root", r.root, "run", "--bundle", r.dir(spec.ID), spec.ID)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Cancel = func() error {
		_ = exec.Command(r.bin, "--root", r.root, "kill", spec.ID, "KILL").Run()
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return &ExitError{Code: ee.ExitCode()}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Start implements Backend.
func (r *Runc) Start(ctx context.Context, spec *Spec) (*Info, error) {
	if err := r.prepare(ctx, spec); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(r.LogFile(spec.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		r.cleanup(spec.ID)
		return nil, err
	}
	defer log.Close()
	// Owned by the app's own (mapped) user: apps that reopen /dev/stdout or
	// /dev/stderr (nginx does) go through /proc/self/fd, which re-checks
	// permissions on the file itself.
	uid, gid := r.owner(spec.ID)
	if err := log.Chown(r.idShift+uid, r.idShift+gid); err != nil {
		r.cleanup(spec.ID)
		return nil, err
	}
	cmd := exec.CommandContext(ctx, r.bin, "--root", r.root, "run", "--detach", "--bundle", r.dir(spec.ID), spec.ID)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		// runc reports start failures on the stdio it shares with the app.
		tail, _ := os.ReadFile(r.LogFile(spec.ID))
		r.cleanup(spec.ID)
		return nil, fmt.Errorf("start sandbox: %v: %s", err, lastLine(string(tail)))
	}
	return &Info{ID: spec.ID, IP: r.ip(spec.ID)}, nil
}

// Stop implements Backend.
func (r *Runc) Stop(id string) error {
	if validID(id) != nil {
		return nil
	}
	if _, err := os.Stat(r.dir(id)); isNotExist(err) {
		return nil
	}
	if r.Alive(id) {
		_ = exec.Command(r.bin, "--root", r.root, "kill", id, "TERM").Run()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && r.Alive(id); {
			time.Sleep(100 * time.Millisecond)
		}
	}
	r.cleanup(id)
	return nil
}

// Alive implements Backend.
func (r *Runc) Alive(id string) bool {
	out, err := exec.Command(r.bin, "--root", r.root, "state", id).Output()
	if err != nil {
		return false
	}
	var st struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(out, &st) == nil && st.Status == "running"
}

// Started implements Backend.
func (r *Runc) Started() ([]*Spec, error) {
	ents, err := os.ReadDir(r.state)
	if err != nil {
		return nil, err
	}
	var out []*Spec
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(r.state, e.Name(), "spec.json"))
		if err != nil {
			continue
		}
		var s Spec
		if json.Unmarshal(b, &s) == nil {
			out = append(out, &s)
		}
	}
	return out, nil
}

// IP returns the bridge address of a sandbox ("" if none).
func (r *Runc) ip(id string) string {
	b, _ := os.ReadFile(filepath.Join(r.dir(id), "ip"))
	return strings.TrimSpace(string(b))
}

// prepare builds the sandbox's bundle: namespaces, network, rootfs, config.
func (r *Runc) prepare(ctx context.Context, s *Spec) (err error) {
	if err := validID(s.ID); err != nil {
		return err
	}
	if len(s.Args) == 0 {
		return errors.New("sandbox has no command")
	}
	dir := r.dir(s.ID)
	if _, err := os.Stat(dir); err == nil {
		// A previous sandbox with this id (e.g. from before a crash).
		r.cleanup(s.ID)
	}
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			r.cleanup(s.ID)
		}
	}()
	if b, err := json.Marshal(s); err != nil {
		return err
	} else if err := os.WriteFile(filepath.Join(dir, "spec.json"), b, 0o600); err != nil {
		return err
	}

	// Namespaces and network.
	if err := createNamespaces(filepath.Join(dir, "ns"), r.idShift, idRange); err != nil {
		return err
	}
	netPath := filepath.Join(dir, "ns", "net")
	if s.Network && r.net != nil {
		ip, err := r.net.Attach(s.ID, netPath, net.ParseIP(r.lastIP(s.ID)))
		if err != nil {
			return fmt.Errorf("network: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ip"), []byte(ip.String()), 0o644); err != nil {
			return err
		}
	} else if err := LoopbackOnly(netPath); err != nil {
		return fmt.Errorf("network: %w", err)
	}

	// Rootfs: overlay of the image layers under a writable upper.
	upper, work := s.UpperDir, s.UpperDir+".work"
	if upper == "" {
		upper, work = filepath.Join(dir, "upper"), filepath.Join(dir, "work")
	}
	for _, d := range []string{upper, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := os.Chown(upper, r.idShift, r.idShift); err != nil {
		return err
	}
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		return err
	}
	lower := make([]string, 0, len(s.Layers))
	for i := len(s.Layers) - 1; i >= 0; i-- {
		lower = append(lower, s.Layers[i])
	}
	if len(lower) == 0 {
		empty := filepath.Join(dir, "empty")
		if err := os.MkdirAll(empty, 0o755); err != nil {
			return err
		}
		lower = []string{empty}
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", strings.Join(lower, ":"), upper, work)
	if len(opts) > unix.Getpagesize()-1 {
		return fmt.Errorf("image has too many layers (%d) to mount", len(s.Layers))
	}
	if err := unix.Mount("overlay", rootfs, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount rootfs: %w", err)
	}

	uid, gid, err := resolveUser(rootfs, s.User)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "owner"), fmt.Appendf(nil, "%d:%d", uid, gid), 0o600); err != nil {
		return err
	}
	if err := r.writeEtc(dir, s); err != nil {
		return err
	}
	cfg := r.ociSpec(s, dir, uid, gid)
	b, err := json.MarshalIndent(cfg, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600)
}

// owner is the in-sandbox uid:gid the sandbox's process runs as.
func (r *Runc) owner(id string) (int, int) {
	b, _ := os.ReadFile(filepath.Join(r.dir(id), "owner"))
	u, g, _ := strings.Cut(strings.TrimSpace(string(b)), ":")
	uid, _ := strconv.Atoi(u)
	gid, _ := strconv.Atoi(g)
	return uid, gid
}

// lastIP is the address a sandbox had before, kept across restarts.
func (r *Runc) lastIP(id string) string {
	b, _ := os.ReadFile(filepath.Join(r.state, "."+id+".ip"))
	return strings.TrimSpace(string(b))
}

// cleanup tears down everything prepare made. Safe to call repeatedly.
func (r *Runc) cleanup(id string) {
	dir := r.dir(id)
	_ = exec.Command(r.bin, "--root", r.root, "delete", "--force", id).Run()
	// Detach the rootfs before removing anything, so no delete can reach
	// through the overlay into shared image layers.
	_ = unix.Unmount(filepath.Join(dir, "rootfs"), unix.MNT_DETACH)
	if ip := r.ip(id); ip != "" {
		_ = os.WriteFile(filepath.Join(r.state, "."+id+".ip"), []byte(ip), 0o600)
		if r.net != nil {
			r.net.Detach(id, net.ParseIP(ip))
		}
	}
	removeNamespaces(filepath.Join(dir, "ns"))
	if b, err := os.ReadFile(filepath.Join(dir, "spec.json")); err == nil {
		var s Spec
		if json.Unmarshal(b, &s) == nil && s.UpperDir != "" {
			_ = os.RemoveAll(s.UpperDir + ".work")
		}
	}
	_ = os.RemoveAll(dir)
}

// Forget drops a sandbox's remembered address (when it is deleted for good).
func (r *Runc) Forget(id string) {
	_ = os.Remove(filepath.Join(r.state, "."+id+".ip"))
}

func (r *Runc) writeEtc(dir string, s *Spec) error {
	host := s.Hostname
	if host == "" {
		host = s.ID
	}
	var resolv strings.Builder
	if r.net != nil && s.Network {
		for _, ns := range r.net.dns {
			fmt.Fprintf(&resolv, "nameserver %s\n", ns)
		}
	}
	files := map[string]string{
		"hostname":    host + "\n",
		"hosts":       "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n127.0.1.1\t" + host + "\n",
		"resolv.conf": resolv.String(),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// buildCaps are granted to build sandboxes (namespaced: no host power).
var buildCaps = []string{
	"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID", "CAP_KILL", "CAP_SETGID",
	"CAP_SETUID", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE", "CAP_SYS_CHROOT", "CAP_AUDIT_WRITE",
}

func (r *Runc) ociSpec(s *Spec, dir string, uid, gid uint32) *specs.Spec {
	caps := []string{}
	if s.Build {
		caps = buildCaps
	}
	env := s.Env
	if !hasEnv(env, "PATH") {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	cwd := s.Cwd
	if cwd == "" {
		cwd = "/"
	}
	host := s.Hostname
	if host == "" {
		host = s.ID
	}
	mounts := []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup", Options: []string{"nosuid", "noexec", "nodev", "relatime", "ro"}},
	}
	for _, f := range []string{"hostname", "hosts", "resolv.conf"} {
		mounts = append(mounts, specs.Mount{Destination: "/etc/" + f, Type: "bind", Source: filepath.Join(dir, f), Options: []string{"rbind", "ro", "nosuid", "nodev", "noexec"}})
	}
	for _, m := range s.Mounts {
		opts := []string{"rbind", "nosuid", "nodev"}
		if m.ReadOnly {
			opts = append(opts, "ro")
		}
		mounts = append(mounts, specs.Mount{Destination: m.Target, Type: "bind", Source: m.Source, Options: opts})
	}

	lim := s.Limits
	mem := int64(lim.MemoryMB) << 20
	quota := int64(lim.CPUs * 100000)
	period := uint64(100000)
	pids := int64(lim.Pids)
	res := &specs.LinuxResources{}
	if mem > 0 {
		res.Memory = &specs.LinuxMemory{Limit: &mem, Swap: &mem}
	}
	if quota > 0 {
		res.CPU = &specs.LinuxCPU{Quota: &quota, Period: &period}
	}
	if pids > 0 {
		res.Pids = &specs.LinuxPids{Limit: &pids}
	}

	return &specs.Spec{
		Version:  specs.Version,
		Hostname: host,
		Root:     &specs.Root{Path: filepath.Join(dir, "rootfs")},
		Process: &specs.Process{
			User:            specs.User{UID: uid, GID: gid},
			Args:            s.Args,
			Env:             env,
			Cwd:             cwd,
			NoNewPrivileges: true,
			Capabilities:    &specs.LinuxCapabilities{Bounding: caps, Effective: caps, Permitted: caps},
			Rlimits:         []specs.POSIXRlimit{nofile()},
		},
		Mounts: mounts,
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.UserNamespace, Path: filepath.Join(dir, "ns", "user")},
				{Type: specs.NetworkNamespace, Path: filepath.Join(dir, "ns", "net")},
				{Type: specs.PIDNamespace}, {Type: specs.IPCNamespace}, {Type: specs.UTSNamespace},
				{Type: specs.MountNamespace}, {Type: specs.CgroupNamespace},
			},
			Resources:   res,
			CgroupsPath: "/servd/" + s.ID,
			Seccomp:     seccompProfile(),
			Sysctl: map[string]string{
				// Let non-root apps bind low ports (Docker's default too).
				"net.ipv4.ip_unprivileged_port_start": "0",
			},
			MaskedPaths: []string{
				"/proc/asound", "/proc/acpi", "/proc/kcore", "/proc/keys", "/proc/latency_stats",
				"/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug", "/proc/scsi",
				"/sys/firmware", "/sys/devices/virtual/powercap",
			},
			ReadonlyPaths: []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"},
		},
	}
}

// nofile allows up to 65536 open files, capped at the platform's own hard
// limit (a user namespace can't raise it).
func nofile() specs.POSIXRlimit {
	lim := uint64(65536)
	var cur unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_NOFILE, &cur) == nil && cur.Max < lim {
		lim = cur.Max
	}
	return specs.POSIXRlimit{Type: "RLIMIT_NOFILE", Hard: lim, Soft: lim}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func hasEnv(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

// resolveUser turns an image's USER ("", "uid", "uid:gid", "name",
// "name:group") into numeric ids, reading the image's own /etc/passwd and
// /etc/group through os.Root so a hostile image can't point us elsewhere.
func resolveUser(rootfs, user string) (uint32, uint32, error) {
	if user == "" {
		return 0, 0, nil
	}
	u, g, hasGroup := strings.Cut(user, ":")
	root, err := os.OpenRoot(rootfs)
	if err != nil {
		return 0, 0, err
	}
	defer root.Close()

	uid, gid := uint32(0), uint32(0)
	if n, err := strconv.ParseUint(u, 10, 32); err == nil {
		uid = uint32(n)
		if f := lookup(root, "etc/passwd", func(fields []string) bool { return fields[2] == u }); f != nil {
			gid = parseID(f[3])
		}
	} else if f := lookup(root, "etc/passwd", func(fields []string) bool { return fields[0] == u }); f != nil {
		uid, gid = parseID(f[2]), parseID(f[3])
	} else {
		return 0, 0, fmt.Errorf("user %q not found in the image", u)
	}
	if hasGroup {
		if n, err := strconv.ParseUint(g, 10, 32); err == nil {
			gid = uint32(n)
		} else if f := lookup(root, "etc/group", func(fields []string) bool { return fields[0] == g }); f != nil {
			gid = parseID(f[2])
		} else {
			return 0, 0, fmt.Errorf("group %q not found in the image", g)
		}
	}
	if uid >= idRange || gid >= idRange {
		return 0, 0, fmt.Errorf("user %q is outside the sandbox's id range", user)
	}
	return uid, gid, nil
}

func lookup(root *os.Root, file string, match func([]string) bool) []string {
	f, err := root.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) >= 4 && match(fields) {
			return fields
		}
	}
	return nil
}

func parseID(s string) uint32 {
	n, _ := strconv.ParseUint(s, 10, 32)
	return uint32(n)
}
