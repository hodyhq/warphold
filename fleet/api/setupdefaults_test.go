package api_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/api"
)

// TestSetupDefaultsLeavesAFleetThatCanEnroll is the brief's acceptance shape: a
// fresh install ends up with a target, a template, a group and a token that can
// be issued right away, without anyone opening the dashboard.
func TestSetupDefaultsLeavesAFleetThatCanEnroll(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	hostedRoot := filepath.Join(h.hostedDir(t), "hosted")

	oneLiner, token, err := h.s.SetupDefaults(t.Context(), h.srv.URL, "disk", hostedRoot)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(token, "wh_"), "a real token is returned")
	require.NotContains(t, oneLiner, token, "the command itself never carries the token")
	require.Contains(t, oneLiner, h.srv.URL+"/enroll.sh")

	fi, err := os.Stat(hostedRoot)
	require.NoError(t, err)

	if runtime.GOOS != "windows" {
		// Windows has no POSIX permission bits, so this only holds where
		// warphold actually ships: Linux and macOS.
		require.Equal(t, os.FileMode(0o750), fi.Mode().Perm(), "the hosted root is not world-readable")
	}

	resp, targets := h.doList("GET", "/api/v1/fleet/targets")
	require.Equal(t, 200, resp.StatusCode)
	require.Len(t, targets, 1)
	require.Equal(t, "Fleet disk", targets[0]["name"])
	require.Equal(t, "hosted", targets[0]["kind"])
	require.Equal(t, hostedRoot, targets[0]["path"])

	resp, templates := h.doList("GET", "/api/v1/fleet/templates")
	require.Equal(t, 200, resp.StatusCode)
	require.Len(t, templates, 1)
	require.Equal(t, "Home", templates[0]["name"])
	require.Equal(t, []any{"~"}, templates[0]["sources"])

	resp, groups := h.doList("GET", "/api/v1/fleet/groups")
	require.Equal(t, 200, resp.StatusCode)
	require.Len(t, groups, 1)
	require.Equal(t, "Devices", groups[0]["name"])
	require.Equal(t, targets[0]["id"], groups[0]["target_id"])
	require.Equal(t, templates[0]["id"], groups[0]["template_id"])

	// ... and the API can issue another token for that group immediately.
	resp, body := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": groups[0]["id"]})
	require.Equal(t, 201, resp.StatusCode)
	require.True(t, strings.HasPrefix(body["token"].(string), "wh_"))

	// Second run: nothing is created twice.
	again, againToken, err := h.s.SetupDefaults(t.Context(), h.srv.URL, "disk", hostedRoot)
	require.NoError(t, err)
	require.Empty(t, again, "a second run has nothing to enroll into")
	require.Empty(t, againToken, "a second run issues no token")

	_, targets = h.doList("GET", "/api/v1/fleet/targets")
	require.Len(t, targets, 1, "no second Fleet disk")
}

func TestSetupDefaultsRefusesCloudAndBadStorage(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	_, _, err := h.s.SetupDefaults(t.Context(), "", "cloud", t.TempDir())
	require.ErrorIs(t, err, api.ErrCloudNeedsWizard)

	_, _, err = h.s.SetupDefaults(t.Context(), "", "tape", t.TempDir())
	require.ErrorContains(t, err, "storage must be disk or cloud")

	_, targets := h.doList("GET", "/api/v1/fleet/targets")
	require.Empty(t, targets, "a refused storage mode creates nothing")
}

// TestSetupDefaultsRepairsAPartialRun: the three rows are not written in one
// transaction, so a run that died after the target must be completed by the
// next one rather than skipped forever.
func TestSetupDefaultsRepairsAPartialRun(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.setPublicURL()

	// The state a crash between CreateTarget and CreateGroup leaves behind.
	resp, _ := h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "Fleet disk", "kind": "filesystem", "path": t.TempDir()})
	require.Equal(t, 201, resp.StatusCode)

	oneLiner, token, err := h.s.SetupDefaults(t.Context(), "", "disk", filepath.Join(h.hostedDir(t), "hosted"))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(token, "wh_"), "the missing group was created and can enroll")
	require.Contains(t, oneLiner, "/enroll.sh")

	_, targets := h.doList("GET", "/api/v1/fleet/targets")
	require.Len(t, targets, 1, "the existing target was reused, not duplicated")

	_, groups := h.doList("GET", "/api/v1/fleet/groups")
	require.Len(t, groups, 1)
	require.Equal(t, targets[0]["id"], groups[0]["target_id"])
}

// A fleet somebody has already configured by hand is left alone.
func TestSetupDefaultsLeavesAConfiguredFleetAlone(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	resp, _ := h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "my nas", "kind": "filesystem", "path": t.TempDir()})
	require.Equal(t, 201, resp.StatusCode)

	oneLiner, token, err := h.s.SetupDefaults(t.Context(), "", "disk", filepath.Join(t.TempDir(), "hosted"))
	require.NoError(t, err)
	require.Empty(t, oneLiner)
	require.Empty(t, token)

	_, targets := h.doList("GET", "/api/v1/fleet/targets")
	require.Len(t, targets, 1)
	require.Equal(t, "my nas", targets[0]["name"])

	_, groups := h.doList("GET", "/api/v1/fleet/groups")
	require.Empty(t, groups)
}

// A hosted root that is a symlink (or a file) is refused before the target is
// written, so no device is ever pointed at it.
func TestSetupDefaultsRefusesABadHostedRoot(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	base := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(base, "real"), 0o750))
	require.NoError(t, os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")))

	_, _, err := h.s.SetupDefaults(t.Context(), "", "disk", filepath.Join(base, "link"))
	require.ErrorContains(t, err, "symlink")

	file := filepath.Join(base, "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	_, _, err = h.s.SetupDefaults(t.Context(), "", "disk", file)
	require.ErrorContains(t, err, "not a directory")

	_, targets := h.doList("GET", "/api/v1/fleet/targets")
	require.Empty(t, targets)
}
