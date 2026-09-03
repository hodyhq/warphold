package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/store"
)

func openTemp(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() }) //nolint:errcheck // test cleanup

	return s
}

// eventually polls until f holds; every scheduler assertion is about work that
// happens on the scheduler's own goroutine.
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	require.Eventually(t, f, 10*time.Second, 2*time.Millisecond)
}

func jobsOf(t *testing.T, s *store.Store, kind string) []store.Job {
	t.Helper()

	js, err := s.RecentJobs(context.Background(), kind, 50)
	require.NoError(t, err)

	return js
}

func TestSchedulerRunsDueJobs(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	var ran atomic.Int64

	s := NewScheduler(st, map[string]Runner{"stats": func(context.Context, store.Job) (string, error) {
		ran.Add(1)

		return "counted", nil
	}}, time.Millisecond)

	for range 3 {
		_, err := st.EnqueueJob(ctx, &store.Job{Kind: "stats", ScheduledFor: now})
		require.NoError(t, err)
	}

	// One job is not due yet and must be left alone.
	_, err := st.EnqueueJob(ctx, &store.Job{Kind: "stats", ScheduledFor: now.Add(time.Hour)})
	require.NoError(t, err)

	s.Start(ctx)
	defer s.Stop()

	eventually(t, func() bool { return ran.Load() == 3 })

	done, pending := 0, 0

	for _, j := range jobsOf(t, st, "stats") {
		switch j.Status {
		case "ok":
			done++

			require.Equal(t, "counted", j.Detail)
			require.NotNil(t, j.StartedAt)
			require.NotNil(t, j.FinishedAt)
		case "pending":
			pending++
		}
	}

	require.Equal(t, 3, done)
	require.Equal(t, 1, pending)
	require.EqualValues(t, 3, ran.Load(), "a finished job is never claimed twice")
}

func TestSchedulerRecordsAFailingRunner(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	s := NewScheduler(st, map[string]Runner{"stats": func(context.Context, store.Job) (string, error) {
		return "partial work", errors.New("provider said no")
	}}, time.Millisecond)

	_, err := st.EnqueueJob(ctx, &store.Job{Kind: "stats", ScheduledFor: time.Now()})
	require.NoError(t, err)

	s.Start(ctx)
	defer s.Stop()

	eventually(t, func() bool { return jobsOf(t, st, "stats")[0].Status == "error" })

	// The runner's own detail is kept: it is what the UI shows.
	require.Equal(t, "partial work", jobsOf(t, st, "stats")[0].Detail)
}

func TestSchedulerRecordsAnUnknownKind(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	s := NewScheduler(st, map[string]Runner{"stats": func(context.Context, store.Job) (string, error) {
		return "", nil
	}}, time.Millisecond)

	_, err := st.EnqueueJob(ctx, &store.Job{Kind: "nonesuch", ScheduledFor: time.Now()})
	require.NoError(t, err)

	s.Start(ctx)
	defer s.Stop()

	eventually(t, func() bool { return jobsOf(t, st, "nonesuch")[0].Status == "error" })
	require.Contains(t, jobsOf(t, st, "nonesuch")[0].Detail, "no runner for kind nonesuch")
}

func TestSchedulerRecoversAPanicAndKeepsRunning(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	var ok atomic.Bool

	s := NewScheduler(st, map[string]Runner{
		"boom": func(context.Context, store.Job) (string, error) { panic("the mirror exploded") },
		"fine": func(context.Context, store.Job) (string, error) { ok.Store(true); return "fine", nil },
	}, time.Millisecond)

	_, err := st.EnqueueJob(ctx, &store.Job{Kind: "boom", ScheduledFor: now})
	require.NoError(t, err)
	_, err = st.EnqueueJob(ctx, &store.Job{Kind: "fine", ScheduledFor: now.Add(time.Millisecond)})
	require.NoError(t, err)

	s.Start(ctx)
	defer s.Stop()

	eventually(t, func() bool { return ok.Load() })

	boom := jobsOf(t, st, "boom")[0]
	require.Equal(t, "error", boom.Status)
	require.Contains(t, boom.Detail, "panic: the mirror exploded")

	require.Equal(t, "ok", jobsOf(t, st, "fine")[0].Status)
}

func TestSchedulerBoundsARunWithTheTimeout(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	released := make(chan struct{})

	s := NewScheduler(st, map[string]Runner{"slow": func(ctx context.Context, _ store.Job) (string, error) {
		<-ctx.Done()
		close(released)

		return "", ctx.Err()
	}}, time.Millisecond)
	s.Timeouts = map[string]time.Duration{"slow": 20 * time.Millisecond}

	_, err := st.EnqueueJob(ctx, &store.Job{Kind: "slow", ScheduledFor: time.Now()})
	require.NoError(t, err)

	s.Start(ctx)
	defer s.Stop()

	select {
	case <-released:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner's context was never cancelled")
	}

	eventually(t, func() bool { return jobsOf(t, st, "slow")[0].Status == "error" })
	require.Contains(t, jobsOf(t, st, "slow")[0].Detail, "context deadline exceeded")
}

func TestSchedulerStopsCleanly(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	entered := make(chan struct{})

	var finished atomic.Bool

	s := NewScheduler(st, map[string]Runner{"slow": func(ctx context.Context, _ store.Job) (string, error) {
		close(entered)
		<-ctx.Done()
		finished.Store(true)

		return "stopped mid-run", nil
	}}, time.Millisecond)

	_, err := st.EnqueueJob(ctx, &store.Job{Kind: "slow", ScheduledFor: time.Now()})
	require.NoError(t, err)

	s.Start(ctx)
	s.Start(ctx) // idempotent
	<-entered
	s.Stop()
	s.Stop() // idempotent

	require.True(t, finished.Load(), "Stop waits for the running job")

	// The outcome is still recorded: the store is written with a context that
	// survives the shutdown, so the row cannot be left looking stale.
	require.Equal(t, "ok", jobsOf(t, st, "slow")[0].Status)
	require.Equal(t, "stopped mid-run", jobsOf(t, st, "slow")[0].Detail)

	NewScheduler(st, nil, 0).Stop() // safe on a scheduler that never started
}

// TestSchedulerStopReturnsWhenARunnerIgnoresCtx covers the bounded wait: a
// runner that never notices ctx is done must not hang Stop forever.
func TestSchedulerStopReturnsWhenARunnerIgnoresCtx(t *testing.T) {
	old := stopWait
	stopWait = 10 * time.Millisecond
	t.Cleanup(func() { stopWait = old })

	st := openTemp(t)
	ctx := context.Background()

	entered := make(chan struct{})

	s := NewScheduler(st, map[string]Runner{"slow": func(context.Context, store.Job) (string, error) {
		close(entered)
		select {} // never returns, the way a stuck syscall would
	}}, time.Millisecond)

	_, err := st.EnqueueJob(ctx, &store.Job{Kind: "slow", ScheduledFor: time.Now()})
	require.NoError(t, err)

	s.Start(ctx)
	<-entered

	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return within its bound")
	}
}

func TestSchedulerRequeuesAStaleClaim(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	// A claim from seven hours ago, the way a crash leaves one behind.
	_, err := st.EnqueueJob(ctx, &store.Job{Kind: "stats", ScheduledFor: now.Add(-8 * time.Hour)})
	require.NoError(t, err)
	_, err = st.ClaimDueJob(ctx, now.Add(-7*time.Hour))
	require.NoError(t, err)

	var ran atomic.Int64

	s := NewScheduler(st, map[string]Runner{"stats": func(context.Context, store.Job) (string, error) {
		ran.Add(1)

		return "re-run", nil
	}}, time.Millisecond)

	s.Start(ctx)
	defer s.Stop()

	eventually(t, func() bool { return ran.Load() == 1 })
	require.Equal(t, "ok", jobsOf(t, st, "stats")[0].Status)
}

// TestSchedulerRequeuesAClaimThatGoesStaleWhileRunning covers the loop path:
// a claim that is still fresh when the scheduler starts, and only crosses the
// kind's timeout later while the scheduler keeps ticking. Before the fix,
// requeueStale ran once before the loop and never again, so this claim would
// stay 'running' forever and permanently block enqueueIntervals for its kind.
func TestSchedulerRequeuesAClaimThatGoesStaleWhileRunning(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	t0 := time.Now()

	var clock atomic.Int64
	clock.Store(t0.UnixNano())
	setClock := func(d time.Duration) { clock.Store(t0.Add(d).UnixNano()) }

	var ran atomic.Int64

	s := NewScheduler(st, map[string]Runner{"mirror": func(context.Context, store.Job) (string, error) {
		ran.Add(1)

		return "0 objects, 0 bytes, 0 skipped", nil
	}}, time.Millisecond)
	s.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	s.Timeouts = map[string]time.Duration{"mirror": 5 * time.Minute}

	_, err := st.EnqueueJob(ctx, &store.Job{Kind: "mirror", ScheduledFor: t0.Add(-time.Minute)})
	require.NoError(t, err)

	// Claimed the way a previous run of the scheduler would have: started_at
	// is t0, so it is not stale yet by the kind's 5-minute timeout.
	_, err = st.ClaimDueJob(ctx, t0)
	require.NoError(t, err)

	s.Start(ctx)
	defer s.Stop()

	// The claim is still fresh: the requeueStale call before the loop (kept
	// for the crash-at-start case) must not touch it.
	time.Sleep(20 * time.Millisecond)
	require.Zero(t, ran.Load())
	require.Equal(t, "running", jobsOf(t, st, "mirror")[0].Status)

	// Six minutes on: the claim is now older than the timeout, but only while
	// the scheduler is already running its loop. Only a requeueStale call
	// made from inside the loop - not the one-shot call before it - can
	// notice this and return the row to pending.
	setClock(6 * time.Minute)
	eventually(t, func() bool { return ran.Load() == 1 })
	require.Equal(t, "ok", jobsOf(t, st, "mirror")[0].Status)

	// With the stale claim cleared, enqueueIntervals is unstuck too: once the
	// interval has elapsed it enqueues, and the scheduler runs, a fresh
	// mirror job on its own.
	setClock(6*time.Minute + time.Hour + time.Second)
	eventually(t, func() bool { return ran.Load() == 2 })
	require.Len(t, jobsOf(t, st, "mirror"), 2)
}

func TestSchedulerEnqueuesTheMirrorItself(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	var ran atomic.Int64

	s := NewScheduler(st, map[string]Runner{"mirror": func(context.Context, store.Job) (string, error) {
		ran.Add(1)

		return "0 objects, 0 bytes, 0 skipped", nil
	}}, time.Millisecond)

	s.Start(ctx)
	eventually(t, func() bool { return ran.Load() == 1 })

	// Many ticks later there is still exactly one: the interval has not
	// elapsed, so the scheduler does not pile up a second.
	time.Sleep(50 * time.Millisecond)
	s.Stop()

	require.EqualValues(t, 1, ran.Load())
	require.Len(t, jobsOf(t, st, "mirror"), 1)
}

func TestSchedulerRunsNoIntervalWithoutARunner(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	s := NewScheduler(st, map[string]Runner{"stats": func(context.Context, store.Job) (string, error) {
		return "", nil
	}}, time.Millisecond)

	s.Start(ctx)
	time.Sleep(20 * time.Millisecond)
	s.Stop()

	require.Empty(t, jobsOf(t, st, "mirror"))
}

func TestMirrorIntervalSettingIsClampedToItsFloor(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	iv := intervals["mirror"]

	s := NewScheduler(st, nil, time.Millisecond)

	require.Equal(t, iv.def, intervalFor(ctx, s.st, iv), "unset means the default")

	require.NoError(t, st.SetSetting(ctx, iv.setting, "not a number"))
	require.Equal(t, iv.def, intervalFor(ctx, s.st, iv))

	require.NoError(t, st.SetSetting(ctx, iv.setting, "1"))
	require.Equal(t, iv.min, intervalFor(ctx, s.st, iv), "a fat-fingered interval is floored")

	require.NoError(t, st.SetSetting(ctx, iv.setting, strconv.Itoa(int((2*time.Hour).Seconds()))))
	require.Equal(t, 2*time.Hour, intervalFor(ctx, s.st, iv))
}

// TestMirrorIntervalIsTheExportedSameClock: fleet/api calls staleness against
// this, so it must resolve the setting exactly as the scheduler does - the two
// drifting apart is how a device gets called stale an hour early.
func TestMirrorIntervalIsTheExportedSameClock(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	iv := intervals["mirror"]

	require.Equal(t, time.Hour, MirrorInterval(ctx, st), "unset means one hour")

	require.NoError(t, st.SetSetting(ctx, iv.setting, "1800"))
	require.Equal(t, 30*time.Minute, MirrorInterval(ctx, st), "the setting is seconds")

	require.NoError(t, st.SetSetting(ctx, iv.setting, "1"))
	require.Equal(t, 5*time.Minute, MirrorInterval(ctx, st), "floored, so staleness cannot go sub-minute")

	require.NoError(t, st.SetSetting(ctx, iv.setting, "half an hour"))
	require.Equal(t, time.Hour, MirrorInterval(ctx, st), "garbage falls back to the default")
}

func TestSchedulerTimeoutFallsBackToTheDefault(t *testing.T) {
	s := NewScheduler(nil, nil, 0)
	require.Equal(t, DefaultTick, s.tick)
	require.Equal(t, DefaultTimeout, s.timeout("mirror"))

	s.Timeout = 0
	require.Equal(t, DefaultTimeout, s.timeout("mirror"))

	s.Timeouts = map[string]time.Duration{"mirror": time.Minute}
	require.Equal(t, time.Minute, s.timeout("mirror"))
	require.Equal(t, DefaultTimeout, s.timeout("stats"))
}
