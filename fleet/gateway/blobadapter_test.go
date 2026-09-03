package gateway_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
)

func hostedStorage(t *testing.T, root, prefix string) blob.Storage {
	t.Helper()

	st, err := blob.NewStorage(context.Background(), blob.ConnectionInfo{
		Type:   gateway.HostedStorageType,
		Config: &gateway.HostedOptions{Root: root, Prefix: prefix},
	}, true)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close(context.Background()) }) //nolint:errcheck

	return st
}

// The whole point of the adapter: the bytes land exactly where the gateway
// serves them from, one flat file per blob under <root>/<device-id>/.
// Kopia's own filesystem provider shards the same blob into a subdirectory
// tree, which the gateway's flat key space (§4.3) cannot address at all.
func TestHostedStorageIsFlatUnlikeFilesystem(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	blobID := blob.ID("pdeadbeefdeadbeefdeadbeef")

	st := hostedStorage(t, root, "dev1/")
	require.NoError(t, st.PutBlob(ctx, blobID, gather.FromSlice([]byte("hello")), blob.PutOptions{}))

	got, err := os.ReadFile(filepath.Join(root, "dev1", string(blobID)))
	require.NoError(t, err, "the blob must be one flat file under the device directory")
	require.Equal(t, "hello", string(got))

	// Same blob, same root, through Kopia's filesystem provider: sharded, and
	// therefore invisible to the gateway.
	fsRoot := t.TempDir()
	fsSt, err := filesystem.New(ctx, &filesystem.Options{Path: fsRoot}, true)
	require.NoError(t, err)
	defer fsSt.Close(ctx) //nolint:errcheck

	require.NoError(t, fsSt.PutBlob(ctx, blobID, gather.FromSlice([]byte("hello")), blob.PutOptions{}))
	require.NoFileExists(t, filepath.Join(fsRoot, string(blobID)),
		"if this ever passes, the filesystem provider stopped sharding and the adapter could be dropped")
	require.FileExists(t, filepath.Join(fsRoot, "p", "dea", "dbeefdeadbeefdeadbeef.f"),
		"the filesystem provider writes <root>/p/dea/<rest>.f - a path the gateway's flat key space cannot address")
}

func TestHostedStorageRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := hostedStorage(t, root, "dev1/")

	require.NoError(t, st.PutBlob(ctx, "p0001", gather.FromSlice([]byte("0123456789")), blob.PutOptions{}))
	require.NoError(t, st.PutBlob(ctx, "p0002", gather.FromSlice([]byte("xy")), blob.PutOptions{}))
	require.NoError(t, st.PutBlob(ctx, "q0001", gather.FromSlice([]byte("z")), blob.PutOptions{}))

	var buf gather.WriteBuffer
	defer buf.Close()

	require.NoError(t, st.GetBlob(ctx, "p0001", 0, -1, &buf))
	require.Equal(t, "0123456789", string(buf.ToByteSlice()))

	require.NoError(t, st.GetBlob(ctx, "p0001", 3, 4, &buf))
	require.Equal(t, "3456", string(buf.ToByteSlice()))

	// A range past the end is invalid, not a short read.
	require.ErrorIs(t, st.GetBlob(ctx, "p0001", 8, 4, &buf), blob.ErrInvalidRange)
	require.ErrorIs(t, st.GetBlob(ctx, "p0001", -1, 4, &buf), blob.ErrInvalidRange)
	require.ErrorIs(t, st.GetBlob(ctx, "nope", 0, -1, &buf), blob.ErrBlobNotFound)

	md, err := st.GetMetadata(ctx, "p0001")
	require.NoError(t, err)
	require.Equal(t, blob.ID("p0001"), md.BlobID)
	require.Equal(t, int64(10), md.Length)
	require.False(t, md.Timestamp.IsZero())

	_, err = st.GetMetadata(ctx, "nope")
	require.ErrorIs(t, err, blob.ErrBlobNotFound)

	all, err := blob.ListAllBlobs(ctx, st, "")
	require.NoError(t, err)
	require.Equal(t, []blob.ID{"p0001", "p0002", "q0001"}, blobIDs(all))

	p, err := blob.ListAllBlobs(ctx, st, "p")
	require.NoError(t, err)
	require.Equal(t, []blob.ID{"p0001", "p0002"}, blobIDs(p))

	// The Fleet server owns this repository, so a plain PutBlob replaces; only
	// the device's HTTP credential is append-only.
	require.NoError(t, st.PutBlob(ctx, "p0002", gather.FromSlice([]byte("replaced")), blob.PutOptions{}))
	require.NoError(t, st.GetBlob(ctx, "p0002", 0, -1, &buf))
	require.Equal(t, "replaced", string(buf.ToByteSlice()))

	require.ErrorIs(t, st.PutBlob(ctx, "p0002", gather.FromSlice([]byte("no")), blob.PutOptions{DoNotRecreate: true}), blob.ErrBlobAlreadyExists)

	require.NoError(t, st.DeleteBlob(ctx, "q0001"))
	require.NoError(t, st.DeleteBlob(ctx, "q0001"), "deleting a missing blob is not an error")
	_, err = st.GetMetadata(ctx, "q0001")
	require.ErrorIs(t, err, blob.ErrBlobNotFound)

	// Another device's prefix is a different key space, sharing the root.
	other := hostedStorage(t, root, "dev2/")
	mine, err := blob.ListAllBlobs(ctx, other, "")
	require.NoError(t, err)
	require.Empty(t, mine)
}

func TestHostedStorageConnectionInfoReopens(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := hostedStorage(t, root, "dev1/")
	require.NoError(t, st.PutBlob(ctx, "p0001", gather.FromSlice([]byte("hi")), blob.PutOptions{}))

	// repo.Connect persists ConnectionInfo and repo.Open rebuilds the storage
	// from it, so the type has to be registered and the config round-trip.
	ci := st.ConnectionInfo()
	require.Equal(t, gateway.HostedStorageType, ci.Type)

	reopened, err := blob.NewStorage(ctx, ci, false)
	require.NoError(t, err)
	defer reopened.Close(ctx) //nolint:errcheck

	var buf gather.WriteBuffer
	defer buf.Close()

	require.NoError(t, reopened.GetBlob(ctx, "p0001", 0, -1, &buf))
	require.Equal(t, "hi", string(buf.ToByteSlice()))
	require.Contains(t, reopened.DisplayName(), root)
}

func blobIDs(md []blob.Metadata) []blob.ID {
	out := make([]blob.ID, 0, len(md))
	for _, m := range md {
		out = append(out, m.BlobID)
	}

	return out
}
