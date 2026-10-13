// Package server implements macbind's serve process: a single shared
// *vfs.NS namespace that holds the bind table, exposed over both a 9P
// control/data server (the full vfs) and a mirror NFSv3 server (used as the
// actual real-fs mount mechanism since macOS has no native 9P client).
package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/hugelgupf/p9/p9"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"

	"macbind/internal/shquote"

	"tractor.dev/toolkit-go/engine/cli"
	"tractor.dev/wanix/fs/fskit"
	"tractor.dev/wanix/fs/localfs"
	"tractor.dev/wanix/fs/p9kit"
	"tractor.dev/wanix/fs/vfs"
	"tractor.dev/wanix/misc"
)

// Server runs macbind's 9P and NFS listeners over one shared vfs namespace.
type Server struct {
	socketPath string
	nfsAddr    string

	ns *vfs.NS

	mu    sync.Mutex
	binds map[string][]layer // vfs path -> stacked bind layers, in bind order
}

type layer struct {
	src     string
	fs      *localfs.FS
	srcPath string // path within fs that is bound: "." for a directory, the name for a file
}

// New creates a Server. socketPath is the unix socket for the 9P server;
// nfsAddr is the listen address for the NFS server (use "host:0" for an
// OS-assigned port).
func New(socketPath, nfsAddr string) *Server {
	return &Server{
		socketPath: socketPath,
		nfsAddr:    nfsAddr,
		binds:      make(map[string][]layer),
	}
}

// Run starts the 9P and NFS listeners and blocks until ctx is canceled or
// either listener fails.
func (s *Server) Run(ctx context.Context) error {
	nfsListener, err := net.Listen("tcp", s.nfsAddr)
	if err != nil {
		return fmt.Errorf("nfs listen: %w", err)
	}
	nfsActualAddr := nfsListener.Addr().String()

	s.ns = vfs.New(ctx)
	ctlfs := fskit.MapFS{
		"ctl": misc.ControlFile(s.ctlCommand()),
		"nfs": misc.FieldFile(func() (string, error) {
			return nfsActualAddr, nil
		}),
		"binds": misc.FieldFile(func() (string, error) {
			return s.listBinds(), nil
		}),
	}
	if err := s.ns.Bind(ctlfs, ".", ".macbind"); err != nil {
		nfsListener.Close()
		return fmt.Errorf("bind .macbind: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o700); err != nil {
		nfsListener.Close()
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	os.Remove(s.socketPath) // clear a stale socket left by a previous crashed run

	p9Listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		nfsListener.Close()
		return fmt.Errorf("9p listen: %w", err)
	}

	p9srv := p9.NewServer(p9kit.Attacher(s.ns))
	nfsHandler := nfshelper.NewCachingHandler(newNFSHandler(s.ns), 1_000_000)

	log.Printf("9p control socket: %s", s.socketPath)
	log.Printf("nfs server: %s", nfsActualAddr)

	errc := make(chan error, 2)
	go func() { errc <- p9srv.Serve(p9Listener) }()
	go func() { errc <- nfs.Serve(nfsListener, nfsHandler) }()

	select {
	case <-ctx.Done():
		p9Listener.Close()
		nfsListener.Close()
		return ctx.Err()
	case err := <-errc:
		p9Listener.Close()
		nfsListener.Close()
		return err
	}
}

// ctlCommand builds the command tree served by the .macbind/ctl control
// file: `bind <dst> <src>` and `unbind <dst>`.
func (s *Server) ctlCommand() *cli.Command {
	root := &cli.Command{Usage: "ctl"}
	root.AddCommand(&cli.Command{
		Usage: "bind <dst> <src>",
		Short: "bind the real directory or file <src> at vfs path <dst>",
		Args:  cli.ExactArgs(2),
		Run: func(ctx *cli.Context, args []string) {
			if err := s.bind(args[0], args[1]); err != nil {
				log.Println("bind:", err)
			}
		},
	})
	root.AddCommand(&cli.Command{
		Usage: "unbind <dst>",
		Short: "remove all bind layers at vfs path <dst> and below",
		Args:  cli.ExactArgs(1),
		Run: func(ctx *cli.Context, args []string) {
			if err := s.unbind(args[0]); err != nil {
				log.Println("unbind:", err)
			}
		},
	})
	return root
}

func (s *Server) bind(dstPath, srcPath string) error {
	rel, err := toVFSPath(dstPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", srcPath, err)
	}

	// A directory is bound as the root of its own localfs; a file as a
	// name within a localfs of its parent directory.
	fsDir, fsPath := srcPath, "."
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("not a directory or regular file: %s", srcPath)
		}
		fsDir, fsPath = filepath.Dir(srcPath), filepath.Base(srcPath)
	}
	srcfs, err := localfs.New(fsDir)
	if err != nil {
		return fmt.Errorf("localfs %s: %w", fsDir, err)
	}

	// Union-binds: a second bind at the same <dst> stacks as another layer
	// rather than replacing or erroring, matching wanix vfs semantics.
	if err := s.ns.Bind(srcfs, fsPath, rel); err != nil {
		return fmt.Errorf("bind: %w", err)
	}

	s.mu.Lock()
	s.binds[rel] = append(s.binds[rel], layer{src: srcPath, fs: srcfs, srcPath: fsPath})
	s.mu.Unlock()

	log.Printf("bind %s %s", dstPath, srcPath)
	return nil
}

func (s *Server) unbind(dstPath string) error {
	rel, err := toVFSPath(dstPath)
	if err != nil {
		return err
	}

	// Take dst and every bind nested under it.
	s.mu.Lock()
	removed := make(map[string][]layer)
	for p, layers := range s.binds {
		if p == rel || rel == "." || strings.HasPrefix(p, rel+"/") {
			removed[p] = layers
			delete(s.binds, p)
		}
	}
	s.mu.Unlock()

	if len(removed) == 0 {
		return fmt.Errorf("not bound: %s", dstPath)
	}

	var firstErr error
	for p, layers := range removed {
		for _, l := range layers {
			if err := s.ns.Unbind(l.fs, l.srcPath, p); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		log.Printf("unbind %s", "/"+strings.TrimPrefix(p, "."))
	}
	return firstErr
}

// listBinds renders the bind table as the macbind commands that would
// reproduce it, one per layer, sorted by dst and in bind order within a dst.
func (s *Server) listBinds() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	dsts := make([]string, 0, len(s.binds))
	for rel := range s.binds {
		dsts = append(dsts, rel)
	}
	sort.Strings(dsts)

	var b strings.Builder
	for _, rel := range dsts {
		for _, l := range s.binds[rel] {
			fmt.Fprintf(&b, "macbind %s %s\n", shquote.Quote("/"+rel), shquote.Quote(l.src))
		}
	}
	return b.String()
}
