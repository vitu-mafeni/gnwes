// Command nsdbg is a minimal, statically-linked (CGO_ENABLED=0) diagnostic
// helper for poking at files and devices inside a container's mount
// namespace via nsenter, when that container's own rootfs has no shell or
// coreutils to exec (e.g. a distroless worker image, or a Kata guest's
// container rootfs).
//
// Why this exists, and why a plain host binary (ls, mknod) does not work
// here: see config/vmgpu/README.md's "nsenter into a minimal container"
// entry. In short -- nsenter --mount (without --pid) leaves /proc/self
// unresolvable inside the target's own procfs instance, so exec via
// /proc/self/fd/N fails with ENOENT; adding --pid fixes that (this
// util-linux version auto-forks on --pid, so no separate --fork flag is
// needed or even exists). Once exec succeeds, a dynamically-linked host
// binary (ls, mknod from Ubuntu) still fails with the same ENOENT, because
// the kernel resolves its ELF interpreter (/lib64/ld-linux-*.so.2) against
// the TARGET namespace's rootfs post-setns, and a distroless/minimal image
// has no such path. A statically-linked binary has no interpreter to
// resolve, which is why this is built with CGO_ENABLED=0. busybox (also
// static) gets past exec but then fails its own argv[0]-based applet
// dispatch, because nsenter always execs via a literal path
// (/proc/self/fd/N) whose basename is never a recognized applet name or
// literally "busybox" -- this tool sidesteps that by not doing any
// argv[0]-based dispatch at all; the subcommand is a normal, explicit CLI
// argument.
//
// Usage (from the host, after opening this binary as an fd so its own exec
// does not depend on anything existing in the target mount namespace):
//
//	exec 3</path/to/nsdbg
//	nsenter --target <pid> --mount --pid -- /proc/self/fd/3 ls <path>
//	nsenter --target <pid> --mount --pid -- /proc/self/fd/3 mknod <path> <c|b> <major> <minor> <mode>
//	nsenter --target <pid> --mount --pid -- /proc/self/fd/3 cat <path>
//	nsenter --target <pid> --mount --pid -- /proc/self/fd/3 dial <unix-socket-path> <vsock-port> <shell command>
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "ls":
		err = runLs(os.Args[2:])
	case "mknod":
		err = runMknod(os.Args[2:])
	case "cat":
		err = runCat(os.Args[2:])
	case "dial":
		err = runDial(os.Args[2:])
	case "install":
		err = runInstall(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nsdbg:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: nsdbg ls <path> | mknod <path> <c|b> <major> <minor> <mode> | cat <path> | dial <unix-socket-path> <vsock-port> <shell command> | install <dest-path>")
	os.Exit(2)
}

// runInstall copies this process's own running binary (via /proc/self/exe,
// which resolves correctly post-setns once --pid has been joined too) to
// dest, mode 0755. Meant to be run after nsenter --mount [--pid] into a
// container's namespace whose own rootfs the caller can write to from the
// OUTSIDE (e.g. a kata container's rootfs bind-mount source), so a plain
// absolute path to a real, statically-linked binary exists inside that
// container's own namespace afterward -- avoiding the /proc/self/fd/N
// fd-trick for every subsequent call into that specific namespace.
func runInstall(args []string) error {
	if len(args) != 1 {
		usage()
	}
	b, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("reading /proc/self/exe: %w", err)
	}
	if err := os.WriteFile(args[0], b, 0o755); err != nil {
		return fmt.Errorf("writing %q: %w", args[0], err)
	}
	return nil
}

// runDial replicates agent-substrate's DebugConsoleDump (cmd/ateworker-microvm/
// internal/kata/agentclient.go) exactly: dial the CH hybrid-vsock unix socket,
// send "CONNECT <port>\n", read the "OK <n>" line, then run cmd on the guest's
// debug console (an interactive shell on a PTY, so it echoes the command back
// before running it -- the sentinel is split by '' so only the shell's own
// OUTPUT echo of it matches, not the command-line echo).
func runDial(args []string) error {
	if len(args) != 3 {
		usage()
	}
	sockPath, portStr, cmd := args[0], args[1], args[2]
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("port %q: %w", portStr, err)
	}
	conn, err := net.DialTimeout("unix", sockPath, 8*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return fmt.Errorf("CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	if _, err := br.ReadString('\n'); err != nil { // the "OK <n>" line
		return fmt.Errorf("CONNECT reply: %w", err)
	}
	if _, err := fmt.Fprintf(conn, "{ %s ; } 2>&1; echo __ATE''_END__\n", cmd); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	const sentinel = "__ATE_END__"
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			if strings.Contains(line, sentinel) {
				break
			}
			fmt.Print(line)
		}
		if err != nil {
			break
		}
	}
	return nil
}

func runLs(args []string) error {
	if len(args) != 1 {
		usage()
	}
	entries, err := os.ReadDir(args[0])
	if err != nil {
		return err
	}
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil {
			fmt.Printf("%-30s <stat error: %v>\n", e.Name(), ierr)
			continue
		}
		fmt.Printf("%-10s %10d %s\n", info.Mode().String(), info.Size(), e.Name())
	}
	return nil
}

func runCat(args []string) error {
	if len(args) != 1 {
		usage()
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

// runMknod creates a device node, mirroring `mknod <path> <type> <major> <minor>`
// plus an explicit mode/permission argument (mknod(1) takes that via a
// separate chmod normally; this folds it in since this tool has no chmod).
func runMknod(args []string) error {
	if len(args) != 5 {
		usage()
	}
	path, typ, majorStr, minorStr, modeStr := args[0], args[1], args[2], args[3], args[4]
	major, err := strconv.ParseUint(majorStr, 10, 32)
	if err != nil {
		return fmt.Errorf("major %q: %w", majorStr, err)
	}
	minor, err := strconv.ParseUint(minorStr, 10, 32)
	if err != nil {
		return fmt.Errorf("minor %q: %w", minorStr, err)
	}
	mode, err := strconv.ParseUint(modeStr, 8, 32)
	if err != nil {
		return fmt.Errorf("mode %q (expected octal, e.g. 666): %w", modeStr, err)
	}
	var devType uint32
	switch typ {
	case "c":
		devType = syscall.S_IFCHR
	case "b":
		devType = syscall.S_IFBLK
	default:
		return fmt.Errorf("type must be c or b, got %q", typ)
	}
	dev := unixMkdev(uint32(major), uint32(minor))
	// mknod(2)'s mode is masked by the calling process's umask, same as
	// creat/open -- confirmed live (2026-11-16) the debug console's shell
	// leaves a 022 umask in place, turning a requested 0666 into 0644. An
	// explicit chmod afterward (not itself subject to umask) guarantees the
	// caller's requested bits regardless of the caller's umask.
	if err := syscall.Mknod(path, devType|uint32(mode), int(dev)); err != nil {
		return fmt.Errorf("mknod %q: %w", path, err)
	}
	if err := os.Chmod(path, os.FileMode(mode)); err != nil {
		return fmt.Errorf("chmod %q: %w", path, err)
	}
	return nil
}

// unixMkdev matches glibc's makedev() encoding, which is what the kernel's
// mknod(2) expects packed into the dev_t argument.
func unixMkdev(major, minor uint32) uint64 {
	return (uint64(major&0xfff) << 8) | uint64(minor&0xff) | (uint64(major&0xfffff000) << 32) | (uint64(minor&0xffffff00) << 12)
}
