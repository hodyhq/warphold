package gateway

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// S3 error codes the gateway emits. The two AppendOnly* codes are WarpHold's
// own: they are not S3 codes, deliberately, so a device's log names the rule it
// hit (docs/RECONCILE-append-only.md) instead of a generic AccessDenied.
const (
	codeAccessDenied         = "AccessDenied"
	codeNoSuchBucket         = "NoSuchBucket"
	codeNoSuchKey            = "NoSuchKey"
	codeInvalidRange         = "InvalidRange"
	codeNotImplemented       = "NotImplemented"
	codeSlowDown             = "SlowDown"
	codeBadDigest            = "BadDigest"
	codeMissingContentMD5    = "MissingContentMD5"
	codeEntityTooLarge       = "EntityTooLarge"
	codeMissingContentLength = "MissingContentLength"
	codeIncompleteBody       = "IncompleteBody"
	codeInvalidObjectKey     = "InvalidObjectKey"
	codeInternalError        = "InternalError"

	codeAuthorizationHeaderMalformed = "AuthorizationHeaderMalformed"
	codeInvalidURI                   = "InvalidURI"
	codeRequestTimeTooSkewed         = "RequestTimeTooSkewed"
	codeInvalidAccessKeyID           = "InvalidAccessKeyId"
	codeSignatureDoesNotMatch        = "SignatureDoesNotMatch"

	codeAppendOnlyDeleteDenied    = "AppendOnlyDeleteDenied"
	codeAppendOnlyOverwriteDenied = "AppendOnlyOverwriteDenied"
)

// errorResponse is S3's error document.
type errorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string
	Message   string
	Resource  string `xml:",omitempty"`
	RequestID string `xml:"RequestId,omitempty"`
}

// listBucketResult is ListObjectsV2's response.
type listBucketResult struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`

	Name                  string
	Prefix                string
	StartAfter            string `xml:",omitempty"`
	ContinuationToken     string `xml:",omitempty"`
	NextContinuationToken string `xml:",omitempty"`
	KeyCount              int
	MaxKeys               int
	Delimiter             string `xml:",omitempty"`
	EncodingType          string `xml:"EncodingType,omitempty"`
	IsTruncated           bool
	Contents              []objectEntry  `xml:"Contents"`
	CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
}

type objectEntry struct {
	Key          string
	LastModified string
	ETag         string `xml:",omitempty"`
	Size         int64
	StorageClass string
	Owner        *owner `xml:",omitempty"`
}

type owner struct {
	ID          string
	DisplayName string
}

type commonPrefix struct{ Prefix string }

// locationConstraint answers GetBucketLocation, which minio-go issues whenever
// the client was configured without a region (RECONCILE §5.4).
type locationConstraint struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ LocationConstraint"`
	Value   string   `xml:",chardata"`
}

// writeXML sends an XML document with the S3 preamble.
func writeXML(w http.ResponseWriter, status int, v any) {
	body, err := xml.Marshal(v)
	if err != nil {
		// Marshalling our own structs cannot fail in practice; if it ever did,
		// a bare status is still a valid S3 answer.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(len(xml.Header)+len(body)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

// writeError sends an S3 error document, or just the status for a HEAD, which
// must not carry a body.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	if r.Method == http.MethodHead {
		w.Header().Set("X-Warphold-Error-Code", code)
		w.WriteHeader(status)

		return
	}

	writeXML(w, status, errorResponse{Code: code, Message: msg, Resource: r.URL.Path})
}

// NotActivatedHandler answers every request with 403 AccessDenied in S3's XML
// shape. The Fleet server serves it until activation creates the device-key
// store, so an unactivated server is indistinguishable from one that holds no
// keys for the caller.
func NotActivatedHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusForbidden, codeAccessDenied, "access denied")
	})
}

// s3Time is the format ListObjectsV2 uses for LastModified.
func s3Time(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// urlEncodeName encodes a key for an encoding-type=url response. It is
// url.QueryEscape with "+" spelled "%20", which is what S3 sends and what
// minio-go's url.QueryUnescape reads back unchanged.
func urlEncodeName(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}
