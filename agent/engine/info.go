package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/state"
)

// Info is engine.json: how to reach the engine of the agent process running
// on this machine. It carries the engine's HTTP credentials and the local
// session token, so it is written 0600 and removed when the engine stops.
// Its presence means "an engine was running"; a stale file left by a killed
// process points at a port nothing is listening on, which readers see as an
// unreachable engine.
type Info struct {
	BaseURL    string    `json:"baseUrl"`
	User       string    `json:"user"`
	Password   string    `json:"password"`
	LocalToken string    `json:"localToken"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"startedAt"`
}

// InfoPath is the engine.json path for a scope.
func InfoPath(scope string) string { return filepath.Join(state.Dir(scope), "engine.json") }

// WriteInfo writes engine.json with mode 0600, creating the state directory
// (0700) if needed.
func WriteInfo(scope string, i Info) error {
	if err := os.MkdirAll(state.Dir(scope), 0o700); err != nil {
		return err
	}

	b, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return err
	}

	// Written to a 0600 temporary file and renamed into place: os.WriteFile
	// would leave the engine password readable at the umask's mode until a
	// following Chmod, and would truncate the live file if the write failed.
	tmp, err := os.CreateTemp(state.Dir(scope), "engine-*.json")
	if err != nil {
		return err
	}

	defer os.Remove(tmp.Name()) //nolint:errcheck

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close() //nolint:errcheck,gosec

		return err
	}

	if _, err := tmp.Write(b); err != nil {
		tmp.Close() //nolint:errcheck,gosec

		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), InfoPath(scope))
}

// ReadInfo reads engine.json. A missing file means no engine is running.
func ReadInfo(scope string) (*Info, error) {
	b, err := os.ReadFile(InfoPath(scope))
	if err != nil {
		return nil, err
	}

	var i Info

	if err := json.Unmarshal(b, &i); err != nil {
		return nil, errors.Wrap(err, "malformed engine.json")
	}

	return &i, nil
}

// RemoveInfo deletes engine.json; a missing file is not an error.
func RemoveInfo(scope string) error {
	if err := os.Remove(InfoPath(scope)); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}
