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

	require.Equal(t, iv.def, s.intervalFor(ctx, iv), "unset means the default")

	require.NoError(t, st.SetSetting(ctx, iv.setting, "not a number"))
	require.Equal(t, iv.def, s.intervalFor(ctx, iv))

	require.NoError(t, st.SetSetting(ctx, iv.setting, "1"))
	require.Equal(t, iv.min, s.intervalFor(ctx, iv), "a fat-fingered interval is floored")

	require.NoError(t, st.SetSetting(ctx, iv.setting, strconv.Itoa(int((2*time.Hour).Seconds()))))
	require.Equal(t, 2*time.Hour, s.intervalFor(ctx, iv))
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
