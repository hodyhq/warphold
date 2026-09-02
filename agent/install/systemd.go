// Package install writes service units so the agent runs at boot.
package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/ospath"
)

// Plan is what an install will do, so it can be printed (--dry-run) or applied.
type Plan struct {
	Files    map[string]string
	Commands [][]string
}

// StartLimitIntervalSec/StartLimitBurst bound Restart=on-failure: without
// them a unit that fails immediately on every start would be restarted
// forever. Five starts inside ten minutes (RestartSec=30 between them) put the
// unit into "failed", where an operator can see it.
const unitTmpl = `[Unit]
Description=WarpHold backup agent
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=600
StartLimitBurst=5

[Service]
ExecStart="%s" agent run --scope %s
Restart=on-failure
RestartSec=30
RestartPreventExitStatus=3
Nice=19
IOSchedulingClass=idle

[Install]
WantedBy=%s
`

// Systemd returns the plan for a scope. User scope resolves the unit
// directory from XDG_CONFIG_HOME or the user's home directory; an
// unresolvable or relative directory is an error rather than a unit written
// somewhere relative to the current working directory, where systemd will
// never find it.
func Systemd(scope, binary string) (Plan, error) {
	if scope == "system" {
		return Plan{
			Files:    map[string]string{"/etc/systemd/system/warphold-agent.service": unit(binary, "system", "multi-user.target")},
			Commands: [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", "warphold-agent"}},
		}, nil
	}

	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Plan{}, errors.Wrap(err, "unable to determine home directory for user-scope install")
		}

		cfg = filepath.Join(home, ".config")
	}

	if !ospath.IsAbs(cfg) {
		return Plan{}, errors.Errorf("config directory %q is not absolute", cfg)
	}

	return Plan{
		Files:    map[string]string{filepath.Join(cfg, "systemd", "user", "warphold-agent.service"): unit(binary, "user", "default.target")},
		Commands: [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", "warphold-agent"}, {"loginctl", "enable-linger"}},
	}, nil
}

func unit(binary, scope, wantedBy string) string {
	return strings.TrimSpace(fmt.Sprintf(unitTmpl, binary, scope, wantedBy)) + "\n"
}

// Apply writes the files then runs the commands.
func Apply(p Plan, runCmd func(name string, args ...string) error) error {
	for path, content := range p.Files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}

		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}

	for _, c := range p.Commands {
		if err := runCmd(c[0], c[1:]...); err != nil {
			return err
		}
	}

	return nil
}
