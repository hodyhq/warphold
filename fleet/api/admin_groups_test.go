package api_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// findGroup returns the group with the given id from a /fleet/groups list body.
func findGroup(list []map[string]any, id float64) map[string]any {
	for _, g := range list {
		if g["id"] == id {
			return g
		}
	}
	return nil
}

func TestGroupUpdate(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	_, tg1 := h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "t1", "kind": "filesystem", "path": t.TempDir()})
	_, tg2 := h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "t2", "kind": "filesystem", "path": t.TempDir()})
	_, tpl1 := h.do("POST", "/api/v1/fleet/templates", map[string]any{"name": "tpl1", "sources": []string{"~"}, "policy": map[string]any{}})
	_, tpl2 := h.do("POST", "/api/v1/fleet/templates", map[string]any{"name": "tpl2", "sources": []string{"~"}, "policy": map[string]any{}})

	mk := func() float64 {
		_, g := h.do("POST", "/api/v1/fleet/groups", map[string]any{"name": "Laptops", "target_id": tg1["id"], "template_id": tpl1["id"]})
		return g["id"].(float64)
	}

	t.Run("rename ok", func(t *testing.T) {
		gid := mk()
		resp, _ := h.do("PUT", "/api/v1/fleet/groups/"+jsonNum(gid), map[string]any{"name": "Renamed"})
		require.Equal(t, 204, resp.StatusCode)
		_, list := h.doList("GET", "/api/v1/fleet/groups")
		require.Equal(t, "Renamed", findGroup(list, gid)["name"])
	})

	t.Run("repoint ok when no devices", func(t *testing.T) {
		gid := mk()
		resp, _ := h.do("PUT", "/api/v1/fleet/groups/"+jsonNum(gid), map[string]any{"target_id": tg2["id"]})
		require.Equal(t, 204, resp.StatusCode)
		_, list := h.doList("GET", "/api/v1/fleet/groups")
		require.Equal(t, tg2["id"], findGroup(list, gid)["target_id"])
	})

	t.Run("template change ok", func(t *testing.T) {
		gid := mk()
		resp, _ := h.do("PUT", "/api/v1/fleet/groups/"+jsonNum(gid), map[string]any{"template_id": tpl2["id"]})
		require.Equal(t, 204, resp.StatusCode)
		_, list := h.doList("GET", "/api/v1/fleet/groups")
		require.Equal(t, tpl2["id"], findGroup(list, gid)["template_id"])
	})

	badInputCases := []struct {
		name string
		body map[string]any
	}{
		{"unknown target 400", map[string]any{"target_id": 999999}},
		{"unknown template 400", map[string]any{"template_id": 999999}},
		{"empty name 400", map[string]any{"name": ""}},
	}
	for _, tc := range badInputCases {
		t.Run(tc.name, func(t *testing.T) {
			gid := mk()
			resp, _ := h.do("PUT", "/api/v1/fleet/groups/"+jsonNum(gid), tc.body)
			require.Equal(t, 400, resp.StatusCode)
		})
	}

	t.Run("unknown id 404", func(t *testing.T) {
		resp, _ := h.do("PUT", "/api/v1/fleet/groups/999999", map[string]any{"name": "x"})
		require.Equal(t, 404, resp.StatusCode)
	})
}

// TestGroupUpdateRefusesRepointWithDevices covers the one case that needs a
// real enrolled device: repointing target_id must be refused once the group
// has a repository living on the old target, even though every other field
// (and target_id left alone) still updates freely.
func TestGroupUpdateRefusesRepointWithDevices(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.setPublicURL()
	gid := h.mkHostedGroup(t, t.TempDir())
	_, tok := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})

	admin := h.jar
	h.jar = nil
	resp, body := h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok["token"], "hostname": "fw16", "os": "linux", "arch": "amd64", "scope": "user"})
	require.Equal(t, 201, resp.StatusCode, body)
	h.jar = admin

	_, tg2 := h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "other", "kind": "hosted", "storage_mode": "disk", "path": t.TempDir()})
	resp, body = h.do("PUT", "/api/v1/fleet/groups/"+jsonNum(gid), map[string]any{"target_id": tg2["id"]})
	require.Equal(t, 409, resp.StatusCode, body)
}

// TestGroupUpdateSameTargetIDWithDevicesIsNotARepoint documents that setting
// target_id to the value the group already has is a no-op as far as the
// repoint guard is concerned: it is not "moving" any device's repository, so
// enrolled devices (including a revoked one) never block it.
func TestGroupUpdateSameTargetIDWithDevicesIsNotARepoint(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.setPublicURL()
	gid := h.mkHostedGroup(t, t.TempDir())
	_, tok := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})

	admin := h.jar
	h.jar = nil
	resp, body := h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok["token"], "hostname": "fw16", "os": "linux", "arch": "amd64", "scope": "user"})
	require.Equal(t, 201, resp.StatusCode, body)
	h.jar = admin

	_, list := h.doList("GET", "/api/v1/fleet/groups")
	sameTarget := findGroup(list, gid)["target_id"]

	resp, body = h.do("PUT", "/api/v1/fleet/groups/"+jsonNum(gid), map[string]any{"target_id": sameTarget})
	require.Equal(t, 204, resp.StatusCode, body)
}

// TestGroupUpdateEmptyOrUnknownFieldsIsANoop documents the PUT convention: an
// empty body, or a body carrying only fields the API doesn't know about,
// changes nothing and still succeeds -- decode() ignores unrecognized JSON
// fields, and every column is left in place by UpdateGroup's COALESCE when
// its pointer is nil.
func TestGroupUpdateEmptyOrUnknownFieldsIsANoop(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	gid := h.mkGroup(t)
	_, before := h.doList("GET", "/api/v1/fleet/groups")

	resp, _ := h.do("PUT", "/api/v1/fleet/groups/"+jsonNum(gid), map[string]any{})
	require.Equal(t, 204, resp.StatusCode)

	resp, _ = h.do("PUT", "/api/v1/fleet/groups/"+jsonNum(gid), map[string]any{"color": "blue", "nonsense": 42})
	require.Equal(t, 204, resp.StatusCode)

	_, after := h.doList("GET", "/api/v1/fleet/groups")
	require.Equal(t, findGroup(before, gid), findGroup(after, gid), "unrecognized/absent fields must not change the row")
}

func TestGroupDelete(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	gid := h.mkGroup(t)

	resp, _ := h.do("DELETE", "/api/v1/fleet/groups/"+jsonNum(gid), nil)
	require.Equal(t, 204, resp.StatusCode)

	resp, _ = h.do("DELETE", "/api/v1/fleet/groups/"+jsonNum(gid), nil)
	require.Equal(t, 404, resp.StatusCode, "already deleted")
}

func TestGroupDeleteRefusedWithAgent(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.setPublicURL()
	gid := h.mkHostedGroup(t, t.TempDir())
	_, tok := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})

	admin := h.jar
	h.jar = nil
	resp, body := h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok["token"], "hostname": "fw16", "os": "linux", "arch": "amd64", "scope": "user"})
	require.Equal(t, 201, resp.StatusCode, body)
	h.jar = admin

	resp, body = h.do("DELETE", "/api/v1/fleet/groups/"+jsonNum(gid), nil)
	require.Equal(t, 409, resp.StatusCode, body)
}

func TestGroupDeleteRefusedWithToken(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.setPublicURL()
	gid := h.mkGroup(t)
	resp, _ := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})
	require.Equal(t, 201, resp.StatusCode)

	resp, body := h.do("DELETE", "/api/v1/fleet/groups/"+jsonNum(gid), nil)
	require.Equal(t, 409, resp.StatusCode, body)
}
