package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/clock"
)

func TestReserveMirrorHides(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := clock.Now().UTC().Truncate(time.Second)
	week := now.Add(-7 * 24 * time.Hour)
	month := now.AddDate(0, -1, 0)

	reserve := func(target int64, device string, at, since time.Time, n, limit int) (int, bool) {
		t.Helper()

		earlier, ok, err := s.ReserveMirrorHides(ctx, target, device, at, since, n, limit)
		require.NoError(t, err)

		return earlier, ok
	}

	// An old row, kept while the window reaches back a month.
	_, ok := reserve(2, "dev1", now.Add(-8*24*time.Hour), month, 5, 100)
	require.True(t, ok)

	// Other targets and devices do not count against dev1 on target 2.
	_, ok = reserve(2, "dev2", now, month, 7, 100)
	require.True(t, ok)
	_, ok = reserve(3, "dev1", now, month, 11, 100)
	require.True(t, ok)

	earlier, ok := reserve(2, "dev1", now, month, 3, 100)
	require.True(t, ok)
	require.Equal(t, 5, earlier)

	// With a one-week window the old row is pruned: 3 + 7 fits in 10, then
	// one more does not, and a refused reservation records nothing.
	earlier, ok = reserve(2, "dev1", now, week, 7, 10)
	require.True(t, ok)
	require.Equal(t, 3, earlier)

	earlier, ok = reserve(2, "dev1", now, week, 1, 10)
	require.False(t, ok)
	require.Equal(t, 10, earlier)

	earlier, ok = reserve(2, "dev1", now, week, 0, 10)
	require.True(t, ok)
	require.Equal(t, 10, earlier)
}
