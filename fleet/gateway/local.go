package gateway

import (
	"context"
	"crypto/md5" //nolint:gosec // S3 defines ETag as MD5; it is an identifier here, not a security control.
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

const (
	dirMode  fs.FileMode = 0o700
	fileMode fs.FileMode = 0o600

	// tmpDir holds partially written objects. It lives at the root so it can
	// never collide with a device key, which is always confined to
	// "<device-id>/", and it is on the same filesystem so link/rename work.
	tmpDir = ".tmp"

	// defaultMaxKeys matches S3's cap for ListObjectsV2.
	defaultMaxKeys = 1000
)

// local is the Fleet-disk backend (§4.3). Every path goes through an os.Root,
// so a key can neither traverse out of the root nor be walked out of it through
// a symlink, whatever the caller passed.
type local struct {
	root *os.Root
	dir  string
}

// NewLocal returns an ObjectStore rooted at dir (0700, files 0600).
func NewLocal(dir string) (ObjectStore, error) {
	if dir == "" {
		return nil, errors.New("gateway: local store needs a directory")
	}

	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}

	// MkdirAll is a no-op on an existing directory, which may have been created
	// with looser modes; the service user's blobs must not be world-readable.
	if err := os.Chmod(dir, dirMode); err != nil {
		return nil, fmt.Errorf("tightening %s: %w", dir, err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", dir, err)
	}

	if err := root.MkdirAll(tmpDir, dirMode); err != nil {
		root.Close() //nolint:errcheck // best effort on the error path

		return nil, fmt.Errorf("creating %s/%s: %w", dir, tmpDir, err)
	}

	return &local{root: root, dir: dir}, nil
}

func (l *local) Put(ctx context.Context, key string, r io.Reader, size int64, overwrite bool) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}

	if err := checkKey(key); err != nil {
		return ObjectInfo{}, err
	}

	tmp, f, err := l.createTemp()
	if err != nil {
		return ObjectInfo{}, err
	}

	// Removing the temp is right on every path: the error paths abandon it, the
	// link path leaves it behind on purpose, and after a rename it is gone.
	defer l.root.Remove(tmp) //nolint:errcheck // best-effort cleanup

	etag, err := writeTemp(f, r, size)
	if err != nil {
		return ObjectInfo{}, err
	}

	if dir := path.Dir(key); dir != "." {
		if err := l.root.MkdirAll(dir, dirMode); err != nil {
			return ObjectInfo{}, fmt.Errorf("creating directory for %q: %w", key, err)
		}
	}

	if overwrite {
		err = l.root.Rename(tmp, key)
	} else {
		// link(2) fails with EEXIST, so the append-only rule is enforced by the
		// kernel rather than by a check-then-write race.
		err = l.root.Link(tmp, key)
		if errors.Is(err, fs.ErrExist) {
			return ObjectInfo{}, ErrExists
		}
	}

	if err != nil {
		return ObjectInfo{}, fmt.Errorf("storing %q: %w", key, err)
	}

	l.syncDir(path.Dir(key))

	st, err := l.root.Stat(key)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("stat %q: %w", key, err)
	}

	return ObjectInfo{Key: key, Size: st.Size(), ETag: etag, LastModified: st.ModTime()}, nil
}

// createTemp opens a fresh temp file under tmpDir and returns its key-space name.
func (l *local) createTemp() (string, *os.File, error) {
	name := path.Join(tmpDir, rand.Text())

	f, err := l.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return "", nil, fmt.Errorf("creating temp file: %w", err)
	}

	return name, f, nil
}

// writeTemp streams r into f, hashing as it goes, and fsyncs before returning.
// It closes f in every case.
func writeTemp(f *os.File, r io.Reader, size int64) (etag string, err error) {
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()

	src := r
	if size >= 0 {
		// One byte past the declared size is enough to catch a reader that
		// delivers more than it promised without reading an unbounded body.
		src = io.LimitReader(r, size+1)
	}

	h := md5.New() //nolint:gosec // ETag, not a security control.

	n, err := io.Copy(io.MultiWriter(f, h), src)
	if err != nil {
		return "", fmt.Errorf("writing object: %w", err)
	}

	if size >= 0 && n != size {
		return "", fmt.Errorf("%w: declared %d bytes, got %d", ErrBadKey, size, n)
	}

	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("syncing object: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// syncDir flushes a directory entry so a crash cannot lose an object that Put
// already reported as stored. Best effort: not every filesystem allows it, and
// failing to fsync a directory is not a reason to fail a written object.
func (l *local) syncDir(dir string) {
	d, err := l.root.Open(dir)
	if err != nil {
		return
	}

	d.Sync()  //nolint:errcheck // best effort, see above
	d.Close() //nolint:errcheck // read-only handle
}

func (l *local) Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, ObjectInfo{}, err
	}

	f, st, err := l.open(key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}

	size := st.Size()

	// A range that starts at or past the end is 416; offset 0 is not a range at
	// all, so a zero-byte object still reads cleanly.
	if offset < 0 || (offset > 0 && offset >= size) {
		f.Close() //nolint:errcheck // nothing read yet

		return nil, ObjectInfo{}, fmt.Errorf("%w: offset %d of %d bytes", ErrRange, offset, size)
	}

	// Written as a subtraction so a caller-supplied length cannot overflow.
	if length < 0 || length > size-offset {
		length = size - offset
	}

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close() //nolint:errcheck // nothing read yet

			return nil, ObjectInfo{}, fmt.Errorf("seeking %q: %w", key, err)
		}
	}

	info := ObjectInfo{Key: key, Size: size, LastModified: st.ModTime()}

	return readCloser{Reader: io.LimitReader(f, length), Closer: f}, info, nil
}

func (l *local) Head(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}

	f, st, err := l.open(key)
	if err != nil {
		return ObjectInfo{}, err
	}

	defer f.Close() //nolint:errcheck // read-only handle

	// ponytail: hashes the object on every HEAD. Objects are immutable, so if
	// HEAD ever turns hot the digest can be cached or stored at Put time.
	h := md5.New() //nolint:gosec // ETag, not a security control.
	if _, err := io.Copy(h, f); err != nil {
		return ObjectInfo{}, fmt.Errorf("hashing %q: %w", key, err)
	}

	return ObjectInfo{
		Key:          key,
		Size:         st.Size(),
		ETag:         hex.EncodeToString(h.Sum(nil)),
		LastModified: st.ModTime(),
	}, nil
}

func (l *local) List(ctx context.Context, prefix, after string, max int) ([]ObjectInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	if max <= 0 || max > defaultMaxKeys {
		max = defaultMaxKeys
	}

	// ponytail: collects the whole prefix, then sorts and slices. One device's
	// blob directory is flat and Kopia lists it whole anyway; if paging a very
	// large prefix ever matters, seek into the sorted directory instead.
	var objs []ObjectInfo

	err := fs.WalkDir(l.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil // raced with a concurrent Delete
		case err != nil:
			return err
		case p == ".":
			return nil
		}

		if d.IsDir() {
			// Descend only into directories that can hold the prefix.
			if p == tmpDir || (!strings.HasPrefix(p+"/", prefix) && !strings.HasPrefix(prefix, p+"/")) {
				return fs.SkipDir
			}

			return nil
		}

		if !d.Type().IsRegular() || !strings.HasPrefix(p, prefix) || p <= after {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return err
		}

		objs = append(objs, ObjectInfo{Key: p, Size: info.Size(), LastModified: info.ModTime()})

		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing %q: %w", prefix, err)
	}

	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })

	if len(objs) > max {
		return objs[:max], true, nil
	}

	return objs, false, nil
}

func (l *local) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := checkKey(key); err != nil {
		return err
	}

	// Lstat, not Stat: only a regular file is an object, and a symlink is never
	// one - so Delete can never remove a directory or follow a link out.
	st, err := l.root.Lstat(key)
	if err != nil || !st.Mode().IsRegular() {
		return ErrNotFound
	}

	if err := l.root.Remove(key); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}

		return fmt.Errorf("deleting %q: %w", key, err)
	}

	return nil
}

// Versioned is false: the disk backend keeps one copy of each object, and
// append-only plus the mirror job is what protects history (§4.3).
func (l *local) Versioned(context.Context) bool { return false }

// open validates key and opens it as an object, so a directory, a device node
// or a dangling name is ErrNotFound rather than a partial success.
func (l *local) open(key string) (*os.File, os.FileInfo, error) {
	if err := checkKey(key); err != nil {
		return nil, nil, err
	}

	f, err := l.root.Open(key)
	if err != nil {
		return nil, nil, ErrNotFound
	}

	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close() //nolint:errcheck // nothing read yet

		return nil, nil, ErrNotFound
	}

	return f, st, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}
