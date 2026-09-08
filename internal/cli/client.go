// Package cli implements the mammoth operator CLI. It is a thin HTTP client
// over the API contract — the CLI adds zero server-side semantics, it only
// batches and formats (docs/09-roadmap.md M6: 批量注册、盘查、安装提交、进度跟踪).
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to a mammoth API.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// NewClient builds a client from the --api / --token values.
func NewClient(base, token string) *Client {
	return &Client{
		Base:  strings.TrimSuffix(base, "/"),
		Token: token,
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		// problem+json: surface the machine-readable code and detail
		var p struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
			Title  string `json:"title"`
		}
		if json.Unmarshal(payload, &p) == nil && p.Code != "" {
			return fmt.Errorf("%s: %s (%s)", resp.Status, p.Detail, p.Code)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	if out != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) Get(path string, out any) error { return c.do(http.MethodGet, path, nil, out) }
func (c *Client) Post(path string, body, out any) error {
	return c.do(http.MethodPost, path, body, out)
}
func (c *Client) Delete(path string) error { return c.do(http.MethodDelete, path, nil, nil) }

// Machine is the CLI's view of the machine resource (contract shape).
type Machine struct {
	ID  string `json:"id"`
	BMC struct {
		Address string `json:"address"`
	} `json:"bmc"`
	State      string `json:"state"`
	PowerState string `json:"power_state"`
}

// Job is the CLI's view of the job resource.
type Job struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	State   string         `json:"state"`
	Summary map[string]any `json:"summary"`
}
