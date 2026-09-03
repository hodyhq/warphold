package api

import (
	"context"
	"crypto/md5" //nolint:gosec // S3 ETags are MD5; this is a fake bucket, not a security boundary.
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
)

const fakeBucketName = "warphold-hosted"

// fakeBucket is a minimal S3-compatible bucket: PUT honouring If-None-Match: *,
// HEAD and ranged GET, which is everything the cloud-direct backend issues to
// store a device's blob and read it back. It is unauthenticated on purpose -
// the device's own signature is the gateway's concern and has already been
// checked by the time a request gets this far.
type fakeBucket struct {
	mu   sync.Mutex
	objs map[string][]byte

	// conditional and unconditional count the PUTs that did and did not carry
	// If-None-Match: *. The second must stay zero: that precondition is the
	// append-only guarantee the hosted gateway relies on.
	conditional, unconditional int

	// rejectPUTStatus, when non-zero, fails every PUT with this status before
	// touching objs - simulating a bucket that never accepts the write, so a
	// caller can pin what a mid-provisioning failure leaves behind.
	rejectPUTStatus int
}

// putCounts reports how many PUTs carried the append-only precondition, and
// how many did not.
func (f *fakeBucket) putCounts() (conditional, unconditional int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.conditional, f.unconditional
}

func (f *fakeBucket) stored(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	b, ok := f.objs[key]

	return string(b), ok
}

func (f *fakeBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != fakeBucketName {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	if key == "" {
		f.bucketOp(w, r)
		return
	}

	body, ok := f.objs[key]

	switch r.Method {
	case http.MethodPut:
		if f.rejectPUTStatus != 0 {
			http.Error(w, "", f.rejectPUTStatus)
			return
		}

		if r.Header.Get("If-None-Match") == "*" {
			f.conditional++

			if ok {
				http.Error(w, "", http.StatusPreconditionFailed)
				return
			}
		} else {
			f.unconditional++
		}

		b := make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, b); err != nil {
			http.Error(w, "", http.StatusBadRequest)
			return
		}

		f.objs[key] = b
		w.Header().Set("ETag", `"`+etag(b)+`"`)
		w.WriteHeader(http.StatusOK)

	case http.MethodHead, http.MethodGet:
		if !ok {
			http.Error(w, "", http.StatusNotFound)
			return
		}

		w.Header().Set("ETag", `"`+etag(body)+`"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))

		out, status := body, http.StatusOK

		if rng := r.Header.Get("Range"); rng != "" && r.Method == http.MethodGet {
			var first, last int

			if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &first, &last); err != nil || first > last || last >= len(body) {
				http.Error(w, "", http.StatusRequestedRangeNotSatisfiable)
				return
			}

			out, status = body[first:last+1], http.StatusPartialContent

			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(body)))
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		w.WriteHeader(status)

		if r.Method == http.MethodGet {
			w.Write(out) //nolint:errcheck // test server
		}

	case http.MethodDelete:
		delete(f.objs, key)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "", http.StatusMethodNotAllowed)
	}
}

// bucketOp serves GetBucketVersioning and ListObjectsV2, the two bucket-level
// calls the cloud-direct backend makes. Called with f.mu held.
func (f *fakeBucket) bucketOp(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if q.Has("versioning") {
		w.Header().Set("Content-Type", "application/xml")
		xml.NewEncoder(w).Encode(struct { //nolint:errcheck,errchkjson // test server
			XMLName xml.Name `xml:"VersioningConfiguration"`
			Status  string
		}{})

		return
	}

	keys := make([]string, 0, len(f.objs))
	for k := range f.objs {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	page, _ := strconv.Atoi(q.Get("max-keys"))
	if page <= 0 || page > 1000 {
		page = 1000
	}

	// A continuation token is just the last key of the previous page.
	after := q.Get("start-after")
	if tok := q.Get("continuation-token"); tok != "" {
		after = tok
	}

	type contents struct {
		XMLName      xml.Name `xml:"Contents"`
		Key          string
		LastModified string
		ETag         string
		Size         int64
	}

	res := struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		Name                  string
		Prefix                string
		MaxKeys               int
		IsTruncated           bool
		NextContinuationToken string `xml:",omitempty"`
		Contents              []contents
	}{Name: fakeBucketName, Prefix: q.Get("prefix"), MaxKeys: page}

	for _, k := range keys {
		if !strings.HasPrefix(k, q.Get("prefix")) || k <= after {
			continue
		}

		if len(res.Contents) == page {
			res.IsTruncated, res.NextContinuationToken = true, res.Contents[len(res.Contents)-1].Key

			break
		}

		res.Contents = append(res.Contents, contents{
			Key:          k,
			LastModified: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         `"` + etag(f.objs[k]) + `"`,
			Size:         int64(len(f.objs[k])),
		})
	}

	w.Header().Set("Content-Type", "application/xml")
	xml.NewEncoder(w).Encode(res) //nolint:errcheck,errchkjson // test server
}

func etag(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // S3 ETags are MD5.

	return hex.EncodeToString(sum[:])
}

// A cloud-direct target is served through the device-facing gateway: the device
// signs an S3 request with its own gateway credential, and the bytes land in
// the customer's bucket under the fleet's admin key, written append-only.
//
// The device is seeded rather than enrolled: hosted enrollment still refuses
// storage_mode "cloud" (fleet/enroll.provisionHosted), so this pins the serving
// path the gateway owns, which is what changed.
func TestGatewayServesACloudDirectTarget(t *testing.T) {
	ctx := t.Context()

	bucket := &fakeBucket{objs: map[string][]byte{}}
	bucketSrv := httptest.NewTLSServer(bucket)
	t.Cleanup(bucketSrv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"))
	require.NoError(t, err)

	s := &Server{
		st: st, key: seal.Derive("passphrase", make([]byte, 16)), nowFn: time.Now,
		// The fake bucket serves a self-signed certificate, which is the only
		// thing a test cannot get past; everything else - the minio client, the
		// conditional PUT, the credential unsealing - is the real backend.
		cloudStore: func(ctx context.Context, ci blob.ConnectionInfo, prefix string) (gateway.ObjectStore, error) {
			ci.Config.(*s3.Options).DoNotVerifyTLS = true //nolint:forcetypeassert // cloudStoreFor always builds s3 options

			return gateway.NewCloud(ctx, ci, prefix)
		},
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck // test cleanup

	sealed, err := s.sealCreds(targetCreds{KeyID: "akid", Key: "secret"})
	require.NoError(t, err)

	now := time.Now()

	targetID, err := st.CreateTarget(ctx, &store.Target{
		Name: "cloud", Kind: "hosted", StorageMode: "cloud",
		Bucket: fakeBucketName, Region: "us-east-1",
		Endpoint:       strings.TrimPrefix(bucketSrv.URL, "https://"),
		SealedAdminKey: sealed, CreatedAt: now,
	})
	require.NoError(t, err)

	templateID, err := st.CreateTemplate(ctx, &store.Template{Name: "t", Sources: []string{"~"}, PolicyJSON: []byte("{}"), CreatedAt: now})
	require.NoError(t, err)

	groupID, err := st.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: targetID, TemplateID: templateID, CreatedAt: now})
	require.NoError(t, err)

	const agentID = "ag_cloud1"

	require.NoError(t, st.CreateAgent(ctx, &store.Agent{ID: agentID, Hostname: "fw16", Scope: "user", GroupID: groupID, BearerHash: []byte("hash"), SealedBundle: []byte("bundle"), EnrolledAt: now}))

	akid, secret, err := enroll.NewGatewayCredentials()
	require.NoError(t, err)

	sealedSecret, err := s.key.Seal([]byte(secret))
	require.NoError(t, err)

	require.NoError(t, st.CreateDeviceKey(ctx, &store.DeviceKey{
		AccessKeyID: akid, AgentID: agentID, SealedSecret: sealedSecret, Prefix: agentID + "/", CreatedAt: now,
	}))

	m := mux.NewRouter()
	s.mountGateway(m)

	// TLS, because a device that signs a PUT over plain HTTP switches to
	// streaming payload signing, which the gateway refuses (spec 14 note 2).
	fleetSrv := httptest.NewTLSServer(m)
	t.Cleanup(fleetSrv.Close)

	// Kopia's stock S3 client, with exactly what a device would be handed.
	dev, err := blob.NewStorage(ctx, blob.ConnectionInfo{Type: "s3", Config: &s3.Options{
		BucketName:      gateway.BucketName,
		Prefix:          agentID + "/",
		Endpoint:        strings.TrimPrefix(fleetSrv.URL, "https://"),
		DoNotVerifyTLS:  true,
		AccessKeyID:     akid,
		SecretAccessKey: secret,
		Region:          gateway.DefaultRegion,
	}}, false)
	require.NoError(t, err)

	t.Cleanup(func() { dev.Close(ctx) }) //nolint:errcheck // test cleanup

	const payload = "hello from a cloud-direct target"

	require.NoError(t, dev.PutBlob(ctx, "sblob1", gather.FromSlice([]byte(payload)), blob.PutOptions{}))

	// The bytes are in the customer's bucket, at the flat <device>/<blob> key,
	// and the write carried the append-only precondition.
	got, ok := bucket.stored(agentID + "/sblob1")
	require.True(t, ok, "the blob must reach the bucket")
	require.Equal(t, payload, got)
	require.Positive(t, bucket.conditional, "the write must carry If-None-Match: *")

	var out gather.WriteBuffer
	defer out.Close()

	require.NoError(t, dev.GetBlob(ctx, "sblob1", 0, -1, &out))
	require.Equal(t, payload, string(out.ToByteSlice()))

	// Closing the Server releases the cached backend - the minio client and any
	// spool file with it - and does not deadlock against s.mu.
	require.NoError(t, s.Close())
	require.Empty(t, s.gwDeps.stores)
}
