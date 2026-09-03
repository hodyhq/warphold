package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet/store"
)

func (s *Server) mountAdmin(m *mux.Router) {
	adm := func(h http.HandlerFunc) http.HandlerFunc {
		return s.requireHost(s.requireActivated(s.requireAdmin(h)))
	}
	m.HandleFunc("/api/v1/fleet/targets", adm(s.handleTargetCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/targets", adm(s.handleTargetList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/targets/{id}/mirror", adm(s.handleTargetMirrorSet)).Methods(http.MethodPut)
	m.HandleFunc("/api/v1/fleet/templates", adm(s.handleTemplateCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/templates/{id}", adm(s.handleTemplateUpdate)).Methods(http.MethodPut)
	m.HandleFunc("/api/v1/fleet/templates", adm(s.handleTemplateList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/groups", adm(s.handleGroupCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/groups", adm(s.handleGroupList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/groups/{id}", adm(s.handleGroupUpdate)).Methods(http.MethodPut)
	m.HandleFunc("/api/v1/fleet/groups/{id}", adm(s.handleGroupDelete)).Methods(http.MethodDelete)
	m.HandleFunc("/api/v1/fleet/admins", adm(s.handleAdminList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/admins", adm(s.handleAdminCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/admins/me/password", adm(s.handleAdminPassword)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/admins/{id}", adm(s.handleAdminDelete)).Methods(http.MethodDelete)
	m.HandleFunc("/api/v1/fleet/overview", adm(s.handleOverview)).Methods(http.MethodGet)    // Task 12
	m.HandleFunc("/api/v1/fleet/settings", adm(s.handleSettingsGet)).Methods(http.MethodGet) // Task 14
	m.HandleFunc("/api/v1/fleet/settings", adm(s.handleSettingsUpdate)).Methods(http.MethodPut)
	s.mountAdminEnrollment(m, adm) // Task 9/11: tokens + agents
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	return id, err == nil
}

type targetCreds struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key"`
}

func (s *Server) sealCreds(c targetCreds) ([]byte, error) {
	b, _ := json.Marshal(c)
	return s.sealKey().Seal(b)
}

// targetCreds unseals the admin credentials of a target.
func (s *Server) targetCreds(_ context.Context, t *store.Target) (string, string, error) {
	if len(t.SealedAdminKey) == 0 {
		return "", "", nil
	}
	b, err := s.sealKey().Open(t.SealedAdminKey)
	if err != nil {
		return "", "", err
	}
	var c targetCreds
	if err := json.Unmarshal(b, &c); err != nil {
		return "", "", err
	}
	return c.KeyID, c.Key, nil
}
