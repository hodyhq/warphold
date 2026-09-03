package gateway_test

import (
	"context"
	"crypto/md5" //nolint:gosec // S3 ETags are MD5 by definition, not a security choice.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/gateway"
)

func newStore(t *testing.T) (gateway.ObjectStore, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "hosted")

	s, err := gateway.NewLocal(dir)
	require.NoError(t, err)

	return s, dir
}

func put(t *testing.T, s gateway.ObjectStore, key, body string, overwrite bool) (gateway.ObjectInfo, error) {
	t.Helper()

	return s.Put(context.Background(), key, strings.NewReader(body), int64(len(body)), overwrite)
}

func getAll(t *testing.T, s gateway.ObjectStore, key string) string {
	t.Helper()

	rc, _, err := s.Get(context.Background(), key, 0, -1)
	require.NoError(t, err)

	defer rc.Close()

	b, err := io.ReadAll(rc)
	require.NoError(t, err)

	return string(b)
}

func TestPutIsAppendOnly(t *testing.T) {
	s, _ := newStore(t)

	info, err := put(t, s, "dev1/p0001", "first", false)
	require.NoError(t, err)
	require.Equal(t, int64(5), info.Size)
	require.Equal(t, md5hex("first"), info.ETag)
	require.False(t, info.LastModified.IsZero())

	_, err = put(t, s, "dev1/p0001", "second", false)
	require.ErrorIs(t, err, gateway.ErrExists)
	require.Equal(t, "first", getAll(t, s, "dev1/p0001"), "a rejected Put must not touch the stored bytes")

	info, err = put(t, s, "dev1/p0001", "second", true)
	require.NoError(t, err)
	require.Equal(t, md5hex("second"), info.ETag)
	require.Equal(t, "second", getAll(t, s, "dev1/p0001"))
}

func TestConcurrentPutHasExactlyOneWinner(t *testing.T) {
	s, _ := newStore(t)

	const n = 16

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)

	for i := range n {
		wg.Add(1)

		go func() {
			defer wg.Done()

			body := fmt.Sprintf("body-%02d", i)

			_, err := put(t, s, "dev1/race", body, false)
			if err == nil {
				mu.Lock()
				winners = append(winners, body)
				mu.Unlock()

				return
			}

			if !errors.Is(err, gateway.ErrExists) {
				mu.Lock()
				winners = append(winners, "unexpected error: "+err.Error())
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	require.Len(t, winners, 1, "exactly one Put must win: %v", winners)
	require.Equal(t, winners[0], getAll(t, s, "dev1/race"), "stored bytes must be the winner's, whole")
}

func TestGetRange(t *testing.T) {
	s, _ := newStore(t)

	const body = "0123456789"

	_, err := put(t, s, "dev1/r", body, false)
	require.NoError(t, err)

	for _, tc := range []struct {
		offset, length int64
		want           string
	}{
		{0, -1, body},
		{0, 4, "0123"},
		{3, 4, "3456"},
		{6, -1, "6789"},
		{6, 100, "6789"}, // a length past the end is clamped, never over-read
		{9, 1, "9"},
		{0, 0, ""},
	} {
		rc, info, err := s.Get(context.Background(), "dev1/r", tc.offset, tc.length)
		require.NoError(t, err)

		b, err := io.ReadAll(rc)
		require.NoError(t, rc.Close())
		require.NoError(t, err)
		require.Equal(t, tc.want, string(b), "offset=%d length=%d", tc.offset, tc.length)
		require.Equal(t, int64(len(body)), info.Size, "ObjectInfo describes the whole object")
	}

	_, _, err = s.Get(context.Background(), "dev1/r", 10, 1)
	require.ErrorIs(t, err, gateway.ErrRange)

	_, _, err = s.Get(context.Background(), "dev1/r", -1, 1)
	require.ErrorIs(t, err, gateway.ErrRange)

	_, _, err = s.Get(context.Background(), "dev1/missing", 0, -1)
	require.ErrorIs(t, err, gateway.ErrNotFound)
}

func TestHead(t *testing.T) {
	s, _ := newStore(t)

	_, err := s.Head(context.Background(), "dev1/nope")
	require.ErrorIs(t, err, gateway.ErrNotFound)

	_, err = put(t, s, "dev1/h", "hello", false)
	require.NoError(t, err)

	info, err := s.Head(context.Background(), "dev1/h")
	require.NoError(t, err)
	require.Equal(t, "dev1/h", info.Key)
	require.Equal(t, int64(5), info.Size)
	require.Equal(t, md5hex("hello"), info.ETag)
	require.False(t, info.LastModified.IsZero())

	// a directory is not an object
	_, err = s.Head(context.Background(), "dev1")
	require.ErrorIs(t, err, gateway.ErrNotFound)
}

func TestListPaginatesAndConfinesToPrefix(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	for _, k := range []string{"dev1/a", "dev1/b", "dev1/c", "dev1/d", "dev1/sub/e", "dev2/a", "dev2/z"} {
		_, err := put(t, s, k, k, false)
		require.NoError(t, err)
	}

	objs, truncated, err := s.List(ctx, "dev1/", "", 3)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Equal(t, []string{"dev1/a", "dev1/b", "dev1/c"}, keysOf(objs))
	require.Equal(t, int64(len("dev1/a")), objs[0].Size)

	objs, truncated, err = s.List(ctx, "dev1/", "dev1/c", 3)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []string{"dev1/d", "dev1/sub/e"}, keysOf(objs), "after is exclusive and order is lexicographic")

	objs, truncated, err = s.List(ctx, "dev1/", "dev1/sub/e", 3)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Empty(t, objs)

	objs, _, err = s.List(ctx, "dev2/", "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{"dev2/a", "dev2/z"}, keysOf(objs), "another device's keys are never returned")

	objs, _, err = s.List(ctx, "dev1/s", "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{"dev1/sub/e"}, keysOf(objs), "a partial-segment prefix still matches")

	objs, _, err = s.List(ctx, "nosuchdev/", "", 100)
	require.NoError(t, err)
	require.Empty(t, objs)
}

func TestPutWithFailingReaderLeavesNothingBehind(t *testing.T) {
	s, dir := newStore(t)

	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader("half"), errReader{boom})

	_, err := s.Put(context.Background(), "dev1/torn", r, 100, false)
	require.ErrorIs(t, err, boom)

	_, err = s.Head(context.Background(), "dev1/torn")
	require.ErrorIs(t, err, gateway.ErrNotFound)

	// and a short reader that lies about its size is rejected too
	_, err = s.Put(context.Background(), "dev1/short", strings.NewReader("ab"), 10, false)
	require.Error(t, err)

	_, err = s.Head(context.Background(), "dev1/short")
	require.ErrorIs(t, err, gateway.ErrNotFound)

	require.Empty(t, regularFiles(t, dir), "no temp file may survive a torn Put: %v", regularFiles(t, dir))

	objs, _, err := s.List(context.Background(), "", "", 100)
	require.NoError(t, err)
	require.Empty(t, objs)
}

func TestDelete(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	_, err := put(t, s, "dev1/d", "x", false)
	require.NoError(t, err)
	require.NoError(t, s.Delete(ctx, "dev1/d"))

	_, err = s.Head(ctx, "dev1/d")
	require.ErrorIs(t, err, gateway.ErrNotFound)
	require.ErrorIs(t, s.Delete(ctx, "dev1/d"), gateway.ErrNotFound)

	// Delete never removes directories, only objects.
	_, err = put(t, s, "dev1/sub/keep", "x", false)
	require.NoError(t, err)
	require.ErrorIs(t, s.Delete(ctx, "dev1/sub"), gateway.ErrNotFound)
	require.Equal(t, "x", getAll(t, s, "dev1/sub/keep"))
}

func TestModes(t *testing.T) {
	s, dir := newStore(t)

	_, err := put(t, s, "dev1/sub/m", "x", false)
	require.NoError(t, err)

	for _, p := range []string{dir, filepath.Join(dir, "dev1"), filepath.Join(dir, "dev1", "sub")} {
		st, err := os.Stat(p)
		require.NoError(t, err)
		require.Equal(t, fs.FileMode(0o700), st.Mode().Perm(), p)
	}

	st, err := os.Stat(filepath.Join(dir, "dev1", "sub", "m"))
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o600), st.Mode().Perm())
}

func TestHostileKeysNeverEscapeTheRoot(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(outside, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "canary"), []byte("canary"), 0o600))

	dir := filepath.Join(base, "root")

	s, err := gateway.NewLocal(dir)
	require.NoError(t, err)

	ctx := context.Background()

	// a symlink an attacker could only plant out of band; the store must not follow it out
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "dev1"), 0o700))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "dev1", "out")))

	for _, key := range append(append([]string{}, malformedKeys...), "dev1/out/canary", "dev1/out/planted") {
		if key == "" {
			continue
		}

		_, err := s.Put(ctx, key, strings.NewReader("pwned"), 5, false)
		require.Error(t, err, "Put(%q) must fail", key)

		_, err = s.Put(ctx, key, strings.NewReader("pwned"), 5, true)
		require.Error(t, err, "Put(%q, overwrite) must fail", key)

		_, _, err = s.Get(ctx, key, 0, -1)
		require.Error(t, err, "Get(%q) must fail", key)

		_, err = s.Head(ctx, key)
		require.Error(t, err, "Head(%q) must fail", key)

		require.Error(t, s.Delete(ctx, key), "Delete(%q) must fail", key)
	}

	b, err := os.ReadFile(filepath.Join(outside, "canary"))
	require.NoError(t, err)
	require.Equal(t, "canary", string(b))

	require.Equal(t, []string{"canary"}, baseNames(regularFiles(t, outside)), "nothing may be written outside the root")
}

func TestVersioned(t *testing.T) {
	s, _ := newStore(t)
	require.False(t, s.Versioned(context.Background()))
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func md5hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // S3 ETags are MD5 by definition.
	return hex.EncodeToString(sum[:])
}

func keysOf(objs []gateway.ObjectInfo) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.Key
	}

	return out
}

func regularFiles(t *testing.T, dir string) []string {
	t.Helper()

	var out []string

	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.Type().IsRegular() {
			out = append(out, p)
		}

		return nil
	}))

	return out
}

func baseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}

	return out
}
