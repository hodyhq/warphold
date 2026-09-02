// Package state persists the agent's enrollment on disk.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is agent.json: the agent's enrollment with a Fleet server. It never
// holds the repository password or the one-time connect token used during
// enrollment - those go through Kopia's own password-persistence strategy
// and are discarded after repo.Connect, respectively.
type Config struct {
	Server       string `json:"server"`
	AgentID      string `json:"agent_id"`
	Bearer       string `json:"bearer"`
	Name         string `json:"name"`
	PollInterval int    `json:"poll_interval_seconds"`
	Scope        string `json:"scope"`
	ETag         string `json:"policy_etag"`
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}

	return h
}

// Dir is where agent.json and repository.config live. $WARPHOLD_STATE_DIR
// overrides both scopes, for tests.
func Dir(scope string) string {
	if d := os.Getenv("WARPHOLD_STATE_DIR"); d != "" {
		return d
	}

	if scope == "system" {
		return "/etc/warphold"
	}

	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "warphold")
	}

	return filepath.Join(home(), ".config", "warphold")
}

// CacheDir is the Kopia content cache directory for the agent's repository.
func CacheDir(scope string) string {
	if d := os.Getenv("WARPHOLD_STATE_DIR"); d != "" {
		return filepath.Join(d, "cache")
	}

	if scope == "system" {
		return "/var/cache/warphold"
	}

	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "warphold")
	}

	return filepath.Join(home(), ".cache", "warphold")
}

// RepoConfigPath is the Kopia repository config file for the agent's scope.
func RepoConfigPath(scope string) string { return filepath.Join(Dir(scope), "repository.config") }

func file(scope string) string { return filepath.Join(Dir(scope), "agent.json") }

// Load reads agent.json for the given scope.
func Load(scope string) (*Config, error) {
	b, err := os.ReadFile(file(scope))
	if err != nil {
		return nil, err
	}

	var c Config

	return &c, json.Unmarshal(b, &c)
}

// Save writes agent.json with mode 0600, creating Dir(scope) (0700) if needed.
func Save(scope string, c *Config) error {
	if err := os.MkdirAll(Dir(scope), 0o700); err != nil {
		return err
	}

	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	// Write-then-rename: run/loop.go saves on every policy ETag change, and a
	// truncated agent.json would cost the agent its server, bearer and id.
	tmp, err := os.CreateTemp(Dir(scope), "agent-*.json")
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

	return os.Rename(tmp.Name(), file(scope))
}
