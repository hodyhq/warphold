package gateway_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content-MD5 is S3's integrity check.
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7/pkg/signer"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/internal/clock"
)

const (
	devA = "agenta"
	devB = "agentb"

	akidA        = testAKID
	akidB        = "WHBBBBBBBBBBBBBBBBBB"
	akidRO       = "WHROROROROROROROROR0"
	akidDisabled = "WHDEADDEADDEADDEADDE"

	secretB  = "dGhpcy1pcy1kZXZpY2UtYnMtc2VjcmV0LTQwLWNo"
	secretRO = "dGhpcy1pcy10aGUtcmVhZC1vbmx5LXNlY3JldC0w"

	// sessionKey is the one blob class the D4 spike proved Kopia must delete.
	sessionKey = "s35391b98f24603bae4bd9a4e4e5408e2-s7cd827313c8b69b0144"
	// packKey stands for every create-only class.
	packKey = "pdeadbeefdeadbeefdeadbeefdeadbeef-s1234567890abcdef1234"
)

// --- fixture ---------------------------------------------------------------

type fixture struct {
	srv  *httptest.Server
	root string
	logs *logSink
	// st and key are the fixture's Fleet store and sealing key, so a test can
	// provision a device into the same store the gateway authenticates against.
	st  *store.Store
	key seal.Key
}

type logSink struct {
	mu      sync.Mutex
	entries []gateway.LogEntry
}

func (l *logSink) add(e gateway.LogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
}

func (l *logSink) all() []gateway.LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]gateway.LogEntry(nil), l.entries...)
}

type fixtureOpts struct {
	now                      func() time.Time
	ratePerSecond, rateBurst float64
	maxObjectBytes           int64
}

func newFixture(t *testing.T, opt fixtureOpts) *fixture {
	t.Helper()

	root := t.TempDir()
	st, key := testStore(t)

	objs, err := gateway.NewLocal(root, gateway.LocalOptions{MaxObjectSize: opt.maxObjectBytes})
	require.NoError(t, err)

	logs := &logSink{}
	gw := gateway.NewGateway(gateway.Config{
		Keys:   gateway.NewKeys(st, key),
		Bucket: gateway.BucketName,
		Region: testRegion,
		Now:    opt.now,
		Log:    logs.add,
		StoreFor: func(context.Context, string) (gateway.ObjectStore, error) {
			return objs, nil
		},
		RatePerSecond:  opt.ratePerSecond,
		RateBurst:      opt.rateBurst,
		MaxObjectBytes: opt.maxObjectBytes,
	})

	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	return &fixture{srv: srv, root: root, logs: logs, st: st, key: key}
}

// testStore builds a Fleet store holding one hosted target, two devices and
// four keys: A, B, a read-only key on A, and a disabled key on A.
func testStore(t *testing.T) (*store.Store, seal.Key) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // test cleanup

	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)

	tid, err := st.CreateTarget(ctx, &store.Target{Name: "hosted", Kind: "hosted", StorageMode: "disk", CreatedAt: now})
	require.NoError(t, err)

	tpl, err := st.CreateTemplate(ctx, &store.Template{Name: "d", Sources: []string{"~"}, PolicyJSON: json.RawMessage(`{}`), CreatedAt: now})
	require.NoError(t, err)

	gid, err := st.CreateGroup(ctx, &store.Group{Name: "g", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	require.NoError(t, err)

	key := seal.Derive("passphrase", []byte("0123456789abcdef"))

	for _, a := range []string{devA, devB} {
		require.NoError(t, st.CreateAgent(ctx, &store.Agent{
			ID: a, Name: a, Hostname: a, OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid,
			BearerHash: []byte(a), SealedBundle: []byte("sealed"), EnrolledAt: now,
		}))
	}

	disabled := now.Add(-time.Hour)

	for _, k := range []struct {
		akid, agent, secret string
		readOnly            bool
		disabledAt          *time.Time
	}{
		{akidA, devA, testSecret, false, nil},
		{akidB, devB, secretB, false, nil},
		{akidRO, devA, secretRO, true, nil},
		{akidDisabled, devA, testSecret, false, &disabled},
	} {
		sealed, err := key.Seal([]byte(k.secret))
		require.NoError(t, err)
		require.NoError(t, st.CreateDeviceKey(ctx, &store.DeviceKey{
			AccessKeyID: k.akid, AgentID: k.agent, SealedSecret: sealed,
			Prefix: k.agent + "/", ReadOnly: k.readOnly, CreatedAt: now, DisabledAt: k.disabledAt,
		}))
	}

	return st, key
}

// --- request helpers -------------------------------------------------------

type call struct {
	akid, secret string
	method, path string
	query        url.Values
	body         []byte
	header       http.Header
	payloadHash  string
	region       string
	unsign       bool        // send without an Authorization header
	tamper       bool        // corrupt the signature after signing
	postSign     http.Header // headers added after signing, so outside it
}

func (f *fixture) do(t *testing.T, c call) *http.Response {
	t.Helper()

	u, err := url.Parse(f.srv.URL)
	require.NoError(t, err)

	u.Path = c.path
	if c.query != nil {
		u.RawQuery = c.query.Encode()
	}

	var body io.Reader
	if c.body != nil {
		body = bytes.NewReader(c.body)
	}

	req, err := http.NewRequestWithContext(t.Context(), c.method, u.String(), body)
	require.NoError(t, err)

	for k, vs := range c.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	if c.body != nil {
		req.ContentLength = int64(len(c.body))
	}

	hash := c.payloadHash
	if hash == "" {
		hash = emptySHA
	}

	req.Header.Set("X-Amz-Content-Sha256", hash)

	if !c.unsign {
		region := c.region
		if region == "" {
			region = testRegion
		}

		req = signer.SignV4(*req, c.akid, c.secret, "", region)

		if c.tamper {
			req.Header.Set("Authorization", strings.TrimSuffix(req.Header.Get("Authorization"), "a")+"b")
		}
	}

	for k, vs := range c.postSign {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := f.srv.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() }) //nolint:errcheck // test cleanup

	return resp
}

// objectPath is the path-style URL of a key in the fixed bucket.
func objectPath(key string) string { return "/" + gateway.BucketName + "/" + key }

func contentMD5(body []byte) string {
	sum := md5.Sum(body) //nolint:gosec // integrity check
	return base64.StdEncoding.EncodeToString(sum[:])
}

// put uploads body as device A would: UNSIGNED-PAYLOAD plus Content-MD5.
func (f *fixture) put(t *testing.T, akid, secret, key string, body []byte) *http.Response {
	t.Helper()

	return f.do(t, call{
		akid: akid, secret: secret, method: http.MethodPut, path: objectPath(key),
		body: body, payloadHash: unsigned,
		header: http.Header{"Content-Md5": {contentMD5(body)}, "Content-Type": {"application/x-kopia"}},
	})
}

func (f *fixture) mustPut(t *testing.T, key string, body []byte) {
	t.Helper()
	require.Equal(t, http.StatusOK, f.put(t, akidA, testSecret, key, body).StatusCode)
}

// errorCode reads the S3 error document's Code.
func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()

	var e struct {
		XMLName xml.Name `xml:"Error"`
		Code    string
		Message string
	}

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, xml.Unmarshal(b, &e), "body: %s", b)

	return e.Code
}

type listResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Name                  string
	Prefix                string
	KeyCount              int
	MaxKeys               int
	Delimiter             string
	EncodingType          string
	IsTruncated           bool
	NextContinuationToken string
	Contents              []struct {
		Key          string
		Size         int64
		LastModified string
		Owner        *struct{ ID string }
	} `xml:"Contents"`
	CommonPrefixes []struct{ Prefix string } `xml:"CommonPrefixes"`
}

func (f *fixture) list(t *testing.T, akid, secret string, q url.Values) (*http.Response, listResult) {
	t.Helper()

	q.Set("list-type", "2")

	resp := f.do(t, call{akid: akid, secret: secret, method: http.MethodGet, path: "/" + gateway.BucketName + "/", query: q})

	var out listResult

	if resp.StatusCode == http.StatusOK {
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, xml.Unmarshal(b, &out), "body: %s", b)
	}

	return resp, out
}

// --- authentication --------------------------------------------------------

func TestGatewayRejectsUnauthenticatedRequests(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	t.Run("unsigned", func(t *testing.T) {
		resp := f.do(t, call{method: http.MethodGet, path: objectPath(devA + "/x"), unsign: true})
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, "AccessDenied", errorCode(t, resp))
	})

	t.Run("wrong signature", func(t *testing.T) {
		resp := f.do(t, call{akid: akidA, secret: "not-the-secret", method: http.MethodGet, path: objectPath(devA + "/x")})
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, "SignatureDoesNotMatch", errorCode(t, resp))
	})

	t.Run("tampered signature", func(t *testing.T) {
		resp := f.do(t, call{akid: akidA, secret: testSecret, method: http.MethodGet, path: objectPath(devA + "/x"), tamper: true})
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, "SignatureDoesNotMatch", errorCode(t, resp))
	})

	t.Run("unknown key", func(t *testing.T) {
		resp := f.do(t, call{akid: "WHZZZZZZZZZZZZZZZZZZ", secret: testSecret, method: http.MethodGet, path: objectPath(devA + "/x")})
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, "InvalidAccessKeyId", errorCode(t, resp))
	})

	t.Run("disabled key is the same as unknown", func(t *testing.T) {
		resp := f.do(t, call{akid: akidDisabled, secret: testSecret, method: http.MethodGet, path: objectPath(devA + "/x")})
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, "InvalidAccessKeyId", errorCode(t, resp))
	})

	t.Run("streaming payload signing", func(t *testing.T) {
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodPut, path: objectPath(devA + "/x"),
			body: []byte("hello"), payloadHash: streaming,
			header: http.Header{"Content-Md5": {contentMD5([]byte("hello"))}},
		})
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
		require.Equal(t, "NotImplemented", errorCode(t, resp))
	})

	t.Run("unsigned x-amz header", func(t *testing.T) {
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodGet, path: objectPath(devA + "/x"),
			postSign: http.Header{"X-Amz-Meta-Injected": {"by-a-proxy"}},
		})
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, "AccessDenied", errorCode(t, resp))
	})

	t.Run("wrong bucket", func(t *testing.T) {
		resp := f.do(t, call{akid: akidA, secret: testSecret, method: http.MethodGet, path: "/other/" + devA + "/x"})
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
		require.Equal(t, "NoSuchBucket", errorCode(t, resp))
	})
}

// --- the append-only rules -------------------------------------------------

func TestGatewayPutIsCreateOnly(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	body := []byte("first write")

	resp := f.put(t, akidA, testSecret, devA+"/"+packKey, body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotEmpty(t, resp.Header.Get("ETag"))

	resp = f.put(t, akidA, testSecret, devA+"/"+packKey, []byte("second write"))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, "AppendOnlyOverwriteDenied", errorCode(t, resp))

	// No header can talk the handler into an overwrite: it passes a constant
	// false to the store, whatever the request carries.
	for _, h := range []http.Header{
		{"X-Amz-Meta-Overwrite": {"true"}},
		{"If-Match": {"*"}},
		{"If-None-Match": {"*"}},
		{"X-Amz-Server-Side-Encryption": {"AES256"}},
		{"Expect": {"100-continue"}},
	} {
		h.Set("Content-Md5", contentMD5([]byte("overwrite attempt")))

		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodPut, path: objectPath(devA + "/" + packKey),
			body: []byte("overwrite attempt"), payloadHash: unsigned, header: h,
		})
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "headers %v", h)
	}

	// The first write survives untouched.
	got := f.do(t, call{akid: akidA, secret: testSecret, method: http.MethodGet, path: objectPath(devA + "/" + packKey)})
	require.Equal(t, http.StatusOK, got.StatusCode)
	b, err := io.ReadAll(got.Body)
	require.NoError(t, err)
	require.Equal(t, body, b)
}

func TestGatewayDeleteAllowlist(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	for _, k := range []string{packKey, sessionKey, "kopia.repository", "xs1234567890abcdef1234", "_log_20260902"} {
		f.mustPut(t, devA+"/"+k, []byte("payload "+k))
	}

	t.Run("session marker is allowed", func(t *testing.T) {
		resp := f.do(t, call{akid: akidA, secret: testSecret, method: http.MethodDelete, path: objectPath(devA + "/" + sessionKey)})
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("deleting a missing session marker is still 204", func(t *testing.T) {
		resp := f.do(t, call{akid: akidA, secret: testSecret, method: http.MethodDelete, path: objectPath(devA + "/" + sessionKey)})
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	// "xs" is the single-epoch compaction prefix: an unanchored "s" match would
	// let it through (RECONCILE section 7.3).
	for _, k := range []string{packKey, "kopia.repository", "xs1234567890abcdef1234", "_log_20260902"} {
		t.Run("denied: "+k, func(t *testing.T) {
			resp := f.do(t, call{akid: akidA, secret: testSecret, method: http.MethodDelete, path: objectPath(devA + "/" + k)})
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
			require.Equal(t, "AppendOnlyDeleteDenied", errorCode(t, resp))

			head := f.do(t, call{akid: akidA, secret: testSecret, method: http.MethodHead, path: objectPath(devA + "/" + k)})
			require.Equal(t, http.StatusOK, head.StatusCode, "the denied object must still be there")
		})
	}
}

// --- prefix confinement ----------------------------------------------------

func TestGatewayConfinesToTheDevicePrefix(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustPut(t, devA+"/"+packKey, []byte("device A's blob"))

	for _, tc := range []struct {
		name, key string
		want      int
	}{
		{"other device", devB + "/" + packKey, http.StatusForbidden},
		{"traversal", "../../etc/passwd", http.StatusForbidden},
		{"prefix as a substring", devA + "-evil/x", http.StatusForbidden},
		{"absolute", "/etc/passwd", http.StatusForbidden},
		{"traversal inside the prefix", devA + "/../" + devB + "/x", http.StatusBadRequest},
		{"nested key", devA + "/sub/blob", http.StatusBadRequest},
		{"reserved temp directory", ".tmp/x", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, m := range []string{http.MethodPut, http.MethodGet, http.MethodHead, http.MethodDelete} {
				resp := f.do(t, call{
					akid: akidA, secret: testSecret, method: m, path: objectPath(tc.key),
					body: bodyFor(m), payloadHash: hashFor(m), header: headerFor(m),
				})
				require.Equal(t, tc.want, resp.StatusCode, "%s %s", m, tc.key)
			}
		})
	}

	t.Run("device B cannot read device A's blob", func(t *testing.T) {
		resp := f.do(t, call{akid: akidB, secret: secretB, method: http.MethodGet, path: objectPath(devA + "/" + packKey)})
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, "AccessDenied", errorCode(t, resp))
	})
}

func bodyFor(method string) []byte {
	if method == http.MethodPut {
		return []byte("x")
	}

	return nil
}

func hashFor(method string) string {
	if method == http.MethodPut {
		return unsigned
	}

	return emptySHA
}

func headerFor(method string) http.Header {
	if method == http.MethodPut {
		return http.Header{"Content-Md5": {contentMD5([]byte("x"))}}
	}

	return nil
}

// --- body integrity and size ----------------------------------------------

func TestGatewayPutBodyChecks(t *testing.T) {
	f := newFixture(t, fixtureOpts{maxObjectBytes: 1024})

	t.Run("missing Content-MD5", func(t *testing.T) {
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodPut, path: objectPath(devA + "/" + packKey),
			body: []byte("hello"), payloadHash: unsigned,
		})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		require.Equal(t, "MissingContentMD5", errorCode(t, resp))
	})

	t.Run("wrong Content-MD5 stores nothing", func(t *testing.T) {
		key := devA + "/" + packKey
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodPut, path: objectPath(key),
			body: []byte("hello"), payloadHash: unsigned,
			header: http.Header{"Content-Md5": {contentMD5([]byte("goodbye"))}},
		})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		require.Equal(t, "BadDigest", errorCode(t, resp))

		head := f.do(t, call{akid: akidA, secret: testSecret, method: http.MethodHead, path: objectPath(key)})
		require.Equal(t, http.StatusNotFound, head.StatusCode, "a rejected body must not be stored")
	})

	t.Run("oversize", func(t *testing.T) {
		big := bytes.Repeat([]byte("z"), 2048)
		resp := f.put(t, akidA, testSecret, devA+"/"+packKey, big)
		require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
		require.Equal(t, "EntityTooLarge", errorCode(t, resp))
	})

	t.Run("read-only key cannot write", func(t *testing.T) {
		resp := f.put(t, akidRO, secretRO, devA+"/"+packKey, []byte("nope"))
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, "AccessDenied", errorCode(t, resp))
	})
}

// --- GET, HEAD and ranges --------------------------------------------------

func TestGatewayGetHeadAndRange(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	body := []byte("0123456789abcdef")
	key := devA + "/" + packKey
	f.mustPut(t, key, body)

	t.Run("whole object", func(t *testing.T) {
		resp := f.do(t, call{akid: akidA, secret: testSecret, method: http.MethodGet, path: objectPath(key)})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, body, b)
		require.Equal(t, strconv.Itoa(len(body)), resp.Header.Get("Content-Length"))
	})

	t.Run("range", func(t *testing.T) {
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodGet, path: objectPath(key),
			header: http.Header{"Range": {"bytes=5-9"}},
		})
		require.Equal(t, http.StatusPartialContent, resp.StatusCode)
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, "56789", string(b))
		require.Equal(t, fmt.Sprintf("bytes 5-9/%d", len(body)), resp.Header.Get("Content-Range"))
	})

	t.Run("open-ended range", func(t *testing.T) {
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodGet, path: objectPath(key),
			header: http.Header{"Range": {"bytes=10-"}},
		})
		require.Equal(t, http.StatusPartialContent, resp.StatusCode)
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, "abcdef", string(b))
	})

	t.Run("zero length probe", func(t *testing.T) {
		// Kopia asks for bytes=0-1 to probe an object without reading it.
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodGet, path: objectPath(key),
			header: http.Header{"Range": {"bytes=0-1"}},
		})
		require.Equal(t, http.StatusPartialContent, resp.StatusCode)
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, "01", string(b))
	})

	t.Run("multi-range", func(t *testing.T) {
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodGet, path: objectPath(key),
			header: http.Header{"Range": {"bytes=0-1,4-5"}},
		})
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
		require.Equal(t, "NotImplemented", errorCode(t, resp))
	})

	t.Run("range past the end", func(t *testing.T) {
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodGet, path: objectPath(key),
			header: http.Header{"Range": {"bytes=999-1000"}},
		})
		require.Equal(t, http.StatusRequestedRangeNotSatisfiable, resp.StatusCode)
	})

	t.Run("head", func(t *testing.T) {
		resp := f.do(t, call{akid: akidA, secret: testSecret, method: http.MethodHead, path: objectPath(key)})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, strconv.Itoa(len(body)), resp.Header.Get("Content-Length"))
		require.NotEmpty(t, resp.Header.Get("ETag"))
		require.NotEmpty(t, resp.Header.Get("Last-Modified"))
	})

	t.Run("missing key", func(t *testing.T) {
		resp := f.do(t, call{akid: akidA, secret: testSecret, method: http.MethodGet, path: objectPath(devA + "/nosuch")})
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
		require.Equal(t, "NoSuchKey", errorCode(t, resp))
	})

	t.Run("read-only key can read", func(t *testing.T) {
		resp := f.do(t, call{akid: akidRO, secret: secretRO, method: http.MethodGet, path: objectPath(key)})
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

// --- ListObjectsV2 ---------------------------------------------------------

func TestGatewayListIsPrefixConfined(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	for i := range 5 {
		f.mustPut(t, fmt.Sprintf("%s/p%02d", devA, i), []byte("a"))
	}

	// Device B's blob, written with device B's own key.
	resp := f.put(t, akidB, secretB, devB+"/p00", []byte("b"))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	t.Run("empty prefix lists only this device", func(t *testing.T) {
		_, res := f.list(t, akidA, testSecret, url.Values{"prefix": {""}})
		require.Equal(t, 5, res.KeyCount)

		for _, c := range res.Contents {
			require.True(t, strings.HasPrefix(c.Key, devA+"/"), "leaked %q", c.Key)
		}
	})

	t.Run("another device's prefix is replaced, not honoured", func(t *testing.T) {
		_, res := f.list(t, akidA, testSecret, url.Values{"prefix": {devB + "/"}})
		require.Equal(t, 5, res.KeyCount)

		for _, c := range res.Contents {
			require.True(t, strings.HasPrefix(c.Key, devA+"/"), "leaked %q", c.Key)
		}
	})

	t.Run("a prefix inside the device is honoured", func(t *testing.T) {
		_, res := f.list(t, akidA, testSecret, url.Values{"prefix": {devA + "/p0"}})
		require.Equal(t, 5, res.KeyCount)
	})

	t.Run("max-keys is capped at 1000", func(t *testing.T) {
		_, res := f.list(t, akidA, testSecret, url.Values{"prefix": {devA + "/"}, "max-keys": {"999999"}})
		require.Equal(t, 1000, res.MaxKeys)
	})

	t.Run("continuation token round-trips", func(t *testing.T) {
		var seen []string

		q := url.Values{"prefix": {devA + "/"}, "max-keys": {"2"}}

		for range 10 {
			_, res := f.list(t, akidA, testSecret, q)
			for _, c := range res.Contents {
				seen = append(seen, c.Key)
			}

			if !res.IsTruncated {
				break
			}

			require.NotEmpty(t, res.NextContinuationToken)
			q.Set("continuation-token", res.NextContinuationToken)
		}

		require.Len(t, seen, 5)
		require.Equal(t, devA+"/p00", seen[0])
		require.Equal(t, devA+"/p04", seen[4])
	})

	t.Run("a token minted for another device is refused", func(t *testing.T) {
		forged := base64.RawURLEncoding.EncodeToString([]byte(devB + "/\x00" + devB + "/p00"))
		resp, _ := f.list(t, akidA, testSecret, url.Values{"prefix": {devA + "/"}, "continuation-token": {forged}})
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, "AccessDenied", errorCode(t, resp))
	})

	t.Run("garbage token is refused", func(t *testing.T) {
		resp, _ := f.list(t, akidA, testSecret, url.Values{"continuation-token": {"!!!not-base64!!!"}})
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("encoding-type and fetch-owner", func(t *testing.T) {
		_, res := f.list(t, akidA, testSecret, url.Values{
			"prefix": {devA + "/"}, "delimiter": {"/"}, "encoding-type": {"url"}, "fetch-owner": {"true"},
		})
		require.Equal(t, "url", res.EncodingType)
		require.Equal(t, url.QueryEscape(devA+"/"), res.Prefix)

		for _, c := range res.Contents {
			require.NotNil(t, c.Owner)
			decoded, err := url.QueryUnescape(c.Key)
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(decoded, devA+"/"))
		}
	})

	t.Run("v1 list is refused", func(t *testing.T) {
		resp := f.do(t, call{akid: akidA, secret: testSecret, method: http.MethodGet, path: "/" + gateway.BucketName + "/"})
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	})
}

func TestGatewayListAcceptsDelimiterOverAFlatKeySpace(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	// minio-go always sends delimiter=/ (RECONCILE section 5.3), but the key
	// space is flat, so nothing can ever roll up into CommonPrefixes.
	for _, k := range []string{"flat", "another", "third"} {
		f.mustPut(t, devA+"/"+k, []byte("x"))
	}

	resp := f.put(t, akidA, testSecret, devA+"/sub/one", []byte("x"))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "a nested key must be refused")
	require.Equal(t, "InvalidObjectKey", errorCode(t, resp))

	_, res := f.list(t, akidA, testSecret, url.Values{"prefix": {devA + "/"}, "delimiter": {"/"}})
	require.Len(t, res.Contents, 3)
	require.Empty(t, res.CommonPrefixes)
	require.Equal(t, 3, res.KeyCount)
	require.False(t, res.IsTruncated)
}

// --- bucket-level operations ----------------------------------------------

func TestGatewayBucketOperations(t *testing.T) {
	f := newFixture(t, fixtureOpts{})

	t.Run("location answers the advertised region", func(t *testing.T) {
		// minio-go hardcodes us-east-1 in this request's signature.
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodGet,
			path: "/" + gateway.BucketName + "/", query: url.Values{"location": {""}}, region: "us-east-1",
		})
		require.Equal(t, http.StatusOK, resp.StatusCode)

		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(b), testRegion)
	})

	t.Run("versioning is 501", func(t *testing.T) {
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodGet,
			path: "/" + gateway.BucketName + "/", query: url.Values{"versioning": {""}},
		})
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
		require.Equal(t, "NotImplemented", errorCode(t, resp))
	})

	t.Run("multipart initiation is 501", func(t *testing.T) {
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodPost,
			path: objectPath(devA + "/" + packKey), query: url.Values{"uploads": {""}},
		})
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	})

	t.Run("bulk delete is 501", func(t *testing.T) {
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodPost,
			path: "/" + gateway.BucketName + "/", query: url.Values{"delete": {""}},
		})
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	})

	t.Run("server-side copy is 501", func(t *testing.T) {
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodPut, path: objectPath(devA + "/copy"),
			header: http.Header{"X-Amz-Copy-Source": {"/warphold/" + devA + "/" + packKey}},
		})
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	})

	t.Run("object retention is 501", func(t *testing.T) {
		resp := f.do(t, call{
			akid: akidA, secret: testSecret, method: http.MethodPut, path: objectPath(devA + "/" + packKey),
			query: url.Values{"retention": {""}}, body: []byte("x"), payloadHash: unsigned,
			header: http.Header{"Content-Md5": {contentMD5([]byte("x"))}},
		})
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	})
}

// --- rate limiting ---------------------------------------------------------

func TestGatewayRateLimitIsPerDevice(t *testing.T) {
	// A frozen clock means the bucket never refills, so the boundary is exact.
	frozen := clock.Now()
	f := newFixture(t, fixtureOpts{now: func() time.Time { return frozen }})

	var throttled int

	for range 250 {
		resp := f.do(t, call{akid: akidA, secret: testSecret, method: http.MethodHead, path: objectPath(devA + "/nosuch")})
		if resp.StatusCode == http.StatusServiceUnavailable {
			throttled++

			require.Equal(t, "1", resp.Header.Get("Retry-After"))
		}
	}

	require.Equal(t, 50, throttled, "burst 200 of 250 requests should pass")

	// A second device has its own bucket.
	resp := f.do(t, call{akid: akidB, secret: secretB, method: http.MethodHead, path: objectPath(devB + "/nosuch")})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// --- the request log -------------------------------------------------------

func TestGatewayLogsWithoutSecrets(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustPut(t, devA+"/"+sessionKey, []byte("marker"))

	entries := f.logs.all()
	require.NotEmpty(t, entries)

	e := entries[len(entries)-1]
	require.Equal(t, devA, e.DeviceID)
	require.Equal(t, akidA, e.AccessKeyID)
	require.Equal(t, http.MethodPut, e.Method)
	require.Equal(t, "s", e.Class, "the log names the blob class, not the content hash")
	require.Equal(t, http.StatusOK, e.Status)
	require.Positive(t, e.Dur)

	// No field of the entry carries the secret.
	blob := fmt.Sprintf("%+v", e)
	require.NotContains(t, blob, testSecret)
	require.NotContains(t, blob, "Signature=")
}
