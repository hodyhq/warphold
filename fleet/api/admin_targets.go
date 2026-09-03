package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/fleet/jobs"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
)

// defaultHostedRoot is where a hosted target keeps device repositories when
// the admin does not name a path (spec D5). It is not created for them: the
// installer makes it, and a typo should fail loudly rather than silently
// backing the fleet up into a fresh empty directory.
const defaultHostedRoot = "/srv/warphold/hosted"

// fleetPrefix is the root prefix Fleet writes under inside a bucket it owns,
// for both a cloud-direct target and a mirror. It is empty: spec §5 lays the
// keys out as <bucket>/<device-id>/<blob-name>, and §7.3's mirror walks the
// same device prefixes. Verification probes must use exactly this prefix, or
// they prove a key space the target never writes to.
const fleetPrefix = ""

// region names are interpolated into a hostname, so they are held to what a
// provider region actually looks like rather than trusted as free text.
var regionRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// cloudAPI is the verification Fleet runs against an S3-compatible bucket
// before it may back a target. It is an interface so a test can stand in for a
// provider instead of reaching the network.
type cloudAPI interface {
	ObjectLock(ctx context.Context, ci blob.ConnectionInfo) error
	ConditionalPut(ctx context.Context, ci blob.ConnectionInfo, prefix string) error
}

// gatewayCloud is the real verifier, over the cloud-direct backend's own client.
type gatewayCloud struct{}

func (gatewayCloud) ObjectLock(ctx context.Context, ci blob.ConnectionInfo) error {
	return gateway.ProbeObjectLock(ctx, ci)
}

func (gatewayCloud) ConditionalPut(ctx context.Context, ci blob.ConnectionInfo, prefix string) error {
	return gateway.ProbeConditionalPut(ctx, ci, prefix)
}

type targetOut struct {
	ID                   int64      `json:"id"`
	Name                 string     `json:"name"`
	Kind                 string     `json:"kind"`
	Bucket               string     `json:"bucket,omitempty"`
	Region               string     `json:"region,omitempty"`
	Path                 string     `json:"path,omitempty"`
	Endpoint             string     `json:"endpoint,omitempty"`
	ObjectLockVerifiedAt *time.Time `json:"object_lock_verified_at,omitempty"`

	// Hosted targets. Sealed credentials are deliberately absent: nothing in
	// this struct may carry a key off the server.
	StorageMode          string     `json:"storage_mode,omitempty"`
	MirrorKind           string     `json:"mirror_kind,omitempty"`
	MirrorBucket         string     `json:"mirror_bucket,omitempty"`
	MirrorRegion         string     `json:"mirror_region,omitempty"`
	MirrorLockVerifiedAt *time.Time `json:"mirror_lock_verified_at,omitempty"`

	// MirrorConditionalPut is false for a mirror bucket whose provider has no
	// conditional writes at all (B2). That is allowed for a mirror - Fleet is
	// its only writer - but the target screen has to be able to say "offsite:
	// locked, no conditional writes" rather than imply the stronger guarantee.
	// Absent when the bucket was never probed.
	MirrorConditionalPut *bool `json:"mirror_conditional_put,omitempty"`

	// Derived, not stored: when anything under this target last reached the
	// mirror bucket, and whether any of its devices is behind. A target row
	// has to say "offsite 2 h ago" or "offsite stale", and neither is
	// answerable from the targets table alone.
	MirroredAt  *time.Time `json:"mirrored_at,omitempty"`
	MirrorStale bool       `json:"mirror_stale,omitempty"`
}

func toTargetOut(t store.Target) targetOut {
	return targetOut{
		ID: t.ID, Name: t.Name, Kind: t.Kind, Bucket: t.Bucket, Region: t.Region, Path: t.Path,
		ObjectLockVerifiedAt: t.ObjectLockVerifiedAt,
		StorageMode:          t.StorageMode,
		Endpoint:             t.Endpoint,
		MirrorKind:           t.MirrorKind, MirrorBucket: t.MirrorBucket, MirrorRegion: t.MirrorRegion,
		MirrorLockVerifiedAt: t.MirrorLockVerifiedAt,
		MirrorConditionalPut: t.MirrorConditionalPut,
	}
}

type targetInput struct {
	Name, Kind, Bucket, Region, Path string
	Endpoint                         string `json:"endpoint"`
	KeyID                            string `json:"key_id"`
	Key                              string `json:"key"`
	StorageMode                      string `json:"storage_mode"`
	MirrorKind                       string `json:"mirror_kind"`
	MirrorBucket                     string `json:"mirror_bucket"`
	MirrorRegion                     string `json:"mirror_region"`
	MirrorKeyID                      string `json:"mirror_key_id"`
	MirrorKey                        string `json:"mirror_key"`
}

func (s *Server) handleTargetCreate(w http.ResponseWriter, r *http.Request) {
	var in targetInput
	if err := decode(r, &in); err != nil || in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name and kind are required")
		return
	}
	t := &store.Target{Name: in.Name, Kind: in.Kind, Bucket: in.Bucket, Region: in.Region, Path: in.Path, CreatedAt: s.now()}
	switch in.Kind {
	case "filesystem":
		if in.Path == "" {
			writeErr(w, http.StatusBadRequest, "path is required for filesystem targets")
			return
		}
		if err := os.MkdirAll(in.Path, 0o700); err != nil {
			writeErr(w, http.StatusBadRequest, "cannot create path: "+err.Error())
			return
		}
	case "b2":
		if in.Bucket == "" || in.KeyID == "" || in.Key == "" {
			writeErr(w, http.StatusBadRequest, "bucket, key_id and key are required for b2 targets")
			return
		}
		sealed, err := s.sealCreds(targetCreds{KeyID: in.KeyID, Key: in.Key})
		if err != nil {
			adminFailed(w, "seal target credentials", err)
			return
		}
		t.SealedAdminKey = sealed
		if s.b2 != nil {
			info, err := s.b2.BucketInfo(r.Context(), in.KeyID, in.Key, in.Bucket) // Task 8
			if err != nil {
				writeErr(w, http.StatusBadRequest, "b2: "+err.Error())
				return
			}
			if info.ObjectLockEnabled {
				now := s.now()
				t.ObjectLockVerifiedAt = &now
			}
		}
	case "hosted":
		if !s.applyHosted(r.Context(), w, &in, t) {
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "kind must be b2, filesystem or hosted")
		return
	}
	id, err := s.store().CreateTarget(r.Context(), t)
	if err != nil {
		adminFailed(w, "create target", err)
		return
	}
	out := map[string]any{"id": id, "object_lock_verified": t.ObjectLockVerifiedAt != nil}
	if t.MirrorConditionalPut != nil {
		out["mirror_conditional_put"] = *t.MirrorConditionalPut
	}
	writeJSON(w, http.StatusCreated, out)
}

// applyHosted validates a hosted target and fills in its storage fields. It
// writes the error response and reports false when the input is rejected.
func (s *Server) applyHosted(ctx context.Context, w http.ResponseWriter, in *targetInput, t *store.Target) bool {
	switch in.StorageMode {
	case "disk":
		return s.applyHostedDisk(ctx, w, in, t)
	case "cloud":
		return s.applyHostedCloud(ctx, w, in, t)
	default:
		writeErr(w, http.StatusBadRequest, "storage_mode must be disk or cloud for hosted targets")
		return false
	}
}

// applyHostedDisk validates the on-disk root and the optional offsite mirror.
func (s *Server) applyHostedDisk(ctx context.Context, w http.ResponseWriter, in *targetInput, t *store.Target) bool {
	if t.Path == "" {
		t.Path = defaultHostedRoot
	}
	if err := checkHostedRoot(t.Path); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return false
	}
	t.StorageMode = "disk"

	if in.MirrorKind == "" {
		return true
	}
	return s.applyMirror(ctx, w, in, t)
}

// applyMirror validates, verifies and seals a mirror onto t. It is shared by
// target creation and the mirror route, so a mirror attached later is held to
// exactly the same proof as one configured up front.
//
// A mirror bucket must have Object Lock, but need not offer conditional writes:
// the Fleet server is the mirror's only writer and the job lists before it
// uploads, so If-None-Match buys nothing here that Object Lock does not already
// give. The answer is recorded rather than required.
func (s *Server) applyMirror(ctx context.Context, w http.ResponseWriter, in *targetInput, t *store.Target) bool {
	if in.MirrorKind != "b2" && in.MirrorKind != "s3" {
		writeErr(w, http.StatusBadRequest, "mirror_kind must be b2 or s3")
		return false
	}
	if in.MirrorBucket == "" || in.MirrorRegion == "" || in.MirrorKeyID == "" || in.MirrorKey == "" {
		writeErr(w, http.StatusBadRequest, "mirror_bucket, mirror_region, mirror_key_id and mirror_key are required for a mirror")
		return false
	}
	// The mirror is written through the S3 path whatever its kind, because
	// B2's native API has no conditional write (spec §14 note 00); the native
	// API is used only to read the bucket's Object Lock flag.
	endpoint, err := s3Endpoint(in.MirrorKind, in.MirrorRegion)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return false
	}
	condPut, err := s.verifyBucket(ctx, in.MirrorKind, in.MirrorBucket, in.MirrorRegion, endpoint, in.MirrorKeyID, in.MirrorKey, true)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return false
	}
	sealed, err := s.sealCreds(targetCreds{KeyID: in.MirrorKeyID, Key: in.MirrorKey})
	if err != nil {
		adminFailed(w, "seal mirror credentials", err)
		return false
	}
	now := s.now()
	t.MirrorKind, t.MirrorBucket, t.MirrorRegion, t.SealedMirrorKey = in.MirrorKind, in.MirrorBucket, in.MirrorRegion, sealed
	t.MirrorLockVerifiedAt, t.MirrorConditionalPut = &now, &condPut
	return true
}

// handleTargetMirrorSet attaches an offsite mirror to a hosted disk target, or
// replaces the one it has, running exactly the verification target creation
// runs and sealing the credentials the same way. Without it a target created
// without a mirror could never gain one, and a rotated mirror key meant
// building a second target row.
//
// It cannot remove a mirror: a target whose mirror silently vanished would keep
// reporting offsite health it no longer has, and deciding what happens to what
// is already in the bucket is its own piece of work.
//
// Nothing caches mirror credentials - jobs/mirror.go opens the bucket from the
// store on every run - so a replacement takes effect on the next run with no
// invalidation to do.
func (s *Server) handleTargetMirrorSet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in targetInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	t, err := s.store().Target(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "target not found")
		return
	}
	switch {
	case t.Kind == "hosted" && t.StorageMode == "cloud":
		writeErr(w, http.StatusConflict, "a cloud-direct target has no mirror: it already writes to the customer's own bucket")
		return
	case t.Kind != "hosted" || t.StorageMode != "disk":
		writeErr(w, http.StatusConflict, "only a hosted target with storage_mode disk can have a mirror")
		return
	case in.MirrorKind == "":
		writeErr(w, http.StatusBadRequest, "mirror_kind is required; a mirror cannot be removed through this route")
		return
	}
	if !s.applyMirror(r.Context(), w, &in, t) {
		return
	}
	if err := s.store().SetTargetMirror(r.Context(), t); err != nil {
		adminFailed(w, "update target mirror", err)
		return
	}
	writeJSON(w, http.StatusOK, toTargetOut(*t))
}

// applyHostedCloud validates a cloud-direct target: Fleet writes every device's
// blobs straight through to the customer's own bucket with the fleet's admin
// key, so the bucket must be proven append-only before a device can be told to
// trust it.
//
// The provider is B2 unless the admin names an endpoint. B2 is the one whose
// S3 host is derivable from the region, and the one whose Object Lock flag
// lives on a different API (§14.5); anything else is plain S3-compatible and
// has to be spelled out.
func (s *Server) applyHostedCloud(ctx context.Context, w http.ResponseWriter, in *targetInput, t *store.Target) bool {
	if in.Bucket == "" || in.Region == "" || in.KeyID == "" || in.Key == "" {
		writeErr(w, http.StatusBadRequest, "bucket, region, key_id and key are required for cloud-direct hosted targets")
		return false
	}
	if in.MirrorKind != "" {
		writeErr(w, http.StatusBadRequest, "a cloud-direct target is already offsite; a mirror is only for storage_mode disk")
		return false
	}

	kind, endpoint := "s3", in.Endpoint
	if endpoint == "" {
		e, err := s3Endpoint("b2", in.Region)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return false
		}
		kind, endpoint = "b2", e
	} else if err := checkEndpoint(endpoint); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return false
	}

	if _, err := s.verifyBucket(ctx, kind, in.Bucket, in.Region, endpoint, in.KeyID, in.Key, false); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return false
	}

	sealed, err := s.sealCreds(targetCreds{KeyID: in.KeyID, Key: in.Key})
	if err != nil {
		adminFailed(w, "seal target credentials", err)
		return false
	}

	now := s.now()
	t.SealedAdminKey = sealed
	t.StorageMode, t.Endpoint, t.Path = "cloud", endpoint, ""
	t.ObjectLockVerifiedAt = &now
	return true
}

// verifyBucket proves a bucket can hold append-only backups before a target is
// created on it: Object Lock is enabled, and - for a bucket devices write to
// themselves - the provider actually enforces the conditional write the gateway
// relies on. A rejection names the bucket and the property it is missing,
// because the fix is in the provider's console and the admin has to know which
// of the two to go and turn on.
//
// mirror says which shape is being verified. A mirror bucket has exactly one
// writer, the Fleet server, and its job lists before it uploads and never
// overwrites, so a provider with no If-None-Match at all (B2, measured in
// docs/RECONCILE-object-lock.md) may still back it; the answer is returned for
// the target row rather than enforced. A cloud-direct bucket is written by the
// devices themselves, where nothing but the provider can keep two writers from
// clobbering each other, so there the conditional write stays mandatory.
//
// The first result is whether the provider enforces conditional writes. It is
// meaningful only when the error is nil.
//
// The probes run under fleetPrefix - the prefix the target will really write
// under - so a prefix-scoped key that cannot write there fails here rather than
// on the first device's first snapshot.
func (s *Server) verifyBucket(ctx context.Context, kind, bucket, region, endpoint, keyID, key string, mirror bool) (bool, error) {
	ci := s3ConnInfo(bucket, region, endpoint, keyID, key)

	if kind == "b2" {
		if s.b2 == nil {
			return false, fmt.Errorf("bucket %q: the B2 API is not configured, so Object Lock cannot be verified", bucket)
		}
		// §14.5: B2 reports the lock flag on b2_list_buckets'
		// fileLockConfiguration, which is a different API from S3's
		// GetObjectLockConfiguration.
		info, err := s.b2.BucketInfo(ctx, keyID, key, bucket)
		if err != nil {
			return false, fmt.Errorf("b2: %w", err)
		}
		if !info.LockReadable {
			// B2 hides fileLockConfiguration from a key that may not read it,
			// and the flag then decodes as false. That is "cannot verify", not
			// "unlocked", and the fix is a different key.
			return false, fmt.Errorf("bucket %q: cannot verify Object Lock: the application key lacks readBucketRetentions/read capability - use a key that can read the bucket's lock configuration", bucket)
		}
		if !info.ObjectLockEnabled {
			return false, fmt.Errorf("bucket %q does not have Object Lock enabled", bucket)
		}
	} else if err := s.cloud.ObjectLock(ctx, ci); err != nil {
		if errors.Is(err, gateway.ErrNoObjectLock) {
			return false, fmt.Errorf("bucket %q does not have Object Lock enabled", bucket)
		}
		return false, fmt.Errorf("bucket %q: %w", bucket, err)
	}

	switch err := s.cloud.ConditionalPut(ctx, ci, fleetPrefix); {
	case err == nil:
		return true, nil
	case errors.Is(err, gateway.ErrCondPutNotImplemented) && mirror:
		// The provider has no If-None-Match (B2). Recorded, not refused: the
		// Fleet server is the mirror's only writer.
		return false, nil
	case errors.Is(err, gateway.ErrCondPutNotImplemented):
		return false, fmt.Errorf("bucket %q: this provider does not implement conditional writes (If-None-Match), which a cloud-direct target needs because the devices write to the bucket themselves; use an S3-compatible provider that does (AWS S3, Cloudflare R2, MinIO), or Fleet disk storage with a mirror on this bucket instead", bucket)
	case errors.Is(err, gateway.ErrNoConditionalPut):
		// Not the same as "has no such feature": this bucket took the
		// precondition and overwrote anyway, so nothing may be built on it.
		return false, fmt.Errorf("bucket %q does not enforce conditional writes, so it cannot be append-only; use Fleet disk storage with a mirror instead", bucket)
	default:
		return false, fmt.Errorf("bucket %q: %w", bucket, err)
	}
}

// s3Endpoint derives the S3-compatible host for a provider whose endpoint the
// admin did not spell out.
func s3Endpoint(kind, region string) (string, error) {
	if !regionRE.MatchString(region) {
		return "", fmt.Errorf("region %q is not a valid region name", region)
	}
	switch kind {
	case "b2":
		return "s3." + region + ".backblazeb2.com", nil
	case "s3":
		return "s3." + region + ".amazonaws.com", nil
	default:
		return "", fmt.Errorf("no endpoint is known for %q; give one explicitly", kind)
	}
}

// checkEndpoint holds an admin-supplied endpoint to a bare host[:port]. minio
// refuses a scheme or a path anyway, but it does so from inside the first
// probe, where the message reads like a connection failure rather than a typo -
// and "https://s3.example.com" is the typo everyone makes.
//
// A private or loopback address is deliberately *allowed*: a self-hosted MinIO
// on the LAN is a supported cloud-direct target, so an SSRF-style blocklist
// here would refuse a legitimate bucket. The endpoint is admin-only input, and
// it is stored only after the bucket has proven itself.
func checkEndpoint(e string) error {
	if strings.Contains(e, "://") {
		return fmt.Errorf("endpoint %q must be a bare host, without a scheme", e)
	}
	if strings.ContainsAny(e, "/ \\\t") || e != strings.TrimSpace(e) {
		return fmt.Errorf("endpoint %q must be a bare host[:port], without a path", e)
	}

	return nil
}

// s3ConnInfo is the connection info Fleet uses for a bucket it writes to
// itself, with its own admin key. Nothing here ever reaches a device.
func s3ConnInfo(bucket, region, endpoint, keyID, key string) blob.ConnectionInfo {
	return blob.ConnectionInfo{Type: "s3", Config: &s3.Options{
		BucketName:      bucket,
		Region:          region,
		Endpoint:        endpoint,
		AccessKeyID:     keyID,
		SecretAccessKey: key,
	}}
}

// cloudStoreFor opens the write-through backend for a cloud-direct target. It
// is what the gateway's per-target store cache calls for storage_mode "cloud",
// the way NewLocal is called for "disk".
func (s *Server) cloudStoreFor(ctx context.Context, t *store.Target) (gateway.ObjectStore, error) {
	keyID, key, err := s.targetCreds(ctx, t)
	if err != nil {
		return nil, fmt.Errorf("unsealing the credentials of target %q: %w", t.Name, err)
	}
	if keyID == "" || key == "" {
		return nil, fmt.Errorf("cloud-direct target %q has no stored credentials", t.Name)
	}

	newCloud := s.cloudStore
	if newCloud == nil {
		newCloud = gateway.NewCloud
	}

	return newCloud(ctx, s3ConnInfo(t.Bucket, t.Region, t.Endpoint, keyID, key), fleetPrefix)
}

// checkHostedRoot proves the hosted root exists and is writable *now*, by the
// user the fleet actually runs as — a stat of the mode bits would lie under
// setgid dirs, ACLs and read-only mounts.
func checkHostedRoot(p string) error {
	if !filepath.IsAbs(p) {
		return errors.New("path must be absolute")
	}
	fi, err := os.Stat(p)
	if err != nil {
		return errors.New("path does not exist: " + p)
	}
	if !fi.IsDir() {
		return errors.New("path is not a directory: " + p)
	}
	f, err := os.CreateTemp(p, ".warphold-write-")
	if err != nil {
		return errors.New("path is not writable by the fleet service user: " + p)
	}
	f.Close()
	os.Remove(f.Name())
	return nil
}

func (s *Server) handleTargetList(w http.ResponseWriter, r *http.Request) {
	ts, err := s.store().Targets(r.Context())
	if err != nil {
		adminFailed(w, "list targets", err)
		return
	}
	newest, stale, err := s.targetMirrorState(r.Context(), ts)
	if err != nil {
		// A store failure here would otherwise render every mirrored target
		// green ("no offsite problem") when the truth is that we do not know.
		// The page already 500s when store.Targets fails; be consistent.
		adminFailed(w, "read offsite state", err)
		return
	}

	out := make([]targetOut, 0, len(ts))

	for _, t := range ts {
		o := toTargetOut(t)
		o.MirroredAt, o.MirrorStale = newest[t.ID], stale[t.ID]
		out = append(out, o)
	}

	writeJSON(w, http.StatusOK, out)
}

// targetMirrorState folds every device's offsite progress onto its target: the
// newest mirror under it, and whether any device in it is behind. Three batch
// queries on a page load, never a query per target.
//
// Every failure is returned rather than swallowed: an empty result here is
// indistinguishable from "every mirror is fine", and a green offsite row on a
// broken store is the one lie a backup dashboard must not tell.
func (s *Server) targetMirrorState(ctx context.Context, targets []store.Target) (newest map[int64]*time.Time, stale map[int64]bool, err error) {
	newest, stale = map[int64]*time.Time{}, map[int64]bool{}

	mirrored := make(map[int64]bool, len(targets))
	for _, t := range targets {
		if t.MirrorKind != "" {
			mirrored[t.ID] = true
		}
	}

	if len(mirrored) == 0 {
		return newest, stale, nil
	}

	st := s.store()

	groups, err := st.Groups(ctx)
	if err != nil {
		return nil, nil, err
	}

	agents, err := st.Agents(ctx)
	if err != nil {
		return nil, nil, err
	}

	stats, err := st.RepoStats(ctx)
	if err != nil {
		return nil, nil, err
	}

	groupTarget := make(map[int64]int64, len(groups))
	for _, g := range groups {
		groupTarget[g.ID] = g.TargetID
	}

	now, every := s.now(), jobs.MirrorInterval(ctx, st)

	for _, a := range agents {
		tid := groupTarget[a.GroupID]
		// A revoked device is not part of "protected right now" (the overview
		// drops it too), so it can neither hold a target back nor freshen it.
		if a.RevokedAt != nil || !mirrored[tid] {
			continue
		}

		var at *time.Time
		if rs, ok := stats[a.ID]; ok {
			at = rs.MirroredAt
		}

		if at != nil && (newest[tid] == nil || at.After(*newest[tid])) {
			newest[tid] = at
		}

		if mirrorStale(at, now, every) {
			stale[tid] = true
		}
	}

	return newest, stale, nil
}
