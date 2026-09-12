package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/3th1nk/mammoth/internal/version"
)

// NewRootCommand assembles the operator CLI; serve (the server facet,
// wired by package main) is passed in. Configuration: --api and --token
// flags default from MAMMOTH_API_URL / MAMMOTH_API_TOKEN.
func NewRootCommand(serve *cobra.Command) *cobra.Command {
	var apiURL, token string

	root := &cobra.Command{
		Use:   "mammoth",
		Short: "Operator CLI for the mammoth bare-metal provisioning engine",
		Long: `Operator CLI for mammoth — a self-contained bare-metal provisioning engine.

The CLI is a thin client over the HTTP API (api/openapi.yaml is the contract):
register machines in bulk, drive discovery, submit installs, and track progress.
API access: --api / --token flags or MAMMOTH_API_URL / MAMMOTH_API_TOKEN.`,
		SilenceUsage: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&apiURL, "api", envOr("MAMMOTH_API_URL", "http://localhost:8080"), "API base URL")
	pf.StringVar(&token, "token", os.Getenv("MAMMOTH_API_TOKEN"), "API bearer token")

	newClient := func() *Client { return NewClient(apiURL, token) }

	root.AddCommand(serve, versionCmd())
	root.AddCommand(credentialsCmd(newClient))
	root.AddCommand(machinesCmd(newClient))
	root.AddCommand(installCmd(newClient))
	root.AddCommand(jobsCmd(newClient))
	root.AddCommand(eventsCmd(newClient))
	return root
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI/server build version",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.Println("mammoth CLI", version.Version)
			return nil
		},
	}
}

func credentialsCmd(newClient func() *Client) *cobra.Command {
	cmd := &cobra.Command{Use: "credentials", Short: "Manage credentials (write-only; never echoed)"}

	var name, typ, username, password string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a credential",
		RunE: func(cmd *cobra.Command, args []string) error {
			if password == "" && !isPiped() {
				fmt.Fprint(os.Stderr, "password: ")
				line, err := bufio.NewReader(os.Stdin).ReadString('\n')
				if err != nil && line == "" {
					return err
				}
				password = strings.TrimSpace(line)
			}
			var out map[string]any
			c := newClient()
			if err := c.Post("/api/v1/credentials", map[string]any{
				"type": typ, "name": name,
				"secret": map[string]string{"username": username, "password": password},
			}, &out); err != nil {
				return err
			}
			return printJSON(out)
		},
	}
	create.Flags().StringVar(&name, "name", "", "credential name (required)")
	create.Flags().StringVar(&typ, "type", "bmc", "credential type: bmc | ssh")
	create.Flags().StringVar(&username, "username", "", "secret username (required)")
	create.Flags().StringVar(&password, "password", "", "secret password (prompted if omitted)")
	_ = create.MarkFlagRequired("name")
	_ = create.MarkFlagRequired("username")

	del := &cobra.Command{
		Use:   "delete ID",
		Short: "Delete a credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return newClient().Delete("/api/v1/credentials/" + args[0])
		},
	}
	cmd.AddCommand(create, del)
	return cmd
}

func machinesCmd(newClient func() *Client) *cobra.Command {
	cmd := &cobra.Command{Use: "machines", Short: "Register and inspect machines"}

	// register: one machine per --bmc-address repetition (批量注册)
	var bmcAddrs []string
	var protocol, credential, sshCredential, sshAddress string
	var labels []string
	register := &cobra.Command{
		Use:   "register",
		Short: "Register one or more machines by out-of-band address",
		RunE: func(cmd *cobra.Command, args []string) error {
			labelMap, err := parseLabels(labels)
			if err != nil {
				return err
			}
			c := newClient()
			for _, addr := range bmcAddrs {
				body := map[string]any{
					"bmc": map[string]any{
						"address": addr, "protocol": protocol, "credential_id": credential,
					},
				}
				if len(labelMap) > 0 {
					body["labels"] = labelMap
				}
				if sshCredential != "" {
					body["ssh_credential_id"] = sshCredential
				}
				if sshAddress != "" {
					body["ssh"] = map[string]any{"address": sshAddress}
				}
				var out map[string]any
				if err := c.Post("/api/v1/machines", body, &out); err != nil {
					return fmt.Errorf("register %s: %w", addr, err)
				}
				fmt.Printf("registered %s -> %s\n", addr, out["id"])
			}
			return nil
		},
	}
	register.Flags().StringArrayVar(&bmcAddrs, "bmc-address", nil, "out-of-band address (repeatable)")
	register.Flags().StringVar(&protocol, "protocol", "auto", "bmc protocol: redfish | ipmi | auto | fake")
	register.Flags().StringVar(&credential, "credential", "", "bmc credential id (required)")
	register.Flags().StringVar(&sshCredential, "ssh-credential", "", "in-band ssh credential id (optional)")
	register.Flags().StringVar(&sshAddress, "ssh-address", "", "in-band address (optional; required for partition-level discovery)")
	register.Flags().StringArrayVar(&labels, "label", nil, "label k=v (repeatable)")
	_ = register.MarkFlagRequired("bmc-address")
	_ = register.MarkFlagRequired("credential")

	list := &cobra.Command{
		Use:   "list",
		Short: "List machines",
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				Items []Machine `json:"items"`
			}
			if err := newClient().Get("/api/v1/machines?page_size=200", &out); err != nil {
				return err
			}
			for _, m := range out.Items {
				fmt.Printf("%s  %-18s state=%-12s power=%s\n", m.ID, m.BMC.Address, m.State, m.PowerState)
			}
			return nil
		},
	}

	var probe string
	discover := &cobra.Command{
		Use:   "discover ID",
		Short: "Trigger (re)discovery on a machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"type": "discover"}
			if probe != "" {
				body["probe"] = probe
			}
			var out map[string]any
			if err := newClient().Post("/api/v1/machines/"+args[0]+"/actions", body, &out); err != nil {
				return err
			}
			return printJSON(map[string]any{"job": out["id"], "state": out["state"]})
		},
	}
	discover.Flags().StringVar(&probe, "probe", "auto", "probe: auto | redfish | inband_ssh")

	del := &cobra.Command{
		Use:   "delete ID",
		Short: "Deregister a machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return newClient().Delete("/api/v1/machines/" + args[0])
		},
	}

	cmd.AddCommand(register, list, discover, del)
	return cmd
}

func installCmd(newClient func() *Client) *cobra.Command {
	cmd := &cobra.Command{Use: "install", Short: "Submit and track install jobs"}

	var specFile string
	var machineIDs []string
	var watch bool
	submit := &cobra.Command{
		Use:   "submit",
		Short: "Submit an install job from a spec file (YAML or JSON)",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(specFile)
			if err != nil {
				return err
			}
			spec, err := specToJSON(raw)
			if err != nil {
				return fmt.Errorf("spec %s: %w", specFile, err)
			}
			var out map[string]any
			c := newClient()
			if err := c.Post("/api/v1/jobs", map[string]any{
				"type":    "install",
				"targets": map[string]any{"machine_ids": machineIDs},
				"spec":    json.RawMessage(spec),
			}, &out); err != nil {
				return err
			}
			jobID, _ := out["id"].(string)
			fmt.Printf("job %s submitted (%d machines)\n", jobID, len(machineIDs))
			if watch {
				return watchJob(c, jobID)
			}
			return nil
		},
	}
	submit.Flags().StringVar(&specFile, "spec", "", "install spec file (required)")
	submit.Flags().StringSliceVar(&machineIDs, "machine", nil, "target machine id (repeatable)")
	submit.Flags().BoolVar(&watch, "watch", false, "stream task progress until terminal")
	_ = submit.MarkFlagRequired("spec")
	_ = submit.MarkFlagRequired("machine")

	cmd.AddCommand(submit)
	return cmd
}

func jobsCmd(newClient func() *Client) *cobra.Command {
	cmd := &cobra.Command{Use: "jobs", Short: "Inspect and control jobs"}

	list := &cobra.Command{
		Use:   "list",
		Short: "List jobs",
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct {
				Items []Job `json:"items"`
			}
			if err := newClient().Get("/api/v1/jobs?page_size=200", &out); err != nil {
				return err
			}
			for _, j := range out.Items {
				fmt.Printf("%s  %-9s %s\n", j.ID, j.Type, j.State)
			}
			return nil
		},
	}
	get := &cobra.Command{
		Use:   "get ID",
		Short: "Show a job with task progress",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var job map[string]any
			if err := newClient().Get("/api/v1/jobs/"+args[0], &job); err != nil {
				return err
			}
			var tasks struct {
				Items []map[string]any `json:"items"`
			}
			_ = newClient().Get("/api/v1/jobs/"+args[0]+"/tasks?page_size=200", &tasks)
			for _, t := range tasks.Items {
				stages := []string{}
				if st, ok := t["stages"].([]any); ok {
					for _, s := range st {
						sm := s.(map[string]any)
						stages = append(stages, fmt.Sprintf("%s:%v", sm["name"], sm["state"]))
					}
				}
				fmt.Printf("  %s machine=%v state=%v attempt=%v\n    %s\n",
					t["id"], t["machine_id"], t["state"], t["attempt"], strings.Join(stages, " → "))
			}
			return printJSON(job)
		},
	}
	cancel := &cobra.Command{
		Use:   "cancel ID",
		Short: "Cancel a job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out map[string]any
			return newClient().Post("/api/v1/jobs/"+args[0]+"/cancel", nil, &out)
		},
	}
	var taskID string
	retry := &cobra.Command{
		Use:   "retry JOB_ID",
		Short: "Retry a failed/interrupted task (--task)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if taskID == "" {
				return fmt.Errorf("--task is required")
			}
			var out map[string]any
			return newClient().Post(fmt.Sprintf("/api/v1/jobs/%s/tasks/%s/retry", args[0], taskID), nil, &out)
		},
	}
	retry.Flags().StringVar(&taskID, "task", "", "task id to retry")

	var logTaskID string
	logs := &cobra.Command{
		Use:   "logs JOB_ID",
		Short: "Show a task's execution logs (--task)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if logTaskID == "" {
				return fmt.Errorf("--task is required")
			}
			var out struct {
				Items []map[string]any `json:"items"`
			}
			path := fmt.Sprintf("/api/v1/jobs/%s/tasks/%s/logs?page_size=200", args[0], logTaskID)
			if err := newClient().Get(path, &out); err != nil {
				return err
			}
			for _, l := range out.Items {
				stage := ""
				if s, ok := l["stage"].(string); ok && s != "" {
					stage = " [" + s + "]"
				}
				fmt.Printf("%s  %-5s%s %v\n", l["ts"], l["level"], stage, l["message"])
			}
			return nil
		},
	}
	logs.Flags().StringVar(&logTaskID, "task", "", "task id to show logs for")

	var watchID string
	watch := &cobra.Command{
		Use:   "watch ID",
		Short: "Stream task progress until the job is terminal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return watchJob(newClient(), args[0])
		},
	}
	_ = watchID

	cmd.AddCommand(list, get, cancel, retry, logs, watch)
	return cmd
}

func eventsCmd(newClient func() *Client) *cobra.Command {
	cmd := &cobra.Command{Use: "events", Short: "Query or stream events"}

	var resource string
	list := &cobra.Command{
		Use:   "list",
		Short: "List events (audit backlog)",
		RunE: func(cmd *cobra.Command, args []string) error {
			var out map[string]any
			if err := newClient().Get("/api/v1/events?page_size=100", &out); err != nil {
				return err
			}
			return printJSON(out)
		},
	}
	list.Flags().StringVar(&resource, "resource", "", "filter by resource id")

	var jobID string
	stream := &cobra.Command{
		Use:   "stream",
		Short: "Stream events (SSE)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			path := "/api/v1/events/stream"
			if jobID != "" {
				path = "/api/v1/jobs/" + jobID + "/events"
			}
			req, err := streamRequest(c, path)
			if err != nil {
				return err
			}
			return streamPrint(c, req)
		},
	}
	stream.Flags().StringVar(&jobID, "job", "", "stream one job's events")

	cmd.AddCommand(list, stream)
	return cmd
}

// ── helpers ─────────────────────────────────────────────────────────────────

func parseLabels(labels []string) (map[string]string, error) {
	out := map[string]string{}
	for _, l := range labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return nil, fmt.Errorf("label %q must be k=v", l)
		}
		out[k] = v
	}
	return out, nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func isPiped() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) == 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
