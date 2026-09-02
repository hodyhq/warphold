package api

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet/health"
	"github.com/kopia/kopia/fleet/store"
)

var allowedCommands = map[string]bool{"snapshot-now": true, "pause": true, "resume": true, "verify": true}

func (s *Server) mountAdminAgents(m *mux.Router, adm func(http.HandlerFunc) http.HandlerFunc) {
	m.HandleFunc("/api/v1/fleet/agents", adm(s.handleAgentList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/agents/{id}", adm(s.handleAgentGet)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/agents/{id}/revoke", adm(s.handleAgentRevoke)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/agents/{id}/commands", adm(s.handleAgentCommand)).Methods(http.MethodPost)
}

type agentOut struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Hostname   string     `json:"hostname"`
	OS         string     `json:"os"`
	Arch       string     `json:"arch"`
	Version    string     `json:"version"`
	Scope      string     `json:"scope"`
	GroupID    int64      `json:"group_id"`
	EnrolledAt time.Time  `json:"enrolled_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	Health     string     `json:"health"`
}

func (s *Server) agentOut(a store.Agent, latest *store.Report) agentOut {
	return agentOut{ID: a.ID, Name: a.Name, Hostname: a.Hostname, OS: a.OS, Arch: a.Arch, Version: a.Version, Scope: a.Scope, GroupID: a.GroupID, EnrolledAt: a.EnrolledAt, LastSeenAt: a.LastSeenAt, RevokedAt: a.RevokedAt, Health: s.healthOf(a, latest)}
}

func (s *Server) healthOf(a store.Agent, latest *store.Report) string {
	in := health.Input{Revoked: a.RevokedAt != nil}
	if latest != nil {
		in.LastRunFailed = latest.Status == "error"
	}
	if ok, err := s.st.LastOKReport(context.Background(), a.ID); err == nil && ok != nil {
		t := ok.FinishedAt
		in.LastOK = &t
	}
	return health.Status(in, s.now())
}

func (s *Server) handleAgentList(w http.ResponseWriter, r *http.Request) {
	as, err := s.st.Agents(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	latest, _ := s.st.LatestReports(r.Context())
	out := make([]agentOut, 0, len(as))
	for _, a := range as {
		var lr *store.Report
		if x, ok := latest[a.ID]; ok {
			lr = &x
		}
		out = append(out, s.agentOut(a, lr))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAgentGet(w http.ResponseWriter, r *http.Request) {
	a, err := s.st.Agent(r.Context(), mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}
	reports, _ := s.st.ReportsForAgent(r.Context(), a.ID, 20)
	var lr *store.Report
	if len(reports) > 0 {
		lr = &reports[0]
	}
	// Flatten agentOut's fields alongside reports (spec: "same object + reports:[last 20]").
	writeJSON(w, http.StatusOK, struct {
		agentOut
		Reports []store.Report `json:"reports"`
	}{s.agentOut(*a, lr), reports})
}

func (s *Server) handleAgentRevoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := s.st.Agent(ctx, mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}
	// best effort; keys may already be gone. Each lookup step logs and skips
	// cleanup on its own failure rather than aborting the revoke below.
	if g, err := s.st.Group(ctx, a.GroupID); err != nil {
		log.Printf("warphold fleet: revoke %s: b2 key cleanup skipped: %v", a.ID, err)
	} else if t, err := s.st.Target(ctx, g.TargetID); err != nil {
		log.Printf("warphold fleet: revoke %s: b2 key cleanup skipped: %v", a.ID, err)
	} else if spec, err := s.specFor(ctx, t); err != nil {
		log.Printf("warphold fleet: revoke %s: b2 key cleanup skipped: %v", a.ID, err)
	} else if b, err := s.bundleFor(ctx, a); err != nil {
		log.Printf("warphold fleet: revoke %s: b2 key cleanup skipped: %v", a.ID, err)
	} else if err := s.provisioner().Revoke(ctx, spec, b); err != nil {
		log.Printf("warphold fleet: revoke %s: b2 key cleanup skipped: %v", a.ID, err)
	}
	if err := s.st.RevokeAgent(ctx, a.ID, s.now()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentCommand(w http.ResponseWriter, r *http.Request) {
	var in struct{ Kind, Source string }
	if err := decode(r, &in); err != nil || !allowedCommands[in.Kind] {
		writeErr(w, http.StatusBadRequest, "kind must be one of snapshot-now, pause, resume, verify")
		return
	}
	a, err := s.st.Agent(r.Context(), mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}
	id, err := s.st.AddCommand(r.Context(), &store.Command{AgentID: a.ID, Kind: in.Kind, Source: in.Source})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}
