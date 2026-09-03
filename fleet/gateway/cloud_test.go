package gateway

import (
	"crypto/md5" //nolint:gosec // S3 defines ETag as MD5; it is an identifier here, not a security control.
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
)

const (
	testBucket = "warphold"
	testPrefix = "wh/"
)

// fakeS3 is a minimal S3-compatible server: PUT (with the If-None-Match: *
// precondition), GET (ranged), HEAD, DELETE, ListObjectsV2 and
// GetBucketVersioning. It is deliberately unauthenticated - signatures are the
// gateway's own concern (Task 3), not this backend's.
type fakeS3 struct {
	mu    sync.Mutex
	objs  map[string][]byte
	times map[string]time.Time

	// ignorePrecondition makes the server accept a second conditional PUT, the
	// way a store that does not implement If-None-Match would.
	ignorePrecondition bool

	// conditionalPuts counts the PUTs that carried If-None-Match: *.
	conditionalPuts int

	// putsWithoutMD5 and multipartCalls must both stay zero: every PUT this
	// backend makes is a single, checksummed request.
	putsWithoutMD5 int
	multipartCalls int

	// putPaths records every key a PUT addressed, root prefix included.
	putPaths []string

	versioning string
}

// stored returns a copy of the fake's objects, so a test never races the
// server goroutine that serves the requests.
func (f *fakeS3) stored() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make(map[string][]byte, len(f.objs))
	for k, v := range f.objs {
		out[k] = v
	}

	return out
}

// inject writes an object behind the backend's back.
func (f *fakeS3) inject(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.objs[key] = []byte(body)
	f.times[key] = time.Now().UTC().Truncate(time.Second)
}

// paths returns the keys every PUT addressed.
func (f *fakeS3) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.putPaths...)
}

// counts reports the request-shape counters.
func (f *fakeS3) counts() (conditional, withoutMD5, multipart int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.conditionalPuts, f.putsWithoutMD5, f.multipartCalls
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != testBucket {
		http.Error(w, "", http.StatusNotFound)

		return
	}

	// This backend never uploads in parts: a multipart ETag is not an MD5.
	if q := r.URL.Query(); q.Has("uploads") || q.Has("uploadId") {
		f.multipartCalls++

		http.Error(w, "", http.StatusBadRequest)

		return
	}

	if key == "" {
		f.bucketOp(w, r)

		return
	}

	switch r.Method {
	case http.MethodPut:
		f.put(w, r, key)
	case http.MethodGet, http.MethodHead:
		f.get(w, r, key)
	case http.MethodDelete:
		// S3 answers 204 whether or not the key was there.
		delete(f.objs, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "", http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) put(w http.ResponseWriter, r *http.Request, key string) {
	f.putPaths = append(f.putPaths, key)

	if r.Header.Get("Content-Md5") == "" {
		f.putsWithoutMD5++
	}

	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if inm != "*" {
			http.Error(w, "", http.StatusBadRequest)

			return
		}

		f.conditionalPuts++

		if _, ok := f.objs[key]; ok && !f.ignorePrecondition {
			// The real S3 error body, so the client's XML branch is what maps
			// this to 412 rather than its bare-status fallback.
			w.WriteHeader(http.StatusPreconditionFailed)
			writeXML(w, struct {
				XMLName xml.Name `xml:"Error"`
				Code    string
				Message string
				Key     string
			}{Code: "PreconditionFailed", Message: "At least one of the pre-conditions you specified did not hold", Key: key})

			return
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)

		return
	}

	f.objs[key] = body
	f.times[key] = time.Now().UTC().Truncate(time.Second)

	w.Header().Set("ETag", etagOf(body))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) get(w http.ResponseWriter, r *http.Request, key string) {
	body, ok := f.objs[key]
	if !ok {
		http.Error(w, "", http.StatusNotFound)

		return
	}

	w.Header().Set("ETag", etagOf(body))
	w.Header().Set("Last-Modified", f.times[key].Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")

	status := http.StatusOK

	if rng := r.Header.Get("Range"); rng != "" && r.Method == http.MethodGet {
		var first, last int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &first, &last); err != nil {
			http.Error(w, "", http.StatusRequestedRangeNotSatisfiable)

			return
		}

		if first >= int64(len(body)) || last >= int64(len(body)) || first > last {
			http.Error(w, "", http.StatusRequestedRangeNotSatisfiable)

			return
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(body)))

		body, status = body[first:last+1], http.StatusPartialContent
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)

	if r.Method == http.MethodGet {
		w.Write(body) //nolint:errcheck // test server
	}
}

// bucketOp serves GetBucketVersioning and ListObjectsV2.
func (f *fakeS3) bucketOp(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if q.Has("versioning") {
		w.Header().Set("Content-Type", "application/xml")
		writeXML(w, struct {
			XMLName xml.Name `xml:"VersioningConfiguration"`
			Status  string
		}{Status: f.versioning})

		return
	}

	keys := make([]string, 0, len(f.objs))
	for k := range f.objs {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	page, _ := strconv.Atoi(q.Get("max-keys"))
	if page <= 0 {
		page = maxKeys
	}

	// A continuation token is just the last key of the previous page.
	after := q.Get("start-after")
	if t := q.Get("continuation-token"); t != "" {
		after = t
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
	}{Name: testBucket, Prefix: q.Get("prefix"), MaxKeys: page}

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
			LastModified: f.times[k].Format("2006-01-02T15:04:05.000Z"),
			ETag:         etagOf(f.objs[k]),
			Size:         int64(len(f.objs[k])),
		})
	}

	w.Header().Set("Content-Type", "application/xml")
	writeXML(w, res)
}

func writeXML(w http.ResponseWriter, v any) {
	xml.NewEncoder(w).Encode(v) //nolint:errcheck // test server
}

func etagOf(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // ETag, not a security control.

	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// newStore starts a fake S3 and returns a cloud-direct store over it.
func newStore(t *testing.T, f *fakeS3) (ObjectStore, *fakeS3) {
	t.Helper()

	s, err := NewCloud(t.Context(), testCI(t, f), testPrefix)
	require.NoError(t, err)

	return s, f
}

func testCI(t *testing.T, f *fakeS3) blob.ConnectionInfo {
	t.Helper()

	f.objs, f.times = map[string][]byte{}, map[string]time.Time{}

	// TLS, because that is how the fleet reaches a real bucket - and because
	// over plain HTTP minio-go switches to chunked payload signing, which is
	// not what production sends (spec §14 note 2).
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)

	return blob.ConnectionInfo{Type: "s3", Config: &s3.Options{
		BucketName:      testBucket,
		Endpoint:        strings.TrimPrefix(srv.URL, "https://"),
		DoNotVerifyTLS:  true,
		Region:          "warphold",
		AccessKeyID:     "WHTESTKEY",
		SecretAccessKey: "secret",
	}}
}

func put(t *testing.T, s ObjectStore, key, body string, overwrite bool) (ObjectInfo, error) {
	t.Helper()

	return s.Put(t.Context(), key, strings.NewReader(body), int64(len(body)), overwrite)
}

func read(t *testing.T, s ObjectStore, key string, offset, length int64) string {
	t.Helper()

	r, _, err := s.Get(t.Context(), key, offset, length)
	require.NoError(t, err)

	defer r.Close() //nolint:errcheck // test

	b, err := io.ReadAll(r)
	require.NoError(t, err)

	return string(b)
}

func TestCloudPutIsAppendOnlyAndAtomic(t *testing.T) {
	s, f := newStore(t, &fakeS3{})

	_, err := put(t, s, "dev1/a.blob", "first", false)
	require.NoError(t, err)

	_, err = put(t, s, "dev1/a.blob", "second", false)
	require.ErrorIs(t, err, ErrExists)

	// The provider refused it: 412 off the wire, not a Head this backend did.
	cond, noMD5, multipart := f.counts()
	require.Equal(t, 2, cond, "every non-overwrite Put must carry If-None-Match: *")
	require.Equal(t, "first", read(t, s, "dev1/a.blob", 0, -1))

	_, err = put(t, s, "dev1/a.blob", "third", true)
	require.NoError(t, err)

	cond, noMD5, multipart = f.counts()
	require.Equal(t, 2, cond, "an overwrite must not be conditional")
	require.Equal(t, "third", read(t, s, "dev1/a.blob", 0, -1))

	require.Zero(t, noMD5, "every PUT must carry Content-MD5")
	require.Zero(t, multipart, "the backend must never upload in parts")
}

func TestCloudPutReportsSizeAndETag(t *testing.T) {
	s, _ := newStore(t, &fakeS3{})

	info, err := put(t, s, "dev1/a.blob", "hello", false)
	require.NoError(t, err)
	require.Equal(t, int64(5), info.Size)
	require.Equal(t, "5d41402abc4b2a76b9719d911017c592", info.ETag) // md5("hello")
	require.False(t, info.LastModified.IsZero())

	hi, err := s.Head(t.Context(), "dev1/a.blob")
	require.NoError(t, err)
	require.Equal(t, int64(5), hi.Size)
	require.Equal(t, info.ETag, hi.ETag, "Head reports the provider's own ETag, unquoted")
}

func TestCloudPutRejectsASizeMismatch(t *testing.T) {
	s, f := newStore(t, &fakeS3{})

	_, err := s.Put(t.Context(), "dev1/a.blob", strings.NewReader("hello"), 4, false)
	require.ErrorIs(t, err, ErrIncompleteBody)

	_, err = s.Put(t.Context(), "dev1/b.blob", strings.NewReader("hello"), 6, false)
	require.ErrorIs(t, err, ErrIncompleteBody)

	require.Empty(t, f.stored(), "a body that does not match its declared length stores nothing")

	// An unknown length is accepted (chunked upload) and spooled.
	info, err := s.Put(t.Context(), "dev1/c.blob", strings.NewReader("hello"), -1, false)
	require.NoError(t, err)
	require.Equal(t, int64(5), info.Size)
	require.Equal(t, "hello", read(t, s, "dev1/c.blob", 0, -1))
}

func TestCloudPutSpoolsALargeObject(t *testing.T) {
	s, f := newStore(t, &fakeS3{})

	big := strings.Repeat("warphold", (spoolAbove/8)+1024) // just over the spool threshold

	info, err := put(t, s, "dev1/big.blob", big, false)
	require.NoError(t, err)
	require.Equal(t, int64(len(big)), info.Size)
	require.Greater(t, info.Size, int64(spoolAbove))
	require.Equal(t, big, string(f.stored()[testPrefix+"dev1/big.blob"]))
	require.Equal(t, big[:16], read(t, s, "dev1/big.blob", 0, 16))
}

func TestCloudGetRange(t *testing.T) {
	s, _ := newStore(t, &fakeS3{})

	_, err := put(t, s, "dev1/a.blob", "0123456789", false)
	require.NoError(t, err)

	require.Equal(t, "0123456789", read(t, s, "dev1/a.blob", 0, -1))
	require.Equal(t, "234", read(t, s, "dev1/a.blob", 2, 3))
	require.Equal(t, "6789", read(t, s, "dev1/a.blob", 6, -1))
	// A length past the end is clamped, not an error.
	require.Equal(t, "89", read(t, s, "dev1/a.blob", 8, 1000))

	// The ObjectInfo describes the whole object, not the range.
	r, info, err := s.Get(t.Context(), "dev1/a.blob", 2, 3)
	require.NoError(t, err)
	r.Close() //nolint:errcheck // test
	require.Equal(t, int64(10), info.Size)

	_, _, err = s.Get(t.Context(), "dev1/a.blob", 10, 1)
	require.ErrorIs(t, err, ErrRange)

	_, _, err = s.Get(t.Context(), "dev1/a.blob", -1, 1)
	require.ErrorIs(t, err, ErrRange)

	// A zero-byte object reads cleanly at offset 0.
	_, err = put(t, s, "dev1/empty.blob", "", false)
	require.NoError(t, err)
	require.Empty(t, read(t, s, "dev1/empty.blob", 0, -1))
}

func TestCloudMissingObjectsAreNotFound(t *testing.T) {
	s, _ := newStore(t, &fakeS3{})

	_, err := s.Head(t.Context(), "dev1/nope")
	require.ErrorIs(t, err, ErrNotFound)

	_, _, err = s.Get(t.Context(), "dev1/nope", 0, -1)
	require.ErrorIs(t, err, ErrNotFound)

	// S3 answers 204 for a delete of a key that was never there; the contract
	// owes the handler a 404, so Delete stats first.
	require.ErrorIs(t, s.Delete(t.Context(), "dev1/nope"), ErrNotFound)

	_, err = put(t, s, "dev1/a.blob", "x", false)
	require.NoError(t, err)
	require.NoError(t, s.Delete(t.Context(), "dev1/a.blob"))
	require.ErrorIs(t, s.Delete(t.Context(), "dev1/a.blob"), ErrNotFound)
}

func TestCloudStoresUnderTheFleetPrefix(t *testing.T) {
	s, f := newStore(t, &fakeS3{})

	_, err := put(t, s, "dev1/a.blob", "x", false)
	require.NoError(t, err)

	// <bucket>/<root-prefix>/<device-id>/<key>: the root prefix is on the
	// stored key, so one bucket can hold every device, and it never leaks back
	// out to the caller.
	require.Contains(t, f.stored(), "wh/dev1/a.blob")

	info, err := s.Head(t.Context(), "dev1/a.blob")
	require.NoError(t, err)
	require.Equal(t, "dev1/a.blob", info.Key)
}

func TestCloudListOrdersPaginatesAndConfines(t *testing.T) {
	s, f := newStore(t, &fakeS3{})

	for _, k := range []string{"dev1/c", "dev1/a", "dev1/b", "dev2/a", "dev3/x"} {
		_, err := put(t, s, k, k, false)
		require.NoError(t, err)
	}

	// A blob written by another fleet outside our root prefix is invisible.
	f.inject("elsewhere/dev1/x", "x")

	objs, truncated, err := s.List(t.Context(), "dev1/", "", 0)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []string{"dev1/a", "dev1/b", "dev1/c"}, keys(objs))

	objs, truncated, err = s.List(t.Context(), "dev1/", "", 2)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Equal(t, []string{"dev1/a", "dev1/b"}, keys(objs))

	// after is exclusive and continues where the page ended.
	objs, truncated, err = s.List(t.Context(), "dev1/", "dev1/b", 2)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []string{"dev1/c"}, keys(objs))

	// An empty prefix lists this fleet's objects and nothing else.
	objs, _, err = s.List(t.Context(), "", "", 0)
	require.NoError(t, err)
	require.Equal(t, []string{"dev1/a", "dev1/b", "dev1/c", "dev2/a", "dev3/x"}, keys(objs))

	// prefix is a raw string prefix, as S3 and the local backend define it, so
	// a prefix that stops short of the path boundary reaches a sibling device.
	// Confining a device is the handler's job (spec §4.1.5), not this layer's.
	_, err = put(t, s, "dev10/a", "x", false)
	require.NoError(t, err)

	objs, _, err = s.List(t.Context(), "dev1", "", 0)
	require.NoError(t, err)
	require.Equal(t, []string{"dev1/a", "dev1/b", "dev1/c", "dev10/a"}, keys(objs))
}

func keys(objs []ObjectInfo) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.Key)
	}

	return out
}

func TestCloudRejectsHostileKeys(t *testing.T) {
	s, f := newStore(t, &fakeS3{})

	for _, key := range []string{
		"", "/dev1/a", "dev1/../dev2/a", "dev1/./a", "dev1//a", "dev1/a\x00b",
		"dev1/..\\..\\x", "dev1/a\nb", strings.Repeat("x", MaxKeyLen+1),
		// 1024 bytes on its own, but over the S3 limit with the fleet prefix.
		strings.Repeat("x", MaxKeyLen),
	} {
		_, err := put(t, s, key, "x", false)
		require.ErrorIsf(t, err, ErrBadKey, "Put(%q)", key)

		_, _, err = s.Get(t.Context(), key, 0, -1)
		require.ErrorIsf(t, err, ErrBadKey, "Get(%q)", key)

		_, err = s.Head(t.Context(), key)
		require.ErrorIsf(t, err, ErrBadKey, "Head(%q)", key)

		require.ErrorIsf(t, s.Delete(t.Context(), key), ErrBadKey, "Delete(%q)", key)
	}

	require.Empty(t, f.stored(), "a rejected key must never reach the provider")

	// The probe's own device id is reserved, so a device can never collide
	// with a conditional-write probe.
	_, err := put(t, s, ".warphold-probe/x", "x", false)
	require.ErrorIs(t, err, ErrBadKey)
	require.Empty(t, f.stored())
}

func TestCloudPutIsBounded(t *testing.T) {
	s, f := newStore(t, &fakeS3{})

	// A declared size over the cap is refused before a byte is read.
	_, err := s.Put(t.Context(), "dev1/huge", failReader{}, DefaultMaxObjectSize+1, false)
	require.ErrorIs(t, err, ErrTooLarge)

	// So is an upload of unknown length that runs past it.
	_, err = s.Put(t.Context(), "dev1/endless", endlessReader{}, -1, false)
	require.ErrorIs(t, err, ErrTooLarge)

	require.Empty(t, f.stored(), "an over-sized upload must never reach the provider")
}

// failReader fails the test if it is read at all.
type failReader struct{}

func (failReader) Read([]byte) (int, error) { panic("body read despite an over-sized declared length") }

// endlessReader never ends, standing in for a chunked upload with no length.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) { return len(p), nil }

func TestNewCloudRejectsUnusableConnectionInfo(t *testing.T) {
	ci := testCI(t, &fakeS3{})

	// An unterminated prefix would let this fleet's keys land inside another
	// whose prefix merely starts the same.
	_, err := NewCloud(t.Context(), ci, "wh")
	require.ErrorIs(t, err, ErrBadKey)

	_, err = NewCloud(t.Context(), ci, "../wh/")
	require.ErrorIs(t, err, ErrBadKey)

	// An empty prefix is legal: the bucket root is the fleet root.
	s, err := NewCloud(t.Context(), ci, "")
	require.NoError(t, err)

	_, err = put(t, s, "dev1/a", "x", false)
	require.NoError(t, err)

	// Native B2 has no conditional write, so it cannot back a hosted target.
	_, err = NewCloud(t.Context(), blob.ConnectionInfo{Type: "b2"}, testPrefix)
	require.ErrorContains(t, err, "S3-compatible endpoint")

	_, err = NewCloud(t.Context(), blob.ConnectionInfo{Type: "filesystem"}, testPrefix)
	require.ErrorContains(t, err, "needs an s3 target")

	// A missing admin key is refused rather than tried anonymously.
	_, err = NewCloud(t.Context(), blob.ConnectionInfo{Type: "s3", Config: &s3.Options{BucketName: "b"}}, testPrefix)
	require.ErrorContains(t, err, "admin key")

	for _, tc := range []struct {
		name string
		edit func(*s3.Options)
		want string
	}{
		// Without a region minio guesses, and a guess is a redirect on every
		// request - or the wrong endpoint entirely.
		{"no region", func(o *s3.Options) { o.Region = "" }, "region"},
		// Plaintext would put the fleet's admin key on the wire.
		{"plaintext", func(o *s3.Options) { o.DoNotUseTLS = true }, "plaintext"},
		// A point-in-time view is read-only; a target must be writable.
		{"point in time", func(o *s3.Options) { t := time.Now(); o.PointInTime = &t }, "point-in-time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := *ci.Config.(*s3.Options) //nolint:forcetypeassert // built by testCI
			tc.edit(&opt)

			_, err := NewCloud(t.Context(), blob.ConnectionInfo{Type: "s3", Config: &opt}, testPrefix)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestNewCloudValidatesTheRootPrefix(t *testing.T) {
	ci := testCI(t, &fakeS3{})

	// A root prefix is not an object key: it may be several segments deep, and
	// that is the whole point of validating it separately from NormalizeKey.
	s, err := NewCloud(t.Context(), ci, "wh/fleet7/")
	require.NoError(t, err)

	_, err = put(t, s, "dev1/a", "x", false)
	require.NoError(t, err)

	for _, prefix := range []string{
		"wh",                                     // unterminated
		"/wh/",                                   // absolute
		"wh/../etc/",                             // relative segment
		"wh//x/",                                 // empty segment
		"wh/./x/",                                // relative segment
		"wh\\x/",                                 // Windows separator
		"wh\u0000/",                              // control byte
		strings.Repeat("x", maxRootPrefix) + "/", // over the length cap
	} {
		_, err := NewCloud(t.Context(), ci, prefix)
		require.ErrorIsf(t, err, ErrBadKey, "prefix %q", prefix)
	}
}

func TestCloudVersionedReflectsTheProvider(t *testing.T) {
	on, _ := newStore(t, &fakeS3{versioning: "Enabled"})
	require.True(t, on.Versioned(t.Context()))

	off, _ := newStore(t, &fakeS3{versioning: "Suspended"})
	require.False(t, off.Versioned(t.Context()))

	none, _ := newStore(t, &fakeS3{})
	require.False(t, none.Versioned(t.Context()))
}

func TestProbeConditionalPut(t *testing.T) {
	f := &fakeS3{}
	ci := testCI(t, f)

	require.NoError(t, ProbeConditionalPut(t.Context(), ci, testPrefix))
	require.Empty(t, f.stored(), "the probe object is deleted again")

	cond, noMD5, multipart := f.counts()
	require.Equal(t, 2, cond, "both probe writes are conditional")
	require.Zero(t, noMD5)
	require.Zero(t, multipart)

	// Under the fleet root, so a prefix-scoped IAM key can still write it.
	for _, p := range f.paths() {
		require.True(t, strings.HasPrefix(p, testPrefix+".warphold-probe/"), "probe key %q", p)
	}

	// A store that accepts the second conditional write cannot enforce the
	// append-only rule, so a target must not be created on it.
	ignoring := &fakeS3{ignorePrecondition: true}
	require.ErrorIs(t, ProbeConditionalPut(t.Context(), testCI(t, ignoring), testPrefix), ErrNoConditionalPut)
	require.Empty(t, ignoring.stored(), "the probe object is deleted even when the check fails")
}
