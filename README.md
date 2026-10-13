# macbind

Plan 9-style `bind` for macOS, without root, kernel extensions or FUSE.

```sh
macbind serve &
macbind ~/work/vendor ~/src/some-lib   # ~/src/some-lib now appears at ~/work/vendor
```

macOS has no bind mounts. macbind gets the same effect by running a small
server that holds a Plan 9-style namespace, and mounting it back onto the real
filesystem over loopback NFS with the stock, unprivileged `mount_nfs`.

Binds compose like Plan 9 namespaces:

- **Union binds.** Binding a second directory at the same path stacks it on
  top of the first; the mounted directory shows both merged.
- **Nested binds.** Binding a path under an existing bind adds it to the
  namespace without another mount. Missing parent directories are
  synthesized, so a whole tree can be assembled from a single mount.

## Install

Requires Go 1.26 or newer.

```sh
go build -o macbind .
```

## Usage

```
macbind serve [--socket path] [--nfs-addr addr]
macbind <dst> <src>
macbind unbind [-f] <dst>
macbind list
```

### `macbind serve`

Runs the server in the foreground and logs each bind and unbind. Background it
yourself (`&`, tmux, launchd) if you like. Every other command talks to it and
fails with `macbind serve is not running` if it isn't.

| Flag | Default | |
|---|---|---|
| `--socket` | `~/.macbind/9p.sock` | Unix socket for the 9P server |
| `--nfs-addr` | `127.0.0.1:0` | NFS listen address (`:0` picks a free port) |

### `macbind <dst> <src>`

Makes `<src>` appear at `<dst>`. `<src>` can be a directory or a regular file.
Both paths are resolved to absolute paths. Reads and writes at `<dst>` go
straight to `<src>`.

- If nothing at or above `<dst>` is bound yet, macbind creates `<dst>` and
  mounts it.
- If `<dst>` is already bound, `<src>` is added as another union layer. For a
  name that exists in more than one layer, the most recently bound layer wins,
  and new files go to it.
- If a parent of `<dst>` is bound, `<src>` is added inside that existing
  mount. Nothing is created on disk, and no new mount is made.

A file can only be bound under an existing bind, since NFS can only mount
directories:

```sh
macbind ~/root ~/root-base
macbind ~/root/bin/pwd /bin/pwd
macbind ~/root/etc/hosts /etc/hosts
```

### `macbind unbind [-f] <dst>`

Removes every layer bound at `<dst>`, along with every bind nested under it,
and unmounts whatever mounts they had.

If a mount is busy, for example because a shell has its working directory
inside it, unmounting fails with `Resource busy`; the binds are still removed.
`-f` passes `-f` to `umount` to force it.

One edge case: if a path was bound before one of its parents, it has its own
mount, which the parent's mount then covers. Unbinding just that path removes
it from the namespace, but its mount stays until the parent is unbound.

### `macbind list`

Prints the bind table as the commands that would recreate it, one line per
layer, with paths quoted for the shell:

```console
$ macbind list
macbind /Users/me/root /Users/me/root-base
macbind /Users/me/root/bin /Users/me/bin
macbind '/Users/me/root/my docs' /Users/me/Documents
```

The bind table only lives in the server's memory, so this is also how to save
it: write the output to a file and run it with `sh` after restarting `serve`.

## Example: composing a tree

```sh
macbind ~/root ~/root-base           # mounts ~/root
macbind ~/root/bin ~/mybin           # nested: no new mount
macbind ~/root/usr/lib /usr/lib      # nested; ~/root/usr is synthesized
mount | grep -c ~/root               # 1
```

## Example: a minimal chroot

A root for `chroot` can be built entirely from binds, with nothing copied.
This needs System Integrity Protection (SIP) disabled; see
[Limitations](#limitations).

```sh
R=~/chroot

# A base layer for the root. dyld looks for the shared cache under
# /System/Cryptexes/OS, which is a symlink on a normal system.
mkdir -p ~/chroot-base/System/Cryptexes
ln -s ../../System/Volumes/Preboot/Cryptexes/OS ~/chroot-base/System/Cryptexes/OS

macbind $R ~/chroot-base                     # the only mount
macbind $R/usr/lib /usr/lib                  # dyld and system libraries
macbind $R/System/Volumes/Preboot/Cryptexes/OS /System/Volumes/Preboot/Cryptexes/OS

# A shell and a few commands, bound one file at a time
macbind $R/bin/bash /bin/bash
macbind $R/bin/ls /bin/ls
macbind $R/bin/cat /bin/cat

sudo chroot $R /bin/bash
```

Inside, `/bin` holds just those three. To add more commands, bind them in from
another terminal; they appear in the running chroot:

```sh
macbind $R/bin/mkdir /bin/mkdir
macbind $R/bin/rm /bin/rm
macbind $R/bin/pwd /bin/pwd
```

Any command whose libraries are all in `/usr/lib` will work; check with
`otool -L`. Writes inside the chroot outside a bound path land in
`~/chroot-base`. To tear it all down:

```sh
macbind unbind $R
```

## How it works

`macbind serve` holds one namespace, a
[wanix](https://github.com/tractordev/wanix) `vfs.NS`, which keeps the bind
table and does the union and nested-path resolution. Two servers expose that
same namespace:

- **9P** on the Unix socket, serving the whole namespace. The CLI uses it to
  send commands and read state through the files in `.macbind/`. macOS can't
  mount 9P, but it's there so a 9P client could mount the namespace later
  without server changes.
- **NFSv3** on loopback, built on
  [go-nfs](https://github.com/willscott/go-nfs). Each mount request gets the
  namespace subtree at the requested path, so one server handles any number of
  mounts, and binds added later show up in existing mounts immediately.

The client mounts with:

```
mount -t nfs -o vers=3,tcp,port=<p>,mountport=<p>,noresvport,soft,nolocks,actimeo=2,nonegnamecache 127.0.0.1:<dst> <dst>
```

Passing the port directly skips the portmapper, and `noresvport` lets a normal
user mount. `actimeo=2` and `nonegnamecache` keep the client's caches short;
see the delay note under Limitations.

The namespace root corresponds to the real `/`: `<dst>` is used as the
namespace path with its leading slash removed. Its layout over 9P:

```
.macbind/
  ctl      write-only: "bind <dst> <src>" or "unbind <dst>", shell-quoted
  nfs      read-only: the NFS server's host:port
  binds    read-only: the bind table, as printed by `macbind list`
Users/me/root/...   bound paths
```

### Code layout

| Path | |
|---|---|
| `main.go` | CLI entry point and subcommands |
| `internal/client` | 9P client; issues commands, runs `mount`/`umount` |
| `internal/server/server.go` | Namespace, 9P and NFS listeners, the `ctl` commands, `binds` |
| `internal/server/handler.go` | NFS handler: maps a mount request to a namespace subtree |
| `internal/server/billyfs.go` | Adapter from wanix's `fs.FS` to the `billy.Filesystem` go-nfs expects |
| `internal/shquote` | Shell quoting shared by `ctl` commands and `list` output |

## Limitations

### Chroots need SIP disabled

With System Integrity Protection (SIP) enabled, almost nothing runs inside a
chroot built with macbind:

```console
$ sudo chroot ~/chroot /bin/bash
dyld: dyld cache '(null)' not loaded: syscall to map cache into shared region failed
dyld: Library not loaded: /usr/lib/libiconv.2.dylib
```

macOS no longer ships system libraries such as `libSystem` as separate files;
they exist only inside the dyld shared cache. In a chroot, dyld looks for the
cache under the chroot's root, and with SIP enabled the kernel only maps the
genuine, SIP-protected cache file. A copy served through macbind is rejected
(the kernel log says `vnode is not SIP-protected`). A symlink doesn't help
either: inside a chroot it resolves under the new root, and the genuine file
lives on the read-only Preboot volume, which can't be hard-linked or mounted
again.

With SIP enabled, only programs that don't load any dynamic libraries can run
in a chroot.

### Apple's binaries need SIP disabled to run from a bind

With SIP enabled, programs from macOS itself (`/bin/bash`, `/bin/ls` and the
rest) are killed with `SIGKILL` when launched from anywhere but their real
location. Apple attaches launch constraints to them, and the kernel checks the
real path, so a bound copy fails even though its bytes are identical.
Re-signing a copy doesn't help: macOS won't run ad-hoc signed `arm64e`
binaries.

With SIP disabled, they run from a bind normally. With SIP enabled, your own
builds still work as long as they target `arm64` rather than `arm64e`, which is
the default for compilers outside Apple's own builds.

### Other limitations

- **No partial unbind.** `unbind` removes all layers at a path; you can't
  remove just one layer of a union.
- **No persistence.** If `serve` stops, existing mounts go stale and need
  `umount -f`. Save `macbind list` output to restore binds.
- **Files need a bind above them.** A file can only be bound under an
  existing bind, and devices, sockets and other special files can't be
  bound.
- **One user.** No authentication; the server trusts every NFS request from
  loopback and does all I/O as the user running `serve`.
- **No locking.** Mounts use `nolocks`.
- **Short delay for outside changes.** Changes made through a mount show up
  immediately. Changes made directly in a `<src>` directory, by anything not
  going through the mount, can take up to 2 seconds to appear, because the
  macOS NFS client caches attributes and directory listings. The defaults
  allow up to 60 seconds; macbind mounts with `actimeo=2` and
  `nonegnamecache` to keep it short.
- **Privacy prompts.** macOS treats NFS mounts as network volumes. A process
  without access to network volumes (for example, one started over SSH
  without "Allow full disk access for remote users") gets
  `Operation not permitted` reading through a mount.
