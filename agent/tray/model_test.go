package tray_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/tray"
	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/serverapi"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/upload"
)

var now = time.Date(2026, 9, 2, 19, 30, 0, 0, time.Local)

func label(t *testing.T, m tray.Model, k tray.Kind) tray.Item {
	t.Helper()

	for _, it := range m.Items {
		if it.Kind == k {
			return it
		}
	}

	t.Fatalf("menu has no item of kind %v", k)

	return tray.Item{}
}

func source(path, status string) *serverapi.SourceStatus {
	return &serverapi.SourceStatus{
		Source: snapshot.SourceInfo{Host: "node-a", UserName: "user", Path: path},
		Status: status,
	}
}

func TestBuildUploading(t *testing.T) {
	src := source("/home/user", "UPLOADING")
	src.CurrentTask = "task-1"
	src.UploadCounters = &upload.Counters{EstimatedBytes: 1000, TotalHashedBytes: 300, TotalCachedBytes: 80}
	last := now.Add(-30 * time.Minute)
	src.LastSnapshot = &snapshot.Manifest{StartTime: fs.UTCTimestampFromTime(last)}
	nxt := now.Add(90 * time.Minute)
	src.NextSnapshotTime = &nxt

	m := tray.Build(tray.Status{Running: true, Vault: "Laptops · laptop-1", Sources: []*serverapi.SourceStatus{src}}, now)

	require.Equal(t, tray.ToneEmber, m.Tone)
	require.Equal(t, "Laptops · laptop-1", label(t, m, tray.KindVault).Label)
	require.False(t, label(t, m, tray.KindVault).Enabled, "the vault name is a label, not an action")
	require.Equal(t, "Backing up /home/user — 38%", label(t, m, tray.KindActivity).Label)
	require.Equal(t, "Last good backup: today 19:00", label(t, m, tray.KindLast).Label)
	require.Equal(t, "Next: today 21:00", label(t, m, tray.KindNext).Label)
	require.Equal(t, "Errors this week: 0", label(t, m, tray.KindErrors).Label)
	require.Equal(t, "Pause", label(t, m, tray.KindPauseResume).Label)
	require.True(t, label(t, m, tray.KindStartAgent).Hidden, "the agent is running")
	require.False(t, label(t, m, tray.KindQuit).Hidden)
}

func TestBuildIdleAndPaused(t *testing.T) {
	m := tray.Build(tray.Status{Running: true, Vault: "laptop-1", Sources: []*serverapi.SourceStatus{source("/home/user", "IDLE")}}, now)
	require.Equal(t, tray.ToneGood, m.Tone)
	require.Equal(t, "Idle", label(t, m, tray.KindActivity).Label)
	require.Equal(t, "Last good backup: never", label(t, m, tray.KindLast).Label)
	require.Equal(t, "Next: not scheduled", label(t, m, tray.KindNext).Label)

	p := tray.Build(tray.Status{Running: true, Vault: "laptop-1", Sources: []*serverapi.SourceStatus{source("/home/user", "PAUSED")}}, now)
	require.Equal(t, "Paused", label(t, p, tray.KindActivity).Label)
	require.Equal(t, "Resume", label(t, p, tray.KindPauseResume).Label)
}

func TestBuildFailedAndErrors(t *testing.T) {
	f := tray.Build(tray.Status{Running: true, Vault: "laptop-1", Sources: []*serverapi.SourceStatus{source("/home/user", "FAILED")}, ErrorsThisWeek: 2}, now)
	require.Equal(t, tray.ToneBad, f.Tone)
	require.Equal(t, "Errors this week: 2", label(t, f, tray.KindErrors).Label)

	w := tray.Build(tray.Status{Running: true, Vault: "laptop-1", Sources: []*serverapi.SourceStatus{source("/home/user", "IDLE")}, ErrorsThisWeek: 1}, now)
	require.Equal(t, tray.ToneWarn, w.Tone, "past errors warn but a healthy source is not bad")
}

// TestBuildAgentNotRunning pins the no-engine model: a dim icon, "Agent not
// running", and only the actions that make sense with no engine to talk to.
func TestBuildAgentNotRunning(t *testing.T) {
	m := tray.Build(tray.Status{Vault: "laptop-1"}, now)

	require.Equal(t, tray.ToneDim, m.Tone)
	require.Equal(t, "Agent not running", label(t, m, tray.KindActivity).Label)
	require.False(t, label(t, m, tray.KindStartAgent).Hidden)
	require.True(t, label(t, m, tray.KindBackupNow).Hidden)
	require.True(t, label(t, m, tray.KindPauseResume).Hidden)
	require.True(t, label(t, m, tray.KindDetails).Hidden)
	require.True(t, label(t, m, tray.KindErrors).Hidden)
	require.False(t, label(t, m, tray.KindQuit).Hidden)
}

func TestVaultLabel(t *testing.T) {
	require.Equal(t, "Laptops · laptop-1", tray.VaultLabel("Laptops", "laptop-1"))
	require.Equal(t, "laptop-1", tray.VaultLabel("", "laptop-1"))
	require.Equal(t, "WarpHold", tray.VaultLabel("", ""))
}

func TestWhenNeighbouringDays(t *testing.T) {
	src := source("/home/user", "IDLE")
	src.LastSnapshot = &snapshot.Manifest{StartTime: fs.UTCTimestampFromTime(now.AddDate(0, 0, -1))}
	m := tray.Build(tray.Status{Running: true, Sources: []*serverapi.SourceStatus{src}}, now)
	require.Equal(t, "Last good backup: yesterday 19:30", label(t, m, tray.KindLast).Label)

	src.LastSnapshot = &snapshot.Manifest{StartTime: fs.UTCTimestampFromTime(now.AddDate(0, 0, -4))}
	m = tray.Build(tray.Status{Running: true, Sources: []*serverapi.SourceStatus{src}}, now)
	require.Equal(t, "Last good backup: "+now.AddDate(0, 0, -4).Format("Jan 2 15:04"), label(t, m, tray.KindLast).Label)
}

// TestToneWatcherNotifiesOncePerFailure is the notification rule: a
// transition into bad notifies, staying bad does not, and a recovery arms it
// again.
func TestToneWatcherNotifiesOncePerFailure(t *testing.T) {
	var w tray.ToneWatcher

	require.False(t, w.Notify(tray.ToneGood))
	require.True(t, w.Notify(tray.ToneBad), "first transition into bad notifies")
	require.False(t, w.Notify(tray.ToneBad), "still bad: no repeat")
	require.False(t, w.Notify(tray.ToneBad))
	require.False(t, w.Notify(tray.ToneGood))
	require.True(t, w.Notify(tray.ToneBad), "a new failure notifies again")
}

// TestToneWatcherFirstPollBad covers a tray started while the agent is
// already failing: there is no earlier tone, so that first poll notifies.
func TestToneWatcherFirstPollBad(t *testing.T) {
	var w tray.ToneWatcher

	require.True(t, w.Notify(tray.ToneBad))
	require.False(t, w.Notify(tray.ToneBad))
}
