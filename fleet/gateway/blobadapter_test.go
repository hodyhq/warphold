package gateway_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/internal/blobtesting"
	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/internal/testlogging"
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

// The blob.Storage contract itself, run by Kopia's own conformance suite
// rather than re-asserted by hand.
func TestHostedStorageVerifyStorage(t *testing.T) {
	ctx := testlogging.Context(t)
	blobtesting.VerifyStorage(ctx, t, hostedStorage(t, t.TempDir(), "dev1/"), blob.PutOptions{})
}

// What VerifyStorage does not cover: two devices sharing one root see
// disjoint key spaces, and the factory refuses a prefix that would not confine
// one device.
func TestHostedStorageConfinesEachDeviceToItsPrefix(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	require.NoError(t, hostedStorage(t, root, "dev1/").PutBlob(ctx, "p0001", gather.FromSlice([]byte("mine")), blob.PutOptions{}))

	other, err := blob.ListAllBlobs(ctx, hostedStorage(t, root, "dev2/"), "")
	require.NoError(t, err)
	require.Empty(t, other, "one device must not see another's blobs")

	for _, prefix := range []string{"", "dev1", "/"} {
		_, err := blob.NewStorage(ctx, blob.ConnectionInfo{
			Type: gateway.HostedStorageType, Config: &gateway.HostedOptions{Root: root, Prefix: prefix},
		}, false)
		require.Error(t, err, "prefix %q does not confine a device", prefix)
	}
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
