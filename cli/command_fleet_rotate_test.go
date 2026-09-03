package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/tests/testenv"
)

// `fleet rotate-passphrase` is the offline form of the API call: it runs
// against the state directory with the server stopped.
func TestFleetRotatePassphraseCLI(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	keyFile := filepath.Join(fleet.StateDirFor(filepath.Join(e.ConfigDir, ".kopia.config")), "seal.key")

	e.RunAndExpectSuccess(t, "fleet", "activate",
		"--email", "hody@hody.dev", "--admin-password", "pw12345678", "--passphrase", "seal-me-please")

	before, err := os.ReadFile(keyFile)
	require.NoError(t, err)

	out := e.RunAndExpectSuccess(t, "fleet", "rotate-passphrase",
		"--passphrase", "seal-me-please", "--new-passphrase", "a-much-longer-passphrase", "--dry-run")
	require.Contains(t, strings.Join(out, "\n"), "Dry run: nothing was written.")

	after, err := os.ReadFile(keyFile)
	require.NoError(t, err)
	require.Equal(t, before, after, "a dry run must not touch seal.key")

	// A wrong current passphrase fails without writing anything.
	e.RunAndExpectFailure(t, "fleet", "rotate-passphrase",
		"--passphrase", "not-the-passphrase", "--new-passphrase", "a-much-longer-passphrase")

	after, err = os.ReadFile(keyFile)
	require.NoError(t, err)
	require.Equal(t, before, after)

	e.RunAndExpectSuccess(t, "fleet", "rotate-passphrase",
		"--passphrase", "seal-me-please", "--new-passphrase", "a-much-longer-passphrase")

	after, err = os.ReadFile(keyFile)
	require.NoError(t, err)
	require.NotEqual(t, before, after, "seal.key must hold the new key")
	require.NoFileExists(t, keyFile+".new")

	// The old passphrase is dead; the new one rotates again.
	e.RunAndExpectFailure(t, "fleet", "rotate-passphrase",
		"--passphrase", "seal-me-please", "--new-passphrase", "yet-another-long-passphrase")
	e.RunAndExpectSuccess(t, "fleet", "rotate-passphrase",
		"--passphrase", "a-much-longer-passphrase", "--new-passphrase", "yet-another-long-passphrase")
}

// The offline rotation must refuse while a Fleet server is running: the server
// holds the state directory's lock, and rotating underneath it would leave the
// store sealed under two keys.
func TestFleetRotatePassphraseRefusesWhileTheStateDirIsLocked(t *testing.T) {
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)

	stateDir := fleet.StateDirFor(filepath.Join(e.ConfigDir, ".kopia.config"))
	keyFile := filepath.Join(stateDir, "seal.key")

	e.RunAndExpectSuccess(t, "fleet", "activate",
		"--email", "hody@hody.dev", "--admin-password", "pw12345678", "--passphrase", "seal-me-please")

	before, err := os.ReadFile(keyFile)
	require.NoError(t, err)

	// Stands in for the running server, which holds the same lock.
	lock, err := fleet.TryLock(stateDir)
	require.NoError(t, err)

	_, stderr := e.RunAndExpectFailure(t, "fleet", "rotate-passphrase",
		"--passphrase", "seal-me-please", "--new-passphrase", "a-much-longer-passphrase")
	require.Contains(t, strings.Join(stderr, "\n"), "the Fleet server is running",
		"the refusal must say what to do about it")

	after, err := os.ReadFile(keyFile)
	require.NoError(t, err)
	require.Equal(t, before, after, "a refused rotation must not touch seal.key")

	// Released: the same command then works, which is what proves the refusal
	// was the lock and not the passphrase.
	require.NoError(t, lock.Unlock())
	e.RunAndExpectSuccess(t, "fleet", "rotate-passphrase",
		"--passphrase", "seal-me-please", "--new-passphrase", "a-much-longer-passphrase")
}
