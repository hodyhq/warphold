package api

import (
	"net/http"
	"os"
	"time"

	"github.com/kopia/kopia/fleet/store"
)

type targetOut struct {
	ID                   int64      `json:"id"`
	Name                 string     `json:"name"`
	Kind                 string     `json:"kind"`
	Bucket               string     `json:"bucket,omitempty"`
	Region               string     `json:"region,omitempty"`
	Path                 string     `json:"path,omitempty"`
	ObjectLockVerifiedAt *time.Time `json:"object_lock_verified_at,omitempty"`
}

func toTargetOut(t store.Target) targetOut {
	return targetOut{ID: t.ID, Name: t.Name, Kind: t.Kind, Bucket: t.Bucket, Region: t.Region, Path: t.Path, ObjectLockVerifiedAt: t.ObjectLockVerifiedAt}
}

func (s *Server) handleTargetCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name, Kind, Bucket, Region, Path, KeyID, Key string
	}
	if err := decode(r, &in); err != nil || in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name and kind are required")
		return
	}
	t := &store.Target{Name: in.Name, Kind: in.Kind, Bucket: in.Bucket, Region: in.Region, Path: in.Path}
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
			writeErr(w, http.StatusInternalServerError, err.Error())
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
	default:
		writeErr(w, http.StatusBadRequest, "kind must be b2 or filesystem")
		return
	}
	id, err := s.st.CreateTarget(r.Context(), t)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "object_lock_verified": verified})
}

func (s *Server) handleTargetList(w http.ResponseWriter, r *http.Request) {
	ts, err := s.st.Targets(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]targetOut, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTargetOut(t))
	}
	writeJSON(w, http.StatusOK, out)
}
