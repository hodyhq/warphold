# WarpHold Plan 3: hosted target, hybrid mirror, installers, recovery kit, jobs, site

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Everything the original spec deferred, in six ordered milestones that each end live-testable: devices back up **to the Fleet server** through an S3-compatible append-only gateway (M1), that storage gets an offsite B2 mirror (M2), both the Fleet server and the single-machine app install from **one command** (M3), every device has a recovery kit that a stock upstream `kopia` binary can use — proven in CI — and the sealing passphrase can be rotated (M4), the server runs verify / test-restore / maintenance / digest on a schedule and the Stored tiles show real numbers (M5), and WarpHold has real screenshots, honest READMEs and a site at **warphold.com** (M6).

**Architecture:** M1 adds `fleet/gateway` — SigV4 verification, per-device keys sealed in the store, an `ObjectStore` with a `local` (disk) backend, and the narrow S3 subset Kopia actually uses — mounted at `/s3/` on the server that already exists, plus a `hosted` target kind, hosted provisioning, and the `public_url` setting everything else hangs off. M2 adds the `cloud` write-through backend and the mirror job. M3 is packaging and shell: goreleaser deb/rpm/signing, `get.warphold.com`, `fleet.sh`, `app.sh`, and the five-step setup wizard. M4 is `fleet/kit`, acks, passphrase rotation and the standalone-restore CI test. M5 is `fleet/jobs` and `fleet/mail`. M6 is the screenshot pipeline, both READMEs and `hodyhq/warphold-site`.

**Tech Stack:** Go 1.26 (mise), `modernc.org/sqlite`, Kopia's own `repo/blob/{s3,b2,filesystem}` on the client side, gorilla/mux, React 19 + Vite + TypeScript + Tailwind v4 (UI repo, published as Go module `github.com/hodyhq/warphold-ui`, currently v0.1.1), goreleaser + nfpms, GitHub Pages + Cloudflare, systemd, POSIX `sh`.

**Spec:** `docs/superpowers/specs/2026-09-02-warphold-plan3-hosted-target-installers-design.md` (the "Plan 3 spec"; §N below means that document). It extends `docs/superpowers/specs/2026-09-01-warphold-core-fleet-design.md` (the "original spec"), whose §4 data model, §5 enrollment, §6 protocol, §7 security, §8 recovery kit, §12 testing, §13 milestones M6–M9 and §15 out-of-scope are the source of everything carried forward here.

## Global Constraints

- **Module path stays `github.com/kopia/kopia`.** The six touched upstream files are already spent: `cli/app.go`, `cli/command_server_start.go`, `Makefile`, `.goreleaser.yml`, `main.go`, `internal/server/htmlui_embed.go`. This plan touches exactly one of them — `.goreleaser.yml` (packaging and signing) — and nothing else upstream. Everything new lives in `fleet/`, `agent/`, `cli/command_fleet_*.go`, `cli/command_agent_*.go`, and new files under `internal/server/` marked `warphold:`. Never edit `repo/`, `snapshot/`, or `internal/` otherwise.
- **New Go dependencies:** none is expected. SigV4 is `crypto/hmac` + `crypto/sha256`; the S3 XML responses are `encoding/xml`; SMTP is `net/smtp` + `crypto/tls`. If a task believes it needs a dependency, it stops and asks, and runs the dependency review (CVE check, `02-vulnerability-scanner`) before any `go get`.
- **`CGO_ENABLED=0` must still build every Linux artifact**, including the deb and rpm.
- **Secrets never in logs, responses, git, or the vault.** The gateway logs the access key id, never the secret; the kit renderer is the only code that returns a repository password over HTTP, and only to an authenticated admin. Homelab hostnames and LAN addresses never appear in either GitHub repo: they live in the vault (`Homelab/WarpHold/WarpHold-Full-Dev-Guide.md`) and in deployment reports. This plan uses the placeholders `<FLEET_HOST>`, `<FLEET_IP>`, `<GATEWAY_IP>`, `<HOMELAB_DOMAIN>`, `<FW16_IP>`. The only real domains permitted in the repos are `warphold.com`, `get.warphold.com` and `warphold.dev`. A private-name grep must be empty before every push:
  ```bash
  grep -rn "hody\.sh\|hody\.dev\|10\.99\." . --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=.superpowers   # expect nothing
  ```
- **CodeRabbit cadence (project rule, verbatim):** once per task on the uncommitted diff (`cr review --agent -t uncommitted`) before committing; re-run **only on fix diffs** (`cr review --agent -t committed --base-commit <sha>`); one whole-branch pass per PR (`cr review --agent --base master`); the GitHub app reviews the PR before merge. **No per-commit gating.** Fix Critical/Warning locally before pushing.
- **Claude task review per task**: after the implementation and before the CodeRabbit run, review the task's own diff against the task's stated interfaces and the spec section it implements.
- **Test-first.** Every task with logic writes the failing test first, in the same commit as the implementation. `go test ./fleet/... ./agent/... ./cli/... -count=1` is green before any commit.
- **Every deployment or live step ends with a real check whose output goes in the task report** (Working Rule #4). Poll, never blind-`sleep`: `until <check>; do sleep 5; done`.
- **PR boundaries:** **PR A** = M1 + M2 (server), opened after Task 15. **PR B** = M3 + M4 + M5 (server), opened after Task 33. **UI PRs** are separate and small, marked at the tasks that need them. **M6 ships as its own PRs** (screenshots + UI README in the UI repo; product README in the main repo; the site in its own new repo) and is never folded into PR A or PR B; it may run in parallel with M4/M5.
- **Vault after every change** (Working Rule #2): the `Homelab/WarpHold/` triad plus an `INGEST` line in `hody/00-Log.md`. Placeholders only.
- **Commit messages as given; no attribution trailers.**

## Carried in from Plan 2

| Item | Closed by |
|---|---|
| `size_bytes` / `stored_bytes` / `dedup_ratio` render as an em dash (`src/pages/fleet/Devices.tsx:232`, `Overview.tsx:144`) | Task 31 |
| `verify` command exists in the protocol but is never executed by the agent | Task 29 |
| B2 real-bucket reconcile (Object Lock verified only in unit tests) | Task 12, HUMAN CHECKPOINT |
| "Turn this machine into a Fleet server" card sits on the Repository page | Task 22 |
| `nfpms` block in `.goreleaser.yml` still carries upstream's Kopia metadata | Task 16 |
| "only on AC" scheduling on the agent | **Deferred** (see the end) |

## File structure

```
fleet/gateway/{sigv4,keys,object,local,cloud,handler,xmlout,limit}.go   (new, M1/M2)
fleet/jobs/{scheduler,verify,testrestore,maintenance,mirror,stats,reap,digest}.go  (new, M2/M5)
fleet/kit/{kit.go,kit.html.tmpl}                                       (new, M4)
fleet/mail/{smtp.go,settings.go}                                       (new, M5)
fleet/store/{schema.sql,migrate.go,device_keys.go,jobs.go,kit_acks.go,repo_stats.go,targets.go}
fleet/api/{gateway_mount.go,admin_targets.go,admin_settings.go,admin_kit.go,admin_jobs.go,publicurl.go}
fleet/enroll/provision.go                                              (hosted path)
cli/command_fleet_activate.go                                          (--public-url)
cli/command_fleet_jobs.go, cli/command_fleet_rotate.go                 (new)
agent/engine/verify.go                                                 (verify command)
.goreleaser.yml                                                        (warphold:)  packaging + signing
scripts/spike-append-only/                                             (Task 1, kept as documentation)
scripts/install/{fleet.sh,app.sh}      the ONLY copy of each install script; shipped as release assets
scripts/{test-fleet-sh.sh,test-app-sh.sh,standalone-restore-test.sh}
docs/superpowers/specs/2026-09-02-…-design.md, docs/superpowers/plans/… (this file)
docs/RECONCILE-append-only.md                                          (Task 1 output, referenced by §14.1)

hodyhq/warphold-ui:
  src/api/{fleet.ts,types.ts}          hosted target, jobs, kit, stats, public_url
  src/pages/fleet/{Targets,Settings,Activate,Device,Devices,Overview}.tsx
  scripts/screenshots.sh + scripts/shots/                              (M6)
  docs/screenshots/                                                    (M6, committed)
  README.md                                                            (M6)

hodyhq/warphold-site:                                                  (M6, new repo)
  index.html, fleet.html, assets/, CNAME        (no get/ - scripts are never copied here)

hody vault: Homelab/WarpHold/{WarpHold-Admin-Overview,WarpHold-Full-Dev-Guide,WarpHold-Issues-and-Fixes}.md
```

---

## M1 — hosted target (branch `plan3-gateway`)

### Task 1: SPIKE — what does Kopia delete or overwrite? (§14.1)

**This task's recorded output is consumed by Task 3** (which `x-amz-content-sha256` forms the verifier must accept), **Task 5** (the `allowDelete` / `allowOverwrite` allowlists) **and Task 9** (which fails on any request outside them). Until it exists, no delete or overwrite rule may be written from a guess.

**Files:**
- Create: `scripts/spike-append-only/main.go` (a throwaway `httptest`-shaped S3 store, kept as documentation), `docs/RECONCILE-append-only.md`

**Interfaces:** none (a spike). The output file is the interface.

- [ ] **Step 1: A minimal S3 store that records everything**

  Write `scripts/spike-append-only/main.go`: an `http.Server` implementing just enough S3 (PUT / GET with Range / HEAD / ListObjectsV2 / DELETE / GetBucketVersioning) over a temp directory, with **no** authentication and **no** restrictions, which appends one line per request to `requests.log`:
  `<verb> <key> exists=<bool> status=<code> content-sha256=<header> len=<n>`.

- [ ] **Step 2: Drive stock Kopia against it**

```bash
cd /home/hody/.dev/Projects/warphold
go run ./scripts/spike-append-only &          # prints its endpoint, e.g. 127.0.0.1:9401
go build -o /tmp/wh-spike .
/tmp/wh-spike repository create s3 --bucket warphold --prefix spike/ \
  --endpoint 127.0.0.1:9401 --disable-tls --access-key spike --secret-access-key spike \
  --password spikepass --config-file /tmp/spike.config
mkdir -p /tmp/spike-src && head -c 30M /dev/urandom > /tmp/spike-src/a.bin && echo hello > /tmp/spike-src/b.txt
/tmp/wh-spike --config-file /tmp/spike.config snapshot create /tmp/spike-src
echo more >> /tmp/spike-src/b.txt
/tmp/wh-spike --config-file /tmp/spike.config snapshot create /tmp/spike-src   # second, incremental
/tmp/wh-spike --config-file /tmp/spike.config snapshot list
/tmp/wh-spike --config-file /tmp/spike.config maintenance run --full
```
  Verify the exact flag names against `cli/storage_s3.go` first (§14.4) — the names above are the expected shape, not a quotation.

- [ ] **Step 3: Record the findings**

  Write `docs/RECONCILE-append-only.md` with, verbatim from `requests.log`:
  1. every `DELETE` Kopia issued, grouped by blob-name class (the prefix character(s) of the blob id: `p`, `q`, `n`, `x`, `s`, `kopia.repository`, …), and which command issued it;
  2. every `PUT` to a key that already existed;
  3. the distinct values of `x-amz-content-sha256` (real digest / `UNSIGNED-PAYLOAD` / `STREAMING-…`) — §14.2;
  4. whether any multipart (`POST ?uploads`) request appeared at all;
  5. the `ListObjectsV2` parameters actually sent.
  Then state, in one table, **the delete allowlist**: the exact key patterns the gateway will permit `DELETE` on, and the exact patterns it will permit overwriting. Anything not in that table is denied.

- [ ] **Step 4: Raise anything that breaks the design**

  If `maintenance run --full` deletes classes that a *device* would also delete, note it — devices run no maintenance (§7.1 step 5), so a delete a device never issues does not need to be allowed. If chunked streaming signatures appear, that is a scoping decision to raise with Hody, **not** to absorb (§14.2).

- [ ] **Step 5: Commit**

```bash
go test ./... -count=1 >/dev/null && cr review --agent -t uncommitted
git add scripts/spike-append-only docs/RECONCILE-append-only.md
git commit -m "spike: record which blobs Kopia deletes or overwrites against an S3 store"
```

---

### Task 2: `fleet/gateway` object store + local backend

**Files:**
- Create: `fleet/gateway/object.go`, `fleet/gateway/local.go`, `fleet/gateway/object_test.go`, `fleet/gateway/local_test.go`

**Interfaces:**
```go
package gateway

// ObjectInfo is what HEAD and LIST return.
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string    // hex MD5 of the stored bytes, quoted by the XML layer
	LastModified time.Time
}

// ErrExists is returned by Put when the key is already present and the caller
// did not pass overwrite (the append-only rule, §4.2).
var ErrExists = errors.New("object already exists")

// ErrNotFound is returned by Get, Head and Delete for a missing key.
var ErrNotFound = errors.New("object not found")

type ObjectStore interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, overwrite bool) (ObjectInfo, error)
	Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, ObjectInfo, error) // length < 0 means "to the end"
	Head(ctx context.Context, key string) (ObjectInfo, error)
	List(ctx context.Context, prefix, after string, max int) (objs []ObjectInfo, truncated bool, err error)
	Delete(ctx context.Context, key string) error
	Versioned(ctx context.Context) bool
}

// NormalizeKey validates an S3 object key and confines it to prefix.
// It rejects: an empty key, a leading '/', any "." or ".." segment, an empty
// segment, any byte < 0x20 or 0x7f, a key over 1024 bytes, and any key that
// does not start with prefix. The returned key is the input unchanged - it
// never rewrites, so the caller cannot be surprised by a different key.
func NormalizeKey(key, prefix string) (string, error)

// NewLocal returns an ObjectStore rooted at dir (0700, files 0600).
func NewLocal(dir string) (ObjectStore, error)
```

- [ ] **Step 1: Failing tests** (`object_test.go`)

```go
func TestNormalizeKeyConfinesToPrefix(t *testing.T) {
	const p = "abc123/"
	for _, bad := range []string{
		"", "/abc123/x", "abc123/../../etc/passwd", "abc123//x", "other/x",
		"abc123/" + strings.Repeat("a", 1100), "abc123/x\x00y", "abc123/.",
	} {
		if _, err := gateway.NormalizeKey(bad, p); err == nil {
			t.Errorf("NormalizeKey(%q) = nil error, want error", bad)
		}
	}
	got, err := gateway.NormalizeKey("abc123/p1234abcd", p)
	require.NoError(t, err)
	require.Equal(t, "abc123/p1234abcd", got)
}
```

  `local_test.go`: `Put` then `Put` again returns `ErrExists` and **leaves the first bytes intact**; `Put(..., overwrite=true)` replaces them; `Get` with `offset/length` returns the right slice; `Head` on a missing key is `ErrNotFound`; `List` paginates with `after` and reports `truncated`; a `Put` whose reader errors mid-stream leaves **no** file behind (the temp-file + `rename` property); modes are `0600`/`0700`.

- [ ] **Step 2: Implement.** `local.Put` writes to `<dir>/.tmp-<random>` in the same directory, `fsync`s, then `link`s (or `rename`s when `overwrite`) into place — `link(2)` fails with `EEXIST`, which is the append-only rule enforced by the kernel rather than by a check-then-write race. `List` reads the directory, sorts, and slices from `after`.

- [ ] **Step 3: Verify and commit**

```bash
go test ./fleet/gateway/... -count=1 -race
cr review --agent -t uncommitted
git add fleet/gateway && git commit -m "fleet/gateway: object store interface and append-only local backend"
```

---

### Task 3: SigV4 verification

**Files:**
- Create: `fleet/gateway/sigv4.go`, `fleet/gateway/sigv4_test.go`

**Interfaces:**
```go
// Credential is what a verified request identifies.
type Credential struct{ AccessKeyID, Secret string }

// SecretLookup resolves an access key id to its secret. A disabled or unknown
// key returns ok=false; the caller answers InvalidAccessKeyId either way, so a
// disabled key is indistinguishable from one that never existed.
type SecretLookup func(ctx context.Context, accessKeyID string) (secret string, ok bool)

// Verify checks an AWS4-HMAC-SHA256 signature on r and returns the access key
// id it was signed with. It rejects: a missing or non-AWS4 Authorization
// header, an X-Amz-Date more than maxClockSkew from now, a SignedHeaders set
// without "host" or "x-amz-content-sha256", an unknown key, and a signature
// mismatch (compared with hmac.Equal). It never reads the body.
func Verify(ctx context.Context, r *http.Request, region, service string, look SecretLookup, now time.Time) (string, error)

const maxClockSkew = 15 * time.Minute
```

- [ ] **Step 1: Failing tests.** Sign a request with a known key using a hand-rolled reference signer in the test (not the code under test), assert `Verify` accepts it; then mutate, one at a time, and assert rejection: the signature, the method, the path, a signed header's value, the date (+16 min and −16 min), the access key id, `SignedHeaders` (drop `host`), and the algorithm token. Add the AWS documentation's canonical-request example as a known-answer vector for the canonicalisation helper. Assert `Verify` does **not** consume `r.Body` (pass a body that panics on read).

- [ ] **Step 2: Implement.** Canonical request → string to sign → signing key (`AWS4` + secret, then date, region, service, `aws4_request`) → `hmac.Equal`. Handle `x-amz-content-sha256` values per the Task 1 recording; if Task 1 recorded `STREAMING-…`, this task stops and raises (§14.2).

- [ ] **Step 3: Commit**

```bash
go test ./fleet/gateway/... -count=1 -race
cr review --agent -t uncommitted
git add fleet/gateway && git commit -m "fleet/gateway: AWS SigV4 request verification"
```

---

### Task 4: `device_keys` table, sealed secrets, and the additive column migration

**Files:**
- Create: `fleet/store/device_keys.go`, `fleet/store/migrate.go`, `fleet/store/migrate_test.go`, `fleet/store/device_keys_test.go`, `fleet/gateway/keys.go`, `fleet/gateway/keys_test.go`
- Modify: `fleet/store/schema.sql`, `fleet/store/store.go` (`Open` calls `migrate`), `fleet/store/targets.go` (new columns on `Target`)

**Interfaces:**
```go
// store
type DeviceKey struct {
	AccessKeyID  string
	AgentID      string
	SealedSecret []byte
	Prefix       string
	ReadOnly     bool
	CreatedAt    time.Time
	DisabledAt   *time.Time
}

func (s *Store) CreateDeviceKey(ctx context.Context, k *DeviceKey) error
func (s *Store) DeviceKey(ctx context.Context, accessKeyID string) (*DeviceKey, error) // ErrNotFound when disabled
func (s *Store) DeviceKeysForAgent(ctx context.Context, agentID string) ([]DeviceKey, error)
func (s *Store) DisableDeviceKeysForAgent(ctx context.Context, agentID string, at time.Time) (int64, error)

// migrate applies additive ALTER TABLE ... ADD COLUMN for every column in
// addedColumns that PRAGMA table_info says is missing. It never drops,
// retypes or backfills. Called by Open after the schema is applied.
func migrate(db *sql.DB) error

// gateway
// Keys resolves access key ids to a device, unsealing the secret and caching
// it for at most cacheTTL. Invalidate drops every entry for an agent, so a
// revoke takes effect on the next request rather than at TTL expiry (§7.2).
type Keys struct{ ... }
func NewKeys(st *store.Store, k seal.Key) *Keys
func (k *Keys) Lookup(ctx context.Context, accessKeyID string) (agentID, prefix, secret string, readOnly, ok bool)
func (k *Keys) Invalidate(agentID string)
const cacheTTL = 5 * time.Minute
const cacheSize = 4096
```

- [ ] **Step 1: Failing tests.** `migrate_test.go`: build a DB from a **copy of the pre-Plan-3 `schema.sql`** checked into the test as a string, insert a target row, run `Open`, assert the new columns exist (`PRAGMA table_info(targets)`) and the row survives with its old values intact; run `Open` twice and assert no error (idempotent). `device_keys_test.go`: create, look up, look up a disabled key (`ErrNotFound`), disable-for-agent returns the count. `keys_test.go`: a cached lookup does not hit the store (count store calls through a wrapper); `Invalidate` forces a re-read; a wrong sealing key yields `ok=false`, not a panic.

- [ ] **Step 2: Implement** the schema block and columns exactly as §5 lists them (`device_keys`, `jobs`, `kit_acks`, `repo_stats` tables; `targets.storage_mode`, `mirror_kind`, `mirror_bucket`, `mirror_region`, `sealed_mirror_key`, `mirror_lock_verified_at`; `agents.retired_at`). The `jobs`, `kit_acks` and `repo_stats` tables land now, unused until M4/M5, so there is exactly one migration to reason about.

- [ ] **Step 3: Commit**

```bash
go test ./fleet/... -count=1
cr review --agent -t uncommitted
git add fleet/store fleet/gateway && git commit -m "fleet/store: device keys, jobs, kit acks and repo stats tables with an additive column migration"
```

---

### Task 5: The `/s3/` handler — the exact S3 subset

**Consumes Task 1:** the delete allowlist and the overwrite allowlist come from `docs/RECONCILE-append-only.md`, referenced by name in the code comment and encoded in one place (`allowDelete(key string) bool`, `allowOverwrite(key string) bool`).

**Files:**
- Create: `fleet/gateway/handler.go`, `fleet/gateway/xmlout.go`, `fleet/gateway/limit.go`, and their tests
- Modify: `fleet/api/server.go` (`Mount` calls `s.mountGateway(m)`), create `fleet/api/gateway_mount.go`

**Interfaces:**
```go
// Gateway serves the S3 subset at /s3/. Stateless: no per-device state
// beyond the Keys cache and the rate limiter.
type Gateway struct{ ... }

type Config struct {
	Keys     *Keys
	StoreFor func(ctx context.Context, agentID string) (ObjectStore, error) // target-aware, one per device
	Bucket   string        // "warphold"
	Region   string        // "warphold"
	Now      func() time.Time
	Log      func(entry LogEntry)
}

type LogEntry struct {
	DeviceID, AccessKeyID, Method, Key string
	Status int
	Bytes  int64
	Dur    time.Duration
}

func NewGateway(c Config) *Gateway
func (g *Gateway) Mount(m *mux.Router)   // registers /s3/{bucket} and /s3/{bucket}/{key:.*}

const BucketName = "warphold"

// Rate limit per device: 50 req/s sustained, burst 200 -> 503 SlowDown + Retry-After.
const (ratePerSecond = 50; rateBurst = 200)
```

- [ ] **Step 1: Failing tests** (`handler_test.go`), each an `httptest` server plus a hand-rolled signer:
  - unsigned request → `403`; wrong signature → `403 SignatureDoesNotMatch`; unknown key → `403 InvalidAccessKeyId`
  - `PUT dev-a/p1` with device A's key → `200`; the same `PUT` again → `409`; `PUT` of a key on Task 1's overwrite allowlist twice → `200` both times
  - `PUT dev-b/p1` with device A's key → `403` (prefix confinement); `PUT ../../etc/x` → `403`
  - `GET` with `Range: bytes=5-9` → `206` and the right 5 bytes; a multi-range header → `501`
  - `HEAD` → `Content-Length`, `ETag`, `Last-Modified`
  - `GET /s3/warphold?list-type=2&prefix=` with device A's key → only A's keys, `max-keys` capped at 1000, `NextContinuationToken` round-trips and cannot be forged into another prefix (feed a token minted for device B → `403`)
  - `DELETE` of a normal blob → `403 AccessDenied`; `DELETE` of a key on Task 1's allowlist → `204`
  - `POST ?uploads` and `POST ?delete` → `501`
  - `GET /s3/warphold?versioning` → `Suspended` for the local backend
  - a wrong bucket name → `404 NoSuchBucket`
  - 250 rapid requests from one device → some `503` with `Retry-After`, and a **second** device is unaffected
  - the log callback receives the device id and never the secret (assert the entry has no field carrying it)

- [ ] **Step 2: Implement.** `xmlout.go` holds the four XML shapes (`ListBucketResult`, `Error`, `VersioningConfiguration`, `InitiateMultipartUploadResult` is not needed) and the S3 error codes. `limit.go` is a `map[string]*bucket` under a mutex with a sweeper — `// ponytail: one mutex over the whole limiter map; shard it if 1000 devices ever contend measurably`.

- [ ] **Step 3: Commit**

```bash
go test ./fleet/gateway/... ./fleet/api/... -count=1 -race
cr review --agent -t uncommitted
git add fleet/gateway fleet/api && git commit -m "fleet/gateway: append-only S3 subset served at /s3/"
```

---

### Task 6: `public_url` — setting, validation, and every consumer

**Files:**
- Create: `fleet/api/publicurl.go`, `fleet/api/publicurl_test.go`
- Modify: `fleet/api/admin_settings.go` (the `public_url` key), `fleet/api/admin_tokens.go` (the gate), `fleet/api/csrf.go` (**new** origin check), `cli/command_fleet_activate.go` (`--public-url`)
- **Every `requestIsHTTPS` caller switches to the `public_url`-derived decision** (grepped 2026-09-02; `requestIsHTTPS` itself stays as the fallback when `public_url` is unset, so nothing regresses on a fresh server):
  - `fleet/api/enrollsh.go:36` — chooses `http`/`https` for the `Server` URL templated into `enroll.sh`; **use `public_url` verbatim when set**, so a device is told the same host the gateway signs for
  - `fleet/api/sessions.go:66` — the `Secure` flag on the session cookie at login
  - `fleet/api/sessions.go:109` — the `Secure` flag on the clearing cookie at logout (must match line 66 or the browser keeps the cookie)
  - `fleet/api/enrollsh.go:52` — `requestIsHTTPS`'s own definition; it gains a doc comment saying it is now the fallback path only

**Interfaces:**
```go
// PublicURL returns the configured public URL, or "" when unset.
func (s *Server) PublicURL(ctx context.Context) string

// ValidatePublicURL fetches <u>/api/v1/fleet/status through the network and
// requires a 200 whose body contains "activated". Redirects are not followed,
// the timeout is 10s, and the returned error carries proxyRequirements so the
// wizard can print them (§6).
func ValidatePublicURL(ctx context.Context, hc *http.Client, u string) error

// proxyRequirements is the operator-facing list shown on validation failure.
var proxyRequirements = []string{
	"forward the Host header unchanged",
	"forward the full path, including /s3/",
	"do not buffer request bodies",
	"allow a request body of at least 5 GiB",
	"allow a read timeout of at least 30 minutes",
}

// PUT /api/v1/fleet/settings {"public_url": "..."} validates before writing.
// POST /api/v1/fleet/tokens -> 409 while public_url is empty.

// fleet/api/csrf.go - NEW. requireCSRF today is double-submit only; this is
// the origin half, applied inside it for non-GET/HEAD/OPTIONS.
//
//   Origin present   -> must equal publicURL's scheme+host, else 403
//   else Referer     -> its origin must match, else 403
//   neither present  -> PASS. Non-browser clients (curl, the agent, CI) send
//                       no Origin; they are already covered by the
//                       double-submit token, which a cross-site page cannot
//                       forge. Failing closed here would break every CLI.
//   publicURL == nil -> check skipped, and a warning is logged once.
func originAllowed(r *http.Request, publicURL *url.URL) bool
```

- [ ] **Step 1: Failing tests.** `PUT` a URL served by an `httptest` server that answers the real status body → `200` and the setting is stored; one that 404s → `400` whose body lists the proxy requirements; one that redirects → `400`; a non-absolute or non-http(s) URL → `400`. `POST /tokens` before any `public_url` → `409` with the exact message; after → `201`. A session cookie issued under an `https://` public URL has `Secure`, and the logout cookie matches it; under `http://` neither does. `GET /enroll.sh` uses the `public_url` host, not the `Host` header, when the setting is present.
  **`originAllowed` (five cases, all in `fleet/api/csrf_test.go`):** matching `Origin` → 2xx; foreign `Origin` → `403`; no `Origin` but a matching `Referer` → 2xx; **neither header present → passes** (this is the case that keeps `curl` and the agent working, so it is an assertion, not an omission); `public_url` unset → the check is skipped and the warning is logged. Plus: a safe method with a foreign `Origin` still passes (`GET` is exempt).
  `warphold fleet activate --public-url <u>` stores it (CLI test in `cli/command_fleet_test.go`).

- [ ] **Step 2: Implement.** Host validation (`421 Misdirected Request`) goes in one middleware in `publicurl.go` that exempts loopback so a local `curl` still works.

- [ ] **Step 3: Commit**

```bash
go test ./fleet/... ./cli/... -count=1
cr review --agent -t uncommitted
git add fleet/api cli && git commit -m "fleet: public_url setting, end-to-end validation, and the token gate"
```

---

### Task 7: `hosted` target kind

**Files:**
- Modify: `fleet/store/targets.go`, `fleet/api/admin_targets.go`, `fleet/api/admin_targets_test.go`

**Interfaces:**
```go
// store.Target gains:
//   StorageMode           string     // "disk" | "cloud", only for kind == "hosted"
//   MirrorKind            string     // "b2" | ""
//   MirrorBucket          string
//   MirrorRegion          string
//   SealedMirrorKey       []byte
//   MirrorLockVerifiedAt  *time.Time
//
// POST /api/v1/fleet/targets accepts:
//   {"name","kind":"hosted","storage_mode":"disk","path":"/srv/warphold/hosted"}
//   {"name","kind":"hosted","storage_mode":"cloud","bucket","region","key_id","key"}   (M2)
// Validation: kind hosted requires storage_mode; disk requires an absolute,
// existing, writable path; cloud requires bucket + credentials and verifies
// Object Lock before the target is created (as the b2 kind already does).
```

- [ ] **Step 1: Failing tests.** `hosted` + `disk` with a temp path → `201`, and `GET /targets` echoes `storage_mode`; `hosted` with no `storage_mode` → `400`; `disk` with a relative path, a missing directory, or a non-writable one → `400` each with a distinct message; the existing `b2` and `filesystem` cases still pass unchanged.

- [ ] **Step 2: Implement.** `storage_mode: "cloud"` returns `501 not implemented yet` until Task 11 — a deliberate stub with a test asserting the `501`, so M1 cannot half-ship M2.

- [ ] **Step 3: Commit**

```bash
go test ./fleet/... -count=1
cr review --agent -t uncommitted
git add fleet && git commit -m "fleet: hosted target kind with a disk storage mode"
```

---

### Task 8: Hosted provisioning and revocation

**Files:**
- Modify: `fleet/enroll/provision.go`, `fleet/enroll/provision_test.go`, `fleet/api/agent_endpoints.go` (the enroll handler passes the hosted spec), `fleet/api/admin_agents.go` (revoke disables device keys and schedules the reap)

**Interfaces:**
```go
// enroll.Bundle gains:
//   GatewayKeyID string `json:"gateway_key_id,omitempty"`
//   GatewayKey   string `json:"gateway_key,omitempty"`
//   Endpoint     string `json:"endpoint,omitempty"`   // host[:port] of public_url
//
// enroll.TargetSpec gains: StorageMode, HostedRoot, PublicHost string; TLS bool.
//
// NewGatewayCredentials returns an S3-shaped key pair: the id is "WH" + 18
// base32 characters, the secret is 40 base64url characters, both from
// crypto/rand.
func NewGatewayCredentials() (accessKeyID, secret string, err error)
//
// For t.Kind == "hosted", Provision:
//  1. NewPassword()                        (unchanged)
//  2. NewGatewayCredentials() -> device_keys row, secret sealed
//  3. agentCI = s3.Options{BucketName: gateway.BucketName, Prefix: agentID+"/",
//       Endpoint: t.PublicHost, DoNotUseTLS: !t.TLS, AccessKeyID: ..., SecretAccessKey: ..., Region: "warphold"}
//  4. adminCI = filesystem.Options{Path: <hosted root>/<agentID>}   // direct, not over HTTP
//  5. initialize(adminCI) -> repository + Fleet as maintenance owner (unchanged)
//  6. seal the bundle
```

- [ ] **Step 1: Failing tests.** Provision against a `hosted`/`disk` spec with a temp root: a `device_keys` row exists with the right prefix, the sealed secret unseals to the returned secret, the connect token decodes to an `s3` connection info with the right bucket/prefix/endpoint, and the repository directory contains `kopia.repository`. Revoke: the agent's device keys all have `disabled_at`, `Keys.Invalidate` was called, a `reap` job is scheduled for `now + revoked_retention_days`, and the repository directory is **still there**.

- [ ] **Step 2: Implement.** Roll back the `device_keys` row if repository creation fails, the way the B2 path already revokes its keys.

- [ ] **Step 3: Commit**

```bash
go test ./fleet/... -count=1
cr review --agent -t uncommitted
git add fleet && git commit -m "fleet/enroll: hosted provisioning with per-device gateway keys; two-phase revocation"
```

---

### Task 9: End-to-end integration test — stock Kopia against the gateway

**This is the D4 spike promoted to a permanent regression test** and the thing that proves the gateway is S3 enough.

**Files:**
- Create: `fleet/gateway/e2e_test.go`

**Interfaces:** none (a test).

- [ ] **Step 1: Write it.**

```go
// TestStockKopiaClientAgainstGateway drives repo/blob/s3 (the same client a
// device runs) against an httptest gateway over a hosted/disk target, and
// fails on any DELETE the allowlist did not expect.
func TestStockKopiaClientAgainstGateway(t *testing.T) {
	// ... harness: store + seal key + hosted target + provisioned device
	// ... httptest.NewServer(gateway mounted at /s3/)
	// st, err := s3.New(ctx, &s3.Options{BucketName: gateway.BucketName, Prefix: agentID + "/",
	//     Endpoint: u.Host, DoNotUseTLS: true, AccessKeyID: akid, SecretAccessKey: secret, Region: "warphold"}, true)
	// repo.Initialize -> repo.Connect -> snapshot a temp tree -> list -> restore -> compare hashes
	// denied := harness.deniedRequests()   // recorded by the Log callback
	// require.Empty(t, denied, "gateway denied a request stock Kopia needed: %v", denied)
}
```

- [ ] **Step 2: Run it.** `go test ./fleet/gateway/ -run StockKopia -count=1 -v`. If it denies something, the fix is either the Task 1 allowlist (if the spike missed a class — update `docs/RECONCILE-append-only.md` in the same commit) or a bug. Never widen the allowlist without recording why in that file.

- [ ] **Step 3: Commit**

```bash
cr review --agent -t uncommitted
git add fleet/gateway && git commit -m "fleet/gateway: end-to-end test driving stock Kopia's S3 client against the gateway"
```

---

### Task 10: Deploy M1 to the Fleet host and enroll the FW16 — **HUMAN CHECKPOINT**

**HUMAN CHECKPOINT:** Hody boots the **FW16** (the old CachyOS laptop — never the FW13 Pro, D12) and connects it to the network.

**Files (vault only):** `Homelab/WarpHold/WarpHold-Full-Dev-Guide.md` (build log), `WarpHold-Issues-and-Fixes.md`, `hody/00-Log.md`

- [ ] **Step 1: Build and deploy**

```bash
cd /home/hody/.dev/Projects/warphold
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /tmp/warphold-linux-amd64 .
scp /tmp/warphold-linux-amd64 root@<FLEET_IP>:/usr/local/bin/warphold.new
ssh root@<FLEET_IP> 'mv /usr/local/bin/warphold.new /usr/local/bin/warphold && chmod 755 /usr/local/bin/warphold && systemctl restart warphold'
until curl -fsS https://<FLEET_HOST>/api/v1/fleet/status | grep -q '"activated":true'; do sleep 3; done
```

- [ ] **Step 2: Set `public_url` and create the hosted target** (authenticated `curl` with the CSRF header, as Plan 2 Task 7 does):
  `PUT /settings {"public_url":"https://<FLEET_HOST>"}` → `200`; `POST /targets {"name":"hosted-disk","kind":"hosted","storage_mode":"disk","path":"/srv/warphold/hosted"}` → `201`; a group on it; a token.

- [ ] **Step 3: Enroll the FW16 for real**

```bash
ssh <FW16_IP> 'curl -fsSL https://<FLEET_HOST>/enroll.sh | sh -s -- --token <T>'
ssh <FW16_IP> 'warphold agent status'
```
  Then queue `snapshot-now` and poll `GET /agents` until the FW16 is green with a real home-directory snapshot. Show the output.

- [ ] **Step 4: Prove it is really hosted.** On the Fleet host: `ls /srv/warphold/hosted/<device-id>/ | head` shows Kopia blobs, and `journalctl -u warphold | grep ' /s3/' | tail` shows the FW16's device id on real requests. On the FW16, the tray shows the last backup (screenshot).

- [ ] **Step 5: Vault** — Full-Dev-Guide gains this build log verbatim with outputs; Issues-and-Fixes gains any symptom hit and its source; `INGEST` line in `hody/00-Log.md`. Placeholders only.

---

## M2 — hybrid B2 mirror (same branch)

### Task 11: Cloud-direct backend

**Files:**
- Create: `fleet/gateway/cloud.go`, `fleet/gateway/cloud_test.go`
- Modify: `fleet/api/admin_targets.go` (drop Task 7's `501`)

**Interfaces:**
```go
// NewCloud returns an ObjectStore writing through to a B2 or S3 bucket with
// the fleet's admin credentials. Put honours the append-only rule by a Head
// before the write - a race is possible in theory and impossible in practice
// (one device owns its prefix), and the loser is a 409, never a lost blob.
func NewCloud(ctx context.Context, ci blob.ConnectionInfo, prefix string) (ObjectStore, error)
```

- [ ] **Step 1: Failing tests** against a `filesystem`-backed `blob.Storage` stand-in plus a fake B2 (`fleet/b2api`'s test server shape): the same append-only, prefix and range behaviour as `local`, and `Versioned` reflects the underlying store.
- [ ] **Step 2: Implement** over `repo/blob`'s interfaces so B2 and S3 share one code path.
- [ ] **Step 3:** `go test ./fleet/... -count=1` → `cr review --agent -t uncommitted` → `git commit -m "fleet/gateway: cloud-direct write-through backend"`

---

### Task 12: Object Lock verification for the mirror and for cloud-direct — **HUMAN CHECKPOINT**

**HUMAN CHECKPOINT:** Hody provides a **B2 bucket with Object Lock enabled and an admin application key** (1Password). Without it this task and Task 14 cannot complete; nothing else in the plan blocks on it.

**Files:**
- Modify: `fleet/b2api/client.go` (bucket Object Lock read, if not already exposed), `fleet/api/admin_targets.go`, tests

- [ ] **Step 1: Failing tests** — a mirror configuration on a bucket without Object Lock → `400` naming the bucket; with it → `mirror_lock_verified_at` is set. For an S3 cloud-direct target the call is `GetObjectLockConfiguration`, not B2's (§14.5) — a separate test with a separate fake.
- [ ] **Step 2: Implement**, then verify against the **real** bucket and record the actual API response shape in `docs/RECONCILE-append-only.md`'s sibling section (or a new `docs/RECONCILE-object-lock.md`) — §14.5 says this must be observed, not read.
- [ ] **Step 3:** commit `-m "fleet: verify Object Lock on mirror and cloud-direct buckets"`

---

### Task 13: The mirror job and the jobs scheduler skeleton

**Files:**
- Create: `fleet/jobs/scheduler.go`, `fleet/jobs/mirror.go`, `fleet/store/jobs.go`, and tests
- Modify: `fleet/api/server.go` (start and stop the scheduler with the server)

**Interfaces:**
```go
// store
type Job struct {
	ID int64; Kind string; AgentID string
	ScheduledFor time.Time; StartedAt, FinishedAt *time.Time
	Status string; Detail string
}
func (s *Store) EnqueueJob(ctx context.Context, j *Job) (int64, error)
func (s *Store) ClaimDueJob(ctx context.Context, now time.Time) (*Job, error) // atomic UPDATE ... WHERE status='pending'
func (s *Store) FinishJob(ctx context.Context, id int64, at time.Time, status, detail string) error
func (s *Store) JobsForAgent(ctx context.Context, agentID string, limit int) ([]Job, error)

// jobs
type Runner func(ctx context.Context, j store.Job) (detail string, err error)
type Scheduler struct{ ... }
func NewScheduler(st *store.Store, runners map[string]Runner, tick time.Duration) *Scheduler
func (s *Scheduler) Start(ctx context.Context)   // one goroutine, claims one job at a time
func (s *Scheduler) Stop()

// mirror: for each hosted/disk target with a mirror, upload every local key
// absent from the mirror bucket. Never deletes. Records repo_stats.mirrored_at
// and mirrored_bytes.
func Mirror(st *store.Store, k seal.Key) Runner
```

- [ ] **Step 1: Failing tests.** `ClaimDueJob` under two concurrent claimers hands the job to exactly one (run it 100× with `-race`). A job whose runner panics is recorded `error` with the panic text, and the scheduler survives. `Mirror` uploads only missing keys (seed the mirror with half of them), never deletes, and is idempotent (a second run uploads nothing).
- [ ] **Step 2: Implement.** `// ponytail: one job at a time, one goroutine; parallelise per-kind only if a real fleet makes it slow.`
- [ ] **Step 3:** commit `-m "fleet/jobs: scheduler and the disk-to-B2 mirror job"`

---

### Task 14: Offsite health in the API and the UI — **UI PR 1**

**Files:**
- Modify: `fleet/api/admin_targets.go`, `fleet/api/overview.go`, `fleet/api/admin_agents.go` (mirror state in the device detail), tests
- UI: `src/api/types.ts`, `src/pages/fleet/Targets.tsx`, `src/pages/fleet/Device.tsx`

**Interfaces:**
```ts
// types.ts
export interface Target {
  id: number; name: string;
  kind: "b2" | "filesystem" | "hosted";
  storage_mode?: "disk" | "cloud";
  mirror_kind?: "b2" | "";
  mirror_bucket?: string;
  mirror_lock_verified_at?: string | null;
  object_lock_verified_at?: string | null;
}
export interface MirrorState { mirrored_at: string | null; mirrored_bytes: number; stale: boolean }
// AgentDetail gains: mirror: MirrorState | null
```

- [ ] **Step 1:** Go tests for the new response fields; vitest for the Targets row rendering "local ✓ / offsite ✓ 2 h ago", "offsite stale" when the last mirror is older than 3× the interval, and nothing at all for a target with no mirror.
- [ ] **Step 2:** Implement both sides. Tag the UI module (`v0.2.x`) and bump it in the main repo's `go.mod`.
- [ ] **Step 3:** UI PR: *"Plan 3 UI 1: hosted targets and offsite mirror state"*. `cr review --agent --base main` in the UI repo before opening it.

---

### Task 15: Live mirror test and **PR A**

- [ ] **Step 1:** Configure the mirror on the live hosted target (real B2 bucket from Task 12), run it: `warphold fleet jobs run --kind mirror` on `<FLEET_HOST>`, then list the bucket and compare the object count with `find /srv/warphold/hosted/<device-id> -type f | wc -l`. Show both numbers.
- [ ] **Step 2:** Restore-from-mirror smoke test: connect a stock upstream `kopia` to `b2://<bucket>/<device-id>/` with a read-only key and the repository password from the store, and run `snapshot list`. This is the M4 CI test done by hand, early, because it is the cheapest place to find out the mirror layout is wrong.
- [ ] **Step 3: PR A**

```bash
grep -rn "hody\.sh\|hody\.dev\|10\.99\." . --exclude-dir=.git --exclude-dir=.superpowers   # expect nothing
go test ./... -count=1
cr review --agent --base master                      # one whole-branch pass
git push -u origin plan3-gateway
gh pr create --title "Plan 3A: hosted target — S3 gateway, device keys, public_url, B2 mirror" --base master
```
  Wait for the CodeRabbit GitHub app; one batched fix commit if needed (`cr review --agent -t committed --base-commit <sha>` on the fix diff only); Hody merges.
- [ ] **Step 4: Vault** — Admin-Overview gains "hosted targets: what they are, how to add one, how the mirror works"; Full-Dev-Guide gains the M1/M2 build log; `INGEST` line.

---

## M3 — installers and setup (branch `plan3-installers`)

### Task 16: goreleaser — WarpHold packaging and signed checksums

**Files:**
- Modify: `.goreleaser.yml` (`warphold:` comments on every changed block), create `packaging/postinstall.sh`, `packaging/postremove.sh`
- Create: `.github/workflows/release.yml` (or rename upstream's, keeping its structure)

**Interfaces:** none (build configuration).

- [ ] **Step 1: Prune `builds` to what WarpHold actually ships.** Upstream builds `linux`/`freebsd`/`openbsd` × `amd64`/`arm`/`arm64`; WarpHold publishes **`goos: [linux]`, `goarch: [amd64, arm64]`** and nothing else (D8). Drop freebsd, openbsd and 32-bit arm from the release matrix; keep darwin and windows as **compile checks in CI** (`GOOS=darwin GOARCH=arm64 go build ./...`, same for windows/amd64) so the fork stays portable without publishing artifacts nobody installs. Assert `dist/` contains no freebsd/openbsd/arm output in Step 3.

- [ ] **Step 2: Replace the `nfpms` block's upstream metadata:** `homepage: https://warphold.com`, `vendor: WarpHold`, `maintainer: Hodahel Moinzadeh <hody@…>` (the address that is already in `NOTICE`), `description: Backups that hold. For every machine you care about.`, `license: Apache 2.0`, `conflicts: [kopia]`, `scripts.postinstall: packaging/postinstall.sh`, `scripts.postremove: packaging/postremove.sh`. `postinstall.sh` creates the `warphold` system user and `/var/lib/warphold` **only** when `/etc/warphold` exists or `WARPHOLD_SERVER=1` — the app package must not create a server user on a laptop.
- [ ] **Step 3: Attach the install scripts to every release.** An `extra_files` (or equivalent) entry publishes `scripts/install/fleet.sh` and `scripts/install/app.sh` as release assets, so `https://github.com/hodyhq/warphold/releases/latest/download/fleet.sh` always serves the script that shipped with the latest binary. This is what makes the `get.warphold.com` redirect (Task 17) work with no second copy of either script anywhere.

- [ ] **Step 4:** Signing — `signs.artifacts: checksum` stays; confirm `tools/sign.sh` works with the key Hody supplies (**HUMAN CHECKPOINT: release signing key**, from 1Password into a GitHub Actions secret; never in the repo).
- [ ] **Step 5:** Dry-run locally: `goreleaser release --snapshot --clean --skip=sign,publish`, then assert `dist/` holds `.deb`, `.rpm`, the tarballs and `checksums.txt` for `linux/amd64` and `linux/arm64`, holds both install scripts, and holds **nothing** for freebsd, openbsd or `arm`:
```bash
ls dist/ | grep -Ei 'freebsd|openbsd|_arm(_|$)' && echo "FAIL: pruned platform still built" || echo "ok: linux amd64/arm64 only"
ls dist/fleet.sh dist/app.sh
```
  Install the `.deb` in a container and run `warphold --version`.
- [ ] **Step 6:** commit `-m "release: WarpHold deb/rpm packaging metadata and signed checksums"`

---

### Task 17: `get.warphold.com` redirect rules — **HUMAN CHECKPOINT**

**HUMAN CHECKPOINT:** **Cloudflare redirect rules** on the `warphold.com` zone. There is **no repo and no site** for `get.warphold.com` (D8) — the scripts live in the main repo and ship as release assets (Task 16), and this host is two redirects, so no copy of either script exists anywhere else.

**Files:** none in any repo. The output is DNS/Cloudflare configuration plus the smoke check recorded in the task report.

- [ ] **Step 1:** Ask Hody for a proxied DNS record for `get.warphold.com` on the `warphold.com` zone and two Cloudflare **redirect rules** (301, preserving the path):

```
get.warphold.com/fleet.sh -> https://github.com/hodyhq/warphold/releases/latest/download/fleet.sh
get.warphold.com/app.sh   -> https://github.com/hodyhq/warphold/releases/latest/download/app.sh
```
  Anything else on the host redirects to `https://warphold.com/` (which lands in M6; until then it is a 404 nobody links to).

- [ ] **Step 2: Smoke check** — the load-bearing property is that `curl -fsSL` follows **both** hops (Cloudflare's 301 and GitHub's own 302 to the asset CDN):

```bash
curl -fsSL https://get.warphold.com/fleet.sh | head -5      # must print the script
curl -fsSL https://get.warphold.com/app.sh   | head -5
curl -fsS -o /dev/null -w '%{http_code} %{num_redirects} %{url_effective}\n' -L https://get.warphold.com/fleet.sh
```
  Show every line. If the redirect drops the path or a rule matches too broadly, fix the rule — never work around it by publishing a copy of the script (that is the duplication D8 exists to prevent).

- [ ] **Step 3:** Record the rules in the vault's Admin-Overview "Public surfaces" section, with the release URL written out, so the next person knows the scripts come from the release and not from a web host.

---

### Task 18: `fleet.sh`

**Files (main repo — the only home of this script):** `scripts/install/fleet.sh`; tests: `scripts/test-fleet-sh.sh` (container test) and a `shellcheck` step in CI. Task 16 attaches it to every release; Task 17's redirect is what `get.warphold.com/fleet.sh` resolves to.

- [ ] **Step 1: Failing test first.** `scripts/test-fleet-sh.sh` starts a local HTTP server serving a **fake release** (a real `warphold` binary built from this tree, plus a matching `checksums.txt`), runs `scripts/install/fleet.sh` inside a `debian:12` container with `WARPHOLD_RELEASE_BASE` pointed at it, and asserts: the `warphold` user exists, the unit is enabled and active, `/api/v1/fleet/status` answers, and a **tampered checksum makes the script exit non-zero without installing anything**.
- [ ] **Step 2: Implement** per §8.2: arch detection, packaging family detection, download + `sha256sum -c`, user and directories, the unit (LAN bind, `--insecure`, reverse proxy assumed), `systemctl enable --now`, poll for status, print the URL and the setup token path and value, print the proxy requirements. Non-interactive when `WARPHOLD_SETUP_PUBLIC_URL` / `_EMAIL` / `_PASSWORD` / `_PASSPHRASE` are all set → `warphold fleet activate --public-url …`, reading secrets from the environment so they never reach `ps`.
- [ ] **Step 3:** `shellcheck scripts/install/fleet.sh` clean; `bash scripts/test-fleet-sh.sh` green (show the output).
- [ ] **Step 4:** one commit, one repo: `git commit -m "scripts: one-command Fleet server install, with a container test and shellcheck"`

---

### Task 19: `app.sh` and the single-machine packages

**Files (main repo):** `scripts/install/app.sh`, `scripts/test-app-sh.sh`

- [ ] **Step 1: Failing test.** Same container shape: `scripts/install/app.sh` installs to `~/.local/bin`, writes the user unit and the tray autostart entry (`agent/install` already renders both), and **does not** create a system user, a `/etc/warphold`, or anything Fleet-shaped. Assert the absence explicitly — that is the D11 boundary.
- [ ] **Step 2: Implement** per §8.3, including `--system` and the `xdg-open` of the local app (skipped when `DISPLAY`/`WAYLAND_DISPLAY` are unset).
- [ ] **Step 3:** `shellcheck` clean, container test green.
- [ ] **Step 4:** commit `-m "scripts: one-command single-machine install"`

---

### Task 20: `warphold fleet activate --public-url` and setup's own repository

**Files:**
- Modify: `cli/command_fleet_activate.go`, `fleet/api/server.go` (`Activate` optionally takes the public URL and the storage choice), tests

**Interfaces:**
```go
// cli
//   --public-url    Public URL clients use (validated); env WARPHOLD_PUBLIC_URL
//   --storage       disk|cloud (default disk)
//   --hosted-root   default /srv/warphold/hosted
//
// api: after Activate succeeds, SetupDefaults creates
//   1. a filesystem target for the Fleet host's own repository,
//   2. a "Fleet server" group on it, and
//   3. the hosted target from --storage,
// so a fresh install can enroll a device without touching any screen.
func (s *Server) SetupDefaults(ctx context.Context, publicURL, storage, hostedRoot string) error
```

- [ ] **Step 1: Failing test** in `cli/command_fleet_test.go`: activate with all flags in a temp state dir, then assert `GET /targets` has both targets, `GET /groups` has the group, `public_url` is set, and a token can be issued immediately.
- [ ] **Step 2: Implement.** `SetupDefaults` is idempotent — running it twice creates nothing twice.
- [ ] **Step 3:** commit `-m "cli: fleet activate --public-url and default targets so a fresh server can enroll immediately"`

---

### Task 21: The five-step setup wizard — **UI PR 2**

**Files (UI):** `src/pages/fleet/Activate.tsx`, `src/api/fleet.ts`, `src/api/types.ts`, tests

**Interfaces:**
```ts
// fleet.ts gains:
setPublicURL(url: string): Promise<{ ok: true } | never>   // 400 carries { error, proxy_requirements: string[] }
createHostedTarget(t: { name: string; storage_mode: "disk" | "cloud"; path?: string;
  bucket?: string; region?: string; key_id?: string; key?: string;
  mirror?: { kind: "b2"; bucket: string; key_id: string; key: string } }): Promise<CreatedTarget>
```

- [ ] **Step 1: Failing vitest** for the step order (§8.4): the passphrase step requires two matching entries; step 3 cannot be passed while the server rejects the URL and **renders every `proxy_requirements` line**; step 4 defaults to Fleet disk with `/srv/warphold/hosted` prefilled and reveals the mirror sub-form only when asked; step 5 shows a copyable enrollment one-liner built from `public_url`.
  **Visual proof** (the UI repo has no e2e runner and this plan adds no Node dependency): drive the wizard end to end in the **real logged-in app** with the chrome-devtools MCP driver against a running server — hard-reload after deploy — and attach a screenshot of each step plus the enrolled device. Never "verify" by inspecting markup you injected.
- [ ] **Step 2: Implement.** Steps 1–2 remain the single `POST /activate`; 3–5 are authenticated calls after the session exists, so a death at step 4 leaves an activated, logged-in Fleet.
- [ ] **Step 3:** UI PR *"Plan 3 UI 2: five-step setup wizard"*; tag; bump the Go module.

---

### Task 22: Move the "Fleet server" card to Settings — **UI PR 2 (same PR)**

- [ ] Remove the "Turn this machine into a Fleet server" card from the Repository page; add one line in Settings linking to the Fleet install documentation (`https://warphold.com/fleet.html` once M6 lands; `https://github.com/hodyhq/warphold#fleet` until then — `get.warphold.com` serves only the two script redirects and has no page to link to). vitest asserts the card is gone from Repository and the link is present in Settings. Commit `-m "ui: the standalone app links to the Fleet docs instead of offering to become a Fleet server"`

---

### Task 23: M3 acceptance — one command each — **HUMAN CHECKPOINT**

**HUMAN CHECKPOINT:** a **fresh VM** for the server test (Hody clones the cloud-init template or approves the plan doing it), and the **FW16 wiped or a fresh Linux machine** for the app test (D12).

- [ ] **Step 1:** On a fresh VM: `curl -fsSL https://get.warphold.com/fleet.sh | sh`, then open the printed URL, complete the wizard, and enroll a device. Show the terminal output and the final `GET /api/v1/fleet/status`.
- [ ] **Step 2:** On the fresh Linux machine: `curl -fsSL https://get.warphold.com/app.sh | sh`; the app opens, the tray appears, a snapshot runs. Screenshot both.
- [ ] **Step 3:** Destroy the throwaway VM; record both runs verbatim in the vault's Full-Dev-Guide, and every symptom hit in Issues-and-Fixes with its source. `INGEST` line.

---

## M4 — recovery kit, rotation, standalone restore (same branch)

### Task 24: `fleet/kit` — the recovery kit page

**Files:**
- Create: `fleet/kit/kit.go`, `fleet/kit/kit.html.tmpl`, `fleet/kit/kit_test.go`, `fleet/api/admin_kit.go`, `fleet/api/admin_kit_test.go`
- Modify: `fleet/store/kit_acks.go` (queries over the table Task 4 created)

**Interfaces:**
```go
// kit
type Data struct {
	DeviceName, DeviceID, TargetKind, Endpoint, Bucket, Prefix, Path string
	RepoPassword, ReadKeyID, ReadKey string
	Commands []string      // the literal, copy-pasteable connect/list/restore lines
	Generated time.Time
}
// Render writes a single self-contained print-ready HTML page: inline CSS, no
// external font, image or script, so it prints from a machine with no network.
func Render(w io.Writer, d Data) error

// api
// GET  /api/v1/fleet/agents/{id}/kit        -> text/html, admin session only, no-store
// POST /api/v1/fleet/agents/{id}/kit/ack    -> 204, writes kit_acks
// For a hosted target the kit mints a NEW read-only device key (device_keys
// with read_only=1) rather than exposing the device's own writing key.
```

- [ ] **Step 1: Failing tests.** The rendered page contains the password, the prefix and the three commands, and contains **no** `http://`/`https://` reference to an external asset (assert with a regex over the output). An unauthenticated `GET` is `401`. The hosted kit's key is a *different* access key id from the device's, and is `read_only`. `Cache-Control: no-store` is set. Commands match §14.4's verified flag names.
- [ ] **Step 2: Implement.**
- [ ] **Step 3:** commit `-m "fleet/kit: printable recovery kit with a read-only key, and its acknowledgement"`

---

### Task 25: Kit banner, ack and regenerate — **UI PR 3**

**Files (UI):** `src/pages/fleet/Device.tsx`, `src/pages/fleet/Devices.tsx`, `src/pages/fleet/Overview.tsx`, `src/api/fleet.ts`, tests

- [ ] vitest: an un-acked device shows the persistent banner on the device page and a marker in the list; acking clears both without a reload; "Open recovery kit" opens the kit in a new tab; "Regenerate" warns that the previous kit's read key stops working.
- [ ] **Visual proof** with the chrome-devtools MCP driver in the real logged-in app: banner present before the ack, gone after, screenshot both. No e2e framework, no new Node dependency. UI PR *"Plan 3 UI 3: recovery kit banner and acknowledgement"*; tag; bump.

---

### Task 26: Passphrase rotation

**Files:**
- Create: `fleet/api/admin_passphrase.go`, `cli/command_fleet_rotate.go`, tests
- Modify: `fleet/seal/seal.go` only if a helper is genuinely missing (prefer not to)

**Interfaces:**
```go
// POST /api/v1/fleet/settings/passphrase {"current","new","dry_run"?}
// -> 200 {"resealed": {"agents": n, "targets": n, "device_keys": n, "settings": n}}
//
// Order (matching Activate's, so a crash is survivable): verify current
// derives the loaded key -> new salt -> ONE transaction re-sealing
// agents.sealed_bundle, targets.sealed_admin_key, targets.sealed_mirror_key,
// device_keys.sealed_secret and the sealed SMTP password, and writing the new
// salt -> commit -> only then replace seal.key -> reload.
func (s *Server) RotatePassphrase(ctx context.Context, current, next string, dryRun bool) (map[string]int, error)
//
// CLI: warphold fleet rotate-passphrase [--dry-run]
```

- [ ] **Step 1: Failing tests.** Seed a store with two agents, two targets and three device keys; rotate; every value still unseals under the **new** key and none under the old; the key file changed. A wrong `current` → `403` and **nothing** written. `dry_run` reports the counts and writes nothing. Inject a failure between the transaction commit and the key-file write and assert the server still opens with the *old* key file and the *old* sealed values (i.e. the transaction is rolled back on that path) — this test is the whole point of the ordering.
- [ ] **Step 2: Implement.**
- [ ] **Step 3:** commit `-m "fleet: sealing-passphrase rotation with a dry run"`

---

### Task 27: Standalone-restore CI test with a pinned upstream `kopia`

**Files:**
- Create: `.github/workflows/standalone-restore.yml`, `scripts/standalone-restore-test.sh`

**Interfaces:** none (CI).

- [ ] **Step 1: Write the script.** Start a WarpHold Fleet server on a temp state dir with a hosted/disk target; enroll an agent against it; snapshot a generated tree; read the kit's values from the API; download a **pinned** upstream `kopia` release binary (version pinned in the script, checksum verified); `kopia repository connect s3 …` with the kit's endpoint, prefix and read-only key; `kopia snapshot list`; `kopia restore <id> <dest>`; `diff -r` the restored tree against the source. **Fail if the restore needed anything from WarpHold** — the script runs the upstream binary from a directory that contains no `warphold` binary and with `PATH` scrubbed of it.
- [ ] **Step 2: Wire the workflow** to run on every push to `master` and every PR, `ubuntu-latest`, ~10 minutes.
- [ ] **Step 3:** Run it locally first (`bash scripts/standalone-restore-test.sh`) and show the diff being empty.
- [ ] **Step 4:** commit `-m "ci: restore a hosted-target snapshot with a pinned upstream kopia binary"`

---

## M5 — jobs, SMTP, digest, stats (same branch)

### Task 28: verify, test-restore, maintenance and reap jobs

**Files:**
- Create: `fleet/jobs/{verify,testrestore,maintenance,reap}.go` and tests
- Modify: `fleet/api/server.go` (register the runners), `fleet/api/admin_jobs.go` (`POST /jobs`, `GET /agents/{id}/jobs`), `cli/command_fleet_jobs.go`

**Interfaces:**
```go
func Verify(st *store.Store, k seal.Key) Runner       // snapshot verify per repository
func TestRestore(st *store.Store, k seal.Key) Runner  // one random small file, hash-compared
func Maintenance(st *store.Store, k seal.Key) Runner  // maintenance run --full, Fleet is the owner
func Reap(st *store.Store, k seal.Key) Runner         // remove a revoked device's repository (D6)

// POST /api/v1/fleet/jobs {"kind":"verify","agent_id":"…"} -> 202 {"id":n}
// GET  /api/v1/fleet/agents/{id}/jobs -> the last 50
// CLI: warphold fleet jobs run --kind verify [--agent <id>]
```

- [ ] **Step 1: Failing tests** against a real filesystem repository built in the test: verify passes on a good repository and fails with the raw error on a corrupted blob; test-restore picks a file, restores it and compares hashes, and fails loudly when the bytes differ; maintenance runs and records its output; reap deletes the directory of a revoked agent past its retention and sets `retired_at`, and **refuses** to touch one inside the window.
- [ ] **Step 2: Implement.** Every job's `detail` is the raw stderr — the original spec's §7 rule that the UI shows the real error, not a paraphrase.
- [ ] **Step 3:** commit `-m "fleet/jobs: verify, test-restore, maintenance and reap"`

---

### Task 29: The `verify` agent command (carried from Plan 1)

**Files:**
- Create: `agent/engine/verify.go`, `agent/engine/verify_test.go`
- Modify: `agent/run` (the command dispatch), `fleet/api/admin_agents.go` (the button already queues `verify`)

**Interfaces:**
```go
// Verify runs a repository verification through the local control API and
// returns a poll.Report of kind "verify", acknowledged in the next report
// exactly like snapshot-now (original §6).
func (l *Local) Verify(ctx context.Context) (poll.Report, error)
```

- [ ] **Step 1: Failing test** — a queued `verify` command produces a report of kind `verify` whose `command_id` matches, and an agent that is not the command's owner still cannot ack it (the original spec's §7 guard must keep holding).
- [ ] **Step 2: Implement.**
- [ ] **Step 3:** commit `-m "agent: execute the verify command and report it"`

---

### Task 30: SMTP settings and a test send

**Files:**
- Create: `fleet/mail/smtp.go`, `fleet/mail/settings.go`, tests
- Modify: `fleet/api/admin_settings.go` (the SMTP keys, password sealed), `fleet/api/admin_mail.go` (`POST /settings/smtp/test`)

**Interfaces:**
```go
type Config struct{ Host string; Port int; Username, Password, From string; TLS bool }
// Defaults are SMTP2GO-shaped: host mail.smtp2go.com, port 2525 (587 and 465
// also offered), TLS on.
func Defaults() Config
func Send(ctx context.Context, c Config, to []string, subject, textBody, htmlBody string) error

// Sender is Send bound to the stored settings; the digest job takes one so it
// can be tested without SMTP.
type Sender func(ctx context.Context, to []string, subject, textBody, htmlBody string) error
func SenderFor(st *store.Store, k seal.Key) Sender

// POST /api/v1/fleet/settings/smtp/test {"to":"…"} -> 200, or 400 carrying the
// raw SMTP error. The password is never returned by GET /settings; the UI
// shows "set" or "not set".
```

- [ ] **Step 1: Failing tests** against an in-process SMTP stub: a message is sent with the right envelope and both parts; a bad password surfaces the server's raw error; `GET /settings` never contains the password; the stored password round-trips through the seal.
- [ ] **Step 2: Implement** with `net/smtp` + `crypto/tls`; no dependency.
- [ ] **Step 3:** commit `-m "fleet/mail: SMTP settings with SMTP2GO defaults and a test send"`

---

### Task 31: Repository stats and the weekly digest

**Files:**
- Create: `fleet/jobs/stats.go`, `fleet/jobs/digest.go`, `fleet/jobs/digest.html.tmpl`, tests
- Modify: `fleet/api/overview.go` (`stored_bytes`, `dedup_ratio`), `fleet/api/admin_agents.go` (`size_bytes`)

**Interfaces:**
```go
func Stats(st *store.Store, k seal.Key) Runner   // -> repo_stats{logical_bytes, stored_bytes, blob_count}
func Digest(st *store.Store, k seal.Key, send mail.Sender) Runner

// overview.go: StoredBytes is the sum of repo_stats.stored_bytes; DedupRatio
// is logical/stored, and stays nil (the UI hides the sub-line) until at least
// one row exists - the existing contract in overview.go's comment.
```

- [ ] **Step 1: Failing tests.** `Stats` on a real repository records a non-zero `stored_bytes` and a `blob_count` matching the files on disk. `Overview` returns `stored_bytes` and a non-nil `dedup_ratio` once rows exist, and `nil` before. The digest renders per-device health, last success, bytes and failures, plus fleet totals, un-acked kits, and any job failing for over a week; with zero devices it renders without dividing by zero.
- [ ] **Step 2: Implement.**
- [ ] **Step 3:** commit `-m "fleet/jobs: repository stats and the weekly digest email"`

---

### Task 32: Jobs, settings and Stored tiles in the UI — **UI PR 4**

**Files (UI):** `src/pages/fleet/Settings.tsx` (SMTP + digest + job intervals + passphrase rotation), `src/pages/fleet/Device.tsx` (job history), `src/pages/fleet/Overview.tsx` and `Devices.tsx` (the Stored tiles stop rendering an em dash), `src/api/{fleet.ts,types.ts}`, tests

- [ ] vitest: the Stored tile renders a real value when `stored_bytes` is present and the dedup sub-line only when `dedup_ratio` is non-null; the Devices row shows `size_bytes`; the SMTP form never renders the password; "Send test email" surfaces the raw error; the rotation form requires the current passphrase twice-confirmed and shows the resealed counts on success; the device page lists job history with the raw `detail` in a `<pre>`.
- [ ] UI PR *"Plan 3 UI 4: jobs, SMTP, digest settings and real Stored tiles"*; tag; bump the Go module.

---

### Task 33: Deploy M3–M5, then **PR B**

- [ ] **Step 1:** Build and deploy to `<FLEET_HOST>` as in Task 10; poll for status; run one of each job by hand (`warphold fleet jobs run --kind verify --agent <id>`, `--kind test-restore`, `--kind maintenance`, `--kind stats`) and show the outputs; send a test email and a digest to Hody's address.
- [ ] **Step 2:** Generate and print a recovery kit for the FW16, ack it, and confirm the banner clears in the live UI (screenshot).
- [ ] **Step 3: PR B**

```bash
grep -rn "hody\.sh\|hody\.dev\|10\.99\." . --exclude-dir=.git --exclude-dir=.superpowers   # expect nothing
go test ./... -count=1 && bash scripts/standalone-restore-test.sh
cr review --agent --base master
git push -u origin plan3-installers
gh pr create --title "Plan 3B: installers, recovery kit, passphrase rotation, jobs and digest" --base master
```
  CodeRabbit app; one batched fix commit if needed; Hody merges.
- [ ] **Step 4: Vault** — the full triad: Admin-Overview (hosted targets, installers, kit, jobs, digest, day-to-day ops), Full-Dev-Guide (the M3–M5 build log), Issues-and-Fixes (**symptom → source map** for everything hit), plus `Claude-Project-Info.md` and `00-Homelab-Index.md`. Run the checklist in `hody/KB/Project-Completion-Audit.md` before the closing `INGEST` line.

---

## M6 — site, READMEs, screenshots (own PRs; may run in parallel with M4/M5)

### Task 34: Screenshot pipeline — **UI repo**

**Files (UI):** `scripts/screenshots.sh`, `scripts/shots/{seed.ts,capture.ts}`, `docs/screenshots/` (committed), `docs/screenshots/index.md` (generated)

**Interfaces:**
```
scripts/screenshots.sh [--only <name>]
  -> docs/screenshots/<screen>@1440.png and <screen>@412.png for every screen
  -> docs/screenshots/index.md listing them
```

- [ ] **Step 1: Check what is already there.** `grep -n '"\(playwright\|puppeteer\|@playwright/test\)"' package.json`. Use whatever is present (§14.9); if nothing is, use the chrome-devtools driver already used for verification. **Do not add a heavy dependency for screenshots.** Record the decision in the task report.
- [ ] **Step 2: Seed.** `seed.ts` activates a throwaway Fleet server and creates **generic** demo data: devices `laptop-1`, `media-nuc`, `office-desktop` (one failing), two groups, templates, one hosted target and one mirrored, and reports spread over 30 days so the day strips are full. It asserts the target server is empty before seeding and refuses otherwise — that is what keeps real fleet data out of the captures.
- [ ] **Step 3: Capture** at 1440 px and 412 px: Fleet — Overview, Devices, Device detail, Groups, Policies/Templates, Targets, Settings, each Activate step, Login; solo — Snapshots, History, Browse/Restore, Policies, Tasks, Repository; the agent page; the tray menu (render `agent/tray`'s `Model`; a real SNI capture is a bonus, not a gate).
- [ ] **Step 4: Check.** `grep -ril "hody\|10\.99\|<FLEET_HOST>" docs/screenshots/` is empty (PNG metadata included), and every file in `index.md` exists. Optimize the PNGs (or emit WebP) and commit.
- [ ] **Step 5:** commit `-m "scripts: screenshot pipeline over seeded demo data"` — **UI PR 5**.

---

### Task 35: Both READMEs

**Files:** `README.md` in `hodyhq/warphold`; `README.md` in `hodyhq/warphold-ui`

- [ ] **Step 1: Product README** (main repo) per §10.5.2: two lines on what WarpHold is; the two ways to use it, each with a screenshot from the UI repo's `docs/screenshots/` and its install command from `get.warphold.com`; a short security-model summary (per-device isolation, append-only gateway, the escrow honesty paragraph the original spec's §7 requires, standalone restore); and a **"Built on Kopia"** section — short, factual, linking `NOTICE`, `LICENSE` and upstream. Re-read `NOTICE` rather than paraphrasing it (§14.10). Keep the quick start and the operations notes Plan 2 added.
- [ ] **Step 2: Developer README** (UI repo): what the Go module is and how the server consumes it, build/tag/bump, the design system (tokens, fonts, components), and a link to the screenshots index.
- [ ] **Step 3:** private-name grep clean in both; commit `-m "docs: product README with screenshots and the Kopia attribution"` / `-m "docs: developer README for the UI module"` — **two small PRs**.

---

### Task 36: `hodyhq/warphold-site`

**Files (new repo):** `index.html` (Home), `fleet.html` (Fleet), `assets/` (tokens CSS, self-hosted OFL fonts, screenshots copied from the UI repo), `CNAME`, `README.md`, `.github/workflows/pages.yml`. **No `get/` directory** — the install scripts are never copied here (D8).

- [ ] **Step 1:** Static, no build step beyond copying assets; Kinetic tokens and fonts from the UI repo; **no trackers and no third-party requests** — assert with `grep -rn "https\?://" index.html fleet.html | grep -v warphold.com | grep -v github.com` returning only the deliberate links.
- [ ] **Step 2: Home tab** — the single-machine app: what it does, features, screenshots, the `app.sh` one-liner, the tray. **Fleet tab** — the dashboard, the isolation and security model, hosted and cloud-direct storage, enrollment, the `fleet.sh` one-liner. Full feature detail per tab with the real screenshots, not stubs.
- [ ] **Step 3:** Footer *"Built on the Kopia engine — Apache 2.0"* with links to upstream, `LICENSE` and `NOTICE`.
- [ ] **Step 4:** The site **links to the two install commands and hosts neither script** — `get.warphold.com/fleet.sh` and `/app.sh` already redirect to the release assets (Task 17), and the scripts' only home is `scripts/install/` in the main repo. Copying them here would create the second copy D8 exists to prevent. The install boxes are `<pre>` blocks with a copy button and nothing behind them.
- [ ] **Step 5:** commit `-m "site: warphold.com with Home and Fleet tabs"`

---

### Task 37: Domain wiring and the site smoke check — **HUMAN CHECKPOINTS**

**HUMAN CHECKPOINTS:** (a) **Cloudflare DNS** — apex and `www` → Pages, and `warphold.dev` → 301 to `warphold.com`. The `get.warphold.com` rules were set up in Task 17 and are **not touched here**; (b) **GitHub Pages custom-domain verification** and HTTPS enforcement for `warphold.com`.

- [ ] **Step 1:** `CNAME` in the repo, Pages enabled from `main`, ask Hody for the DNS and redirect rules, then poll until the certificate is valid:
```bash
until curl -fsS -o /dev/null -w '%{http_code}\n' https://warphold.com/ | grep -q 200; do sleep 20; done
```
- [ ] **Step 2: Site smoke check** (show every line of output):
```bash
curl -fsSL https://get.warphold.com/fleet.sh | head -5      # must print the script, through the redirect
curl -fsSL https://get.warphold.com/app.sh  | head -5
curl -fsS -o /dev/null -w '%{http_code} %{url_effective}\n' -L https://warphold.dev/   # 200, ends at warphold.com
curl -fsS https://warphold.com/fleet.html -o /dev/null -w '%{http_code}\n'
```
  The two `get.` lines are a **regression check on Task 17**, not new setup: they prove the apex work did not break the install redirects.
- [ ] **Step 3:** Confirm the site hosts no copy of either install script: `test -d get && echo "FAIL: scripts copied into the site" || echo ok`.
- [ ] **Step 4: Vault** — Admin-Overview gains a "Public surfaces" section (warphold.com, get.warphold.com, the GitHub repos) with the DNS shape as placeholders; `INGEST` line. Cross-check the `private-infra-not-public` rule before anything goes live: `grep -rn "hody\.sh" .` in the site repo must be empty.

---

### Task 38: Close out Plan 3

- [ ] **Step 1:** Run `hody/KB/Project-Completion-Audit.md` over the whole plan: doc triad covered, symptom → source map present in Issues-and-Fixes, git clean, no secrets, `[[wikilinks]]` resolve.
- [ ] **Step 2:** Update the Claude memory `warphold-project.md`: Plan 3 built and live-verified, the hosted gateway, the installers, the site, and what Plan 4 (Windows/macOS agents, restore through the dashboard) inherits.
- [ ] **Step 3:** Final `INGEST` line in `hody/00-Log.md`.

---

## Deferred beyond Plan 3

- **"Only on AC" scheduling on the agent** (carried from Plan 2) — needs an upstream policy field or a WarpHold-side scheduler wrapper; neither is worth opening in this plan.
- **Restore through the dashboard** (original spec sub-project 3) — full, single-file, and restore-to-a-new-machine.
- **Windows and macOS agents with signed installers** (original spec sub-project 4).
- **Horizontal scaling of the Fleet server** — more than one host in front of one store; SQLite is the constraint (§11).
- **S3 multipart upload and bulk delete** — only if Task 1 or a future Kopia version makes them necessary (§14.2).
- **Cross-device dedup** — ruled out by D2 and not revisitable without changing the isolation model.
- **Sharding the local backend's per-device directory** — the upgrade path if any device exceeds ~100 k blobs (§11).
