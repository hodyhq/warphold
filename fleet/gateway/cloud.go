package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
)

const (
	// maxCloudPut caps a single write-through PUT. Kopia's default pack blob is
	// 20 MB and the largest PUT observed in the D3 spike was 21.4 MB, so this is
	// 3x headroom - but it is a ceiling, and a device that raises its pack size
	// past it gets a 400 rather than the fleet an OOM.
	//
	// ponytail: hard cap. Raising it means a multipart upload, not a bigger buffer.
	maxCloudPut = 64 << 20

	// spoolAbove is where a PUT stops being buffered in RAM and starts being
	// spooled to a 0600 temp file. A conditional PUT may have to be replayed and
	// its length is needed up front, so the body is always materialised - the
	// only question is where.
	spoolAbove = 8 << 20

	// probeDir holds ProbeConditionalPut's throwaway objects. It sits at the
	// connection's own prefix and can never collide with a device key, which is
	// always confined to "<device-id>/".
	probeDir = ".warphold-probe/"
)

// ErrNoConditionalPut is returned by ProbeConditionalPut when the provider
// accepts a second write that carried If-None-Match: *. Such a store cannot
// enforce the append-only rule, so it must not back a hosted target.
var ErrNoConditionalPut = errors.New("provider does not enforce conditional writes; use Fleet disk + mirror")

// cloud is the cloud-direct backend (spec §5, D5): the Fleet writes through to
// the customer's own bucket with the fleet's admin credentials, so a device
// never holds a cloud key.
//
// Keys are laid out as <bucket>/<root-prefix>/<device-id>/<key>: the root
// prefix belongs to the fleet, is invisible to callers, and the device prefix
// is part of the key, so one bucket can hold every device.
type cloud struct {
	cli *minio.Client

	bucket string

	// prefix is the fleet's root prefix inside the bucket, including whatever
	// prefix the connection info itself carried: "" or slash-terminated.
	prefix string
}

// NewCloud returns an ObjectStore writing through to an S3-compatible bucket
// with the fleet's admin credentials.
//
// ci arrives already unsealed from the credential store. It is never logged and
// never returned in an error.
//
// **Append-only is atomic**, not check-then-write: every Put that is not an
// overwrite carries `If-None-Match: *`, and a provider that already holds the
// key answers 412, which becomes ErrExists. That is a guarantee only where the
// provider enforces the precondition - AWS S3 does, and Backblaze B2's
// S3-compatible endpoint documents it (**verify against a real bucket at Task
// 12**). ProbeConditionalPut is how a target proves it before it is created; a
// store that ignores the precondition is refused rather than silently degraded.
//
// Only `Type == "s3"` is accepted. B2 is reached through its S3-compatible
// endpoint (s3.<region>.backblazeb2.com), because B2's native API has no
// conditional write at all.
func NewCloud(ctx context.Context, ci blob.ConnectionInfo, prefix string) (ObjectStore, error) {
	cli, opt, err := s3Client(ci)
	if err != nil {
		return nil, err
	}

	root := opt.Prefix + prefix
	if root != "" {
		// Same rule as NormalizeKey: an unterminated prefix would let one
		// fleet's keys land inside another whose prefix merely starts the same.
		if !strings.HasSuffix(root, "/") {
			return nil, fmt.Errorf("%w: prefix %q does not end in a slash", ErrBadKey, root)
		}

		if err := checkKey(strings.TrimSuffix(root, "/")); err != nil {
			return nil, fmt.Errorf("prefix: %w", err)
		}
	}

	return &cloud{cli: cli, bucket: opt.BucketName, prefix: root}, nil
}

// s3Client builds a minio client from an unsealed connection info. It refuses
// anything it cannot serve, rather than degrading quietly.
func s3Client(ci blob.ConnectionInfo) (*minio.Client, *s3.Options, error) {
	if ci.Type == "b2" {
		return nil, nil, errors.New("cloud-direct needs B2's S3-compatible endpoint (s3.<region>.backblazeb2.com) as an s3 target: the native B2 API has no conditional write, so it cannot enforce the append-only rule")
	}

	opt, ok := ci.Config.(*s3.Options)
	if ci.Type != "s3" || !ok {
		return nil, nil, fmt.Errorf("cloud-direct needs an s3 target, got %q", ci.Type)
	}

	switch {
	case opt.BucketName == "":
		return nil, nil, errors.New("cloud-direct needs a bucket name")
	case opt.AccessKeyID == "" || opt.SecretAccessKey == "":
		return nil, nil, errors.New("cloud-direct needs the fleet's admin key id and secret")
	case opt.RoleARN != "" || opt.WebIdentityToken != "" || opt.WebIdentityTokenFile != "":
		return nil, nil, errors.New("cloud-direct uses a static admin key; assumed-role and web-identity credentials are not supported")
	}

	mo := &minio.Options{
		Creds:  credentials.NewStaticV4(opt.AccessKeyID, opt.SecretAccessKey, opt.SessionToken),
		Secure: !opt.DoNotUseTLS,
		Region: opt.Region,
	}

	if len(opt.RootCA) > 0 || opt.DoNotVerifyTLS {
		tr, err := minio.DefaultTransport(!opt.DoNotUseTLS)
		if err != nil {
			return nil, nil, fmt.Errorf("building the HTTP transport: %w", err)
		}

		//nolint:gosec // mirrors the s3 provider's own DoNotVerifyTLS option, which is the admin's explicit choice.
		tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: opt.DoNotVerifyTLS}

		if len(opt.RootCA) > 0 {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(opt.RootCA) {
				return nil, nil, errors.New("the configured root CA is not a PEM certificate")
			}

			tc.RootCAs = pool
		}

		tr.TLSClientConfig = tc
		mo.Transport = tr
	}

	cli, err := minio.New(opt.Endpoint, mo)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to %s: %w", opt.Endpoint, err)
	}

	return cli, opt, nil
}

// ProbeConditionalPut proves that a bucket enforces If-None-Match: * before a
// hosted target is created on it (Task 12 calls it alongside the Object Lock
// check). It writes a throwaway key twice, expects the second write to be
// refused with 412, and deletes the probe either way.
func ProbeConditionalPut(ctx context.Context, ci blob.ConnectionInfo) error {
	cli, opt, err := s3Client(ci)
	if err != nil {
		return err
	}

	name := opt.Prefix + probeDir + rand.Text()

	defer cli.RemoveObject(context.WithoutCancel(ctx), opt.BucketName, name, minio.RemoveObjectOptions{}) //nolint:errcheck // best-effort cleanup

	if _, err := cli.PutObject(ctx, opt.BucketName, name, strings.NewReader("warphold"), 8, condPut()); err != nil {
		return fmt.Errorf("writing the conditional-write probe: %w", err)
	}

	switch _, err := cli.PutObject(ctx, opt.BucketName, name, strings.NewReader("warphold"), 8, condPut()); {
	case minio.ToErrorResponse(err).StatusCode == http.StatusPreconditionFailed:
		return nil
	case err != nil:
		return fmt.Errorf("re-writing the conditional-write probe: %w", err)
	default:
		return ErrNoConditionalPut
	}
}

// condPut is a PUT that must not replace an existing object.
func condPut() minio.PutObjectOptions {
	o := minio.PutObjectOptions{
		ContentType: "application/x-kopia",
		// Kopia already splits data into small blobs, and a multipart ETag is
		// not an MD5, so a single PUT keeps the ETag meaningful.
		DisableMultipart: true,
		SendContentMd5:   true,
	}
	o.SetMatchETagExcept("*")

	return o
}

// objectName maps an object key to its name under the bucket's root prefix.
func (c *cloud) objectName(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}

	full := c.prefix + key
	if len(full) > MaxKeyLen {
		return "", fmt.Errorf("%w: %d bytes with the fleet prefix, limit %d", ErrBadKey, len(full), MaxKeyLen)
	}

	return full, nil
}

func (c *cloud) Put(ctx context.Context, key string, r io.Reader, size int64, overwrite bool) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}

	name, err := c.objectName(key)
	if err != nil {
		return ObjectInfo{}, err
	}

	if size > maxCloudPut {
		return ObjectInfo{}, fmt.Errorf("%w: %d bytes, limit %d", ErrBadKey, size, maxCloudPut)
	}

	body, n, cleanup, err := spool(r, size)
	defer cleanup()

	if err != nil {
		return ObjectInfo{}, err
	}

	opts := condPut()
	if overwrite {
		opts = minio.PutObjectOptions{ContentType: "application/x-kopia", DisableMultipart: true, SendContentMd5: true}
	}

	ui, err := c.cli.PutObject(ctx, c.bucket, name, body, n, opts)
	if err != nil {
		if minio.ToErrorResponse(err).StatusCode == http.StatusPreconditionFailed {
			return ObjectInfo{}, ErrExists
		}

		return ObjectInfo{}, fmt.Errorf("storing %q: %w", key, err)
	}

	mt := ui.LastModified
	if mt.IsZero() {
		// Not every provider echoes the timestamp on a PUT; the local clock is
		// close enough here, and Head always reads the provider's own value.
		mt = time.Now().UTC()
	}

	return ObjectInfo{Key: key, Size: n, ETag: unquote(ui.ETag), LastModified: mt}, nil
}

// spool materialises r, because a PUT needs its length up front and a
// conditional PUT may have to be replayed. Small bodies stay in memory; a big
// or unknown-length one goes to a 0600 temp file that is removed on every path.
// A body that does not match its declared length - or an unknown-length one
// that runs past the cap - is rejected before anything is stored.
func spool(r io.Reader, size int64) (body io.Reader, n int64, cleanup func(), err error) {
	cleanup = func() {}

	limit := int64(maxCloudPut)
	if size >= 0 {
		limit = size
	}

	// One byte past the limit catches a reader that delivers more than it
	// promised without ever reading an unbounded body.
	src := io.LimitReader(r, limit+1)

	if size >= 0 && size <= spoolAbove {
		b, err := io.ReadAll(src)
		if err != nil {
			return nil, 0, cleanup, fmt.Errorf("reading object: %w", err)
		}

		if int64(len(b)) != size {
			return nil, 0, cleanup, fmt.Errorf("%w: declared %d bytes, got %d", ErrBadKey, size, len(b))
		}

		return bytes.NewReader(b), size, cleanup, nil
	}

	// os.CreateTemp opens with 0600, which is what a spooled device blob needs.
	f, err := os.CreateTemp("", "warphold-put-*")
	if err != nil {
		return nil, 0, cleanup, fmt.Errorf("creating a spool file: %w", err)
	}

	cleanup = func() {
		f.Close()           //nolint:errcheck // best-effort cleanup
		os.Remove(f.Name()) //nolint:errcheck // best-effort cleanup
	}

	n, err = io.Copy(f, src)
	if err != nil {
		return nil, 0, cleanup, fmt.Errorf("spooling object: %w", err)
	}

	switch {
	case size >= 0 && n != size:
		return nil, 0, cleanup, fmt.Errorf("%w: declared %d bytes, got %d", ErrBadKey, size, n)
	case size < 0 && n > maxCloudPut:
		return nil, 0, cleanup, fmt.Errorf("%w: over the %d byte limit for an upload of unknown length", ErrBadKey, maxCloudPut)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, cleanup, fmt.Errorf("rewinding the spool file: %w", err)
	}

	return f, n, cleanup, nil
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
	// provider would answer 416; the contract clamps instead.
	if length < 0 || length > size-offset {
		length = size - offset
	}

	if length == 0 {
		return io.NopCloser(strings.NewReader("")), info, nil
	}

	var opts minio.GetObjectOptions
	if err := opts.SetRange(offset, offset+length-1); err != nil {
		return nil, ObjectInfo{}, fmt.Errorf("%w: %d+%d of %q", ErrRange, offset, length, key)
	}

	name, err := c.objectName(key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}

	obj, err := c.cli.GetObject(ctx, c.bucket, name, opts)
	if err != nil {
		return nil, ObjectInfo{}, mapErr(key, err)
	}

	return readCloser{Reader: io.LimitReader(obj, length), Closer: obj}, info, nil
}

// Head returns the object's metadata, including the provider's own ETag - which
// is the MD5 for the single-part uploads this backend makes, and is exactly
// what an S3 client expects. Put reports the same value from its upload result;
// List leaves it empty rather than heading every key.
func (c *cloud) Head(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}

	name, err := c.objectName(key)
	if err != nil {
		return ObjectInfo{}, err
	}

	st, err := c.cli.StatObject(ctx, c.bucket, name, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, mapErr(key, err)
	}

	return ObjectInfo{Key: key, Size: st.Size, ETag: unquote(st.ETag), LastModified: st.LastModified}, nil
}

// List returns objects under prefix, using S3's own listing, which is
// lexicographic and whose start-after is exclusive - the same order and
// exclusivity the local backend produces.
//
// prefix is a raw string prefix, exactly as S3 and the local backend treat it:
// "dev1" matches "dev10/a" as well as "dev1/a". Confining a device to its own
// keys is the handler's job (spec §4.1.5 rewrites an out-of-prefix
// ListObjectsV2 prefix to "<device-id>/"), not this layer's.
func (c *cloud) List(ctx context.Context, prefix, after string, max int) ([]ObjectInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	if max <= 0 || max > defaultMaxKeys {
		max = defaultMaxKeys
	}

	// Cancelled on return so the provider's listing goroutine cannot outlive
	// this call when the page fills early.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	opts := minio.ListObjectsOptions{
		Prefix:    c.prefix + prefix,
		Recursive: true,
		// One past the page: the extra key is what says "truncated".
		MaxKeys: max + 1,
	}

	if after != "" {
		opts.StartAfter = c.prefix + after
	}

	objs := make([]ObjectInfo, 0, max)

	for o := range c.cli.ListObjects(ctx, c.bucket, opts) {
		if o.Err != nil {
			return nil, false, fmt.Errorf("listing %q: %w", prefix, o.Err)
		}

		key, ok := strings.CutPrefix(o.Key, c.prefix)
		if !ok || !strings.HasPrefix(key, prefix) || key <= after {
			continue
		}

		if len(objs) == max {
			return objs, true, nil
		}

		objs = append(objs, ObjectInfo{Key: key, Size: o.Size, LastModified: o.LastModified})
	}

	return objs, false, nil
}

// Delete removes one object. It stats first because a delete of a key that was
// never there is a success on S3 (and on B2's S3 endpoint), and the contract
// owes the handler a 404. The allowlist that decides what may be deleted lives
// in the handler.
func (c *cloud) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	name, err := c.objectName(key)
	if err != nil {
		return err
	}

	if _, err := c.cli.StatObject(ctx, c.bucket, name, minio.StatObjectOptions{}); err != nil {
		return mapErr(key, err)
	}

	if err := c.cli.RemoveObject(ctx, c.bucket, name, minio.RemoveObjectOptions{}); err != nil {
		return mapErr(key, err)
	}

	return nil
}

// Versioned reads the bucket's versioning configuration. A bucket that cannot
// answer is reported unversioned, which is the answer that makes a client keep
// its own history.
func (c *cloud) Versioned(ctx context.Context) bool {
	v, err := c.cli.GetBucketVersioning(ctx, c.bucket)

	return err == nil && v.Enabled()
}

// mapErr translates the provider's HTTP status into the store's own sentinels.
func mapErr(key string, err error) error {
	switch minio.ToErrorResponse(err).StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusRequestedRangeNotSatisfiable:
		return fmt.Errorf("%w: %q", ErrRange, key)
	default:
		return fmt.Errorf("on %q: %w", key, err)
	}
}

// unquote strips the quotes S3 wraps an ETag in.
func unquote(etag string) string { return strings.Trim(etag, `"`) }
