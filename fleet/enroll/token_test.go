package enroll_test

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/fleet/store"
)

func TestTokenLifecycle(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	require.NoError(t, err)
	defer st.Close()
	ctx := context.Background()
	tk := enroll.NewTokens(st)
	now := time.Now()
	tk.SetNowForTesting(func() time.Time { return now })

	// enrollment_tokens.group_id is a foreign key, so a real group must exist first.
	tid, err := st.CreateTarget(ctx, &store.Target{Name: "local", Kind: "filesystem", Path: t.TempDir()})
	require.NoError(t, err)
	tpl, err := st.CreateTemplate(ctx, &store.Template{Name: "Home default", Sources: []string{"~"}})
	require.NoError(t, err)
	gid, err := st.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl})
	require.NoError(t, err)

	plain, tok, err := tk.Issue(ctx, gid, 0, -1, 7)
	require.NoError(t, err)
	require.True(t, len(plain) > 20 && plain[:3] == "wh_")
	require.Equal(t, 1, tok.MaxUses)
	require.WithinDuration(t, now.Add(enroll.DefaultTTL), tok.ExpiresAt, time.Second)

	got, err := tk.Consume(ctx, plain)
	require.NoError(t, err)
	require.Equal(t, tok.ID, got.ID)
	_, err = tk.Consume(ctx, plain)
	require.ErrorIs(t, err, enroll.ErrTokenInvalid, "single use")

	_, err = tk.Consume(ctx, "wh_nope")
	require.ErrorIs(t, err, enroll.ErrTokenInvalid)

	_, _, err = tk.Issue(ctx, gid, 31*24*time.Hour, 1, 7)
	require.ErrorIs(t, err, enroll.ErrTTLTooLong)

	multi, _, err := tk.Issue(ctx, gid, 2*time.Hour, 0, 7)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		_, err = tk.Consume(ctx, multi)
		require.NoError(t, err, "unlimited uses")
	}
	now = now.Add(3 * time.Hour)
	_, err = tk.Consume(ctx, multi)
	require.ErrorIs(t, err, enroll.ErrTokenInvalid, "expired")
}

// TestConsumeIsAtomic pins I3: fleet/api builds a fresh enroll.Tokens per
// request, so a mutex on the struct guards nothing and a read-check-increment
// would let two concurrent enrollments both spend a single-use token. The
// conditional UPDATE in store.ConsumeToken makes exactly one win.
func TestConsumeIsAtomic(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	require.NoError(t, err)
	defer st.Close()

	ctx := context.Background()
	tid, err := st.CreateTarget(ctx, &store.Target{Name: "local", Kind: "filesystem", Path: t.TempDir()})
	require.NoError(t, err)
	tpl, err := st.CreateTemplate(ctx, &store.Template{Name: "Home default", Sources: []string{"~"}})
	require.NoError(t, err)
	gid, err := st.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl})
	require.NoError(t, err)

	// consume plain from n goroutines, each with its own Tokens (as the API
	// does per request), and count the successes.
	race := func(plain string, n int) int {
		var wg sync.WaitGroup
		var okCount int64
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := enroll.NewTokens(st).Consume(ctx, plain); err == nil {
					atomic.AddInt64(&okCount, 1)
				}
			}()
		}
		close(start)
		wg.Wait()
		return int(okCount)
	}

	single, _, err := enroll.NewTokens(st).Issue(ctx, gid, time.Hour, 1, 7)
	require.NoError(t, err)
	require.Equal(t, 1, race(single, 10), "max_uses=1 token can only be spent once")

	unlimited, _, err := enroll.NewTokens(st).Issue(ctx, gid, time.Hour, 0, 7)
	require.NoError(t, err)
	require.Equal(t, 10, race(unlimited, 10), "max_uses=0 means unlimited")
}
