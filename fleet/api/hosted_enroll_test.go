package api_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
)

// mkHostedGroup creates a hosted/disk target rooted at dir and a group on it.
func (h *harness) mkHostedGroup(t *testing.T, dir string) float64 {
	t.Helper()
	_, tg := h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": dir})
	require.NotNil(t, tg["id"], tg)
	_, tp := h.do("POST", "/api/v1/fleet/templates", map[string]any{"name": "Home default", "sources": []string{"~"}, "policy": map[string]any{}})
	_, g := h.do("POST", "/api/v1/fleet/groups", map[string]any{"name": "Laptops", "target_id": tg["id"], "template_id": tp["id"]})
	return g["id"].(float64)
}

// deviceStore opens Kopia's stock S3 backend with exactly the credentials the
// device was handed, pointed at this Fleet's own gateway.
func deviceStore(t *testing.T, connectToken string) blob.Storage {
	t.Helper()
	ci, _, err := repo.DecodeToken(connectToken)
	require.NoError(t, err)
	require.Equal(t, "s3", ci.Type)

	o, ok := ci.Config.(*s3.Options)
	require.True(t, ok)
	require.Equal(t, "warphold", o.BucketName)
	require.True(t, o.DoNotUseTLS, "the harness serves plain http")

	st, err := blob.NewStorage(context.Background(), ci, false)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close(context.Background()) }) //nolint:errcheck
	return st
}

// A hosted enrollment provisions the repository server-side and hands the
// device a gateway credential that can read it back over the gateway; revoking
// the device stops that credential on the next request, not at cache expiry.
func TestHostedEnrollProvisionsAndRevokeDisablesTheGatewayKey(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.activateAndLogin()
	h.setPublicURL()
	root := t.TempDir()
	gid := h.mkHostedGroup(t, root)
	_, tok := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})

	admin := h.jar
	h.jar = nil
	resp, body := h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok["token"], "hostname": "fw16", "os": "linux", "arch": "amd64", "scope": "user"})
	require.Equal(t, 201, resp.StatusCode, body)

	id := body["agent_id"].(string)
	st := deviceStore(t, body["connect_token"].(string))

	// The server-provisioned repository is visible to the device, through the
	// gateway, with the device's own key and nothing else.
	found, err := blob.ListAllBlobs(ctx, st, "kopia.repository")
	require.NoError(t, err)
	require.Len(t, found, 1, "the device must see the repository the server created for it")

	h.jar = admin
	resp, _ = h.do("POST", "/api/v1/fleet/agents/"+id+"/revoke", nil)
	require.Equal(t, 204, resp.StatusCode)

	// Immediately, not in five minutes: the revoke invalidates the key cache
	// that the successful list above had just warmed.
	_, err = blob.ListAllBlobs(ctx, st, "kopia.repository")
	require.Error(t, err, "a revoked device's gateway key must stop working at once")

	// The repository itself stays for the retention window (D6); only the reap
	// job removes it.
	require.FileExists(t, filepath.Join(root, id, "kopia.repository"), "revocation keeps the repository")
	require.NotNil(t, h.s.AgentForTesting(ctx, id).RevokedAt)
}

// Enrollment on a hosted target needs the public URL: it is the S3 endpoint
// the device is told to use, and there is no correct value to invent.
func TestHostedEnrollWithoutPublicURLIs409(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.activateAndLogin()
	gid := h.mkHostedGroup(t, t.TempDir())

	// The token endpoint is gated on public_url too, so the token is issued
	// out of band to reach the enrollment gate behind it.
	tok := h.s.IssueTokenForTesting(ctx, int64(gid))
	h.jar = nil

	resp, body := h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok, "hostname": "fw16", "os": "linux", "arch": "amd64", "scope": "user"})
	require.Equal(t, 409, resp.StatusCode, body)
	require.Contains(t, body["error"], "public URL")
}
