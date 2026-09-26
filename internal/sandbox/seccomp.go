package sandbox

import (
	"runtime"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// deniedSyscalls are refused (EPERM) in every sandbox. This is one layer of
// several: sandboxes also run in their own user namespace with no host
// privileges and (apps) no capabilities, so the list targets the kernel
// attack surface that remains reachable from there — the same calls Docker's
// default profile withholds, plus io_uring.
var deniedSyscalls = []string{
	// kernel modules, kexec, reboot, swap, clock
	"init_module", "finit_module", "delete_module", "create_module", "query_module", "get_kernel_syms",
	"kexec_load", "kexec_file_load", "reboot", "swapon", "swapoff",
	"settimeofday", "clock_settime", "clock_adjtime", "stime", "adjtimex",
	// mounting and namespace games
	"mount", "umount", "umount2", "pivot_root", "move_mount", "open_tree",
	"fsopen", "fsconfig", "fsmount", "fspick", "mount_setattr", "setns", "unshare",
	// kernel keyrings, BPF, perf, userfaultfd, io_uring
	"add_key", "request_key", "keyctl", "bpf", "perf_event_open", "userfaultfd",
	"io_uring_setup", "io_uring_enter", "io_uring_register",
	// peeking at other processes and handles
	"ptrace", "process_vm_readv", "process_vm_writev", "kcmp", "pidfd_getfd",
	"name_to_handle_at", "open_by_handle_at",
	// legacy / hardware access
	"acct", "lookup_dcookie", "nfsservctl", "quotactl", "sysfs", "_sysctl", "uselib", "ustat",
	"vm86", "vm86old", "iopl", "ioperm", "syslog",
}

// namespaceFlags may not be passed to clone(2).
var namespaceFlags = []uint64{
	unix.CLONE_NEWNS, unix.CLONE_NEWUTS, unix.CLONE_NEWIPC, unix.CLONE_NEWUSER,
	unix.CLONE_NEWPID, unix.CLONE_NEWNET, unix.CLONE_NEWCGROUP,
}

func seccompProfile() *specs.LinuxSeccomp {
	eperm := uint(unix.EPERM)
	enosys := uint(unix.ENOSYS)
	p := &specs.LinuxSeccomp{
		DefaultAction: specs.ActAllow,
		Architectures: seccompArches(),
		Syscalls: []specs.LinuxSyscall{
			{Names: deniedSyscalls, Action: specs.ActErrno, ErrnoRet: &eperm},
			// clone3 passes flags in a struct seccomp can't inspect; ENOSYS
			// makes libcs fall back to clone, whose flags we can check.
			{Names: []string{"clone3"}, Action: specs.ActErrno, ErrnoRet: &enosys},
		},
	}
	for _, f := range namespaceFlags {
		p.Syscalls = append(p.Syscalls, specs.LinuxSyscall{
			Names:    []string{"clone"},
			Action:   specs.ActErrno,
			ErrnoRet: &eperm,
			Args:     []specs.LinuxSeccompArg{{Index: 0, Value: f, ValueTwo: f, Op: specs.OpMaskedEqual}},
		})
	}
	return p
}

func seccompArches() []specs.Arch {
	switch runtime.GOARCH {
	case "arm64":
		return []specs.Arch{specs.ArchAARCH64, specs.ArchARM}
	default:
		return []specs.Arch{specs.ArchX86_64, specs.ArchX86, specs.ArchX32}
	}
}
