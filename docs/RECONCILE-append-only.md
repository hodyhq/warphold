# RECONCILE — what stock Kopia deletes and overwrites against append-only S3

- **Status:** SPIKE COMPLETE 2026-09-02 (Plan 3, Task 1 / decision D4)
- **Answers:** spec §14.1 (the delete allowlist), §14.2 (payload signing mode), §14.3 (ListObjectsV2 shape), §14.4 (S3 flag names)
- **Consumed by:** Task 2 (gateway skeleton), Task 3 (SigV4 verifier), Task 5 (`allowDelete` / `allowOverwrite`), Task 9 (integration test)
- **Harness:** [`scripts/spike-append-only/main.go`](../scripts/spike-append-only/main.go) — a throwaway, unauthenticated, loopback-only S3 store that logs every request.

> This file records what was **observed**, not what the code was read to imply. Where the
> code and the log disagree, the log wins. Section §7 lists what the spike could not observe.

## 1. The question

Which blob-name classes, if any, must stock Kopia `DeleteObject` or overwrite in order to
complete `repository create`, several `snapshot create` runs with changing files,
`snapshot list`, and a `restore`, when the storage behind it refuses deletes and overwrites?

## 2. Method

A minimal S3 store (`scripts/spike-append-only`) serving PUT / GET (incl. `Range`) / HEAD /
ListObjectsV2 / DELETE / GetBucketVersioning / GetBucketLocation over a temp directory, with
no authentication and no restrictions, appending one line per request to `requests.log`:

```
<verb> <key> exists=<bool> status=<code> content-sha256=<header> len=<n>
```

It permits everything, so Kopia never fails and the log is the *complete* set of what Kopia
*wanted* — which is exactly what a deny-by-default gateway needs to know.

Stock Kopia (this repo's binary, `go build -o /tmp/wh-spike .`) was driven against it twice:

| Run | Endpoint | Sections |
|---|---|---|
| **A** (`dev-0001/`, plain HTTP) | `127.0.0.1:9401`, `--disable-tls` | `repository create`, 3 × `snapshot create` with changing files, `snapshot list`, `restore` of one file, `maintenance run --full`, then 3 more snapshots **after `maintenance set --owner=fleet@warphold`** and one with `--no-auto-maintenance` |
| **B** (`dev-0002/`, TLS) | `127.0.0.1:9402`, self-signed + `--disable-tls-verification` | `repository create`, `snapshot create` |
| **C** (`dev-0003/`, HTTP, **no `--region`**) | `127.0.0.1:9401` | `repository create` only |

231 requests were recorded in run A. Every command succeeded, including the restore
(`hello\nmore` came back byte-exact).

## 3. Every DELETE Kopia issued

**Run A — 8 DELETEs, all of them session-marker blobs, one per write session:**

```
DELETE dev-0001/s35391b98f24603bae4bd9a4e4e5408e2-s7cd827313c8b69b0144   (repository create)
DELETE dev-0001/s3c3e403825b960e0fa5b074dc87161bf-s0a2c2b13a477b485144   (snapshot create #1)
DELETE dev-0001/sf1063f400a6a3c5c6ca600639a16670c-s35333dd17da937cc144   (snapshot create #2)
DELETE dev-0001/sc4821adb8dc5474901bcd5873432511e-sdb64a4c743afb694144   (snapshot create #3)
DELETE dev-0001/s99cdbbb8405eade150a4ec431d4e8485-s9ab58a6109481cd5144   (snapshot create #4, non-owner)
DELETE dev-0001/sd8bfa36bd7e312b51240f6575dcc83d6-sdc1a9714350e9074144   (snapshot create #5, non-owner)
DELETE dev-0001/s1031ec102af79ff964d101d637c96383-s1d91d1fb50da18a3144   (snapshot create #6, non-owner)
DELETE dev-0001/s9cf7969cd92d7efac62578ec022b5b6f-s6bf4599439c9ef28144   (snapshot create #7, --no-auto-maintenance)
```

Run B (TLS) produced the same shape: 2 DELETEs, both `s…`.

**`snapshot list` and `restore` issued no DELETE at all. `maintenance run --full` issued no
DELETE either** — see §7.1, that is a limitation of a young repository, not a guarantee.

The source is `repo/content/sessions.go:125`, `WriteManager.commitSession`:

```go
for _, b := range bm.sessionMarkerBlobIDs {
    if err := bm.st.DeleteBlob(ctx, b); err != nil && !errors.Is(err, blob.ErrBlobNotFound) {
        return errors.Wrapf(err, "failed to delete session marker %v", b)
    }
}
```

Session blob IDs are `s` + 16 hex + epoch-hex (`repo/content/sessions.go:88`,
`BlobIDPrefixSession = "s"`), and the written blob adds the blobcrypto suffix — hence the
observed `s<32hex>-s<20hex>` shape. **A denied delete here is a hard error**: it propagates
out of `commitSession` and fails the flush, i.e. it fails the snapshot. There is no flag
that disables it.

## 4. Every PUT to a key that already existed

**Exactly one key was ever overwritten, in both runs: `kopia.maintenance`.**

| Run | Overwrites | Key |
|---|---|---|
| A | 16 | `dev-0001/kopia.maintenance` |
| B | 8 | `dev-0002/kopia.maintenance` |

Nothing else — not `kopia.repository`, not `kopia.blobcfg`, not a single `p`/`q`/`x`/`_log_`
blob — was ever written to an existing key. Kopia's blob names are content-addressed or
random, so re-creation does not arise.

`kopia.maintenance` is the maintenance *schedule* blob
(`repo/maintenance/maintenance_schedule.go:22`, `SetSchedule` → `PutBlob`). It is rewritten
by every maintenance cycle and by `ReportRun`, which is why one `snapshot create` produced
8–9 overwrites: `snapshot create` runs **automatic maintenance** first
(`cli/app.go:589`, `maybeRunMaintenance`).

### 4.1 The lever that removes the last overwrite — proven

`maintenance.RunExclusive` (`repo/maintenance/maintenance_run.go:166`) checks ownership
**before** touching the schedule blob:

```go
if !force && !p.isOwnedByByThisUser(rep) {
    return NotOwnedError{p.Owner}
}
```

and `maybeRunMaintenance` swallows `NotOwnedError` silently. Ownership lives in a *manifest*
(`Params.Owner`), not in the blob, so a non-owner does not even **read** `kopia.maintenance`.

Proven empirically. After `kopia maintenance set --owner=fleet@warphold`, each subsequent
`snapshot create` from the device produced **exactly this**, twice over:

```
1 × DELETE  s…        (the session marker)
1 × PUT     p…   1 × PUT q…   1 × PUT x…(xn)   1 × PUT s…   1 × PUT _log_…
1 × GET     q…   1 × GET .storageconfig
1 × HEAD    s…   1 × HEAD x…
6 × LIST    x…
```

**Zero overwrites. One delete, on `s…`.** The `--no-auto-maintenance` global flag
(`cli/app.go:272`, a hidden negatable bool — `--auto-maintenance=false` does *not* parse)
gives the same result and is the belt to the owner's braces.

## 5. The S3 request profile the gateway must implement

### 5.1 `x-amz-content-sha256` — §14.2 settled

| Transport | PUT header | Body framing |
|---|---|---|
| **HTTPS** (production) | `UNSIGNED-PAYLOAD` | plain, `Content-Length` known |
| **plain HTTP** (`--disable-tls`) | `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` | `aws-chunked`, per-chunk signatures |
| GET / HEAD / DELETE / LIST (both) | real digest of the empty body (`e3b0c442…b855`) | — |

The switch is `minio-go@v7.3.0/api.go:1082`, `case metadata.streamSha256 && !c.secure:` —
streaming chunked signing is used **only when the endpoint is not TLS**. Over TLS it falls
through to the default branch, which sets `UNSIGNED-PAYLOAD` (and computes MD5 instead of
SHA-256, `api.go:434`).

> **Scoping decision, not absorbed (§14.2).** WarpHold's gateway is always reached over
> HTTPS (`public_url`, D7), so it only ever needs to accept `UNSIGNED-PAYLOAD` on PUT and a
> real digest elsewhere. **Recommendation: the verifier accepts a real digest and
> `UNSIGNED-PAYLOAD`, and answers `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` with
> `501 NotImplemented` and an error message naming TLS as the fix.** Implementing chunk
> signature verification is real work for a configuration WarpHold never ships. Raise with
> Hody only if a plain-HTTP LAN endpoint is ever wanted.

### 5.2 The verbs, and only these

| Request | Seen | Notes |
|---|---|---|
| `PUT /<bucket>/<key>` | 61 across runs | `Content-Type: application/x-kopia`, `Content-Md5` **always** present (`s3_storage.go`, `SendContentMd5: true`). Largest single PUT observed: **21,438,866 bytes**. |
| `GET /<bucket>/<key>` | 43 | Including one `Range: bytes=32-70`. Single range only; Kopia sets `bytes=0-1` for a zero-length probe. |
| `HEAD /<bucket>/<key>` | 15 | `StatObject`, used for `GetMetadata` after a PUT with `GetModTime` and by the index manager. |
| `GET /<bucket>?list-type=2&…` | 74 | See §5.3. |
| `DELETE /<bucket>/<key>` | 10 | `s…` only, see §3. |
| `GET /<bucket>?location=` | **3, run C only** | Issued only when `--region` is **omitted**. |
| `POST` (`?uploads`, `?delete`) | **0** | Kopia sets `DisableMultipart: true` — "Kopia already splits snapshot contents into small blobs… no need for further splitting". Multipart and bulk delete never appear. |
| `PUT …?retention` | **0** | `ExtendBlobRetention` is only reached via blob-retention maintenance, which this configuration does not use. |
| `GET /<bucket>?versioning=` | **0** | `IsVersioned` (`repo/blob/s3/s3_versioned.go:30`) has **no caller anywhere in the Kopia tree**. The spec's §4.2 note that "Kopia's `IsVersioned` calls it" is correct about the method and wrong about it ever running. Implement it anyway — it is five lines and `minio-go` may probe it in future. |
| `HEAD /<bucket>` | 0 | Bucket existence is probed with a `ListObjectsV2`, not `HeadBucket`. |

### 5.3 `ListObjectsV2` — the exact parameters sent

Every list, without exception:

```
GET /<bucket>?list-type=2&prefix=<prefix>&delimiter=%2F&encoding-type=url&fetch-owner=true
```

- **`list-type=2` always** — `UseV1` is false and Kopia never sets it.
- **`delimiter=/` is always sent**, because Kopia does not set `Recursive`
  (`minio-go/api-list.go:122`, `delimiter := "/"`). Kopia's blob names are flat under its own
  prefix, so no key rolls into `CommonPrefixes` in practice — but the gateway must accept the
  parameter and must not mistake it for a hierarchy request.
- **`encoding-type=url`** — keys in the response must be URL-encoded.
- **`fetch-owner=true`** — an `<Owner>` element may be requested; minio-go tolerates its
  absence, and this spike omitted it with no ill effect.
- `max-keys` is **not** sent (server default applies); `continuation-token` was never needed
  at this scale. The response fields actually consumed are `Key`, `Size`, `LastModified`,
  `IsTruncated` and `NextContinuationToken` (`repo/blob/s3/s3_storage.go`, `ListBlobs`).
- Observed prefixes: `<dev>/`, `<dev>/_log_`, `<dev>/xe`, `<dev>/xn`, `<dev>/xn0_`,
  `<dev>/xn1_`, `<dev>/xr`, `<dev>/xs`, `<dev>/xw` — all inside the device prefix, so §4.1.5's
  prefix-confinement rewrite is never exercised by a well-behaved client.

### 5.4 Flags — §14.4 verified against `cli/storage_s3.go`

`--bucket`, `--prefix`, `--endpoint` (default `s3.amazonaws.com`), `--region` (default empty),
`--access-key` / `AWS_ACCESS_KEY_ID`, `--secret-access-key` / `AWS_SECRET_ACCESS_KEY`,
`--disable-tls`, `--disable-tls-verification`.

> **`--region` is not optional in practice.** Omitting it makes minio-go call
> `GetBucketLocation` (run C: 3 calls during `repository create`). The enrollment command and
> the recovery kit **must** pass `--region`, *and* the gateway should implement `?location=`
> anyway so an unfamiliar binary years from now still works.

## 6. The gateway allowlist — the answer

Keys below are relative to the device prefix `<device-id>/`; the gateway matches after
prefix confinement (§4.1.5). Anything not in this table is denied.

| Blob class | Who writes it | Delete? | Overwrite? | When | If denied | Gateway rule |
|---|---|---|---|---|---|---|
| `s<32hex>-s<hex>` **session markers** | `repo/content/sessions.go` `writeSessionMarkerLocked` / `commitSession` | **ALLOW** | no | end of every write session (create, each snapshot) | **hard error — the snapshot fails** | `allowDelete`: `^s[0-9a-f]{16,}` |
| `kopia.maintenance` | `repo/maintenance/maintenance_schedule.go` `SetSchedule` | no | **not from devices** | only when the caller **is** the maintenance owner | maintenance can't record its schedule | `allowOverwrite`: **empty for device keys.** Provisioning sets the owner to the Fleet identity (§6.1); an overwrite attempt from a device means the lever failed and must surface as `409` + an alert |
| `p…`, `q…` pack blobs | content manager flush | no | no | every snapshot | — | create-only |
| `x…` (`xn`/`xe`/`xs`/`xr`/`xw`) epoch index blobs | `internal/epoch` | no | no | every flush / compaction | — | create-only |
| `n…`, `m…`, `l…` v0 index blobs | `repo/content/indexblob/index_blob_manager_v0.go` | no | no | **never** — this repo format is epoch-index (`x…`); v0 is legacy | — | create-only (deny delete: v0 index rewriting deletes `n…`, and a hosted repo must never be v0) |
| `_log_…` | internal log writer | no | no | every command | — | create-only |
| `kopia.repository`, `kopia.blobcfg` | `repo/format` | no | no | `repository create` only | — | create-only. **`kopia.repository.backup.*` is deleted by `repo/format/upgrade_lock.go:178`** — format upgrade is not a device operation; deny |
| `.storageconfig` | read-only probe | no | no | every command (404 is normal) | — | GET only |

### 6.1 The two rules, stated for Task 5

```
allowDelete    = [ "^s[0-9a-f]{16,}" ]      // session markers, and nothing else
allowOverwrite = [ ]                         // nothing; PUT to an existing key is 409
```

For this to hold, **hosted provisioning must set the repository's maintenance owner to the
Fleet identity, not the device's** (spec §7.1 step 5 — this spike says *how*):

1. after `repository create`, run `maintenance set --owner=<fleet-user>@<fleet-host>`; and
2. install the agent's Kopia invocations with `--no-auto-maintenance`.

Either alone is sufficient; both together mean a device issues zero overwrites even if one
is missed. Without them a device overwrites `kopia.maintenance` on **every snapshot** and
will, once the repository is old enough, start deleting `p`/`q`/`x`/`_log_` blobs from
`maintenance run` — see §7.1.

## 7. Open risks

1. **`maintenance run --full` deleted nothing here, and that is not a guarantee.** The
   repository was minutes old, so `pack_gc` (`repo/maintenance/pack_gc.go:54`) and
   `cleanup_logs` (`repo/maintenance/cleanup_logs.go:113`) had nothing past their safety
   margins. On an aged repository, maintenance **will** delete `p`, `q`, `x`/`n` and `_log_`
   blobs. This does not widen the device allowlist — devices run no maintenance (§6.1) — but
   the **Fleet's own maintenance job** (`fleet/jobs/maintenance.go`) must therefore not go
   through the device-scoped gateway path. It operates on the store directly, or through an
   admin key with a different, wider allowlist.
2. **Epoch `deletePartiallyWrittenShards`** (`internal/epoch/epoch_manager.go:911`) deletes
   `x…` blobs it just wrote, but only on the `ErrVerySlowIndexWrite` path — an index write
   that straddles two epoch advances. Never observed. If it fires against a deny-delete
   gateway the wrapped error is returned and the flush fails; the *next* flush succeeds and
   the orphaned shard is cleaned up by a later compaction. **Accepted risk**: rare, and
   recoverable by retry. Revisit if it appears in gateway logs.
3. **`s…` is also the prefix of `xs…`** (`SingleEpochCompactionBlobPrefix = "xs"`). Anchor the
   allowlist regex at the start of the *blob name* (after the device prefix) — `^s`, never a
   substring match — or the allowlist silently permits deleting compacted epoch indexes.
4. **No MinIO cross-check.** `minio` is not installed on this machine and was not installed.
   The store here is our own; a real S3 implementation may differ in header casing, `Owner`
   elements or error shapes. Task 9's integration test against the real gateway is the
   backstop.
5. **Only the epoch-index repository format was exercised** (the current default). A repo
   created with `--index-version=1` would use v0 index blobs and rewrite/delete `n…`/`m…`/`l…`.
   Hosted provisioning must not offer that.
6. **`DoNotRecreate` is unsupported by Kopia's S3 backend** (`s3_storage.go:135` returns
   `ErrUnsupportedPutBlobOption`). Kopia will never send a conditional PUT, so the gateway's
   "only if absent" rule is enforced entirely server-side (`O_EXCL` + rename, §4.3), and a
   `409` is a signal that something is wrong rather than a condition Kopia handles.

## 8. Re-running it

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"     # Go 1.26
cd <repo>
go build -o /tmp/wh-spike .
go run ./scripts/spike-append-only -addr 127.0.0.1:9401 -dir /tmp/spike-store &
until curl -sf -o /dev/null "http://127.0.0.1:9401/warphold?versioning="; do sleep 0.2; done

K="/tmp/wh-spike --config-file=/tmp/spike.config --password=spikepass"
$K repository create s3 --bucket=warphold --prefix=dev-0001/ --endpoint=127.0.0.1:9401 \
    --disable-tls --region=us-east-1 --access-key=spike --secret-access-key=spikespikespike \
    --cache-directory=/tmp/spike-cache
mkdir -p /tmp/spike-src && head -c 30M /dev/urandom > /tmp/spike-src/a.bin && echo hello > /tmp/spike-src/b.txt
$K snapshot create /tmp/spike-src
echo more >> /tmp/spike-src/b.txt && head -c 5M /dev/urandom > /tmp/spike-src/c.bin
$K snapshot create /tmp/spike-src
$K snapshot list
$K restore <root-id>/b.txt /tmp/spike-restore/b.txt
$K maintenance run --full

# the device profile: no maintenance ownership, no auto-maintenance
$K maintenance set --owner=fleet@warphold
$K --no-auto-maintenance snapshot create /tmp/spike-src

# what was deleted, and what was overwritten
grep '^DELETE' /tmp/spike-store/requests.log
awk '/^PUT /&&$3=="exists=true"{print $2}' /tmp/spike-store/requests.log | sort | uniq -c

# TLS profile (proves UNSIGNED-PAYLOAD): add -tls-cert/-tls-key to the store and use
# --disable-tls-verification instead of --disable-tls on a self-signed 127.0.0.1 cert.
```

The store binds loopback only and refuses any other address — it has no authentication.
