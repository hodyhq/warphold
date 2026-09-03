// Command spike-append-only is a throwaway, unauthenticated, minimal S3 store used by
// the Plan 3 Task 1 spike to record exactly which requests stock Kopia issues against
// S3 storage -- in particular which keys it DELETEs and which it overwrites.
//
// It is documentation, not production code: there is no authentication, no signature
// verification and no access control, so it must only ever be bound to loopback.
// The gateway it informs (fleet/gateway) is a different, authenticated implementation.
//
// Usage:
//
//	go run ./scripts/spike-append-only -addr 127.0.0.1:9401 -dir /tmp/spike-store
//
// Every request appends one line to <dir>/requests.log:
//
//	<verb> <key> exists=<bool> status=<code> content-sha256=<header> len=<n>
package main

import (
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"crypto/md5" //nolint:gosec // ETag is defined as MD5 by S3; not a security use.

	"errors"
)

// streamingPayload is the x-amz-content-sha256 value minio-go sends over plain HTTP.
const streamingPayload = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"

type store struct {
	dir string

	mu  sync.Mutex
	log *os.File
}

// keyPath maps an S3 object key to a file under the store directory. Slashes in the
// key become a single flat file name so that "a/b" and "a%2Fb" cannot collide with a
// directory; the spike never needs real hierarchy.
func (s *store) keyPath(key string) string {
	return filepath.Join(s.dir, "obj", strings.ReplaceAll(key, "/", "%2F"))
}

func (s *store) record(verb, key string, existed bool, status int, sha string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fmt.Fprintf(s.log, "%s %s exists=%t status=%d content-sha256=%s len=%d\n",
		verb, key, existed, status, sha, n)
	s.log.Sync() //nolint:errcheck
}

type listEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type listResult struct {
	XMLName               xml.Name    `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	Name                  string      `xml:"Name"`
	Prefix                string      `xml:"Prefix"`
	Delimiter             string      `xml:"Delimiter,omitempty"`
	MaxKeys               int         `xml:"MaxKeys"`
	KeyCount              int         `xml:"KeyCount"`
	IsTruncated           bool        `xml:"IsTruncated"`
	NextContinuationToken string      `xml:"NextContinuationToken,omitempty"`
	Contents              []listEntry `xml:"Contents"`
}

func (s *store) objects() ([]listEntry, error) {
	ents, err := os.ReadDir(filepath.Join(s.dir, "obj"))
	if err != nil {
		return nil, err
	}

	out := make([]listEntry, 0, len(ents))

	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}

		body, err := os.ReadFile(filepath.Join(s.dir, "obj", e.Name()))
		if err != nil {
			return nil, err
		}

		sum := md5.Sum(body) //nolint:gosec

		out = append(out, listEntry{
			Key:          strings.ReplaceAll(e.Name(), "%2F", "/"),
			LastModified: fi.ModTime().UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         `"` + hex.EncodeToString(sum[:]) + `"`,
			Size:         fi.Size(),
			StorageClass: "STANDARD",
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })

	return out, nil
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprint(w, xml.Header)
	xml.NewEncoder(w).Encode(v) //nolint:errcheck,errchkjson
}

type s3Error struct {
	XMLName xml.Name `xml:"Error"`
	Code    string
	Message string
	Key     string `xml:",omitempty"`
}

func (s *store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sha := r.Header.Get("x-amz-content-sha256")
	if sha == "" {
		sha = "-"
	}

	// The bucket is the first path segment; everything after it is the object key.
	p := strings.TrimPrefix(r.URL.Path, "/")

	bucket, key, _ := strings.Cut(p, "/")
	q := r.URL.Query()

	switch {
	case r.Method == http.MethodGet && key == "" && q.Has("versioning"):
		s.record("GET-versioning", bucket, false, http.StatusOK, sha, 0)
		writeXML(w, http.StatusOK, struct {
			XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ VersioningConfiguration"`
			Status  string   `xml:"Status,omitempty"`
		}{})

	case r.Method == http.MethodGet && key == "" && q.Has("location"):
		s.record("GET-location", bucket, false, http.StatusOK, sha, 0)
		writeXML(w, http.StatusOK, struct {
			XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ LocationConstraint"`
			Value   string   `xml:",chardata"`
		}{Value: "us-east-1"})

	case r.Method == http.MethodGet && key == "":
		s.list(w, r, bucket, sha)

	case r.Method == http.MethodHead && key == "":
		// HeadBucket -- bucket existence probe.
		s.record("HEAD-bucket", bucket, true, http.StatusOK, sha, 0)
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodPost:
		// Multipart initiation (?uploads) or bulk delete (?delete) would land here.
		s.record("POST["+r.URL.RawQuery+"]", key, false, http.StatusNotImplemented, sha, 0)
		writeXML(w, http.StatusNotImplemented, s3Error{Code: "NotImplemented", Message: "POST not implemented", Key: key})

	case r.Method == http.MethodPut && q.Has("retention"):
		s.record("PUT-retention", key, s.exists(key), http.StatusNotImplemented, sha, 0)
		writeXML(w, http.StatusNotImplemented, s3Error{Code: "NotImplemented", Message: "object lock not supported", Key: key})

	case r.Method == http.MethodPut:
		s.put(w, r, key, sha)

	case r.Method == http.MethodGet, r.Method == http.MethodHead:
		s.get(w, r, key, sha)

	case r.Method == http.MethodDelete:
		existed := s.exists(key)
		os.Remove(s.keyPath(key)) //nolint:errcheck
		s.record("DELETE", key, existed, http.StatusNoContent, sha, 0)
		w.WriteHeader(http.StatusNoContent)

	default:
		s.record(r.Method+"?", key, false, http.StatusNotImplemented, sha, 0)
		writeXML(w, http.StatusNotImplemented, s3Error{Code: "NotImplemented", Message: r.Method, Key: key})
	}
}

func (s *store) exists(key string) bool {
	_, err := os.Stat(s.keyPath(key))
	return err == nil
}

// decodeAWSChunked strips the aws-chunked framing that minio-go emits when it signs
// a payload with STREAMING-AWS4-HMAC-SHA256-PAYLOAD. Each chunk is
// "<hex-size>;chunk-signature=<sig>\r\n<data>\r\n", terminated by a zero-size chunk.
// The spike does not verify the per-chunk signatures -- it only needs the bytes.
func decodeAWSChunked(raw []byte) ([]byte, error) {
	var out []byte

	for {
		i := indexCRLF(raw)
		if i < 0 {
			return nil, errors.New("aws-chunked: no chunk header")
		}

		hdr := string(raw[:i])
		raw = raw[i+2:]

		sizeHex, _, _ := strings.Cut(hdr, ";")

		n, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("aws-chunked: bad size %q: %w", sizeHex, err)
		}

		if n == 0 {
			return out, nil
		}

		if int64(len(raw)) < n+2 {
			return nil, errors.New("aws-chunked: short chunk")
		}

		out = append(out, raw[:n]...)
		raw = raw[n+2:]
	}
}

func indexCRLF(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == '\r' && b[i+1] == '\n' {
			return i
		}
	}

	return -1
}

func (s *store) put(w http.ResponseWriter, r *http.Request, key, sha string) {
	existed := s.exists(key)

	body, err := io.ReadAll(r.Body)
	if err == nil && sha == streamingPayload {
		body, err = decodeAWSChunked(body)
	}
	if err != nil {
		s.record("PUT", key, existed, http.StatusBadRequest, sha, 0)
		writeXML(w, http.StatusBadRequest, s3Error{Code: "IncompleteBody", Message: err.Error(), Key: key})

		return
	}

	if err := os.WriteFile(s.keyPath(key), body, 0o600); err != nil {
		s.record("PUT", key, existed, http.StatusInternalServerError, sha, int64(len(body)))
		writeXML(w, http.StatusInternalServerError, s3Error{Code: "InternalError", Message: err.Error(), Key: key})

		return
	}

	sum := md5.Sum(body) //nolint:gosec
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	s.record("PUT", key, existed, http.StatusOK, sha, int64(len(body)))
	w.WriteHeader(http.StatusOK)
}

func (s *store) get(w http.ResponseWriter, r *http.Request, key, sha string) {
	verb := r.Method
	if rng := r.Header.Get("Range"); rng != "" {
		verb += "[" + rng + "]"
	}

	body, err := os.ReadFile(s.keyPath(key))
	if err != nil {
		s.record(verb, key, false, http.StatusNotFound, sha, 0)
		writeXML(w, http.StatusNotFound, s3Error{Code: "NoSuchKey", Message: "not found", Key: key})

		return
	}

	fi, err := os.Stat(s.keyPath(key))
	if err != nil {
		s.record(verb, key, false, http.StatusNotFound, sha, 0)
		writeXML(w, http.StatusNotFound, s3Error{Code: "NoSuchKey", Message: "not found", Key: key})

		return
	}

	sum := md5.Sum(body) //nolint:gosec
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.Header().Set("Last-Modified", fi.ModTime().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", "application/x-kopia")
	s.record(verb, key, true, http.StatusOK, sha, int64(len(body)))
	// http.ServeContent handles Range, 206 and Content-Length for us.
	http.ServeContent(w, r, key, fi.ModTime(), strings.NewReader(string(body)))
}

func (s *store) list(w http.ResponseWriter, r *http.Request, bucket, sha string) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delim := q.Get("delimiter")

	maxKeys := 1000
	if v, err := strconv.Atoi(q.Get("max-keys")); err == nil && v > 0 {
		maxKeys = v
	}

	all, err := s.objects()
	if err != nil {
		s.record("LIST", "?"+r.URL.RawQuery, false, http.StatusInternalServerError, sha, 0)
		writeXML(w, http.StatusInternalServerError, s3Error{Code: "InternalError", Message: err.Error()})

		return
	}

	var matched []listEntry

	for _, e := range all {
		if !strings.HasPrefix(e.Key, prefix) {
			continue
		}
		// A delimiter would normally roll deeper keys into CommonPrefixes. Kopia's blob
		// names are flat under its own prefix, so the spike records the delimiter it was
		// sent (below) and otherwise returns every matching key.
		matched = append(matched, e)
	}

	res := listResult{
		Name:      bucket,
		Prefix:    prefix,
		Delimiter: delim,
		MaxKeys:   maxKeys,
	}

	if len(matched) > maxKeys {
		matched = matched[:maxKeys]
		res.IsTruncated = true
		res.NextContinuationToken = matched[len(matched)-1].Key
	}

	res.Contents = matched
	res.KeyCount = len(matched)

	s.record("LIST["+r.URL.RawQuery+"]", prefix, false, http.StatusOK, sha, int64(len(matched)))
	writeXML(w, http.StatusOK, res)
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9401", "loopback address to listen on")
	dir := flag.String("dir", "", "store directory (required)")
	certFile := flag.String("tls-cert", "", "optional TLS certificate; serves HTTPS when set (with -tls-key)")
	keyFile := flag.String("tls-key", "", "optional TLS key")
	flag.Parse()

	if *dir == "" {
		log.Fatal("-dir is required")
	}

	if !strings.HasPrefix(*addr, "127.0.0.1:") && !strings.HasPrefix(*addr, "localhost:") {
		log.Fatalf("refusing to listen on %q: this store has no authentication, loopback only", *addr)
	}

	if err := os.MkdirAll(filepath.Join(*dir, "obj"), 0o700); err != nil {
		log.Fatal(err)
	}

	f, err := os.OpenFile(filepath.Join(*dir, "requests.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close() //nolint:errcheck

	s := &store{dir: *dir, log: f}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           s,
		ReadHeaderTimeout: 30 * time.Second,
	}

	if *certFile != "" {
		log.Printf("spike S3 store listening on https://%s, dir=%s", *addr, *dir)
		log.Fatal(srv.ListenAndServeTLS(*certFile, *keyFile))
	}

	log.Printf("spike S3 store listening on http://%s, dir=%s", *addr, *dir)
	log.Fatal(srv.ListenAndServe())
}
