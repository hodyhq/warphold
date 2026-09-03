package gateway

import (
	"context"
	"crypto/md5" //nolint:gosec // S3 defines ETag as MD5; it is an identifier here, not a security control.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/repo/blob"
)

// cloud is the cloud-direct backend (spec §5, D5): the Fleet writes through to
// the customer's own B2 or S3 bucket with the fleet's admin credentials, so a
// device never holds a cloud key. It is built on repo/blob so both providers -
// and every other one Kopia supports - share a single code path.
//
// Keys are laid out as <bucket>/<root-prefix>/<device-id>/<key>: the root
// prefix belongs to the fleet and is invisible to callers, the device prefix is
// part of the key and is enforced by NormalizeKey in the handler. One bucket
// can therefore hold every device of every fleet that shares it.
type cloud struct {
	st blob.Storage

	// prefix is the fleet's root prefix inside the bucket: "" or slash-terminated.
	prefix string
}

// maxCloudPut caps a single write-through PUT, because blob.Bytes needs the
// whole object in memory before the provider will take it. Kopia's default
// pack blob is 20 MB and the largest PUT observed in the D3 spike was 21.4 MB,
// so this is 3x headroom - but it is a ceiling, and a device that raises its
// pack size past it gets a 400 rather than the fleet an OOM.
//
// ponytail: hard cap. Raising it means a multipart/streaming upload, not a
// bigger buffer.
const maxCloudPut = 64 << 20

// NewCloud returns an ObjectStore writing through to a B2 or S3 bucket with the
// fleet's admin credentials.
//
// ci arrives already unsealed from the credential store. It is never logged and
// never returned in an error - only the provider's own errors are wrapped.
//
// Append-only: neither the S3 nor the B2 provider honours blob.PutOptions'
// DoNotRecreate (both reject it outright), so Put falls back to a Head before
// the write. A race is possible in theory and impossible in practice - one
// device owns its prefix - and the loser is a 409, never a lost blob. On a
// provider that does support DoNotRecreate (GCS, Azure) the conditional put is
// used instead and the race does not exist at all.
func NewCloud(ctx context.Context, ci blob.ConnectionInfo, prefix string) (ObjectStore, error) {
	st, err := blob.NewStorage(ctx, ci, false)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s storage: %w", ci.Type, err)
	}

	c, err := newCloud(st, prefix)
	if err != nil {
		st.Close(ctx) //nolint:errcheck // best effort on the error path

		return nil, err
	}

	return c, nil
}

// newCloud wraps an already-open blob.Storage. Split out so tests can drive a
// filesystem- or map-backed store without a network.
func newCloud(st blob.Storage, prefix string) (ObjectStore, error) {
	if prefix != "" {
		// Same rule as NormalizeKey: an unterminated prefix would let one
		// fleet's keys land inside another whose prefix merely starts the same.
		if !strings.HasSuffix(prefix, "/") {
			return nil, fmt.Errorf("%w: prefix %q does not end in a slash", ErrBadKey, prefix)
		}

		if err := checkKey(strings.TrimSuffix(prefix, "/")); err != nil {
			return nil, fmt.Errorf("prefix: %w", err)
		}
	}

	return &cloud{st: st, prefix: prefix}, nil
}

// blobID maps an object key to the blob ID under the bucket's root prefix.
func (c *cloud) blobID(key string) (blob.ID, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}

	full := c.prefix + key
	if len(full) > MaxKeyLen {
		return "", fmt.Errorf("%w: %d bytes with the fleet prefix, limit %d", ErrBadKey, len(full), MaxKeyLen)
	}

	return blob.ID(full), nil
}

func (c *cloud) Put(ctx context.Context, key string, r io.Reader, size int64, overwrite bool) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}

	id, err := c.blobID(key)
	if err != nil {
		return ObjectInfo{}, err
	}

	// ponytail: buffers the whole object in memory, because blob.Bytes needs a
	// length up front. Kopia caps a blob at a few tens of MB, so this is the
	// same working set the provider's own client would hold; if the gateway
	// ever accepts larger objects, switch to a multipart upload.
	buf := gather.NewWriteBuffer()
	defer buf.Close()

	if size > maxCloudPut {
		return ObjectInfo{}, fmt.Errorf("%w: %d bytes, limit %d", ErrBadKey, size, maxCloudPut)
	}

	// One byte past the limit catches a reader that delivers more than it
	// promised - or, for an unknown length, more than the fleet can hold -
	// without ever reading an unbounded body.
	limit := int64(maxCloudPut)
	if size >= 0 {
		limit = size
	}

	h := md5.New() //nolint:gosec // ETag, not a security control.

	n, err := io.Copy(io.MultiWriter(buf, h), io.LimitReader(r, limit+1))
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("reading object: %w", err)
	}

	switch {
	case size >= 0 && n != size:
		return ObjectInfo{}, fmt.Errorf("%w: declared %d bytes, got %d", ErrBadKey, size, n)
	case size < 0 && n > maxCloudPut:
		return ObjectInfo{}, fmt.Errorf("%w: over the %d byte limit for an upload of unknown length", ErrBadKey, maxCloudPut)
	}

	var mt time.Time

	opts := blob.PutOptions{DoNotRecreate: !overwrite, GetModTime: &mt}

	err = c.st.PutBlob(ctx, id, buf.Bytes(), opts)
	if errors.Is(err, blob.ErrUnsupportedPutBlobOption) {
		// S3 and B2 both refuse a conditional put, and they refuse it before
		// uploading anything, so the fallback costs a round trip, not a body.
		if err = c.headExists(ctx, id); err != nil {
			return ObjectInfo{}, err
		}

		opts.DoNotRecreate = false
		err = c.st.PutBlob(ctx, id, buf.Bytes(), opts)
	}

	switch {
	case errors.Is(err, blob.ErrBlobAlreadyExists):
		return ObjectInfo{}, ErrExists
	case err != nil:
		return ObjectInfo{}, fmt.Errorf("storing %q: %w", key, err)
	}

	if mt.IsZero() {
		// The server assigns the timestamp and not every provider reports it
		// back; the local clock is close enough for the Put response, and Head
		// always reads the provider's own value.
		mt = time.Now().UTC()
	}

	return ObjectInfo{Key: key, Size: n, ETag: hex.EncodeToString(h.Sum(nil)), LastModified: mt}, nil
}

// headExists is the non-atomic half of the append-only rule: ErrExists if the
// blob is already there, nil if it is not.
func (c *cloud) headExists(ctx context.Context, id blob.ID) error {
	switch _, err := c.st.GetMetadata(ctx, id); {
	case err == nil:
		return ErrExists
	case errors.Is(err, blob.ErrBlobNotFound):
		return nil
	default:
		return fmt.Errorf("checking for an existing object: %w", err)
	}
}

func (c *cloud) Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, ObjectInfo{}, err
	}

	info, err := c.Head(ctx, key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}

	size := info.Size

	// A range that starts at or past the end is 416; offset 0 is not a range at
	// all, so a zero-byte object still reads cleanly.
	if offset < 0 || (offset > 0 && offset >= size) {
		return nil, ObjectInfo{}, fmt.Errorf("%w: offset %d of %d bytes", ErrRange, offset, size)
	}

	// Written as a subtraction so a caller-supplied length cannot overflow. The
	// provider would reject a length past the end; the contract clamps it.
	if length < 0 || length > size-offset {
		length = size - offset
	}

	if length == 0 {
		return io.NopCloser(strings.NewReader("")), info, nil
	}

	id, err := c.blobID(key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}

	out := gather.NewWriteBuffer()

	if err := c.st.GetBlob(ctx, id, offset, length, out); err != nil {
		out.Close()

		return nil, ObjectInfo{}, mapGetErr(key, err)
	}

	return &bufReader{Reader: out.Bytes().Reader(), buf: out}, info, nil
}

// Head returns the object's metadata. ETag is empty: blob.Metadata carries no
// provider ETag, and B2's own checksum is a content SHA1, not the MD5 an S3
// client expects - an empty ETag is honest where a wrong one would be a silent
// corruption report. Put still fills it in, since it hashes as it streams.
func (c *cloud) Head(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}

	id, err := c.blobID(key)
	if err != nil {
		return ObjectInfo{}, err
	}

	md, err := c.st.GetMetadata(ctx, id)
	if err != nil {
		return ObjectInfo{}, mapGetErr(key, err)
	}

	return ObjectInfo{Key: key, Size: md.Length, LastModified: md.Timestamp}, nil
}

func (c *cloud) List(ctx context.Context, prefix, after string, max int) ([]ObjectInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	if max <= 0 || max > defaultMaxKeys {
		max = defaultMaxKeys
	}

	// prefix is a raw string prefix, exactly as S3 and the local backend treat
	// it: "dev1" matches "dev10/a" as well as "dev1/a". Confining a device to
	// its own keys is the handler's job (spec §4.1.5 rewrites an out-of-prefix
	// ListObjectsV2 prefix to "<device-id>/"), not this layer's.
	//
	// ponytail: collects the whole prefix, then sorts and slices, exactly like
	// the local backend. blob.Lister promises no ordering, so an early stop
	// would be wrong even though S3 and B2 both happen to list lexicographically;
	// if paging a very large prefix ever matters, drop to the provider's own
	// paginated list API.
	var objs []ObjectInfo

	err := c.st.ListBlobs(ctx, blob.ID(c.prefix+prefix), func(bm blob.Metadata) error {
		key, ok := strings.CutPrefix(string(bm.BlobID), c.prefix)
		if !ok || !strings.HasPrefix(key, prefix) || key <= after {
			return nil
		}

		objs = append(objs, ObjectInfo{Key: key, Size: bm.Length, LastModified: bm.Timestamp})

		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing %q: %w", prefix, err)
	}

	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })

	if len(objs) > max {
		return objs[:max], true, nil
	}

	return objs, false, nil
}

// Delete removes one object. It heads first because B2's DeleteBlob reports
// success for a blob that was never there, and the contract owes the handler a
// 404. The allowlist that decides what may be deleted lives in the handler.
func (c *cloud) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	id, err := c.blobID(key)
	if err != nil {
		return err
	}

	if _, err := c.st.GetMetadata(ctx, id); err != nil {
		return mapGetErr(key, err)
	}

	if err := c.st.DeleteBlob(ctx, id); err != nil {
		return mapGetErr(key, err)
	}

	return nil
}

// Versioned asks the provider. S3 answers from the bucket's own versioning
// configuration; a provider that cannot say is reported as unversioned, which
// is the answer that makes a client keep its own history.
func (c *cloud) Versioned(ctx context.Context) bool {
	v, ok := c.st.(interface {
		IsVersioned(ctx context.Context) (bool, error)
	})
	if !ok {
		return false
	}

	on, err := v.IsVersioned(ctx)

	return err == nil && on
}

// Close releases the provider connection. ObjectStore does not require it, so a
// caller that tears a target down type-asserts for it.
func (c *cloud) Close(ctx context.Context) error {
	return c.st.Close(ctx) //nolint:wrapcheck // pass the provider's error through unchanged
}

// mapGetErr translates the provider's sentinels into the store's own.
func mapGetErr(key string, err error) error {
	switch {
	case errors.Is(err, blob.ErrBlobNotFound):
		return ErrNotFound
	case errors.Is(err, blob.ErrInvalidRange):
		return fmt.Errorf("%w: %q", ErrRange, key)
	default:
		return fmt.Errorf("reading %q: %w", key, err)
	}
}

// bufReader hands out the fetched range and releases the buffer on Close.
type bufReader struct {
	io.Reader

	buf *gather.WriteBuffer
}

func (b *bufReader) Close() error {
	b.buf.Close()

	return nil
}
