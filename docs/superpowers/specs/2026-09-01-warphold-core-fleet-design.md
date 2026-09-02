# WarpHold — Core + Fleet design (sub-project 1)

- **Status:** APPROVED 2026-09-01 (design + written spec + prototype); implementation plan next
- **Author:** Hody with Claude Code
- **Supersedes:** `~/Downloads/kopia-fleet-handover.md` (the "wrap, don't fork" plan) and the never-started laptop migration spec `hody/handoffs/kopia-b2-migration-SPEC.md`
- **Name:** WarpHold (stylized `WarpHold`; binaries and package names lowercase `warphold`)

## 1. What this is

WarpHold is a full fork of [Kopia](https://github.com/kopia/kopia) with a rebuilt UI and a **Fleet** mode: a web dashboard that enrolls machines, pushes backup policy to them, escrows their encryption keys, and shows whether each one is still backing up. It is the Backblaze-Personal-Backup-for-Linux that does not exist, built family-first but with an enterprise-shaped data model.

This spec covers **sub-project 1**: the fork, the Fleet control plane, the Linux agent with tray, the Fleet dashboard, and the restyle of the existing single-user UI. Later sub-projects, each with its own spec:

2. Hosted and hybrid target (Fleet server as Kopia repository server, then `sync-to` B2/S3)
3. Restore from the dashboard (full, single file, restore to a new machine)
4. Windows and macOS agents with signed installers

## 2. Decisions locked

| Decision | Rationale |
|---|---|
| Full fork of kopia/kopia **and** kopia/htmlui | Hody owns the UI and the product; upstream stays mergeable (§3.2) |
| Keep module path `github.com/kopia/kopia` | Same pattern as Upvora keeping `getfider/fider`: upstream merges and upstream PRs both stay trivial |
| Never touch encryption, dedup, chunking, or repository format | The product is the UX layer; touching the engine turns it into a crypto project |
| GitHub only: `github.com/hodyhq/warphold`, `github.com/hodyhq/warphold-ui` | Upstream PRs open from the same place the code lives |
| Family/homelab first, enterprise-shaped | Groups, targets, and policy templates from day one; RBAC/SSO/tenants are columns and screens later, not a rewrite |
| Linux agents first | Windows and macOS are sub-project 4 |
| Control plane, not data plane | Agents write directly to the target with scoped credentials; Fleet never relays backup bytes in this sub-project |
| One repo per agent, prefix-isolated, on B2 | A compromised agent cannot read or delete another machine's history |
| Agents poll outbound; no inbound ports on agents | NAT and CGNAT are the norm |
| "Activate Fleet" is compiled in | One binary; activation enables routes and creates the DB. No second download. |
| Agent runs Kopia's engine in-process | No pinned-CLI download, no checksum step, no second scheduler |

> **Non-negotiable.** Fleet must never be required for a restore. A person holding the recovery kit restores with a stock upstream `kopia` binary and nothing else. An integration test enforces this (§12).

## 3. Architecture

### 3.1 Repos and binaries

- `hodyhq/warphold` — fork of kopia/kopia. Remote `upstream` points at kopia/kopia; `main` is ours.
- `hodyhq/warphold-ui` — fork of kopia/htmlui. Its build output is published as a Go module (mirroring upstream's `htmluibuild`) and imported by the server.
- One binary, `warphold`, three modes:
  - `warphold server start` — upstream's single-user app, with our UI.
  - `warphold fleet …` — same HTTP server with Fleet routes and DB enabled.
  - `warphold agent …` — device side: enroll, run, tray.

### 3.2 Fork discipline

Upstream files touched, and nothing else (the UI import swap in `internal/server/htmlui_embed.go` originally planned here didn't happen in sub-project 1 — no UI fork exists yet, so it's Plan 2's item, §15):

1. `cli/app.go` — register the `fleet` and `agent` command groups (`c.fleet.setup(c, app)`, an `agent commandAgent` field).
2. `cli/command_server_start.go` — a `setupHandlers` hook: before mounting the control-API and UI handlers, `setupHandlers` walks `serverExtraHandlers` (populated by the new `cli/server_hooks.go`'s `RegisterServerHandlers`) so Fleet's routes mount on the same mux router ahead of the UI catch-all. `server_hooks.go` is a new file, not an upstream one, but it's the seam `command_server_start.go` was cut open to expose — one three-line hook, no other changes to `server start`.
3. `Makefile` — a `warphold-build` target (`CGO_ENABLED=0 go build … -o dist/warphold$(exe_suffix) .`), additive.
4. `.goreleaser.yml` — `project_name: warphold`, binary renamed to `warphold`; ldflags/homepage otherwise unchanged.
5. `main.go` — the kingpin app name changes from `kopia` to `warphold` (`kingpin.New("warphold", …)`); usage text otherwise unchanged.

All Fleet and agent code is **new files** in new packages:

```
fleet/            control plane (server side)
  store/          SQLite schema, migrations, queries
  seal/           argon2id key derivation + secretbox sealing
  enroll/         tokens, enrollment handler, B2 provisioning, repo creation
  policy/         templates → per-agent Kopia policy JSON
  health/         status computation, digests
  jobs/           verify, test-restore, prune/GC, digest email
  kit/            recovery-kit HTML render
  api/            /api/v1/fleet/* handlers + admin auth middleware
agent/            device side
  engine/         starts Kopia's server engine headless, applies policy
  poll/           poll + report client
  tray/           StatusNotifierItem tray
  install/        enroll.sh generator, systemd units, XDG autostart
cli/command_fleet_*.go, cli/command_agent_*.go   thin wrappers
```

Merging upstream: `git fetch upstream && git merge upstream/master`, expected to conflict only in the three touched files. Any Fleet package is PR-able upstream as a unit.

### 3.3 Fleet server

Mounts `/api/v1/fleet/*` on the same gorilla/mux router the upstream server exposes through its public `Setup*Handlers` methods. State is SQLite via `modernc.org/sqlite` (pure Go, no CGO). Fleet admin auth is separate from Kopia's repository users: email + argon2id password, HTTP-only session cookie, upstream's existing CSRF token scheme, login rate limiting.

**Activation.** `warphold fleet activate` (also the Activate Fleet wizard in the UI) prompts for an admin passphrase, derives the sealing key, creates the DB and the first admin, and enables the routes. Unattended restarts read the sealing key from a `0600` file in the state directory (`/var/lib/warphold/` or `$XDG_STATE_HOME/warphold/`). Documented alternative: systemd `LoadCredential`. Stated as accepted risk in the docs.

Activating over HTTP (`POST /api/v1/fleet/activate`, as opposed to the `fleet activate` CLI, which calls `Activate` directly and is unaffected) requires either a loopback client (`127.0.0.1`/`::1`) or the one-time setup token: a CSPRNG value the server writes to `<stateDir>/setup-token` on first boot before activation, logs the path, and deletes once activation succeeds. The caller presents it via the `X-WarpHold-Setup-Token` header, compared with `subtle.ConstantTimeCompare`. This closes the window where an unauthenticated LAN client could activate (and so become) the Fleet admin before the real admin gets to it.

**Scheduled jobs** (run by the Fleet server with the target's admin key, never by agents):

| Job | Cadence | Does |
|---|---|---|
| verify | weekly per agent | `snapshot verify` on the agent's repo |
| test-restore | monthly per agent | restore one random small file to a temp dir, compare hash with the snapshot manifest |
| maintenance | daily | `maintenance run --full` on each repo; Fleet's user is the maintenance owner |
| digest | weekly | fleet-status email over SMTP (settings) |

### 3.4 Agent

**Scopes.** `--scope user` (default): systemd user unit + `loginctl enable-linger`, backs up `$HOME`. `--scope system`: root system unit, backs up system paths. Same binary, same protocol.

**Runtime loop.**

1. Connect to the repo from the stored connect token.
2. Start Kopia's server engine in-process: UI off, control API bound to `127.0.0.1` with a random control password written to a local `0600` token file. Scheduling, uploads, task tracking, throttling, pause/resume come from upstream's source manager unchanged.
3. Every 5 minutes ± 60 s jitter, `POST /api/v1/fleet/agent/poll` with a heartbeat and the last policy ETag. Response is `304` or a new policy document plus pending commands.
4. On policy change: for each listed source, set the Kopia policy object verbatim; remove sources no longer listed; refresh the engine.
5. When an in-process task finishes, `POST /api/v1/fleet/agent/report` immediately.

**Commands** carried in the poll response: `snapshot-now`, `pause`, `resume`, `verify`. Each is executed through the local control API and acknowledged in the next report.

**State files** (`$XDG_CONFIG_HOME/warphold/` for user scope, `/etc/warphold/` for system scope, all `0600`): `agent.json` (server URL, agent id, bearer token, name), Kopia config and connect token, control-API token. Cache in `$XDG_CACHE_HOME/warphold/`.

**Tray.** `warphold agent tray` is a separate process started by an XDG autostart entry, so headless servers never need a display. It reads the service's localhost control API and renders: health-colored icon; current backup with progress; last and next backup; recent errors; the vault name (the agent's display name). Never shown: bucket, keys, server URL. "Details" opens the agent-mode UI (§9.3) on localhost. Desktop notification only on red. Library: a pure-Go StatusNotifierItem implementation over D-Bus (`fyne.io/systray`); the Omarchy/waybar tray module speaks SNI — verify at execution time.

## 4. Data model

SQLite. Secrets are sealed with a key derived from the admin passphrase (argon2id) using NaCl secretbox.

| Table | Columns (abridged) |
|---|---|
| `admins` | id, email, pw_hash (argon2id), role (`owner` only today), created_at |
| `targets` | id, name, kind (`b2`, `filesystem`; `s3`, `hosted` later), bucket, region/path, object_lock_verified_at, sealed_admin_key |
| `policy_templates` | id, name, sources (JSON list of paths), policy_json (a Kopia `policy.Policy`, verbatim) |
| `groups` | id, name, target_id, policy_template_id |
| `agents` | id, name, hostname, os, arch, version, scope, group_id, bearer_hash, enrolled_at, last_seen_at, sealed_repo_bundle (connect token, repo password, writer key id/secret, reader key id/secret, prefix), revoked_at |
| `enrollment_tokens` | id, token_hash, group_id, expires_at, max_uses, uses, revoked_at, created_by |
| `reports` | id, agent_id, task_id, kind (`snapshot`, `verify`, `restore`), source, started_at, finished_at, status, bytes, files, snapshot_id, stderr |
| `kit_acks` | agent_id, acknowledged_at, acknowledged_by |
| `settings` | key, value (SMTP, poll interval, digest recipients) |

Health is computed at read time: **green** when the newest successful snapshot is under 26 h old, **yellow** under 7 d, **red** beyond 7 d or when the last run failed.

## 5. Enrollment

**Tokens.** Default lifetime **1 hour**, admin-settable up to **30 days**. Default `max_uses = 1`; an admin may set it higher or unlimited so a group's preconfigured installer can enroll many machines. Tokens are revocable and stored hashed.

**Flow.**

1. Admin: Devices → Add device → pick group → gets a token and a one-liner: `curl -fsSL https://<fleet>/enroll.sh | sh -s -- --token <T>`. Groups also offer a **preconfigured installer download** with the token baked in.
2. Script installs `warphold` to the user's local bin (or `/usr/local/bin` for system scope), writes the unit and autostart entry, and runs `warphold agent enroll --server <url> --token <T>`.
3. `POST /api/v1/fleet/enroll {token, hostname, os, arch, version, scope}`. Fleet validates the token and, using the target's admin key:
   - creates a **writer** B2 application key scoped to the bucket and prefix `agents/<agent-id>/` with `writeFiles`, `listFiles`, `readFiles` and **no** `deleteFiles`;
   - creates a **reader** key with `listFiles` and `readFiles` only (for the recovery kit);
   - generates a 32-byte CSPRNG repository password;
   - creates the Kopia repository at that prefix and sets Fleet's own user as maintenance owner, so agents skip prune/GC;
   - seals the bundle into `agents.sealed_repo_bundle`.
4. Response: agent id, bearer token, display name, Kopia connect token, poll interval. Agent stores them `0600`, connects, starts the engine, and polls.

**Filesystem targets** exist so CI and homelab tests run without cloud credentials. Isolation there is a per-agent directory plus a per-agent repo password; the hosted-mode ACL model comes in sub-project 2.

`GET /enroll.sh?token=<T>` validates both attacker-controlled inputs it interpolates into the served shell script before templating: `token` must match `^wh_[A-Za-z0-9_-]{20,64}$`, and the `Host` header (used to build the `Server` URL baked into the script) must match `^(\[ipv6\]|host)(:port)?$`. Either mismatch is a `400`, not a template-injection opportunity.

## 6. Agent ↔ Fleet protocol

All over HTTPS with `Authorization: Bearer <token>`. Two agent endpoints:

```
POST /api/v1/fleet/agent/poll
  { "etag": "…", "heartbeat": { "version", "os", "arch", "disk_free_bytes",
    "repo_connected": true, "engine_status": "idle|uploading|paused" } }
→ 304, or
  { "etag": "…", "name": "Hody's laptop",
    "sources": [ { "path": "/home/hody", "policy": { …Kopia policy… } } ],
    "commands": [ { "id": "…", "kind": "snapshot-now|pause|resume|verify", "source": "…" } ],
    "poll_interval_seconds": 300 }

POST /api/v1/fleet/agent/report
  { "task_id", "kind": "snapshot|verify|restore|command", "command_id"?,
    "source", "started_at", "finished_at", "status": "ok|error|cancelled",
    "bytes", "files", "snapshot_id"?, "stderr"? }
→ 204
```

Admin endpoints under `/api/v1/fleet/`: `admins`, `targets`, `templates`, `groups`, `agents`, `agents/{id}/{kit,commands,revoke}`, `tokens`, `reports`, `settings`, `activate`, `session`. JSON, session-cookie auth, CSRF header.

## 7. Security model

- Object Lock is required on B2 buckets and verified when a target is created; the target screen shows the verification state.
- Writer keys can never delete. All prune/GC runs from Fleet with the admin key.
- Agents never hold the admin key and cannot list other agents' prefixes.
- Enrollment tokens: hashed at rest, expiring, use-counted, revocable.
- Bearer tokens: 32 bytes CSPRNG, hashed at rest, rotated on re-enroll, revocable (revocation also deletes the B2 keys).
- Repo passwords and B2 keys sealed at rest under the admin passphrase.
- Fleet admin login rate-limited; sessions HTTP-only, SameSite=Strict.
- Every error shown in the UI is the raw Kopia stderr.
- **Documented honestly:** the Fleet admin can decrypt every agent's backups. For a family fleet that is the point; it is stated on the Activate screen and in the README.
- Reports may only acknowledge commands that belong to the reporting agent: `POST /agent/report` with a `command_id` looks up that command's owning `agent_id` and rejects the ack (`400`) if it doesn't match the authenticated caller, so one agent can't guess another's sequential command id and silently discard its pending command.

## 8. Recovery kit

Generated on enrollment and regenerable from the device page. Rendered server-side as a single print-ready HTML page (the browser makes the PDF; no PDF dependency). Contents:

- repository location (bucket, prefix) and repository password
- reader B2 key id and secret
- literal copy-pasteable commands: `kopia repository connect b2 --bucket … --prefix agents/<id>/ --key-id … --key …`, `kopia snapshot list`, `kopia restore <snapshot-id> <dest>`
- a plain-English paragraph on what this paper is and why to keep it

The dashboard shows a persistent banner per device until an admin ticks "recovery kit stored".

## 9. UI

### 9.0 Prototype

The approved clickable prototype lives in `docs/superpowers/design/` (Kinetic direction; tokens and type in its README) and is the visual reference for every screen below. Implementation matches it; deviations are called out in the plan.

### 9.1 Stack

`hodyhq/warphold-ui`: React 19 + Vite kept. Bootstrap removed; Tailwind plus a small headless component set. New Fleet code in TypeScript; existing JSX pages migrate as they are restyled. Data fetching stays on axios polling, as upstream does. Design direction is set with the frontend-design skill at implementation under a fixed brief: dark-first, dense, fast-feeling, health legible at a glance.

### 9.2 Screens

- **Restyled single-user pages:** Repository, Snapshots, Snapshot browse/restore, Policies + editor, Tasks, Preferences.
- **Activate Fleet** wizard.
- **Fleet Overview:** health grid, counts, last night's failures.
- **Devices:** list (health, group, last snapshot, size); detail (sources, report history, raw stderr, recovery kit + ack, actions: snapshot now, pause, resume, verify, regenerate kit, revoke).
- **Groups:** target, template, member devices, preconfigured installer download, enrollment tokens.
- **Policy templates:** a simple six-field form (sources, schedule, exclude, keep, compression, scope) covers the common case; an **Advanced** drawer exposes upstream's full Kopia policy editor for everything else. Both write the same `policy.Policy` JSON. (Decided 2026-09-01.)
- **Targets:** B2 credentials, bucket, Object Lock verification.
- **Settings:** admins, SMTP/digest, passphrase, poll interval.

### 9.3 Agent-mode build

The same SPA with `mode=agent`, served by the agent on `127.0.0.1`. Shows sources, current task, history, errors, and the vault name. No repository, target, or policy screens.

## 10. Default policy template

Shipped as the first template (from the handover):

```
~/.cache
~/.local/share/Trash
~/.var/app/*/cache
**/node_modules
**/.venv
**/target
**/*.iso
**/*.qcow2
~/.steam
~/Games
```

Dotfiles are backed up (`~/.ssh`, `~/.config`, `~/.claude` included), which is desired and is another reason the repo password is a first-class secret.

## 11. Dependencies added

| Dependency | For |
|---|---|
| `modernc.org/sqlite` | Fleet state, pure Go |
| `fyne.io/systray` | tray (SNI over D-Bus on Linux, native on Windows) |
| `golang.org/x/crypto` argon2 + nacl/secretbox | sealing and password hashing (already in upstream's go.mod) |
| Tailwind + headless components (UI repo) | restyle |

Anything else is asked about first.

## 12. Testing and CI

- Unit: store, seal, enrollment tokens, health computation, policy rendering.
- Integration (no cloud): Fleet + agent in-process against a `filesystem` target: enroll → policy push → snapshot → report → verify job → test-restore job.
- **Standalone restore (required):** enroll, snapshot, read the recovery kit values, restore with a **pinned upstream `kopia` release binary** downloaded in CI, hash-compare against the source tree. Fails the build if it needs anything from WarpHold.
- UI: upstream's vitest + Playwright e2e kept; Fleet screens get e2e for enroll-to-green.
- CI: upstream GitHub Actions retained and renamed; goreleaser builds Linux amd64 and arm64.

## 13. Milestones

| # | Delivers |
|---|---|
| M0 | Fork, rename, UI module wired, all three modes build, upstream merge rehearsed |
| M1 | Fleet server skeleton, activation, admin auth, DB |
| M2 | Enrollment, B2 key provisioning, repo creation, filesystem target |
| M3 | Agent engine, policy poll, reports, systemd units, installer script |
| M4 | Design system + Fleet screens |
| M5 | Tray + agent-mode UI |
| M6 | Recovery kit, escrow, ack banner |
| M7 | Verify, test-restore, maintenance, digest email |
| M8 | Restyle of single-user screens |
| M9 | Standalone-restore test, README, restore runbook, vault docs triad |

## 14. Reconcile at execution time

- Whether Kopia's B2 backend needs `deleteFiles` for its own temporary blobs during `snapshot create`. If it does, the design falls back to Object Lock as the sole immutability guarantee and the key gains delete.
- Whether the Omarchy bar (waybar) tray module renders SNI icons from `fyne.io/systray` without extra config.
- Whether upstream's Kopia connect token can carry a B2 config with a prefix; if not, the agent stores storage config and password separately.
- Kopia's `--without-password`/control-API startup flags for the headless engine may differ on current master; check `cli/command_server_start.go`.
- ~~Resolved during implementation~~: the real Snapshot task `Description` from `internal/server.runSnapshotTask` is `fmt.Sprintf("%v at %v", src, time)` where `src` is a `snapshot.SourceInfo.String()` rendering `user@host:path` — e.g. `hody@fw13:/home/hody at 2026-09-01T23:00:00Z` — not the placeholder `Snapshot <path>:` shape originally assumed. The agent's task-watcher parses this with `^(?:Snapshot )?[^@\s]+@[^:\s]+:(.+?)(?: at \S+)?$`.

## 15. Out of scope for sub-project 1

Multi-tenancy, SSO, RBAC beyond the `role` column, billing, Windows/macOS agents, signed packages, restore through the dashboard, hosted/hybrid target, byte relay, agent auto-update, group inheritance beyond one template per group.
