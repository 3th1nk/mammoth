package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/3th1nk/mammoth/internal/sandbox"
)

// sandboxCmd mounts `mammoth sandbox up|status|down` — a local, self-contained
// provisioning sandbox: postgres (docker), a throwaway mammoth server and N
// qemu guests backed by fake BMCs, alpine installed through the default
// virtual_media carrier. It brings its own server and token, so the operator
// --api/--token flags do not apply to it.
func sandboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Local sandbox: fake-BMC virtual machines installed end to end",
		Long: `Run a full provisioning sandbox on this laptop.

  mammoth sandbox up      bring up the sandbox and install alpine on every guest
  mammoth sandbox status  what is running, and how far the install got
  mammoth sandbox down    stop everything (cached ISO and logs are kept)

` + sandboxDescription(),
	}
	cmd.AddCommand(
		sandboxUpCmd(),
		sandboxStatusCmd(),
		sandboxDownCmd(),
	)
	return cmd
}

func sandboxDescription() string {
	return `The sandbox needs: docker, qemu (system + img), xorriso, curl, ssh.
No bridges, no privileged containers, no root — guests are isolated
slirp NATs and docker only supplies postgresql.`
}

func sandboxUpCmd() *cobra.Command {
	var (
		nodes int
		keep  bool
	)
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start the sandbox and run a full alpine install on every guest",
		RunE: func(cmd *cobra.Command, args []string) error {
			return sandbox.Up(context.Background(), sandbox.UpOptions{Nodes: nodes, Keep: keep})
		},
	}
	cmd.Flags().IntVar(&nodes, "nodes", sandbox.DefaultNodes, "number of sandbox machines (1-5)")
	cmd.Flags().BoolVar(&keep, "keep", false, "on failure or Ctrl-C, keep the processes and state for debugging")
	return cmd
}

func sandboxStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what the sandbox is running and how far the install got",
		RunE: func(cmd *cobra.Command, args []string) error {
			return sandbox.Status()
		},
	}
}

func sandboxDownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop the sandbox and remove its processes (assets are cached)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return sandbox.Down()
		},
	}
}
