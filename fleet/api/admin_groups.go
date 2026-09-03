package api

import (
	"errors"
	"net/http"

	"github.com/kopia/kopia/fleet/store"
)

type groupOut struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	TargetID   int64  `json:"target_id"`
	TemplateID int64  `json:"template_id"`
}

func (s *Server) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string `json:"name"`
		TargetID   int64  `json:"target_id"`
		TemplateID int64  `json:"template_id"`
	}
	if err := decode(r, &in); err != nil || in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name, target_id and template_id are required")
		return
	}
	if _, err := s.store().Target(r.Context(), in.TargetID); err != nil {
		writeErr(w, http.StatusBadRequest, "unknown target_id")
		return
	}
	if _, err := s.store().Template(r.Context(), in.TemplateID); err != nil {
		writeErr(w, http.StatusBadRequest, "unknown template_id")
		return
	}
	id, err := s.store().CreateGroup(r.Context(), &store.Group{Name: in.Name, TargetID: in.TargetID, TemplateID: in.TemplateID, CreatedAt: s.now()})
	if err != nil {
		adminFailed(w, "create group", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// handleGroupUpdate applies a partial update to a group: name, target_id and
// template_id may each be supplied independently. A target_id change is
// refused with 409 once the group has enrolled a device -- the device's
// repository lives on the old target, so moving it is a migration the admin
// has to do deliberately, not a side effect of a rename.
func (s *Server) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in struct {
		Name       *string `json:"name"`
		TargetID   *int64  `json:"target_id"`
		TemplateID *int64  `json:"template_id"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	if in.Name != nil && *in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name cannot be empty")
		return
	}
	g, err := s.store().Group(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "group not found")
		return
	}
	if in.TargetID != nil {
		if _, err := s.store().Target(r.Context(), *in.TargetID); err != nil {
			writeErr(w, http.StatusBadRequest, "unknown target_id")
			return
		}
		if *in.TargetID != g.TargetID {
			has, err := s.store().GroupHasAgents(r.Context(), id)
			if err != nil {
				adminFailed(w, "update group", err)
				return
			}
			if has {
				writeErr(w, http.StatusConflict, "group has enrolled devices; their repositories live on the current target, so repointing it is a migration, not a rename")
				return
			}
		}
	}
	if in.TemplateID != nil {
		if _, err := s.store().Template(r.Context(), *in.TemplateID); err != nil {
			writeErr(w, http.StatusBadRequest, "unknown template_id")
			return
		}
	}
	if err := s.store().UpdateGroup(r.Context(), id, in.Name, in.TargetID, in.TemplateID); err != nil {
		adminFailed(w, "update group", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGroupDelete removes a group. It is refused with 409 while a
// non-revoked agent or a live enrollment token still references it -- either
// would be left pointing at nothing.
func (s *Server) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	switch err := s.store().DeleteGroup(r.Context(), id, s.now()); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrGroupInUse):
		writeErr(w, http.StatusConflict, "group has enrolled devices or a live enrollment token")
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "group not found")
	default:
		adminFailed(w, "delete group", err)
	}
}

func (s *Server) handleGroupList(w http.ResponseWriter, r *http.Request) {
	gs, err := s.store().Groups(r.Context())
	if err != nil {
		adminFailed(w, "list groups", err)
		return
	}
	out := make([]groupOut, 0, len(gs))
	for _, g := range gs {
		out = append(out, groupOut{ID: g.ID, Name: g.Name, TargetID: g.TargetID, TemplateID: g.TemplateID})
	}
	writeJSON(w, http.StatusOK, out)
}
