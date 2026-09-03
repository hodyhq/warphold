package engine

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

const (
	// verifyFilesPercent is the share of files verify downloads and reads end
	// to end - the `--verify-files-percent` of `kopia snapshot verify`. Every
	// object is checked against the index and the blob map regardless;
	// downloading is the expensive part, so a fleet-wide verify samples rather
	// than pulling the whole repository over the network on every run.
	verifyFilesPercent = 10.0

	// verifyMaxErrors bounds both the work and the report: the walk stops once
	// this many errors are found, and those are what lands in stderr. Zero
	// would mean "stop at the first one" (snapshotfs.NewTreeWalker turns 0 into
	// 1), which makes for a report an admin cannot act on.
	verifyMaxErrors = 10

	// verifyMaxStderr matches the Fleet server's per-report cap, so the agent
	// truncates on a rune boundary rather than letting the server cut one in
	// half.
	verifyMaxStderr = 8 << 10
)

// Verify checks the repository's snapshots and returns a poll.Report of kind
// "verify". agent/run stamps the task and command ids onto it; the command is
// acknowledged by that report exactly like snapshot-now (original design §6).
//
// The error return means the verification could not be run at all (no
// repository credentials, the repository would not open). A verification that
// ran and found damage is not an error: it comes back as a report with status
// "failed" and the first errors in Stderr, so Fleet records it like any other
// outcome. Fleet health counts kind='snapshot' only, so neither result moves
// the device's health - a verify is evidence about the repository, not about
// whether this device backed up.
func (l *Local) Verify(ctx context.Context) (poll.Report, error) {
	started := clock.Now()

	if l.ConfigFile == "" || l.RepoPassword == "" {
		return poll.Report{}, errors.New("verify needs the agent's repository credentials")
	}

	// The engine holds the repository open for snapshots but Kopia's server API
	// exposes no verify endpoint, so open a second handle against the same
	// config. Verify only ever reads through it - it opens no write session and
	// touches nothing but the index, the blob list and object contents - but
	// the handle itself is write-capable, exactly like the one
	// `kopia snapshot verify` uses. Kopia supports concurrent openers, and a
	// separate handle keeps a long verify off the engine's own request path.
	rep, err := repo.Open(ctx, l.ConfigFile, l.RepoPassword, &repo.Options{})
	if err != nil {
		return poll.Report{}, errors.Wrap(err, "open repository")
	}

	defer rep.Close(ctx) //nolint:errcheck

	opts := snapshotfs.VerifierOptions{
		VerifyFilesPercent: verifyFilesPercent,
		MaxErrors:          verifyMaxErrors,
	}

	// The blob map turns "the index says this content lives in pack X" into a
	// real existence check, which is how a deleted or lost pack is caught
	// without downloading every file.
	if dr, ok := rep.(repo.DirectRepository); ok {
		// Pin the index for the whole walk: a background refresh partway
		// through would leave the blob map below describing an older set of
		// packs, so every content that moved since would look like a missing
		// blob. An always-on agent verifying for longer than the refresh
		// interval would report false damage. cli/command_snapshot_verify.go
		// does the same, for the same reason.
		dr.DisableIndexRefresh()

		blobMap, err := blob.ReadBlobMap(ctx, dr.BlobReader())
		if err != nil {
			return poll.Report{}, errors.Wrap(err, "read blob map")
		}

		opts.BlobMap = blobMap
	}

	v := snapshotfs.NewVerifier(ctx, rep, opts)

	result, verifyErr := v.InParallel(ctx, func(tw *snapshotfs.TreeWalker) error {
		return enqueueAllSnapshots(ctx, rep, tw)
	})

	// Files/Bytes are what verification covered, not what it downloaded: the
	// sampled read is a subset and would understate the work by an order of
	// magnitude.
	out := poll.Report{
		Kind:       "verify",
		StartedAt:  started,
		FinishedAt: clock.Now(),
		Status:     "ok",
		Files:      result.Stats.ProcessedObjectCount,
		Bytes:      result.Stats.ProcessedBytes,
	}

	if verifyErr != nil || result.ErrorCount > 0 {
		out.Status = "failed"
		out.Stderr = verifyStderr(result.ErrorStrings, verifyErr)
	}

	return out, nil
}

// enqueueAllSnapshots hands every snapshot manifest's root to the tree walker.
func enqueueAllSnapshots(ctx context.Context, rep repo.Repository, tw *snapshotfs.TreeWalker) error {
	ids, err := snapshot.ListSnapshotManifests(ctx, rep, nil, nil)
	if err != nil {
		return errors.Wrap(err, "list snapshot manifests")
	}

	manifests, err := snapshot.LoadSnapshots(ctx, rep, ids)
	if err != nil {
		return errors.Wrap(err, "load snapshot manifests")
	}

	for _, man := range manifests {
		if man.RootEntry == nil {
			continue
		}

		root, err := snapshotfs.SnapshotRoot(rep, man)
		if err != nil {
			return errors.Wrapf(err, "snapshot root for %v", man.Source)
		}

		// Per-entry errors are accumulated by the walker and surface in the
		// InParallel result, so a broken tree does not abort the others.
		tw.Process(ctx, root, man.Source.String()) //nolint:errcheck
	}

	return nil
}

// verifyStderr renders the errors found, truncated on a rune boundary.
func verifyStderr(errStrings []string, err error) string {
	lines := errStrings
	if len(lines) == 0 && err != nil {
		lines = []string{err.Error()}
	}

	s := strings.Join(lines, "\n")
	if len(s) <= verifyMaxStderr {
		return s
	}

	s = s[:verifyMaxStderr]

	// Drop only the partial rune the cut may have created. Validating the
	// whole string here would let one bad byte anywhere in an error message
	// eat the entire report, one byte per iteration.
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}

		s = s[:len(s)-1]
	}

	return s
}
