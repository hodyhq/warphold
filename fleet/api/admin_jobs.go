package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet/jobs"
	"github.com/kopia/kopia/fleet/store"
)

// jobsPerAgent is how many rows GET /agents/{id}/jobs returns.
const jobsPerAgent = 50

func (s *Server) mountAdminJobs(m *mux.Router, adm func(http.HandlerFunc) http.HandlerFunc) {
	m.HandleFunc("/api/v1/fleet/jobs", adm(s.handleJobCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/agents/{id}/jobs", adm(s.handleAgentJobs)).Methods(http.MethodGet)
}

// jobOut is one row of the jobs table as the dashboard reads it.
type jobOut struct {
	ID           int64      `json:"id"`
	Kind         string     `json:"kind"`
	AgentID      string     `json:"agent_id,omitempty"`
	ScheduledFor time.Time  `json:"scheduled_for"`
	StartedAt    *time.Time `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	Status       string     `json:"status"`
	Detail       string     `json:"detail"`
}

func jobsOut(js []store.Job) []jobOut {
	out := make([]jobOut, 0, len(js))
	for _, j := range js {
		out = append(out, jobOut{
			ID: j.ID, Kind: j.Kind, AgentID: j.AgentID, ScheduledFor: j.ScheduledFor,
			StartedAt: j.StartedAt, FinishedAt: j.FinishedAt, Status: j.Status, Detail: j.Detail,
		})
	}

	return out
}

// handleJobCreate queues one job. It is 202, not 201: the scheduler picks the
// row up on its next tick, so nothing has run yet when this returns.
func (s *Server) handleJobCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind    string `json:"kind"`
		AgentID string `json:"agent_id"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}

	if !jobs.HasKind(in.Kind) {
		writeErr(w, http.StatusBadRequest, "kind must be one of "+strings.Join(jobs.KindList(), ", "))
		return
	}

	ctx := r.Context()
	if in.AgentID != "" {
		// A job whose agent does not exist would fail the foreign key on
		// insert; answer for the agent rather than for the database.
		if _, err := s.store().Agent(ctx, in.AgentID); err != nil {
			writeErr(w, http.StatusNotFound, "agent not found")
			return
		}
	}

	id, err := s.store().EnqueueJob(ctx, &store.Job{Kind: in.Kind, AgentID: in.AgentID, ScheduledFor: s.now()})
	if err != nil {
		adminFailed(w, "enqueue job", err)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"id": id})
}

func (s *Server) handleAgentJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	a, err := s.store().Agent(ctx, mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}

	js, err := s.store().JobsForAgent(ctx, a.ID, jobsPerAgent)
	if err != nil {
		adminFailed(w, "list jobs", err)
		return
	}

	writeJSON(w, http.StatusOK, jobsOut(js))
}
