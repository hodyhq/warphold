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
	"log"
	"os"
	"strings"
	"syscall"
)

const (
	dirMode  fs.FileMode = 0o700
	fileMode fs.FileMode = 0o600

	// DefaultMaxObjectSize bounds a single object. Kopia's S3 backend uploads
	// whole blobs and its blobs are tens of megabytes; this is the ceiling that
	// keeps a device from filling the disk with one request.
	DefaultMaxObjectSize int64 = 64 << 20

	// maxKeys matches S3's cap for ListObjectsV2.
	maxKeys = 1000
)

// LocalOptions configures NewLocal. The zero value is the supported default.
type LocalOptions struct {
	// MaxObjectSize is the largest object Put will store, in bytes.
	// Zero means DefaultMaxObjectSize.
	MaxObjectSize int64

	// SkipTempSweep leaves abandoned partial writes alone. Set it whenever
	// this is not the only store open on the root: the sweep cannot tell a
	// crashed write from one another process is making right now.
	SkipTempSweep bool
}

// local is the Fleet-disk backend (§4.3). Every path goes through an os.Root,
// so a key can neither traverse out of the root nor be walked out of it through
// a symlink, whatever the caller passed.
type local struct {
	root    *os.Root
	dir     string
	maxSize int64
}

// NewLocal returns an ObjectStore rooted at dir (0700, files 0600).
//
// The layout is flat, one directory per device: <dir>/<device-id>/<blob-name>
// (§4.3), plus the reserved <dir>/.tmp for partial writes, which NewLocal
// sweeps on start.
//
// It assumes Linux on a case-sensitive filesystem, which is what the Fleet
// server runs on. Device ids are lowercase base32, so two devices can never
// collide by case even on a case-insensitive filesystem, but list ordering and
// the ETag xattr are only guaranteed on the supported platform - the ETag falls
// back to hashing on read wherever xattrs are unavailable.
func NewLocal(dir string, opts LocalOptions) (ObjectStore, error) {
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

	l := &local{root: root, dir: dir, maxSize: opts.MaxObjectSize}
	if l.maxSize <= 0 {
		l.maxSize = DefaultMaxObjectSize
	}

	if n := l.sweepTemp(opts.SkipTempSweep); n > 0 {
		log.Printf("warphold gateway: removed %d abandoned temp file(s) from %s/%s", n, dir, tmpDir)
	}

	return l, nil
}

// sweepTemp removes partial writes left by a crash. Nothing links to them: a
// temp file is only ever named by the Put that is writing it.
func (l *local) sweepTemp(skip bool) int {
	if skip {
		return 0
	}

	ents, err := fs.ReadDir(l.root.FS(), tmpDir)
	if err != nil {
		return 0
	}

	var n int

	for _, e := range ents {
		if err := l.root.RemoveAll(tmpDir + "/" + e.Name()); err == nil {
			n++
		}
	}

	return n
}

func (l *local) Put(ctx context.Context, key string, r io.Reader, size int64, overwrite bool) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}

	if err := checkKey(key); err != nil {
		return ObjectInfo{}, err
	}

	if size > l.maxSize {
		return ObjectInfo{}, fmt.Errorf("%w: declared %d bytes, limit %d", ErrTooLarge, size, l.maxSize)
	}

	tmp, f, err := l.createTemp()
	if err != nil {
		return ObjectInfo{}, err
	}

	// Removing the temp is right on every path: the error paths abandon it, the
	// link path leaves it behind on purpose, and after a rename it is gone.
	defer l.root.Remove(tmp) //nolint:errcheck // best-effort cleanup

	etag, err := writeTemp(ctx, f, r, size, l.maxSize)
	if err != nil {
		return ObjectInfo{}, err
	}

	device, _, _ := strings.Cut(key, "/")
	if err := l.root.MkdirAll(device, dirMode); err != nil {
		return ObjectInfo{}, mapKeyErr(key, fmt.Errorf("creating directory for %q: %w", key, err))
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
		return ObjectInfo{}, mapKeyErr(key, fmt.Errorf("storing %q: %w", key, err))
	}

	l.syncDir(device)

	st, err := l.root.Stat(key)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("stat %q: %w", key, err)
	}

	return ObjectInfo{Key: key, Size: st.Size(), ETag: etag, LastModified: st.ModTime()}, nil
}

// mapKeyErr turns a filesystem complaint about the name itself into ErrBadKey,
// so a key the kernel refuses is a 400 and not a 500.
func mapKeyErr(key string, err error) error {
	if errors.Is(err, syscall.ENAMETOOLONG) {
		return fmt.Errorf("%w: %q is too long for this filesystem", ErrBadKey, key)
	}

	return err
}

// lstatObject is the one place that decides whether a key names an object: a
// symlink, a directory or a device node is not one, and a name the filesystem
// itself rejects is a bad key rather than a missing object.
func (l *local) lstatObject(key string) (os.FileInfo, error) {
	st, err := l.root.Lstat(key)
	if err != nil {
		if errors.Is(err, syscall.ENAMETOOLONG) {
			return nil, mapKeyErr(key, err)
		}

		return nil, ErrNotFound
	}

	if !st.Mode().IsRegular() {
		return nil, ErrNotFound
	}

	return st, nil
}

// createTemp opens a fresh temp file under tmpDir and returns its key-space name.
func (l *local) createTemp() (string, *os.File, error) {
	name := tmpDir + "/" + rand.Text()

	f, err := l.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return "", nil, fmt.Errorf("creating temp file: %w", err)
	}

	return name, f, nil
}

// writeTemp streams r into f, hashing as it goes, records the hash as an xattr
// and fsyncs before returning. It closes f in every case.
func writeTemp(ctx context.Context, f *os.File, r io.Reader, size, maxSize int64) (etag string, err error) {
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()

	// One byte past the limit is enough to catch a reader that delivers more
	// than it promised, without ever reading an unbounded body.
	limit := maxSize
	if size >= 0 && size < limit {
		limit = size
	}

	h := md5.New() //nolint:gosec // ETag, not a security control.

	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(ctxReader{ctx, r}, limit+1))
	if err != nil {
		return "", fmt.Errorf("writing object: %w", err)
	}

	switch {
	case n > maxSize:
		return "", fmt.Errorf("%w: over %d bytes", ErrTooLarge, maxSize)
	case size >= 0 && n != size:
		return "", fmt.Errorf("%w: declared %d bytes, got %d", ErrIncompleteBody, size, n)
	}

	etag = hex.EncodeToString(h.Sum(nil))

	// Best effort: without the xattr, Head hashes the object on read instead.
	setETag(f.Fd(), etag) //nolint:errcheck // documented fallback

	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("syncing object: %w", err)
	}

	return etag, nil
}

// ctxReader stops a copy when the context is done.
//
// ponytail: checks between reads, so it unblocks a slow stream rather than a
// stuck one. The HTTP server closes the request body on cancellation, which is
// what actually interrupts a hung client.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}

	return c.r.Read(p)
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

	etag, err := getETag(f.Fd())
	if err != nil || !isMD5Hex(etag) {
		// The xattr is missing, corrupt, or unsupported by this filesystem, so
		// fall back to hashing the object.
		h := md5.New() //nolint:gosec // ETag, not a security control.
		if _, err := io.Copy(h, ctxReader{ctx, f}); err != nil {
			return ObjectInfo{}, fmt.Errorf("hashing %q: %w", key, err)
		}

		etag = hex.EncodeToString(h.Sum(nil))
	}

	return ObjectInfo{Key: key, Size: st.Size(), ETag: etag, LastModified: st.ModTime()}, nil
}

func isMD5Hex(s string) bool {
	if len(s) != hex.EncodedLen(md5.Size) {
		return false
	}

	_, err := hex.DecodeString(s)

	return err == nil
}

func (l *local) List(ctx context.Context, prefix, after string, max int) ([]ObjectInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	if max <= 0 || max > maxKeys {
		max = maxKeys
	}

	dirs, name, err := l.listDirs(prefix)
	if err != nil {
		return nil, false, err
	}

	objs := make([]ObjectInfo, 0, min(max, 64))

	for _, d := range dirs {
		// ReadDir sorts by name, so one device's keys come out in key order and
		// the page can stop as soon as it is full. Across devices the order
		// holds because device ids are lowercase base32 (see NewLocal).
		ents, err := fs.ReadDir(l.root.FS(), d)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			return nil, false, fmt.Errorf("listing %q: %w", prefix, err)
		}

		for _, e := range ents {
			key := d + "/" + e.Name()
			if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), name) || key <= after {
				continue
			}

			info, err := e.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue // raced with a concurrent Delete
				}

				return nil, false, fmt.Errorf("listing %q: %w", prefix, err)
			}

			objs = append(objs, ObjectInfo{Key: key, Size: info.Size(), LastModified: info.ModTime()})
			if len(objs) > max {
				return objs[:max], true, nil
			}
		}
	}

	return objs, false, nil
}

// listDirs resolves a prefix to the device directories a list must read and the
// blob-name prefix within them. A prefix with a '/' names one device, so a list
// touches exactly one directory; without one it selects device directories by
// name, which is the internal "every device" case.
func (l *local) listDirs(prefix string) (dirs []string, name string, err error) {
	if device, blob, found := strings.Cut(prefix, "/"); found {
		if device == "" || device == "." || device == ".." || device == tmpDir || strings.Contains(device, `\`) {
			return nil, "", fmt.Errorf("%w: prefix %q", ErrBadKey, prefix)
		}

		return []string{device}, blob, nil
	}

	ents, err := fs.ReadDir(l.root.FS(), ".")
	if err != nil {
		return nil, "", fmt.Errorf("listing %q: %w", prefix, err)
	}

	for _, e := range ents {
		if e.IsDir() && e.Name() != tmpDir && strings.HasPrefix(e.Name(), prefix) {
			dirs = append(dirs, e.Name())
		}
	}

	return dirs, "", nil
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
	if _, err := l.lstatObject(key); err != nil {
		return err
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

// Close releases the root directory handle. It is not part of ObjectStore:
// only a caller that opened its own store (blob.NewStorage, via the hosted
// adapter) may close one, never a shared handle out of the gateway's cache.
func (l *local) Close() error { return l.root.Close() }

// open validates key and opens it as an object. It Lstats first, so a symlink
// is ErrNotFound rather than something read through, and re-checks the open
// file, so a directory or a device node cannot be read either.
func (l *local) open(key string) (*os.File, os.FileInfo, error) {
	if err := checkKey(key); err != nil {
		return nil, nil, err
	}

	if _, err := l.lstatObject(key); err != nil {
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
