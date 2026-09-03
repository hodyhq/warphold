package api

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
)

// cloudStoreFor is what the gateway's per-target store cache calls for a
// cloud-direct target. It never touches the network here: building the backend
// only builds a client, so this pins the credential unsealing and the
// connection info the client is built from.
func TestCloudStoreForACloudDirectTarget(t *testing.T) {
	s := &Server{key: seal.Derive("passphrase", make([]byte, 16))}

	sealed, err := s.sealCreds(targetCreds{KeyID: "akid", Key: "secret"})
	require.NoError(t, err)

	tgt := &store.Target{
		Name: "cloud", Kind: "hosted", StorageMode: "cloud",
		Bucket: "hody-hosted", Region: "us-west-004",
		Endpoint: "s3.us-west-004.backblazeb2.com", SealedAdminKey: sealed,
	}

	objs, err := s.cloudStoreFor(t.Context(), tgt)
	require.NoError(t, err)
	require.NotNil(t, objs)

	// A target whose credentials never got sealed must not silently open an
	// anonymous connection to someone else's bucket.
	tgt.SealedAdminKey = nil
	_, err = s.cloudStoreFor(t.Context(), tgt)
	require.ErrorContains(t, err, "no stored credentials")

	// Nor may a target sealed under a different passphrase open at all.
	other := &Server{key: seal.Derive("other", make([]byte, 16))}
	tgt.SealedAdminKey = sealed
	_, err = other.cloudStoreFor(t.Context(), tgt)
	require.ErrorContains(t, err, "unsealing the credentials")
}

func TestS3EndpointDerivation(t *testing.T) {
	for _, tc := range []struct{ kind, region, want string }{
		{"b2", "us-west-004", "s3.us-west-004.backblazeb2.com"},
		{"s3", "us-east-1", "s3.us-east-1.amazonaws.com"},
	} {
		got, err := s3Endpoint(tc.kind, tc.region)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}

	// A region is interpolated into a hostname, so anything that is not a
	// region name is refused rather than reaching a host of the caller's choice.
	for _, bad := range []string{"", "us west", "a/b", "evil.example.com", "US-EAST-1", "-x", strings.Repeat("a", 33)} {
		_, err := s3Endpoint("b2", bad)
		require.ErrorContains(t, err, "is not a valid region name", "region %q", bad)
	}

	_, err := s3Endpoint("glacier", "us-east-1")
	require.ErrorContains(t, err, "no endpoint is known")
}
