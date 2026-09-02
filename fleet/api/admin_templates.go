package api

import (
	"encoding/json"
	"net/http"

	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/snapshot/policy"
)

type templateIn struct {
	Name    string          `json:"name"`
	Sources []string        `json:"sources"`
	Policy  json.RawMessage `json:"policy"`
}

type templateOut struct {
	ID      int64           `json:"id"`
	Name    string          `json:"name"`
	Sources []string        `json:"sources"`
	Policy  json.RawMessage `json:"policy"`
}

func (in *templateIn) validate() error {
	if in.Name == "" || len(in.Sources) == 0 {
		return errMsg("name and at least one source are required")
	}
	var p policy.Policy
	if len(in.Policy) == 0 {
		in.Policy = json.RawMessage(`{}`)
	}
	return json.Unmarshal(in.Policy, &p) // must be a Kopia policy object
}

type errMsg string

func (e errMsg) Error() string { return string(e) }

func (s *Server) handleTemplateCreate(w http.ResponseWriter, r *http.Request) {
	var in templateIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.st.CreateTemplate(r.Context(), &store.Template{Name: in.Name, Sources: in.Sources, PolicyJSON: in.Policy})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleTemplateUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in templateIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.st.Template(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "template not found")
		return
	}
	if err := s.st.UpdateTemplate(r.Context(), &store.Template{ID: id, Name: in.Name, Sources: in.Sources, PolicyJSON: in.Policy}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTemplateList(w http.ResponseWriter, r *http.Request) {
	ts, err := s.st.Templates(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]templateOut, 0, len(ts))
	for _, t := range ts {
		out = append(out, templateOut{ID: t.ID, Name: t.Name, Sources: t.Sources, Policy: t.PolicyJSON})
	}
	writeJSON(w, http.StatusOK, out)
}
