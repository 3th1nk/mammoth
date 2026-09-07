// Command mammoth is the single binary serving all runtime facets:
//
//	mammoth serve --mode=all|api|runner|builder|prober
//
// All facets share one configuration and one contract; modes only decide
// which components start (docs/02-architecture.md §5.1).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/3th1nk/mammoth/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := serve(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "mammoth:", err)
			os.Exit(1)
		}
	case "version":
		fmt.Printf("mammoth %s (commit %s)\n", version.Version, version.Commit)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "mammoth: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mammoth — self-contained bare-metal provisioning engine

Usage:
  mammoth serve --mode=all|api|runner|builder|prober [flags]
  mammoth version

Configuration is environment-based (MAMMOTH_* prefix); see docs/.
`)
}

// mustContext installs SIGINT/SIGTERM shutdown.
func mustContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
	}()
	return ctx
}

var _ = flag.CommandLine
