package gateway_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7/pkg/signer"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/gateway"
)

const (
	testAKID   = "WH7QVXKJ2M4NPRTS6BCD"
	testSecret = "dGhpcy1pcy1hLXRlc3Qtc2VjcmV0LTQwLWNoYXJz"
	testRegion = "warphold"
	testHost   = "fleet.example.com"
	emptySHA   = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	unsigned   = "UNSIGNED-PAYLOAD"
	streaming  = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
)

// lookup resolves the one test key. Keys not in the map — unknown or
// disabled — are misses, which is the whole point: the two are the same case.
func lookup(secrets map[string]string) gateway.SecretLookup {
	return func(_ context.Context, akid string) (string, bool) {
		s, ok := secrets[akid]
		return s, ok
	}
}

func liveLookup() gateway.SecretLookup {
	return lookup(map[string]string{testAKID: testSecret})
}

// signed builds a client request, signs it with minio-go's signer — the exact
// code Kopia's S3 backend uses — writes it to the wire and reads it back as a
// server request, so what Verify sees is what would arrive over a socket.
func signed(t *testing.T, method, uri, payloadHash string, body []byte, extra http.Header) *http.Request {
	t.Helper()

	return signedIn(t, testRegion, method, uri, payloadHash, body, extra)
}

// signedIn is signed with an explicit signing region, which GetBucketLocation
// needs: minio-go signs it with a hardcoded "us-east-1".
func signedIn(t *testing.T, region, method, uri, payloadHash string, body []byte, extra http.Header) *http.Request {
	t.Helper()

	var rc io.Reader
	if body != nil {
		rc = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, uri, rc)
	require.NoError(t, err)

	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	if body != nil {
		req.ContentLength = int64(len(body))
	}

	return roundTrip(t, signer.SignV4(*req, testAKID, testSecret, "", region))
}

// roundTrip serializes a client request and parses it back as a server one.
func roundTrip(t *testing.T, req *http.Request) *http.Request {
	t.Helper()

	var buf bytes.Buffer

	require.NoError(t, req.Write(&buf))

	sr, err := http.ReadRequest(bufio.NewReader(&buf))
	require.NoError(t, err)

	return sr
}

// signedAt returns the X-Amz-Date the signer stamped on the request.
func signedAt(t *testing.T, r *http.Request) time.Time {
	t.Helper()

	ts, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	require.NoError(t, err)

	return ts
}

func verify(t *testing.T, r *http.Request, look gateway.SecretLookup) (string, error) {
	t.Helper()

	return gateway.Verify(context.Background(), r, testRegion, "s3", look, signedAt(t, r))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// The five verbs Kopia's S3 backend actually issues (docs/RECONCILE-append-only.md
// §5.2), each with the payload-hash form that transport produces.
func TestVerifyAcceptsKopiaRequestProfile(t *testing.T) {
	t.Parallel()

	blob := []byte("pack blob contents")

	cases := []struct {
		name        string
		method      string
		uri         string
		payloadHash string
		body        []byte
		extra       http.Header
	}{{
		name:        "PUT over TLS is UNSIGNED-PAYLOAD with Content-Md5",
		method:      http.MethodPut,
		uri:         "https://" + testHost + "/warphold/dev-0001/pf3a1b2c3d4e5f6a7",
		payloadHash: unsigned,
		body:        blob,
		extra:       http.Header{"Content-Type": {"application/x-kopia"}, "Content-Md5": {"cnwyYnJ6QW9NUXBIY0RuVA=="}},
	}, {
		name:        "PUT with a real payload digest",
		method:      http.MethodPut,
		uri:         "https://" + testHost + "/warphold/dev-0001/qf3a1b2c3d4e5f6a7",
		payloadHash: sha256Hex(blob),
		body:        blob,
	}, {
		name:        "GET",
		method:      http.MethodGet,
		uri:         "https://" + testHost + "/warphold/dev-0001/kopia.repository",
		payloadHash: emptySHA,
	}, {
		name:        "GET with a Range",
		method:      http.MethodGet,
		uri:         "https://" + testHost + "/warphold/dev-0001/xn0_1a2b",
		payloadHash: emptySHA,
		extra:       http.Header{"Range": {"bytes=32-70"}},
	}, {
		name:        "HEAD",
		method:      http.MethodHead,
		uri:         "https://" + testHost + "/warphold/dev-0001/s35391b98f24603ba",
		payloadHash: emptySHA,
	}, {
		name:        "DELETE",
		method:      http.MethodDelete,
		uri:         "https://" + testHost + "/warphold/dev-0001/s35391b98f24603ba",
		payloadHash: emptySHA,
	}, {
		name:        "ListObjectsV2 as minio-go sends it",
		method:      http.MethodGet,
		uri:         "https://" + testHost + "/warphold?list-type=2&prefix=dev-0001%2Fxn&delimiter=%2F&encoding-type=url&fetch-owner=true",
		payloadHash: emptySHA,
	}, {
		name:        "repeated signed header, joined with a comma",
		method:      http.MethodGet,
		uri:         "https://" + testHost + "/warphold/dev-0001/kopia.repository",
		payloadHash: emptySHA,
		extra:       http.Header{"X-Amz-Meta-Tag": {"one", "two"}},
	}, {
		name:        "key with non-ASCII characters",
		method:      http.MethodGet,
		uri:         "https://" + testHost + "/warphold/dev-0001/naïve-été",
		payloadHash: emptySHA,
	}, {
		name:        "key with ~, + and a doubly-encoded percent",
		method:      http.MethodGet,
		uri:         "https://" + testHost + "/warphold/dev-0001/a~b+c%2520d",
		payloadHash: emptySHA,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			akid, err := verify(t, signed(t, tc.method, tc.uri, tc.payloadHash, tc.body, tc.extra), liveLookup())
			require.NoError(t, err)
			require.Equal(t, testAKID, akid)
		})
	}
}

// A request the signer produced, then damaged one way at a time.
func TestVerifyRejectsTamperedRequests(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(t *testing.T, r *http.Request) // returns via r
		now    func(signedAt time.Time) time.Time
		look   gateway.SecretLookup
		want   error
	}{{
		name: "flipped signature",
		mutate: func(_ *testing.T, r *http.Request) {
			setAuthField(r, "Signature", strings.Repeat("a", 64))
		},
		want: gateway.ErrSignatureDoesNotMatch,
	}, {
		name:   "different method",
		mutate: func(_ *testing.T, r *http.Request) { r.Method = http.MethodDelete },
		want:   gateway.ErrSignatureDoesNotMatch,
	}, {
		name:   "different path",
		mutate: func(_ *testing.T, r *http.Request) { r.URL.Path = "/warphold/dev-0001/other" },
		want:   gateway.ErrSignatureDoesNotMatch,
	}, {
		name:   "added query parameter",
		mutate: func(_ *testing.T, r *http.Request) { r.URL.RawQuery = "versionId=2" },
		want:   gateway.ErrSignatureDoesNotMatch,
	}, {
		name:   "changed signed header value",
		mutate: func(_ *testing.T, r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		want:   gateway.ErrSignatureDoesNotMatch,
	}, {
		name:   "removed signed header",
		mutate: func(_ *testing.T, r *http.Request) { r.Header.Del("Content-Type") },
		want:   gateway.ErrSignatureDoesNotMatch,
	}, {
		name:   "different Host",
		mutate: func(_ *testing.T, r *http.Request) { r.Host = "evil.example.com" },
		want:   gateway.ErrSignatureDoesNotMatch,
	}, {
		name: "signed 16 minutes in the past",
		now:  func(s time.Time) time.Time { return s.Add(16 * time.Minute) },
		want: gateway.ErrRequestTimeTooSkewed,
	}, {
		name: "signed 16 minutes in the future",
		now:  func(s time.Time) time.Time { return s.Add(-16 * time.Minute) },
		want: gateway.ErrRequestTimeTooSkewed,
	}, {
		name:   "unknown access key id",
		mutate: func(_ *testing.T, r *http.Request) { swapCredentialAKID(r, "WHUNKNOWNKEY00000000") },
		want:   gateway.ErrInvalidAccessKeyID,
	}, {
		name: "disabled access key id",
		look: lookup(map[string]string{}), // a disabled key is a lookup miss
		want: gateway.ErrInvalidAccessKeyID,
	}, {
		name:   "SignedHeaders without host",
		mutate: func(t *testing.T, r *http.Request) { dropSignedHeader(t, r, "host") },
		want:   gateway.ErrMalformedAuthorization,
	}, {
		name:   "SignedHeaders without x-amz-content-sha256",
		mutate: func(t *testing.T, r *http.Request) { dropSignedHeader(t, r, "x-amz-content-sha256") },
		want:   gateway.ErrMalformedAuthorization,
	}, {
		name:   "SignedHeaders without x-amz-date",
		mutate: func(t *testing.T, r *http.Request) { dropSignedHeader(t, r, "x-amz-date") },
		want:   gateway.ErrMalformedAuthorization,
	}, {
		name: "AWS2 algorithm token",
		mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256", "AWS4-HMAC-SHA1", 1))
		},
		want: gateway.ErrMalformedAuthorization,
	}, {
		name:   "no Authorization header at all",
		mutate: func(_ *testing.T, r *http.Request) { r.Header.Del("Authorization") },
		want:   gateway.ErrMalformedAuthorization,
	}, {
		name: "credential scope naming another service",
		mutate: func(_ *testing.T, r *http.Request) {
			setAuthField(r, "Credential", testAKID+"/20260902/"+testRegion+"/sts/aws4_request")
		},
		want: gateway.ErrMalformedAuthorization,
	}, {
		name: "credential scope naming another region",
		mutate: func(_ *testing.T, r *http.Request) {
			setAuthField(r, "Credential", testAKID+"/20260902/us-east-1/s3/aws4_request")
		},
		want: gateway.ErrMalformedAuthorization,
	}, {
		name: "credential scope date disagreeing with X-Amz-Date",
		mutate: func(_ *testing.T, r *http.Request) {
			setAuthField(r, "Credential", testAKID+"/19990101/"+testRegion+"/s3/aws4_request")
		},
		want: gateway.ErrMalformedAuthorization,
	}, {
		name:   "unsigned x-amz-* header added after signing",
		mutate: func(_ *testing.T, r *http.Request) { r.Header.Set("X-Amz-Decoded-Content-Length", "4") },
		want:   gateway.ErrUnsignedAmzHeader,
	}, {
		name:   "mounted under a route prefix",
		mutate: func(_ *testing.T, r *http.Request) { r.URL.Path = "/s3" + r.URL.Path },
		want:   gateway.ErrSignatureDoesNotMatch,
	}, {
		name:   "query string Go cannot parse whole",
		mutate: func(_ *testing.T, r *http.Request) { r.URL.RawQuery = "list-type=2;prefix=dev-0002/" },
		want:   gateway.ErrMalformedQuery,
	}, {
		name:   "unparseable X-Amz-Date",
		mutate: func(_ *testing.T, r *http.Request) { r.Header.Set("X-Amz-Date", "yesterday") },
		want:   gateway.ErrMalformedAuthorization,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := signed(t, http.MethodPut, "https://"+testHost+"/warphold/dev-0001/p1a2b3c4d", unsigned,
				[]byte("blob"), http.Header{"Content-Type": {"application/x-kopia"}})

			at := signedAt(t, r)

			if tc.mutate != nil {
				tc.mutate(t, r)
			}

			now := at
			if tc.now != nil {
				now = tc.now(at)
			}

			look := tc.look
			if look == nil {
				look = liveLookup()
			}

			akid, err := gateway.Verify(context.Background(), r, testRegion, "s3", look, now)
			require.ErrorIs(t, err, tc.want)
			require.Empty(t, akid)
		})
	}
}

// A disabled key and an unknown key must be the same answer: no enumeration.
func TestDisabledAndUnknownKeyAreIndistinguishable(t *testing.T) {
	t.Parallel()

	r := signed(t, http.MethodGet, "https://"+testHost+"/warphold/dev-0001/kopia.repository", emptySHA, nil, nil)

	_, disabled := verify(t, r, lookup(map[string]string{}))

	r2 := signed(t, http.MethodGet, "https://"+testHost+"/warphold/dev-0001/kopia.repository", emptySHA, nil, nil)
	swapCredentialAKID(r2, "WHNEVEREXISTED000000")

	_, unknown := verify(t, r2, liveLookup())

	require.ErrorIs(t, disabled, gateway.ErrInvalidAccessKeyID)
	require.ErrorIs(t, unknown, gateway.ErrInvalidAccessKeyID)
	require.Equal(t, disabled.Error(), unknown.Error())
}

// Streaming chunked signing is classified apart so the edge can answer 501
// rather than 403 — the gateway is always behind TLS, where minio-go sends
// UNSIGNED-PAYLOAD instead (docs/RECONCILE-append-only.md §5.1).
func TestVerifyRejectsStreamingPayloadAs501(t *testing.T) {
	t.Parallel()

	r := signed(t, http.MethodPut, "https://"+testHost+"/warphold/dev-0001/p1a2b3c4d", streaming, []byte("chunked"), nil)

	_, err := verify(t, r, liveLookup())
	require.ErrorIs(t, err, gateway.ErrStreamingUnsupported)
	require.Contains(t, err.Error(), "https://")
}

// The signature covers the path, so a device cannot lift its own signed
// request onto another device's prefix.
func TestSignatureCannotBeReplayedToAnotherDevicePrefix(t *testing.T) {
	t.Parallel()

	r := signed(t, http.MethodGet, "https://"+testHost+"/warphold/devA/x", emptySHA, nil, nil)

	akid, err := verify(t, r, liveLookup())
	require.NoError(t, err)
	require.Equal(t, testAKID, akid)

	replay := signed(t, http.MethodGet, "https://"+testHost+"/warphold/devA/x", emptySHA, nil, nil)
	replay.URL.Path = "/warphold/devB/x"

	_, err = verify(t, replay, liveLookup())
	require.ErrorIs(t, err, gateway.ErrSignatureDoesNotMatch)
}

// panicReader fails the test if the body is touched.
type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("Verify read the request body") }
func (panicReader) Close() error             { return nil }

func TestVerifyNeverReadsTheBody(t *testing.T) {
	t.Parallel()

	r := signed(t, http.MethodPut, "https://"+testHost+"/warphold/dev-0001/p1a2b3c4d", unsigned,
		[]byte("a large blob that must never be buffered"), nil)
	r.Body = panicReader{}

	akid, err := verify(t, r, liveLookup())
	require.NoError(t, err)
	require.Equal(t, testAKID, akid)
}

// GetBucketLocation is the one request minio-go signs with a region other than
// the configured one: bucket-cache.go hardcodes "us-east-1", and sends it
// path-style with a trailing slash and, over TLS, UNSIGNED-PAYLOAD. The
// handler must therefore verify "?location=" against "us-east-1", and nothing
// else against it.
func TestGetBucketLocationIsSignedForUSEast1(t *testing.T) {
	t.Parallel()

	const locationURI = "https://" + testHost + "/warphold/?location="

	r := signedIn(t, "us-east-1", http.MethodGet, locationURI, unsigned, nil, nil)

	akid, err := gateway.Verify(context.Background(), r, "us-east-1", "s3", liveLookup(), signedAt(t, r))
	require.NoError(t, err)
	require.Equal(t, testAKID, akid)

	r2 := signedIn(t, "us-east-1", http.MethodGet, locationURI, unsigned, nil, nil)

	_, err = gateway.Verify(context.Background(), r2, testRegion, "s3", liveLookup(), signedAt(t, r2))
	require.ErrorIs(t, err, gateway.ErrMalformedAuthorization)
}

// A proxy in front of the gateway adds X-Forwarded-* after the client signed,
// so those headers must not break verification — only x-amz-* must be signed.
func TestUnsignedForwardedHeadersAreTolerated(t *testing.T) {
	t.Parallel()

	r := signed(t, http.MethodGet, "https://"+testHost+"/warphold/dev-0001/kopia.repository", emptySHA, nil, nil)
	r.Header.Set("X-Forwarded-Host", "attacker.example")
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	r.Header.Set("X-Forwarded-Proto", "https")

	akid, err := verify(t, r, liveLookup())
	require.NoError(t, err)
	require.Equal(t, testAKID, akid)
	require.Equal(t, testHost, r.Host)
}

// Clock skew against wall-clock times that owe nothing to the request, so a
// bug that compares the date to itself cannot pass.
func TestClockSkewAgainstAbsoluteTimes(t *testing.T) {
	t.Parallel()

	for name, now := range map[string]time.Time{
		"a server clock stuck in 2001":   time.Date(2001, 9, 11, 12, 0, 0, 0, time.UTC),
		"a server clock running to 2099": time.Date(2099, 12, 31, 23, 59, 0, 0, time.UTC),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := signed(t, http.MethodGet, "https://"+testHost+"/warphold/dev-0001/kopia.repository", emptySHA, nil, nil)

			_, err := gateway.Verify(context.Background(), r, testRegion, "s3", liveLookup(), now)
			require.ErrorIs(t, err, gateway.ErrRequestTimeTooSkewed)
		})
	}
}

// The known-answer vector from AWS's own SigV4 documentation ("Example: GET
// Object" with a Range header), signed by AWS rather than by minio-go, so the
// canonicalisation is checked against the specification and not only against
// the client we happen to use.
func TestAWSDocumentationVector(t *testing.T) {
	t.Parallel()

	const (
		docAKID   = "AKIAIOSFODNN7EXAMPLE"
		docSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		docHost   = "examplebucket.s3.amazonaws.com"
		docSig    = "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	)

	raw := "GET /test.txt HTTP/1.1\r\n" +
		"Host: " + docHost + "\r\n" +
		"Range: bytes=0-9\r\n" +
		"x-amz-content-sha256: " + emptySHA + "\r\n" +
		"x-amz-date: 20130524T000000Z\r\n" +
		"Authorization: AWS4-HMAC-SHA256 Credential=" + docAKID + "/20130524/us-east-1/s3/aws4_request," +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,Signature=" + docSig + "\r\n\r\n"

	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	require.NoError(t, err)

	signedTime := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	akid, err := gateway.Verify(context.Background(), r, "us-east-1", "s3",
		lookup(map[string]string{docAKID: docSecret}), signedTime)
	require.NoError(t, err)
	require.Equal(t, docAKID, akid)
}

// setAuthField rewrites one "Key=value" component of the Authorization header.
func setAuthField(r *http.Request, key, value string) {
	parts := strings.Split(r.Header.Get("Authorization"), ", ")
	for i, p := range parts {
		if strings.Contains(p, key+"=") {
			prefix := ""
			if idx := strings.Index(p, key+"="); idx > 0 {
				prefix = p[:idx]
			}

			parts[i] = prefix + key + "=" + value
		}
	}

	r.Header.Set("Authorization", strings.Join(parts, ", "))
}

func swapCredentialAKID(r *http.Request, akid string) {
	auth := r.Header.Get("Authorization")
	r.Header.Set("Authorization", strings.Replace(auth, "Credential="+testAKID, "Credential="+akid, 1))
}

func dropSignedHeader(t *testing.T, r *http.Request, name string) {
	t.Helper()

	auth := r.Header.Get("Authorization")
	require.Contains(t, auth, "SignedHeaders=")

	for _, form := range []string{name + ";", ";" + name} {
		if after := strings.Replace(auth, form, "", 1); after != auth {
			r.Header.Set("Authorization", after)
			require.NotEqual(t, auth, r.Header.Get("Authorization"))

			return
		}
	}

	t.Fatalf("SignedHeaders does not list %q, so the test would pass vacuously", name)
}
