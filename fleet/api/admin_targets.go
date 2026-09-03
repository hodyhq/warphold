package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/kopia/kopia/fleet/jobs"
	"github.com/kopia/kopia/fleet/store"
)

// defaultHostedRoot is where a hosted target keeps device repositories when
// the admin does not name a path (spec D5). It is not created for them: the
// installer makes it, and a typo should fail loudly rather than silently
// backing the fleet up into a fresh empty directory.
const defaultHostedRoot = "/srv/warphold/hosted"

type targetOut struct {
	ID                   int64      `json:"id"`
	Name                 string     `json:"name"`
	Kind                 string     `json:"kind"`
	Bucket               string     `json:"bucket,omitempty"`
	Region               string     `json:"region,omitempty"`
	Path                 string     `json:"path,omitempty"`
	ObjectLockVerifiedAt *time.Time `json:"object_lock_verified_at,omitempty"`

	// Hosted targets. Sealed credentials are deliberately absent: nothing in
	// this struct may carry a key off the server.
	StorageMode          string     `json:"storage_mode,omitempty"`
	MirrorKind           string     `json:"mirror_kind,omitempty"`
	MirrorBucket         string     `json:"mirror_bucket,omitempty"`
	MirrorRegion         string     `json:"mirror_region,omitempty"`
	MirrorLockVerifiedAt *time.Time `json:"mirror_lock_verified_at,omitempty"`

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
		MirrorKind:           t.MirrorKind, MirrorBucket: t.MirrorBucket, MirrorRegion: t.MirrorRegion,
		MirrorLockVerifiedAt: t.MirrorLockVerifiedAt,
	}
}

type targetInput struct {
	Name, Kind, Bucket, Region, Path string
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
	verified := false
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
				verified = true
			}
		}
	case "hosted":
		if !s.applyHosted(w, &in, t) {
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
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "object_lock_verified": verified})
}

// applyHosted validates a hosted target and fills in its storage fields. It
// writes the error response and reports false when the input is rejected.
func (s *Server) applyHosted(w http.ResponseWriter, in *targetInput, t *store.Target) bool {
	switch in.StorageMode {
	case "cloud":
		// Task 11 builds the cloud-direct gateway backend. Stubbed so M1
		// cannot half-ship M2: no target is created.
		writeErr(w, http.StatusNotImplemented, "cloud-direct storage lands in a later release")
		return false
	case "disk":
	default:
		writeErr(w, http.StatusBadRequest, "storage_mode must be disk or cloud for hosted targets")
		return false
	}
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
	if in.MirrorKind != "b2" {
		writeErr(w, http.StatusBadRequest, "mirror_kind must be b2")
		return false
	}
	if in.MirrorBucket == "" || in.MirrorKeyID == "" || in.MirrorKey == "" {
		writeErr(w, http.StatusBadRequest, "mirror_bucket, mirror_key_id and mirror_key are required for a b2 mirror")
		return false
	}
	sealed, err := s.sealCreds(targetCreds{KeyID: in.MirrorKeyID, Key: in.MirrorKey})
	if err != nil {
		adminFailed(w, "seal mirror credentials", err)
		return false
	}
	// Task 12 verifies the mirror bucket's Object Lock and stamps
	// mirror_lock_verified_at; Task 13 uploads to it.
	t.MirrorKind, t.MirrorBucket, t.MirrorRegion, t.SealedMirrorKey = in.MirrorKind, in.MirrorBucket, in.MirrorRegion, sealed
	return true
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
	newest, stale := s.targetMirrorState(r.Context(), ts)

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
func (s *Server) targetMirrorState(ctx context.Context, targets []store.Target) (newest map[int64]*time.Time, stale map[int64]bool) {
	newest, stale = map[int64]*time.Time{}, map[int64]bool{}

	mirrored := make(map[int64]bool, len(targets))
	for _, t := range targets {
		if t.MirrorKind != "" {
			mirrored[t.ID] = true
		}
	}

	if len(mirrored) == 0 {
		return newest, stale
	}

	st := s.store()

	groups, err := st.Groups(ctx)
	if err != nil {
		return newest, stale
	}

	agents, err := st.Agents(ctx)
	if err != nil {
		return newest, stale
	}

	stats, err := st.RepoStats(ctx)
	if err != nil {
		return newest, stale
	}

	groupTarget := make(map[int64]int64, len(groups))
	for _, g := range groups {
		groupTarget[g.ID] = g.TargetID
	}

	now, every := s.now(), jobs.MirrorInterval(ctx, st)

	for _, a := range agents {
		tid := groupTarget[a.GroupID]
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

	return newest, stale
}
