// Package install writes service units so the agent runs at boot.
package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Plan is what an install will do, so it can be printed (--dry-run) or applied.
type Plan struct {
	Files    map[string]string
	Commands [][]string
}

const unitTmpl = `[Unit]
Description=WarpHold backup agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s agent run --scope %s
Restart=on-failure
RestartSec=30
RestartPreventExitStatus=3
Nice=19
IOSchedulingClass=idle

[Install]
WantedBy=%s
`

// Systemd returns the plan for a scope.
func Systemd(scope, binary string) Plan {
	if scope == "system" {
		return Plan{
			Files:    map[string]string{"/etc/systemd/system/warphold-agent.service": unit(binary, "system", "multi-user.target")},
			Commands: [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", "warphold-agent"}},
		}
	}

	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		home, _ := os.UserHomeDir()
		cfg = filepath.Join(home, ".config")
	}

	return Plan{
		Files:    map[string]string{filepath.Join(cfg, "systemd", "user", "warphold-agent.service"): unit(binary, "user", "default.target")},
		Commands: [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", "warphold-agent"}, {"loginctl", "enable-linger"}},
	}
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
