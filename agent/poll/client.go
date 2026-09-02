// Package poll is the agent's client for the Fleet poll/report endpoints.
package poll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

type Heartbeat struct {
	Version       string `json:"version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	DiskFreeBytes uint64 `json:"disk_free_bytes"`
	RepoConnected bool   `json:"repo_connected"`
	EngineStatus  string `json:"engine_status"`
}

type Source struct {
	Path   string          `json:"path"`
	Policy json.RawMessage `json:"policy"`
}

type Command struct {
	ID     int64  `json:"id"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
}

type PolicyDoc struct {
	ETag                string    `json:"etag"`
	Name                string    `json:"name"`
	Sources             []Source  `json:"sources"`
	Commands            []Command `json:"commands"`
	PollIntervalSeconds int       `json:"poll_interval_seconds"`
}

type Report struct {
	TaskID     string    `json:"task_id"`
	Kind       string    `json:"kind"`
	CommandID  int64     `json:"command_id,omitempty"`
	Source     string    `json:"source"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Status     string    `json:"status"`
	Bytes      int64     `json:"bytes"`
	Files      int64     `json:"files"`
	SnapshotID string    `json:"snapshot_id,omitempty"`
	Stderr     string    `json:"stderr,omitempty"`
}

// ErrRevoked means the Fleet no longer accepts this agent's bearer token.
var ErrRevoked = errors.New("this agent was revoked by the Fleet server")

// Client talks to one Fleet server.
type Client struct {
	Server string
	Bearer string
	HTTP   *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *Client) post(ctx context.Context, path string, body any) (*http.Response, []byte, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Server, "/")+path, bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp, raw, nil
}

// Poll sends a heartbeat and returns the policy document, or nil when unchanged.
func (c *Client) Poll(ctx context.Context, hb Heartbeat, etag string) (*PolicyDoc, error) {
	resp, raw, err := c.post(ctx, "/api/v1/fleet/agent/poll", map[string]any{"etag": etag, "heartbeat": hb})
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, nil
	case http.StatusUnauthorized:
		return nil, ErrRevoked
	case http.StatusOK:
		var doc PolicyDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("malformed policy document: %w", err)
		}
		return &doc, nil
	default:
		return nil, fmt.Errorf("fleet returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

// Report posts one finished task.
func (c *Client) Report(ctx context.Context, r Report) error {
	resp, raw, err := c.post(ctx, "/api/v1/fleet/agent/report", r)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrRevoked
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("fleet returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// Jitter returns base ± 60 s, never below 30 s.
func Jitter(base time.Duration) time.Duration {
	d := base + time.Duration(rand.IntN(121)-60)*time.Second
	if d < 30*time.Second {
		return 30 * time.Second
	}
	return d
}
