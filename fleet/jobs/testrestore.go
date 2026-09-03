package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"

	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/restore"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

const (
	// sampleMaxBytes is the largest file a test-restore will pull back. The
	// point is to prove a restore works end to end, not to move data.
	sampleMaxBytes = 8 << 20

	// sampleMaxEntries and sampleMaxDepth bound the walk that looks for a
	// candidate, so a pathological tree cannot turn the sample into a crawl.
	sampleMaxEntries = 5000
	sampleMaxDepth   = 24
)

// TestRestore returns the runner for the "test-restore" job: restore one small
// file from each agent's most recent snapshot into a temporary directory and
// compare the bytes on disk with the object in the repository (spec §3.3,
// monthly). The repository is opened read-only and nothing is written back to
// it: only the temporary copy, which is removed either way.
func TestRestore(st *store.Store, k seal.Key) Runner {
	return perAgent(st, k, "restored", true, testRestoreRepo)
}

func testRestoreRepo(ctx context.Context, rep repo.Repository, _ store.Agent) error {
	man, err := latestSnapshot(ctx, rep)
	if err != nil {
		return err
	}

	if man == nil {
		// A device that has never finished a snapshot has nothing to prove
		// yet; that is the health check's business, not this job's.
		return nil
	}

	root, err := snapshotfs.SnapshotRoot(rep, man)
	if err != nil {
		return fmt.Errorf("snapshot root: %w", err)
	}

	dir, ok := root.(fs.Directory)
	if !ok {
		return errors.New("the snapshot root is not a directory")
	}

	file, err := pickSample(ctx, dir)
	if err != nil {
		return err
	}

	if file == nil {
		return nil // nothing small enough to sample
	}

	want, size, err := hashObject(ctx, file)
	if err != nil {
		return fmt.Errorf("reading %s from the repository: %w", file.Name(), err)
	}

	tmp, err := os.MkdirTemp("", "warphold-restore-*")
	if err != nil {
		return fmt.Errorf("scratch directory: %w", err)
	}

	defer os.RemoveAll(tmp) //nolint:errcheck // best effort

	out := &restore.FilesystemOutput{
		TargetPath: filepath.Join(tmp, "sample"),
		// The Fleet is not the device's user and does not run as root: the
		// bytes are the assertion, not the uid.
		SkipOwners:             true,
		IgnorePermissionErrors: true,
	}
	if err := out.Init(ctx); err != nil {
		return fmt.Errorf("preparing the restore: %w", err)
	}

	if _, err := restore.Entry(ctx, rep, out, file, restore.Options{Parallel: 1}); err != nil {
		return fmt.Errorf("restoring %s: %w", file.Name(), err)
	}

	return checkRestored(file.Name(), want, size, out.TargetPath)
}

// checkRestored compares what landed on disk with what the repository holds.
// It is the whole point of the job, so it says both digests when they differ.
func checkRestored(name string, want []byte, wantSize int64, path string) error {
	got, size, err := hashFile(path)
	if err != nil {
		return fmt.Errorf("reading the restored %s: %w", name, err)
	}

	if size != wantSize {
		return fmt.Errorf("restored bytes differ for %s: %d bytes restored, %d in the repository", name, size, wantSize)
	}

	if !bytes.Equal(got, want) {
		return fmt.Errorf("restored bytes differ for %s: sha256 %s restored, %s in the repository",
			name, hex.EncodeToString(got), hex.EncodeToString(want))
	}

	return nil
}

// latestSnapshot is the newest snapshot manifest in the repository, or nil.
func latestSnapshot(ctx context.Context, rep repo.Repository) (*snapshot.Manifest, error) {
	ids, err := snapshot.ListSnapshotManifests(ctx, rep, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("listing snapshots: %w", err)
	}

	mans, err := snapshot.LoadSnapshots(ctx, rep, ids)
	if err != nil {
		return nil, fmt.Errorf("loading snapshots: %w", err)
	}

	sort.Slice(mans, func(i, j int) bool { return mans[i].StartTime.ToTime().Before(mans[j].StartTime.ToTime()) })

	for i := len(mans) - 1; i >= 0; i-- {
		if mans[i].RootEntry != nil {
			return mans[i], nil
		}
	}

	return nil, nil
}

// pickSample walks the snapshot until it has seen enough entries and returns
// one small file, chosen uniformly at random among the candidates it saw
// (reservoir sampling: one pass, one entry held). Picking at random rather
// than the first file means a monthly job eventually covers the tree.
func pickSample(ctx context.Context, root fs.Directory) (fs.File, error) {
	var (
		chosen fs.File
		seen   int
		n      int
	)

	var walk func(context.Context, fs.Directory, int) error

	walk = func(ctx context.Context, dir fs.Directory, depth int) error {
		if depth > sampleMaxDepth || seen >= sampleMaxEntries {
			return nil
		}

		return fs.IterateEntries(ctx, dir, func(ctx context.Context, e fs.Entry) error {
			if err := ctx.Err(); err != nil {
				return err
			}

			seen++
			if seen >= sampleMaxEntries {
				return nil
			}

			switch e := e.(type) {
			case fs.Directory:
				return walk(ctx, e, depth+1)
			case fs.File:
				if e.Size() <= 0 || e.Size() > sampleMaxBytes {
					return nil
				}

				n++
				//nolint:gosec // sampling which file to restore, not a secret
				if rand.IntN(n) == 0 {
					chosen = e
				}
			}

			return nil
		})
	}

	if err := walk(ctx, root, 0); err != nil {
		return nil, fmt.Errorf("walking the snapshot: %w", err)
	}

	return chosen, nil
}

// hashObject streams the file out of the repository and hashes it, so neither
// side of the comparison is ever held in memory.
func hashObject(ctx context.Context, f fs.File) ([]byte, int64, error) {
	r, err := f.Open(ctx)
	if err != nil {
		return nil, 0, err //nolint:wrapcheck // the caller names the file
	}

	defer r.Close() //nolint:errcheck // read-only handle

	return hashStream(r)
}

func hashFile(path string) ([]byte, int64, error) {
	f, err := os.Open(path) //nolint:gosec // a path this job just created
	if err != nil {
		return nil, 0, err //nolint:wrapcheck // the caller names the file
	}

	defer f.Close() //nolint:errcheck // read-only handle

	return hashStream(f)
}

func hashStream(r io.Reader) ([]byte, int64, error) {
	h := sha256.New()

	n, err := io.Copy(h, r)
	if err != nil {
		return nil, 0, err //nolint:wrapcheck // the caller names the file
	}

	return h.Sum(nil), n, nil
}
