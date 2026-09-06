package jobs

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/kopia/kopia/fleet/health"
	"github.com/kopia/kopia/fleet/mail"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/internal/units"
)

// fleetNameSetting is the same key fleet/api's settings endpoint writes.
// Kept here rather than imported because fleet/api imports fleet/jobs, not
// the other way round - the same reason publicURLSetting is duplicated in
// repo.go.
const fleetNameSetting = "fleet_name"

const (
	digestSubject = "WarpHold weekly digest"

	// failingFor is how long a job kind has to have been erroring, run after
	// run with no success in between, before the digest calls it out by name.
	failingFor = 7 * 24 * time.Hour

	// failingLookback bounds how far back a kind's history is scanned for
	// failingFor: enough runs to cover a week even at the fastest cadence
	// (mirror, hourly) without an unbounded query.
	//
	// ponytail: a kind scheduled more often than every ~34 minutes could run
	// out of lookback before reaching a week of consecutive failures and so
	// under-report; raise this, or scan by time instead of count, if that
	// ever becomes a real cadence.
	failingLookback = 300
)

// digestJobKinds is every job kind the digest reports on, in the fixed order
// it lists them - every kind but itself, since a run cannot report on its own
// outcome before it has one.
var digestJobKinds = []string{"mirror", "verify", "test-restore", "maintenance", "reap", "stats"}

//go:embed digest.html.tmpl
var digestHTMLSrc string

var digestHTMLTmpl = template.Must(template.New("digest").Parse(digestHTMLSrc))

// Digest returns the runner for the "digest" job: one weekly email to every
// admin summarising fleet health, so nobody has to open the dashboard to
// notice a device has gone quiet (spec §7.4). A fleet with no SMTP configured
// - the default state - is not a failure: the run is recorded 'skipped', with
// a detail saying why, rather than 'error'.
func Digest(st *store.Store, open seal.Opener, send mail.Sender) Runner {
	return func(ctx context.Context, j store.Job) (string, error) {
		cfg, err := mail.Load(ctx, st, open)
		if err != nil {
			return "", fmt.Errorf("reading smtp settings: %w", err)
		}

		if cfg.From == "" {
			return "smtp not configured", ErrSkipped
		}

		admins, err := st.Admins(ctx)
		if err != nil {
			return "", fmt.Errorf("listing admins: %w", err)
		}

		to := make([]string, 0, len(admins))
		for _, a := range admins {
			if a.Email != "" {
				to = append(to, a.Email)
			}
		}

		if len(to) == 0 {
			return "no admin to send to", ErrSkipped
		}

		d, err := buildDigest(ctx, st, time.Now())
		if err != nil {
			return "", fmt.Errorf("building the digest: %w", err)
		}

		var html strings.Builder
		if err := digestHTMLTmpl.Execute(&html, d); err != nil {
			return "", fmt.Errorf("rendering the digest: %w", err)
		}

		if err := send(ctx, to, digestSubject, d.text(), html.String()); err != nil {
			// The server's own error is the useful part ("550 5.7.1 relaying
			// denied"); it is kept, minus anything that looks like the
			// account's credentials, in case a relay echoes them back.
			return "", errors.New(redactCredentials(err.Error(), cfg.Username, cfg.Password))
		}

		return fmt.Sprintf("sent to %d admin(s)", len(to)), nil
	}
}

// redactCredentials keeps both halves of the SMTP login out of an error text
// that quotes the command it failed on - some relays echo the rejected
// username, and a few echo the whole AUTH argument. Duplicated in
// fleet/api/admin_mail.go, which cannot be imported here without a cycle
// (fleet/api already imports fleet/jobs).
func redactCredentials(msg, username, password string) string {
	for _, secret := range []string{password, username} {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}

	return msg
}

// digestDevice is one row of the digest's device table. Name is the fleet
// admin's own label for the device, never its hostname: the digest names no
// host but the Fleet's own public URL.
type digestDevice struct {
	Name    string
	Health  string
	LastOK  string
	Bytes   string
	Offsite string
}

// digestJobResult is the most recent outcome of one job kind, fleet-wide.
type digestJobResult struct {
	Kind, Status, Since string
}

// digestData is everything the digest renders, in both bodies.
type digestData struct {
	FleetName   string
	PublicURL   string
	GeneratedAt string
	Devices     []digestDevice
	TotalBytes  string
	// DedupRatio is "" until at least one device has been measured - the same
	// contract the overview API keeps, so the digest never claims a ratio it
	// computed from a division by zero.
	DedupRatio string
	// Failing names every job kind that has done nothing but error, run after
	// run, for at least failingFor.
	Failing    []string
	JobResults []digestJobResult
	// UnackedKits is how many live devices have no row in kit_acks: nobody has
	// said they hold that device's printed recovery kit. It is the one number
	// here that is about the humans rather than the machines, and a fleet whose
	// kits were never saved cannot be restored from without the Fleet server.
	UnackedKits int
}

// text renders the plain-text body: every mail client can read it, and it is
// what a terminal `fleet jobs run digest` test send shows first.
func (d *digestData) text() string {
	var b strings.Builder

	name := d.FleetName
	if name == "" {
		name = "WarpHold Fleet"
	}

	fmt.Fprintf(&b, "%s -- weekly digest\n", name)
	fmt.Fprintf(&b, "Generated %s", d.GeneratedAt)

	if d.PublicURL != "" {
		fmt.Fprintf(&b, " for %s", d.PublicURL)
	}

	b.WriteString("\n\n")

	fmt.Fprintf(&b, "Fleet totals: stored %s", d.TotalBytes)

	if d.DedupRatio != "" {
		fmt.Fprintf(&b, " (dedup ratio %s)", d.DedupRatio)
	}

	b.WriteString("\n\n")

	if len(d.Failing) > 0 {
		b.WriteString("Needs attention:\n")

		for _, kind := range d.Failing {
			fmt.Fprintf(&b, "  - %s has been failing for over a week\n", kind)
		}

		b.WriteString("\n")
	}

	if d.UnackedKits > 0 {
		fmt.Fprintf(&b, "Recovery kits: %d device(s) have no saved recovery kit\n\n", d.UnackedKits)
	}

	b.WriteString("Devices:\n")

	if len(d.Devices) == 0 {
		b.WriteString("  (none enrolled yet)\n")
	}

	for _, dv := range d.Devices {
		fmt.Fprintf(&b, "  - %s: %s, last snapshot %s, stored %s, %s\n", dv.Name, dv.Health, dv.LastOK, dv.Bytes, dv.Offsite)
	}

	b.WriteString("\nRecent jobs:\n")

	for _, r := range d.JobResults {
		if r.Since != "" {
			fmt.Fprintf(&b, "  - %s: %s (%s)\n", r.Kind, r.Status, r.Since)
		} else {
			fmt.Fprintf(&b, "  - %s: %s\n", r.Kind, r.Status)
		}
	}

	return b.String()
}

// buildDigest reads everything the digest reports on as of now. now is a
// parameter, not time.Now() inline, so a test can pin the relative times
// ("2 h ago") it asserts on.
func buildDigest(ctx context.Context, st *store.Store, now time.Time) (*digestData, error) {
	agents, err := st.Agents(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing agents: %w", err)
	}

	latest, err := st.LatestReports(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading reports: %w", err)
	}

	lastOK, err := st.LastOKReports(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading reports: %w", err)
	}

	stats, err := st.RepoStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading repo stats: %w", err)
	}

	kitAcks, err := st.KitAcks(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading kit acknowledgements: %w", err)
	}

	fleetName, err := st.Setting(ctx, fleetNameSetting)
	if err != nil {
		return nil, fmt.Errorf("reading settings: %w", err)
	}

	pub, err := st.Setting(ctx, publicURLSetting)
	if err != nil {
		return nil, fmt.Errorf("reading settings: %w", err)
	}

	d := &digestData{FleetName: fleetName, PublicURL: pub, GeneratedAt: now.UTC().Format("2006-01-02 15:04 MST")}

	var totalStored, totalLogical int64
	for _, r := range stats {
		totalStored += r.StoredBytes
		totalLogical += r.LogicalBytes
	}

	d.TotalBytes = units.BytesString(totalStored)
	if totalStored > 0 {
		d.DedupRatio = fmt.Sprintf("%.2fx", float64(totalLogical)/float64(totalStored))
	}

	mirrorEvery := MirrorInterval(ctx, st)

	// agents keeps its enrolled_at order, same as the overview.
	for _, a := range agents {
		if a.RevokedAt != nil {
			continue
		}

		if _, acked := kitAcks[a.ID]; !acked {
			d.UnackedKits++
		}

		var lr *store.Report
		if x, ok := latest[a.ID]; ok {
			lr = &x
		}

		d.Devices = append(d.Devices, digestDeviceFor(a, lr, lastOK, stats, mirrorEvery, now))
	}

	for _, kind := range digestJobKinds {
		result, failing, err := digestKindResult(ctx, st, kind, now)
		if err != nil {
			return nil, err
		}

		d.JobResults = append(d.JobResults, result)

		if failing {
			d.Failing = append(d.Failing, kind)
		}
	}

	return d, nil
}

func digestDeviceFor(a store.Agent, lr *store.Report, lastOK map[string]time.Time, stats map[string]store.RepoStat, mirrorEvery time.Duration, now time.Time) digestDevice {
	var okAt *time.Time

	lastOKStr := "never"
	if t, ok := lastOK[a.ID]; ok {
		okAt = &t
		lastOKStr = agoString(now.Sub(t))
	}

	in := health.Input{LastOK: okAt}
	if lr != nil {
		in.LastRunFailed = lr.Status == "error"
	}

	rs, hasStats := stats[a.ID]

	bytesStr := units.BytesString(int64(0))

	offsite := "never mirrored"

	if hasStats {
		bytesStr = units.BytesString(rs.StoredBytes)

		if rs.MirroredAt != nil {
			age := now.Sub(*rs.MirroredAt)
			if MirrorStale(rs.MirroredAt, now, mirrorEvery) {
				offsite = "stale, last offsite " + agoString(age)
			} else {
				offsite = "offsite " + agoString(age)
			}
		}
	}

	return digestDevice{
		Name: a.Name, Health: health.Status(in, now),
		LastOK: lastOKStr, Bytes: bytesStr, Offsite: offsite,
	}
}

// digestKindResult is one job kind's most recent outcome, plus whether it
// has been failing for at least failingFor: every run since, and including,
// the most recent one has errored, and the oldest of that unbroken run is
// old enough.
func digestKindResult(ctx context.Context, st *store.Store, kind string, now time.Time) (digestJobResult, bool, error) {
	recent, err := st.RecentJobs(ctx, kind, failingLookback)
	if err != nil {
		return digestJobResult{}, false, fmt.Errorf("reading %s jobs: %w", kind, err)
	}

	if len(recent) == 0 {
		return digestJobResult{Kind: kind, Status: "never run"}, false, nil
	}

	last := recent[0]

	since := "in progress"
	if last.FinishedAt != nil {
		since = agoString(now.Sub(*last.FinishedAt))
	}

	result := digestJobResult{Kind: kind, Status: last.Status, Since: since}

	if last.Status != "error" {
		return result, false, nil
	}

	oldest := last
	for _, j := range recent {
		if j.Status != "error" {
			break
		}

		oldest = j
	}

	return result, now.Sub(oldest.ScheduledFor) >= failingFor, nil
}

// agoString renders an age the way the digest reads it: coarser than the
// dashboard's, since a week-old email never needs minute precision.
func agoString(d time.Duration) string {
	switch {
	case d < time.Hour:
		return "under an hour ago"
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
}
