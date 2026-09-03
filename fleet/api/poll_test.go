package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/poll"
)

func enrollAgent(t *testing.T, h *harness) (id string, bearer string) {
	t.Helper()
	h.setPublicURL()
	gid := h.mkGroup(t)
	_, tok := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})
	admin := h.jar
	h.jar = nil
	resp, body := h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok["token"], "hostname": "fw13", "os": "linux", "arch": "amd64", "version": "0.1.0", "scope": "user"})
	require.Equal(t, 201, resp.StatusCode)
	h.jar = admin
	return body["agent_id"].(string), body["bearer"].(string)
}

func TestPollReportHealth(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	id, bearer := enrollAgent(t, h)
	c := &poll.Client{Server: h.srv.URL, Bearer: bearer}
	ctx := t.Context()

	doc, err := c.Poll(ctx, poll.Heartbeat{Version: "0.1.1"}, "")
	require.NoError(t, err)
	require.Equal(t, "fw13", doc.Name)
	require.Equal(t, "~", doc.Sources[0].Path)
	require.JSONEq(t, `{}`, string(doc.Sources[0].Policy))
	require.Equal(t, 300, doc.PollIntervalSeconds)
	require.NotEmpty(t, doc.ETag)

	again, err := c.Poll(ctx, poll.Heartbeat{}, doc.ETag)
	require.NoError(t, err)
	require.Nil(t, again)

	// a pending command breaks the 304
	resp, _ := h.do("POST", "/api/v1/fleet/agents/"+id+"/commands", map[string]any{"kind": "snapshot-now", "source": "~"})
	require.Equal(t, 201, resp.StatusCode)
	withCmd, err := c.Poll(ctx, poll.Heartbeat{}, doc.ETag)
	require.NoError(t, err)
	require.Len(t, withCmd.Commands, 1)

	now := time.Now()
	require.NoError(t, c.Report(ctx, poll.Report{TaskID: "t1", Kind: "command", CommandID: withCmd.Commands[0].ID, Source: "~", StartedAt: now.Add(-time.Minute), FinishedAt: now, Status: "ok", SnapshotID: "k1", Bytes: 5, Files: 1}))
	after, err := c.Poll(ctx, poll.Heartbeat{}, doc.ETag)
	require.NoError(t, err)
	require.Nil(t, after, "command acknowledged, back to 304")

	// A command ack is NOT a backup: acknowledging a pause/resume must not
	// turn health green (fleet/store.LastOKReport counts kind='snapshot').
	_, detail := h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	require.Equal(t, "unknown", detail["health"], "a command ack is not evidence of a backup")
	require.Equal(t, "0.1.1", detail["version"])
	require.NotNil(t, detail["last_seen_at"])

	// an actual snapshot report is what makes it green
	require.NoError(t, c.Report(ctx, poll.Report{TaskID: "t1s", Kind: "snapshot", Source: "~", StartedAt: now.Add(-time.Minute), FinishedAt: now, Status: "ok", SnapshotID: "k1", Bytes: 5, Files: 1}))
	_, detail = h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	require.Equal(t, "green", detail["health"])

	require.NoError(t, c.Report(ctx, poll.Report{TaskID: "t2", Kind: "snapshot", Source: "~", StartedAt: now, FinishedAt: now.Add(time.Second), Status: "error", Stderr: "kopia: error: unable to write blob"}))
	_, detail = h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	require.Equal(t, "red", detail["health"])
	reports := detail["reports"].([]any)
	require.Equal(t, "kopia: error: unable to write blob", reports[0].(map[string]any)["stderr"])

	// template change → new etag
	_, tpls := h.doList("GET", "/api/v1/fleet/templates")
	tplID := tpls[0]["id"].(float64)
	req := h.newRequest("PUT", "/api/v1/fleet/templates/"+jsonNum(tplID), jsonBody(map[string]any{"name": "Home default", "sources": []string{"~", "/etc"}, "policy": map[string]any{"retention": map[string]any{"keepLatest": 3}}}))
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, 204, res.StatusCode)
	changed, err := c.Poll(ctx, poll.Heartbeat{}, doc.ETag)
	require.NoError(t, err)
	require.NotNil(t, changed)
	require.Len(t, changed.Sources, 2)

	// revoke → 401
	resp, _ = h.do("POST", "/api/v1/fleet/agents/"+id+"/revoke", nil)
	require.Equal(t, 204, resp.StatusCode)
	_, err = c.Poll(ctx, poll.Heartbeat{}, "")
	require.ErrorIs(t, err, poll.ErrRevoked)
	_ = json.Marshal
}

// TestReportRejectsOtherAgentsCommand pins the fix for a cross-agent ack: an
// agent must not be able to acknowledge (and so silently discard) a command
// that was queued for a different agent, even though command ids are small
// sequential integers an attacker could guess.
func TestReportRejectsOtherAgentsCommand(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	_, bearerA := enrollAgent(t, h)
	idB, bearerB := enrollAgent(t, h)
	ctx := t.Context()

	resp, cmd := h.do("POST", "/api/v1/fleet/agents/"+idB+"/commands", map[string]any{"kind": "snapshot-now", "source": "~"})
	require.Equal(t, 201, resp.StatusCode)
	cmdID := int64(cmd["id"].(float64))

	cA := &poll.Client{Server: h.srv.URL, Bearer: bearerA}
	now := time.Now()
	err := cA.Report(ctx, poll.Report{TaskID: "steal", Kind: "command", CommandID: cmdID, Source: "~", StartedAt: now, FinishedAt: now, Status: "ok"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "400")

	cB := &poll.Client{Server: h.srv.URL, Bearer: bearerB}
	docB, err := cB.Poll(ctx, poll.Heartbeat{}, "")
	require.NoError(t, err)
	require.Len(t, docB.Commands, 1, "B's command must still be pending; A's report must not have acked it")
}

// TestAgentPollRejectsBeforeActivation pins that a request carrying a bearer
// token, on a server that has never been activated, is rejected with a 4xx
// JSON error rather than panicking on a nil store: requireActivated must run
// (and reject) before requireAgent ever calls store().
func TestAgentPollRejectsBeforeActivation(t *testing.T) {
	h := newHarness(t)
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/fleet/agent/poll", jsonBody(map[string]any{}))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.GreaterOrEqual(t, resp.StatusCode, 400)
	require.Less(t, resp.StatusCode, 500, "must be a client error, not a 5xx from a nil-store panic")
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.NotEmpty(t, out["error"])
}

// TestReportAckIsIdempotentOnRetry pins the fix that ack failures other than
// "already acked" must fail the request. A retried report for a command that
// this same agent already acknowledged hits store.ErrNotFound on the second
// AckCommand call; that must still be a 204 (the report itself is stored
// either way), not a 500.
func TestReportAckIsIdempotentOnRetry(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	id, bearer := enrollAgent(t, h)
	resp, cmd := h.do("POST", "/api/v1/fleet/agents/"+id+"/commands", map[string]any{"kind": "snapshot-now", "source": "~"})
	require.Equal(t, 201, resp.StatusCode)
	cmdID := int64(cmd["id"].(float64))

	c := &poll.Client{Server: h.srv.URL, Bearer: bearer}
	ctx := t.Context()
	now := time.Now()
	rep := poll.Report{TaskID: "t1", Kind: "command", CommandID: cmdID, Source: "~", StartedAt: now, FinishedAt: now, Status: "ok"}
	require.NoError(t, c.Report(ctx, rep))
	// Retry: the command is already acked, so AckCommand now returns
	// ErrNotFound. That must not surface as a failed report.
	require.NoError(t, c.Report(ctx, rep))
}

func TestReportStderrIsCapped(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	id, bearer := enrollAgent(t, h)
	c := &poll.Client{Server: h.srv.URL, Bearer: bearer}
	now := time.Now()
	huge := strings.Repeat("x", 3*8192)
	require.NoError(t, c.Report(t.Context(), poll.Report{TaskID: "t-big", Kind: "snapshot", Source: "~", StartedAt: now, FinishedAt: now, Status: "failed", Stderr: huge}))
	resp, body := h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	require.Equal(t, 200, resp.StatusCode)
	reports := body["reports"].([]any)
	require.Len(t, reports, 1)
	got := reports[0].(map[string]any)["stderr"].(string)
	require.Len(t, got, 8192) // == maxReportStderr in package api
}
