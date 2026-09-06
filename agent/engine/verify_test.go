package engine_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/internal/passwordpersist"
	"github.com/kopia/kopia/internal/uitask"
)

// TestVerifyReportsRepositoryDamage runs Verify against a real repository that
// holds a real snapshot: intact, it is "ok"; with one data pack removed from
// the target, it is "failed" and names the damage. This is the whole point of
// the Verify button - a snapshot that uploaded successfully says nothing about
// whether its bytes are still readable.
func TestVerifyReportsRepositoryDamage(t *testing.T) {
	ctx := context.Background()

	t.Setenv("WARPHOLD_STATE_DIR", t.TempDir())

	cfg, pw, blobDir := provisionedRepo(t)

	h, err := engine.StartHeadless(ctx, cfg, pw, "user", passwordpersist.None())
	require.NoError(t, err)

	defer h.Stop(ctx) //nolint:errcheck

	api, err := h.Client()
	require.NoError(t, err)

	l, err := engine.NewLocal(ctx, api)
	require.NoError(t, err)

	// Big enough that its contents land in their own data pack rather than
	// being inlined with the directory metadata.
	src := t.TempDir()

	big := make([]byte, 512<<10)
	for i := range big {
		big[i] = byte(i)
	}

	require.NoError(t, os.WriteFile(filepath.Join(src, "big.bin"), big, 0o600))

	pol, err := json.Marshal(map[string]any{"scheduling": map[string]any{"manual": true}})
	require.NoError(t, err)
	require.NoError(t, l.Apply(ctx, []poll.Source{{Path: src, Policy: pol}}))
	require.NoError(t, l.Snapshot(ctx, src))

	require.Eventually(t, func() bool {
		tasks, err := l.Tasks(ctx)
		if err != nil {
			return false
		}

		for _, tk := range tasks {
			if tk.Kind == "Snapshot" && tk.EndTime != nil && tk.Status == uitask.StatusSuccess {
				return true
			}
		}

		return false
	}, 60*time.Second, 200*time.Millisecond, "the snapshot must finish before there is anything to verify")

	// Without credentials Verify refuses rather than guessing: the tray builds
	// a Local too and must not be able to start a repository-wide read.
	_, err = l.Verify(ctx)
	require.ErrorContains(t, err, "credentials")

	l.ConfigFile, l.RepoPassword = cfg, pw

	rep, err := l.Verify(ctx)
	require.NoError(t, err)
	require.Equal(t, "verify", rep.Kind)
	require.Equal(t, "ok", rep.Status)
	require.Empty(t, rep.Stderr)
	require.Positive(t, rep.Files, "an intact snapshot must report the objects it covered")
	require.False(t, rep.StartedAt.IsZero())
	require.False(t, rep.FinishedAt.Before(rep.StartedAt))

	require.NoError(t, removeOneDataPack(t, blobDir))

	rep, err = l.Verify(ctx)
	require.NoError(t, err, "damage found is a report, not a failure to run")
	require.Equal(t, "verify", rep.Kind)
	require.Equal(t, "failed", rep.Status)
	require.Contains(t, rep.Stderr, "missing blob", "an admin needs to see what broke")
	require.LessOrEqual(t, len(rep.Stderr), 8<<10, "stderr is capped like every other report")
}

// removeOneDataPack deletes a single "p" pack blob - the ones holding file
// contents - from a filesystem repository, leaving the index still pointing at
// it. Metadata ("q") packs are left alone so the tree stays walkable and the
// failure is the missing data, not an unreadable directory.
func removeOneDataPack(t *testing.T, blobDir string) error {
	t.Helper()

	var found string

	require.NoError(t, filepath.WalkDir(blobDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return err //nolint:wrapcheck
		}

		// The filesystem provider sharded the blob id into directories, so the
		// "p" of "p<hex>" is the first path segment, not a filename prefix.
		rel, relErr := filepath.Rel(blobDir, path)
		if relErr == nil && strings.HasPrefix(rel, "p"+string(filepath.Separator)) && strings.HasSuffix(rel, ".f") {
			found = path
		}

		return nil
	}))

	require.NotEmpty(t, found, "the snapshot should have written at least one data pack")

	return os.Remove(found)
}
