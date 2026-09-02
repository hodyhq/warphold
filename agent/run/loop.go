// Package run is the agent's main loop: poll Fleet, apply policy, report tasks.
package run

import (
	"context"
	"errors"
	"regexp"
	"runtime"
	"strconv"
	"time"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/uitask"
	"github.com/kopia/kopia/repo"
)

// LocalEngine is what the loop needs from engine.Local (interface so tests can fake it).
type LocalEngine interface {
	Apply(ctx context.Context, sources []poll.Source) error
	Snapshot(ctx context.Context, path string) error
	Pause(ctx context.Context, path string) error
	Resume(ctx context.Context, path string) error
	Tasks(ctx context.Context) ([]uitask.Info, error)
	TaskLog(ctx context.Context, id string) (string, error)
	Status(ctx context.Context) (engineStatus string, repoConnected bool)
}

// Deps wires the loop.
type Deps struct {
	Fleet *poll.Client
	Local LocalEngine
	State *state.Config
	Now   func() time.Time
	Log   func(format string, args ...any)
}

// Loop is the agent's control loop: poll Fleet for policy and commands, apply
// them locally, and watch the local task list for finished snapshots to
// report back.
type Loop struct {
	d    Deps
	seen map[string]bool
}

// New creates a Loop.
func New(d Deps) *Loop {
	if d.Now == nil {
		d.Now = time.Now
	}

	if d.Log == nil {
		d.Log = func(string, ...any) {}
	}

	return &Loop{d: d, seen: map[string]bool{}}
}

func (l *Loop) heartbeat(ctx context.Context) poll.Heartbeat {
	status, connected := l.d.Local.Status(ctx)
	return poll.Heartbeat{Version: repo.BuildVersion, OS: runtime.GOOS, Arch: runtime.GOARCH, RepoConnected: connected, EngineStatus: status}
}

// PollOnce does one poll cycle: heartbeat, apply policy on a new ETag, run
// any pending commands, and report their results.
func (l *Loop) PollOnce(ctx context.Context) error {
	doc, err := l.d.Fleet.Poll(ctx, l.heartbeat(ctx), l.d.State.ETag)
	if err != nil {
		return err
	}

	if doc == nil {
		return nil
	}

	if doc.ETag != l.d.State.ETag {
		if err := l.d.Local.Apply(ctx, doc.Sources); err != nil {
			return err
		}

		l.d.State.ETag = doc.ETag
		if doc.PollIntervalSeconds > 0 {
			l.d.State.PollInterval = doc.PollIntervalSeconds
		}

		if err := state.Save(l.d.State.Scope, l.d.State); err != nil {
			l.d.Log("cannot save state: %v", err)
		}
	}

	for _, c := range doc.Commands {
		started := l.d.Now()

		var cerr error

		switch c.Kind {
		case "snapshot-now":
			cerr = l.d.Local.Snapshot(ctx, c.Source)
		case "pause":
			cerr = l.d.Local.Pause(ctx, c.Source)
		case "resume":
			cerr = l.d.Local.Resume(ctx, c.Source)
		case "verify":
			cerr = errors.New("verify runs from the Fleet server in this version") // Plan 3 (M7)
		default:
			cerr = errors.New("unknown command " + c.Kind)
		}

		rep := poll.Report{TaskID: "cmd-" + strconv.FormatInt(c.ID, 10), Kind: "command", CommandID: c.ID, Source: c.Source, StartedAt: started, FinishedAt: l.d.Now(), Status: "ok"}
		if cerr != nil {
			rep.Status, rep.Stderr = "error", cerr.Error()
		}

		if err := l.d.Fleet.Report(ctx, rep); err != nil {
			return err
		}
	}

	return nil
}

// descPath extracts the source path from a Snapshot task's Description. The
// real format, from internal/server.runSnapshotTask, is
// fmt.Sprintf("%v at %v", src, time) where snapshot.SourceInfo.String()
// renders "user@host:path" (snapshot/source.go) - e.g.
// "hody@fw13:/home/hody at 2026-09-01T23:00:00Z". The "Snapshot " prefix and
// " at <timestamp>" suffix are both optional so older/synthetic descriptions
// still match.
// ponytail: Linux-only heuristic (host has no ':'); Windows paths with a
// drive-letter colon would need a smarter split if agents ever run there.
var descPath = regexp.MustCompile(`^(?:Snapshot )?[^@\s]+@[^:\s]+:(.+?)(?: at \S+)?$`)

// WatchOnce reports every finished task not yet reported, then remembers it.
func (l *Loop) WatchOnce(ctx context.Context) error {
	tasks, err := l.d.Local.Tasks(ctx)
	if err != nil {
		return err
	}

	for _, t := range tasks {
		if t.EndTime == nil || l.seen[t.TaskID] {
			continue
		}

		source := ""
		if m := descPath.FindStringSubmatch(t.Description); m != nil {
			source = m[1]
		}

		rep := engine.ToReport(t, source)
		if rep.Status == "error" && rep.Stderr == "" {
			rep.Stderr, _ = l.d.Local.TaskLog(ctx, t.TaskID)
		}

		if err := l.d.Fleet.Report(ctx, rep); err != nil {
			return err
		}

		l.seen[t.TaskID] = true
	}

	return nil
}

// Run loops until ctx ends or the agent is revoked. once means one poll plus
// one watch, then return.
func (l *Loop) Run(ctx context.Context, once bool) error {
	if once {
		if err := l.PollOnce(ctx); err != nil {
			return err
		}

		return l.WatchOnce(ctx)
	}

	interval := time.Duration(l.d.State.PollInterval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Minute
	}

	pollT := time.NewTimer(0)
	watchT := time.NewTicker(10 * time.Second)

	defer pollT.Stop()
	defer watchT.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pollT.C:
			if err := l.PollOnce(ctx); err != nil {
				if errors.Is(err, poll.ErrRevoked) {
					return err
				}

				l.d.Log("poll: %v", err)
			}

			interval = time.Duration(l.d.State.PollInterval) * time.Second
			pollT.Reset(poll.Jitter(interval))
		case <-watchT.C:
			if err := l.WatchOnce(ctx); err != nil {
				if errors.Is(err, poll.ErrRevoked) {
					return err
				}

				l.d.Log("watch: %v", err)
			}
		}
	}
}
