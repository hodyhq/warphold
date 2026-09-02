package engine

import (
	"context"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/serverapi"
)

// snapshotIDSkew is how far *before* a task's start a manifest may still be
// timestamped and belong to that task. Both timestamps come from this
// machine's clock, but the task is registered and the manifest timestamped at
// slightly different moments, and their rounding differs. It is deliberately
// small, and deliberately applied to the lower bound only: widening the upper
// bound past the task's own end time lets the next snapshot of the same
// source - which can start moments after this task finished - fall inside the
// window and be reported as this task's backup.
const snapshotIDSkew = 2 * time.Second

// LatestSnapshotID returns the manifest id of the snapshot of path whose
// start time is closest to notBefore among those starting inside a finished
// task's [notBefore - snapshotIDSkew, notAfter] window - in practice the
// task's own start and end times. The window is bounded at both ends on
// purpose: a lower bound alone would hand an already-finished task the
// manifest of a later snapshot of the same source, which the watch loop can
// observe whenever two snapshots complete between polls. Closest-to-start
// rather than newest for the same reason at finer grain: back-to-back tasks
// overlap within the skew, and the newest candidate there is the *next*
// task's manifest. Reporting no id is always preferable to reporting an
// unrelated one, so a window that matches nothing yields "".
func (l *Local) LatestSnapshotID(ctx context.Context, path string, notBefore, notAfter time.Time) (string, error) {
	if path == "" {
		return "", nil
	}

	var sr serverapi.SnapshotsResponse

	// all=1: without it the server collapses snapshots that share a root
	// entry, so an incremental snapshot of unchanged data would be dropped
	// from the response and the newest matching manifest would be missed.
	if err := l.API.Get(ctx, "snapshots?all=1&"+l.sourceQuery(ExpandHome(path)), nil, &sr); err != nil {
		return "", errors.Wrapf(err, "snapshots for %s", path)
	}

	first, last := notBefore.Add(-snapshotIDSkew), notAfter

	var (
		best      *serverapi.Snapshot
		bestDelta time.Duration
	)

	for _, s := range sr.Snapshots {
		st := s.StartTime.ToTime()
		if st.Before(first) || st.After(last) {
			continue
		}

		delta := st.Sub(notBefore)
		if delta < 0 {
			delta = -delta
		}

		if best == nil || delta < bestDelta {
			best, bestDelta = s, delta
		}
	}

	if best == nil {
		return "", nil
	}

	return string(best.ID), nil
}
