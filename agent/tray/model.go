// Package tray is the agent's Linux system-tray companion: it polls the
// locally running engine and renders its state as an icon and a menu.
package tray

import (
	"fmt"
	"time"

	"github.com/kopia/kopia/internal/serverapi"
)

// Tone selects which tinted icon the tray shows. It is also the unit the
// notification rule works in: only a transition into ToneBad notifies.
type Tone string

// The five tones. ToneDim means "no engine to talk to".
const (
	ToneGood  Tone = "good"
	ToneEmber Tone = "ember"
	ToneWarn  Tone = "warn"
	ToneBad   Tone = "bad"
	ToneDim   Tone = "dim"
)

// Kind identifies a menu item. The model always returns every kind, in this
// order, so the tray can create the items once and afterwards only relabel,
// enable and hide them.
type Kind int

// Menu items, in menu order.
const (
	KindVault Kind = iota
	KindActivity
	KindLast
	KindNext
	KindErrors
	KindSep1
	KindStartAgent
	KindBackupNow
	KindPauseResume
	KindDetails
	KindSep2
	KindQuit
)

// Item is one rendered menu entry.
type Item struct {
	Kind    Kind
	Label   string
	Enabled bool
	Hidden  bool
}

// Model is the whole tray UI for one poll: an icon tone, a tooltip and the
// menu. It deliberately carries no repository target, bucket or credential -
// the tray shows the vault name, status lines and actions, nothing else.
type Model struct {
	Tone    Tone
	Tooltip string
	Items   []Item
}

// Status is what one poll of the engine found. Running is false when there is
// no engine.json or the engine behind it does not answer.
type Status struct {
	Running        bool
	Vault          string
	Sources        []*serverapi.SourceStatus
	ErrorsThisWeek int
	Paused         bool
}

// VaultLabel renders the disabled first menu item. The group is the Fleet
// group this agent belongs to; agents enrolled before groups were reported
// have none and show their name alone.
func VaultLabel(group, name string) string {
	switch {
	case name == "" && group == "":
		return "WarpHold"
	case group == "":
		return name
	case name == "":
		return group
	default:
		return group + " · " + name
	}
}

// Build renders the menu for a poll result. It is pure: every time-dependent
// label is derived from now, so the tests do not depend on the wall clock.
func Build(s Status, now time.Time) Model {
	m := Model{Tone: tone(s), Items: []Item{
		{Kind: KindVault, Label: s.Vault},
		{Kind: KindActivity, Label: activity(s)},
		{Kind: KindLast, Label: "Last good backup: " + lastGood(s, now), Hidden: !s.Running},
		{Kind: KindNext, Label: "Next: " + next(s, now), Hidden: !s.Running},
		{Kind: KindErrors, Label: fmt.Sprintf("Errors this week: %d", s.ErrorsThisWeek), Enabled: true, Hidden: !s.Running},
		{Kind: KindSep1},
		{Kind: KindStartAgent, Label: "Start agent", Enabled: true, Hidden: s.Running},
		{Kind: KindBackupNow, Label: "Back up now", Enabled: true, Hidden: !s.Running},
		{Kind: KindPauseResume, Label: pauseLabel(s), Enabled: true, Hidden: !s.Running},
		{Kind: KindDetails, Label: "Details…", Enabled: true, Hidden: !s.Running},
		{Kind: KindSep2},
		{Kind: KindQuit, Label: "Quit tray", Enabled: true},
	}}

	m.Tooltip = s.Vault + " — " + activity(s)

	return m
}

func tone(s Status) Tone {
	if !s.Running {
		return ToneDim
	}

	for _, src := range s.Sources {
		if src.Status == "FAILED" {
			return ToneBad
		}
	}

	if uploading(s) != nil {
		return ToneEmber
	}

	if s.ErrorsThisWeek > 0 {
		return ToneWarn
	}

	return ToneGood
}

// uploading returns the first source with a snapshot in flight.
func uploading(s Status) *serverapi.SourceStatus {
	for _, src := range s.Sources {
		if src.CurrentTask != "" {
			return src
		}
	}

	return nil
}

func activity(s Status) string {
	if !s.Running {
		return "Agent not running"
	}

	if len(s.Sources) == 0 {
		return "No sources configured"
	}

	if src := uploading(s); src != nil {
		if pct, ok := percent(src); ok {
			return fmt.Sprintf("Backing up %s — %d%%", src.Source.Path, pct)
		}

		return "Backing up " + src.Source.Path
	}

	if allPaused(s) {
		return "Paused"
	}

	return "Idle"
}

// percent is progress as hashed+cached over the estimate. The estimate grows
// while the upload walks the tree, so it is capped at 99: a source that
// briefly reports more bytes done than estimated must not read "100%" and
// then keep running.
func percent(src *serverapi.SourceStatus) (int, bool) {
	c := src.UploadCounters
	if c == nil || c.EstimatedBytes <= 0 {
		return 0, false
	}

	pct := (c.TotalHashedBytes + c.TotalCachedBytes) * 100 / c.EstimatedBytes
	if pct > 99 {
		pct = 99
	}

	if pct < 0 {
		pct = 0
	}

	return int(pct), true
}

func allPaused(s Status) bool {
	if len(s.Sources) == 0 {
		return false
	}

	for _, src := range s.Sources {
		if src.Status != "PAUSED" {
			return false
		}
	}

	return true
}

func pauseLabel(s Status) string {
	if allPaused(s) {
		return "Resume"
	}

	return "Pause"
}

func lastGood(s Status, now time.Time) string {
	var newest time.Time

	for _, src := range s.Sources {
		if src.LastSnapshot == nil {
			continue
		}

		if t := src.LastSnapshot.StartTime.ToTime(); t.After(newest) {
			newest = t
		}
	}

	if newest.IsZero() {
		return "never"
	}

	return when(newest, now)
}

func next(s Status, now time.Time) string {
	var soonest time.Time

	for _, src := range s.Sources {
		if src.NextSnapshotTime == nil {
			continue
		}

		if t := *src.NextSnapshotTime; soonest.IsZero() || t.Before(soonest) {
			soonest = t
		}
	}

	if soonest.IsZero() {
		return "not scheduled"
	}

	return when(soonest, now)
}

// when renders a timestamp the way a person reads a backup schedule: a clock
// time for the neighbouring days, a date otherwise. Both times are compared
// in the local zone, so "today" means the user's today.
func when(t, now time.Time) string {
	t = t.Local()

	switch {
	case sameDay(t, now):
		return "today " + t.Format("15:04")
	case sameDay(t, now.AddDate(0, 0, 1)):
		return "tomorrow " + t.Format("15:04")
	case sameDay(t, now.AddDate(0, 0, -1)):
		return "yesterday " + t.Format("15:04")
	}

	return t.Format("Jan 2 15:04")
}

func sameDay(a, b time.Time) bool {
	y1, m1, d1 := a.Local().Date()
	y2, m2, d2 := b.Local().Date()

	return y1 == y2 && m1 == m2 && d1 == d2
}

// ToneWatcher decides when to raise a desktop notification: only on a
// transition into ToneBad, so one failing source notifies once and keeps
// notifying nothing until it recovers and fails again.
type ToneWatcher struct {
	prev Tone
	seen bool
}

// Notify records the new tone and reports whether it should notify.
func (w *ToneWatcher) Notify(next Tone) bool {
	was := w.prev
	seen := w.seen
	w.prev, w.seen = next, true

	return next == ToneBad && (!seen || was != ToneBad)
}
