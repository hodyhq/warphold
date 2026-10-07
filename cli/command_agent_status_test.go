package cli_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/tests/testenv"
)

// TestAgentStatusWaitingForTheFleet: an engine that is up but still waiting
// for its Fleet to answer is reported as waiting, not as a dead engine.
func TestAgentStatusWaitingForTheFleet(t *testing.T) {
	t.Setenv("WARPHOLD_STATE_DIR", t.TempDir())

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/repo/status" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"connected":false,"initTaskID":"t1"}`)) //nolint:errcheck

			return
		}

		http.Error(w, `{"code":"NOT_CONNECTED","error":"not connected"}`, http.StatusBadRequest)
	}))
	defer stub.Close()

	require.NoError(t, engine.WriteInfo("user", engine.Info{BaseURL: stub.URL, User: "u", Password: "p"}))

	e := testenv.NewCLITest(t, nil, testenv.NewInProcRunner(t))
	out := strings.Join(e.RunAndExpectSuccess(t, "agent", "status"), "\n")
	require.Contains(t, out, "waiting for the Fleet")
}
