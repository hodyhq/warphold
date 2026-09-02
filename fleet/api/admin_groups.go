package api

import (
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
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleGroupList(w http.ResponseWriter, r *http.Request) {
	gs, err := s.store().Groups(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]groupOut, 0, len(gs))
	for _, g := range gs {
		out = append(out, groupOut{ID: g.ID, Name: g.Name, TargetID: g.TargetID, TemplateID: g.TemplateID})
	}
	writeJSON(w, http.StatusOK, out)
}
