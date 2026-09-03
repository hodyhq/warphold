package api_test

import (
	"fmt"
	"net/http"
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
	require.Equal(t, true, list[0]["mirror_conditional_put"])
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

// An under-scoped B2 key cannot read fileLockConfiguration, so the lock flag
// decodes as false. That is "cannot verify", not "unlocked", and the admin has
// to be told to bring a different key rather than to go and enable a setting
// that may already be on.
func TestHostedTargetMirrorReportsAnUnverifiableB2Key(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.s.SetB2ForTesting(fakeB2API{lock: true, lockUnreadable: true})
	h.s.SetCloudForTesting(&fakeCloud{lock: true, cond: true})

	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": t.TempDir(),
		"mirror_kind": "b2", "mirror_bucket": "hody-offsite", "mirror_region": "us-west-004",
		"mirror_key_id": "k", "mirror_key": "s",
	})
	require.Equal(t, 400, resp.StatusCode)
	require.Equal(t, `bucket "hody-offsite": cannot verify Object Lock: the application key lacks `+
		`readBucketRetentions/read capability - use a key that can read the bucket's lock configuration`, body["error"])

	_, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Empty(t, list)
}

// An s3 mirror is verified entirely over S3: GetObjectLockConfiguration against
// the derived AWS endpoint, then the conditional put.
func TestHostedTargetS3MirrorIsVerifiedOverS3(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	c := verifiedFakes(h)

	resp, _ := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": t.TempDir(),
		"mirror_kind": "s3", "mirror_bucket": "hody-offsite", "mirror_region": "us-east-1",
		"mirror_key_id": "k", "mirror_key": "s",
	})
	require.Equal(t, 201, resp.StatusCode)

	asked, prefixes := c.probed()
	require.Equal(t, []string{"s3.us-east-1.amazonaws.com/hody-offsite"}, asked)
	require.Equal(t, []string{""}, prefixes)

	_, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Equal(t, "s3", list[0]["mirror_kind"])
	require.Equal(t, "us-east-1", list[0]["mirror_region"])
	require.NotEmpty(t, list[0]["mirror_lock_verified_at"])
	// A disk target's own lock stamp stays unset: it is the mirror that was
	// verified, not the local root.
	_, has := list[0]["object_lock_verified_at"]
	require.False(t, has)
}

// Backblaze B2 has Object Lock but no conditional writes at all: its S3
// endpoint answers 501 to If-None-Match (docs/RECONCILE-object-lock.md). That
// is enough for a mirror, whose only writer is the Fleet server, so the target
// is created and the missing capability is recorded rather than refused.
func TestHostedTargetB2MirrorVerifiedWithoutConditionalPut(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.s.SetB2ForTesting(fakeB2API{lock: true})
	h.s.SetCloudForTesting(&fakeCloud{lock: true, condUnsupported: true})

	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": t.TempDir(),
		"mirror_kind": "b2", "mirror_bucket": "hody-offsite", "mirror_region": "us-west-004",
		"mirror_key_id": "k", "mirror_key": "s",
	})
	require.Equal(t, 201, resp.StatusCode)
	require.Equal(t, false, body["mirror_conditional_put"], "the 201 says what the bucket can do")

	_, list := h.doList("GET", "/api/v1/fleet/targets")
	require.NotEmpty(t, list[0]["mirror_lock_verified_at"], "Object Lock is still required")
	require.Equal(t, false, list[0]["mirror_conditional_put"])
}

// The same bucket cannot back a cloud-direct target: there the devices write
// themselves, and nothing but the provider can stop two of them clobbering one
// key. The refusal has to name providers that do implement it.
func TestHostedTargetCloudDirectRefusesAProviderWithoutConditionalPut(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.s.SetB2ForTesting(fakeB2API{lock: true})
	h.s.SetCloudForTesting(&fakeCloud{lock: true, condUnsupported: true})

	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "cloud", "kind": "hosted", "storage_mode": "cloud",
		"bucket": "hody-hosted", "region": "us-west-004", "key_id": "k", "key": "s",
	})
	require.Equal(t, 400, resp.StatusCode)
	require.Equal(t, `bucket "hody-hosted": this provider does not implement conditional writes (If-None-Match), `+
		`which a cloud-direct target needs because the devices write to the bucket themselves; `+
		`use an S3-compatible provider that does (AWS S3, Cloudflare R2, MinIO), `+
		`or Fleet disk storage with a mirror on this bucket instead`, body["error"])

	_, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Empty(t, list, "an unverified bucket creates no target")
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

// A target created without a mirror has to be able to gain one later, and a
// rotated mirror key has to be able to land on the target that already exists.
func TestTargetMirrorAttachAndReplace(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.s.SetB2ForTesting(fakeB2API{lock: true})
	c := &fakeCloud{lock: true, condUnsupported: true}
	h.s.SetCloudForTesting(c)

	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": t.TempDir(),
	})
	require.Equal(t, 201, resp.StatusCode)

	id := int64(body["id"].(float64))
	path := fmt.Sprintf("/api/v1/fleet/targets/%d/mirror", id)

	resp, body = h.do("PUT", path, map[string]any{
		"mirror_kind": "b2", "mirror_bucket": "hody-offsite", "mirror_region": "us-west-004",
		"mirror_key_id": "first-key", "mirror_key": "s",
	})
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, "hody-offsite", body["mirror_bucket"])
	require.NotEmpty(t, body["mirror_lock_verified_at"])
	require.Equal(t, false, body["mirror_conditional_put"], "B2 answers 501, and that is recorded")
	require.Equal(t, "first-key", c.lastKeyID())
	for _, k := range []string{"mirror_key", "mirror_key_id", "sealed_mirror_key"} {
		_, has := body[k]
		require.False(t, has, "%s must never leave the server", k)
	}

	// Replacing it re-verifies with the new key, and the new key is the one the
	// mirror will use: nothing caches the old one.
	c.condUnsupported, c.cond = false, true

	resp, body = h.do("PUT", path, map[string]any{
		"mirror_kind": "s3", "mirror_bucket": "hody-offsite-2", "mirror_region": "us-east-1",
		"mirror_key_id": "second-key", "mirror_key": "s2",
	})
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, "second-key", c.lastKeyID(), "the old key is not used again")

	_, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Len(t, list, 1, "a replacement updates the target, it does not add one")
	require.Equal(t, "s3", list[0]["mirror_kind"])
	require.Equal(t, "hody-offsite-2", list[0]["mirror_bucket"])
	require.Equal(t, "us-east-1", list[0]["mirror_region"])
	require.Equal(t, true, list[0]["mirror_conditional_put"])
}

func TestTargetMirrorRefusals(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	verifiedFakes(h)

	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "cloud", "kind": "hosted", "storage_mode": "cloud",
		"bucket": "hody-hosted", "region": "us-west-004", "key_id": "k", "key": "s",
	})
	require.Equal(t, 201, resp.StatusCode)
	cloudID := int64(body["id"].(float64))

	resp, body = h.do("POST", "/api/v1/fleet/targets", map[string]any{
		"name": "hosted", "kind": "hosted", "storage_mode": "disk", "path": t.TempDir(),
	})
	require.Equal(t, 201, resp.StatusCode)
	diskID := int64(body["id"].(float64))

	mirror := map[string]any{
		"mirror_kind": "b2", "mirror_bucket": "hody-offsite", "mirror_region": "us-west-004",
		"mirror_key_id": "k", "mirror_key": "s",
	}

	t.Run("cloud-direct has no mirror", func(t *testing.T) {
		resp, body := h.do("PUT", fmt.Sprintf("/api/v1/fleet/targets/%d/mirror", cloudID), mirror)
		require.Equal(t, 409, resp.StatusCode)
		require.Equal(t, "a cloud-direct target has no mirror: it already writes to the customer's own bucket", body["error"])
	})

	t.Run("a filesystem target has no mirror", func(t *testing.T) {
		resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{
			"name": "local", "kind": "filesystem", "path": t.TempDir(),
		})
		require.Equal(t, 201, resp.StatusCode)

		resp, body = h.do("PUT", fmt.Sprintf("/api/v1/fleet/targets/%d/mirror", int64(body["id"].(float64))), mirror)
		require.Equal(t, 409, resp.StatusCode)
		require.Equal(t, "only a hosted target with storage_mode disk can have a mirror", body["error"])
	})

	t.Run("no csrf token", func(t *testing.T) {
		req := h.newRequest("PUT", fmt.Sprintf("/api/v1/fleet/targets/%d/mirror", diskID), nil)
		req.Header.Del(csrfHeaderName)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		defer resp.Body.Close()

		require.Equal(t, 403, resp.StatusCode)
	})

	t.Run("unknown target", func(t *testing.T) {
		resp, body := h.do("PUT", "/api/v1/fleet/targets/9999/mirror", mirror)
		require.Equal(t, 404, resp.StatusCode)
		require.Equal(t, "target not found", body["error"])
	})

	t.Run("a mirror cannot be removed", func(t *testing.T) {
		resp, body := h.do("PUT", fmt.Sprintf("/api/v1/fleet/targets/%d/mirror", diskID), map[string]any{})
		require.Equal(t, 400, resp.StatusCode)
		require.Equal(t, "mirror_kind is required; a mirror cannot be removed through this route", body["error"])
	})

	t.Run("an unverified bucket changes nothing", func(t *testing.T) {
		h.s.SetB2ForTesting(fakeB2API{lock: false})
		t.Cleanup(func() { h.s.SetB2ForTesting(fakeB2API{lock: true}) })

		resp, body := h.do("PUT", fmt.Sprintf("/api/v1/fleet/targets/%d/mirror", diskID), mirror)
		require.Equal(t, 400, resp.StatusCode)
		require.Equal(t, `bucket "hody-offsite" does not have Object Lock enabled`, body["error"])

		_, list := h.doList("GET", "/api/v1/fleet/targets")
		for _, tg := range list {
			_, has := tg["mirror_kind"]
			require.False(t, has, "no mirror was attached")
		}
	})
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
