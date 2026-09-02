//go:build unix

package api_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The served installer is a shell script, so string-matching it only proves it
// contains the right lines. These tests run it: the token prompt is the one
// piece of the enrollment flow whose failure mode (hanging on a terminal that
// is not there, or enrolling with an empty token) does not show up in the text.

// runEnrollScript executes the script with no controlling terminal, so
// /dev/tty cannot be opened - the state cron, CI and a container are in, and
// the one where an unguarded prompt would hang. Setsid is what guarantees it
// regardless of how "go test" itself was started.
func runEnrollScript(t *testing.T, script, home string, env ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", script)
	cmd.Dir = home
	cmd.Env = append([]string{"HOME=" + home, "PATH=/usr/bin:/bin"}, env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// A nil Stdin is /dev/null, which is what "curl ... | sh" leaves behind
	// once the script has been read.
	out, err := cmd.CombinedOutput()
	require.NoError(t, ctx.Err(), "the script did not finish: it is waiting on something")

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	require.NoError(t, err)
	return string(out), 0
}

// enrollScript downloads the script the server serves into a file.
func enrollScript(t *testing.T, h *harness, dir string) string {
	t.Helper()
	res, err := http.Get(h.srv.URL + "/enroll.sh")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	path := filepath.Join(dir, "enroll.sh")
	require.NoError(t, os.WriteFile(path, body, 0o700))
	return path
}

// stubAgent puts a fake warphold binary where the script looks for one, so the
// run never reaches the download and never touches the network. It reports its
// arguments and whether it was handed a token - never the token itself, which
// would then sit in the test output of every failed run.
func stubAgent(t *testing.T, home string) {
	t.Helper()
	bin := filepath.Join(home, ".local", "bin")
	require.NoError(t, os.MkdirAll(bin, 0o700))
	stub := "#!/bin/sh\nif [ -n \"${WARPHOLD_ENROLL_TOKEN:-}\" ]; then t=present; else t=absent; fi\necho \"stub $* token=$t\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "warphold"), []byte(stub), 0o700))
}

func TestEnrollShWithoutATerminalAsksForNothing(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	dir := t.TempDir()

	out, code := runEnrollScript(t, enrollScript(t, h, dir), t.TempDir())

	require.Equal(t, 2, code, "no token and no terminal is a usage error")
	require.Contains(t, out, "usage: run it on a terminal")
	require.NotContains(t, out, "Enrollment token:", "there is no terminal to prompt on")
}

func TestEnrollShSkipsThePromptWhenTheTokenIsInTheEnvironment(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	dir := t.TempDir()
	home := t.TempDir()
	stubAgent(t, home)

	out, code := runEnrollScript(t, enrollScript(t, h, dir), home, "WARPHOLD_ENROLL_TOKEN="+wellFormedEnrollToken)

	require.Equal(t, 0, code, out)
	require.NotContains(t, out, "Enrollment token:", "a token in the environment means no prompt")
	require.Contains(t, out, "using existing")
	// The token reaches the agent through its environment, not its argv, and
	// nothing the run prints carries it.
	require.Contains(t, out, "stub agent enroll --server "+h.srv.URL+" --scope user token=present")
	require.Contains(t, out, "stub agent install --scope user token=present")
	require.NotContains(t, out, wellFormedEnrollToken, "the token must not be echoed anywhere")
}
