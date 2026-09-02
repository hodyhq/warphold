//go:build !linux

package tray

import (
	"context"

	"github.com/pkg/errors"
)

// Run reports that there is no tray on this platform. The Linux
// implementation lives in run_linux.go; this stub keeps the darwin and
// windows builds (and the release cross-compiles) working.
func Run(_ context.Context, _ Options) error {
	return errors.New("tray is Linux-only in this release")
}
