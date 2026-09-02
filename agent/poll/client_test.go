package poll_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/poll"
)

func TestPollEtagAndRevoked(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/api/v1/fleet/agent/poll":
			json.NewDecoder(r.Body).Decode(&gotBody)
			if gotBody["etag"] == "e1" {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			if gotBody["etag"] == "revoked" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"etag": "e1", "name": "fw13", "sources": []map[string]any{{"path": "/home/hody", "policy": map[string]any{}}}, "commands": []any{}, "poll_interval_seconds": 300})
		case "/api/v1/fleet/agent/report":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := &poll.Client{Server: srv.URL, Bearer: "wa_1"}
	doc, err := c.Poll(context.Background(), poll.Heartbeat{Version: "0.1.0"}, "")
	require.NoError(t, err)
	require.Equal(t, "Bearer wa_1", gotAuth)
	require.Equal(t, "e1", doc.ETag)
	require.Equal(t, "/home/hody", doc.Sources[0].Path)
	require.Equal(t, "0.1.0", gotBody["heartbeat"].(map[string]any)["version"])

	doc, err = c.Poll(context.Background(), poll.Heartbeat{}, "e1")
	require.NoError(t, err)
	require.Nil(t, doc, "304 means unchanged")

	_, err = c.Poll(context.Background(), poll.Heartbeat{}, "revoked")
	require.ErrorIs(t, err, poll.ErrRevoked)

	require.NoError(t, c.Report(context.Background(), poll.Report{TaskID: "t1", Kind: "snapshot", Status: "ok", StartedAt: time.Now(), FinishedAt: time.Now()}))
}

func TestJitterBounds(t *testing.T) {
	for i := 0; i < 100; i++ {
		d := poll.Jitter(5 * time.Minute)
		require.True(t, d >= 4*time.Minute && d <= 6*time.Minute, d)
	}
	require.GreaterOrEqual(t, poll.Jitter(10*time.Second), 30*time.Second)
}
