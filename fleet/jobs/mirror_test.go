package jobs

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/repo/blob/s3"
)

// testMirror stands in for the mirror bucket. It is a real ObjectStore, so the
// append-only contract (ErrExists on a second write) is the real one; it counts
// the calls the mirror job must never make.
type testMirror struct {
	gateway.ObjectStore

	c *mirrorCounters
}

type mirrorCounters struct {
	mu      sync.Mutex
	deletes int
	puts    []string
	putErr  func(key string) error
}

func (m *testMirror) Delete(ctx context.Context, key string) error {
	m.c.mu.Lock()
	m.c.deletes++
	m.c.mu.Unlock()

	return m.ObjectStore.Delete(ctx, key)
}

func (m *testMirror) Put(ctx context.Context, key string, r io.Reader, size int64, overwrite bool) (gateway.ObjectInfo, error) {
	m.c.mu.Lock()
	m.c.puts = append(m.c.puts, key)
	fail := m.c.putErr
	m.c.mu.Unlock()

	if fail != nil {
		if err := fail(key); err != nil {
			return gateway.ObjectInfo{}, err
		}
	}

	return m.ObjectStore.Put(ctx, key, r, size, overwrite)
}

func (c *mirrorCounters) snapshot() (deletes int, puts []string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.deletes, append([]string(nil), c.puts...)
}

// mirrorFixture is a hosted disk target, its local root and a stand-in mirror.
type mirrorFixture struct {
	st       *store.Store
	key      seal.Key
	dir      string
	mirror   string
	counters *mirrorCounters
	target   store.Target
}

// seedAgents creates the group chain and the agents the jobs and repo_stats
// foreign keys require.
func seedAgents(t *testing.T, s *store.Store, ids ...string) {
	t.Helper()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	tid, err := s.CreateTarget(ctx, &store.Target{Name: "group target", Kind: "hosted", CreatedAt: now})
	require.NoError(t, err)

	tpl, err := s.CreateTemplate(ctx, &store.Template{Name: "default", Sources: []string{"~"}, PolicyJSON: json.RawMessage(`{}`), CreatedAt: now})
	require.NoError(t, err)

	gid, err := s.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl, CreatedAt: now})
	require.NoError(t, err)

	for _, id := range ids {
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID: id, Name: id, Hostname: id, OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid,
			BearerHash: []byte("h_" + id), SealedBundle: []byte("b"), EnrolledAt: now,
		}))
	}
}

// newMirrorFixture builds a hosted disk target with a verified mirror; tweak
// may change the row before it is created (there is no UpdateTarget yet).
func newMirrorFixture(t *testing.T, tweak func(*store.Target)) *mirrorFixture {
	t.Helper()

	st := openTemp(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	salt, err := seal.NewSalt()
	require.NoError(t, err)

	key := seal.Derive("a fleet passphrase", salt)

	creds, err := json.Marshal(mirrorCreds{KeyID: "0051key", Key: "K005secret"})
	require.NoError(t, err)

	sealed, err := key.Seal(creds)
	require.NoError(t, err)

	root := t.TempDir()
	verified := now

	tgt := store.Target{
		Name: "hosted", Kind: "hosted", StorageMode: "disk", Path: root,
		MirrorKind: "b2", MirrorBucket: "warphold-offsite", MirrorRegion: "us-west-004",
		SealedMirrorKey: sealed, MirrorLockVerifiedAt: &verified, CreatedAt: now,
	}

	if tweak != nil {
		tweak(&tgt)
	}

	id, err := st.CreateTarget(ctx, &tgt)
	require.NoError(t, err)
	tgt.ID = id

	f := &mirrorFixture{st: st, key: key, dir: root, mirror: t.TempDir(), counters: &mirrorCounters{}, target: tgt}

	old := openMirror
	t.Cleanup(func() { openMirror = old })

	openMirror = func(_ context.Context, _ store.Target, c mirrorCreds) (gateway.ObjectStore, error) {
		require.Equal(t, "0051key", c.KeyID)
		require.Equal(t, "K005secret", c.Key)

		// A fresh store per call, exactly as the real one is opened per run.
		s, err := gateway.NewLocal(f.mirror, gateway.LocalOptions{})
		if err != nil {
			return nil, err
		}

		return &testMirror{ObjectStore: s, c: f.counters}, nil
	}

	return f
}

// write puts a blob in the local hosted root, the way the gateway would.
func (f *mirrorFixture) write(t *testing.T, dir, key, body string) {
	t.Helper()

	full := filepath.Join(dir, filepath.FromSlash(key))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
	require.NoError(t, os.WriteFile(full, []byte(body), 0o600))
}

// contents walks a root and returns key -> body, so the mirror can be compared
// with the local disk after the store has been closed.
func contents(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, ".") {
			return nil // the backend's own .tmp staging area
		}

		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		out[key] = string(b)

		return nil
	}))

	return out
}

func (f *mirrorFixture) run(t *testing.T) (string, error) {
	t.Helper()

	return Mirror(f.st, f.key)(context.Background(), store.Job{Kind: "mirror"})
}

func TestMirrorUploadsOnlyWhatIsMissing(t *testing.T) {
	f := newMirrorFixture(t, nil)
	seedAgents(t, f.st, "dev1", "dev2")

	f.write(t, f.dir, "dev1/kopia.repository", "format")
	f.write(t, f.dir, "dev1/p001", "pack one")
	f.write(t, f.dir, "dev1/p002", "pack two")
	f.write(t, f.dir, "dev2/p003", "another device")

	// Half of dev1 is already offsite.
	f.write(t, f.mirror, "dev1/p001", "pack one")

	detail, err := f.run(t)
	require.NoError(t, err)
	require.Equal(t, "3 objects, 28 bytes, 1 skipped", detail)

	require.Equal(t, contents(t, f.dir), contents(t, f.mirror), "the mirror holds every local blob")

	deletes, puts := f.counters.snapshot()
	require.Zero(t, deletes, "the mirror job must never delete")
	require.Equal(t, []string{"dev1/kopia.repository", "dev1/p002", "dev2/p003"}, sorted(puts))

	// Idempotent: a second run uploads nothing.
	detail, err = f.run(t)
	require.NoError(t, err)
	require.Equal(t, "0 objects, 0 bytes, 4 skipped", detail)

	deletes, puts = f.counters.snapshot()
	require.Zero(t, deletes)
	require.Len(t, puts, 3, "the second run put nothing")
}

func TestMirrorRecordsPerDeviceProgress(t *testing.T) {
	f := newMirrorFixture(t, nil)
	seedAgents(t, f.st, "dev1", "dev2")
	ctx := context.Background()

	f.write(t, f.dir, "dev1/p001", "12345")
	f.write(t, f.dir, "dev1/p002", "678")
	f.write(t, f.dir, "dev2/p003", "9")

	before := time.Now().Add(-time.Second)

	_, err := f.run(t)
	require.NoError(t, err)

	one, err := f.st.RepoStat(ctx, "dev1")
	require.NoError(t, err)
	require.EqualValues(t, 8, one.MirroredBytes)
	require.NotNil(t, one.MirroredAt)
	require.True(t, one.MirroredAt.After(before))

	two, err := f.st.RepoStat(ctx, "dev2")
	require.NoError(t, err)
	require.EqualValues(t, 1, two.MirroredBytes)
}

func TestMirrorSkipsADirectoryThatIsNotAnAgent(t *testing.T) {
	f := newMirrorFixture(t, nil)
	seedAgents(t, f.st, "dev1")

	// A directory left behind by a removed agent still gets its bytes offsite;
	// there is simply no repo_stats row to write for it.
	f.write(t, f.dir, "gone/p001", "orphan")

	detail, err := f.run(t)
	require.NoError(t, err)
	require.Equal(t, "1 objects, 6 bytes, 0 skipped", detail)
	require.Equal(t, map[string]string{"gone/p001": "orphan"}, contents(t, f.mirror))

	_, err = f.st.RepoStat(context.Background(), "gone")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestMirrorRefusesAnUnverifiedBucket(t *testing.T) {
	f := newMirrorFixture(t, func(tg *store.Target) { tg.MirrorLockVerifiedAt = nil })
	seedAgents(t, f.st, "dev1")
	f.write(t, f.dir, "dev1/p001", "never leaves")

	detail, err := f.run(t)
	require.Error(t, err)
	require.Equal(t, "0 objects, 0 bytes, 0 skipped, errors: hosted: mirror not verified", detail)

	_, puts := f.counters.snapshot()
	require.Empty(t, puts, "an unverified bucket receives nothing")
	require.Empty(t, contents(t, f.mirror))
}

func TestMirrorContinuesAfterADeviceFails(t *testing.T) {
	f := newMirrorFixture(t, nil)
	seedAgents(t, f.st, "dev1", "dev2")

	f.write(t, f.dir, "dev1/p001", "first")
	f.write(t, f.dir, "dev1/p002", "second")
	f.write(t, f.dir, "dev2/p003", "third")

	f.counters.putErr = func(key string) error {
		if strings.HasPrefix(key, "dev1/") {
			return io.ErrUnexpectedEOF
		}

		return nil
	}

	detail, err := f.run(t)
	require.Error(t, err)
	require.Contains(t, detail, "errors: hosted/dev1: uploading dev1/p001")
	require.Contains(t, detail, "1 objects, 5 bytes")

	// The device that worked is mirrored and recorded; the one that failed is
	// left for the next run, with no stats row claiming it is offsite.
	require.Equal(t, map[string]string{"dev2/p003": "third"}, contents(t, f.mirror))

	_, err = f.st.RepoStat(context.Background(), "dev2")
	require.NoError(t, err)

	_, err = f.st.RepoStat(context.Background(), "dev1")
	require.ErrorIs(t, err, store.ErrNotFound)
}

// A key the mirror already has is treated as mirrored two different ways:
// the pre-upload listing catches most of them, but a key uploaded by a
// racing run between that listing and this one's own Put is caught by Put
// itself returning ErrExists. Both must count as skipped, and neither must
// ever reach Delete.
func TestMirrorTreatsAnExistingKeyAsMirrored(t *testing.T) {
	f := newMirrorFixture(t, nil)
	seedAgents(t, f.st, "dev1")

	f.write(t, f.dir, "dev1/p001", "first")

	detail, err := f.run(t)
	require.NoError(t, err)
	require.Equal(t, "1 objects, 5 bytes, 0 skipped", detail)

	deletes, _ := f.counters.snapshot()
	require.Zero(t, deletes)

	// A second local object appears. Nothing pre-seeds the mirror with it, so
	// the listing does not know about it either -- upload is attempted, and
	// Put itself fails with ErrExists, the way it would if another run's
	// upload landed the key first.
	f.write(t, f.dir, "dev1/p002", "second")
	f.counters.putErr = func(string) error { return gateway.ErrExists }

	detail, err = f.run(t)
	require.NoError(t, err)
	// p001 is skipped via the pre-upload listing (the first run's real Put
	// left it in the mirror); p002 is skipped via Put's ErrExists.
	require.Equal(t, "0 objects, 0 bytes, 2 skipped", detail)

	deletes, _ = f.counters.snapshot()
	require.Zero(t, deletes, "the mirror job must never delete")
}

func TestMirrorIgnoresTargetsWithoutADiskMirror(t *testing.T) {
	for name, change := range map[string]func(*store.Target){
		"no mirror":  func(tg *store.Target) { tg.MirrorKind = "" },
		"cloud mode": func(tg *store.Target) { tg.StorageMode = "cloud" },
		"not hosted": func(tg *store.Target) { tg.Kind = "b2" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newMirrorFixture(t, change)
			seedAgents(t, f.st, "dev1")
			f.write(t, f.dir, "dev1/p001", "not mirrored")

			detail, err := f.run(t)
			require.NoError(t, err)
			require.Equal(t, "0 objects, 0 bytes, 0 skipped", detail)

			_, puts := f.counters.snapshot()
			require.Empty(t, puts)
		})
	}
}

func TestMirrorReportsOnlyTheFirstFewErrors(t *testing.T) {
	f := newMirrorFixture(t, nil)
	seedAgents(t, f.st, "dev1")

	for _, d := range []string{"d1", "d2", "d3", "d4", "d5"} {
		f.write(t, f.dir, d+"/p001", "body")
	}

	f.counters.putErr = func(string) error { return io.ErrUnexpectedEOF }

	detail, err := f.run(t)
	require.Error(t, err)
	require.Equal(t, 2, strings.Count(detail, "; "), "three errors are named")
	require.Contains(t, detail, "(+2 more)")
}

func TestMirrorRejectsAnUnsealableKey(t *testing.T) {
	f := newMirrorFixture(t, func(tg *store.Target) { tg.SealedMirrorKey = []byte("not sealed with this key") })
	seedAgents(t, f.st, "dev1")
	f.write(t, f.dir, "dev1/p001", "body")

	detail, err := f.run(t)
	require.Error(t, err)
	require.Contains(t, detail, "unsealing the mirror credentials failed")
	require.Empty(t, contents(t, f.mirror))
}

// TestMirrorRecordsFailureWhenCancelledBetweenTargets covers the outer target
// loop, not the per-device cancellation path TestMirrorContinuesAfterADevice-
// Fails-adjacent tests already cover: once ctx is cancelled, the loop must
// break AND say so, not just leave whatever the target in flight happened to
// fail with as the only error. The fixture's one real target fails on its own
// (its listing sees the same cancelled ctx) - the assertion is that the
// loop's own break adds a second, distinct entry on top of that, which is
// only true once the break itself records the cancellation.
func TestMirrorRecordsFailureWhenCancelledBetweenTargets(t *testing.T) {
	f := newMirrorFixture(t, nil)
	seedAgents(t, f.st, "dev1") // also creates a second, unrelated target row

	f.write(t, f.dir, "dev1/p001", "body")

	cctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Cancels as soon as the first target's connection opens: everything
	// after that - the target's own remaining work, and the loop's next
	// iteration - sees a done context, deterministically and without racing
	// real time.
	old := openMirror
	t.Cleanup(func() { openMirror = old })
	openMirror = func(c context.Context, tg store.Target, cr mirrorCreds) (gateway.ObjectStore, error) {
		cancel()

		return old(c, tg, cr)
	}

	detail, err := Mirror(f.st, f.key)(cctx, store.Job{Kind: "mirror"})
	require.Error(t, err)
	require.Contains(t, detail, "mirror: context canceled",
		"the loop's own break must record the cancellation, not just leave it to the target in flight")

	require.Empty(t, contents(t, f.mirror), "cancelled before any upload could start")
}

func TestMirrorConnectionInfo(t *testing.T) {
	creds := mirrorCreds{KeyID: "id", Key: "secret"}
	base := store.Target{MirrorKind: "b2", MirrorBucket: "offsite", MirrorRegion: "us-west-004"}

	ci, err := mirrorCI(base, creds)
	require.NoError(t, err)
	require.Equal(t, "s3", ci.Type)

	opt, ok := ci.Config.(*s3.Options)
	require.True(t, ok)
	require.Equal(t, "s3.us-west-004.backblazeb2.com", opt.Endpoint)
	require.Equal(t, "offsite", opt.BucketName)
	require.Equal(t, "us-west-004", opt.Region)
	require.Equal(t, "id", opt.AccessKeyID)
	require.False(t, opt.DoNotUseTLS)

	aws := base
	aws.MirrorKind, aws.MirrorRegion = "s3", "us-east-1"
	ci, err = mirrorCI(aws, creds)
	require.NoError(t, err)
	require.Equal(t, "s3.us-east-1.amazonaws.com", ci.Config.(*s3.Options).Endpoint)

	for name, bad := range map[string]struct {
		t store.Target
		c mirrorCreds
	}{
		"no bucket":      {t: store.Target{MirrorKind: "b2", MirrorRegion: "us-west-004"}, c: creds},
		"no region":      {t: store.Target{MirrorKind: "b2", MirrorBucket: "offsite"}, c: creds},
		"no credentials": {t: base, c: mirrorCreds{}},
		"unknown kind":   {t: store.Target{MirrorKind: "gdrive", MirrorBucket: "offsite", MirrorRegion: "r"}, c: creds},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := mirrorCI(bad.t, bad.c)
			require.Error(t, err)
		})
	}
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)

	return out
}
