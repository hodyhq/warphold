package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/kopia/kopia/fleet/health"
	"github.com/kopia/kopia/fleet/store"
)

const (
	// overviewDays is the width of the per-device heartbeat strip and
	// overviewHours the width of the snapshot timeline, both in the payload's
	// oldest-first order.
	overviewClockSkew = 5 * time.Minute
	overviewDays      = 30
	overviewHours     = 24

	// fleetNameSetting is the display name shown in the header; it is written
	// by the settings endpoint and is empty until an admin sets one.
	fleetNameSetting = "fleet_name"
)

type overviewCounts struct {
	Agents  int `json:"agents"`
	Green   int `json:"green"`
	Yellow  int `json:"yellow"`
	Red     int `json:"red"`
	Unknown int `json:"unknown"`
	Targets int `json:"targets"`
}

type overviewBucket struct {
	Hour   time.Time `json:"hour"`
	OK     int       `json:"ok"`
	Failed int       `json:"failed"`
}

type overviewTimeline struct {
	Completed int              `json:"completed"`
	Failed    int              `json:"failed"`
	Buckets   []overviewBucket `json:"buckets"`
}

type overviewFailure struct {
	AgentID    string    `json:"agent_id"`
	Name       string    `json:"name"`
	FinishedAt time.Time `json:"finished_at"`
	Stderr     string    `json:"stderr"`
}

type overviewDevice struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Group  string `json:"group"`
	Health string `json:"health"`
	// Last is the relative time since the last good snapshot ("2 h ago"), or
	// "never". The server formats it so every client reads one clock - the
	// server's - instead of subtracting timestamps against a browser's.
	Last string `json:"last"`
	// SizeBytes is 0 until Plan 3's jobs collect Kopia repository stats, like
	// the fleet-wide StoredBytes.
	SizeBytes int64    `json:"size_bytes"`
	Days      []string `json:"days"`
}

type overviewOut struct {
	FleetName   string         `json:"fleet_name"`
	Counts      overviewCounts `json:"counts"`
	StoredBytes int64          `json:"stored_bytes"`
	// DedupRatio is null until repository stats exist; the UI hides the Stored
	// tile rather than showing a made-up number.
	DedupRatio    *float64         `json:"dedup_ratio"`
	Last24h       overviewTimeline `json:"last24h"`
	LatestFailure *overviewFailure `json:"latest_failure"`
	Devices       []overviewDevice `json:"devices"`
}

// relativeSince renders an age the way the device rows read it. Anything in
// the future (a client clock ahead of the server's) reads as "just now"
// rather than a negative age.
func relativeSince(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
}

// handleOverview answers the Fleet dashboard in one round trip: five batched
// queries (agents, groups, targets, the newest and newest-good report per
// agent) plus one 30-day report window, all folded together in Go. Nothing
// here may become a per-agent or per-day query - this endpoint is polled every
// 30 s by every open dashboard.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := s.store()

	agents, err := st.Agents(ctx)
	if err != nil {
		adminFailed(w, "list agents", err)
		return
	}
	groups, err := st.Groups(ctx)
	if err != nil {
		adminFailed(w, "list groups", err)
		return
	}
	targets, err := st.Targets(ctx)
	if err != nil {
		adminFailed(w, "list targets", err)
		return
	}
	latest, err := st.LatestReports(ctx)
	if err != nil {
		adminFailed(w, "read reports", err)
		return
	}
	lastOK, err := st.LastOKReports(ctx)
	if err != nil {
		adminFailed(w, "read reports", err)
		return
	}
	fleetName, err := st.Setting(ctx, fleetNameSetting)
	if err != nil {
		adminFailed(w, "read settings", err)
		return
	}

	now := s.now().UTC()
	// Day 0 of a strip is 29 days ago (UTC midnight) and bucket 0 is 23 hours
	// ago (on the hour); one window covers whichever reaches further back.
	dayStart := now.Truncate(24*time.Hour).AddDate(0, 0, -(overviewDays - 1))
	hourStart := now.Truncate(time.Hour).Add(-(overviewHours - 1) * time.Hour)
	since := dayStart
	if hourStart.Before(since) {
		since = hourStart
	}
	// Agents report with their own clocks; allow a little skew but keep
	// clearly future-dated reports out of the timeline.
	reports, err := st.ReportsBetween(ctx, since, now.Add(overviewClockSkew))
	if err != nil {
		adminFailed(w, "read reports", err)
		return
	}

	// A revoked device is not part of "protected right now": it is dropped
	// from the counts, the strips and the timeline alike, so the four health
	// buckets always add up to Counts.Agents.
	live := make(map[string]store.Agent, len(agents))
	days := make(map[string][]string, len(agents))
	for _, a := range agents {
		if a.RevokedAt != nil {
			continue
		}
		live[a.ID] = a
		strip := make([]string, overviewDays)
		for i := range strip {
			strip[i] = "none"
		}
		days[a.ID] = strip
	}

	buckets := make([]overviewBucket, overviewHours)
	for i := range buckets {
		buckets[i] = overviewBucket{Hour: hourStart.Add(time.Duration(i) * time.Hour)}
	}

	var latestFailure *overviewFailure
	for _, rep := range reports {
		// Only snapshots count: an agent reports every command it applies
		// (a pause that runs no backup included) as kind='command', status='ok'.
		if rep.Kind != "snapshot" {
			continue
		}
		agent, ok := live[rep.AgentID]
		if !ok {
			continue
		}
		finished := rep.FinishedAt.UTC()
		if d := int(finished.Truncate(24*time.Hour).Sub(dayStart) / (24 * time.Hour)); d >= 0 && d < overviewDays {
			strip := days[rep.AgentID]
			switch {
			case rep.Status == "ok":
				strip[d] = "good"
			case strip[d] == "none":
				// A day with errors and no success is bad; one good snapshot
				// that day wins, whichever ran last.
				strip[d] = "bad"
			}
		}
		if h := int(finished.Truncate(time.Hour).Sub(hourStart) / time.Hour); h >= 0 && h < overviewHours {
			if rep.Status == "ok" {
				buckets[h].OK++
			} else {
				buckets[h].Failed++
			}
		}
		if rep.Status != "ok" {
			// ReportsBetween is oldest first, so the last error wins.
			latestFailure = &overviewFailure{AgentID: rep.AgentID, Name: agent.Name, FinishedAt: finished, Stderr: rep.Stderr}
		}
	}

	out := overviewOut{
		FleetName: fleetName,
		Counts:    overviewCounts{Agents: len(live), Targets: len(targets)},
		Last24h:   overviewTimeline{Buckets: buckets},
		Devices:   make([]overviewDevice, 0, len(live)),
	}
	for _, b := range buckets {
		out.Last24h.Completed += b.OK
		out.Last24h.Failed += b.Failed
	}
	out.LatestFailure = latestFailure

	groupNames := make(map[int64]string, len(groups))
	for _, g := range groups {
		groupNames[g.ID] = g.Name
	}
	// agents keeps its enrolled_at order; the dashboard lists devices in the
	// order they joined the fleet.
	for _, a := range agents {
		if _, ok := live[a.ID]; !ok {
			continue
		}
		var lr *store.Report
		if x, found := latest[a.ID]; found {
			lr = &x
		}
		var okAt *time.Time
		last := "never"
		if t, found := lastOK[a.ID]; found {
			okAt = &t
			last = relativeSince(now.Sub(t))
		}
		hs := s.healthOf(a, lr, okAt)
		switch hs {
		case health.Green:
			out.Counts.Green++
		case health.Yellow:
			out.Counts.Yellow++
		case health.Red:
			out.Counts.Red++
		default:
			out.Counts.Unknown++
		}
		out.Devices = append(out.Devices, overviewDevice{
			ID: a.ID, Name: a.Name, Group: groupNames[a.GroupID],
			Health: hs, Last: last, Days: days[a.ID],
		})
	}

	writeJSON(w, http.StatusOK, out)
}
