package jobs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/fleet/store"
)

// versionedMirror is an in-memory S3 bucket with versioning on: Put adds a
// version, Delete without a version id adds a delete marker (B2's "hide"), and
// nothing ever removes a version, which is what Object Lock enforces.
type versionedMirror struct {
	mu   sync.Mutex
	objs map[string][]mirrorVersion

	// unversioned makes the bucket answer GetBucketVersioning "off", where a
	// DeleteObject would be a real delete.
	unversioned bool
}

type mirrorVersion struct {
	data   []byte
	marker bool
	mod    time.Time
}

func (v *versionedMirror) live(key string) (mirrorVersion, bool) {
	vs := v.objs[key]
	if len(vs) == 0 || vs[len(vs)-1].marker {
		return mirrorVersion{}, false
	}

	return vs[len(vs)-1], true
}

func (v *versionedMirror) seed(key string, mod time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.objs[key] = append(v.objs[key], mirrorVersion{data: []byte(key), mod: mod})
}

func (v *versionedMirror) versions(key string) []mirrorVersion {
	v.mu.Lock()
	defer v.mu.Unlock()

	return append([]mirrorVersion(nil), v.objs[key]...)
}

func (v *versionedMirror) markers() int {
	v.mu.Lock()
	defer v.mu.Unlock()

	n := 0

	for _, vs := range v.objs {
		for _, x := range vs {
			if x.marker {
				n++
			}
		}
	}

	return n
}

func (v *versionedMirror) Put(_ context.Context, key string, r io.Reader, _ int64, overwrite bool) (gateway.ObjectInfo, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return gateway.ObjectInfo{}, err
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if _, ok := v.live(key); ok && !overwrite {
		return gateway.ObjectInfo{}, gateway.ErrExists
	}

	v.objs[key] = append(v.objs[key], mirrorVersion{data: b, mod: time.Now()})

	return gateway.ObjectInfo{Key: key, Size: int64(len(b))}, nil
}

func (v *versionedMirror) Get(_ context.Context, key string, _, _ int64) (io.ReadCloser, gateway.ObjectInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	x, ok := v.live(key)
	if !ok {
		return nil, gateway.ObjectInfo{}, gateway.ErrNotFound
	}

	return io.NopCloser(bytes.NewReader(x.data)), gateway.ObjectInfo{Key: key, Size: int64(len(x.data))}, nil
}

func (v *versionedMirror) Head(ctx context.Context, key string) (gateway.ObjectInfo, error) {
	_, info, err := v.Get(ctx, key, 0, -1)
	return info, err
}

func (v *versionedMirror) List(_ context.Context, prefix, after string, max int) ([]gateway.ObjectInfo, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	var keys []string

	for k := range v.objs {
		if _, ok := v.live(k); ok && strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}

	sort.Strings(keys)

	truncated := len(keys) > max
	if truncated {
		keys = keys[:max]
	}

	out := make([]gateway.ObjectInfo, 0, len(keys))
	for _, k := range keys {
		x, _ := v.live(k)
		out = append(out, gateway.ObjectInfo{Key: k, Size: int64(len(x.data)), LastModified: x.mod})
	}

	return out, truncated, nil
}

func (v *versionedMirror) Delete(_ context.Context, key string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if _, ok := v.live(key); !ok {
		return gateway.ErrNotFound
	}

	v.objs[key] = append(v.objs[key], mirrorVersion{marker: true, mod: time.Now()})

	return nil
}

func (v *versionedMirror) Versioned(context.Context) bool { return !v.unversioned }

// versionedFixture is a mirror fixture whose bucket is a versionedMirror.
func versionedFixture(t *testing.T) (*mirrorFixture, *versionedMirror) {
	t.Helper()

	f := newMirrorFixture(t, nil)
	seedAgents(t, f.st, "dev1")

	vm := &versionedMirror{objs: map[string][]mirrorVersion{}}
	openMirror = func(context.Context, store.Target, mirrorCreds) (gateway.ObjectStore, error) {
		return vm, nil
	}

	return f, vm
}

func TestMirrorHidesOnlyWhatMaintenanceRemoved(t *testing.T) {
	f, vm := versionedFixture(t)

	ancient := time.Now().AddDate(-5, 0, 0)

	f.write(t, f.dir, "dev1/p001", "dev1/p001")
	f.write(t, f.dir, "dev1/p002", "dev1/p002")
	vm.seed("dev1/p001", ancient) // live and very old: must stay
	vm.seed("dev1/p002", time.Now())
	vm.seed("dev1/p003", ancient) // removed by maintenance locally

	detail, err := f.run(t)
	require.NoError(t, err)
	require.Equal(t, "0 objects, 0 bytes, 2 skipped, 1 hidden", detail)

	gone := vm.versions("dev1/p003")
	require.Len(t, gone, 2, "the version is retained under the marker")
	require.False(t, gone[0].marker)
	require.Equal(t, "dev1/p003", string(gone[0].data))
	require.True(t, gone[1].marker)

	for _, k := range []string{"dev1/p001", "dev1/p002"} {
		vs := vm.versions(k)
		require.Len(t, vs, 1, k)
		require.False(t, vs[0].marker, k)
	}

	// Idempotent: a second run hides nothing new.
	detail, err = f.run(t)
	require.NoError(t, err)
	require.Equal(t, "0 objects, 0 bytes, 2 skipped", detail)
	require.Equal(t, 1, vm.markers())
}

func TestMirrorHideGuard(t *testing.T) {
	for _, tc := range []struct {
		remote, local int
		trip          bool
	}{
		{remote: 100, local: 80, trip: false},  // 20 = max(20, 10)
		{remote: 100, local: 79, trip: true},   // 21 > 20
		{remote: 300, local: 270, trip: false}, // 30 = 10% of 300
		{remote: 300, local: 269, trip: true},  // 31 > 30
	} {
		t.Run(fmt.Sprintf("%d-of-%d", tc.remote-tc.local, tc.remote), func(t *testing.T) {
			f, vm := versionedFixture(t)

			for i := range tc.remote {
				k := fmt.Sprintf("dev1/p%04d", i)
				vm.seed(k, time.Now())

				if i < tc.local {
					f.write(t, f.dir, k, k)
				}
			}

			detail, err := f.run(t)

			n := tc.remote - tc.local
			if tc.trip {
				require.Error(t, err)
				require.Contains(t, detail, fmt.Sprintf("hide guard tripped: %d of %d", n, tc.remote))
				require.Zero(t, vm.markers(), "a tripped guard hides nothing")

				return
			}

			require.NoError(t, err)
			require.Contains(t, detail, fmt.Sprintf("%d hidden", n))
			require.Equal(t, n, vm.markers())
		})
	}
}

func TestMirrorHidesNothingForAnEmptyLocalStore(t *testing.T) {
	f, vm := versionedFixture(t)

	vm.seed("dev1/p001", time.Now())
	vm.seed("dev1/p002", time.Now())

	// The disk is wiped (or unmounted): no device directory at all.
	detail, err := f.run(t)
	require.NoError(t, err)
	require.Equal(t, "0 objects, 0 bytes, 0 skipped", detail)
	require.Zero(t, vm.markers())
}

func TestMirrorHidesNothingWhenTheLocalListingFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads an unreadable directory")
	}

	f, vm := versionedFixture(t)

	f.write(t, f.dir, "dev1/p001", "dev1/p001")
	vm.seed("dev1/p001", time.Now())
	vm.seed("dev1/p002", time.Now())

	dev := filepath.Join(f.dir, "dev1")
	require.NoError(t, os.Chmod(dev, 0))
	t.Cleanup(func() { _ = os.Chmod(dev, 0o700) })

	detail, err := f.run(t)
	require.Error(t, err)
	require.Contains(t, detail, "listing the hosted root")
	require.Zero(t, vm.markers())
}

func TestMirrorRefusesToHideOnAnUnversionedBucket(t *testing.T) {
	f, vm := versionedFixture(t)
	vm.unversioned = true

	f.write(t, f.dir, "dev1/p001", "dev1/p001")
	vm.seed("dev1/p001", time.Now())
	vm.seed("dev1/p002", time.Now())

	detail, err := f.run(t)
	require.Error(t, err)
	require.Contains(t, detail, "mirror bucket is not versioned; refusing to hide")
	require.Zero(t, vm.markers())
	require.Len(t, vm.versions("dev1/p002"), 1)
}

func TestMirrorHideGuardSumsTheLastWeek(t *testing.T) {
	f, vm := versionedFixture(t)

	// 100 mirrored keys: the limit is max(20, 10) = 20 per rolling week.
	for i := range 100 {
		k := fmt.Sprintf("dev1/p%04d", i)
		vm.seed(k, time.Now())
		f.write(t, f.dir, k, k)
	}

	// Maintenance removes 8 blobs a night. Two nights fit (16), the third
	// would make 24 and trips the guard.
	removed := 0
	for night := 1; night <= 3; night++ {
		for range 8 {
			require.NoError(t, os.Remove(filepath.Join(f.dir, "dev1", fmt.Sprintf("p%04d", removed))))
			removed++
		}

		detail, err := f.run(t)
		if night < 3 {
			require.NoError(t, err, detail)
			require.Equal(t, 8*night, vm.markers())

			continue
		}

		require.Error(t, err)
		require.Contains(t, detail, "hide guard tripped: 8 of 84 (16 more in the last 7 days)")
		require.Equal(t, 16, vm.markers(), "the tripped run hides nothing")
	}
}
