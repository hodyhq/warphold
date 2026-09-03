package api_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite" // the store's driver, registered as "sqlite"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/store"
)

// dropTable breaks exactly one query on a live server, from its own connection.
// Closing the whole store would fail the session lookup first and never reach
// the handler under test; this fails the offsite fold and nothing else.
func dropTable(t *testing.T, h *harness, name string) {
	t.Helper()

	db, err := sql.Open("sqlite", fleet.PathsFor(h.stateDir).DB)
	require.NoError(t, err)

	defer db.Close() //nolint:errcheck // test cleanup

	_, err = db.Exec("DROP TABLE " + name)
	require.NoError(t, err)
}

// mirrorGroup creates a group whose target keeps an offsite mirror, and
// returns the group id.
//
// The target is written through the store as a *filesystem* target rather than
// posted as a hosted one, because a device cannot enrol into a hosted target
// yet - that provisioning arrives in a later task. Nothing under test cares:
// the offsite rules read `targets.mirror_kind` and never the target's own kind.
func mirrorGroup(t *testing.T, h *harness) float64 {
	t.Helper()

	tid, err := openStore(t, h).CreateTarget(t.Context(), &store.Target{
		Name: "mirrored", Kind: "filesystem", Path: t.TempDir(),
		MirrorKind: "b2", MirrorBucket: "hody-offsite", MirrorRegion: "us-west-004",
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, tp := h.do("POST", "/api/v1/fleet/templates", map[string]any{"name": "Home", "sources": []string{"~"}, "policy": map[string]any{}})
	resp, g := h.do("POST", "/api/v1/fleet/groups", map[string]any{"name": "Mirrored", "target_id": tid, "template_id": tp["id"]})
	require.Equal(t, 201, resp.StatusCode, g)

	return g["id"].(float64)
}

// openStore is a second connection to the fleet's database, so a test can seed
// the rows the jobs would have written. The server owns its own connection.
func openStore(t *testing.T, h *harness) *store.Store {
	t.Helper()

	st, err := store.Open(fleet.PathsFor(h.stateDir).DB)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // test cleanup

	return st
}

// TestTargetOutCarriesOffsiteState pins the target shape the UI reads: the
// storage mode and the mirror's kind and bucket, and never a credential.
func TestTargetOutCarriesOffsiteState(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": t.TempDir(),
		"mirror_kind": "b2", "mirror_bucket": "hody-offsite", "mirror_region": "us-west-004",
		"mirror_key_id": "k", "mirror_key": "s",
	})
	require.Equal(t, 201, resp.StatusCode, body)

	_, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Len(t, list, 1)
	require.Equal(t, "hosted", list[0]["kind"])
	require.Equal(t, "disk", list[0]["storage_mode"])
	require.Equal(t, "b2", list[0]["mirror_kind"])
	require.Equal(t, "hody-offsite", list[0]["mirror_bucket"])

	for _, k := range []string{"key", "key_id", "mirror_key", "mirror_key_id", "sealed_admin_key", "sealed_mirror_key"} {
		require.NotContains(t, list[0], k, "%s must never leave the server", k)
	}
}

// TestTargetRowMirrorFreshness pins the three states a target row renders:
// no offsite at all, offsite behind, offsite current.
func TestTargetRowMirrorFreshness(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	id, _ := enrollInto(t, h, mirrorGroup(t, h), "laptop-1")

	byName := func() map[string]map[string]any {
		_, list := h.doList("GET", "/api/v1/fleet/targets")
		out := map[string]map[string]any{}

		for _, row := range list {
			out[row["name"].(string)] = row
		}

		return out
	}

	// The plain target enrollAgent-style group made has no mirror at all.
	h.mkGroup(t)
	rows := byName()
	require.NotContains(t, rows["local"], "mirror_stale", "a target without a mirror is never stale")
	require.NotContains(t, rows["local"], "mirrored_at")

	require.Equal(t, true, rows["mirrored"]["mirror_stale"], "never mirrored is behind")
	require.NotContains(t, rows["mirrored"], "mirrored_at")

	st := openStore(t, h)
	ctx := context.Background()
	require.NoError(t, st.SetSetting(ctx, "mirror_interval", "300"))

	now := time.Now().UTC().Truncate(time.Second)
	h.s.SetNowForTesting(func() time.Time { return now })
	require.NoError(t, st.SetMirrored(ctx, id, now.Add(-2*time.Hour), 4096))

	rows = byName()
	require.Equal(t, true, rows["mirrored"]["mirror_stale"], "two hours is past three 5-minute intervals")
	require.NotNil(t, rows["mirrored"]["mirrored_at"])

	require.NoError(t, st.SetMirrored(ctx, id, now.Add(-time.Minute), 4096))
	rows = byName()
	require.NotContains(t, rows["mirrored"], "mirror_stale", "a fresh mirror is not stale")
	require.NotNil(t, rows["mirrored"]["mirrored_at"])
}

// TestDeviceMirrorIsNullWithoutAMirror: a device on a plain filesystem target
// has no offsite copy to be behind on, which is a different state from one
// whose mirror is stale.
func TestDeviceMirrorIsNullWithoutAMirror(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	id, _ := enrollInto(t, h, h.mkGroup(t), "laptop-1")

	resp, body := h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	require.Equal(t, 200, resp.StatusCode)
	require.Contains(t, body, "mirror")
	require.Nil(t, body["mirror"])
}

// TestDeviceMirrorNeverMirroredIsStale: the target mirrors, this device is not
// in the mirror, so it is behind - not "fine, nothing to report".
func TestDeviceMirrorNeverMirroredIsStale(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	id, _ := enrollInto(t, h, mirrorGroup(t, h), "laptop-1")

	resp, body := h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	require.Equal(t, 200, resp.StatusCode)

	m := body["mirror"].(map[string]any)
	require.Nil(t, m["mirrored_at"])
	require.Equal(t, float64(0), m["mirrored_bytes"])
	require.Equal(t, true, m["stale"])
}

// TestDeviceMirrorStaleBoundary walks the exact rule: stale once the last
// mirror is older than three intervals, and not one second before.
func TestDeviceMirrorStaleBoundary(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	id, _ := enrollInto(t, h, mirrorGroup(t, h), "laptop-1")

	st := openStore(t, h)
	ctx := context.Background()
	// The scheduler's floor, so the window is exactly 15 min.
	require.NoError(t, st.SetSetting(ctx, "mirror_interval", "300"))

	now := time.Now().UTC().Truncate(time.Second)
	h.s.SetNowForTesting(func() time.Time { return now })

	mirror := func(age time.Duration) map[string]any {
		require.NoError(t, st.SetMirrored(ctx, id, now.Add(-age), 4096))
		_, body := h.do("GET", "/api/v1/fleet/agents/"+id, nil)

		return body["mirror"].(map[string]any)
	}

	m := mirror(15 * time.Minute)
	require.Equal(t, false, m["stale"], "exactly three intervals old is not yet stale")
	require.Equal(t, float64(4096), m["mirrored_bytes"])
	require.NotNil(t, m["mirrored_at"])

	m = mirror(15*time.Minute + time.Second)
	require.Equal(t, true, m["stale"], "a second past three intervals is stale")
}

// TestOverviewOffsiteCounts: the dashboard tile counts the targets that mirror
// at all and the devices behind in one; a device on a target without a mirror
// is never counted.
func TestOverviewOffsiteCounts(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	plainID, _ := enrollInto(t, h, h.mkGroup(t), "no-mirror")
	freshID, _ := enrollInto(t, h, mirrorGroup(t, h), "fresh")
	staleID, _ := enrollInto(t, h, mirrorGroup(t, h), "behind")

	_, body := h.do("GET", "/api/v1/fleet/overview", nil)
	off := body["offsite"].(map[string]any)
	require.Equal(t, float64(2), off["targets_with_mirror"])
	require.Equal(t, float64(2), off["stale_devices"], "never mirrored counts as behind")

	st := openStore(t, h)
	ctx := context.Background()
	require.NoError(t, st.SetSetting(ctx, "mirror_interval", "300"))

	now := time.Now().UTC().Truncate(time.Second)
	h.s.SetNowForTesting(func() time.Time { return now })
	require.NoError(t, st.SetMirrored(ctx, freshID, now.Add(-time.Minute), 1))
	require.NoError(t, st.SetMirrored(ctx, staleID, now.Add(-time.Hour), 1))
	// A device outside a mirrored target is not counted even when its own row
	// is ancient.
	require.NoError(t, st.SetMirrored(ctx, plainID, now.Add(-24*time.Hour), 1))

	_, body = h.do("GET", "/api/v1/fleet/overview", nil)
	off = body["offsite"].(map[string]any)
	require.Equal(t, float64(2), off["targets_with_mirror"])
	require.Equal(t, float64(1), off["stale_devices"])
}

// TestOverviewOffsiteEmptyWithoutMirrors keeps the tile hideable: a fleet with
// no mirror reports zeros rather than every device as behind.
func TestOverviewOffsiteEmptyWithoutMirrors(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	enrollInto(t, h, h.mkGroup(t), "laptop-1")

	_, body := h.do("GET", "/api/v1/fleet/overview", nil)
	off := body["offsite"].(map[string]any)
	require.Equal(t, float64(0), off["targets_with_mirror"])
	require.Equal(t, float64(0), off["stale_devices"])
}

// TestTargetListFailsLoudlyWhenOffsiteStateCannotBeRead: an unreadable store
// must not render every mirrored target as "no offsite problem".
func TestTargetListFailsLoudlyWhenOffsiteStateCannotBeRead(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	mirrorGroup(t, h)

	resp, _ := h.doList("GET", "/api/v1/fleet/targets")
	require.Equal(t, 200, resp.StatusCode)

	dropTable(t, h, "repo_stats")

	resp, _ = h.doList("GET", "/api/v1/fleet/targets")
	require.Equal(t, 500, resp.StatusCode, "a green row would be a lie, so the page fails instead")
}

// TestDeviceDetailFailsLoudlyWhenTheTargetCannotBeRead: omitting the offsite
// line would read as "this fleet keeps no offsite copy".
func TestDeviceDetailFailsLoudlyWhenTheTargetCannotBeRead(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	id, _ := enrollInto(t, h, mirrorGroup(t, h), "laptop-1")

	resp, _ := h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	require.Equal(t, 200, resp.StatusCode)

	dropTable(t, h, "targets")

	resp, _ = h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	require.Equal(t, 500, resp.StatusCode)
}

// TestOverviewOffsiteUnknownRatherThanGreen: the dashboard is polled every 30 s,
// so it stays up - but it says "unknown", not "0 behind".
func TestOverviewOffsiteUnknownRatherThanGreen(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	enrollInto(t, h, mirrorGroup(t, h), "laptop-1")

	_, body := h.do("GET", "/api/v1/fleet/overview", nil)
	require.Equal(t, false, body["offsite"].(map[string]any)["unknown"])

	dropTable(t, h, "repo_stats")

	resp, body := h.do("GET", "/api/v1/fleet/overview", nil)
	require.Equal(t, 200, resp.StatusCode, "a polled dashboard stays up")

	off := body["offsite"].(map[string]any)
	require.Equal(t, true, off["unknown"])
	require.Equal(t, float64(1), off["targets_with_mirror"], "the target count needs no stats")
	require.Equal(t, float64(0), off["stale_devices"], "meaningless while unknown, and the UI hides it")
}

// TestOffsiteIgnoresRevokedDevices: a revoked device is not part of "protected
// right now", so it can neither hold a target back nor be counted as behind.
func TestOffsiteIgnoresRevokedDevices(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	gid := mirrorGroup(t, h)
	liveID, _ := enrollInto(t, h, gid, "laptop-1")
	goneID, _ := enrollInto(t, h, gid, "retired")

	st := openStore(t, h)
	ctx := context.Background()
	require.NoError(t, st.SetSetting(ctx, "mirror_interval", "300"))

	now := time.Now().UTC().Truncate(time.Second)
	h.s.SetNowForTesting(func() time.Time { return now })
	// Only the live device is mirrored; the other has no stats row at all.
	require.NoError(t, st.SetMirrored(ctx, liveID, now.Add(-time.Minute), 1))

	_, body := h.do("GET", "/api/v1/fleet/overview", nil)
	require.Equal(t, float64(1), body["offsite"].(map[string]any)["stale_devices"], "both count while both are live")

	resp, _ := h.do("POST", "/api/v1/fleet/agents/"+goneID+"/revoke", nil)
	require.Equal(t, 204, resp.StatusCode)

	_, body = h.do("GET", "/api/v1/fleet/overview", nil)
	require.Equal(t, float64(0), body["offsite"].(map[string]any)["stale_devices"])

	_, list := h.doList("GET", "/api/v1/fleet/targets")
	require.NotContains(t, list[0], "mirror_stale", "the revoked device no longer holds the target back")
}
