package run_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/agent/run"
	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/uitask"
)

type fakeLocal struct {
	mu       sync.Mutex
	applied  [][]poll.Source
	snapshot []string
	tasks    []uitask.Info
}

func (f *fakeLocal) Apply(_ context.Context, s []poll.Source) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, s)
	return nil
}

func (f *fakeLocal) Snapshot(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshot = append(f.snapshot, p)
	return nil
}

func (f *fakeLocal) Pause(context.Context, string) error  { return nil }
func (f *fakeLocal) Resume(context.Context, string) error { return nil }

func (f *fakeLocal) Tasks(context.Context) ([]uitask.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasks, nil
}

func (f *fakeLocal) TaskLog(context.Context, string) (string, error) { return "log", nil }
func (f *fakeLocal) Status(context.Context) (string, bool)           { return "idle", true }

func TestPollAppliesOnNewEtagAndRunsCommands(t *testing.T) {
	var reports []poll.Report
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/fleet/agent/poll":
			polls++
			var in struct {
				ETag string `json:"etag"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			if in.ETag == "e1" && polls > 1 {
				w.WriteHeader(304)
				return
			}
			json.NewEncoder(w).Encode(poll.PolicyDoc{ETag: "e1", Name: "fw13", Sources: []poll.Source{{Path: "/data", Policy: json.RawMessage(`{}`)}}, Commands: []poll.Command{{ID: 7, Kind: "snapshot-now", Source: "/data"}}, PollIntervalSeconds: 300})
		case "/api/v1/fleet/agent/report":
			var rep poll.Report
			json.NewDecoder(r.Body).Decode(&rep)
			reports = append(reports, rep)
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	t.Setenv("WARPHOLD_STATE_DIR", t.TempDir())
	st := &state.Config{Server: srv.URL, Bearer: "wa_1", Scope: "user"}
	fl := &fakeLocal{}
	l := run.New(run.Deps{Fleet: &poll.Client{Server: srv.URL, Bearer: "wa_1"}, Local: fl, State: st, Now: time.Now, Log: t.Logf})

	require.NoError(t, l.PollOnce(context.Background()))
	require.Len(t, fl.applied, 1)
	require.Equal(t, []string{"/data"}, fl.snapshot)
	require.Len(t, reports, 1)
	require.EqualValues(t, 7, reports[0].CommandID)
	require.Equal(t, "ok", reports[0].Status)
	saved, _ := state.Load("user")
	require.Equal(t, "e1", saved.ETag)

	require.NoError(t, l.PollOnce(context.Background()))
	require.Len(t, fl.applied, 1, "304 -> no re-apply")
}

func TestWatchReportsFinishedTasksOnce(t *testing.T) {
	var reports []poll.Report
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep poll.Report
		json.NewDecoder(r.Body).Decode(&rep)
		reports = append(reports, rep)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	end := time.Now()
	fl := &fakeLocal{tasks: []uitask.Info{
		{TaskID: "t1", Kind: "Snapshot", Description: "Snapshot hody@fw13:/data", StartTime: end.Add(-time.Minute), EndTime: &end, Status: uitask.StatusSuccess},
		{TaskID: "t2", Kind: "Snapshot", Description: "Snapshot hody@fw13:/data", StartTime: end, Status: uitask.StatusRunning},
		{TaskID: "t3", Kind: "Maintenance", Description: "Maintenance", StartTime: end.Add(-time.Minute), EndTime: &end, Status: uitask.StatusFailed, ErrorMessage: "kopia: error: boom"},
	}}
	l := run.New(run.Deps{Fleet: &poll.Client{Server: srv.URL, Bearer: "wa_1"}, Local: fl, State: &state.Config{Scope: "user"}, Now: time.Now, Log: t.Logf})
	require.NoError(t, l.WatchOnce(context.Background()))
	require.Len(t, reports, 2)
	require.Equal(t, "/data", reports[0].Source)
	require.Equal(t, "error", reports[1].Status)
	require.Equal(t, "kopia: error: boom", reports[1].Stderr)
	require.NoError(t, l.WatchOnce(context.Background()))
	require.Len(t, reports, 2, "already reported")
}
