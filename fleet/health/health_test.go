package health_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/health"
)

func TestStatus(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	require.Equal(t, health.Unknown, health.Status(health.Input{}, now))
	require.Equal(t, health.Red, health.Status(health.Input{LastRunFailed: true}, now))
	require.Equal(t, health.Green, health.Status(health.Input{LastOK: at(2 * time.Hour)}, now))
	require.Equal(t, health.Green, health.Status(health.Input{LastOK: at(25 * time.Hour)}, now))
	require.Equal(t, health.Yellow, health.Status(health.Input{LastOK: at(27 * time.Hour)}, now))
	require.Equal(t, health.Yellow, health.Status(health.Input{LastOK: at(6 * 24 * time.Hour)}, now))
	require.Equal(t, health.Red, health.Status(health.Input{LastOK: at(8 * 24 * time.Hour)}, now))
	require.Equal(t, health.Red, health.Status(health.Input{LastOK: at(time.Hour), LastRunFailed: true}, now))
	require.Equal(t, "revoked", health.Status(health.Input{LastOK: at(time.Hour), Revoked: true}, now))
	// A LastOK in the future (skewed agent clock) must not read as green.
	require.Equal(t, health.Unknown, health.Status(health.Input{LastOK: at(-time.Hour)}, now))
	require.Equal(t, health.Unknown, health.Status(health.Input{LastOK: at(-365 * 24 * time.Hour)}, now))
}
