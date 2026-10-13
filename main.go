// Command macbind emulates Plan 9-style bind on macOS using a 9P control
// server and a loopback NFSv3 server, both wrapping a single shared wanix
// vfs namespace.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"macbind/internal/client"
	"macbind/internal/server"
)

const defaultNFSAddr = "127.0.0.1:0"

func main() {
	log.SetFlags(0)
	log.SetPrefix("macbind: ")

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "unbind":
		err = runUnbind(os.Args[2:])
	case "list":
		err = runList(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = runBind(os.Args[1:])
	}

	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  macbind serve [--socket path] [--nfs-addr addr]
  macbind <dst> <src>
  macbind unbind [-f] <dst>
  macbind list
`)
}

func defaultSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".macbind", "9p.sock")
}

func runServe(args []string) error {
	fset := flag.NewFlagSet("serve", flag.ExitOnError)
	socket := fset.String("socket", defaultSocketPath(), "unix socket path for the 9p control server")
	nfsAddr := fset.String("nfs-addr", defaultNFSAddr, "listen address for the nfs server")
	if err := fset.Parse(args); err != nil {
		return err
	}

	srv := server.New(*socket, *nfsAddr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func runBind(args []string) error {
	if len(args) != 2 {
		usage()
		return errors.New("expected <dst> <src>")
	}
	return client.Bind(defaultSocketPath(), args[0], args[1])
}

func runUnbind(args []string) error {
	force := false
	var dsts []string
	for _, a := range args {
		if a == "-f" {
			force = true
		} else {
			dsts = append(dsts, a)
		}
	}
	if len(dsts) != 1 {
		usage()
		return errors.New("unbind: expected [-f] <dst>")
	}
	return client.Unbind(defaultSocketPath(), dsts[0], force)
}

func runList(args []string) error {
	if len(args) != 0 {
		usage()
		return errors.New("list: takes no arguments")
	}
	out, err := client.List(defaultSocketPath())
	if err != nil {
		return err
	}
	if out != "" {
		fmt.Println(out)
	}
	return nil
}
