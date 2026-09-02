package engine

import (
	"context"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/serverapi"
)

// snapshotIDSkew is how far outside a task's [start, end] window a manifest
// may still belong to that task. Both timestamps come from this machine's
// clock, but the task is registered and the manifest timestamped at slightly
// different moments, and their rounding differs.
const snapshotIDSkew = 5 * time.Second

// LatestSnapshotID returns the manifest id of the newest snapshot of path
// that started within a finished task's [notBefore, notAfter] window - in
// practice the task's own start and end times. The window is bounded at both
// ends on purpose: a lower bound alone would hand an already-finished task
// the manifest of a later snapshot of the same source, which the watch loop
// can observe whenever two snapshots complete between polls. Reporting no id
// is always preferable to reporting an unrelated one, so a window that
// matches nothing yields "".
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

	first, last := notBefore.Add(-snapshotIDSkew), notAfter.Add(snapshotIDSkew)

	var newest *serverapi.Snapshot

	for _, s := range sr.Snapshots {
		if st := s.StartTime.ToTime(); st.Before(first) || st.After(last) {
			continue
		}

		if newest == nil || s.StartTime.After(newest.StartTime) {
			newest = s
		}
	}

	if newest == nil {
		return "", nil
	}

	return string(newest.ID), nil
}
