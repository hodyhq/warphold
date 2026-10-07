package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMirrorHidesWindow(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	week := now.Add(-7 * 24 * time.Hour)

	// An old row kept (pruning bound further back), then a recent one.
	require.NoError(t, s.AddMirrorHides(ctx, 2, "dev1", now.Add(-8*24*time.Hour), now.AddDate(0, -1, 0), 5))
	require.NoError(t, s.AddMirrorHides(ctx, 2, "dev1", now, now.AddDate(0, -1, 0), 3))
	require.NoError(t, s.AddMirrorHides(ctx, 2, "dev2", now, now.AddDate(0, -1, 0), 7))
	require.NoError(t, s.AddMirrorHides(ctx, 3, "dev1", now, now.AddDate(0, -1, 0), 11))

	n, err := s.MirrorHidesSince(ctx, 2, "dev1", week)
	require.NoError(t, err)
	require.Equal(t, 3, n, "only the window, only this target and device")

	n, err = s.MirrorHidesSince(ctx, 2, "dev1", time.Time{})
	require.NoError(t, err)
	require.Equal(t, 8, n)

	// The next write prunes everything before the window.
	require.NoError(t, s.AddMirrorHides(ctx, 2, "dev1", now, week, 1))

	n, err = s.MirrorHidesSince(ctx, 2, "dev1", time.Time{})
	require.NoError(t, err)
	require.Equal(t, 4, n)
}
