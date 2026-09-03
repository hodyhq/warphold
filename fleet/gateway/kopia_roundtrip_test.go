package gateway_test

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/restore"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/upload"
)

const repoPassword = "correct-horse-battery-staple"

// tlsFixture is newFixture over an httptest TLS server, which is what a device
// actually reaches: over TLS minio-go sends UNSIGNED-PAYLOAD rather than the
// chunked signing the gateway answers 501 (RECONCILE section 5.1).
func tlsFixture(t *testing.T) (*fixture, *httptest.Server) {
	t.Helper()

	f := newFixture(t, fixtureOpts{})

	// Replace the plain server with a TLS one over the same handler.
	h := f.srv.Config.Handler
	f.srv.Close()

	tlsSrv := httptest.NewTLSServer(h)
	t.Cleanup(tlsSrv.Close)

	f.srv = tlsSrv

	return f, tlsSrv
}

// kopiaStorage connects Kopia's own S3 backend to the gateway, exactly as an
// enrolled device would (spec section 7.1 step 3).
func kopiaStorage(ctx context.Context, t *testing.T, srv *httptest.Server, isCreate bool) blob.Storage {
	t.Helper()

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	st, err := s3.New(ctx, &s3.Options{
		BucketName:      gateway.BucketName,
		Prefix:          devA + "/",
		Endpoint:        u.Host,
		DoNotVerifyTLS:  true,
		AccessKeyID:     akidA,
		SecretAccessKey: testSecret,
		Region:          testRegion,
	}, isCreate)
	require.NoError(t, err)

	t.Cleanup(func() { st.Close(ctx) }) //nolint:errcheck // test cleanup

	return st
}

// TestKopiaBlobStorageRoundTrip drives Kopia's S3 backend against the gateway:
// the blob.Storage contract, without a repository on top of it.
func TestKopiaBlobStorageRoundTrip(t *testing.T) {
	ctx := testlogging.Context(t)
	_, srv := tlsFixture(t)
	st := kopiaStorage(ctx, t, srv, true)

	payload := gather.FromSlice([]byte("kopia blob payload"))

	require.NoError(t, st.PutBlob(ctx, blob.ID(packKey), payload, blob.PutOptions{}))

	var got gather.WriteBuffer
	defer got.Close()

	require.NoError(t, st.GetBlob(ctx, blob.ID(packKey), 0, -1, &got))
	require.Equal(t, "kopia blob payload", string(got.ToByteSlice()))

	got.Reset()
	require.NoError(t, st.GetBlob(ctx, blob.ID(packKey), 6, 4, &got))
	require.Equal(t, "blob", string(got.ToByteSlice()))

	meta, err := st.GetMetadata(ctx, blob.ID(packKey))
	require.NoError(t, err)
	require.EqualValues(t, len(payload.ToByteSlice()), meta.Length)

	var listed []blob.ID

	require.NoError(t, st.ListBlobs(ctx, "", func(m blob.Metadata) error {
		listed = append(listed, m.BlobID)
		return nil
	}))
	require.Equal(t, []blob.ID{blob.ID(packKey)}, listed)

	// The allowlist: a session marker may be deleted, a pack blob may not.
	require.NoError(t, st.PutBlob(ctx, blob.ID(sessionKey), gather.FromSlice([]byte("marker")), blob.PutOptions{}))
	require.NoError(t, st.DeleteBlob(ctx, blob.ID(sessionKey)))
	require.Error(t, st.DeleteBlob(ctx, blob.ID(packKey)), "deleting a pack blob must fail")

	// And the object survives the denied delete.
	got.Reset()
	require.NoError(t, st.GetBlob(ctx, blob.ID(packKey), 0, -1, &got))
	require.Equal(t, "kopia blob payload", string(got.ToByteSlice()))
}

// TestKopiaRepositoryRoundTrip is the D4 spike promoted to a regression test:
// stock Kopia creates a repository through the gateway, snapshots a tree, lists
// it and restores it, and every request the gateway served was allowed.
func TestKopiaRepositoryRoundTrip(t *testing.T) {
	ctx := testlogging.Context(t)
	f, srv := tlsFixture(t)

	st := kopiaStorage(ctx, t, srv, true)

	require.NoError(t, repo.Initialize(ctx, st, &repo.NewRepositoryOptions{}, repoPassword))

	dir := t.TempDir()
	configFile := filepath.Join(dir, "repo.config")

	require.NoError(t, repo.Connect(ctx, configFile, st, repoPassword, &repo.ConnectOptions{
		CachingOptions: content.CachingOptions{CacheDirectory: filepath.Join(dir, "cache")},
	}))

	source := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(filepath.Join(source, "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "hello.txt"), []byte("hello\nmore\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "nested", "big.bin"), make([]byte, 3<<20), 0o600))

	// Hosted provisioning gives maintenance to the Fleet identity, which is what
	// removes the only overwrite Kopia otherwise issues (RECONCILE section 4.1).
	rep, err := repo.Open(ctx, configFile, repoPassword, &repo.Options{})
	require.NoError(t, err)

	require.NoError(t, repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "maintenance owner"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			p := maintenance.DefaultParams()
			p.Owner = "fleet@warphold"

			return maintenance.SetParams(ctx, w, &p)
		}))

	si := snapshot.SourceInfo{Host: "device", UserName: "user", Path: source}

	var manifestID manifest.ID

	require.NoError(t, repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "snapshot"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			entry, err := localfs.NewEntry(source)
			if err != nil {
				return err
			}

			tree, err := policy.TreeForSource(ctx, w, si)
			if err != nil {
				return err
			}

			man, err := upload.NewUploader(w).Upload(ctx, entry, tree, si)
			if err != nil {
				return err
			}

			id, err := snapshot.SaveSnapshot(ctx, w, man)
			manifestID = id

			return err
		}))

	require.NoError(t, rep.Close(ctx))

	// Re-open, list and restore -- a fresh connection, so nothing is served
	// from the write session's memory.
	rep, err = repo.Open(ctx, configFile, repoPassword, &repo.Options{})
	require.NoError(t, err)

	defer rep.Close(ctx) //nolint:errcheck // test cleanup

	mans, err := snapshot.ListSnapshots(ctx, rep, si)
	require.NoError(t, err)
	require.Len(t, mans, 1)
	require.Equal(t, manifestID, mans[0].ID)

	root, err := snapshotfs.SnapshotRoot(rep, mans[0])
	require.NoError(t, err)

	target := filepath.Join(dir, "restored")
	out := &restore.FilesystemOutput{TargetPath: target}
	require.NoError(t, out.Init(ctx))

	_, err = restore.Entry(ctx, rep, out, root, restore.Options{RestoreDirEntryAtDepth: math.MaxInt32})
	require.NoError(t, err)

	restored, err := os.ReadFile(filepath.Join(target, "hello.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello\nmore\n", string(restored))

	big, err := os.Stat(filepath.Join(target, "nested", "big.bin"))
	require.NoError(t, err)
	require.EqualValues(t, 3<<20, big.Size())

	assertNoDeniedRequests(t, f)
}

// assertNoDeniedRequests fails if the gateway refused anything stock Kopia
// asked for. 404 is expected and normal (.storageconfig, and every
// create-only probe).
func assertNoDeniedRequests(t *testing.T, f *fixture) {
	t.Helper()

	entries := f.logs.all()
	require.NotEmpty(t, entries, "nothing reached the gateway")

	var denied []gateway.LogEntry

	sessionDeletes := 0

	for _, e := range entries {
		if e.Status >= http.StatusBadRequest && e.Status != http.StatusNotFound {
			denied = append(denied, e)
		}

		if e.Method == http.MethodDelete && e.Status == http.StatusNoContent {
			sessionDeletes++

			require.Equal(t, "s", e.Class, "only session markers may be deleted")
		}
	}

	require.Empty(t, denied, "the gateway denied a request stock Kopia needed: %+v", denied)
	require.Positive(t, sessionDeletes, "the session-marker delete allowlist was never exercised")
}
