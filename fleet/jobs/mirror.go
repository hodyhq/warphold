package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
)

const (
	// listPage bounds one page of a local or remote listing, so a device with a
	// million blobs does not become a million-element response.
	listPage = 1000

	// detailErrors is how many device failures the job's detail names; the rest
	// are counted. The row is read in the UI, not in a log.
	detailErrors = 3

	// hideGuardMin and hideGuardPercent bound how many mirror objects one run
	// may hide for one device: more than max(hideGuardMin, hideGuardPercent% of
	// the device's mirrored keys) looks like a damaged local store, not
	// maintenance, so the run hides nothing for that device and fails.
	//
	// ponytail: constants, not settings; make them fleet settings if a real
	// maintenance run (a large compaction) ever trips the guard.
	hideGuardMin     = 20
	hideGuardPercent = 10

	// hideWindow is how far back the guard sums earlier runs' hides, so a
	// slow erosion (a little every night) trips it as surely as one big run.
	hideWindow = 7 * 24 * time.Hour
)

// mirrorCreds is the shape sealed into targets.sealed_mirror_key by
// fleet/api's sealCreds.
type mirrorCreds struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key"`
}

// openMirror opens the cloud store for a target's mirror bucket. It is a
// package var so a test can point it at a fake endpoint; production always
// builds the connection info with mirrorCI.
var openMirror = func(ctx context.Context, t store.Target, c mirrorCreds) (gateway.ObjectStore, error) {
	ci, err := mirrorCI(t, c)
	if err != nil {
		return nil, err
	}

	return gateway.NewCloud(ctx, ci, "")
}

// mirrorCI builds the S3-compatible connection info for a mirror bucket.
// B2 is reached through its S3 endpoint, never its native API: the cloud
// backend needs a conditional PUT, which B2's own API does not have.
func mirrorCI(t store.Target, c mirrorCreds) (blob.ConnectionInfo, error) {
	var endpoint string

	switch {
	case t.MirrorBucket == "":
		return blob.ConnectionInfo{}, errors.New("mirror bucket is not set")
	case t.MirrorRegion == "":
		// Without a region minio guesses, which costs a redirect per request or
		// reaches the wrong endpoint entirely.
		return blob.ConnectionInfo{}, errors.New("mirror region is not set")
	case c.KeyID == "" || c.Key == "":
		return blob.ConnectionInfo{}, errors.New("mirror credentials are not set")
	}

	switch t.MirrorKind {
	case "b2":
		endpoint = "s3." + t.MirrorRegion + ".backblazeb2.com"
	case "s3":
		endpoint = "s3." + t.MirrorRegion + ".amazonaws.com"
	default:
		return blob.ConnectionInfo{}, fmt.Errorf("unsupported mirror kind %q", t.MirrorKind)
	}

	return blob.ConnectionInfo{Type: "s3", Config: &s3.Options{
		BucketName:      t.MirrorBucket,
		Region:          t.MirrorRegion,
		Endpoint:        endpoint,
		AccessKeyID:     c.KeyID,
		SecretAccessKey: c.Key,
	}}, nil
}

// Mirror returns the runner for the "mirror" job: for every hosted disk target
// with a mirror configured, upload every local object the mirror does not
// already hold (spec §7.3), then hide the mirror objects maintenance removed
// locally (see mirrorRun.hide). No version is ever deleted: hiding is a plain
// DeleteObject on a versioned bucket, which only adds a delete marker, and the
// bucket's lifecycle rule decides when hidden versions go. A failure on one
// device continues with the next.
func Mirror(st *store.Store, open seal.Opener) Runner {
	return func(ctx context.Context, j store.Job) (string, error) {
		m := &mirrorRun{st: st, open: open, now: time.Now()}

		targets, err := st.Targets(ctx)
		if err != nil {
			return "", fmt.Errorf("listing targets: %w", err)
		}

		for i := range targets {
			if err := ctx.Err(); err != nil {
				// Shutting down or out of time: stop here rather than opening
				// a provider connection per remaining target only to fail -
				// but say so, or a partial mirror would report itself ok.
				m.fail("mirror", err)

				break
			}

			t := targets[i]
			if t.Kind != "hosted" || t.StorageMode != "disk" || t.MirrorKind == "" {
				continue
			}

			m.target(ctx, t)
		}

		if len(m.errs) > 0 {
			return m.detail(), errors.New("the mirror did not complete")
		}

		return m.detail(), nil
	}
}

// mirrorRun accumulates one job's counters across every mirrored target.
type mirrorRun struct {
	st   *store.Store
	open seal.Opener
	now  time.Time

	local, remote gateway.ObjectStore

	// tgt is the target being mirrored, and versioned is what its bucket
	// answered to GetBucketVersioning. A hide is only a hide on a versioned
	// bucket; anywhere else DeleteObject destroys the only copy.
	tgt       store.Target
	versioned bool

	// plain is set for a mirror bucket whose provider has no conditional write
	// (B2's S3 endpoint answers 501 to If-None-Match at all, so every upload
	// would fail). Safe here and only here: this job is the bucket's only
	// writer, it lists the mirror before it uploads and skips what is already
	// there, and the bucket's Object Lock keeps any superseded version. A
	// cloud-direct target, where the devices write, still requires the
	// conditional put and is refused without it.
	plain bool

	objects int
	skipped int
	hidden  int
	bytes   int64
	errs    []string
}

func (m *mirrorRun) fail(what string, err error) {
	m.errs = append(m.errs, what+": "+err.Error())
}

func (m *mirrorRun) detail() string {
	d := fmt.Sprintf("%d objects, %d bytes, %d skipped", m.objects, m.bytes, m.skipped)
	if m.hidden > 0 {
		d += fmt.Sprintf(", %d hidden", m.hidden)
	}

	if len(m.errs) == 0 {
		return d
	}

	shown := m.errs
	if len(shown) > detailErrors {
		shown = shown[:detailErrors]
	}

	d += ", errors: " + strings.Join(shown, "; ")
	if n := len(m.errs) - len(shown); n > 0 {
		d += fmt.Sprintf(" (+%d more)", n)
	}

	return d
}

// target mirrors one hosted disk target.
func (m *mirrorRun) target(ctx context.Context, t store.Target) {
	if t.MirrorLockVerifiedAt == nil {
		// Object Lock is verified when the mirror is configured. Until it is,
		// no device data goes near the bucket.
		m.fail(t.Name, errors.New("mirror not verified"))

		return
	}

	var c mirrorCreds

	plain, err := m.open(t.SealedMirrorKey)
	if err != nil {
		m.fail(t.Name, errors.New("unsealing the mirror credentials failed"))

		return
	}

	if err := json.Unmarshal(plain, &c); err != nil {
		m.fail(t.Name, errors.New("the sealed mirror credentials are malformed"))

		return
	}

	local, err := gateway.NewLocal(t.Path, gateway.LocalOptions{})
	if err != nil {
		m.fail(t.Name, err)

		return
	}

	defer closeStore(ctx, local)

	remote, err := openMirror(ctx, t, c)
	if err != nil {
		// openMirror surfaces gateway/cloud.go messages verbatim ("needs the
		// bucket's region"); prefix so the noun in the jobs UI is right.
		m.fail(t.Name, fmt.Errorf("mirror bucket: %w", err))

		return
	}

	defer closeStore(ctx, remote)

	m.local, m.remote, m.tgt = local, remote, t
	m.plain = t.MirrorConditionalPut != nil && !*t.MirrorConditionalPut
	m.versioned = remote.Versioned(ctx)

	defer func() {
		m.local, m.remote, m.plain, m.tgt, m.versioned = nil, nil, false, store.Target{}, false
	}()

	if err := m.walk(ctx, t.Name); err != nil {
		m.fail(t.Name, err)
	}
}

// walk pages the target's local objects and mirrors them one device at a time.
// Keys sort as "<device-id>/<blob-name>", so a device's objects are contiguous
// and the run holds only one device's listing at a time.
//
// ponytail: one device's keys in memory (~100 bytes each); page them to disk
// only if a single device ever holds millions of blobs.
func (m *mirrorRun) walk(ctx context.Context, target string) error {
	var (
		after, device string
		objs          []gateway.ObjectInfo
	)

	for {
		page, truncated, err := m.local.List(ctx, "", after, listPage)
		if err != nil {
			return fmt.Errorf("listing the hosted root: %w", err)
		}

		if len(page) == 0 {
			break
		}

		for _, o := range page {
			d, _, ok := strings.Cut(o.Key, "/")
			if !ok || d == "" {
				continue
			}

			if d != device {
				m.device(ctx, target, device, objs)

				device, objs = d, nil
			}

			objs = append(objs, o)
		}

		after = page[len(page)-1].Key

		if !truncated {
			break
		}
	}

	m.device(ctx, target, device, objs)

	return nil
}

// device uploads one device's missing objects, hides the ones maintenance
// removed, and records its offsite progress. objs is the device's complete
// local listing: walk calls this only once a device's keys are all read, and
// never for a device with none, so an empty or failed listing hides nothing.
func (m *mirrorRun) device(ctx context.Context, target, device string, objs []gateway.ObjectInfo) {
	if device == "" || len(objs) == 0 {
		return
	}

	where := target + "/" + device

	have, err := m.mirrored(ctx, device)
	if err != nil {
		m.fail(where, err)

		return
	}

	// offsite is what this device now has in the mirror, not what this run
	// uploaded: it is the number the "offsite" tile reports.
	var offsite int64

	for _, o := range objs {
		if err := ctx.Err(); err != nil {
			m.fail(where, err)

			return
		}

		if _, ok := have[o.Key]; ok {
			m.skipped++
			offsite += o.Size

			continue
		}

		n, err := m.upload(ctx, o)

		switch {
		case errors.Is(err, gateway.ErrExists):
			// The mirror already had it - a listing raced an earlier upload.
			m.skipped++
			offsite += o.Size
		case err != nil:
			// One device's failure must not cost the rest of the fleet its run.
			m.fail(where, err)

			return
		default:
			m.objects++
			m.bytes += n
			offsite += n
		}
	}

	m.hide(ctx, where, device, objs, have)
	m.record(ctx, where, device, offsite)
}

// hide adds a delete marker for every mirror key the local store no longer
// holds. Live keys are never touched, however old. It fails closed: on a
// bucket that is not lock-verified and versioned, or once this run plus the
// last hideWindow of runs would pass the guard, the device hides nothing and
// the run fails, so the digest flags it.
func (m *mirrorRun) hide(ctx context.Context, where, device string, objs []gateway.ObjectInfo, have map[string]struct{}) {
	local := make(map[string]struct{}, len(objs))
	for _, o := range objs {
		local[o.Key] = struct{}{}
	}

	var gone []string

	for k := range have {
		if _, ok := local[k]; !ok {
			gone = append(gone, k)
		}
	}

	if len(gone) == 0 {
		return
	}

	if m.tgt.MirrorLockVerifiedAt == nil || !m.versioned {
		m.fail(where, errors.New("mirror bucket is not versioned; refusing to hide"))

		return
	}

	earlier, err := m.st.MirrorHidesSince(ctx, m.tgt.ID, device, m.now.Add(-hideWindow))
	if err != nil {
		m.fail(where, fmt.Errorf("reading earlier hides: %w", err))

		return
	}

	if limit := max(hideGuardMin, len(have)*hideGuardPercent/100); earlier+len(gone) > limit {
		m.fail(where, fmt.Errorf("hide guard tripped: %d of %d (%d more in the last 7 days)", len(gone), len(have), earlier))

		return
	}

	sort.Strings(gone)

	n := 0

	defer func() {
		if n == 0 {
			return
		}

		m.hidden += n
		if err := m.st.AddMirrorHides(ctx, m.tgt.ID, device, m.now, m.now.Add(-hideWindow), n); err != nil {
			m.fail(where, fmt.Errorf("recording hides: %w", err))
		}
	}()

	for _, k := range gone {
		err := m.remote.Delete(ctx, k)

		switch {
		case errors.Is(err, gateway.ErrNotFound):
			// Already hidden: a listing raced an earlier run.
		case err != nil:
			m.fail(where, fmt.Errorf("hiding %s: %w", k, err))

			return
		default:
			n++
		}
	}
}

// record writes the device's offsite progress, if the device is an agent this
// fleet knows: repo_stats.agent_id has a foreign key to agents, so a directory
// left behind by a removed agent has nowhere to record and is not an error.
func (m *mirrorRun) record(ctx context.Context, where, device string, offsite int64) {
	if _, err := m.st.Agent(ctx, device); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			m.fail(where, err)
		}

		return
	}

	if err := m.st.SetMirrored(ctx, device, m.now, offsite); err != nil {
		m.fail(where, err)
	}
}

// mirrored is the set of keys the mirror already holds for one device.
func (m *mirrorRun) mirrored(ctx context.Context, device string) (map[string]struct{}, error) {
	have := map[string]struct{}{}
	after := ""

	for {
		page, truncated, err := m.remote.List(ctx, device+"/", after, listPage)
		if err != nil {
			return nil, fmt.Errorf("listing the mirror: %w", err)
		}

		if len(page) == 0 {
			return have, nil
		}

		for _, o := range page {
			have[o.Key] = struct{}{}
		}

		after = page[len(page)-1].Key

		if !truncated {
			return have, nil
		}
	}
}

// upload streams one object to the mirror. overwrite is false wherever the
// provider can enforce it, so a key the listing missed is refused rather than
// rewritten; see mirrorRun.plain for the providers that cannot.
func (m *mirrorRun) upload(ctx context.Context, o gateway.ObjectInfo) (int64, error) {
	rc, info, err := m.local.Get(ctx, o.Key, 0, -1)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", o.Key, err)
	}

	defer rc.Close() //nolint:errcheck // read-only handle

	put, err := m.remote.Put(ctx, o.Key, rc, info.Size, m.plain)
	if err != nil {
		if errors.Is(err, gateway.ErrExists) {
			return 0, err
		}

		return 0, fmt.Errorf("uploading %s: %w", o.Key, err)
	}

	return put.Size, nil
}

func closeStore(ctx context.Context, s gateway.ObjectStore) {
	c, ok := s.(interface{ Close(context.Context) error })
	if !ok {
		return
	}

	if err := c.Close(ctx); err != nil {
		log.Printf("warphold fleet: closing a mirror store: %v", err)
	}
}
