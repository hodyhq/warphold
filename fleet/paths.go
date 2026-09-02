// Package fleet holds shared paths for the Fleet control plane.
package fleet

import "path/filepath"

// StateDirFor returns the Fleet state directory next to the repository config file.
func StateDirFor(configFile string) string { return filepath.Join(filepath.Dir(configFile), "fleet") }

// Paths are the files inside a state directory.
type Paths struct{ StateDir, DB, KeyFile string }

// PathsFor derives Paths from a state directory.
func PathsFor(stateDir string) Paths {
	return Paths{StateDir: stateDir, DB: filepath.Join(stateDir, "fleet.db"), KeyFile: filepath.Join(stateDir, "seal.key")}
}
