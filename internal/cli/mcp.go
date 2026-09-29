package cli

import (
	"context"
	"log/slog"
	"os"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/3th1nk/mammoth/internal/mcp"
)

// mcpCmd mounts `mammoth mcp serve`: the provisioning surface as MCP tools
// over stdio, for AI clients (Claude Desktop, Cursor, …). Connection target is
// the same --api / --token as every other CLI command.
func mcpCmd(newClient func() *Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Model Context Protocol (MCP) surface",
	}
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Expose register/discover/install/job tools over MCP stdio",
		Long: `Run the mammoth MCP server on stdin/stdout.

AI clients speak to the same API the operator CLI uses: register machines,
drive discovery, submit declarative installs and track them to completion.
Configure your client with this binary — for example (Claude Desktop):

  "mammoth": {
    "command": "/usr/local/bin/mammoth",
    "args": ["mcp", "serve"],
    "env": { "MAMMOTH_API_URL": "http://127.0.0.1:8080",
             "MAMMOTH_API_TOKEN": "<token>" }
  }

All tools are read-only except install_submit and job_cancel; install_submit
wipes and reinstalls the targeted machines.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// stdout is the MCP transport — every log line must go to stderr
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
			server := mcp.New(newClient())
			return server.Run(context.Background(), &mcpsdk.StdioTransport{})
		},
	}
	cmd.AddCommand(serve)
	return cmd
}
