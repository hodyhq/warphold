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
