//go:build linux

package codexbridge

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Landlock confines the Codex child so the no-write guarantee does not rest on
// Codex's own policy code (design §2.12, decision §4.3). ezllm re-execs itself
// as a helper (LandlockHelperArg) that builds a ruleset, restricts itself and
// execs the CLI; the child and everything it spawns inherit the domain.
//
// Rules: read+execute on the system paths a static binary and its TLS stack
// need plus the CLI's own directory; full access on the per-request work dir
// and CODEX_HOME; write on /dev/null. Everything else — /data (ledger, master
// key), /app, other subscription logins — is out of reach. Network is not
// restricted: the CLI must reach the subscription.

// landlockRights per ABI version (kernel landlock(7)).
func landlockHandled(abi int) uint64 {
	rights := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		rights |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		rights |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		rights |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return rights
}

const (
	landlockRead      = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	landlockFileWrite = unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE
	landlockFileRead  = unix.LANDLOCK_ACCESS_FS_READ_FILE
)

// rulesetAttr is the ABI-1 prefix of struct landlock_ruleset_attr; the kernel
// accepts the short size on every version.
type rulesetAttr struct{ handledFS uint64 }

// pathBeneath mirrors the packed struct landlock_path_beneath_attr (12 bytes:
// the Go struct's tail padding is never read).
type pathBeneath struct {
	allowed  uint64
	parentFD int32
}

// LandlockABI reports the kernel's Landlock ABI version, or why there is none
// (old kernel, LSM not active, syscall blocked by a seccomp profile).
func LandlockABI() (int, error) {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		switch errno {
		case unix.ENOSYS:
			return 0, errors.New("the kernel has no Landlock (needs Linux 5.13+)")
		case unix.EOPNOTSUPP:
			return 0, errors.New("Landlock is built in but not active (add it to the kernel's lsm= boot parameter)")
		case unix.EPERM:
			return 0, errors.New("the Landlock syscalls are blocked (container seccomp profile)")
		}
		return 0, fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	return int(abi), nil
}

// landlockRestrict confines the calling thread: read-only on ro, full on rw,
// file read on roFiles, file write on rwFiles. The caller must hold the OS
// thread and exec from it.
func landlockRestrict(ro, rw, roFiles, rwFiles []string) error {
	abi, err := LandlockABI()
	if err != nil {
		return err
	}
	handled := landlockHandled(abi)
	attr := rulesetAttr{handledFS: handled}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	defer unix.Close(int(fd))
	add := func(path string, access uint64) error {
		pfd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return nil
			}
			return fmt.Errorf("landlock: open %s: %w", path, err)
		}
		defer unix.Close(pfd)
		rule := pathBeneath{allowed: access & handled, parentFD: int32(pfd)}
		if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(fd), unix.LANDLOCK_RULE_PATH_BENEATH,
			uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
			return fmt.Errorf("landlock_add_rule %s: %w", path, errno)
		}
		return nil
	}
	for _, p := range ro {
		if err := add(p, landlockRead); err != nil {
			return err
		}
	}
	for _, p := range rw {
		if err := add(p, handled); err != nil {
			return err
		}
	}
	for _, p := range roFiles {
		if err := add(p, landlockFileRead); err != nil {
			return err
		}
	}
	for _, p := range rwFiles {
		if err := add(p, landlockFileWrite); err != nil {
			return err
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("landlock: no_new_privs: %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}

// LandlockMain is the helper's entry point: argv after LandlockHelperArg is
// `--ro P … --rw P … --rofile P … --rwfile P … -- BIN ARGS…`, or `--selftest DIR FORBIDDEN`.
// It never returns.
func LandlockMain(args []string) {
	runtime.LockOSThread() // the restriction and execve must share a thread
	if len(args) == 3 && args[0] == "--selftest" {
		os.Exit(landlockSelftest(args[1], args[2]))
	}
	var ro, rw, roFiles, rwFiles []string
	i := 0
	for ; i < len(args) && args[i] != "--"; i += 2 {
		if i+1 >= len(args) {
			landlockDie("flag %s without a value", args[i])
		}
		switch args[i] {
		case "--ro":
			ro = append(ro, args[i+1])
		case "--rw":
			rw = append(rw, args[i+1])
		case "--rofile":
			roFiles = append(roFiles, args[i+1])
		case "--rwfile":
			rwFiles = append(rwFiles, args[i+1])
		default:
			landlockDie("unknown flag %s", args[i])
		}
	}
	if i >= len(args)-1 {
		landlockDie("no command after --")
	}
	cmd := args[i+1:]
	if err := landlockRestrict(ro, rw, roFiles, rwFiles); err != nil {
		landlockDie("%v", err)
	}
	if err := unix.Exec(cmd[0], cmd, os.Environ()); err != nil {
		landlockDie("exec %s: %v", cmd[0], err)
	}
}

// landlockSelftest proves enforcement, not just availability: inside a domain
// that allows only dir, a write there succeeds and a read of forbidden fails.
func landlockSelftest(dir, forbidden string) int {
	if err := landlockRestrict(nil, []string{dir}, nil, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := os.WriteFile(dir+"/ok", []byte("ok"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write inside the allowed dir failed:", err)
		return 3
	}
	if _, err := os.ReadFile(forbidden); err == nil || !errors.Is(err, os.ErrPermission) {
		fmt.Fprintln(os.Stderr, "read outside the allowed dir was not refused:", err)
		return 4
	}
	return 0
}

func landlockDie(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "codex-bridge landlock: "+format+"\n", args...)
	os.Exit(126)
}

// landlockSupported is true on builds that can confine the child.
const landlockSupported = true
