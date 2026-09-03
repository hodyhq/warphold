package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/restore"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/upload"
)

// treeDigest maps every regular file's path, relative to root, to the SHA-256
// of its contents: comparing two of these is what "byte-identical" means.
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

// TestCloudDirectEnrollmentAndKopiaRoundTrip is the cloud-direct half of the
// end-to-end matrix: a device enrolls through the real /enroll endpoint onto a
// hosted target whose bytes live in the customer's own bucket, and then drives
// stock Kopia over the gateway - initialize-free connect, two snapshots, list,
// restore - with every write reaching the bucket append-only.
func TestCloudDirectEnrollmentAndKopiaRoundTrip(t *testing.T) {
	ctx := t.Context()

	bucket := &fakeBucket{objs: map[string][]byte{}}
	bucketSrv := httptest.NewTLSServer(bucket)
	t.Cleanup(bucketSrv.Close)

	s := New(t.TempDir())
	t.Cleanup(func() { s.Close() }) //nolint:errcheck // test cleanup

	// The fake bucket serves a self-signed certificate, which is the only thing
	// a test cannot get past; everything else is the real backend.
	s.cloudStore = func(ctx context.Context, ci blob.ConnectionInfo, prefix string) (gateway.ObjectStore, error) {
		ci.Config.(*s3.Options).DoNotVerifyTLS = true //nolint:forcetypeassert // cloudStoreFor always builds s3 options

		return gateway.NewCloud(ctx, ci, prefix)
	}

	require.NoError(t, s.Activate(ctx, "seal-me!", "hody@hody.dev", "pw12345678"))

	m := mux.NewRouter()
	s.Mount(m)

	// TLS, because a device that signs a PUT over plain HTTP switches to
	// streaming payload signing, which the gateway refuses (spec 14 note 2).
	fleetSrv := httptest.NewTLSServer(m)
	t.Cleanup(fleetSrv.Close)

	require.NoError(t, s.store().SetSetting(ctx, publicURLSetting, fleetSrv.URL))

	groupID := seedCloudGroup(ctx, t, s, bucketSrv.URL)
	token := s.IssueTokenForTesting(ctx, groupID)

	body := enrollDevice(ctx, t, fleetSrv, token)
	require.NotEmpty(t, body["agent_id"])

	// From here on every PUT reaching the bucket is the device's, through the
	// gateway. Provisioning's own writes went through the blob adapter, which
	// is the repository's owner and does not carry the precondition.
	_, unconditionalBefore := bucket.putCounts()

	st := connectedDeviceStore(ctx, t, fleetSrv, body["connect_token"].(string))

	dir := t.TempDir()
	configFile := filepath.Join(dir, "repo.config")
	password := body["connect_token"].(string)

	_, password, err := repo.DecodeToken(password)
	require.NoError(t, err)

	require.NoError(t, repo.Connect(ctx, configFile, st, password, &repo.ConnectOptions{
		CachingOptions: content.CachingOptions{CacheDirectory: filepath.Join(dir, "cache")},
	}))

	rep, err := repo.Open(ctx, configFile, password, &repo.Options{})
	require.NoError(t, err)

	defer rep.Close(ctx) //nolint:errcheck // test cleanup

	// Provisioning handed maintenance to Fleet, so the device never runs it.
	params, err := maintenance.GetParams(ctx, rep)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(params.Owner, "fleet@"), "owner %q", params.Owner)

	source := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(filepath.Join(source, "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "hello.txt"), []byte("hello\nmore\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "nested", "big.bin"), bytes.Repeat([]byte("x"), 1<<20), 0o600))

	si := snapshot.SourceInfo{Host: "device", UserName: "user", Path: source}
	first := cloudSnapshot(ctx, t, rep, si)

	require.NoError(t, os.WriteFile(filepath.Join(source, "nested", "second.txt"), []byte("added later\n"), 0o600))

	second := cloudSnapshot(ctx, t, rep, si)
	require.NotEqual(t, first, second)

	mans, err := snapshot.ListSnapshots(ctx, rep, si)
	require.NoError(t, err)
	require.Len(t, mans, 2, "snapshot list must show both")

	latest := mans[0]
	if mans[1].StartTime.After(latest.StartTime) {
		latest = mans[1]
	}

	root, err := snapshotfs.SnapshotRoot(rep, latest)
	require.NoError(t, err)

	target := filepath.Join(dir, "restored")
	out := &restore.FilesystemOutput{TargetPath: target}
	require.NoError(t, out.Init(ctx))

	_, err = restore.Entry(ctx, rep, out, root, restore.Options{RestoreDirEntryAtDepth: math.MaxInt32})
	require.NoError(t, err)

	require.Equal(t, treeDigest(t, source), treeDigest(t, target))

	// The device's bytes are in the customer's bucket, under the fleet root and
	// the device's own prefix, and every one of its writes was conditional.
	conditional, unconditional := bucket.putCounts()
	require.Equal(t, unconditionalBefore, unconditional, "every device PUT must carry If-None-Match: *")
	require.Positive(t, conditional)

	agentID := body["agent_id"].(string)
	_, ok := bucket.stored(fleetPrefix + agentID + "/kopia.repository")
	require.True(t, ok, "the repository must live under <fleet root>/<agent>/ in the bucket")
}

// seedCloudGroup creates the cloud-direct target, a template and a group on it,
// straight in the store: target creation probes the provider's Object Lock and
// conditional PUT over the admin API, which has tests of its own.
func seedCloudGroup(ctx context.Context, t *testing.T, s *Server, bucketURL string) int64 {
	t.Helper()

	sealed, err := s.sealCreds(targetCreds{KeyID: "akid", Key: "secret"})
	require.NoError(t, err)

	now := time.Now()

	targetID, err := s.store().CreateTarget(ctx, &store.Target{
		Name: "cloud", Kind: "hosted", StorageMode: "cloud",
		Bucket: fakeBucketName, Region: "us-east-1",
		Endpoint:       strings.TrimPrefix(bucketURL, "https://"),
		SealedAdminKey: sealed, CreatedAt: now,
	})
	require.NoError(t, err)

	templateID, err := s.store().CreateTemplate(ctx, &store.Template{Name: "t", Sources: []string{"~"}, PolicyJSON: []byte("{}"), CreatedAt: now})
	require.NoError(t, err)

	groupID, err := s.store().CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: targetID, TemplateID: templateID, CreatedAt: now})
	require.NoError(t, err)

	return groupID
}

// enrollDevice posts to the real /enroll endpoint and returns its response.
func enrollDevice(ctx context.Context, t *testing.T, srv *httptest.Server, token string) map[string]any {
	t.Helper()

	in, err := json.Marshal(map[string]string{"token": token, "hostname": "fw16", "os": "linux", "arch": "amd64", "scope": "user"})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/v1/fleet/enroll", bytes.NewReader(in))
	require.NoError(t, err)

	req.Header.Set("Content-Type", "application/json")

	resp, err := srv.Client().Do(req)
	require.NoError(t, err)

	defer resp.Body.Close() //nolint:errcheck // test cleanup

	var out map[string]any

	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, http.StatusCreated, resp.StatusCode, out)

	return out
}

// connectedDeviceStore opens Kopia's stock S3 backend with exactly what the
// device was handed, trusting the fleet server's own certificate rather than
// skipping verification.
func connectedDeviceStore(ctx context.Context, t *testing.T, srv *httptest.Server, connectToken string) blob.Storage {
	t.Helper()

	ci, _, err := repo.DecodeToken(connectToken)
	require.NoError(t, err)
	require.Equal(t, "s3", ci.Type)

	o, ok := ci.Config.(*s3.Options)
	require.True(t, ok)
	require.False(t, o.DoNotUseTLS, "a hosted device must be told https")

	o.RootCA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	st, err := s3.New(ctx, o, false)
	require.NoError(t, err)

	t.Cleanup(func() { st.Close(ctx) }) //nolint:errcheck // test cleanup

	return st
}

// cloudSnapshot uploads si.Path and returns the new manifest id.
func cloudSnapshot(ctx context.Context, t *testing.T, rep repo.Repository, si snapshot.SourceInfo) string {
	t.Helper()

	var id string

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

			mid, err := snapshot.SaveSnapshot(ctx, w, man)
			id = string(mid)

			return err
		}))

	return id
}
