package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7/pkg/s3utils"
)

// maxClockSkew is how far a request's X-Amz-Date may be from the server clock.
const maxClockSkew = 15 * time.Minute

const (
	signV4Algorithm  = "AWS4-HMAC-SHA256"
	iso8601Format    = "20060102T150405Z"
	scopeDateFormat  = "20060102"
	scopeTerminator  = "aws4_request"
	streamingPrefix  = "STREAMING-"
	contentSHAHeader = "x-amz-content-sha256"
	dateHeader       = "x-amz-date"
	hostHeader       = "host"
)

// Errors returned by Verify. They map onto S3 error codes at the HTTP edge:
// ErrInvalidAccessKeyID -> 403 InvalidAccessKeyId, ErrSignatureDoesNotMatch ->
// 403 SignatureDoesNotMatch, ErrMalformedAuthorization -> 400
// AuthorizationHeaderMalformed, ErrRequestTimeTooSkewed -> 403
// RequestTimeTooSkewed, ErrStreamingUnsupported -> 501 NotImplemented.
var (
	ErrMalformedAuthorization = errors.New("malformed Authorization header")
	ErrInvalidAccessKeyID     = errors.New("invalid access key id")
	ErrSignatureDoesNotMatch  = errors.New("signature does not match")
	ErrRequestTimeTooSkewed   = errors.New("request time too skewed")

	// ErrUnsignedAmzHeader is an x-amz-* request header the client left out of
	// SignedHeaders. Such a header is outside the signature, so a proxy or an
	// attacker could add one and change how the request is interpreted.
	ErrUnsignedAmzHeader = errors.New("unsigned x-amz-* header")

	// ErrMalformedQuery is a query string url.ParseQuery cannot read whole.
	// Go's URL.Query() would silently drop the unreadable pairs, which would
	// leave them outside the signature; the gateway refuses them instead.
	ErrMalformedQuery = errors.New("malformed query string")

	// ErrStreamingUnsupported is returned for aws-chunked payload signing,
	// which minio-go only uses over plain HTTP. WarpHold's gateway is always
	// reached over TLS, where the client sends UNSIGNED-PAYLOAD instead.
	ErrStreamingUnsupported = errors.New("streaming payload signing is not supported; use an https:// endpoint so the client sends UNSIGNED-PAYLOAD")
)

// SecretLookup resolves an access key id to its secret. A disabled or unknown
// key returns ok=false; the caller answers InvalidAccessKeyId either way, so a
// disabled key is indistinguishable from one that never existed.
type SecretLookup func(ctx context.Context, accessKeyID string) (secret string, ok bool)

// decoySecret keeps an unknown key on the same code path as a known one, so a
// miss costs the same four HMACs as a hit rather than returning early.
const decoySecret = "0000000000000000000000000000000000000000"

// Verify checks an AWS4-HMAC-SHA256 signature on r and returns the access key
// id it was signed with. It rejects: a missing or non-AWS4 Authorization
// header, an X-Amz-Date more than maxClockSkew from now, a SignedHeaders set
// without "host", "x-amz-content-sha256" or "x-amz-date", any x-amz-* request
// header the client did not sign, a credential scope naming a different region
// or service, an unknown key, and a signature mismatch (compared with
// hmac.Equal). It never reads the body: the payload hash is taken verbatim
// from x-amz-content-sha256.
//
// Region is exact; there is no wildcard. minio-go signs GetBucketLocation with
// a hardcoded "us-east-1" whatever the client's configured region is
// (minio-go/bucket-cache.go), so the handler must pass region "us-east-1" for
// a "?location=" request and the target's configured region for every other
// request.
//
// Mounting: the gateway is mounted at the bucket path ("/warphold/") on the
// public host — path-style S3, endpoint = the public host, no route prefix —
// so r.URL.Path is exactly the path the client signed. Mounting it under a
// prefix such as "/s3" changes the canonical request and every signature
// fails.
//
// Canonicalisation aliasing: different wire spellings of a path or query can
// canonicalise to the same signed bytes, so callers must key storage access
// and prefix confinement off the decoded r.URL.Path and r.URL.Query() and
// never off r.RequestURI or r.URL.RawPath. Normalising the key is the
// caller's job (Task 2's NormalizeKey): Verify authenticates a request, it
// does not authorise a key.
//
// Replaying a byte-identical signed request inside the skew window is accepted
// by design — the store is append-only, so a replay is a no-op or a 409.
func Verify(ctx context.Context, r *http.Request, region, service string, look SecretLookup, now time.Time) (string, error) {
	akid, scope, signedHeaders, sig, err := parseAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		return "", err
	}

	scopeDate, scopeRegion, err := parseScope(scope, region, service)
	if err != nil {
		return "", err
	}

	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return "", fmt.Errorf("%w: no X-Amz-Date", ErrMalformedAuthorization)
	}

	t, err := time.Parse(iso8601Format, amzDate)
	if err != nil {
		return "", fmt.Errorf("%w: unparseable X-Amz-Date", ErrMalformedAuthorization)
	}

	if t.Format(scopeDateFormat) != scopeDate {
		return "", fmt.Errorf("%w: credential scope date does not match X-Amz-Date", ErrMalformedAuthorization)
	}

	if d := now.Sub(t); d > maxClockSkew || d < -maxClockSkew {
		return "", ErrRequestTimeTooSkewed
	}

	if err := requireSignedHeaders(r, signedHeaders); err != nil {
		return "", err
	}

	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if strings.HasPrefix(payloadHash, streamingPrefix) {
		return "", ErrStreamingUnsupported
	}

	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrMalformedQuery, err)
	}

	secret, known := look(ctx, akid)
	if !known {
		secret = decoySecret
	}

	creq := canonicalRequest(r, query, signedHeaders, payloadHash)
	sts := stringToSign(amzDate, scope, creq)
	want := hex.EncodeToString(hmacSHA256(signingKey(secret, scopeDate, scopeRegion, service), sts))

	// Both branches run after the derivation so an unknown key costs the same
	// work as a known one; a disabled key is a lookup miss and lands here too.
	if !known {
		return "", ErrInvalidAccessKeyID
	}

	if !hmac.Equal([]byte(want), []byte(sig)) {
		return "", ErrSignatureDoesNotMatch
	}

	return akid, nil
}

// parseAuthorization splits "AWS4-HMAC-SHA256 Credential=<akid>/<scope>,
// SignedHeaders=<a;b>, Signature=<hex>" into its parts.
func parseAuthorization(v string) (akid, scope string, signedHeaders []string, sig string, err error) {
	algo, rest, ok := strings.Cut(v, " ")
	if !ok || algo != signV4Algorithm {
		return "", "", nil, "", fmt.Errorf("%w: not %s", ErrMalformedAuthorization, signV4Algorithm)
	}

	var credential string

	for _, part := range strings.Split(rest, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return "", "", nil, "", fmt.Errorf("%w: bad component %q", ErrMalformedAuthorization, k)
		}

		switch k {
		case "Credential":
			credential = val
		case "SignedHeaders":
			signedHeaders = strings.Split(val, ";")
		case "Signature":
			sig = val
		}
	}

	if credential == "" || sig == "" || len(signedHeaders) == 0 {
		return "", "", nil, "", fmt.Errorf("%w: missing Credential, SignedHeaders or Signature", ErrMalformedAuthorization)
	}

	akid, scope, ok = strings.Cut(credential, "/")
	if !ok || akid == "" {
		return "", "", nil, "", fmt.Errorf("%w: bad Credential", ErrMalformedAuthorization)
	}

	return akid, scope, signedHeaders, sig, nil
}

// parseScope validates "<yyyymmdd>/<region>/<service>/aws4_request" and
// returns the date and region it names.
func parseScope(scope, wantRegion, wantService string) (date, region string, err error) {
	parts := strings.Split(scope, "/")
	if len(parts) != 4 || parts[3] != scopeTerminator {
		return "", "", fmt.Errorf("%w: bad credential scope", ErrMalformedAuthorization)
	}

	date, region, service := parts[0], parts[1], parts[2]
	if service != wantService {
		return "", "", fmt.Errorf("%w: credential scope names service %q", ErrMalformedAuthorization, service)
	}

	if region != wantRegion {
		return "", "", fmt.Errorf("%w: credential scope names region %q", ErrMalformedAuthorization, region)
	}

	return date, region, nil
}

// requireSignedHeaders checks that SignedHeaders names the three headers the
// signature is worthless without, and that it covers every x-amz-* header the
// request actually carries. The x-amz-* sweep is deliberately not widened to
// all headers: proxies legitimately add X-Forwarded-* after the client signed.
func requireSignedHeaders(r *http.Request, signed []string) error {
	set := make(map[string]bool, len(signed))
	for _, h := range signed {
		set[h] = true
	}

	for _, required := range []string{hostHeader, contentSHAHeader, dateHeader} {
		if !set[required] {
			return fmt.Errorf("%w: SignedHeaders omits %s", ErrMalformedAuthorization, required)
		}
	}

	for name := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") && !set[lower] {
			return fmt.Errorf("%w: %s", ErrUnsignedAmzHeader, lower)
		}
	}

	return nil
}

// canonicalRequest rebuilds the client's canonical request. It matches
// minio-go's signer byte for byte: the path is re-encoded with
// s3utils.EncodePath, the query is Go's sorted encoding with "+" spelled
// "%20", and header values are whitespace-collapsed and comma-joined.
func canonicalRequest(r *http.Request, query url.Values, signedHeaders []string, payloadHash string) string {
	var b strings.Builder

	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(s3utils.EncodePath(r.URL.Path))
	b.WriteByte('\n')
	b.WriteString(strings.ReplaceAll(query.Encode(), "+", "%20"))
	b.WriteByte('\n')

	for _, h := range signedHeaders {
		b.WriteString(h)
		b.WriteByte(':')
		b.WriteString(headerValue(r, h))
		b.WriteByte('\n')
	}

	b.WriteByte('\n')
	b.WriteString(strings.Join(signedHeaders, ";"))
	b.WriteByte('\n')
	b.WriteString(payloadHash)

	return b.String()
}

// headerValue reads one signed header. Go's server hoists Host and
// Content-Length out of the header map, so those two are read back from the
// request fields the client signed them as.
func headerValue(r *http.Request, name string) string {
	switch name {
	case hostHeader:
		return r.Host
	case "content-length":
		if r.ContentLength < 0 {
			return ""
		}

		return strconv.FormatInt(r.ContentLength, 10)
	}

	vals := r.Header.Values(name)
	trimmed := make([]string, len(vals))

	for i, v := range vals {
		trimmed[i] = strings.Join(strings.Fields(v), " ")
	}

	return strings.Join(trimmed, ",")
}

func stringToSign(amzDate, scope, creq string) string {
	sum := sha256.Sum256([]byte(creq))
	return signV4Algorithm + "\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)

	return hmacSHA256(k, scopeTerminator)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))

	return h.Sum(nil)
}
