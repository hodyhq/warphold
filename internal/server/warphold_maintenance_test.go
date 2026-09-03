// warphold: the Options.DisableMaintenance gate, which is how the WarpHold
// agent's engine gets the effect of --no-auto-maintenance (spec §7.1 step 5).
package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/auth"
	"github.com/kopia/kopia/internal/passwordpersist"
	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/internal/testutil"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/maintenance"
)

func TestDisableMaintenanceStopsTheMaintenanceManager(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "enabled", true: "disabled"}[disabled], func(t *testing.T) {
			ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)

			// Own maintenance, so nothing but the flag can be the reason the
			// manager does not start.
			require.NoError(t, repo.DirectWriteSession(ctx, env.RepositoryWriter, repo.WriteSessionOptions{}, func(ctx context.Context, dw repo.DirectRepositoryWriter) error {
				return maintenance.SetParams(ctx, dw, &maintenance.Params{
					Owner:      env.Repository.ClientOptions().UsernameAtHost(),
					QuickCycle: maintenance.CycleParams{Enabled: true, Interval: time.Hour},
				})
			}))

			s, err := New(testlogging.Context(t), &Options{
				ConfigFile:        env.ConfigFile(),
				PasswordPersist:   passwordpersist.None(),
				Authorizer:        auth.DefaultAuthorizer(),
				Authenticator:     auth.AuthenticateSingleUser("u", "p"),
				RefreshInterval:   time.Hour,
				UIPreferencesFile: filepath.Join(testutil.TempDirectory(t), "ui-pref.json"),

				DisableMaintenance: disabled,
			})
			require.NoError(t, err)

			require.NoError(t, s.SetRepository(ctx, env.RepositoryWriter))
			t.Cleanup(func() { s.SetRepository(ctx, nil) }) //nolint:errcheck

			if disabled {
				require.Nil(t, s.maint, "the maintenance manager must not run on a WarpHold device")
			} else {
				require.NotNil(t, s.maint, "without the flag this test proves nothing")
			}
		})
	}
}
