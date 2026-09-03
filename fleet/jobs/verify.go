package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

const (
	// verifyFilesPercent is how much of a repository's file data is actually
	// read back per run. The blob map below already proves every content's
	// pack exists; this samples the bytes inside it. 10 matches the agent's
	// own `verify` command (Task 29) so the two report comparable work.
	verifyFilesPercent = 10

	// verifyMaxErrors bounds both the work and the row: 0 would mean "stop at
	// the first error", which is a report nobody can act on.
	verifyMaxErrors = 10

	// verifyParallelism is per device, and the scheduler runs one job at a
	// time, so this is the Fleet's whole verify footprint.
	verifyParallelism = 4

	verifyQueueLength = 1000

	// detailRawErrors is how many verifier errors reach the row verbatim.
	detailRawErrors = 2
)

// Verify returns the runner for the "verify" job: walk every snapshot of every
// (or one) agent's repository and prove the content behind it is still there
// (spec §3.3, weekly). It never writes: the repository is opened read-only.
func Verify(st *store.Store, k seal.Key) Runner {
	return perAgent(st, k, "verified", true, verifyRepo)
}

func verifyRepo(ctx context.Context, rep repo.Repository, _ store.Agent) error {
	opts := snapshotfs.VerifierOptions{
		VerifyFilesPercent: verifyFilesPercent,
		FileQueueLength:    verifyQueueLength,
		Parallelism:        verifyParallelism,
		MaxErrors:          verifyMaxErrors,
	}

	if dr, ok := rep.(repo.DirectRepository); ok {
		// The blob map turns "the index says content X lives in pack P" into a
		// real existence check, so a lost pack is caught without reading every
		// file. It is one entry per blob, which is the job's memory bound.
		bm, err := blob.ReadBlobMap(ctx, dr.BlobReader())
		if err != nil {
			return fmt.Errorf("reading the blob map: %w", err)
		}

		opts.BlobMap = bm
	}

	if dr, ok := rep.(repo.DirectRepositoryWriter); ok {
		dr.DisableIndexRefresh()
	}

	v := snapshotfs.NewVerifier(ctx, rep, opts)

	result, err := v.InParallel(ctx, func(tw *snapshotfs.TreeWalker) error {
		ids, err := snapshot.ListSnapshotManifests(ctx, rep, nil, nil)
		if err != nil {
			return fmt.Errorf("listing snapshots: %w", err)
		}

		mans, err := snapshot.LoadSnapshots(ctx, rep, ids)
		if err != nil {
			return fmt.Errorf("loading snapshots: %w", err)
		}

		for _, man := range mans {
			if man.RootEntry == nil {
				continue
			}

			root, err := snapshotfs.SnapshotRoot(rep, man)
			if err != nil {
				return fmt.Errorf("snapshot root %v: %w", man.ID, err)
			}

			// The walker accumulates its own errors; the aggregate is read
			// from the result below.
			tw.Process(ctx, root, man.Source.String()) //nolint:errcheck
		}

		return nil
	})

	if result.ErrorCount > 0 {
		// InParallel's own error collapses several failures into "encountered
		// N errors"; §7 wants the real ones, so they are rebuilt from the
		// result rather than reported through err.
		return rawErrors(result.ErrorCount, result.ErrorStrings)
	}

	if err != nil {
		return fmt.Errorf("verifying: %w", err)
	}

	return nil
}

// rawErrors renders the first failures verbatim, with a count for the rest.
func rawErrors(total int, msgs []string) error {
	shown := msgs
	if len(shown) > detailRawErrors {
		shown = shown[:detailRawErrors]
	}

	d := strings.Join(shown, "; ")
	if n := total - len(shown); n > 0 {
		d += fmt.Sprintf(" (+%d more)", n)
	}

	if d == "" {
		return fmt.Errorf("%d errors", total)
	}

	return errors.New(d)
}
