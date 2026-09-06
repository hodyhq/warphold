package api_test

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
)

// getRaw fetches a non-JSON body with the admin jar attached.
func (h *harness) getRaw(path string) (*http.Response, string) {
	h.t.Helper()
	resp, err := http.DefaultClient.Do(h.newRequest("GET", path, nil))
	require.NoError(h.t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	return resp, string(b)
}

// kitCreds pulls the printed read credentials back out of the page, which is
// the only place they exist -- exactly what a human reads off the paper.
var kitCreds = regexp.MustCompile(`--access-key (\S+) --secret-access-key (\S+)`)

func readCredsFrom(t *testing.T, page string) (string, string) {
	t.Helper()
	m := kitCreds.FindStringSubmatch(page)
	require.Len(t, m, 3, "the page must print an --access-key/--secret-access-key pair")
	return m[1], m[2]
}

// storeWithCreds opens Kopia's stock S3 backend against this Fleet's gateway
// using the credentials from the kit rather than the device's own.
func storeWithCreds(t *testing.T, connectToken, akid, secret string) blob.Storage {
	t.Helper()
	ci, _, err := repo.DecodeToken(connectToken)
	require.NoError(t, err)
	o, ok := ci.Config.(*s3.Options)
	require.True(t, ok)
	opts := *o
	opts.AccessKeyID, opts.SecretAccessKey = akid, secret
	st, err := s3.New(context.Background(), &opts, false)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close(context.Background()) }) //nolint:errcheck
	return st
}

// enrollHosted enrols one device on a hosted target and returns its id and
// connect token, with the admin session restored in the jar.
func (h *harness) enrollHosted(t *testing.T) (string, string) {
	t.Helper()
	h.setPublicURL()
	gid := h.mkHostedGroup(t, t.TempDir())
	_, tok := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})
	admin := h.jar
	h.jar = nil
	resp, body := h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok["token"], "hostname": "fw16", "os": "linux", "arch": "amd64", "scope": "user"})
	require.Equal(t, 201, resp.StatusCode, body)
	h.jar = admin
	return body["agent_id"].(string), body["connect_token"].(string)
}

func TestRecoveryKitIsAdminOnlyAndSelfContained(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	id, connect := h.enrollHosted(t)

	_, password, err := repo.DecodeToken(connect)
	require.NoError(t, err)

	admin := h.jar
	h.jar = nil
	resp, _ := h.getRaw("/api/v1/fleet/agents/" + id + "/kit")
	require.Equal(t, 401, resp.StatusCode, "the kit is admin-session only")
	h.jar = admin

	resp, page := h.getRaw("/api/v1/fleet/agents/" + id + "/kit")
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.True(t, strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline"))
	require.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	require.Contains(t, resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'")
	require.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))

	require.Contains(t, page, password, "the repository password is on the page")
	require.Contains(t, page, id+"/", "the device's prefix is on the page")
	require.Contains(t, page, "kopia repository connect s3 --bucket warphold")
	require.Contains(t, page, "kopia snapshot list")
	require.Contains(t, page, "kopia restore")
	require.NotContains(t, strings.ToLower(page), "<script")

	resp, _ = h.getRaw("/api/v1/fleet/agents/nope/kit")
	require.Equal(t, 404, resp.StatusCode)
}

// The key on paper is a second, read-only credential: it reads the device's
// backups over the gateway and cannot write to them.
func TestRecoveryKitMintsAReadOnlyKeyAndReusesIt(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.activateAndLogin()
	id, connect := h.enrollHosted(t)

	ci, _, err := repo.DecodeToken(connect)
	require.NoError(t, err)
	deviceKey := ci.Config.(*s3.Options).AccessKeyID //nolint:forcetypeassert

	_, page := h.getRaw("/api/v1/fleet/agents/" + id + "/kit")
	akid, secret := readCredsFrom(t, page)
	require.NotEqual(t, deviceKey, akid, "the kit must not print the device's writing key")
	require.NotContains(t, page, ci.Config.(*s3.Options).SecretAccessKey) //nolint:forcetypeassert

	st := storeWithCreds(t, connect, akid, secret)
	found, err := blob.ListAllBlobs(ctx, st, "kopia.repository")
	require.NoError(t, err)
	require.Len(t, found, 1, "the printed key reads the device's repository")

	require.Error(t, st.PutBlob(ctx, "phello", gather.FromSlice([]byte("x")), blob.PutOptions{}),
		"the printed key must not be able to write")

	// A second GET reuses the key rather than minting another: the page is
	// stable, and nothing accumulates in device_keys.
	_, again := h.getRaw("/api/v1/fleet/agents/" + id + "/kit")
	akid2, secret2 := readCredsFrom(t, again)
	require.Equal(t, akid, akid2)
	require.Equal(t, secret, secret2)
}

// Regenerating retires the old paper: its key stops working at once.
func TestRecoveryKitRegenerateDisablesTheOldKey(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.activateAndLogin()
	id, connect := h.enrollHosted(t)

	_, page := h.getRaw("/api/v1/fleet/agents/" + id + "/kit")
	oldID, oldSecret := readCredsFrom(t, page)
	old := storeWithCreds(t, connect, oldID, oldSecret)
	_, err := blob.ListAllBlobs(ctx, old, "kopia.repository")
	require.NoError(t, err, "the first kit's key works")

	resp, _ := h.do("POST", "/api/v1/fleet/agents/"+id+"/kit/regenerate", nil)
	require.Equal(t, 204, resp.StatusCode)

	_, page2 := h.getRaw("/api/v1/fleet/agents/" + id + "/kit")
	newID, newSecret := readCredsFrom(t, page2)
	require.NotEqual(t, oldID, newID)

	// The list above warmed the gateway's key cache, so this is invalidation
	// being tested, not the cache TTL expiring.
	_, err = blob.ListAllBlobs(ctx, old, "kopia.repository")
	require.Error(t, err, "the retired kit's key must stop working at once")

	fresh := storeWithCreds(t, connect, newID, newSecret)
	_, err = blob.ListAllBlobs(ctx, fresh, "kopia.repository")
	require.NoError(t, err)

	resp, _ = h.do("POST", "/api/v1/fleet/agents/nope/kit/regenerate", nil)
	require.Equal(t, 404, resp.StatusCode)
}

func TestRecoveryKitAckShowsUpOnTheAgent(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	id, _ := h.enrollHosted(t)

	_, body := h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	require.Nil(t, body["kit_acked_at"], "a fresh device has no acknowledgement")
	_, list := h.doList("GET", "/api/v1/fleet/agents")
	require.Len(t, list, 1)
	require.Nil(t, list[0]["kit_acked_at"])

	resp, _ := h.do("POST", "/api/v1/fleet/agents/"+id+"/kit/ack", nil)
	require.Equal(t, 204, resp.StatusCode)

	_, body = h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	require.NotNil(t, body["kit_acked_at"])
	_, list = h.doList("GET", "/api/v1/fleet/agents")
	require.NotNil(t, list[0]["kit_acked_at"])

	// Re-acking is an update, not a duplicate-key failure.
	resp, _ = h.do("POST", "/api/v1/fleet/agents/"+id+"/kit/ack", nil)
	require.Equal(t, 204, resp.StatusCode)

	resp, _ = h.do("POST", "/api/v1/fleet/agents/nope/kit/ack", nil)
	require.Equal(t, 404, resp.StatusCode)
}

// A filesystem target has no gateway key at all; its kit is the path and the
// password, with upstream's filesystem flag.
func TestRecoveryKitForFilesystemTarget(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	gid := h.mkGroup(t)
	h.setPublicURL()
	_, tok := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})
	admin := h.jar
	h.jar = nil
	resp, body := h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok["token"], "hostname": "nuc", "os": "linux", "arch": "amd64"})
	require.Equal(t, 201, resp.StatusCode, body)
	h.jar = admin

	_, password, err := repo.DecodeToken(body["connect_token"].(string))
	require.NoError(t, err)

	resp, page := h.getRaw("/api/v1/fleet/agents/" + body["agent_id"].(string) + "/kit")
	require.Equal(t, 200, resp.StatusCode)
	require.Contains(t, page, "kopia repository connect filesystem --path ")
	require.Contains(t, page, password)
	require.NotRegexp(t, kitCreds, page, "a filesystem kit has no gateway credentials")
}

// A filesystem or b2 kit carries the target's own reader credential, which
// regenerate cannot rotate. It used to answer 204 anyway, which told an admin
// chasing a printed kit that had walked out that the key was retired when
// nothing had changed. It refuses now.
func TestRecoveryKitRegenerateRefusesANonHostedTarget(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	id, _ := enrollInto(t, h, mirrorGroup(t, h), "laptop-1")

	resp, body := h.do("POST", "/api/v1/fleet/agents/"+id+"/kit/regenerate", nil)
	require.Equal(t, 409, resp.StatusCode)
	require.Contains(t, body["error"], "hosted target")

	// The kit itself is still served -- only the rotation is refused.
	res, page := h.getRaw("/api/v1/fleet/agents/" + id + "/kit")
	require.Equal(t, 200, res.StatusCode)
	require.NotEmpty(t, page)
}
