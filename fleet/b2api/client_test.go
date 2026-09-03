package b2api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
				json.NewEncoder(w).Encode(map[string]any{"buckets": []any{map[string]any{"bucketId": "bkt1", "bucketName": body["bucketName"], "fileLockConfiguration": map[string]any{"isClientAuthorizedToRead": true, "value": map[string]any{"isFileLockEnabled": true}}}}})
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

// B2's error body is upstream text of unbounded size, and it ends up in fleet
// logs and in admin-facing errors, so only its first 512 bytes are quoted.
func TestErrorBodyIsTruncated(t *testing.T) {
	huge := strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/b2api/v3/b2_authorize_account" {
			json.NewEncoder(w).Encode(map[string]any{"accountId": "acct1", "authorizationToken": "tok1", "apiInfo": map[string]any{"storageApi": map[string]any{"apiUrl": "http://" + r.Host}}}) //nolint:errcheck,errchkjson
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(huge)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	_, err := b2api.New(srv.Client()).WithBase(srv.URL).BucketInfo(context.Background(), "adminKeyId", "adminKey", "hody-backups")
	require.Error(t, err)
	require.Contains(t, err.Error(), "b2 returned 500")
	require.Contains(t, err.Error(), "(truncated)")
	require.NotContains(t, err.Error(), strings.Repeat("x", 513))
	require.Less(t, len(err.Error()), 700, "the whole 4 KiB body must not be quoted")
}

func TestBucketInfoCreateDeleteKey(t *testing.T) {
	srv, calls := fakeB2(t)
	c := b2api.New(srv.Client()).WithBase(srv.URL)
	ctx := context.Background()

	info, err := c.BucketInfo(ctx, "adminKeyId", "adminKey", "hody-backups")
	require.NoError(t, err)
	require.Equal(t, "bkt1", info.ID)
	require.True(t, info.ObjectLockEnabled)
	require.True(t, info.LockReadable)

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

// B2 hides fileLockConfiguration.value from a key that is not authorized to
// read it, so isFileLockEnabled decodes as false on a bucket that may well be
// locked. LockReadable is what tells those two apart.
func TestBucketInfoReportsAnUnreadableLockConfiguration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/b2api/v3/b2_authorize_account" {
			json.NewEncoder(w).Encode(map[string]any{"accountId": "acc", "authorizationToken": "tok",
				"apiInfo": map[string]any{"storageApi": map[string]any{"apiUrl": "http://" + r.Host}}})

			return
		}

		json.NewEncoder(w).Encode(map[string]any{"buckets": []any{map[string]any{
			"bucketId": "bkt1", "bucketName": "hody-backups",
			"fileLockConfiguration": map[string]any{"isClientAuthorizedToRead": false, "value": nil},
		}}})
	}))
	t.Cleanup(srv.Close)

	info, err := b2api.New(srv.Client()).WithBase(srv.URL).BucketInfo(context.Background(), "k", "s", "hody-backups")
	require.NoError(t, err)
	require.False(t, info.LockReadable)
	require.False(t, info.ObjectLockEnabled, "unknown decodes as false, which is why LockReadable exists")
}
