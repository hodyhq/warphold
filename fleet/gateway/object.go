// Package gateway implements the S3-compatible storage gateway that hosted
// targets expose to devices (spec §4). This file is the storage boundary: every
// object key a device sends passes through NormalizeKey before any backend sees
// it, and the ObjectStore contract is append-only by default (§4.2).
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxKeyLen is the longest object key we accept, matching S3's own limit.
const MaxKeyLen = 1024

// tmpDir is reserved: it is where backends stage partial writes, so no device
// may own a key under it.
const tmpDir = ".tmp"

// ObjectInfo is what HEAD and LIST return.
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string // hex MD5 of the stored bytes, quoted by the XML layer
	LastModified time.Time
}

// ErrExists is returned by Put when the key is already present and the caller
// did not pass overwrite (the append-only rule, §4.2).
var ErrExists = errors.New("object already exists")

// ErrNotFound is returned by Get, Head and Delete for a missing key.
var ErrNotFound = errors.New("object not found")

// ErrRange is returned by Get when offset/length fall outside the object; the
// XML layer turns it into 416 InvalidRange.
var ErrRange = errors.New("invalid range")

// ErrBadKey is returned for a key that is not a valid, confined object key. It
// wraps every rejection from NormalizeKey so callers can answer 400 without
// matching on strings.
var ErrBadKey = errors.New("invalid object key")

// ErrTooLarge is returned by Put when the body exceeds the store's object-size
// limit, whether the caller declared the size or streamed it. 413.
var ErrTooLarge = errors.New("object too large")

// ErrIncompleteBody is returned by Put when the body does not deliver exactly
// the declared number of bytes. 400 IncompleteBody.
var ErrIncompleteBody = errors.New("body does not match the declared size")

// ObjectStore is the subset of S3 the gateway needs, over one backing store.
//
// Keys are flat: exactly "<device-id>/<blob-name>" (§4.3). There is no object
// hierarchy below the device, so a device can never make the server create an
// arbitrarily deep directory tree.
//
// ETag is filled in by Put (which hashes as it streams, so it is free there)
// and by Head, which is the guarantee. On Get it is best-effort: a backend that
// already holds the digest may pass it on - the cloud backend does, since the
// provider returns it with the metadata - but the local backend leaves it
// empty. List always leaves it empty rather than reading every object back only
// to hash it.
type ObjectStore interface {
	// Put stores r under key. size is the expected byte count, or -1 if it is
	// unknown; a reader that delivers a different count is ErrIncompleteBody
	// and stores nothing. Without overwrite, an existing key is ErrExists and
	// its bytes are left untouched.
	Put(ctx context.Context, key string, r io.Reader, size int64, overwrite bool) (ObjectInfo, error)

	// Get returns the bytes at [offset, offset+length), where length < 0 means
	// "to the end" and a length past the end is clamped. The ObjectInfo
	// describes the whole object, not the returned range; its ETag may be empty
	// - call Head when it is required.
	Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, ObjectInfo, error)

	// Head returns the object's metadata, including its ETag.
	Head(ctx context.Context, key string) (ObjectInfo, error)

	// List returns objects under prefix in lexicographic order, starting after
	// the key after (exclusive) and capped at max. truncated reports whether
	// more objects follow the last one returned. ETag is left empty.
	List(ctx context.Context, prefix, after string, max int) (objs []ObjectInfo, truncated bool, err error)

	// Delete removes exactly one object. A missing key is ErrNotFound.
	Delete(ctx context.Context, key string) error

	// Versioned reports whether the backing store keeps object versions, which
	// is what GetBucketVersioning answers.
	Versioned(ctx context.Context) bool
}

// A backend that holds a connection also implements
//
//	interface{ Close(context.Context) error }
//
// which a caller must type-assert for and call when a target is deconfigured.
// The local backend does not need it; the cloud backend releases its HTTP
// client's idle connections.

// NormalizeKey validates an S3 object key and confines it to prefix.
// It rejects: an empty key, a leading '/', any "." or ".." segment, an empty
// segment, any byte < 0x20 or 0x7f, a key over 1024 bytes, and any key that
// does not start with prefix. The returned key is the input unchanged - it
// never rewrites, so the caller cannot be surprised by a different key.
//
// It also rejects, for the reasons in checkKey: invalid UTF-8, C1 controls,
// backslashes, a second '/' (the key space is flat, §4.3), and anything under
// the reserved ".tmp" directory.
//
// A non-empty prefix must end in '/'. Otherwise "abc123" would confine a device
// to its own keys and to every device whose id merely starts with "abc123".
func NormalizeKey(key, prefix string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}

	if prefix != "" {
		if !strings.HasSuffix(prefix, "/") {
			return "", fmt.Errorf("%w: prefix %q does not end in a slash", ErrBadKey, prefix)
		}

		if strings.TrimSuffix(prefix, "/") == tmpDir {
			return "", fmt.Errorf("%w: prefix %q is reserved", ErrBadKey, prefix)
		}
	}

	if !strings.HasPrefix(key, prefix) {
		return "", fmt.Errorf("%w: %q is outside %q", ErrBadKey, key, prefix)
	}

	return key, nil
}

// checkKey is the prefix-independent half of NormalizeKey. Backends call it
// too: a key must be safe on its own, whichever caller produced it.
func checkKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: empty", ErrBadKey)
	case len(key) > MaxKeyLen:
		return fmt.Errorf("%w: %d bytes, limit %d", ErrBadKey, len(key), MaxKeyLen)
	case key[0] == '/':
		return fmt.Errorf("%w: %q is absolute", ErrBadKey, key)
	case !utf8.ValidString(key):
		return fmt.Errorf("%w: not valid UTF-8", ErrBadKey)
	}

	for _, r := range key {
		// C0 and C1 controls and DEL have no place in a key and are how a log
		// line or an XML response gets forged. Backslash is rejected because it
		// is a path separator on Windows, so a segment check that splits only
		// on '/' would miss `..\` traversal.
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == '\\' {
			return fmt.Errorf("%w: %q contains a forbidden character", ErrBadKey, key)
		}
	}

	// The key space is flat: "<device-id>/<blob-name>", nothing deeper (§4.3).
	device, name, found := strings.Cut(key, "/")
	if !found || strings.Contains(name, "/") {
		return fmt.Errorf("%w: %q is not <device-id>/<blob-name>", ErrBadKey, key)
	}

	for _, seg := range []string{device, name} {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: %q has an empty or relative segment", ErrBadKey, key)
		}
	}

	if device == tmpDir {
		return fmt.Errorf("%w: %q is reserved", ErrBadKey, key)
	}

	return nil
}
