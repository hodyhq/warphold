package gateway_test

import (
	"context"
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/enroll"
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

// testRootCA is the httptest server's own certificate, PEM-encoded. Handing it
// to the device's client keeps TLS verification on, which is the setting a real
// device runs with; DoNotVerifyTLS would hide a certificate the gateway serves
// wrongly.
func testRootCA(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

// kopiaStorage connects Kopia's own S3 backend to the gateway, exactly as an
// enrolled device would (spec section 7.1 step 3).
func kopiaStorage(ctx context.Context, t *testing.T, srv *httptest.Server, akid, secret string, isCreate bool) blob.Storage {
	t.Helper()

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	st, err := s3.New(ctx, &s3.Options{
		BucketName:      gateway.BucketName,
		Prefix:          devA + "/",
		Endpoint:        u.Host,
		RootCA:          testRootCA(srv),
		AccessKeyID:     akid,
		SecretAccessKey: secret,
		Region:          testRegion,
	}, isCreate)
	require.NoError(t, err)

	t.Cleanup(func() { st.Close(ctx) }) //nolint:errcheck // test cleanup

	return st
}

// --- shared round-trip helpers ---------------------------------------------

// takeSnapshot uploads si.Path and returns the new manifest id.
func takeSnapshot(ctx context.Context, t *testing.T, rep repo.Repository, si snapshot.SourceInfo) manifest.ID {
	t.Helper()

	var id manifest.ID

	require.NoError(t, repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "snapshot"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			entry, err := localfs.NewEntry(si.Path)
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

			id, err = snapshot.SaveSnapshot(ctx, w, man)

			return err
		}))

	return id
}

// restoreSnapshot restores one manifest into target.
func restoreSnapshot(ctx context.Context, t *testing.T, rep repo.Repository, man *snapshot.Manifest, target string) {
	t.Helper()

	root, err := snapshotfs.SnapshotRoot(rep, man)
	require.NoError(t, err)

	out := &restore.FilesystemOutput{TargetPath: target}
	require.NoError(t, out.Init(ctx))

	_, err = restore.Entry(ctx, rep, out, root, restore.Options{RestoreDirEntryAtDepth: math.MaxInt32})
	require.NoError(t, err)
}

// treeDigest maps every regular file's path, relative to root, to the SHA-256
// of its contents: comparing two of these is what "byte-identical" means here.
func treeDigest(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		out[rel] = fmt.Sprintf("%x", sha256.Sum256(b))

		return nil
	}))

	require.NotEmpty(t, out, "the source tree is empty, so the comparison would prove nothing")

	return out
}

// requireBlob asserts a blob reads back with exactly these bytes.
func requireBlob(ctx context.Context, t *testing.T, st blob.Storage, key, want string) {
	t.Helper()

	var got gather.WriteBuffer
	defer got.Close()

	require.NoError(t, st.GetBlob(ctx, blob.ID(key), 0, -1, &got))
	require.Equal(t, want, string(got.ToByteSlice()))
}

// requireS3Error asserts the gateway answered the stock client with exactly
// this status and S3 error code. The pair is the contract: minio-go decides
// whether to retry from the status, and Kopia maps the code.
func requireS3Error(t *testing.T, err error, status int, code string) minio.ErrorResponse {
	t.Helper()

	var e minio.ErrorResponse

	require.ErrorAs(t, err, &e)
	require.Equal(t, status, e.StatusCode)
	require.Equal(t, code, e.Code)

	return e
}

// --- the round trips -------------------------------------------------------

// TestKopiaBlobStorageRoundTrip drives Kopia's S3 backend against the gateway:
// the blob.Storage contract, without a repository on top of it.
func TestKopiaBlobStorageRoundTrip(t *testing.T) {
	ctx := testlogging.Context(t)
	_, srv := tlsFixture(t)
	st := kopiaStorage(ctx, t, srv, akidA, testSecret, true)

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
}

// deviceClient is minio-go -- the very client Kopia's s3 backend drives --
// with the device's credential and the fixture's certificate, but *without*
// Kopia's retrying wrapper on top.
//
// The wrapper is what a device really runs, and it treats the gateway's 403 and
// 409 as retriable: a single append-only denial costs ten attempts and ~22s of
// backoff. That is correct behaviour to keep (the rules test would just take
// three minutes to assert it), so the denials are asserted here, one attempt
// each. See task-9-report.
func deviceClient(t *testing.T, srv *httptest.Server, akid, secret string) *minio.Client {
	t.Helper()

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	tr, ok := srv.Client().Transport.(*http.Transport)
	require.True(t, ok)

	cli, err := minio.New(u.Host, &minio.Options{
		Creds:     credentials.NewStaticV4(akid, secret, ""),
		Secure:    true,
		Region:    testRegion,
		Transport: tr,
	})
	require.NoError(t, err)

	return cli
}

// putObject uploads with exactly the options repo/blob/s3 uses.
func putObject(ctx context.Context, cli *minio.Client, key, body string) error {
	_, err := cli.PutObject(ctx, gateway.BucketName, devA+"/"+key, strings.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: "application/x-kopia", SendContentMd5: true})

	return err
}

// getObject reads a whole object back.
func getObject(ctx context.Context, t *testing.T, cli *minio.Client, key string) string {
	t.Helper()

	o, err := cli.GetObject(ctx, gateway.BucketName, devA+"/"+key, minio.GetObjectOptions{})
	require.NoError(t, err)

	defer o.Close() //nolint:errcheck // test cleanup

	b, err := io.ReadAll(o)
	require.NoError(t, err)

	return string(b)
}

// TestGatewayDeviceRulesThroughTheStockClient pins the append-only contract on
// the path a device actually takes -- minio-go, sigv4, TLS -- rather than the
// hand-rolled requests of handler_test.go, and asserts the exact status and
// error code each rule answers, because that pair is what the client sees.
func TestGatewayDeviceRulesThroughTheStockClient(t *testing.T) {
	ctx := testlogging.Context(t)
	_, srv := tlsFixture(t)

	cli := deviceClient(t, srv, akidA, testSecret)
	// The recovery kit's credential: same device, same prefix, read-only.
	ro := deviceClient(t, srv, akidRO, secretRO)

	const body = "the first write"

	// "xs..." is the single-epoch compaction prefix: an unanchored session
	// match would let it through (RECONCILE section 7.3).
	keptKeys := []string{packKey, "kopia.repository", "xs1234567890abcdef1234"}

	for _, k := range append([]string{sessionKey}, keptKeys...) {
		require.NoError(t, putObject(ctx, cli, k, body))
	}

	t.Run("a second PUT of an existing key is refused and changes nothing", func(t *testing.T) {
		requireS3Error(t, putObject(ctx, cli, packKey, "second write"), http.StatusConflict, "ObjectAlreadyExists")
		require.Equal(t, body, getObject(ctx, t, cli, packKey))
	})

	t.Run("only a session blob may be deleted", func(t *testing.T) {
		require.NoError(t, cli.RemoveObject(ctx, gateway.BucketName, devA+"/"+sessionKey, minio.RemoveObjectOptions{}))

		for _, k := range keptKeys {
			err := cli.RemoveObject(ctx, gateway.BucketName, devA+"/"+k, minio.RemoveObjectOptions{})
			requireS3Error(t, err, http.StatusForbidden, "AppendOnlyDeleteDenied")
			require.Equal(t, body, getObject(ctx, t, cli, k))
		}
	})

	t.Run("a read-only key reads and lists but never writes", func(t *testing.T) {
		require.Equal(t, body, getObject(ctx, t, ro, packKey))

		var listed []string

		for o := range ro.ListObjects(ctx, gateway.BucketName, minio.ListObjectsOptions{Recursive: true}) {
			require.NoError(t, o.Err)

			listed = append(listed, o.Key)
		}

		require.Len(t, listed, len(keptKeys), "a read-only key sees the device's blobs")

		e := requireS3Error(t, putObject(ctx, ro, sessionKey, "nope"), http.StatusForbidden, "AccessDenied")
		require.Equal(t, "this key is read-only", e.Message)

		// DELETE is the one place the two refusals share a branch: a read-only
		// key is refused with the append-only code even for a session marker it
		// would otherwise be allowed to delete. Same 403, and deliberately the
		// same answer a writer key gets, so the mode is not enumerable -- but
		// it is NOT the "this key is read-only" message PUT gives, and this
		// pins that. See task-9-report.
		e = requireS3Error(t,
			ro.RemoveObject(ctx, gateway.BucketName, devA+"/"+sessionKey, minio.RemoveObjectOptions{}),
			http.StatusForbidden, "AppendOnlyDeleteDenied")
		require.Contains(t, e.Message, "only Kopia session markers may be deleted")
	})
}

// TestKopiaRepositoryRoundTrip is the D4 spike promoted to a regression test:
// stock Kopia creates a repository through the gateway, snapshots a tree, lists
// it and restores it, and every request the gateway served was allowed.
func TestKopiaRepositoryRoundTrip(t *testing.T) {
	ctx := testlogging.Context(t)
	f, srv := tlsFixture(t)

	st := kopiaStorage(ctx, t, srv, akidA, testSecret, true)

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
	manifestID := takeSnapshot(ctx, t, rep, si)

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

	target := filepath.Join(dir, "restored")
	restoreSnapshot(ctx, t, rep, mans[0], target)

	require.Equal(t, treeDigest(t, source), treeDigest(t, target))

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

// TestKopiaRepositoryFromServerProvisionedRepo is the other half of the round
// trip: the repository is created by the *Fleet server*, through the hosted
// blob adapter and never over HTTP (spec section 7.1 step 4), and the device
// then reads and writes it with the credential it was enrolled with. If the
// server's on-disk layout ever diverged from the one the gateway serves --
// Kopia's own filesystem provider shards, this adapter does not -- the device
// would not find kopia.repository here.
//
// Two snapshots, because the second is the one that reuses the first's index
// and pack blobs, which is where an append-only violation would surface.
func TestKopiaRepositoryFromServerProvisionedRepo(t *testing.T) {
	ctx := testlogging.Context(t)
	f, srv := tlsFixture(t)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	owner := "fleet@" + u.Hostname()
	p := &enroll.Provisioner{Owner: owner, Store: f.st, SealKey: f.key}

	b, err := p.Provision(ctx, enroll.TargetSpec{
		Kind: "hosted", StorageMode: "disk", HostedRoot: f.root,
		PublicHost: u.Host, TLS: true, Region: testRegion,
	}, devA)
	require.NoError(t, err)

	// The device connects with nothing but its token: no Initialize here, the
	// repository already exists.
	ci, password, err := repo.DecodeToken(b.ConnectToken)
	require.NoError(t, err)
	require.Equal(t, b.Password, password)

	o, ok := ci.Config.(*s3.Options)
	require.True(t, ok)
	o.RootCA = testRootCA(srv) // httptest's certificate is its own root

	st, err := s3.New(ctx, o, false)
	require.NoError(t, err)

	t.Cleanup(func() { st.Close(ctx) }) //nolint:errcheck // test cleanup

	dir := t.TempDir()
	configFile := filepath.Join(dir, "repo.config")

	require.NoError(t, repo.Connect(ctx, configFile, st, password, &repo.ConnectOptions{
		CachingOptions: content.CachingOptions{CacheDirectory: filepath.Join(dir, "cache")},
	}))

	rep, err := repo.Open(ctx, configFile, password, &repo.Options{})
	require.NoError(t, err)

	defer rep.Close(ctx) //nolint:errcheck // test cleanup

	// Provisioning already handed maintenance to Fleet, so the device does not
	// have to (and its engine runs with --no-auto-maintenance anyway).
	params, err := maintenance.GetParams(ctx, rep)
	require.NoError(t, err)
	require.Equal(t, owner, params.Owner)

	source := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(filepath.Join(source, "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "hello.txt"), []byte("hello\nmore\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "nested", "big.bin"), make([]byte, 3<<20), 0o600))

	si := snapshot.SourceInfo{Host: "device", UserName: "user", Path: source}
	first := takeSnapshot(ctx, t, rep, si)

	// A second snapshot over a changed tree, on the same connection.
	require.NoError(t, os.WriteFile(filepath.Join(source, "nested", "second.txt"), []byte("added later\n"), 0o600))

	second := takeSnapshot(ctx, t, rep, si)
	require.NotEqual(t, first, second)

	mans, err := snapshot.ListSnapshots(ctx, rep, si)
	require.NoError(t, err)
	require.Len(t, mans, 2, "snapshot list must show both")
	require.ElementsMatch(t, []manifest.ID{first, second}, []manifest.ID{mans[0].ID, mans[1].ID})

	// The newest snapshot restores byte-identically to what was uploaded.
	latest := mans[0]
	if mans[1].StartTime.After(latest.StartTime) {
		latest = mans[1]
	}

	target := filepath.Join(dir, "restored")
	restoreSnapshot(ctx, t, rep, latest, target)

	require.Equal(t, treeDigest(t, source), treeDigest(t, target))

	assertNoDeniedRequests(t, f)
}
