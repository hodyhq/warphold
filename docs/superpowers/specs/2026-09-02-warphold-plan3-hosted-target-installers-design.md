# WarpHold — hosted target, installers, kit and jobs (sub-project 2+3, "Plan 3")

- **Status:** DESIGN 2026-09-02 — written against the code as it stands at the end of Plan 2
- **Author:** Hody with Claude Code
- **Extends:** `docs/superpowers/specs/2026-09-01-warphold-core-fleet-design.md` (the "original spec"). Section references of the form "original §N" point there.
- **Implemented by:** `docs/superpowers/plans/2026-09-02-warphold-plan3-hosted-target-installers.md`

## 1. What this is

Plan 1 built the Fleet control plane and the Linux agent. Plan 2 built the UI, the tray and the homelab deployment. Everything the original spec deferred is now in one place: the **hosted target** (devices back up *to the Fleet server* instead of to a cloud bucket they hold credentials for), the **hybrid B2 mirror**, the **one-command installers** for both the Fleet server and the single-machine app, the **recovery kit** with passphrase rotation and a standalone-restore CI test, and the **scheduled jobs** (verify, test-restore, maintenance, weekly digest) with SMTP settings and repository stats.

It is deliberately one plan in five ordered milestones rather than three plans, because the milestones share one data model and one deployment: M1 gives hosted storage, M2 gives it an offsite copy, M3 makes both installable in one command, M4 makes the result recoverable without WarpHold, and M5 makes it observable. Each milestone ends live-testable on real hardware.

> **The non-negotiable from the original spec still holds.** Fleet must never be required for a restore. A person holding the recovery kit restores with a stock upstream `kopia` binary and nothing else. §12 turns that into a CI test that fails the build.

## 2. Decisions

Each of these was made with Hody on 2026-09-02 and is binding on the plan.

| # | Decision | Consequence |
|---|---|---|
| D1 | **One plan, five ordered milestones**, each ending live-testable: M1 hosted target → M2 hybrid B2 mirror → M3 installers + setup wizard → M4 recovery kit + passphrase rotation + standalone-restore CI test → M5 jobs, SMTP, weekly digest, repository stats. Shipped as **two server PRs** — PR A = M1+M2, PR B = M3+M4+M5 — plus small separate UI PRs. | The plan's task list is milestone-ordered; PR boundaries are marked in it. |
| D14 | **M6 — site, READMEs, screenshots** (added 2026-09-02, after the first five were fixed). A screenshot pipeline in the UI repo, both READMEs rewritten, and a static site at **warphold.com**. It runs **in parallel with M4/M5** and ships as **its own small PRs**, never inside PR A or PR B. | §13 gains M6; §10.5 specifies it. |
| D2 | **Complete per-device isolation.** One Kopia repository per device, with its own random repository password. Nothing is shared between devices: not a key, not a repository, not a content index. | Dedup is per device, not fleet-wide. Accepted: a family fleet's cross-machine duplication is small next to the blast radius of a shared repository. |
| D3 | **The Fleet server is an S3-compatible gateway**, mounted at `/s3/` on the server that already exists. SigV4 auth; one access key id + secret per device, sealed in the store, scoped to the prefix `<device-id>/`. Stateless: no per-device process, no per-device state beyond a SQLite row and a small in-memory cache. Rate-limited per device. Every request logged with its device id. Must scale to 1000+ devices (§11). | New package `fleet/gateway`. Devices run Kopia's **stock** S3 backend (`repo/blob/s3`) — no WarpHold-specific client code on the device. |
| D4 | **Task 1 is a spike.** Run stock Kopia (`repository create s3`, snapshots, `snapshot list`) against a minimal append-only S3 store and record which blob-name classes, if any, Kopia must `DeleteObject` or overwrite to complete a snapshot. The gateway then allows DELETE for **exactly** those classes and nothing else. | The append-only rule in §4.3 is written from the spike's output, not from reading the code. Every later gateway task consumes the spike's recorded class list. |
| D5 | **Backing store per hosted target**, chosen at target creation and in the setup wizard, default **Fleet disk**: (a) *Fleet disk* — root `/srv/warphold/hosted/` (configurable), with an optional **B2 mirror job**; (b) *cloud-direct* — the gateway writes through to a B2 or S3 bucket with the fleet's single admin key. **Devices never hold cloud credentials in either mode.** | `targets` gains a storage mode and mirror configuration (§5). |
| D6 | **Revoking a device disables its gateway key immediately.** Its repository stays for the retention window (default 30 days), then a server job removes it. | Revocation is two-phase: `device_keys.disabled_at` now, repository deletion later. |
| D7 | **`public_url` is a required setting.** Asked in the wizard and validated end to end (the server fetches `/api/v1/fleet/status` *through* it; on failure it shows the proxy requirements), or set via `warphold fleet activate --public-url` / `WARPHOLD_PUBLIC_URL`. It drives the S3 endpoint handed to devices, the enrollment one-liner, the tray's Details link, the secure-cookie decision, the CSRF origin check and Host validation. **Enrollment tokens cannot be issued until it is set.** | §6. |
| D8 | **Distribution:** goreleaser on tags publishes tarballs + deb + rpm + signed checksums for **linux/{amd64,arm64}** to GitHub Releases, and attaches `fleet.sh` and `app.sh` as release assets. The scripts' single source of truth is the main repo (`scripts/install/{fleet,app}.sh`) — they are never copied anywhere. **`get.warphold.com` is a Cloudflare redirect**, not a repo and not a site: `/fleet.sh` and `/app.sh` redirect to `releases/latest/download/…`, and `curl -fsSL` follows it. Hody owns `warphold.com` (canonical) and `warphold.dev` (redirect). Fleet servers keep serving `enroll.sh` and `/dl/` from their own domain, built from `public_url`. | §8, §10.5. |
| D9 | **`fleet.sh`** detects arch and distro, downloads and verifies the checksum, creates the `warphold` user and directories, writes a systemd unit (LAN bind, `--insecure`, reverse proxy assumed — it prints the proxy requirements), starts it, and prints the URL and setup token. Non-interactive via `WARPHOLD_SETUP_*` environment variables, which drive `warphold fleet activate`. | §8.2. |
| D10 | **Setup wizard order** (the existing Activate wizard, extended): (1) setup token + passphrase ×2, (2) first admin, (3) public URL (tested), (4) storage — Fleet disk [+ optional B2 mirror] or cloud-direct (B2/S3), (5) done, with the first group's enrollment one-liner. Setup also creates **the Fleet host's own local repository**, so hosted targets work immediately. | §8.4. |
| D11 | **`app.sh` + deb/rpm** install the single-machine app: binary + user-scope engine service + tray autostart, then open the local app. The Repository page's "Turn this machine into a Fleet server" card **moves to Settings** as a link to the Fleet install docs. The standalone app never runs the Fleet server. | §8.3. |
| D12 | **Acceptance device is the FW16** (the old CachyOS laptop) — never the FW13 Pro. M1 acceptance: the FW16 enrolls against the Fleet host, completes a real home-directory snapshot, and the tray shows it. M3 acceptance: a fresh VM becomes a Fleet server from one command, and a fresh Linux machine gets the app from one command. | Both are HUMAN CHECKPOINTS in the plan. |
| D13 | **Upstream discipline unchanged.** Still only the six touched upstream files. Everything new lives in `fleet/`, `agent/`, `cli/command_*`, and new files under `internal/server/` marked `warphold:`. The gateway is a new package `fleet/gateway` (SigV4 verification + an object-store abstraction with `local` and `cloud` backends), reusing `fleet/b2api` and reading Kopia's own `repo/blob/{s3,b2,filesystem}` for the client side. The server side is our code. | §3.1. |

## 3. Architecture

### 3.1 New and changed packages

```
fleet/gateway/          NEW — the S3-compatible gateway
  sigv4.go              AWS4-HMAC-SHA256 request verification (server side)
  keys.go               access key id → device id + secret, sealed, LRU-cached
  object.go             ObjectStore interface + shared key/prefix validation
  local.go              disk backend rooted at the target's path
  cloud.go              write-through backend over repo/blob/{s3,b2} with the admin key
  handler.go            /s3/ router: Put/Get/Head/List/Delete + GetBucketVersioning
  xmlout.go             the S3 XML responses and error codes we emit
  limit.go              per-device token bucket
fleet/jobs/             NEW — the scheduler and the jobs from original §3.3
  scheduler.go          due-job loop, one goroutine, SQLite-backed
  verify.go             snapshot verify per repository
  testrestore.go        restore one random small file, compare hashes
  maintenance.go        maintenance run --full per repository
  mirror.go             Fleet disk → B2 mirror under Object Lock
  stats.go              repository stats collection (Stored tiles)
  reap.go               revoked-device repository removal after the retention window
  digest.go             weekly fleet-status email
fleet/kit/              NEW — recovery-kit HTML render
fleet/mail/             NEW — SMTP client + settings (SMTP2GO-style defaults)
fleet/enroll/           CHANGED — hosted provisioning path
fleet/store/            CHANGED — schema additions + additive column migration
fleet/api/              CHANGED — public_url, hosted targets, kit, jobs, stats endpoints
agent/                  CHANGED — `verify` command execution; no maintenance on devices
cli/command_fleet_*.go  CHANGED — `--public-url` on activate; `fleet jobs run` for operators
```

`internal/server/warphold_spa_routes.go` is unchanged. `.goreleaser.yml` (already a touched upstream file) gains packaging and signing detail; no other upstream file is touched by this plan.

### 3.2 Where the bytes go

```
device ──HTTPS──▶ reverse proxy ──▶ warphold server
  stock Kopia S3 backend            /s3/  (fleet/gateway)
  endpoint  <public_url>/s3/          │
  bucket    warphold                  ├─ local backend  → /srv/warphold/hosted/<device-id>/…
  prefix    <device-id>/              │      └─ optional mirror job → b2://<bucket>/<device-id>/ (Object Lock)
  keys      per-device SigV4 pair     └─ cloud backend  → b2:// or s3:// with the fleet admin key
```

This is the one place WarpHold becomes a data plane. The original spec's "control plane, not data plane" line was scoped to sub-project 1; hosted mode is the sub-project that changes it, and it changes it for hosted targets only. `b2` and `filesystem` targets are untouched and still bypass the Fleet server entirely.

### 3.3 Why an S3 gateway rather than a Kopia repository server

The original spec sketched hosted mode as "Fleet server as Kopia repository server, then `sync-to` B2". Rejected here for three reasons, all of which the gateway avoids:

1. Kopia's repository server holds server-side state per connected user and re-encrypts on behalf of that user; the gateway holds none and never sees a repository password.
2. The repository server multiplexes many users over one repository, which contradicts D2's one-repository-per-device isolation.
3. The device would need a WarpHold-specific client. With the gateway, the device runs stock `repo/blob/s3` — which is also what makes the recovery kit (§9) work with an upstream `kopia` binary.

The cost is that WarpHold must implement enough of S3 to satisfy minio-go, which is the client library `repo/blob/s3` uses. §4 pins that surface exactly.

## 4. The gateway

### 4.1 Request flow

1. The request arrives at `/s3/<bucket>/<key>` (or `/s3/<bucket>?list-type=2&…`). The bucket name is a fixed constant, `warphold`; a request for any other bucket is `404 NoSuchBucket`.
2. `sigv4.Verify` parses the `Authorization` header (`AWS4-HMAC-SHA256 Credential=<akid>/<date>/<region>/s3/aws4_request, SignedHeaders=…, Signature=…`), rejecting anything that is not `AWS4-HMAC-SHA256`, whose `X-Amz-Date` is more than 15 minutes from the server clock, or whose signed-header set omits `host` or `x-amz-content-sha256`.
3. `keys.Lookup(akid)` resolves the access key id to `{agent_id, prefix, secret}`. The secret is stored sealed (`seal.Key.Seal`) and is only ever unsealed into a short-lived buffer inside the lookup; the LRU cache holds the unsealed secret for at most 5 minutes (§11). A `disabled_at` row is a lookup miss.
4. The canonical request is rebuilt and the signature compared with `hmac.Equal`. Failure is `403 SignatureDoesNotMatch` with no detail.
5. **Prefix confinement:** the object key must begin with exactly `<prefix>` (`<agent-id>/`) after normalisation. Normalisation rejects `..`, an empty segment, a leading `/`, any byte below 0x20, and anything longer than 1024 bytes. A `ListObjectsV2` `prefix` parameter that does not start with the device's prefix is *replaced* by the device's prefix rather than erroring, and the returned keys are always re-checked against it. This is a defence-in-depth pair: the key check is the boundary, the list rewrite prevents accidental enumeration.
6. The verb-specific rules of §4.2 apply, then the request goes to the `ObjectStore` (local or cloud).
7. Every request logs `device_id`, verb, key, byte count, status and duration. Keys and signatures are never logged; the access key id is logged, the secret never.

Steps 2–5 run before any storage call, so an unauthenticated or out-of-prefix request never touches disk.

### 4.2 The exact S3 subset

| Operation | Allowed | Rules |
|---|---|---|
| `PutObject` | yes | **Only if the key does not already exist.** An existing key answers `409 ObjectAlreadyExists` — except for the blob-name classes the D4 spike proves Kopia must overwrite, which are listed in §14 and allowed byte-for-byte by name pattern. No multipart upload: `POST ?uploads` is `501 NotImplemented` (Kopia's S3 backend uploads whole blobs; if the spike shows otherwise, §14). |
| `GetObject` | yes | Including `Range:` (`bytes=a-b`, single range only; a multi-range request is `501`). |
| `HeadObject` | yes | Returns `Content-Length`, `ETag` (the MD5 of the stored object), `Last-Modified`. |
| `ListObjectsV2` | yes | Prefix-confined per §4.1.5. `max-keys` capped at 1000. Continuation tokens are opaque and signed so they cannot be used to escape the prefix. `delimiter` supported (Kopia does not use it, minio-go may send it). |
| `DeleteObject` | **denied by default** | `403 AccessDenied`, unless the key matches one of the spike-derived classes in §14. `POST ?delete` (bulk) is always `501`. |
| `GetBucketVersioning` | yes | Kopia's `IsVersioned` calls it (`repo/blob/s3/s3_versioned.go:31`). Local backend answers `Suspended`; cloud-direct answers what the underlying bucket says. |
| `PutObjectRetention` | conditional | Kopia calls it for blob retention (`s3_storage.go:239`). Local backend: `501 NotImplemented` (retention is the mirror job's Object Lock, §7). Cloud-direct: passed through. |
| everything else | no | `403 AccessDenied` for anything else under `/s3/`, and no S3 API is exposed outside `/s3/`. |

**Append-only is the point.** A device's key can write new blobs and read its own; it cannot overwrite history and cannot delete outside the narrow spike-derived set. That is a stronger guarantee than the B2 target's writer key, which — as the original spec's §7 admits — can hide every blob it wrote because B2 implements `DeleteBlob` as `b2_hide_file`.

### 4.3 Storage layout

**Fleet disk** (target `storage_mode = "disk"`, root from `targets.path`, default `/srv/warphold/hosted`):

```
/srv/warphold/hosted/<device-id>/<blob-name>        the Kopia repository's blobs, flat
/srv/warphold/hosted/<device-id>/kopia.repository   the format blob
```

Written with `O_EXCL` to a temporary name in the same directory and then `rename(2)`, so a half-written blob is never visible and the "only if the key does not exist" rule is enforced by the filesystem rather than by a check-then-write race. Directory mode `0700`, file mode `0600`, owned by the `warphold` service user.

**Mirror** (optional, `storage_mode = "disk"` only): `b2://<mirror-bucket>/<device-id>/<blob-name>`, Object Lock on, verified when the mirror is configured.

**Cloud-direct** (`storage_mode = "cloud"`): `<bucket>/<device-id>/<blob-name>` in the target's B2 or S3 bucket, written by the gateway with the fleet's single admin key. Object Lock is verified at target creation exactly as the existing `b2` target does.

The device sees the same key space in all three modes: `<device-id>/<blob-name>` under bucket `warphold` at `<public_url>/s3/`. Switching a target's backing store therefore does not change anything a device holds.

## 5. Data model additions

`fleet/store/schema.sql` is `CREATE TABLE IF NOT EXISTS` throughout, which is idempotent for new tables but silently skips new *columns* on an existing database. This plan therefore adds a small additive migration in `store.Open`: for each `(table, column, type, default)` in a fixed list, `PRAGMA table_info(<table>)` and, if absent, `ALTER TABLE <table> ADD COLUMN …`. No column is ever dropped or retyped; no data migration is ever run.

**New tables**

```sql
CREATE TABLE IF NOT EXISTS device_keys (
  access_key_id TEXT PRIMARY KEY,
  agent_id      TEXT NOT NULL REFERENCES agents(id),
  sealed_secret BLOB NOT NULL,
  prefix        TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  disabled_at   TEXT);
CREATE INDEX IF NOT EXISTS device_keys_agent ON device_keys(agent_id);

CREATE TABLE IF NOT EXISTS jobs (
  id            INTEGER PRIMARY KEY,
  kind          TEXT NOT NULL,            -- verify|test-restore|maintenance|mirror|stats|digest|reap
  agent_id      TEXT REFERENCES agents(id),
  scheduled_for TEXT NOT NULL,
  started_at    TEXT, finished_at TEXT,
  status        TEXT NOT NULL DEFAULT 'pending',  -- pending|running|ok|error|skipped
  detail        TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS jobs_due ON jobs(status, scheduled_for);
CREATE INDEX IF NOT EXISTS jobs_agent ON jobs(agent_id, finished_at DESC);

CREATE TABLE IF NOT EXISTS kit_acks (
  agent_id        TEXT PRIMARY KEY REFERENCES agents(id),
  acknowledged_at TEXT NOT NULL,
  acknowledged_by INTEGER NOT NULL REFERENCES admins(id));

CREATE TABLE IF NOT EXISTS repo_stats (
  agent_id      TEXT PRIMARY KEY REFERENCES agents(id),
  collected_at  TEXT NOT NULL,
  logical_bytes INTEGER NOT NULL DEFAULT 0,
  stored_bytes  INTEGER NOT NULL DEFAULT 0,
  blob_count    INTEGER NOT NULL DEFAULT 0,
  mirrored_at   TEXT,
  mirrored_bytes INTEGER NOT NULL DEFAULT 0);
```

**New columns (via the additive migration)**

| Table | Column | Meaning |
|---|---|---|
| `targets` | `storage_mode TEXT NOT NULL DEFAULT ''` | `disk` or `cloud`, only meaningful when `kind = 'hosted'` |
| `targets` | `mirror_kind TEXT NOT NULL DEFAULT ''` | `b2` or empty (no mirror) |
| `targets` | `mirror_bucket TEXT NOT NULL DEFAULT ''` | mirror bucket name |
| `targets` | `mirror_region TEXT NOT NULL DEFAULT ''` | mirror region, unused for B2 |
| `targets` | `sealed_mirror_key BLOB` | sealed `{key_id, key}` for the mirror |
| `targets` | `mirror_lock_verified_at TEXT` | Object Lock verification of the mirror bucket |
| `agents` | `retired_at TEXT` | set by the reap job when the repository is removed (D6) |

`targets.kind` gains the value `hosted` alongside the existing `b2` and `filesystem`. `store.Target` gains the matching Go fields; `TargetInput`/`Target` in the UI's `src/api/types.ts` gain `kind: "hosted"`, `storage_mode`, `mirror_*` and `mirror_lock_verified_at`.

**Settings keys** (in the existing `settings` table): `public_url`, `hosted_root`, `smtp_host`, `smtp_port`, `smtp_username`, `sealed_smtp_password`, `smtp_from`, `smtp_tls`, `digest_recipients`, `digest_weekday`, `digest_hour`, `revoked_retention_days`, `verify_interval_days`, `test_restore_interval_days`, `maintenance_interval_hours`, `mirror_interval_hours`, `stats_interval_hours`.

## 6. `public_url`

One setting, several consumers, and the plan wires each one explicitly:

| Consumer | Use |
|---|---|
| Gateway endpoint | `<public_url>/s3/` is what goes into the device's sealed bundle |
| `enroll.sh` | the `Server` value templated into the script (today it is derived from the `Host` header; `public_url` takes precedence when set) |
| Enrollment one-liner | shown in the Groups screen and the wizard's last step |
| Tray "Details" | unchanged (it is a localhost URL), but the device page links back to `<public_url>` |
| Cookies | `Secure` is set when `public_url` is `https://`, and not otherwise |
| CSRF | **New.** `fleet/api/csrf.go` is double-submit only today; it gains an origin check applied inside the existing `requireCSRF` middleware for non-`GET`/`HEAD`/`OPTIONS`: if `Origin` is present it must equal `public_url`'s scheme+host; else if `Referer` is present, its origin must match; if **both are absent the request passes**, because non-browser clients (curl, the agent) rely on the double-submit token alone. With `public_url` unset the check is skipped and a warning is logged. |
| Host validation | a request whose `Host` matches neither `public_url`'s host nor a loopback address is `421 Misdirected Request` |

**Validation** is end-to-end, not a regex: the server issues `GET <public_url>/api/v1/fleet/status` from its own process, with a 10-second timeout and redirects disabled, and requires a `200` whose body has `"activated"`. On failure the response carries the proxy requirements verbatim — forward `Host`, forward the full path including `/s3/`, do not buffer request bodies, allow at least a 5 GiB body and a 30-minute read timeout — so the wizard can print them.

**Gate:** `POST /api/v1/fleet/tokens` returns `409 {"error":"set the public URL before issuing enrollment tokens"}` while `public_url` is empty. Set it via the wizard, `warphold fleet activate --public-url <url>`, `WARPHOLD_PUBLIC_URL`, or `PUT /api/v1/fleet/settings`.

## 7. Provisioning, revocation and mirroring

### 7.1 Hosted provisioning (extends `fleet/enroll.Provisioner.Provision`)

For `t.Kind == "hosted"`:

1. Generate the repository password (`NewPassword`, unchanged — 32 CSPRNG bytes, base64url).
2. Generate a gateway key pair: access key id = `WH` + 18 base32 characters (20 chars total, S3-shaped), secret = 40 base64url characters from `crypto/rand`. Insert into `device_keys` with `prefix = agentID + "/"` and the secret sealed.
3. Build the agent's `blob.ConnectionInfo` as `{Type: "s3", Config: &s3.Options{BucketName: "warphold", Prefix: agentID + "/", Endpoint: <host of public_url>, DoNotUseTLS: <public_url is http>, AccessKeyID: akid, SecretAccessKey: secret, Region: "warphold"}}`.
4. Create the repository **through the gateway's own `ObjectStore`, not over HTTP** — the Fleet host has direct storage access, so `initialize` uses a `filesystem`/`b2`/`s3` connection info pointing at the same bytes. This keeps repository creation off the network path and means a broken proxy fails at step 6 rather than half-way through `repository create`.
5. Set Fleet's own user as maintenance owner (existing `initialize` behaviour) so **devices run no maintenance** — D3.
6. Seal the bundle (connect token, repository password, prefix, access key id and secret) into `agents.sealed_bundle` and return it to the device.

The bundle format gains `GatewayKeyID` / `GatewayKey` / `Endpoint` fields next to the existing `WriterKeyID` etc.; `b2` and `filesystem` targets keep using the fields they use today.

### 7.2 Revocation (D6)

`POST /api/v1/fleet/agents/{id}/revoke` today revokes the bearer token and deletes the B2 keys. It gains: set `device_keys.disabled_at = now` for every key of that agent (effect is immediate — the LRU cache is invalidated by agent id on write), and schedule a `reap` job for `now + revoked_retention_days` (default 30). The reap job deletes `<hosted root>/<device-id>/` (and, if mirrored, leaves the mirror alone — Object Lock retention governs it) and sets `agents.retired_at`. Until the reap job runs, the device page shows "revoked — repository kept until <date>".

### 7.3 Mirror job (M2)

For every `hosted` target with `storage_mode = "disk"` and a `mirror_kind`, on the `mirror_interval_hours` schedule: walk each device prefix on disk, list the mirror bucket for that prefix, and upload anything missing. Append-only, so the diff is "keys present locally and absent remotely"; nothing is ever deleted from the mirror by WarpHold. Uploads set Object Lock retention where the bucket has it configured, which is verified at configuration time exactly as the `b2` target's is.

Device and target health gain an offsite dimension: `repo_stats.mirrored_at` and `mirrored_bytes`, surfaced as "local ✓ / offsite ✓ 2 h ago" on the target and device screens. A mirror that has not completed in 3× its interval is a yellow target; a mirror job that errors is red.

## 8. Installers and setup

### 8.1 Release artifacts (D8)

`.goreleaser.yml` on a tag produces, for **`linux/{amd64,arm64}` only** — the `builds` block is pruned from upstream's linux/freebsd/openbsd × amd64/arm/arm64 matrix, with darwin and windows kept as CI compile checks and never published — a tarball, a `.deb`, a `.rpm`, and `checksums.txt` signed by `tools/sign.sh` (the signing key is a HUMAN CHECKPOINT — it lives in 1Password and is provided to CI as a secret). `nfpms` already exists; it gains WarpHold's homepage/vendor/maintainer/description, a `postinstall` that creates the `warphold` user for the system-scope case, and `conflicts: kopia`.

**The install scripts live in the main repo and nowhere else:** `scripts/install/fleet.sh` and `scripts/install/app.sh`, versioned with the code they install and tested in CI against it. goreleaser attaches both to **every GitHub Release** as assets, so `releases/latest/download/fleet.sh` always resolves to the script that shipped with the latest binary.

`get.warphold.com` is therefore **not a repo and not a site** — it is a pair of Cloudflare redirect rules: `get.warphold.com/fleet.sh` → `https://github.com/hodyhq/warphold/releases/latest/download/fleet.sh`, and the same for `app.sh`. `curl -fsSL` follows the redirect, so the one-liner works and there is exactly one copy of each script in existence. Each script resolves the latest release through the GitHub API once and pins the version it downloaded into its own output, so a re-run is reproducible and a failure is loud.

### 8.2 `fleet.sh` (D9)

```
curl -fsSL https://get.warphold.com/fleet.sh | sh
```

1. Detect `uname -m` → `amd64`/`arm64`; detect the packaging family (`apt`, `dnf`, else tarball).
2. Resolve the latest release, download the artifact and `checksums.txt`, verify the checksum (`sha256sum -c`), refuse to continue on a mismatch.
3. Create the `warphold` system user, `/var/lib/warphold` (0750), `/srv/warphold/hosted` (0700), `/etc/warphold` (0750).
4. Write `/etc/systemd/system/warphold.service` binding to the LAN address with `--insecure`, because a reverse proxy is assumed. **Print the proxy requirements** (the same list §6 sends on validation failure).
5. `systemctl enable --now warphold`, then poll `/api/v1/fleet/status` until it answers.
6. Print the URL to open and the setup token's path and value.

Non-interactive: when `WARPHOLD_SETUP_PUBLIC_URL`, `WARPHOLD_SETUP_EMAIL`, `WARPHOLD_SETUP_PASSWORD` and `WARPHOLD_SETUP_PASSPHRASE` are all set, the script skips step 6 and runs `warphold fleet activate --public-url …` instead, reading the secrets from the environment so they never appear in `ps`.

### 8.3 `app.sh` and the packages (D11)

```
curl -fsSL https://get.warphold.com/app.sh | sh
```

Installs the binary to `~/.local/bin` (or `/usr/local/bin` with `--system`), writes the user-scope engine service and the tray autostart entry (both already exist as `agent/install`), starts them, and opens the local app with `xdg-open`. The deb and rpm do the same for a system install, with the user-scope pieces left to `warphold agent install` on first run.

The standalone app **never** runs the Fleet server. The "Turn this machine into a Fleet server" card leaves the Repository page and becomes a line in Settings linking to the Fleet install documentation.

### 8.4 Setup wizard (D10)

Five steps, in this order, in the existing `src/pages/fleet/Activate.tsx`:

1. **Setup token + passphrase ×2.** Unchanged, plus the second passphrase field and a "this passphrase unlocks every device's repository password; losing it loses the escrow, not the backups" warning.
2. **First admin.** Email + password. Unchanged.
3. **Public URL.** Tested by the server (§6); on failure the proxy requirements are shown inline and the step cannot be passed.
4. **Storage.** *Fleet disk* (default; path defaults to `/srv/warphold/hosted`, with an optional "also mirror to B2" sub-form) or *cloud-direct* (B2 or S3 credentials and bucket, Object Lock verified before the step passes).
5. **Done.** Creates the Fleet host's own local repository (`filesystem` target + a "Fleet server" group + an agent record for the host itself), a default group on the chosen target, and shows that group's enrollment one-liner.

Steps 1 and 2 stay in the single `POST /activate` call; steps 3–5 are ordinary authenticated admin calls made after the session exists, so a wizard that dies at step 4 leaves an activated, logged-in Fleet rather than half-written state.

## 9. Recovery kit, escrow and rotation (M4)

Lifted from original §8 and updated for hosted targets.

**Contents.** Repository location (endpoint, bucket, prefix — or `b2://bucket/prefix` for a `b2` target, or the path for `filesystem`), repository password, the read credentials (the reader B2 key for `b2` targets; for hosted targets a **read-only gateway key**, generated on demand with the same `device_keys` row shape plus a `read_only` flag), literal copy-pasteable commands, and a plain-English paragraph. For a hosted target the commands read:

```
kopia repository connect s3 --bucket warphold --prefix <device-id>/ \
  --endpoint <public-host> --access-key <akid> --secret-access-key <secret>
kopia snapshot list
kopia restore <snapshot-id> <dest>
```

**Rendering.** Server-side single print-ready HTML page from `fleet/kit`, served by `GET /api/v1/fleet/agents/{id}/kit`. No PDF dependency — the browser makes the PDF. The page has no external resources at all (inline CSS, no fonts, no images beyond an inline SVG mark), so it prints correctly from a machine with no network.

**Ack.** `POST /api/v1/fleet/agents/{id}/kit/ack` writes `kit_acks`. The dashboard shows a per-device banner until it is acked; the Devices list shows an un-acked marker.

**Passphrase rotation.** `POST /api/v1/fleet/settings/passphrase {current, new}`: verify `current` derives the loaded key, derive the new key from a **new salt**, then in one SQLite transaction re-seal every sealed value (`agents.sealed_bundle`, `targets.sealed_admin_key`, `targets.sealed_mirror_key`, `device_keys.sealed_secret`, the sealed SMTP password), write the new salt, and only then replace `seal.key` — writing the key file last, exactly as `Activate` does, so a crash mid-rotation leaves the old key file and the old sealed values consistent. A `--dry-run` variant unseals and re-seals in memory and reports the count without writing, and the CLI form is `warphold fleet rotate-passphrase`.

## 10. Jobs, SMTP and digest (M5)

**Scheduler.** One goroutine started with the Fleet server, waking every 60 seconds: claim due `jobs` rows (`UPDATE … SET status='running' WHERE id=? AND status='pending'`, so a claim is atomic even with two schedulers), run them one at a time with a per-kind timeout, write the outcome, and enqueue the next occurrence. Job intervals come from settings (§5). Manual runs: `POST /api/v1/fleet/jobs {kind, agent_id?}` and `warphold fleet jobs run --kind verify --agent <id>`.

| Job | Default cadence | Does |
|---|---|---|
| `verify` | weekly per agent | `snapshot verify` against the agent's repository, with Fleet's direct storage access |
| `test-restore` | monthly per agent | restore one random small file to a temp dir, compare its hash with the snapshot manifest |
| `maintenance` | daily per repository | `maintenance run --full`; Fleet is the maintenance owner, so devices never run it |
| `mirror` | hourly per mirrored target | §7.3 |
| `stats` | every 6 h per agent | repository stats → `repo_stats` → the Stored tiles |
| `digest` | weekly | the fleet-status email |
| `reap` | on demand | remove a revoked device's repository after the retention window (D6) |

The `verify` **agent command** (original §6 lists it in the protocol but Plan 1 never executed it) is separate: it is a device-side verification of the device's own repository, run through the local control API and acknowledged in the next report, and it is what the Devices screen's "Verify" button queues.

**SMTP settings** with SMTP2GO-style defaults: host `mail.smtp2go.com`, port `2525` (with 587 and 465 offered), TLS on, username + password, a `From` address, and a "Send test email" button that reports the raw SMTP error on failure. The password is sealed. Nothing is sent until the settings validate.

**Weekly digest**: one HTML+text email per `digest_weekday`/`digest_hour`, listing per device — health, last successful snapshot, bytes, failures this week — plus fleet totals, un-acked recovery kits, and any job that has been failing for more than a week. Recipients are a comma-separated setting; the admin's own address is the default.

**Repository stats** fill `stored_bytes` and `dedup_ratio` in `GET /api/v1/fleet/overview` and `size_bytes` in the Devices rows — the fields the UI already declares and currently renders as an em dash (`src/pages/fleet/Devices.tsx:232`, `Overview.tsx:144`).

## 10.5 Site, READMEs and screenshots (M6)

M6 is the public face of everything M1–M5 built. It touches no server behaviour, runs in parallel with M4/M5, and ships as its own PRs.

### 10.5.1 Screenshot pipeline

`scripts/screenshots.sh` in the UI repo, driving the **chrome-devtools MCP driver already used for visual verification**. The UI repo's `devDependencies` carry vitest and nothing else for browsers, and **no new Node dependency is added** — not Playwright, not Puppeteer. It:

1. Starts a local Fleet server on a temp state directory, activates it, and seeds **generic demo data**: devices `laptop-1`, `media-nuc`, `office-desktop` (one of them failing, so the red path is visible), two groups, policy templates, a hosted target and a mirrored one, reports spread over 30 days so the day strips are full.
2. Starts a second, solo server with a small seeded repository for the single-machine screens.
3. Signs in with the seeded test credentials and captures every screen at **1440 px** and **412 px**: Fleet — Overview, Devices, Device detail, Groups, Policies/Templates, Targets, Settings, each Activate step, Login; solo — Snapshots, History, Browse/Restore, Policies, Tasks, Repository; the agent page; and the tray menu (a render of `agent/tray`'s `Model` is acceptable; a real SNI capture is a bonus, not a gate).
4. Writes optimized PNG/WebP to **`docs/screenshots/`** in the UI repo, committed, with a generated `docs/screenshots/index.md` naming each file.

No real hostname, IP, email or device name from Hody's fleet appears in any capture — the seed data is the only data, and the script fails if the seeded server is not empty at start.

Both READMEs and the site consume the same files, so a re-run refreshes every surface at once.

### 10.5.2 READMEs

- **`hodyhq/warphold`** — product README: what WarpHold is in two lines; the two ways to use it (single machine, Fleet) each with a screenshot and its install command from `get.warphold.com`; a short security-model summary (per-device isolation, append-only gateway, escrow honesty, standalone restore); and a **"Built on Kopia"** section that is short, factual and correctly attributed — links to `NOTICE`, `LICENSE` and upstream, enough for legal and for trust, and deliberately not loud.
- **`hodyhq/warphold-ui`** — developer README: what the Go module is and how the server consumes it, how to build, tag and bump it, the Kinetic design system (tokens, fonts, components), and the screenshots index.

### 10.5.3 Website

New repo **`hodyhq/warphold-site`**: static, built with the Kinetic tokens and the self-hosted OFL fonts from the UI repo, **no trackers, no third-party requests**. Deployed by GitHub Pages to **warphold.com**.

- Two tabs. **Home** — the single-machine app: what it does, features, screenshots, the `app.sh` one-liner, the tray. **Fleet** — the dashboard, the isolation and security model, hosted and cloud-direct storage, enrollment, the `fleet.sh` one-liner. Full feature detail per tab with real screenshots, not marketing stubs.
- Footer: *"Built on the Kopia engine — Apache 2.0"* with links to upstream, `LICENSE` and `NOTICE`.
- The site **links to the two install commands and never hosts the scripts** — `get.warphold.com` redirects straight to the release assets (D8), so there is no second copy to drift.
- **DNS (HUMAN CHECKPOINT):** Cloudflare — apex and `www` → Pages; `warphold.dev` → 301 to `warphold.com`. The `get.warphold.com` redirect rules are set up earlier, in M3 (§8.1), and are unaffected by the site. GitHub Pages custom-domain verification and HTTPS enforcement are part of the same checkpoint, as is a `CNAME` file in the repo.
- **Site smoke check** after DNS: `curl -fsSL https://get.warphold.com/fleet.sh | head -5` must print the script through the redirect, and both tabs must load over HTTPS with a valid certificate.

## 11. Sizing and the 1000-device target

The gateway is the only component whose cost scales with device count.

- **Per request:** one SigV4 verification (two HMAC-SHA256 chains over a canonical request — microseconds), one key lookup, one storage operation. The key lookup hits an LRU cache of 4096 entries with a 5-minute TTL; a miss is one indexed SQLite read on a primary key. Cache invalidation is explicit on revoke and on key rotation, so the TTL is a safety net, not the mechanism.
- **Load estimate:** 1000 devices at one snapshot per hour, ~200 blob writes per incremental snapshot, is ≈55 write requests/second fleet-wide with peaks at the top of the hour. Poll jitter (already ±60 s in the agent) plus the scheduler's per-source spread keeps the peak inside single-digit multiples of the mean.
- **What is stateless:** everything. No per-device goroutine, no per-device connection, no session. A second Fleet host in front of the same storage would work for the gateway; SQLite is what makes that untrue today, and horizontal scaling is explicitly out of scope (§15).
- **Rate limit:** a token bucket per device — 50 requests/second sustained, burst 200 — returning `503 SlowDown` with `Retry-After`. minio-go retries on `SlowDown`, so a throttled device slows down rather than failing.
- **Disk:** the local backend is one directory per device with flat blob names; Kopia's default 20 MiB pack blobs keep the file count per device in the low thousands per TB. `ext4` with `dir_index` handles that without sharding. If a device's directory ever exceeds ~100 k entries, sharding is the upgrade path — noted, not built.
- **Not built:** connection pooling per device, a shared content cache, and any cross-device dedup — all of which D2 rules out by design.

## 12. Testing and CI

- **Unit:** SigV4 verification against known-answer vectors (the AWS test suite's canonical request examples), prefix normalisation and traversal rejection, the append-only PUT rule, the delete allowlist, `ListObjectsV2` prefix rewriting and pagination, the rate limiter, key sealing and rotation, the additive column migration (open an old DB, assert the columns appear and the rows survive), the job scheduler's atomic claim, and the digest's rendering.
- **Integration, no cloud:** a `hosted` target on a temp dir, a real Kopia S3 client pointed at the in-process gateway over `httptest`: create the repository, snapshot a tree, list snapshots, restore, and assert that no DELETE outside the allowed classes was attempted (the test's gateway records every denied request and fails on an unexpected one). This is the D4 spike promoted to a permanent regression test.
- **Standalone restore (required, original §12):** enroll a device against a hosted target, snapshot, read the recovery kit's values, then restore with a **pinned upstream `kopia` release binary** downloaded in CI, and hash-compare against the source tree. It fails the build if it needs anything from WarpHold. The hosted variant is the sharper test of the two: it proves the gateway is S3 enough for a stock client.
- **Installer:** `fleet.sh` and `app.sh` run under `shellcheck`, plus a container test that runs `fleet.sh` end to end against a locally served fake release and asserts an activated server.
- **UI:** vitest for the new screens' logic. Visual and flow proof (wizard → enrolled device; kit ack clears the banner) is captured with the chrome-devtools driver against a real logged-in server, not with a browser test framework — the UI repo has no e2e runner and this plan does not add one.

## 13. Milestones

| # | Delivers | Live-testable end |
|---|---|---|
| M1 | Spike, `fleet/gateway` (local backend), device keys, hosted target kind, hosted provisioning, `public_url` | The FW16 enrolls against the Fleet host on a hosted target and completes a real home snapshot; the tray shows it (D12) |
| M2 | Cloud-direct backend, mirror job, Object Lock verification, offsite health | A hosted target mirrors to a real B2 bucket; the target screen shows local ✓ / offsite ✓ — **PR A** |
| M3 | goreleaser packaging + signing, `get.warphold.com`, `fleet.sh`, `app.sh`, deb/rpm, wizard reorder, Settings link | A fresh VM becomes a Fleet server from one command; a fresh Linux machine gets the app from one command (D12) |
| M4 | Recovery kit + ack + banner, passphrase rotation, standalone-restore CI test | CI restores a hosted-target snapshot with a stock upstream `kopia` binary |
| M5 | Jobs + scheduler, `verify` agent command, SMTP, weekly digest, repository stats | The digest email lands, the Stored tiles show real numbers, a verify job passes — **PR B** |
| M6 | Screenshot pipeline, both READMEs, `warphold.com` site, apex DNS | `https://warphold.com` serves both tabs with real screenshots, and `curl -fsSL https://get.warphold.com/fleet.sh` (set up in M3) still resolves to the release asset — **its own PRs**, parallel with M4/M5 |

## 14. Reconcile at execution time

These are the things this spec asserts from reading code, and which the plan must confirm against reality before depending on them.

1. **The D4 spike's output is the source of truth for §4.2's delete allowlist.** This spec deliberately does not name the classes: `repo/blob/s3/s3_storage.go:223` shows `DeleteBlob` → `RemoveObject`, and Kopia's session (`s`) blobs and index compaction are the suspected callers, but the allowlist is written from the spike's recorded log, not from that guess. Until then, every task that mentions the allowlist refers to "the classes recorded by Task 1".
2. **minio-go's payload signing mode.** `x-amz-content-sha256` may be a real digest, `UNSIGNED-PAYLOAD`, or `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` depending on TLS and object size. The verifier must handle whichever the client actually sends; the spike's request log settles it. Streaming chunked uploads need chunk-signature verification, which is meaningful extra work — if the client uses them, that is a scoping decision to raise, not to absorb silently.
3. **`ListObjectsV2` vs V1 and the exact XML shapes.** Written from minio-go's expectations; verified by the integration test, which is the only thing that matters.
4. **Kopia's S3 backend flags on current master** — `--endpoint`, `--access-key`, `--secret-access-key`, `--region`, `--disable-tls` — must be checked against `cli/storage_s3.go` before they are printed in a recovery kit, because the kit is the one artefact that must work years later with an unfamiliar binary.
5. **Object Lock verification per provider.** The existing `b2` verification path (`fleet/b2api`) is B2-specific; S3 cloud-direct needs `GetObjectLockConfiguration`, which is a different call. Verify against a real bucket per provider before claiming "verified" in the UI.
6. **Cloudflare redirect rules for `get.warphold.com`** (M3) and **GitHub Pages for `warphold.com`** (M6): whether a Cloudflare redirect rule follows through to a GitHub release asset's own 302 without breaking `curl -fsSL`, whether any cache rule is needed, and the Pages custom-domain verification and HTTPS-enforcement flow. HUMAN CHECKPOINT in the plan.
9. **The UI repo has no browser test runner** (`package.json` `devDependencies`: vitest + `@vitest/coverage-v8` only). Screenshots and visual proof go through the chrome-devtools driver. Re-check `package.json` at execution before writing the script, and add no Node dependency for it.
10. **Kopia trademark wording** in the rewritten READMEs and the site footer: `NOTICE` already states the position; the site repeats it. Re-read `NOTICE` at execution rather than paraphrasing it from here.
7. **Release signing key.** `tools/sign.sh` is upstream's; whether it fits WarpHold's key (and where that key lives) is settled at execution, with the key itself from 1Password and never in the repo.
8. **`fyne.io/systray` and packaging.** The deb/rpm must not pull a display dependency; the tray is autostarted, not required.

## 15. Out of scope

Multi-tenancy, SSO, RBAC beyond the `role` column, billing, Windows and macOS agents, restore *through* the dashboard (still sub-project 3), agent auto-update, horizontal scaling of the Fleet server (more than one Fleet host in front of one store), cross-device dedup, S3 multipart upload, bulk delete, and any S3 operation not listed in §4.2. The site is static marketing and documentation only: no accounts, no downloads it hosts itself (releases stay on GitHub), no analytics.
