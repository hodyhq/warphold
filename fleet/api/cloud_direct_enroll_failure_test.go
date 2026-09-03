package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
)

// A failed cloud-direct enrollment leaves nothing behind either: the device
// key minted mid-provisioning is unwound by Provisioner.rollback, and the
// enroll handler's own cleanup (agent_endpoints.go) then deletes the agent row
// and, with it, every device_keys row for that id - the same cleanup a failed
// disk-hosted enrollment gets (TestHostedEnrollFailureLeavesNothingBehind).
// Nothing ever reaches the bucket: the fake S3 rejects the very first write
// repo.Initialize attempts, before any bytes are stored, so there is nothing
// under <agent-id>/ for the reap job to collect later.
func TestCloudDirectEnrollFailureLeavesNothingBehind(t *testing.T) {
	ctx := t.Context()

	bucket := &fakeBucket{objs: map[string][]byte{}, rejectPUTStatus: http.StatusBadRequest}
	bucketSrv := httptest.NewTLSServer(bucket)
	t.Cleanup(bucketSrv.Close)

	s := New(t.TempDir())
	t.Cleanup(func() { s.Close() }) //nolint:errcheck // test cleanup

	s.cloudStore = func(ctx context.Context, ci blob.ConnectionInfo, prefix string) (gateway.ObjectStore, error) {
		ci.Config.(*s3.Options).DoNotVerifyTLS = true //nolint:forcetypeassert // cloudStoreFor always builds s3 options

		return gateway.NewCloud(ctx, ci, prefix)
	}

	require.NoError(t, s.Activate(ctx, "seal-me!", "hody@hody.dev", "pw12345678"))

	m := mux.NewRouter()
	s.Mount(m)

	fleetSrv := httptest.NewTLSServer(m)
	t.Cleanup(fleetSrv.Close)

	require.NoError(t, s.store().SetSetting(ctx, publicURLSetting, fleetSrv.URL))

	groupID := seedCloudGroup(ctx, t, s, bucketSrv.URL)
	token := s.IssueTokenForTesting(ctx, groupID)

	before, err := s.store().Agents(ctx)
	require.NoError(t, err)

	in, err := json.Marshal(map[string]string{"token": token, "hostname": "fw16", "os": "linux", "arch": "amd64", "scope": "user"})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fleetSrv.URL+"/api/v1/fleet/enroll", bytes.NewReader(in))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := fleetSrv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, out)
	require.Nil(t, out["agent_id"])

	msg, _ := out["error"].(string)
	require.NotContains(t, msg, "akid", "the target's admin key id must never reach an enroller")
	require.NotContains(t, msg, "secret", "the target's admin secret must never reach an enroller")

	after, err := s.store().Agents(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after, "a failed enrollment must leave no agent row - and none of its device_keys rows, which DeleteAgent removes first")

	require.Empty(t, bucket.objs, "the bucket rejected the first write, so nothing ever landed under any agent's prefix")
}
