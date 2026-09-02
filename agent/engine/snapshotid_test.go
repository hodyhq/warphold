package engine_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/serverapi"
	"github.com/kopia/kopia/repo/manifest"
)

// TestLatestSnapshotIDPicksTheTasksOwnManifest pins that each finished task
// is matched to the snapshot it created. Two snapshots of one source can
// finish between two watch cycles, and a search bounded only from below would
// then report the later snapshot's manifest for the earlier task.
func TestLatestSnapshotIDPicksTheTasksOwnManifest(t *testing.T) {
	ctx := context.Background()
	base := clock.Now().UTC().Truncate(time.Second)
	first, second := base, base.Add(10*time.Minute)

	snap := func(id string, start time.Time) *serverapi.Snapshot {
		return &serverapi.Snapshot{
			ID:        manifest.ID(id),
			StartTime: fs.UTCTimestampFromTime(start),
			EndTime:   fs.UTCTimestampFromTime(start.Add(time.Minute)),
		}
	}

	var gotQuery string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		json.NewEncoder(w).Encode(&serverapi.SnapshotsResponse{ //nolint:errcheck
			Snapshots: []*serverapi.Snapshot{snap("k1111", first), snap("k2222", second)},
		})
	}))
	defer srv.Close()

	api, err := apiclient.NewKopiaAPIClient(apiclient.Options{BaseURL: srv.URL, Username: "u", Password: "p"})
	require.NoError(t, err)

	l := &engine.Local{API: api, Host: "fw13", User: "hody"}

	id, err := l.LatestSnapshotID(ctx, "/data", first, first.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, "k1111", id, "the earlier task keeps its own manifest")

	// all=1 so the server does not collapse snapshots sharing a root entry.
	require.Contains(t, gotQuery, "all=1")
	require.Contains(t, gotQuery, "path=%2Fdata")
	require.Contains(t, gotQuery, "host=fw13")

	id, err = l.LatestSnapshotID(ctx, "/data", second, second.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, "k2222", id)

	id, err = l.LatestSnapshotID(ctx, "/data", base.Add(time.Hour), base.Add(2*time.Hour))
	require.NoError(t, err)
	require.Empty(t, id, "a window matching no manifest yields no id")

	id, err = l.LatestSnapshotID(ctx, "", first, first.Add(time.Minute))
	require.NoError(t, err)
	require.Empty(t, id, "a task with no parsed source path is not looked up")
}

// TestLatestSnapshotIDSeparatesOverlappingTasks pins the round-1 review fix:
// two tasks whose windows overlap within the clock skew must each keep their
// own manifest. A skew applied to the upper bound, or picking the newest
// candidate in the window, gave the earlier task the later task's backup -
// and Fleet would then offer a restore from the wrong snapshot.
func TestLatestSnapshotIDSeparatesOverlappingTasks(t *testing.T) {
	ctx := context.Background()
	base := clock.Now().UTC().Truncate(time.Second)

	// Task A runs [base, base+3s], task B [base+2s, base+5s]: adjacent tasks
	// less than the old five-second skew apart, with overlapping windows.
	aStart, bStart := base, base.Add(2*time.Second)

	snap := func(id string, start time.Time) *serverapi.Snapshot {
		return &serverapi.Snapshot{
			ID:        manifest.ID(id),
			StartTime: fs.UTCTimestampFromTime(start),
			EndTime:   fs.UTCTimestampFromTime(start.Add(time.Second)),
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(&serverapi.SnapshotsResponse{ //nolint:errcheck
			Snapshots: []*serverapi.Snapshot{snap("kaaaa", aStart), snap("kbbbb", bStart)},
		})
	}))
	defer srv.Close()

	api, err := apiclient.NewKopiaAPIClient(apiclient.Options{BaseURL: srv.URL, Username: "u", Password: "p"})
	require.NoError(t, err)

	l := &engine.Local{API: api, Host: "fw13", User: "hody"}

	id, err := l.LatestSnapshotID(ctx, "/data", aStart, aStart.Add(3*time.Second))
	require.NoError(t, err)
	require.Equal(t, "kaaaa", id, "the earlier task must not be given the later task's manifest")

	id, err = l.LatestSnapshotID(ctx, "/data", bStart, bStart.Add(3*time.Second))
	require.NoError(t, err)
	require.Equal(t, "kbbbb", id)
}
