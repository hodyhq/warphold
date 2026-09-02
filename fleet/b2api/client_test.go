package b2api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/b2api"
)

func fakeB2(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var calls []map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/b2api/v3/b2_authorize_account":
			u, p, ok := r.BasicAuth()
			if !ok || u != "adminKeyId" || p != "adminKey" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"accountId": "acct1", "authorizationToken": "tok1", "apiInfo": map[string]any{"storageApi": map[string]any{"apiUrl": srv.URL}}})
		case "/b2api/v3/b2_list_buckets", "/b2api/v3/b2_create_key", "/b2api/v3/b2_delete_key":
			if r.Header.Get("Authorization") != "tok1" {
				w.WriteHeader(401)
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			body["_path"] = r.URL.Path
			calls = append(calls, body)
			switch r.URL.Path {
			case "/b2api/v3/b2_list_buckets":
				json.NewEncoder(w).Encode(map[string]any{"buckets": []any{map[string]any{"bucketId": "bkt1", "bucketName": body["bucketName"], "fileLockConfiguration": map[string]any{"value": map[string]any{"isFileLockEnabled": true}}}}})
			case "/b2api/v3/b2_create_key":
				json.NewEncoder(w).Encode(map[string]any{"applicationKeyId": "newKeyId", "applicationKey": "newKey"})
			default:
				json.NewEncoder(w).Encode(map[string]any{})
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestBucketInfoCreateDeleteKey(t *testing.T) {
	srv, calls := fakeB2(t)
	c := b2api.New(srv.Client()).WithBase(srv.URL)
	ctx := context.Background()

	info, err := c.BucketInfo(ctx, "adminKeyId", "adminKey", "hody-backups")
	require.NoError(t, err)
	require.Equal(t, "bkt1", info.ID)
	require.True(t, info.ObjectLockEnabled)

	k, err := c.CreateKey(ctx, "adminKeyId", "adminKey", b2api.KeyRequest{Name: "warphold-ag1-writer", BucketID: "bkt1", NamePrefix: "agents/ag1/", Capabilities: b2api.WriterCaps})
	require.NoError(t, err)
	require.Equal(t, b2api.CreatedKey{KeyID: "newKeyId", Key: "newKey"}, k)
	created := (*calls)[1]
	require.Equal(t, "/b2api/v3/b2_create_key", created["_path"])
	require.Equal(t, "agents/ag1/", created["namePrefix"])
	require.NotContains(t, created["capabilities"], "deleteFiles")

	require.NoError(t, c.DeleteKey(ctx, "adminKeyId", "adminKey", "newKeyId"))
	require.Equal(t, "newKeyId", (*calls)[2]["applicationKeyId"])

	_, err = c.BucketInfo(ctx, "bad", "creds", "hody-backups")
	require.Error(t, err)
}
