package fleet

import (
	"errors"
	"os"

	"github.com/gofrs/flock"
)

// ErrLocked is returned by TryLock when another process is already using the
// state directory - in practice, a running Fleet server.
var ErrLocked = errors.New("another process is using this fleet state directory")

// TryLock takes the state directory's lock without waiting. A running Fleet
// server holds it for its whole life, so an offline command that would write
// sealed state can tell it apart from a stopped one: SQLite's WAL and busy
// timeout would happily let both write, and the running process would keep
// sealing with a key the offline one just replaced.
//
// The returned lock must be released with Unlock.
func TryLock(stateDir string) (*flock.Flock, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}

	fl := flock.New(PathsFor(stateDir).LockFile)

	ok, err := fl.TryLock()
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, ErrLocked
	}

	return fl, nil
}
