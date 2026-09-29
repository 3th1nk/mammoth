// The operator CLI's HTTP client lives in internal/apiclient (shared with the
// sandbox orchestrator and the MCP server); these aliases keep the CLI's own
// call sites unchanged.
package cli

import "github.com/3th1nk/mammoth/internal/apiclient"

type Client = apiclient.Client
type Machine = apiclient.Machine
type Job = apiclient.Job

func NewClient(base, token string) *Client { return apiclient.NewClient(base, token) }
