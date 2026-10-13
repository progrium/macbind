// Package client implements the macbind CLI's 9P client side: writing
// bind/unbind commands to the server's .macbind/ctl file, reading the NFS
// address from .macbind/nfs, and shelling out to mount/umount.
package client

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"macbind/internal/shquote"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/p9kit"
	"tractor.dev/wanix/misc/shlex"
)

func dial(socket string) (fs.FS, net.Conn, error) {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil, nil, fmt.Errorf("macbind serve is not running (socket %s): %w", socket, err)
	}
	cfs, err := p9kit.ClientFS(conn, "")
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("9p attach: %w", err)
	}
	return cfs, conn, nil
}

func ctl(socket, command string) error {
	cfs, conn, err := dial(socket)
	if err != nil {
		return err
	}
	defer conn.Close()

	f, err := fs.OpenFile(cfs, ".macbind/ctl", os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open .macbind/ctl: %w", err)
	}
	if _, err := fs.Write(f, []byte(command)); err != nil {
		f.Close()
		return fmt.Errorf("write .macbind/ctl: %w", err)
	}
	return f.Close()
}

func readFile(socket, name string) (string, error) {
	cfs, conn, err := dial(socket)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	f, err := cfs.Open(name)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// List returns the server's bind table as the macbind commands that would
// reproduce it, one per line.
func List(socket string) (string, error) {
	return readFile(socket, ".macbind/binds")
}

// Bind resolves dst/src to absolute paths and tells the server to bind them
// in its vfs. src may be a directory or a file. If dst is not already
// covered by an existing bind, it also creates dst on the real fs and mounts
// the server's NFS export of dst there; that's only possible for a
// directory.
func Bind(socket, dstPath, srcPath string) error {
	dstAbs, err := filepath.Abs(dstPath)
	if err != nil {
		return err
	}
	srcAbs, err := filepath.Abs(srcPath)
	if err != nil {
		return err
	}

	dsts, err := boundDsts(socket)
	if err != nil {
		return err
	}

	// NFS can only mount directories, so a file can only be bound where an
	// existing mount will serve it: at or under an already-bound path.
	srcInfo, err := os.Stat(srcAbs)
	if err != nil {
		return err
	}
	if !srcInfo.IsDir() && !srcInfo.Mode().IsRegular() {
		return fmt.Errorf("can't bind %s: not a directory or regular file", srcAbs)
	}
	if !srcInfo.IsDir() && !bindCovers(dsts, dstAbs) {
		return fmt.Errorf("can't bind file %s at %s: a file can only be bound under an existing bind", srcAbs, dstAbs)
	}

	if err := ctl(socket, "bind "+shquote.Quote(dstAbs)+" "+shquote.Quote(srcAbs)); err != nil {
		return err
	}

	// If dst or one of its ancestors is already bound, there's already an
	// NFS mount serving it: the server resolves the whole bind table under a
	// mount, so a stacked or nested bind shows up there without another
	// mount. Creating dst on the real fs would also write through that mount
	// into the layer below.
	if bindCovers(dsts, dstAbs) {
		return nil
	}

	addr, err := readFile(socket, ".macbind/nfs")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dstAbs, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dstAbs, err)
	}
	return mountNFS(addr, dstAbs)
}

// Unbind resolves dst to an absolute path and tells the server to remove
// every bind layer at dst and every bind nested under it. Then it unmounts
// the macbind mounts at or under dst. A nested bind usually has no mount of
// its own, but it does if it was bound before its parent. force passes -f to
// umount.
func Unbind(socket, dstPath string, force bool) error {
	dstAbs, err := filepath.Abs(dstPath)
	if err != nil {
		return err
	}

	dsts, err := boundDsts(socket)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(dsts, func(d string) bool { return isUnder(d, dstAbs) }) {
		return fmt.Errorf("not bound: %s", dstAbs)
	}

	mounts, err := mountsToUnmount(dstAbs)
	if err != nil {
		return err
	}

	if err := ctl(socket, "unbind "+shquote.Quote(dstAbs)); err != nil {
		return err
	}

	var firstErr error
	for _, m := range mounts {
		if err := umount(m, force); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// isUnder reports whether path is dir or inside it.
func isUnder(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

// mountsToUnmount returns the macbind NFS mounts at or under dir, most
// recently mounted first, which unmounts children before parents whichever
// order they were bound in. It leaves out a mount that a later mount above
// dir covers (a child bound before its parent): it can't be reached to
// unmount, and is unmounted when the covering bind is unbound.
func mountsToUnmount(dir string) ([]string, error) {
	all, err := nfsMounts()
	if err != nil {
		return nil, err
	}
	var mounts []string
	for i, m := range all {
		if !isUnder(m, dir) {
			continue
		}
		covered := slices.ContainsFunc(all[i+1:], func(later string) bool {
			return later != m && isUnder(m, later) && !isUnder(later, dir)
		})
		if !covered {
			mounts = append(mounts, m)
		}
	}
	slices.Reverse(mounts)
	return mounts, nil
}

// nfsMounts returns the mount points of macbind NFS mounts in mount order.
// macbind exports each dst at its own path, so its mounts look like
// "<host>:<dst> on <dst>".
func nfsMounts() ([]string, error) {
	n, err := syscall.Getfsstat(nil, 1 /* MNT_WAIT */)
	if err != nil {
		return nil, fmt.Errorf("getfsstat: %w", err)
	}
	stats := make([]syscall.Statfs_t, n)
	if n, err = syscall.Getfsstat(stats, 1); err != nil {
		return nil, fmt.Errorf("getfsstat: %w", err)
	}
	var mounts []string
	for _, st := range stats[:n] {
		on := cString(st.Mntonname[:])
		if cString(st.Fstypename[:]) == "nfs" && strings.HasSuffix(cString(st.Mntfromname[:]), ":"+on) {
			mounts = append(mounts, on)
		}
	}
	return mounts, nil
}

func cString(b []int8) string {
	buf := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		buf = append(buf, byte(c))
	}
	return string(buf)
}

// boundDsts returns the dst of every entry in the server's bind table.
func boundDsts(socket string) ([]string, error) {
	out, err := List(socket)
	if err != nil {
		return nil, err
	}
	var dsts []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		args, err := shlex.Split(line, true)
		if err != nil || len(args) != 3 {
			return nil, fmt.Errorf("parse bind table line %q", line)
		}
		dsts = append(dsts, args[1])
	}
	return dsts, nil
}

// bindCovers reports whether dst or one of its ancestors is in dsts.
func bindCovers(dsts []string, dst string) bool {
	return slices.ContainsFunc(dsts, func(d string) bool { return isUnder(dst, d) })
}

func mountNFS(addr, target string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad nfs address %q: %w", addr, err)
	}

	opts := fmt.Sprintf("vers=3,tcp,port=%s,mountport=%s,noresvport,soft,nolocks,actimeo=2,nonegnamecache", port, port)
	cmd := exec.Command("mount", "-t", "nfs", "-o", opts, fmt.Sprintf("%s:%s", host, target), target)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	return nil
}

func umount(target string, force bool) error {
	args := []string{target}
	if force {
		args = []string{"-f", target}
	}
	cmd := exec.Command("umount", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("umount: %w", err)
	}
	return nil
}
