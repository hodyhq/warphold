# RECONCILE — Object Lock and conditional writes on a real bucket

- **Status:** ✅ **RUN against a real bucket, 2026-09-03.** One real Backblaze B2 bucket
  (region `us-east-005`, S3 endpoint `s3.us-east-005.backblazeb2.com`, native default
  retention `governance` / 30 days) was exercised through both the native B2 API, raw
  SigV4 S3 calls, and Fleet's own `POST /api/v1/fleet/targets` (Task 15). Only the
  *locked* bucket was available for this run; no *unlocked* B2 bucket was tested, so the
  "plain" halves of 2.1/2.2 below remain unconfirmed against a real bucket.
- **Headline result: claim 2 fails.** B2's S3 endpoint does not merely *not enforce*
  `If-None-Match: *` (the "200 twice" case this file anticipated) — it refuses the header
  outright with `501 NotImplemented` on the very first PUT. Object Lock itself (claims 1
  and 3) is genuinely enabled and correctly reported by both APIs. Net effect: this bucket
  cannot back a cloud-direct target *or* a disk+mirror target through Fleet's current
  `verifyBucket`, because that path requires the conditional-write probe to pass
  unconditionally, for both target shapes.
- **Answers:** spec §14.5 (Object Lock verification per provider) and the second half of
  §14 note 00 (whether B2's S3 endpoint really enforces `If-None-Match: *`).
- **Consumed by:** Task 12 (mirror and cloud-direct verification), Task 13 (the mirror job),
  Task 14 (the real-bucket end-to-end).
- **Needs:** a B2 bucket with Object Lock **enabled**, a second bucket with it **disabled**,
  and an application key with `listBuckets` + read/write on both (1Password).

> This file records what was **observed**. Until the checkpoint is run, every "recorded
> response" below reads PENDING, and nothing in the UI may claim a provider is "verified"
> on the strength of the code alone.

## 1. What the code does, and what has to be confirmed

Two properties are proven before a bucket may back a mirror or a cloud-direct target
(`fleet/api/admin_targets.go`, `verifyBucket`). Either failure is a `400` naming the bucket:

| Property | B2 (`mirror_kind: "b2"`, or cloud-direct with no explicit endpoint) | S3-compatible |
|---|---|---|
| Object Lock enabled | native `b2_list_buckets` → `fileLockConfiguration.value.isFileLockEnabled` (`fleet/b2api`) | `GetObjectLockConfiguration` via minio-go (`fleet/gateway.ProbeObjectLock`) |
| Conditional writes | `fleet/gateway.ProbeConditionalPut` over B2's **S3** endpoint | same |

Both kinds *write* through the S3 path — B2 through `s3.<region>.backblazeb2.com`, derived
from the region — because B2's native API has no conditional write at all (§14 note 00). The
native API is used for the lock flag and nothing else.

**The three claims that are documentation, not measurement:**

1. B2's `b2_list_buckets` really returns `fileLockConfiguration.value.isFileLockEnabled` on
   the v3 API with an *application* (not master) key.
2. B2's S3 endpoint really honours `If-None-Match: *` and answers **412** on the second PUT.
3. B2's S3 endpoint answers `GetObjectLockConfiguration` at all, and what it says for a bucket
   with Object Lock off (AWS answers `404 ObjectLockConfigurationNotFoundError`, which is what
   `ProbeObjectLock` matches on; a different code there would make the S3 path report a
   transport error instead of "no Object Lock").

## 2. Commands to run at the checkpoint

Set the credentials from 1Password; never paste them into a file.

```sh
export B2_KEY_ID=...            # application key id
export B2_KEY=...               # application key
export B2_REGION=us-west-004
export B2_BUCKET_LOCKED=...     # Object Lock ENABLED
export B2_BUCKET_PLAIN=...      # Object Lock DISABLED
```

### 2.1 B2 native — the lock flag (claim 1)

```sh
TOKEN_JSON=$(curl -fsSL -u "$B2_KEY_ID:$B2_KEY" \
  https://api.backblazeb2.com/b2api/v3/b2_authorize_account)
API=$(printf '%s' "$TOKEN_JSON" | jq -r .apiInfo.storageApi.apiUrl)
TOK=$(printf '%s' "$TOKEN_JSON" | jq -r .authorizationToken)
ACC=$(printf '%s' "$TOKEN_JSON" | jq -r .accountId)

for B in "$B2_BUCKET_LOCKED" "$B2_BUCKET_PLAIN"; do
  echo "== $B"
  curl -fsSL -X POST "$API/b2api/v3/b2_list_buckets" \
    -H "Authorization: $TOK" -H 'Content-Type: application/json' \
    -d "{\"accountId\":\"$ACC\",\"bucketName\":\"$B\"}" |
    jq '.buckets[] | {bucketName, fileLockConfiguration}'
done
```

**Expected** (what `fleet/b2api.BucketInfo` decodes) — locked bucket:

```json
{
  "bucketName": "…",
  "fileLockConfiguration": {
    "isClientAuthorizedToRead": true,
    "value": { "isFileLockEnabled": true, "defaultRetention": { "mode": null, "period": null } }
  }
}
```

and `"isFileLockEnabled": false` on the plain one.

> ⚠️ If `isClientAuthorizedToRead` is **false**, `value` is `null` and the client decodes
> `isFileLockEnabled` as `false` — a locked bucket would be reported unlocked and refused.
> That needs a key with `readBucketEncryption`/`listBuckets` scope; record which capability
> actually flips it.

**Recorded response (2026-09-03, locked bucket):** CONFIRMED as documented. `b2_list_buckets`
returned `fileLockConfiguration.value.isFileLockEnabled: true`,
`isClientAuthorizedToRead: true`, `defaultRetention: {mode: "governance", period: {duration:
30, unit: "days"}}`, using an application key scoped to
`listBuckets,listFiles,readFiles,writeFiles,readBucketRetentions,readBucketEncryption` (no
`deleteFiles`, no `bypassGovernance` — see §2.3/§2.4 below on why that matters). The plain
(Object-Lock-disabled) bucket was not tested this run — none was provisioned alongside the
locked one for Task 15.

### 2.2 S3 — `GetObjectLockConfiguration` (claim 3)

```sh
aws --endpoint-url "https://s3.$B2_REGION.backblazeb2.com" \
  s3api get-object-lock-configuration --bucket "$B2_BUCKET_LOCKED"
aws --endpoint-url "https://s3.$B2_REGION.backblazeb2.com" \
  s3api get-object-lock-configuration --bucket "$B2_BUCKET_PLAIN"
```

**Expected**, locked (this is the shape `ProbeObjectLock` reads — only
`ObjectLockEnabled` is looked at; a `Rule` may or may not be present):

```xml
<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>
```

**Expected**, not locked — a 404 whose `<Code>` must be exactly
`ObjectLockConfigurationNotFoundError` for `ProbeObjectLock` to report
`ErrNoObjectLock` rather than a transport error:

```xml
<Error><Code>ObjectLockConfigurationNotFoundError</Code>…</Error>
```

**Recorded response (2026-09-03, locked bucket):** CONFIRMED, and it matches the native
answer exactly. A hand-signed SigV4 `GET /?object-lock=` (stdlib Python, no aws-cli/boto3
available on the workstation) returned `200`:

```xml
<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
    <ObjectLockEnabled>Enabled</ObjectLockEnabled>
    <Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>30</Days></DefaultRetention></Rule>
</ObjectLockConfiguration>
```

This is exactly the shape `ProbeObjectLock` reads (`GetObjectLockConfig` via minio-go), and
`fleet/gateway.ProbeObjectLock` returned no error for this bucket, confirming the code path
end to end. **Recorded response (unlocked), with the exact `<Code>`: not tested this run —
no unlocked bucket available.**

### 2.3 S3 — the conditional put (claim 2)

Two identical PUTs of the same key with `If-None-Match: *`; the second must be **412**.

```sh
KEY=".warphold-probe/reconcile-$(date +%s)"
for i in 1 2; do
  printf 'warphold' | aws --endpoint-url "https://s3.$B2_REGION.backblazeb2.com" \
    s3api put-object --bucket "$B2_BUCKET_LOCKED" --key "$KEY" \
    --if-none-match '*' --body /dev/stdin --debug 2>&1 | grep -E 'HTTP/1.1 [0-9]{3}|PreconditionFailed'
done
aws --endpoint-url "https://s3.$B2_REGION.backblazeb2.com" \
  s3api delete-object --bucket "$B2_BUCKET_LOCKED" --key "$KEY"
```

Or, straight through the code that ships — the same call `verifyBucket` makes:

```sh
go test ./fleet/gateway/ -run TestProbeConditionalPut -count=1   # fakes, always runs
# real bucket: create the target through the API and watch it succeed or 400
```

**Expected:** first PUT `200`, second `412 PreconditionFailed`.

**Recorded response (2026-09-03): FAILED, and worse than the anticipated failure mode.** The
*first* PUT with `If-None-Match: *` never reaches 200 — B2's S3 endpoint answers `501
NotImplemented` immediately:

```xml
<Error><Code>NotImplemented</Code>
<Message>A header you provided implies functionality that is not implemented</Message></Error>
```

Reproduced two independent ways: (a) through the code that ships, via Fleet's own
`POST /api/v1/fleet/targets` (§2.4 below), whose error was
`writing the conditional-write probe: <the 501 above>`; (b) with a hand-signed SigV4 PUT
carrying the same header, same result. This is not "B2 silently ignores the precondition and
lets the second write through" (the 200-then-200 case this file anticipated) — B2 rejects the
conditional header itself before any write is attempted, on a plain unversioned PutObject
call. A plain PUT (no conditional header, but with `Content-MD5` — required once Object Lock
is on, discovered live: without it B2 answers `400 InvalidRequest`) succeeds normally and
returns a version ID, so the bucket and key are otherwise fully writable.

> A `200` on the second PUT (B2 silently not enforcing the precondition) would have meant one
> thing; a `501` on the *first* PUT (B2 rejecting conditional writes as a feature) means
> another, but the consequence is the same either way: `ProbeConditionalPut` returns a non-nil
> error, target creation answers `400 …`, and B2 is **off the table** for both cloud-direct
> *and* disk+mirror hosted targets under Fleet's current `verifyBucket` — it requires the
> conditional-write probe unconditionally for both shapes (`fleet/api/admin_targets.go`
> `applyHostedDisk`/`applyHostedCloud`, both call `verifyBucket`). Fleet disk **without** a
> configured mirror remains the only supported shape against this provider today. Raise it
> with Backblaze if this matters, do not soften the check.

### 2.4 End to end, through Fleet

With a real Fleet server, an admin session and the real credentials:

```sh
curl -fsS -X POST "$FLEET/api/v1/fleet/targets" -H 'Content-Type: application/json' \
  -b cookies -H "X-WarpHold-CSRF: $CSRF" \
  -d "{\"name\":\"offsite\",\"kind\":\"hosted\",\"storage_mode\":\"cloud\",
       \"bucket\":\"$B2_BUCKET_LOCKED\",\"region\":\"$B2_REGION\",
       \"key_id\":\"$B2_KEY_ID\",\"key\":\"$B2_KEY\"}"
```

**Expected:** `201` with `"object_lock_verified": true`, and
`GET /api/v1/fleet/targets` showing `object_lock_verified_at` set and
`endpoint: "s3.<region>.backblazeb2.com"`. The same call against `$B2_BUCKET_PLAIN` must be
`400 bucket "…" does not have Object Lock enabled`, with **no** target created.

**Recorded response (2026-09-03, Task 15):** the shape actually tried was a *hosted disk*
target with a mirror attached (`storage_mode: "disk"`, plus `mirror_kind: "b2"` and the
`mirror_*` fields) against the live Fleet server (`fleet.hody.sh`, build `fd111cf6`), because
`admin_targets.go` has no route to add a mirror to an already-created target — a mirror can
only be set at target-create time (confirmed by reading `handleTargetCreate` /
`applyHostedDisk` and the full route table in `fleet/api/admin.go`; there is no
`PATCH`/`PUT /targets/{id}` at all). Result: **`400`**

```
{"error":"bucket \"<redacted>\": writing the conditional-write probe: A header you provided implies functionality that is not implemented"}
```

`GET /api/v1/fleet/targets` immediately after confirms **no target was created** — the list
is unchanged (`fleet-local` id 1, `hosted-disk` id 2, no third row). The Object Lock half of
`verifyBucket` passed silently before this (native `b2_list_buckets` check, §2.1) — the
rejection is the conditional-write probe alone, exactly matching the raw SigV4 result in
§2.3. No unlocked bucket was available to exercise the "does not have Object Lock enabled"
branch this run.

**Consequence for Task 15 (attach a mirror to the live hosted target):** blocked by design,
not by a config mistake. `hosted-disk` (target id 2) still has no mirror configured. No
workaround target was created and no source was changed, per instructions — this file's own
guidance is to raise the finding, not soften the check. The scheduler's periodic `mirror` job
(no manual enqueue route/CLI exists either — see Task 15 report) still runs on its own
interval regardless of whether any target has a mirror configured, and it did: job id 1,
`2026-09-03T10:57:50Z`, status `ok`, detail `"0 objects, 0 bytes, 0 skipped"` — the correct
"nothing to mirror yet" outcome, both because no devices are enrolled and because no target
currently carries `mirror_kind`.

### 2.5 Independent delete-refusal check (outside Fleet)

Since Fleet itself could not put a mirror on this bucket (§2.4), the "does Object Lock
actually stop a delete" question was checked directly against B2 with the same application
key, bypassing Fleet entirely: a plain PUT under `_warphold-probe/` (Content-MD5 required —
see §2.3) succeeded and returned a version ID; a DELETE of that specific version answered
**`403 AccessDenied: not entitled`**. The key's capabilities
(`listBuckets,listFiles,readFiles,writeFiles,readBucketRetentions,readBucketEncryption`) have
neither `deleteFiles` nor `bypassGovernance`, so the refusal is enforced twice over — by the
key's own scope and, independently, by the bucket's governance-mode 30-day retention. The
probe object is left in place under `_warphold-probe/` (undeletable during the retention
window regardless).

## 3. What this file does not cover

- **Default retention.** Only the *enabled* flag is checked. Retention periods are a bucket
  policy the admin owns, and per-object retention is deliberately not passed through
  (§14 note 00), so a bucket with Object Lock on and no default retention verifies fine and
  protects nothing. Whether the UI should warn about that is a product decision, not a
  verification one.
- **AWS S3 and MinIO** were not exercised either; the S3 path is the same code, but the
  `ObjectLockConfigurationNotFoundError` code above is AWS's documented one and MinIO's may
  differ. Record it when a non-B2 bucket is first configured.
- **Revocation.** Nothing re-verifies a bucket after creation: `object_lock_verified_at` and
  `mirror_lock_verified_at` are stamps, not a live state. If an admin turns Object Lock off
  afterwards, Fleet does not notice. A periodic re-probe belongs with the jobs scheduler.
