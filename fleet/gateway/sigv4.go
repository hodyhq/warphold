// Package gateway implements WarpHold's append-only, S3-compatible gateway.
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

	// ErrMalformedQuery is a query string url.ParseQuery cannot read whole.
	// Go's URL.Query() would silently drop the unreadable pairs, which would
	// leave them outside the signature; the gateway refuses them instead.
	ErrMalformedQuery = errors.New("malformed query string")

	// ErrStreamingUnsupported is returned for aws-chunked payload signing,
	// which minio-go only uses over plain HTTP. WarpHold's gateway is always
	// reached over TLS, where the client sends UNSIGNED-PAYLOAD instead.
	ErrStreamingUnsupported = errors.New("streaming payload signing is not supported; use an https:// endpoint so the client sends UNSIGNED-PAYLOAD")
)

// Credential is what a verified request identifies.
type Credential struct{ AccessKeyID, Secret string }

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
// without "host", "x-amz-content-sha256" or "x-amz-date", a credential scope
// naming a different service (or, when region is non-empty, a different
// region), an unknown key, and a signature mismatch (compared with
// hmac.Equal). It never reads the body: the payload hash is taken verbatim
// from x-amz-content-sha256.
//
// An empty region accepts whatever region the client signed with, which is
// what an S3 client that discovered the region via GetBucketLocation sends.
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

	if err := requireSignedHeaders(signedHeaders); err != nil {
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
// returns the date and region it names. An empty wantRegion accepts any.
func parseScope(scope, wantRegion, wantService string) (date, region string, err error) {
	parts := strings.Split(scope, "/")
	if len(parts) != 4 || parts[3] != scopeTerminator {
		return "", "", fmt.Errorf("%w: bad credential scope", ErrMalformedAuthorization)
	}

	date, region, service := parts[0], parts[1], parts[2]
	if service != wantService {
		return "", "", fmt.Errorf("%w: credential scope names service %q", ErrMalformedAuthorization, service)
	}

	if wantRegion != "" && region != wantRegion {
		return "", "", fmt.Errorf("%w: credential scope names region %q", ErrMalformedAuthorization, region)
	}

	return date, region, nil
}

func requireSignedHeaders(signed []string) error {
	for _, required := range []string{hostHeader, contentSHAHeader, dateHeader} {
		found := false

		for _, h := range signed {
			if h == required {
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf("%w: SignedHeaders omits %s", ErrMalformedAuthorization, required)
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
