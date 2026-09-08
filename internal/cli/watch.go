package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// specToJSON converts an install spec file (YAML or JSON) into the JSON the
// API expects. YAML is accepted because hand-written install specs read
// better as YAML; the wire format stays JSON per the contract.
func specToJSON(raw []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		return raw, nil // already JSON
	}
	var v map[string]any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// watchJob polls task progress until the job reaches a terminal state.
func watchJob(c *Client, jobID string) error {
	terminal := map[string]bool{"succeeded": true, "failed": true, "canceled": true, "partial": true}
	for {
		var job struct {
			State string `json:"state"`
		}
		if err := c.Get("/api/v1/jobs/"+jobID, &job); err != nil {
			return err
		}
		var tasks struct {
			Items []struct {
				ID     string `json:"id"`
				State  string `json:"state"`
				Stages []struct {
					Name  string `json:"name"`
					State string `json:"state"`
				} `json:"stages"`
			} `json:"items"`
		}
		if err := c.Get(fmt.Sprintf("/api/v1/jobs/%s/tasks?page_size=200", jobID), &tasks); err != nil {
			return err
		}
		fmt.Printf("\r[%s job] ", job.State)
		for i, t := range tasks.Items {
			if i > 0 {
				fmt.Print(" | ")
			}
			last := ""
			for _, s := range t.Stages {
				if s.State != "pending" {
					last = s.Name
				}
			}
			fmt.Printf("%s %s@%s", t.ID, t.State, last)
		}
		fmt.Println()
		if terminal[job.State] {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

// streamRequest builds an authenticated SSE request.
func streamRequest(c *Client, path string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "text/event-stream")
	return req, nil
}

// streamPrint consumes an SSE stream, printing events as they arrive.
func streamPrint(c *Client, req *http.Request) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") || strings.HasPrefix(line, "event: ") || strings.HasPrefix(line, "id: ") {
			fmt.Println(line)
		}
	}
	return sc.Err()
}
