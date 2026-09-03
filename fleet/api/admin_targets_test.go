package api_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostedTargetDiskMode(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	root := t.TempDir()

	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": root,
	})
	require.Equal(t, 201, resp.StatusCode)
	tid := body["id"].(float64)

	resp, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Equal(t, 200, resp.StatusCode)
	require.Len(t, list, 1)
	require.Equal(t, "hosted", list[0]["kind"])
	require.Equal(t, "disk", list[0]["storage_mode"])
	require.Equal(t, root, list[0]["path"])

	// A group may point at a hosted target like any other kind.
	resp, body = h.do("POST", "/api/v1/fleet/templates", map[string]any{"name": "Home", "sources": []string{"~"}})
	require.Equal(t, 201, resp.StatusCode)
	resp, _ = h.do("POST", "/api/v1/fleet/groups", map[string]any{"name": "Laptops", "target_id": tid, "template_id": body["id"]})
	require.Equal(t, 201, resp.StatusCode)
}

func TestHostedTargetMirrorIsSealedAndNeverEchoed(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	resp, _ := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": t.TempDir(),
		"mirror_kind": "b2", "mirror_bucket": "hody-offsite", "mirror_region": "us-west-004",
		"mirror_key_id": "k", "mirror_key": "s",
	})
	require.Equal(t, 201, resp.StatusCode)

	_, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Equal(t, "b2", list[0]["mirror_kind"])
	require.Equal(t, "hody-offsite", list[0]["mirror_bucket"])
	require.Equal(t, "us-west-004", list[0]["mirror_region"])
	for _, k := range []string{"mirror_key", "mirror_key_id", "sealed_mirror_key", "sealed_admin_key", "key", "key_id"} {
		_, has := list[0][k]
		require.False(t, has, "%s must never leave the server", k)
	}
	// Task 12 verifies the mirror's Object Lock; nothing is verified here.
	_, has := list[0]["mirror_lock_verified_at"]
	require.False(t, has)
}

func TestHostedTargetCloudModeIsStubbed(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "cloud", "kind": "hosted", "storage_mode": "cloud",
		"bucket": "hody-hosted", "key_id": "k", "key": "s",
	})
	require.Equal(t, 501, resp.StatusCode)
	require.Equal(t, "cloud-direct storage lands in a later release", body["error"])

	_, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Empty(t, list, "the stubbed target is not created")
}

func TestHostedTargetRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	missing := filepath.Join(t.TempDir(), "nope")
	notDir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notDir, nil, 0o600))

	for _, tc := range []struct {
		name, want string
		in         map[string]any
	}{
		{"no storage_mode", "storage_mode must be disk or cloud for hosted targets",
			map[string]any{"storage_mode": ""}},
		{"bogus storage_mode", "storage_mode must be disk or cloud for hosted targets",
			map[string]any{"storage_mode": "tape"}},
		{"relative path", "path must be absolute",
			map[string]any{"storage_mode": "disk", "path": "srv/warphold/hosted"}},
		{"missing path", "path does not exist: " + missing,
			map[string]any{"storage_mode": "disk", "path": missing}},
		{"path is a file", "path is not a directory: " + notDir,
			map[string]any{"storage_mode": "disk", "path": notDir}},
		{"bad mirror kind", "mirror_kind must be b2",
			map[string]any{"storage_mode": "disk", "path": os.TempDir(), "mirror_kind": "s3"}},
		{"mirror without credentials", "mirror_bucket, mirror_key_id and mirror_key are required for a b2 mirror",
			map[string]any{"storage_mode": "disk", "path": os.TempDir(), "mirror_kind": "b2", "mirror_bucket": "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := map[string]any{"name": "hosted", "kind": "hosted"}
			for k, v := range tc.in {
				in[k] = v
			}
			resp, body := h.do("POST", "/api/v1/fleet/targets", in)
			require.Equal(t, 400, resp.StatusCode)
			require.Equal(t, tc.want, body["error"])
		})
	}
}

func TestHostedTargetRejectsNonWritablePath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	h := newHarness(t)
	h.activateAndLogin()
	root := filepath.Join(t.TempDir(), "ro")
	require.NoError(t, os.Mkdir(root, 0o500))
	t.Cleanup(func() { os.Chmod(root, 0o700) })

	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": root,
	})
	require.Equal(t, 400, resp.StatusCode)
	require.Equal(t, "path is not writable by the fleet service user: "+root, body["error"])
}
