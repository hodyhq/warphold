package api

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet/health"
	"github.com/kopia/kopia/fleet/jobs"
	"github.com/kopia/kopia/fleet/store"
)

// mirrorOut is a device's offsite copy: when its blobs last reached the
// mirror bucket, how much is there, and whether that was long enough ago to
// be a problem. It is null for a device whose target has no mirror at all -
// "no offsite" and "offsite behind" are different states and the UI says so.
type mirrorOut struct {
	MirroredAt    *time.Time `json:"mirrored_at"`
	MirroredBytes int64      `json:"mirrored_bytes"`
	Stale         bool       `json:"stale"`
}

// mirrorStale calls a copy stale once it is older than three mirror
// intervals, so a fleet can miss two runs (one slow, one failed) before it
// complains. A device that has never been mirrored is stale by definition:
// its target has a mirror and the device is not in it.
func mirrorStale(at *time.Time, now time.Time, every time.Duration) bool {
	return at == nil || now.Sub(*at) > 3*every
}

// mirrorFor resolves a device's offsite state through its group's target, or
// nil when that target keeps no mirror.
//
// A store failure is returned, not swallowed: omitting the line would read as
// "this fleet keeps no offsite copy", which is the wrong answer in the
// dangerous direction.
func (s *Server) mirrorFor(ctx context.Context, a store.Agent) (*mirrorOut, error) {
	st := s.store()

	g, err := st.Group(ctx, a.GroupID)
	if err != nil {
		return nil, err
	}

	t, err := st.Target(ctx, g.TargetID)
	if err != nil {
		return nil, err
	}

	if t.MirrorKind == "" {
		return nil, nil
	}

	var m mirrorOut
	// No stats row yet means nothing has been mirrored, which is exactly what
	// a zero mirrorOut says; a read failure is not worth failing the page over.
	if rs, err := st.RepoStat(ctx, a.ID); err == nil {
		m.MirroredAt, m.MirroredBytes = rs.MirroredAt, rs.MirroredBytes
	}

	m.Stale = mirrorStale(m.MirroredAt, s.now(), jobs.MirrorInterval(ctx, st))

	return &m, nil
}

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

func (s *Server) agentOut(a store.Agent, latest *store.Report, lastOK *time.Time) agentOut {
	return agentOut{ID: a.ID, Name: a.Name, Hostname: a.Hostname, OS: a.OS, Arch: a.Arch, Version: a.Version, Scope: a.Scope, GroupID: a.GroupID, EnrolledAt: a.EnrolledAt, LastSeenAt: a.LastSeenAt, RevokedAt: a.RevokedAt, Health: s.healthOf(a, latest, lastOK)}
}

// healthOf takes the last successful snapshot time rather than looking it up:
// the list handler resolves every agent's in one batch query, and both
// callers scope their lookup to the request context instead of the detached
// context.Background() this used to run on.
func (s *Server) healthOf(a store.Agent, latest *store.Report, lastOK *time.Time) string {
	in := health.Input{Revoked: a.RevokedAt != nil, LastOK: lastOK}
	if latest != nil {
		in.LastRunFailed = latest.Status == "error"
	}
	return health.Status(in, s.now())
}

func (s *Server) handleAgentList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	as, err := s.store().Agents(ctx)
	if err != nil {
		adminFailed(w, "list agents", err)
		return
	}
	latest, _ := s.store().LatestReports(ctx)
	// One batch query, not one LastOKReport per agent: this endpoint renders
	// the whole fleet and the per-row lookup made it O(agents) round trips.
	lastOK, _ := s.store().LastOKReports(ctx)
	out := make([]agentOut, 0, len(as))
	for _, a := range as {
		var lr *store.Report
		if x, ok := latest[a.ID]; ok {
			lr = &x
		}
		var ok *time.Time
		if t, found := lastOK[a.ID]; found {
			ok = &t
		}
		out = append(out, s.agentOut(a, lr, ok))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAgentGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := s.store().Agent(ctx, mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}
	reports, _ := s.store().ReportsForAgent(ctx, a.ID, 20)
	var lr *store.Report
	if len(reports) > 0 {
		lr = &reports[0]
	}
	var lastOK *time.Time
	if ok, err := s.store().LastOKReport(ctx, a.ID); err == nil && ok != nil {
		t := ok.FinishedAt
		lastOK = &t
	}
	mirror, err := s.mirrorFor(ctx, *a)
	if err != nil {
		adminFailed(w, "read offsite state", err)
		return
	}

	// Flatten agentOut's fields alongside reports (spec: "same object + reports:[last 20]").
	writeJSON(w, http.StatusOK, struct {
		agentOut
		Reports []store.Report `json:"reports"`
		Mirror  *mirrorOut     `json:"mirror"`
	}{s.agentOut(*a, lr, lastOK), reports, mirror})
}

func (s *Server) handleAgentRevoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := s.store().Agent(ctx, mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}
	// best effort; keys may already be gone. Each lookup step logs and skips
	// cleanup on its own failure rather than aborting the revoke below. Errors
	// here can carry B2 response text or provisioning URLs, so only the safe
	// category is logged, never err itself.
	if g, err := s.store().Group(ctx, a.GroupID); err != nil {
		log.Printf("warphold fleet: revoke %s: b2 key cleanup skipped (%s)", a.ID, errCategory(err))
	} else if t, err := s.store().Target(ctx, g.TargetID); err != nil {
		log.Printf("warphold fleet: revoke %s: b2 key cleanup skipped (%s)", a.ID, errCategory(err))
	} else if spec, err := s.specFor(ctx, t); err != nil {
		log.Printf("warphold fleet: revoke %s: b2 key cleanup skipped (%s)", a.ID, errCategory(err))
	} else if b, err := s.bundleFor(ctx, a); err != nil {
		log.Printf("warphold fleet: revoke %s: b2 key cleanup skipped (%s)", a.ID, errCategory(err))
	} else if err := s.provisioner().Revoke(ctx, spec, b); err != nil {
		log.Printf("warphold fleet: revoke %s: b2 key cleanup skipped (%s)", a.ID, errCategory(err))
	}
	if err := s.store().RevokeAgent(ctx, a.ID, s.now()); err != nil {
		adminFailed(w, "revoke agent", err)
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
	a, err := s.store().Agent(r.Context(), mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}
	id, err := s.store().AddCommand(r.Context(), &store.Command{AgentID: a.ID, Kind: in.Kind, Source: in.Source, CreatedAt: s.now()})
	if err != nil {
		adminFailed(w, "add command", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}
