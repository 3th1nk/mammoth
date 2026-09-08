// Command mammoth is the single binary serving all runtime facets and the
// operator CLI:
//
//	mammoth serve --mode=all|api|runner|builder|prober
//	mammoth machines register ...
//	mammoth install submit --spec ...
//
// All facets share one configuration and one contract; modes only decide
// which components start (docs/02-architecture.md §5.1).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/3th1nk/mammoth/internal/cli"
	"github.com/3th1nk/mammoth/internal/version"
)

func main() {
	root := cli.NewRootCommand(serveCobraCommand())
	root.Version = version.Version
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// serveCobraCommand wraps the serve facet as a cobra subcommand; flags after
// `serve` pass through to the serve flag set (e.g. --mode).
func serveCobraCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the server (all facets or a single one)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return serve(args)
		},
		DisableFlagParsing: true, // --mode etc. parsed by the serve flag set
	}
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
