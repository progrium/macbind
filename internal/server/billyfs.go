package server

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"

	"tractor.dev/wanix/fs"
)

// billyFS adapts a tractor.dev/wanix/fs.FS to the billy.Filesystem
// interface required by willscott/go-nfs. It is a thin translation layer:
// every method delegates to the matching tractor.dev/wanix/fs package-level
// helper (fs.Create, fs.OpenFile, fs.Mkdir, ...), which resolves through the
// wanix vfs bind table — including any stacked/union binds — down to the
// real underlying filesystem. No generic fs.FS-to-billy adapter exists
// upstream; this is net-new.
type billyFS struct {
	fsys fs.FS
	root string
}

var (
	_ billy.Filesystem = (*billyFS)(nil)
	_ billy.Change     = (*billyFS)(nil)
)

func newBillyFS(fsys fs.FS) *billyFS {
	return &billyFS{fsys: fsys, root: "/"}
}

// cleanName normalizes a billy path (which may be relative, absolute, or
// use backslashes on exotic clients) into the "."-rooted relative form
// tractor.dev/wanix/fs expects.
func cleanName(name string) string {
	name = path.Clean("/" + strings.ReplaceAll(name, "\\", "/"))
	rel := strings.TrimPrefix(name, "/")
	if rel == "" {
		rel = "."
	}
	return rel
}

func (b *billyFS) Create(filename string) (billy.File, error) {
	f, err := fs.Create(b.fsys, cleanName(filename))
	if err != nil {
		return nil, err
	}
	return newBillyFile(f, filename), nil
}

func (b *billyFS) Open(filename string) (billy.File, error) {
	f, err := fs.OpenContext(context.Background(), b.fsys, cleanName(filename))
	if err != nil {
		return nil, err
	}
	return newBillyFile(f, filename), nil
}

func (b *billyFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	f, err := fs.OpenFile(b.fsys, cleanName(filename), flag, fs.FileMode(perm))
	if err != nil {
		return nil, err
	}
	return newBillyFile(f, filename), nil
}

func (b *billyFS) Stat(filename string) (os.FileInfo, error) {
	return b.statOrOpen(filename, fs.Stat)
}

func (b *billyFS) Lstat(filename string) (os.FileInfo, error) {
	return b.statOrOpen(filename, fs.Lstat)
}

// statOrOpen falls back to opening the path when stat reports it missing.
// The vfs synthesizes parent directories of nested binds (e.g. "usr" when
// only "usr/lib" is bound under a bound root) in Open, but stat routes to
// the enclosing bind's source, which doesn't have them.
func (b *billyFS) statOrOpen(filename string, stat func(fs.FS, string) (fs.FileInfo, error)) (os.FileInfo, error) {
	name := cleanName(filename)
	fi, err := stat(b.fsys, name)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return fi, err
	}
	f, oerr := fs.OpenContext(context.Background(), b.fsys, name)
	if oerr != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

func (b *billyFS) Rename(oldpath, newpath string) error {
	return fs.Rename(b.fsys, cleanName(oldpath), cleanName(newpath))
}

func (b *billyFS) Remove(filename string) error {
	return fs.Remove(b.fsys, cleanName(filename))
}

func (b *billyFS) Join(elem ...string) string {
	return path.Join(elem...)
}

func (b *billyFS) TempFile(dir, prefix string) (billy.File, error) {
	base := cleanName(dir)
	for range 10000 {
		name := fmt.Sprintf("%s%d", prefix, rand.Int63())
		full := path.Join(base, name)
		f, err := fs.OpenFile(b.fsys, full, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return newBillyFile(f, "/"+full), nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("tempfile: could not create unique file in %s", dir)
}

func (b *billyFS) ReadDir(dir string) ([]os.FileInfo, error) {
	entries, err := fs.ReadDirContext(context.Background(), b.fsys, cleanName(dir))
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		infos = append(infos, fi)
	}
	return infos, nil
}

func (b *billyFS) MkdirAll(filename string, perm os.FileMode) error {
	return fs.MkdirAll(b.fsys, cleanName(filename), fs.FileMode(perm))
}

func (b *billyFS) Symlink(target, link string) error {
	return fs.Symlink(b.fsys, target, cleanName(link))
}

func (b *billyFS) Readlink(link string) (string, error) {
	return fs.Readlink(b.fsys, cleanName(link))
}

func (b *billyFS) Chroot(dir string) (billy.Filesystem, error) {
	sub, err := fs.Sub(b.fsys, cleanName(dir))
	if err != nil {
		return nil, err
	}
	return &billyFS{fsys: sub, root: path.Join(b.root, dir)}, nil
}

func (b *billyFS) Root() string {
	return b.root
}

// billy.Change

func (b *billyFS) Chmod(name string, mode os.FileMode) error {
	return fs.Chmod(b.fsys, cleanName(name), fs.FileMode(mode))
}

func (b *billyFS) Chown(name string, uid, gid int) error {
	return fs.Chown(b.fsys, cleanName(name), uid, gid)
}

// Lchown has no distinct wanix fs helper (no separate "don't follow
// symlinks" chown); approximated by Chown, which is fine for the common
// case of non-symlink targets.
func (b *billyFS) Lchown(name string, uid, gid int) error {
	return b.Chown(name, uid, gid)
}

func (b *billyFS) Chtimes(name string, atime, mtime time.Time) error {
	return fs.Chtimes(b.fsys, cleanName(name), atime, mtime)
}

// billyFile adapts a tractor.dev/wanix/fs.File to billy.File.
type billyFile struct {
	f    fs.File
	name string
}

var _ billy.File = (*billyFile)(nil)

func newBillyFile(f fs.File, name string) *billyFile {
	return &billyFile{f: f, name: name}
}

func (f *billyFile) Name() string { return f.name }

func (f *billyFile) Write(p []byte) (int, error) {
	return fs.Write(f.f, p)
}

func (f *billyFile) Read(p []byte) (int, error) {
	return f.f.Read(p)
}

func (f *billyFile) ReadAt(p []byte, off int64) (int, error) {
	return fs.ReadAt(f.f, p, off)
}

func (f *billyFile) Seek(offset int64, whence int) (int64, error) {
	return fs.Seek(f.f, offset, whence)
}

func (f *billyFile) Close() error {
	return f.f.Close()
}

// Lock/Unlock are no-ops: NFS mounts in this prototype are expected to use
// the "nolocks" mount option rather than relying on server-side locking.
func (f *billyFile) Lock() error   { return nil }
func (f *billyFile) Unlock() error { return nil }

func (f *billyFile) Truncate(size int64) error {
	if t, ok := f.f.(interface{ Truncate(int64) error }); ok {
		return t.Truncate(size)
	}
	return fmt.Errorf("truncate: %w", fs.ErrNotSupported)
}
