package gateway

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content-MD5 is S3's integrity check, not a security control.
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// BucketName is the single bucket every device sees, whatever backs it.
const BucketName = "warphold"

// PathPrefix is where the gateway mounts: the bucket path itself, path-style.
// It cannot live under /s3/ because minio-go refuses an endpoint URL that
// carries a path, so the device's endpoint is the bare public host and the
// bucket is the first path segment. It must be registered before the SPA's "/"
// catch-all.
const PathPrefix = "/" + BucketName + "/"

// DefaultRegion is the region the gateway advertises and verifies signatures
// against when Config.Region is empty. Provisioning puts it in the device's
// connection info, so the two ends cannot drift apart.
const DefaultRegion = BucketName

// locationRegion is the region minio-go hardcodes into the signature of a
// GetBucketLocation request, whatever the client was configured with.
const locationRegion = "us-east-1"

const (
	// DefaultMaxObjectBytes caps a single PUT (spec §4). The D4 spike's largest
	// observed single object was 21.4 MB, against Kopia's 20 MiB default pack
	// blob, so 64 MiB is roomy without letting one request eat the disk.
	DefaultMaxObjectBytes = 64 << 20

	// maxListKeys is the ceiling on max-keys, per spec §4.2.
	maxListKeys = 1000

	// retryAfterSeconds is what a throttled device is told to wait.
	retryAfterSeconds = 1
)

// The append-only rules, from the D4 spike recorded in
// docs/RECONCILE-append-only.md §6.1:
//
//	allowDelete    = [ "^s[0-9a-f]{16,}" ]   // session markers, and nothing else
//	allowOverwrite = [ ]                     // nothing; an existing key is denied
//
// Both match the *blob name* -- the key with the device prefix already
// stripped -- and both are anchored, because "s" is also the first byte of the
// "xs" single-epoch compaction blobs a substring match would let through
// (spike §7.3).
var allowDeleteRE = regexp.MustCompile(`^s[0-9a-f]{16,}`)

// allowDelete reports whether a device may delete this blob.
func allowDelete(blobName string) bool { return allowDeleteRE.MatchString(blobName) }

// deviceOverwrite is the overwrite flag every device PUT passes. It is a
// constant, not a computed allowlist, because allowOverwrite is empty for every
// blob class there is: hosted provisioning gives the repository's maintenance
// owner to the Fleet identity and runs the device with --no-auto-maintenance,
// which the spike proved removes the only overwrite Kopia otherwise issues
// (kopia.maintenance). A device request can therefore never reach
// Put(..., overwrite=true), and an overwrite attempt means that lever failed.
const deviceOverwrite = false

// ErrUnsupportedStorageMode is what StoreFor returns for a target the gateway
// cannot serve yet; the handler answers 501 rather than 500.
var ErrUnsupportedStorageMode = errors.New("gateway: unsupported target storage mode")

// knownQueryKeys is every query parameter the gateway understands: the
// ListObjectsV2 set, the two bucket sub-resources it answers, and the four
// out-of-scope sub-resources it answers 501 to. Anything else is refused
// before dispatch with 400 InvalidArgument rather than silently ignored, so a
// client cannot smuggle an unimplemented sub-resource (?acl, ?tagging, ?policy)
// past the verb switch and have it read as a plain GET or PUT.
var knownQueryKeys = map[string]bool{
	// ListObjectsV2 (RECONCILE §5.3).
	"list-type": true, "prefix": true, "start-after": true, "continuation-token": true,
	"max-keys": true, "delimiter": true, "encoding-type": true, "fetch-owner": true,
	// Bucket sub-resources the gateway answers.
	"location": true, "versioning": true,
	// Out of scope, answered 501 by name so the message can say why.
	"uploads": true, "uploadId": true, "partNumber": true, "delete": true, "retention": true,
	// Versioned and point-in-time reads: no backend the gateway serves keeps
	// versions (GetBucketVersioning is 501 too), so these are refused by name
	// rather than ignored, which would silently answer with the live object.
	"versionId": true, "versions": true, "metadata": true,
}

// ErrAccessDenied is what StoreFor returns when a credential resolves to
// nothing it may use -- a revoked device, most of all. The handler answers the
// same 403 an unknown key gets, so the two are indistinguishable.
var ErrAccessDenied = errors.New("gateway: access denied")

// errBadDigest is raised by the streaming Content-MD5 check, so a body whose
// digest does not match is never stored.
var errBadDigest = errors.New("gateway: Content-MD5 does not match the body")

// LogEntry is one gateway request, handed to Config.Log. It carries no secret
// and no signature: AccessKeyID is a public identifier, Key is the object key.
type LogEntry struct {
	DeviceID, AccessKeyID, Method, Key string
	// Class is the Kopia blob class ("s", "p", "xn", "kopia.repository", ...),
	// which is what the default log line prints instead of the full key.
	Class  string
	Status int
	Bytes  int64
	Dur    time.Duration
}

// Config configures a Gateway.
type Config struct {
	// Keys resolves an access key id to a device. Required.
	Keys *Keys

	// StoreFor resolves a verified device to the store behind its target: one
	// ObjectStore per hosted target, shared by that target's devices.
	StoreFor func(ctx context.Context, agentID string) (ObjectStore, error)

	// Bucket and Region default to BucketName and DefaultRegion. Region is what
	// GetBucketLocation advertises and what a credential scope must name.
	Bucket, Region string

	// MaxObjectBytes caps a single PUT; zero means DefaultMaxObjectBytes.
	MaxObjectBytes int64

	// RatePerSecond and RateBurst are the per-device token bucket; zero means
	// the spec §11 defaults (50/s, burst 200).
	RatePerSecond, RateBurst float64

	// IPRatePerSecond and IPRateBurst are the pre-auth, client-IP-keyed bucket
	// that runs ahead of signature verification; zero means 200/s, burst 800.
	IPRatePerSecond, IPRateBurst float64

	// TrustedProxies are the CIDRs whose X-Forwarded-For header the pre-auth
	// limiter believes. Empty (the default) means the peer address is always
	// used and the header is ignored entirely. It is a snapshot taken when the
	// Gateway is built, so a change to the setting applies at restart.
	TrustedProxies []net.IPNet

	Now func() time.Time
	Log func(LogEntry)
}

// Gateway serves the append-only S3 subset of spec §4.2 at /s3/. It is
// stateless: the only per-device state is the key cache and the rate limiter,
// both bounded and both self-evicting.
type Gateway struct {
	keys     *Keys
	storeFor func(ctx context.Context, agentID string) (ObjectStore, error)
	bucket   string
	region   string
	maxBytes int64
	limit    *limiter
	ipLimit  *limiter
	trusted  []net.IPNet
	now      func() time.Time
	logf     func(LogEntry)
}

// NewGateway builds a Gateway from c.
func NewGateway(c Config) *Gateway {
	g := &Gateway{
		keys:     c.Keys,
		storeFor: c.StoreFor,
		bucket:   c.Bucket,
		region:   c.Region,
		maxBytes: c.MaxObjectBytes,
		limit:    newLimiter(c.RatePerSecond, c.RateBurst),
		ipLimit:  newIPLimiter(c.IPRatePerSecond, c.IPRateBurst),
		trusted:  cloneNets(c.TrustedProxies),
		now:      c.Now,
		logf:     c.Log,
	}

	if g.bucket == "" {
		g.bucket = BucketName
	}

	if g.region == "" {
		g.region = DefaultRegion
	}

	if g.maxBytes <= 0 {
		g.maxBytes = DefaultMaxObjectBytes
	}

	if g.now == nil {
		g.now = time.Now
	}

	if g.logf == nil {
		g.logf = defaultLog
	}

	return g
}

// cloneNets deep-copies the trusted-proxy list, IP and Mask bytes included:
// it is a trust boundary, and the Gateway must not share mutable state with
// whoever built its Config.
func cloneNets(nets []net.IPNet) []net.IPNet {
	if len(nets) == 0 {
		return nil
	}

	out := make([]net.IPNet, len(nets))
	for i, n := range nets {
		out[i] = net.IPNet{IP: slices.Clone(n.IP), Mask: slices.Clone(n.Mask)}
	}

	return out
}

func defaultLog(e LogEntry) {
	log.Printf("warphold gateway: device=%s key=%s %s %s -> %d %d bytes in %s",
		orDash(e.DeviceID), orDash(e.AccessKeyID), e.Method, orDash(e.Class), e.Status, e.Bytes, e.Dur.Round(time.Millisecond))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

// InvalidateKeys drops every cached credential of an agent, so a revocation
// takes effect on the next request rather than when the cache entry expires.
func (g *Gateway) InvalidateKeys(agentID string) { g.keys.Invalidate(agentID) }

// Handler returns the gateway as an http.Handler. It reads the full request
// path (bucket segment included), because that is what the client signed -- do
// not wrap it in http.StripPrefix.
func (g *Gateway) Handler() http.Handler { return g }

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := g.now()
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	e := LogEntry{Method: r.Method}

	g.serve(rec, r, &e)

	e.Status, e.Bytes, e.Dur = rec.status, rec.bytes, g.now().Sub(start)
	g.logf(e)
}

//nolint:cyclop,gocyclo // one switch over the S3 verbs; splitting it hides the shape.
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, e *LogEntry) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != g.bucket {
		writeError(w, r, http.StatusNotFound, codeNoSuchBucket, "no such bucket")
		return
	}

	e.Key = key

	// GetBucketLocation is signed with a region minio-go hardcodes, not with
	// the one the client was configured with, so it is the one request whose
	// signature must be checked against us-east-1. A query the signer could not
	// parse falls through to Verify, which fails it closed.
	region := g.region

	if q, err := url.ParseQuery(r.URL.RawQuery); err == nil && key == "" {
		if _, ok := q["location"]; ok {
			region = locationRegion
		}
	}

	// The credential is captured by the lookup Verify calls, so the signature
	// check and the identity resolution are one store hit.
	var agentID, prefix string

	var readOnly bool

	look := func(ctx context.Context, accessKeyID string) (string, bool) {
		a, p, secret, ro, ok := g.keys.Lookup(ctx, accessKeyID)
		if !ok {
			return "", false
		}

		// The prefix is the entire confinement boundary, so it is validated
		// here -- once, where it enters the request -- rather than trusted
		// because a provisioning path is believed to have written it well.
		// "p == a+\"/\"" is three conditions at once: non-empty, ends in a
		// slash (so no HasPrefix test downstream can match a neighbouring
		// device), and names this agent and no other. checkKey on a synthetic
		// key under it then runs the agent id through the same boundary every
		// real key crosses -- UTF-8, control bytes, backslash, relative
		// segments, the flat shape and the reserved temp directory -- rather
		// than re-implementing any of it here.
		if p != a+"/" || checkKey(p+"x") != nil {
			// The prefix itself is never logged, only its shape: it is
			// attacker-influenced only through provisioning, but a log line is
			// not the place to find out.
			log.Printf("warphold gateway: device key with malformed prefix, refusing: agent=%q key=%q prefix_len=%d ends_in_slash=%t",
				a, accessKeyID, len(p), strings.HasSuffix(p, "/"))
			return "", false
		}

		agentID, prefix, readOnly = a, p, ro

		return secret, true
	}

	// The pre-auth limiter runs before Verify, so an unauthenticated flood
	// cannot buy four HMAC chains per request.
	if !g.ipLimit.allow(ClientIP(r, g.trusted), g.now()) {
		writeSlowDown(w, r)
		return
	}

	if r.Header.Get("Authorization") == "" {
		// An anonymous request, which S3 answers 403 rather than 400: there is
		// no malformed header to complain about.
		writeError(w, r, http.StatusForbidden, codeAccessDenied, "access denied")
		return
	}

	akid, err := Verify(r.Context(), r, region, "s3", look, g.now())
	if err != nil {
		status, code := mapAuthError(err)
		writeError(w, r, status, code, authMessage(err, code))

		return
	}

	e.AccessKeyID, e.DeviceID = akid, agentID

	// Keyed on the device, not the access key id: a device with two keys
	// (rotation, or the recovery kit's read-only key) gets one budget, not one
	// per key.
	if !g.limit.allow(agentID, g.now()) {
		writeSlowDown(w, r)
		return
	}

	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidURI, "malformed query string")
		return
	}

	for k := range query {
		if !knownQueryKeys[k] {
			// The key is not echoed: a fixed message says as much to a real
			// client and reflects nothing a caller controls.
			writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "unsupported query parameter")
			return
		}
	}

	// Multipart and server-side copy are out of scope (spec §15), and both are
	// refused before any storage call.
	if _, ok := query["uploads"]; ok {
		writeNotImplemented(w, r, "multipart upload is not supported")
		return
	}

	for _, k := range []string{"uploadId", "partNumber"} {
		// Key presence, not a non-empty value: "?uploadId=" would otherwise
		// slip past and be served as a plain object request.
		if _, ok := query[k]; ok {
			writeNotImplemented(w, r, "multipart upload is not supported")
			return
		}
	}

	for _, k := range []string{"versionId", "versions", "metadata"} {
		if _, ok := query[k]; ok {
			writeNotImplemented(w, r, "object versions are not supported by hosted targets")
			return
		}
	}

	if r.Header.Get("X-Amz-Copy-Source") != "" {
		writeNotImplemented(w, r, "server-side copy is not supported")
		return
	}

	st, err := g.storeFor(r.Context(), agentID)
	if err != nil {
		if errors.Is(err, ErrAccessDenied) {
			writeError(w, r, http.StatusForbidden, codeAccessDenied, "access denied")
			return
		}

		if errors.Is(err, ErrUnsupportedStorageMode) {
			writeNotImplemented(w, r, "this target's storage mode is not served by the gateway")
			return
		}

		log.Printf("warphold gateway: no store for device %s: %v", agentID, err)
		writeError(w, r, http.StatusInternalServerError, codeInternalError, "storage unavailable")

		return
	}

	if key == "" {
		g.serveBucket(w, r, query, st, prefix, e)
		return
	}

	full, err := NormalizeKey(key, prefix)
	if err != nil {
		if !strings.HasPrefix(key, prefix) {
			// Outside the device's prefix. 403 with no detail: a device must not
			// learn whether another device's key exists.
			writeError(w, r, http.StatusForbidden, codeAccessDenied, "access denied")
			return
		}

		// Inside the prefix but not a key we accept -- a nested key (the key
		// space is flat, spec §4.3), a control byte, a relative segment.
		writeError(w, r, http.StatusBadRequest, codeInvalidObjectKey, "invalid object key")

		return
	}

	blobName := strings.TrimPrefix(full, prefix)
	e.Class = keyClass(blobName)

	switch r.Method {
	case http.MethodPut:
		if _, ok := query["retention"]; ok {
			writeNotImplemented(w, r, "object retention is applied by the mirror job, not by the device")
			return
		}

		g.put(w, r, st, full, blobName, readOnly)
	case http.MethodGet:
		g.get(w, r, st, full)
	case http.MethodHead:
		g.head(w, r, st, full)
	case http.MethodDelete:
		g.delete(w, r, st, full, blobName, readOnly)
	case http.MethodPost:
		writeNotImplemented(w, r, "this operation is not supported")
	default:
		writeError(w, r, http.StatusForbidden, codeAccessDenied, "access denied")
	}
}

// serveBucket handles the bucket-level requests: ListObjectsV2, and the two
// probes minio-go may issue before it.
func (g *Gateway) serveBucket(w http.ResponseWriter, r *http.Request, query url.Values, st ObjectStore, prefix string, e *LogEntry) {
	if r.Method == http.MethodPost {
		// ?delete is bulk delete; anything else posted at the bucket is equally
		// out of scope.
		writeNotImplemented(w, r, "this operation is not supported")
		return
	}

	if _, ok := query["versioning"]; ok {
		// RECONCILE §5.2: Kopia's IsVersioned has no caller, and no backend the
		// gateway serves is versioned.
		e.Class = "versioning"
		writeNotImplemented(w, r, "bucket versioning is not supported")

		return
	}

	if _, ok := query["location"]; ok {
		// Sent whenever the client was configured without --region.
		e.Class = "location"
		writeXML(w, http.StatusOK, locationConstraint{Value: g.region})

		return
	}

	if r.Method == http.MethodHead {
		// HeadBucket: the bucket name is a constant and the caller is already
		// authenticated, so this leaks nothing.
		e.Class = "bucket"
		w.WriteHeader(http.StatusOK)

		return
	}

	if r.Method != http.MethodGet {
		writeError(w, r, http.StatusForbidden, codeAccessDenied, "access denied")
		return
	}

	if query.Get("list-type") != "2" {
		writeNotImplemented(w, r, "only ListObjectsV2 (list-type=2) is supported")
		return
	}

	e.Class = "list"

	g.list(w, r, query, st, prefix)
}

func (g *Gateway) put(w http.ResponseWriter, r *http.Request, st ObjectStore, key, blobName string, readOnly bool) {
	if readOnly {
		writeError(w, r, http.StatusForbidden, codeAccessDenied, "this key is read-only")
		return
	}

	// Content-Length is required: it is what catches a truncated body, and
	// minio-go always sends it for the whole-object PUTs Kopia issues.
	if r.ContentLength < 0 {
		writeError(w, r, http.StatusLengthRequired, codeMissingContentLength, "Content-Length is required")
		return
	}

	if r.ContentLength > g.maxBytes {
		writeError(w, r, http.StatusRequestEntityTooLarge, codeEntityTooLarge,
			fmt.Sprintf("object exceeds the %d byte limit", g.maxBytes))

		return
	}

	// The payload is UNSIGNED-PAYLOAD over TLS (RECONCILE §5.1), so Content-MD5
	// is the only end-to-end integrity check on the body. minio-go always sends
	// it (SendContentMd5: true), so requiring it costs nothing and closes the
	// gap.
	want, err := base64.StdEncoding.DecodeString(r.Header.Get("Content-Md5"))
	if err != nil || len(want) != md5.Size {
		writeError(w, r, http.StatusBadRequest, codeMissingContentMD5, "a valid base64 Content-MD5 header is required")
		return
	}

	body := &md5Check{r: r.Body, h: md5.New(), want: want} //nolint:gosec // integrity check, see above

	info, err := st.Put(r.Context(), key, body, r.ContentLength, deviceOverwrite)

	switch {
	case err == nil:
		if !body.verified {
			// The store reported success without reading the body to EOF, so
			// the Content-MD5 was never checked. That is a backend contract
			// bug, not a client error: fail closed rather than acknowledge
			// bytes whose integrity nothing verified.
			log.Printf("warphold gateway: BUG: store accepted a %q blob without reading the body to EOF; Content-MD5 unverified", blobName2Class(key))
			writeError(w, r, http.StatusInternalServerError, codeInternalError, "could not store the object")

			return
		}
	case errors.Is(err, errBadDigest):
		writeError(w, r, http.StatusBadRequest, codeBadDigest, "the Content-MD5 does not match the body")
		return
	case errors.Is(err, ErrExists):
		// The append-only rule (spec §4.2, RECONCILE §6.1). 409, not 403: the
		// credential was allowed to write, the object's prior existence is what
		// refused it, and a 409 is the signal that the maintenance-owner lever
		// failed rather than a permissions problem (RECONCILE §7.6).
		writeError(w, r, http.StatusConflict, codeObjectAlreadyExists,
			"append-only: this object already exists and cannot be replaced")

		return
	case errors.Is(err, ErrIncompleteBody):
		writeError(w, r, http.StatusBadRequest, codeIncompleteBody, "the body did not match Content-Length")
		return
	case errors.Is(err, ErrTooLarge):
		writeError(w, r, http.StatusRequestEntityTooLarge, codeEntityTooLarge, "object exceeds the store's size limit")
		return
	case errors.Is(err, ErrBadKey):
		writeError(w, r, http.StatusBadRequest, codeInvalidObjectKey, "invalid object key")
		return
	default:
		log.Printf("warphold gateway: put failed: %v", err)
		writeError(w, r, http.StatusInternalServerError, codeInternalError, "could not store the object")

		return
	}

	w.Header().Set("ETag", strconv.Quote(info.ETag))
	w.WriteHeader(http.StatusOK)
}

func (g *Gateway) get(w http.ResponseWriter, r *http.Request, st ObjectStore, key string) {
	offset, length, ranged, err := g.parseRange(r.Context(), r.Header.Get("Range"), st, key)
	if err != nil {
		writeRangeError(w, r, err)
		return
	}

	rc, info, err := st.Get(r.Context(), key, offset, length)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	defer rc.Close() //nolint:errcheck // read-only handle

	served := info.Size - offset
	if length >= 0 && length < served {
		served = length
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(served, 10))
	h.Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")

	status := http.StatusOK

	// A zero-length range has no valid Content-Range to name ("bytes 5-4/10"
	// is malformed), so it is answered as a plain empty 200.
	if ranged && served > 0 {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+served-1, info.Size))
		status = http.StatusPartialContent
	}

	w.WriteHeader(status)

	if _, err := io.Copy(w, rc); err != nil {
		// The status is already on the wire; the truncated body is the client's
		// signal. Log it so a failing disk is visible.
		log.Printf("warphold gateway: get %s truncated: %v", key, err)
	}
}

func (g *Gateway) head(w http.ResponseWriter, r *http.Request, st ObjectStore, key string) {
	info, err := st.Head(r.Context(), key)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	h.Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")

	if info.ETag != "" {
		h.Set("ETag", strconv.Quote(info.ETag))
	}

	w.WriteHeader(http.StatusOK)
}

func (g *Gateway) delete(w http.ResponseWriter, r *http.Request, st ObjectStore, key, blobName string, readOnly bool) {
	if readOnly || !allowDelete(blobName) {
		writeError(w, r, http.StatusForbidden, codeAppendOnlyDeleteDenied,
			"append-only: only Kopia session markers may be deleted (docs/RECONCILE-append-only.md)")

		return
	}

	if err := st.Delete(r.Context(), key); err != nil && !errors.Is(err, ErrNotFound) {
		writeStoreError(w, r, err)
		return
	}

	// S3's DeleteObject is idempotent: a missing key is still 204.
	w.WriteHeader(http.StatusNoContent)
}

// parseRange reads a Range header. Only a single byte range is supported; a
// multi-range request is 501 (spec §4.2). length < 0 means "to the end".
func (g *Gateway) parseRange(ctx context.Context, hdr string, st ObjectStore, key string) (offset, length int64, ranged bool, err error) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(hdr), "bytes=")
	if !ok {
		// No Range, or a unit we do not implement: serve the whole object, which
		// is what S3 does with an unsatisfiable Range unit.
		return 0, -1, false, nil
	}

	if strings.Contains(spec, ",") {
		return 0, 0, false, errMultiRange
	}

	first, last, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, -1, false, nil
	}

	if first == "" {
		// A suffix range ("bytes=-N"). Kopia never sends one, so paying for a
		// Head here is fine; it is the only way to turn it into an offset.
		n, perr := strconv.ParseInt(last, 10, 64)
		if perr != nil || n <= 0 {
			return 0, -1, false, nil
		}

		info, herr := st.Head(ctx, key)
		if herr != nil {
			return 0, 0, false, herr
		}

		if n > info.Size {
			n = info.Size
		}

		return info.Size - n, n, true, nil
	}

	start, perr := strconv.ParseInt(first, 10, 64)
	if perr != nil || start < 0 {
		return 0, -1, false, nil
	}

	if last == "" {
		return start, -1, true, nil
	}

	end, perr := strconv.ParseInt(last, 10, 64)
	if perr != nil || end < start {
		return 0, -1, false, nil
	}

	return start, end - start + 1, true, nil
}

var errMultiRange = errors.New("gateway: multiple byte ranges are not supported")

func writeRangeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errMultiRange) {
		writeNotImplemented(w, r, "only a single byte range is supported")
		return
	}

	writeStoreError(w, r, err)
}

func writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, r, http.StatusNotFound, codeNoSuchKey, "no such key")
	case errors.Is(err, ErrRange):
		writeError(w, r, http.StatusRequestedRangeNotSatisfiable, codeInvalidRange, "the requested range is not satisfiable")
	case errors.Is(err, ErrBadKey):
		writeError(w, r, http.StatusForbidden, codeAccessDenied, "access denied")
	default:
		log.Printf("warphold gateway: storage error: %v", err)
		writeError(w, r, http.StatusInternalServerError, codeInternalError, "storage error")
	}
}

func writeSlowDown(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	writeError(w, r, http.StatusServiceUnavailable, codeSlowDown, "too many requests; slow down")
}

func writeNotImplemented(w http.ResponseWriter, r *http.Request, msg string) {
	writeError(w, r, http.StatusNotImplemented, codeNotImplemented, msg)
}

// mapAuthError turns a Verify sentinel into its S3 answer (Task 3's table).
func mapAuthError(err error) (int, string) {
	switch {
	case errors.Is(err, ErrStreamingUnsupported):
		return http.StatusNotImplemented, codeNotImplemented
	case errors.Is(err, ErrRequestTimeTooSkewed):
		return http.StatusForbidden, codeRequestTimeTooSkewed
	case errors.Is(err, ErrInvalidAccessKeyID):
		return http.StatusForbidden, codeInvalidAccessKeyID
	case errors.Is(err, ErrSignatureDoesNotMatch):
		return http.StatusForbidden, codeSignatureDoesNotMatch
	case errors.Is(err, ErrUnsignedAmzHeader):
		// An x-amz-* header outside the signature could have been added by a
		// proxy; the request is not the one the client signed.
		return http.StatusForbidden, codeAccessDenied
	case errors.Is(err, ErrMalformedQuery):
		return http.StatusBadRequest, codeInvalidURI
	default:
		return http.StatusBadRequest, codeAuthorizationHeaderMalformed
	}
}

// authMessage keeps the streaming rejection's message, which names the fix, and
// says nothing about any other failure -- an attacker learns only the code.
func authMessage(err error, code string) string {
	if errors.Is(err, ErrStreamingUnsupported) {
		return ErrStreamingUnsupported.Error()
	}

	return code
}

// md5Check verifies Content-MD5 while the body streams, so a mismatched body
// fails the Put and nothing is ever stored.
type md5Check struct {
	r        io.Reader
	h        hash.Hash
	want     []byte
	verified bool
}

func (m *md5Check) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n > 0 {
		m.h.Write(p[:n])
	}

	if errors.Is(err, io.EOF) && !m.verified {
		m.verified = true

		if !bytes.Equal(m.h.Sum(nil), m.want) {
			return n, errBadDigest
		}
	}

	return n, err //nolint:wrapcheck // pass the reader's own error through untouched.
}

// blobName2Class is keyClass over a full key: the log never carries the key
// itself, only the class, so a blob name cannot end up in an operator's log.
func blobName2Class(key string) string {
	_, name, _ := strings.Cut(key, "/")
	return keyClass(name)
}

// keyClass names the Kopia blob class of a key for the request log. It never
// returns any part of a blob name: the classes are a closed set, so a log line
// says "s" or "xn" or "kopia-meta" and a content hash can never reach an
// operator's log through it.
func keyClass(blobName string) string {
	switch {
	case blobName == "":
		return ""
	case strings.HasPrefix(blobName, "kopia."):
		// kopia.repository, kopia.blobcfg, kopia.maintenance and the format
		// backups: all repository metadata, none of it worth distinguishing in
		// a log line.
		return "kopia-meta"
	case blobName[0] == '.':
		// .storageconfig and anything else dot-prefixed.
		return "dot"
	case strings.HasPrefix(blobName, "_log_"):
		return "_log_"
	case blobName[0] == 'x' && len(blobName) > 1:
		// Epoch index blobs are xn/xe/xs/xr/xw; the second byte is the class.
		return blobName[:2]
	default:
		return blobName[:1]
	}
}

// recorder captures the status and byte count for the request log.
type recorder struct {
	http.ResponseWriter
	status  int
	bytes   int64
	written bool
}

func (rec *recorder) WriteHeader(status int) {
	if rec.written {
		return
	}

	rec.written, rec.status = true, status
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Write(p []byte) (int, error) {
	if !rec.written {
		rec.WriteHeader(http.StatusOK)
	}

	n, err := rec.ResponseWriter.Write(p)
	rec.bytes += int64(n)

	return n, err //nolint:wrapcheck // pass the writer's own error through untouched.
}

// list serves ListObjectsV2, confined to the device's prefix.
//
// Prefix confinement is the defence-in-depth pair of spec §4.1.5: a prefix
// parameter that does not start with the device's prefix is *replaced* by it
// rather than erroring, and every key returned is under that prefix because
// that is the only prefix the store is ever asked for.
func (g *Gateway) list(w http.ResponseWriter, r *http.Request, query url.Values, st ObjectStore, devPrefix string) {
	askedPrefix := query.Get("prefix")

	// The device prefix is the floor (spec §4.1.5 first half): a prefix
	// parameter outside it is replaced rather than refused, so an accidental
	// enumeration lists the device's own keys instead of erroring. This
	// HasPrefix is sound only because the credential's prefix was validated as
	// exactly "<agent-id>/" where it was read -- an unterminated prefix would
	// let "agenta" match "agenta-evil/".
	prefix := devPrefix
	if strings.HasPrefix(askedPrefix, devPrefix) {
		prefix = askedPrefix
	}

	maxKeys := maxListKeys
	if v := query.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < maxListKeys {
			maxKeys = n
		}
	}

	after := query.Get("start-after")

	token := query.Get("continuation-token")
	if token != "" {
		var err error
		if after, err = decodeToken(token, devPrefix); err != nil {
			// A token minted for another device names another prefix, so it can
			// never be replayed here.
			writeError(w, r, http.StatusForbidden, codeAccessDenied, "access denied")

			return
		}
	}

	if after < prefix {
		after = ""
	}

	delimiter := query.Get("delimiter")
	encode := strings.EqualFold(query.Get("encoding-type"), "url")
	fetchOwner := strings.EqualFold(query.Get("fetch-owner"), "true")

	res := listBucketResult{
		Name:              g.bucket,
		Prefix:            askedPrefix,
		StartAfter:        query.Get("start-after"),
		ContinuationToken: token,
		MaxKeys:           maxKeys,
		Delimiter:         delimiter,
	}

	if encode {
		res.EncodingType = "url"
		res.Prefix = urlEncodeName(res.Prefix)
		res.StartAfter = urlEncodeName(res.StartAfter)
		res.Delimiter = urlEncodeName(res.Delimiter)
	}

	objs, next, truncated, err := collect(r.Context(), st, prefix, after, maxKeys)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	for _, o := range objs {
		if !strings.HasPrefix(o.Key, devPrefix) {
			// Spec §4.1.5 second half. The store was only ever asked for a
			// prefix at or below devPrefix, so this is unreachable; if a
			// backend ever returns it anyway, the whole list is refused rather
			// than partially leaked.
			log.Printf("warphold gateway: BUG: store returned a %q blob outside the device prefix", blobName2Class(o.Key))
			writeError(w, r, http.StatusInternalServerError, codeInternalError, "storage error")

			return
		}

		entry := objectEntry{
			Key:          o.Key,
			LastModified: s3Time(o.LastModified),
			Size:         o.Size,
			StorageClass: "STANDARD",
		}

		if o.ETag != "" {
			entry.ETag = strconv.Quote(o.ETag)
		}

		if encode {
			entry.Key = urlEncodeName(entry.Key)
		}

		if fetchOwner {
			entry.Owner = &owner{ID: g.bucket, DisplayName: g.bucket}
		}

		res.Contents = append(res.Contents, entry)
	}

	res.KeyCount = len(res.Contents)
	res.IsTruncated = truncated

	if truncated {
		res.NextContinuationToken = encodeToken(devPrefix, next)
	}

	writeXML(w, http.StatusOK, res)
}

// collect walks the store from after until maxKeys objects have been produced.
// next is the last key consumed, which is what the continuation token carries.
//
// There is no delimiter roll-up: the key space is flat -- exactly
// "<device-id>/<blob-name>", enforced by NormalizeKey -- and every list is
// confined to at least the device prefix, so no key can ever contain a
// delimiter after the prefix. The parameter is accepted and echoed because
// minio-go always sends it (RECONCILE §5.3); it can never group anything.
func collect(ctx context.Context, st ObjectStore, prefix, after string, maxKeys int) (objs []ObjectInfo, next string, truncated bool, err error) {
	batch, more, err := st.List(ctx, prefix, after, maxKeys)
	if err != nil {
		return nil, "", false, err
	}

	if len(batch) == 0 {
		return nil, after, false, nil
	}

	return batch, batch[len(batch)-1].Key, more, nil
}

// encodeToken makes an opaque continuation token. It carries the device prefix
// it was minted for, so decodeToken can refuse one replayed by another device
// -- which is the property spec §4.2 asks a signature for, without a key to
// manage: the token grants nothing the credential does not already grant.
func encodeToken(devPrefix, after string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(devPrefix + "\x00" + after))
}

func decodeToken(token, devPrefix string) (after string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("%w: undecodable continuation token", ErrBadKey)
	}

	got, rest, ok := strings.Cut(string(raw), "\x00")
	if !ok || got != devPrefix {
		return "", fmt.Errorf("%w: continuation token was minted for another prefix", ErrBadKey)
	}

	return rest, nil
}
