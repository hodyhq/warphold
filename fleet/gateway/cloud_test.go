package gateway

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/blobtesting"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
)

const testPrefix = "wh/"

// mapBacked is the primary stand-in: a flat blob.Storage whose IDs round-trip
// through List unchanged, which is how S3 and B2 behave. It rejects
// DoNotRecreate exactly as they do, so it exercises the Head-then-Put path.
func mapBacked(t *testing.T) (ObjectStore, blobtesting.DataMap) {
	t.Helper()

	data := blobtesting.DataMap{}
	c, err := newCloud(blobtesting.NewMapStorage(data, map[blob.ID]time.Time{}, time.Now), testPrefix)
	require.NoError(t, err)

	return c, data
}

// fsBacked is a real Kopia provider (repo/blob/filesystem). Its sharded layout
// rebuilds a blob ID from directory names, so a key containing '/' does not
// survive ListBlobs - it is used for everything but List.
func fsBacked(t *testing.T) ObjectStore {
	t.Helper()

	st, err := filesystem.New(t.Context(), &filesystem.Options{Path: t.TempDir()}, true)
	require.NoError(t, err)

	t.Cleanup(func() { st.Close(context.WithoutCancel(t.Context())) }) //nolint:errcheck // test cleanup

	c, err := newCloud(st, testPrefix)
	require.NoError(t, err)

	return c
}

// eachBackend runs fn against both stand-ins.
func eachBackend(t *testing.T, fn func(t *testing.T, s ObjectStore)) {
	t.Helper()

	t.Run("map", func(t *testing.T) {
		s, _ := mapBacked(t)
		fn(t, s)
	})

	t.Run("filesystem", func(t *testing.T) { fn(t, fsBacked(t)) })
}

func put(t *testing.T, s ObjectStore, key, body string, overwrite bool) (ObjectInfo, error) {
	t.Helper()

	return s.Put(t.Context(), key, strings.NewReader(body), int64(len(body)), overwrite)
}

func read(t *testing.T, s ObjectStore, key string, offset, length int64) string {
	t.Helper()

	r, _, err := s.Get(t.Context(), key, offset, length)
	require.NoError(t, err)

	defer r.Close() //nolint:errcheck // test

	b, err := io.ReadAll(r)
	require.NoError(t, err)

	return string(b)
}

func TestCloudPutIsAppendOnly(t *testing.T) {
	eachBackend(t, func(t *testing.T, s ObjectStore) {
		_, err := put(t, s, "dev1/a.blob", "first", false)
		require.NoError(t, err)

		_, err = put(t, s, "dev1/a.blob", "second", false)
		require.ErrorIs(t, err, ErrExists)

		require.Equal(t, "first", read(t, s, "dev1/a.blob", 0, -1), "the rejected Put must not touch the stored bytes")

		_, err = put(t, s, "dev1/a.blob", "third", true)
		require.NoError(t, err)
		require.Equal(t, "third", read(t, s, "dev1/a.blob", 0, -1))
	})
}

func TestCloudPutReportsSizeAndETag(t *testing.T) {
	eachBackend(t, func(t *testing.T, s ObjectStore) {
		info, err := put(t, s, "dev1/a.blob", "hello", false)
		require.NoError(t, err)
		require.Equal(t, int64(5), info.Size)
		// md5("hello")
		require.Equal(t, "5d41402abc4b2a76b9719d911017c592", info.ETag)
		require.False(t, info.LastModified.IsZero())

		// Head reports the provider's metadata, and no ETag: see the doc comment.
		hi, err := s.Head(t.Context(), "dev1/a.blob")
		require.NoError(t, err)
		require.Equal(t, int64(5), hi.Size)
		require.Empty(t, hi.ETag)
	})
}

func TestCloudPutRejectsASizeMismatch(t *testing.T) {
	eachBackend(t, func(t *testing.T, s ObjectStore) {
		_, err := s.Put(t.Context(), "dev1/a.blob", strings.NewReader("hello"), 4, false)
		require.ErrorIs(t, err, ErrBadKey)

		_, err = s.Put(t.Context(), "dev1/b.blob", strings.NewReader("hello"), 6, false)
		require.ErrorIs(t, err, ErrBadKey)

		// An unknown length is accepted (chunked upload).
		info, err := s.Put(t.Context(), "dev1/c.blob", strings.NewReader("hello"), -1, false)
		require.NoError(t, err)
		require.Equal(t, int64(5), info.Size)
	})
}

func TestCloudGetRange(t *testing.T) {
	eachBackend(t, func(t *testing.T, s ObjectStore) {
		_, err := put(t, s, "dev1/a.blob", "0123456789", false)
		require.NoError(t, err)

		require.Equal(t, "0123456789", read(t, s, "dev1/a.blob", 0, -1))
		require.Equal(t, "234", read(t, s, "dev1/a.blob", 2, 3))
		require.Equal(t, "6789", read(t, s, "dev1/a.blob", 6, -1))
		// A length past the end is clamped, not an error.
		require.Equal(t, "89", read(t, s, "dev1/a.blob", 8, 1000))

		// The ObjectInfo describes the whole object, not the range.
		r, info, err := s.Get(t.Context(), "dev1/a.blob", 2, 3)
		require.NoError(t, err)
		r.Close() //nolint:errcheck // test
		require.Equal(t, int64(10), info.Size)

		_, _, err = s.Get(t.Context(), "dev1/a.blob", 10, 1)
		require.ErrorIs(t, err, ErrRange)

		_, _, err = s.Get(t.Context(), "dev1/a.blob", -1, 1)
		require.ErrorIs(t, err, ErrRange)

		// A zero-byte object reads cleanly at offset 0.
		_, err = put(t, s, "dev1/empty.blob", "", false)
		require.NoError(t, err)
		require.Empty(t, read(t, s, "dev1/empty.blob", 0, -1))
	})
}

func TestCloudMissingObjectsAreNotFound(t *testing.T) {
	eachBackend(t, func(t *testing.T, s ObjectStore) {
		_, err := s.Head(t.Context(), "dev1/nope")
		require.ErrorIs(t, err, ErrNotFound)

		_, _, err = s.Get(t.Context(), "dev1/nope", 0, -1)
		require.ErrorIs(t, err, ErrNotFound)

		require.ErrorIs(t, s.Delete(t.Context(), "dev1/nope"), ErrNotFound)

		_, err = put(t, s, "dev1/a.blob", "x", false)
		require.NoError(t, err)
		require.NoError(t, s.Delete(t.Context(), "dev1/a.blob"))
		require.ErrorIs(t, s.Delete(t.Context(), "dev1/a.blob"), ErrNotFound)
	})
}

func TestCloudStoresUnderTheFleetPrefix(t *testing.T) {
	s, data := mapBacked(t)

	_, err := put(t, s, "dev1/a.blob", "x", false)
	require.NoError(t, err)

	// <bucket>/<root-prefix>/<device-id>/<key>: the root prefix is on the
	// stored key, so one bucket can hold every device, and it never leaks back
	// out to the caller.
	require.Contains(t, data, blob.ID("wh/dev1/a.blob"))

	info, err := s.Head(t.Context(), "dev1/a.blob")
	require.NoError(t, err)
	require.Equal(t, "dev1/a.blob", info.Key)
}

func TestCloudListOrdersPaginatesAndConfines(t *testing.T) {
	s, data := mapBacked(t)

	for _, k := range []string{"dev1/c", "dev1/a", "dev1/b", "dev2/a", "other"} {
		_, err := put(t, s, k, k, false)
		require.NoError(t, err)
	}

	// A blob written by another fleet outside our root prefix is invisible.
	data["elsewhere/dev1/x"] = []byte("x")

	objs, truncated, err := s.List(t.Context(), "dev1/", "", 0)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []string{"dev1/a", "dev1/b", "dev1/c"}, keys(objs))

	objs, truncated, err = s.List(t.Context(), "dev1/", "", 2)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Equal(t, []string{"dev1/a", "dev1/b"}, keys(objs))

	// after is exclusive and continues where the page ended.
	objs, truncated, err = s.List(t.Context(), "dev1/", "dev1/b", 2)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []string{"dev1/c"}, keys(objs))

	// An empty prefix lists this fleet's objects and nothing else.
	objs, _, err = s.List(t.Context(), "", "", 0)
	require.NoError(t, err)
	require.Equal(t, []string{"dev1/a", "dev1/b", "dev1/c", "dev2/a", "other"}, keys(objs))

	// prefix is a raw string prefix, as S3 and the local backend define it, so
	// a prefix that stops short of the path boundary reaches a sibling device.
	// Confining a device is the handler's job (spec §4.1.5), not this layer's -
	// asserted here so the two backends cannot drift apart.
	_, err = put(t, s, "dev10/a", "x", false)
	require.NoError(t, err)

	objs, _, err = s.List(t.Context(), "dev1", "", 0)
	require.NoError(t, err)
	require.Equal(t, []string{"dev1/a", "dev1/b", "dev1/c", "dev10/a"}, keys(objs))
}

func TestCloudPutIsBoundedInMemory(t *testing.T) {
	s, data := mapBacked(t)

	// A declared size over the cap is refused before a byte is read.
	_, err := s.Put(t.Context(), "dev1/huge", failReader{}, maxCloudPut+1, false)
	require.ErrorIs(t, err, ErrBadKey)

	// So is an upload of unknown length that runs past it - the whole object
	// has to fit in memory before the provider will take it.
	_, err = s.Put(t.Context(), "dev1/endless", endlessReader{}, -1, false)
	require.ErrorIs(t, err, ErrBadKey)

	require.Empty(t, data, "an over-sized upload must never reach the provider")
}

// failReader fails the test if it is read at all.
type failReader struct{}

func (failReader) Read([]byte) (int, error) { panic("body read despite an over-sized declared length") }

// endlessReader never ends, standing in for a chunked upload with no length.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) { return len(p), nil }

func keys(objs []ObjectInfo) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.Key)
	}

	return out
}

func TestCloudRejectsHostileKeys(t *testing.T) {
	s, data := mapBacked(t)

	for _, key := range []string{
		"", "/dev1/a", "dev1/../dev2/a", "dev1/./a", "dev1//a", "dev1/a\x00b",
		"dev1/..\\..\\x", "dev1/a\nb", strings.Repeat("x", MaxKeyLen+1),
		// 1024 bytes on its own, but over the S3 limit once the fleet prefix
		// is on the front.
		strings.Repeat("x", MaxKeyLen),
	} {
		_, err := put(t, s, key, "x", false)
		require.ErrorIsf(t, err, ErrBadKey, "Put(%q)", key)

		_, _, err = s.Get(t.Context(), key, 0, -1)
		require.ErrorIsf(t, err, ErrBadKey, "Get(%q)", key)

		_, err = s.Head(t.Context(), key)
		require.ErrorIsf(t, err, ErrBadKey, "Head(%q)", key)

		require.ErrorIsf(t, s.Delete(t.Context(), key), ErrBadKey, "Delete(%q)", key)
	}

	require.Empty(t, data, "a rejected key must never reach the provider")
}

func TestNewCloudRejectsAnUnterminatedPrefix(t *testing.T) {
	// "wh" would confine this fleet to its own keys and to every fleet whose
	// prefix merely starts with it ("wh2/...").
	_, err := newCloud(blobtesting.NewMapStorage(blobtesting.DataMap{}, map[blob.ID]time.Time{}, time.Now), "wh")
	require.ErrorIs(t, err, ErrBadKey)

	_, err = newCloud(blobtesting.NewMapStorage(blobtesting.DataMap{}, map[blob.ID]time.Time{}, time.Now), "../wh/")
	require.ErrorIs(t, err, ErrBadKey)

	// An empty prefix is legal: the bucket root is the fleet root.
	s, err := newCloud(blobtesting.NewMapStorage(blobtesting.DataMap{}, map[blob.ID]time.Time{}, time.Now), "")
	require.NoError(t, err)

	_, err = put(t, s, "dev1/a", "x", false)
	require.NoError(t, err)
}

// fake is a provider stand-in for the behaviours the real ones differ on:
// a conditional put (GCS, Azure), a delete that swallows a missing blob (B2),
// and a versioning answer (S3).
type fake struct {
	blob.Storage

	conditional   bool
	swallowDelete bool
	versioned     *bool
	versionedErr  error
	heads         int
}

func (f *fake) GetMetadata(ctx context.Context, id blob.ID) (blob.Metadata, error) {
	f.heads++

	return f.Storage.GetMetadata(ctx, id) //nolint:wrapcheck // test double
}

func (f *fake) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, opts blob.PutOptions) error {
	if f.conditional && opts.DoNotRecreate {
		if _, err := f.Storage.GetMetadata(ctx, id); err == nil {
			return blob.ErrBlobAlreadyExists
		}

		opts.DoNotRecreate = false
	}

	return f.Storage.PutBlob(ctx, id, data, opts) //nolint:wrapcheck // test double
}

func (f *fake) DeleteBlob(ctx context.Context, id blob.ID) error {
	if f.swallowDelete {
		f.Storage.DeleteBlob(ctx, id) //nolint:errcheck // deliberately ignored, as B2 does

		return nil
	}

	return f.Storage.DeleteBlob(ctx, id) //nolint:wrapcheck // test double
}

func (f *fake) IsVersioned(context.Context) (bool, error) {
	if f.versioned == nil {
		return false, f.versionedErr
	}

	return *f.versioned, f.versionedErr
}

func newFake(t *testing.T, f *fake) ObjectStore {
	t.Helper()

	f.Storage = blobtesting.NewMapStorage(blobtesting.DataMap{}, map[blob.ID]time.Time{}, time.Now)

	c, err := newCloud(f, testPrefix)
	require.NoError(t, err)

	return c
}

func TestCloudUsesAConditionalPutWhenTheProviderHasOne(t *testing.T) {
	f := &fake{conditional: true}
	s := newFake(t, f)

	_, err := put(t, s, "dev1/a", "first", false)
	require.NoError(t, err)

	_, err = put(t, s, "dev1/a", "second", false)
	require.ErrorIs(t, err, ErrExists)

	require.Zero(t, f.heads, "a provider that honours DoNotRecreate must not need a Head before the write")
	require.Equal(t, "first", read(t, s, "dev1/a", 0, -1))
}

func TestCloudDeleteIsNotFoundEvenWhenTheProviderSwallowsIt(t *testing.T) {
	s := newFake(t, &fake{swallowDelete: true})

	require.ErrorIs(t, s.Delete(t.Context(), "dev1/nope"), ErrNotFound)

	_, err := put(t, s, "dev1/a", "x", false)
	require.NoError(t, err)
	require.NoError(t, s.Delete(t.Context(), "dev1/a"))
}

func TestCloudVersionedReflectsTheProvider(t *testing.T) {
	yes, no := true, false

	require.True(t, newFake(t, &fake{versioned: &yes}).Versioned(t.Context()))
	require.False(t, newFake(t, &fake{versioned: &no}).Versioned(t.Context()))
	// A provider that cannot answer is reported unversioned.
	require.False(t, newFake(t, &fake{versioned: &yes, versionedErr: errors.New("boom")}).Versioned(t.Context()))

	s, _ := mapBacked(t)
	require.False(t, s.Versioned(t.Context()), "a provider with no versioning API is unversioned")
}

func TestNewCloudFromConnectionInfo(t *testing.T) {
	s, err := NewCloud(t.Context(), blob.ConnectionInfo{
		Type:   "filesystem",
		Config: &filesystem.Options{Path: t.TempDir()},
	}, testPrefix)
	require.NoError(t, err)

	t.Cleanup(func() { s.(*cloud).Close(context.WithoutCancel(t.Context())) }) //nolint:errcheck,forcetypeassert // test cleanup

	_, err = put(t, s, "dev1/a", "hello", false)
	require.NoError(t, err)
	require.Equal(t, "hello", read(t, s, "dev1/a", 0, -1))

	_, err = NewCloud(t.Context(), blob.ConnectionInfo{Type: "nosuchprovider"}, testPrefix)
	require.Error(t, err)
}
