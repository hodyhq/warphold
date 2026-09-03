# RECONCILE — Object Lock and conditional writes on a real bucket

- **Status:** ⛔ **PENDING — HUMAN CHECKPOINT.** The code is written and tested against fakes;
  no real bucket has been touched. The *recorded response* sections below are empty on purpose.
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

**Recorded response: PENDING (human checkpoint).**

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

**Recorded response (locked): PENDING (human checkpoint).**
**Recorded response (unlocked), with the exact `<Code>`: PENDING (human checkpoint).**

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
**Recorded response: PENDING (human checkpoint).**

> A `200` on the second PUT means B2 does not enforce the precondition. That is not a bug to
> work around: `ProbeConditionalPut` returns `ErrNoConditionalPut`, target creation answers
> `400 … does not enforce conditional writes …`, and cloud-direct on B2 is **off the table**
> until B2 implements it — Fleet disk + mirror stays the supported shape. Raise it, do not
> soften the check.

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

**Recorded response: PENDING (human checkpoint).**

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
