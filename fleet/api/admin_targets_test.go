package api_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// verifiedFakes installs a B2 and an S3 stand-in that both answer "yes", so a
// target creation gets past verification without touching a network.
func verifiedFakes(h *harness) *fakeCloud {
	c := &fakeCloud{lock: true, cond: true}
	h.s.SetB2ForTesting(fakeB2API{lock: true})
	h.s.SetCloudForTesting(c)

	return c
}

func TestHostedTargetDiskMode(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	root := t.TempDir()

	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": root,
	})
	require.Equal(t, 201, resp.StatusCode)
	tid := body["id"].(float64)

	resp, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Equal(t, 200, resp.StatusCode)
	require.Len(t, list, 1)
	require.Equal(t, "hosted", list[0]["kind"])
	require.Equal(t, "disk", list[0]["storage_mode"])
	require.Equal(t, root, list[0]["path"])

	// A group may point at a hosted target like any other kind.
	resp, body = h.do("POST", "/api/v1/fleet/templates", map[string]any{"name": "Home", "sources": []string{"~"}})
	require.Equal(t, 201, resp.StatusCode)
	resp, _ = h.do("POST", "/api/v1/fleet/groups", map[string]any{"name": "Laptops", "target_id": tid, "template_id": body["id"]})
	require.Equal(t, 201, resp.StatusCode)
}

func TestHostedTargetMirrorIsSealedAndVerified(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	c := verifiedFakes(h)

	resp, _ := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": t.TempDir(),
		"mirror_kind": "b2", "mirror_bucket": "hody-offsite", "mirror_region": "us-west-004",
		"mirror_key_id": "k", "mirror_key": "s",
	})
	require.Equal(t, 201, resp.StatusCode)

	_, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Equal(t, "b2", list[0]["mirror_kind"])
	require.Equal(t, "hody-offsite", list[0]["mirror_bucket"])
	require.Equal(t, "us-west-004", list[0]["mirror_region"])
	require.NotEmpty(t, list[0]["mirror_lock_verified_at"])
	for _, k := range []string{"mirror_key", "mirror_key_id", "sealed_mirror_key", "sealed_admin_key", "key", "key_id"} {
		_, has := list[0][k]
		require.False(t, has, "%s must never leave the server", k)
	}

	// A b2 mirror's lock is read natively, so the S3 verifier is asked only for
	// the conditional put - under the prefix the mirror will really write to.
	asked, prefixes := c.probed()
	require.Empty(t, asked)
	require.Equal(t, []string{""}, prefixes)
}

// A mirror on a bucket without Object Lock, or on a provider that ignores the
// conditional write, is refused outright: a mirror nobody can trust is worse
// than no mirror, because the target screen would report it as offsite health.
func TestHostedTargetMirrorRequiresLockAndConditionalWrites(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kind       string
		lock, cond bool
		want       string
	}{
		{"b2 without object lock", "b2", false, true,
			`bucket "hody-offsite" does not have Object Lock enabled`},
		{"s3 without object lock", "s3", false, true,
			`bucket "hody-offsite" does not have Object Lock enabled`},
		{"provider ignores if-none-match", "s3", true, false,
			`bucket "hody-offsite" does not enforce conditional writes, so it cannot be append-only; use Fleet disk storage with a mirror instead`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.activateAndLogin()
			h.s.SetB2ForTesting(fakeB2API{lock: tc.lock})
			h.s.SetCloudForTesting(&fakeCloud{lock: tc.lock, cond: tc.cond})

			resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
				"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": t.TempDir(),
				"mirror_kind": tc.kind, "mirror_bucket": "hody-offsite", "mirror_region": "us-west-004",
				"mirror_key_id": "k", "mirror_key": "s",
			})
			require.Equal(t, 400, resp.StatusCode)
			require.Equal(t, tc.want, body["error"])

			_, list := h.doList("GET", "/api/v1/fleet/targets")
			require.Empty(t, list, "an unverified bucket creates no target")
		})
	}
}

func TestHostedTargetCloudDirect(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	c := verifiedFakes(h)

	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "cloud", "kind": "hosted", "storage_mode": "cloud",
		"bucket": "hody-hosted", "region": "us-west-004", "key_id": "k", "key": "s",
	})
	require.Equal(t, 201, resp.StatusCode)
	require.Equal(t, true, body["object_lock_verified"])

	_, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Len(t, list, 1)
	require.Equal(t, "cloud", list[0]["storage_mode"])
	require.Equal(t, "hody-hosted", list[0]["bucket"])
	// No endpoint was given, so B2's S3 host is derived from the region, and
	// stored, so a reconnect uses the host the bucket was verified against.
	require.Equal(t, "s3.us-west-004.backblazeb2.com", list[0]["endpoint"])
	require.NotEmpty(t, list[0]["object_lock_verified_at"])
	for _, k := range []string{"key", "key_id", "sealed_admin_key"} {
		_, has := list[0][k]
		require.False(t, has, "%s must never leave the server", k)
	}

	// A derived endpoint means B2, so the lock came from the native API and
	// only the conditional put went through the S3 verifier.
	asked, prefixes := c.probed()
	require.Empty(t, asked)
	require.Equal(t, []string{""}, prefixes)
}

func TestHostedTargetCloudDirectWithAnExplicitEndpoint(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	c := verifiedFakes(h)

	resp, _ := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "cloud", "kind": "hosted", "storage_mode": "cloud",
		"bucket": "hody-hosted", "region": "us-east-1", "endpoint": "s3.us-east-1.amazonaws.com",
		"key_id": "k", "key": "s",
	})
	require.Equal(t, 201, resp.StatusCode)

	// A named endpoint means plain S3-compatible: the lock is read with
	// GetObjectLockConfiguration against that endpoint's bucket, not B2's API.
	asked, prefixes := c.probed()
	require.Equal(t, []string{"s3.us-east-1.amazonaws.com/hody-hosted"}, asked)
	require.Equal(t, []string{""}, prefixes)

	_, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Equal(t, "s3.us-east-1.amazonaws.com", list[0]["endpoint"])
}

func TestHostedTargetCloudDirectRequiresLockAndConditionalWrites(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lock, cond bool
		want       string
	}{
		{"no object lock", false, true, `bucket "hody-hosted" does not have Object Lock enabled`},
		{"no conditional writes", true, false,
			`bucket "hody-hosted" does not enforce conditional writes, so it cannot be append-only; use Fleet disk storage with a mirror instead`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.activateAndLogin()
			h.s.SetB2ForTesting(fakeB2API{lock: tc.lock})
			h.s.SetCloudForTesting(&fakeCloud{lock: tc.lock, cond: tc.cond})

			resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
				"name": "cloud", "kind": "hosted", "storage_mode": "cloud",
				"bucket": "hody-hosted", "region": "us-west-004", "key_id": "k", "key": "s",
			})
			require.Equal(t, 400, resp.StatusCode)
			require.Equal(t, tc.want, body["error"])

			_, list := h.doList("GET", "/api/v1/fleet/targets")
			require.Empty(t, list, "an unverified bucket creates no target")
		})
	}
}

func TestHostedTargetRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	verifiedFakes(h)
	missing := filepath.Join(t.TempDir(), "nope")
	notDir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notDir, nil, 0o600))

	for _, tc := range []struct {
		name, want string
		in         map[string]any
	}{
		{"no storage_mode", "storage_mode must be disk or cloud for hosted targets",
			map[string]any{"storage_mode": ""}},
		{"bogus storage_mode", "storage_mode must be disk or cloud for hosted targets",
			map[string]any{"storage_mode": "tape"}},
		{"relative path", "path must be absolute",
			map[string]any{"storage_mode": "disk", "path": "srv/warphold/hosted"}},
		{"missing path", "path does not exist: " + missing,
			map[string]any{"storage_mode": "disk", "path": missing}},
		{"path is a file", "path is not a directory: " + notDir,
			map[string]any{"storage_mode": "disk", "path": notDir}},
		{"bad mirror kind", "mirror_kind must be b2 or s3",
			map[string]any{"storage_mode": "disk", "path": os.TempDir(), "mirror_kind": "glacier"}},
		{"mirror without credentials", "mirror_bucket, mirror_region, mirror_key_id and mirror_key are required for a mirror",
			map[string]any{"storage_mode": "disk", "path": os.TempDir(), "mirror_kind": "b2", "mirror_bucket": "b"}},
		{"mirror with a bogus region", `region "us west" is not a valid region name`,
			map[string]any{"storage_mode": "disk", "path": os.TempDir(), "mirror_kind": "b2",
				"mirror_bucket": "b", "mirror_region": "us west", "mirror_key_id": "k", "mirror_key": "s"}},
		{"cloud without credentials", "bucket, region, key_id and key are required for cloud-direct hosted targets",
			map[string]any{"storage_mode": "cloud", "bucket": "b", "key_id": "k"}},
		{"cloud with a bogus region", `region "us west" is not a valid region name`,
			map[string]any{"storage_mode": "cloud", "bucket": "b", "region": "us west", "key_id": "k", "key": "s"}},
		{"cloud endpoint with a scheme", `endpoint "https://s3.example.com" must be a bare host, without a scheme`,
			map[string]any{"storage_mode": "cloud", "bucket": "b", "region": "us-east-1", "key_id": "k", "key": "s",
				"endpoint": "https://s3.example.com"}},
		{"cloud endpoint with a path", `endpoint "s3.example.com/bucket" must be a bare host[:port], without a path`,
			map[string]any{"storage_mode": "cloud", "bucket": "b", "region": "us-east-1", "key_id": "k", "key": "s",
				"endpoint": "s3.example.com/bucket"}},
		{"cloud with a mirror", "a cloud-direct target is already offsite; a mirror is only for storage_mode disk",
			map[string]any{"storage_mode": "cloud", "bucket": "b", "region": "us-west-004", "key_id": "k", "key": "s",
				"mirror_kind": "b2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := map[string]any{"name": "hosted", "kind": "hosted"}
			for k, v := range tc.in {
				in[k] = v
			}
			resp, body := h.do("POST", "/api/v1/fleet/targets", in)
			require.Equal(t, 400, resp.StatusCode)
			require.Equal(t, tc.want, body["error"])
		})
	}
}

func TestHostedTargetRejectsNonWritablePath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	h := newHarness(t)
	h.activateAndLogin()
	root := filepath.Join(t.TempDir(), "ro")
	require.NoError(t, os.Mkdir(root, 0o500))
	t.Cleanup(func() { os.Chmod(root, 0o700) })

	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": root,
	})
	require.Equal(t, 400, resp.StatusCode)
	require.Equal(t, "path is not writable by the fleet service user: "+root, body["error"])
}
