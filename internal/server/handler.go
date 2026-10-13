package server

import (
	"context"
	"fmt"
	"net"

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/vfs"
)

// nfsHandler implements nfs.Handler over the shared vfs. Mount resolves the
// requested dirpath to a view of the vfs (via fs.Sub, which routes through
// ns's union-aware Open/Route) and wraps it as a billy.Filesystem — the
// same thing a 9P client does with attach+walk, just for a different wire
// protocol over the same namespace.
type nfsHandler struct {
	ns *vfs.NS
}

func newNFSHandler(ns *vfs.NS) nfs.Handler {
	return &nfsHandler{ns: ns}
}

func (h *nfsHandler) Mount(ctx context.Context, conn net.Conn, req nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	// go-nfs's onMount calls userHandle.ToHandle(handle, ...) unconditionally,
	// even when status != MountStatusOk, so handle/billy.Filesystem must
	// never be a literal nil here (helpers.CachingHandler.ToHandle derefs it
	// immediately and would crash the whole server for every mount, not just
	// the failing one). Fall back to wrapping the root vfs on any error path;
	// it's discarded by the client since status is not Ok.
	fallback := newBillyFS(h.ns)

	rel, err := toVFSPath(string(req.Dirpath))
	if err != nil {
		return nfs.MountStatusErrInval, fallback, nil
	}

	subfs, err := fs.Sub(h.ns, rel)
	if err != nil {
		return nfs.MountStatusErrNoEnt, fallback, nil
	}
	if info, err := fs.Stat(subfs, "."); err != nil || !info.IsDir() {
		return nfs.MountStatusErrNoEnt, fallback, nil
	}

	return nfs.MountStatusOk, newBillyFS(subfs), []nfs.AuthFlavor{nfs.AuthFlavorNull}
}

// Change can return nil to mark a filesystem read-only; this adapter
// supports it, so forward it through when available.
func (h *nfsHandler) Change(f billy.Filesystem) billy.Change {
	if c, ok := f.(billy.Change); ok {
		return c
	}
	return nil
}

// FSStat reports fixed, generous stats: real usage isn't tracked per-bind,
// and NFS clients mostly just want non-zero/non-full values here.
func (h *nfsHandler) FSStat(ctx context.Context, f billy.Filesystem, s *nfs.FSStat) error {
	const totalSize = 1 << 40
	const totalFiles = 1 << 20
	s.TotalSize, s.FreeSize, s.AvailableSize = totalSize, totalSize, totalSize
	s.TotalFiles, s.FreeFiles, s.AvailableFiles = totalFiles, totalFiles, totalFiles
	return nil
}

// ToHandle/FromHandle/InvalidateHandle/HandleLimit are stubbed out here
// because the server wraps this handler with
// willscott/go-nfs/helpers.NewCachingHandler, which supplies real
// path-keyed handle caching (same pattern as helpers.NullAuthHandler).
func (h *nfsHandler) ToHandle(f billy.Filesystem, path []string) []byte {
	return []byte{}
}

func (h *nfsHandler) FromHandle(fh []byte) (billy.Filesystem, []string, error) {
	return nil, nil, fmt.Errorf("handle not found")
}

func (h *nfsHandler) InvalidateHandle(billy.Filesystem, []byte) error {
	return nil
}

func (h *nfsHandler) HandleLimit() int {
	return -1
}
