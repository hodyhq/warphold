// Package jobs runs the Fleet's scheduled work: one goroutine claims due rows
// from the jobs table, runs the Runner registered for the row's kind, and
// records the outcome (spec §10).
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/kopia/kopia/fleet/store"
)

// Runner executes one job. The detail it returns is recorded on the row
// whether or not it also returns an error, so a partial run can explain itself.
type Runner func(ctx context.Context, j store.Job) (detail string, err error)

// DefaultTimeout bounds a single job run. It doubles as the staleness
// threshold: a row still 'running' after this long was left behind by a crash,
// because the scheduler is one goroutine in one process and never abandons a
// claim otherwise.
const DefaultTimeout = 6 * time.Hour

// DefaultTick is how often the scheduler looks for due work (spec §10).
const DefaultTick = time.Minute

// interval describes a kind the scheduler keeps enqueued itself, so a fleet
// needs no cron. Other kinds are enqueued by whatever triggers them.
type interval struct {
	setting string
	def     time.Duration
	min     time.Duration
}

// intervals: the setting holds seconds. The floor keeps a fat-fingered "1" from
// turning an hourly mirror into a busy loop against the provider.
var intervals = map[string]interval{
	"mirror": {setting: "mirror_interval", def: time.Hour, min: 5 * time.Minute},
}

// Scheduler runs due jobs, one at a time.
//
// ponytail: one job at a time, one goroutine; parallelise per-kind only if a
// real fleet makes it slow.
type Scheduler struct {
	st      *store.Store
	runners map[string]Runner
	tick    time.Duration

	// Timeout bounds one job run and is the staleness threshold for a claim.
	// Timeouts overrides it per kind. Set both before Start: the scheduler
	// goroutine reads them for every job it runs.
	Timeout  time.Duration
	Timeouts map[string]time.Duration

	// Now is the scheduler clock; nil means time.Now.
	Now func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewScheduler returns a scheduler for the runners, keyed by job kind. A
// non-positive tick means DefaultTick.
func NewScheduler(st *store.Store, runners map[string]Runner, tick time.Duration) *Scheduler {
	if tick <= 0 {
		tick = DefaultTick
	}

	return &Scheduler{st: st, runners: runners, tick: tick, Timeout: DefaultTimeout}
}

// Start runs the scheduler in one goroutine until ctx is cancelled or Stop is
// called. Calling it on a running scheduler is a no-op.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.cancel, s.done = cancel, done

	go func() {
		defer close(done)

		s.run(ctx)
	}()
}

// Stop cancels the scheduler and waits for the running job to return. It is
// idempotent and safe on a scheduler that was never started.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()

	if cancel == nil {
		return
	}

	cancel()
	<-done
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}

	return time.Now()
}

// timeout is the per-kind run bound.
func (s *Scheduler) timeout(kind string) time.Duration {
	if d, ok := s.Timeouts[kind]; ok && d > 0 {
		return d
	}

	if s.Timeout > 0 {
		return s.Timeout
	}

	return DefaultTimeout
}

func (s *Scheduler) run(ctx context.Context) {
	s.requeueStale(ctx)

	t := time.NewTicker(s.tick)
	defer t.Stop()

	for {
		s.enqueueIntervals(ctx)

		for ctx.Err() == nil && s.runOne(ctx) {
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// requeueStale returns claims left behind by a crash to the pending pool.
func (s *Scheduler) requeueStale(ctx context.Context) {
	for kind := range s.runners {
		n, err := s.st.RequeueStaleJobs(ctx, kind, s.now().Add(-s.timeout(kind)))
		if err != nil {
			log.Printf("warphold fleet: requeueing stale %s jobs: %v", kind, err)

			continue
		}

		if n > 0 {
			log.Printf("warphold fleet: requeued %d stale %s job(s)", n, kind)
		}
	}
}

// enqueueIntervals keeps the interval-driven kinds scheduled: one pending or
// running job at a time, and a new one only once the last finished long enough
// ago. This is why the fleet needs no cron.
func (s *Scheduler) enqueueIntervals(ctx context.Context) {
	now := s.now()

	for kind, iv := range intervals {
		if _, ok := s.runners[kind]; !ok {
			continue
		}

		every := intervalFor(ctx, s.st, iv)

		recent, err := s.st.RecentJobs(ctx, kind, 1)
		if err != nil {
			log.Printf("warphold fleet: reading recent %s jobs: %v", kind, err)

			continue
		}

		if len(recent) > 0 {
			last := recent[0]
			if last.Status == "pending" || last.Status == "running" {
				continue
			}

			if last.FinishedAt != nil && now.Sub(*last.FinishedAt) < every {
				continue
			}
		}

		if _, err := s.st.EnqueueJob(ctx, &store.Job{Kind: kind, ScheduledFor: now}); err != nil {
			log.Printf("warphold fleet: enqueueing a %s job: %v", kind, err)
		}
	}
}

// MirrorInterval is the gap between mirror runs the scheduler is currently
// using, clamped the same way. The API reads it so a device's offsite copy is
// called stale by the scheduler's clock rather than a second hard-coded one.
func MirrorInterval(ctx context.Context, st *store.Store) time.Duration {
	return intervalFor(ctx, st, intervals["mirror"])
}

func intervalFor(ctx context.Context, st *store.Store, iv interval) time.Duration {
	v, err := st.Setting(ctx, iv.setting)
	if err != nil || v == "" {
		return iv.def
	}

	secs, err := strconv.Atoi(v)
	if err != nil {
		return iv.def
	}

	if d := time.Duration(secs) * time.Second; d >= iv.min {
		return d
	}

	return iv.min
}

// runOne claims and runs a single due job, reporting whether it found one.
func (s *Scheduler) runOne(ctx context.Context) bool {
	j, err := s.st.ClaimDueJob(ctx, s.now())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
			log.Printf("warphold fleet: claiming a job: %v", err)
		}

		return false
	}

	detail, runErr := s.exec(ctx, *j)

	status := "ok"

	if runErr != nil {
		status = "error"
		// A runner that returned a detail has already said what happened, in
		// the shape it wants the UI to show; the error only sets the status.
		if detail == "" {
			detail = runErr.Error()
		}
	}

	// WithoutCancel: a job that ran to its end during shutdown still owes the
	// row an outcome, or it would look stale and be re-run.
	if err := s.st.FinishJob(context.WithoutCancel(ctx), j.ID, s.now(), status, detail); err != nil {
		log.Printf("warphold fleet: recording job %d (%s): %v", j.ID, j.Kind, err)
	}

	return true
}

// exec runs the kind's runner under a timeout, turning a panic into a failed
// job rather than a dead scheduler.
func (s *Scheduler) exec(ctx context.Context, j store.Job) (detail string, err error) {
	r, ok := s.runners[j.Kind]
	if !ok {
		return "", errors.New("no runner for kind " + j.Kind)
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout(j.Kind))
	defer cancel()

	defer func() {
		if p := recover(); p != nil {
			// The stack goes to the log, not to the row: the row is shown in
			// the UI and a stack there is noise, not information.
			log.Printf("warphold fleet: job %d (%s) panicked: %v\n%s", j.ID, j.Kind, p, debug.Stack())

			detail, err = "", fmt.Errorf("panic: %v", p)
		}
	}()

	return r(ctx, j)
}
