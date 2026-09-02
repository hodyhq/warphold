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
		{TaskID: "t4", Kind: "Snapshot", Description: "hody@fw13:/data2 at 2026-09-01T23:00:00Z", StartTime: end.Add(-time.Minute), EndTime: &end, Status: uitask.StatusFailed},
	}}
	l := run.New(run.Deps{Fleet: &poll.Client{Server: srv.URL, Bearer: "wa_1"}, Local: fl, State: &state.Config{Scope: "user"}, Now: time.Now, Log: t.Logf})
	require.NoError(t, l.WatchOnce(context.Background()))
	require.Len(t, reports, 3)
	require.Equal(t, "/data", reports[0].Source)
	require.Equal(t, "error", reports[1].Status)
	require.Equal(t, "kopia: error: boom", reports[1].Stderr)
	require.Equal(t, "/data2", reports[2].Source, "real 'user@host:path at <timestamp>' description parsed")
	require.Equal(t, "error", reports[2].Status)
	require.Equal(t, "log", reports[2].Stderr, "empty ErrorMessage falls back to the task log")
	require.NoError(t, l.WatchOnce(context.Background()))
	require.Len(t, reports, 3, "already reported")
}

func TestWatchParsesRealSnapshotDescriptionFormat(t *testing.T) {
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
		{TaskID: "t1", Kind: "Snapshot", Description: "hody@fw13:/data at 2026-09-01T23:00:00.123456789Z", StartTime: end.Add(-time.Minute), EndTime: &end, Status: uitask.StatusSuccess},
	}}
	l := run.New(run.Deps{Fleet: &poll.Client{Server: srv.URL, Bearer: "wa_1"}, Local: fl, State: &state.Config{Scope: "user"}, Now: time.Now, Log: t.Logf})
	require.NoError(t, l.WatchOnce(context.Background()))
	require.Len(t, reports, 1)
	require.Equal(t, "/data", reports[0].Source)
}

// TestRestartedAgentReportsUniqueTaskIDs pins C1: Kopia numbers tasks from a
// per-process counter, so the first snapshot after an agent restart is task
// "1" again. Fleet dedupes on (agent_id, task_id) with INSERT OR IGNORE, so
// a repeated id would be silently dropped and health would never go green
// again. Two loops (a simulated restart) reporting a task with the same local
// id must reach Fleet as two distinct reports.
func TestRestartedAgentReportsUniqueTaskIDs(t *testing.T) {
	var mu sync.Mutex
	var reports []poll.Report
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep poll.Report
		json.NewDecoder(r.Body).Decode(&rep) //nolint:errcheck
		mu.Lock()
		reports = append(reports, rep)
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer srv.Close()

	report := func(start time.Time) {
		end := start.Add(time.Minute)
		fl := &fakeLocal{tasks: []uitask.Info{
			{TaskID: "1", Kind: "Snapshot", Description: "hody@fw13:/data at 2026-09-01T23:00:00Z", StartTime: start, EndTime: &end, Status: uitask.StatusSuccess},
		}}
		l := run.New(run.Deps{Fleet: &poll.Client{Server: srv.URL, Bearer: "wa_1"}, Local: fl, State: &state.Config{Scope: "user"}, Now: time.Now, Log: t.Logf})
		require.NoError(t, l.WatchOnce(context.Background()))
		// a re-poll within one engine lifetime must still dedupe locally
		require.NoError(t, l.WatchOnce(context.Background()))
	}

	first := time.Now().Add(-time.Hour)
	report(first)
	report(first.Add(30 * time.Minute)) // restart: same local task id, later start

	require.Len(t, reports, 2)
	require.NotEqual(t, reports[0].TaskID, reports[1].TaskID, "wire task id must be unique per engine lifetime")
	require.Contains(t, reports[0].TaskID, "-1")
	require.Equal(t, "/data", reports[0].Source)
}
