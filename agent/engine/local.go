package engine

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/serverapi"
	"github.com/kopia/kopia/internal/uitask"
	"github.com/kopia/kopia/snapshot/policy"
)

// Local drives a Kopia server for this machine's sources.
type Local struct {
	API  *apiclient.KopiaAPIClient
	Host string
	User string
}

// NewLocal learns the local host/user identity from the server.
func NewLocal(ctx context.Context, api *apiclient.KopiaAPIClient) (*Local, error) {
	var sr serverapi.SourcesResponse
	if err := api.Get(ctx, "sources", nil, &sr); err != nil {
		return nil, errors.Wrap(err, "sources")
	}
	return &Local{API: api, Host: sr.LocalHost, User: sr.LocalUsername}, nil
}

// ExpandHome turns "~" and "~/x" into absolute paths.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func (l *Local) sourceQuery(path string) string {
	q := url.Values{}
	q.Set("userName", l.User)
	q.Set("host", l.Host)
	q.Set("path", path)
	return q.Encode()
}

// Sources lists the server's sources.
func (l *Local) Sources(ctx context.Context) ([]*serverapi.SourceStatus, error) {
	var sr serverapi.SourcesResponse
	if err := l.API.Get(ctx, "sources", nil, &sr); err != nil {
		return nil, err
	}
	return sr.Sources, nil
}

// Apply makes the server's source set and policies match the document.
func (l *Local) Apply(ctx context.Context, sources []poll.Source) error {
	want := map[string]bool{}
	for _, s := range sources {
		path := ExpandHome(s.Path)
		want[path] = true
		var pol policy.Policy
		if len(s.Policy) > 0 {
			if err := json.Unmarshal(s.Policy, &pol); err != nil {
				return errors.Wrapf(err, "policy for %s", s.Path)
			}
		}
		// handleSourcesCreate already calls policy.SetPolicy with req.Policy,
		// so no separate PUT /policy is needed.
		var resp serverapi.CreateSnapshotSourceResponse
		if err := l.API.Post(ctx, "sources", &serverapi.CreateSnapshotSourceRequest{Path: path, CreateSnapshot: false, Policy: &pol}, &resp); err != nil {
			return errors.Wrapf(err, "add source %s", path)
		}
	}
	existing, err := l.Sources(ctx)
	if err != nil {
		return err
	}
	for _, s := range existing {
		if s.Source.Host != l.Host || s.Source.UserName != l.User || want[s.Source.Path] {
			continue
		}
		if err := l.API.Delete(ctx, "policy?"+l.sourceQuery(s.Source.Path), nil, nil, &serverapi.Empty{}); err != nil {
			return errors.Wrapf(err, "remove policy %s", s.Source.Path)
		}
	}
	return l.API.Post(ctx, "refresh", &serverapi.Empty{}, &serverapi.Empty{})
}

// Snapshot starts a snapshot of path now.
func (l *Local) Snapshot(ctx context.Context, path string) error {
	var resp serverapi.MultipleSourceActionResponse
	return l.API.Post(ctx, "sources/upload?"+l.sourceQuery(ExpandHome(path)), &serverapi.Empty{}, &resp)
}

// Pause pauses scheduled snapshots of path (all sources when path is empty).
func (l *Local) Pause(ctx context.Context, path string) error {
	return l.sourceAction(ctx, "control/pause-source", path)
}

// Resume resumes scheduled snapshots of path.
func (l *Local) Resume(ctx context.Context, path string) error {
	return l.sourceAction(ctx, "control/resume-source", path)
}

func (l *Local) sourceAction(ctx context.Context, op, path string) error {
	var resp serverapi.MultipleSourceActionResponse
	q := ""
	if path != "" {
		q = "?" + l.sourceQuery(ExpandHome(path))
	}
	return l.API.Post(ctx, op+q, &serverapi.Empty{}, &resp)
}

// Tasks lists all tasks the server knows about.
func (l *Local) Tasks(ctx context.Context) ([]uitask.Info, error) {
	var tr serverapi.TaskListResponse
	if err := l.API.Get(ctx, "tasks", nil, &tr); err != nil {
		return nil, err
	}
	return tr.Tasks, nil
}

// TaskLog returns the raw log lines of a task, newline-joined.
func (l *Local) TaskLog(ctx context.Context, id string) (string, error) {
	var out serverapi.TaskLogResponse
	if err := l.API.Get(ctx, "tasks/"+id+"/logs", nil, &out); err != nil {
		return "", err
	}
	lines := make([]string, 0, len(out.Logs))
	for _, raw := range out.Logs {
		var e struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Msg != "" {
			lines = append(lines, e.Msg)
		} else {
			lines = append(lines, string(raw))
		}
	}
	return strings.Join(lines, "\n"), nil
}

func counter(t uitask.Info, names ...string) int64 {
	var n int64
	for _, name := range names {
		if c, ok := t.Counters[name]; ok {
			n += c.Value
		}
	}
	return n
}

// ToReport converts a finished task into a Fleet report.
func ToReport(t uitask.Info, source string) poll.Report {
	r := poll.Report{TaskID: t.TaskID, Source: source, StartedAt: t.StartTime, Stderr: t.ErrorMessage}
	if t.EndTime != nil {
		r.FinishedAt = *t.EndTime
	}
	switch t.Status {
	case uitask.StatusSuccess:
		r.Status = "ok"
	case uitask.StatusCanceled:
		r.Status = "cancelled"
	default:
		r.Status = "error"
	}
	if t.Kind == "Snapshot" {
		r.Kind = "snapshot"
	} else {
		r.Kind = strings.ToLower(t.Kind)
	}
	r.Bytes = counter(t, "Hashed Bytes", "Cached Bytes")
	r.Files = counter(t, "Hashed Files", "Cached Files")
	return r
}

// Status summarizes the engine's overall state for Fleet heartbeats.
func (l *Local) Status(ctx context.Context) (engineStatus string, repoConnected bool) {
	sources, err := l.Sources(ctx)
	if err != nil {
		return "unknown", false
	}
	allPaused := len(sources) > 0
	for _, s := range sources {
		if s.CurrentTask != "" {
			return "uploading", true
		}
		if s.Status != "PAUSED" {
			allPaused = false
		}
	}
	if allPaused {
		return "paused", true
	}
	return "idle", true
}
