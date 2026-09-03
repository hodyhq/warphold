package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/kopia/kopia/repo/blob"
)

// HostedStorageType is the blob.Storage type name for a device's repository
// inside a hosted target. It exists so the Fleet server can open one device's
// repository directly - provisioning (§7.1 step 4) and, later, the M5 jobs -
// without going out to its own S3 gateway over HTTP.
const HostedStorageType = "warphold-hosted"

// HostedOptions names one device's repository: the hosted target's root and
// the device's key prefix ("<device-id>/").
type HostedOptions struct {
	Root   string `json:"root"`
	Prefix string `json:"prefix"`
}

//nolint:gochecknoinits // blob storage providers register themselves; this is how repo/blob/* does it.
func init() {
	blob.AddSupportedStorage(HostedStorageType, HostedOptions{},
		func(_ context.Context, o *HostedOptions, _ bool) (blob.Storage, error) {
			// SkipTempSweep: this store is a *second* opener of a root that the
			// serving gateway already owns, and the sweep would delete the
			// partial writes of devices uploading right now.
			objs, err := NewLocal(o.Root, LocalOptions{SkipTempSweep: true})
			if err != nil {
				return nil, err
			}

			return NewBlobStorage(objs, *o), nil
		})
}

// NewBlobStorage adapts an ObjectStore to Kopia's blob.Storage, so a
// repository created by the Fleet server is byte-for-byte the repository the
// gateway serves to the device.
//
// This is the whole reason the adapter exists: Kopia's own "filesystem"
// provider shards blob names into <root>/xx/yy/<blob>, while the gateway's key
// space is flat (§4.3, one file per blob under <root>/<device-id>/). A
// server-side repository created through "filesystem" would be invisible to
// the device, and a device's blobs invisible to the server.
//
// Unlike the device-facing HTTP handler, this path is *not* append-only: it is
// the Fleet server acting as the repository's owner (initialization,
// maintenance), and PutBlob keeps blob.Storage's "replaces existing blob"
// contract. The append-only rule is a property of the device's credential, not
// of the store.
func NewBlobStorage(objs ObjectStore, o HostedOptions) blob.Storage {
	return &blobStore{objs: objs, opts: o}
}

type blobStore struct {
	blob.DefaultProviderImplementation

	objs ObjectStore
	opts HostedOptions
}

func (s *blobStore) key(id blob.ID) string { return s.opts.Prefix + string(id) }

// mapErr translates the ObjectStore's errors into blob's. ErrBadKey is
// deliberately left alone: a blob id the flat key space cannot hold is a bug,
// not a missing blob, and it should be loud rather than look like a 404.
func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return blob.ErrBlobNotFound
	case errors.Is(err, ErrRange):
		return blob.ErrInvalidRange
	case errors.Is(err, ErrExists):
		return blob.ErrBlobAlreadyExists
	default:
		return err
	}
}

func (s *blobStore) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, opts blob.PutOptions) error {
	switch {
	case opts.HasRetentionOptions():
		// Retention on the Fleet disk is the mirror job's Object Lock (§7.3).
		return fmt.Errorf("%w: blob-retention", blob.ErrUnsupportedPutBlobOption)
	case !opts.SetModTime.IsZero():
		return blob.ErrSetTimeUnsupported
	}

	r := data.Reader()
	defer r.Close() //nolint:errcheck // read-only handle over an in-memory buffer

	info, err := s.objs.Put(ctx, s.key(id), r, int64(data.Length()), !opts.DoNotRecreate)
	if err != nil {
		return mapErr(err)
	}

	if opts.GetModTime != nil {
		*opts.GetModTime = info.LastModified
	}

	return nil
}

func (s *blobStore) GetBlob(ctx context.Context, id blob.ID, offset, length int64, output blob.OutputBuffer) error {
	output.Reset()

	if offset < 0 {
		return fmt.Errorf("%w: negative offset %d", blob.ErrInvalidRange, offset)
	}

	rc, info, err := s.objs.Get(ctx, s.key(id), offset, length)
	if err != nil {
		return mapErr(err)
	}
	defer rc.Close() //nolint:errcheck // read-only handle

	n, err := io.Copy(output, rc)
	if err != nil {
		return fmt.Errorf("reading blob %q: %w", id, err)
	}

	// Get clamps a range that runs past the end; blob.Storage requires the
	// exact number of bytes asked for, so a short read is an invalid range.
	if length >= 0 && n != length {
		output.Reset()

		return fmt.Errorf("%w: %d bytes at offset %d of a %d-byte blob", blob.ErrInvalidRange, length, offset, info.Size)
	}

	return nil
}

func (s *blobStore) GetMetadata(ctx context.Context, id blob.ID) (blob.Metadata, error) {
	info, err := s.objs.Head(ctx, s.key(id))
	if err != nil {
		return blob.Metadata{}, mapErr(err)
	}

	return blob.Metadata{BlobID: id, Length: info.Size, Timestamp: info.LastModified.UTC()}, nil
}

func (s *blobStore) ListBlobs(ctx context.Context, idPrefix blob.ID, cb func(blob.Metadata) error) error {
	prefix, after := s.key(idPrefix), ""

	for {
		objs, truncated, err := s.objs.List(ctx, prefix, after, 0)
		if err != nil {
			return mapErr(err)
		}

		for _, o := range objs {
			after = o.Key

			if err := cb(blob.Metadata{
				BlobID:    blob.ID(o.Key[len(s.opts.Prefix):]),
				Length:    o.Size,
				Timestamp: o.LastModified.UTC(),
			}); err != nil {
				return err
			}
		}

		if !truncated {
			return nil
		}

		if len(objs) == 0 {
			return fmt.Errorf("listing %q: the store reported more results but returned none", prefix)
		}
	}
}

func (s *blobStore) DeleteBlob(ctx context.Context, id blob.ID) error {
	err := s.objs.Delete(ctx, s.key(id))
	if errors.Is(err, ErrNotFound) {
		// blob.Storage's contract: deleting a missing blob succeeds.
		return nil
	}

	return mapErr(err)
}

func (s *blobStore) ConnectionInfo() blob.ConnectionInfo {
	o := s.opts

	return blob.ConnectionInfo{Type: HostedStorageType, Config: &o}
}

func (s *blobStore) DisplayName() string {
	return "WarpHold hosted " + s.opts.Root + "/" + s.opts.Prefix
}

// Close releases the underlying store's directory handle, when it has one.
func (s *blobStore) Close(context.Context) error {
	if c, ok := s.objs.(io.Closer); ok {
		return c.Close()
	}

	return nil
}

// compile-time proof that the adapter is a complete blob.Storage.
var _ blob.Storage = (*blobStore)(nil)
