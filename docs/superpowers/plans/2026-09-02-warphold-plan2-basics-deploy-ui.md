# WarpHold Plan 2: release basics, homelab deployment, UI fork, Fleet screens, tray

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** WarpHold runs for real on the homelab (Fleet server on VM 140 behind `https://<FLEET_HOST>`, this laptop enrolled and backing up), ships as a branded, license-clean fork, and gets its own UI: the Kinetic design system, the Fleet dashboard, the restyled single-user app, the agent page, and the tray.

**Architecture:** Part A adds the missing release/ops pieces to the Go code (NOTICE, README, `/dl/` route, server-side sessions + admin management, an engine info file + local session handoff for the tray). Part B deploys: clone the cloud-init template, one Traefik route file, `warphold server start` as a systemd service, activation, targets, enrollment of the laptop. Part C forks `kopia/htmlui` into `hodyhq/warphold-ui` (source + committed `build/` + a one-function Go module), swaps the one import line in the server, and builds the screens from the approved prototype. Part D is the tray and the agent-mode page served by the agent's own engine.

**Tech Stack:** Go 1.26 (mise), React 19 + Vite 7 + TypeScript for new code, Tailwind v4, self-hosted OFL fonts (Unbounded, Space Grotesk, Space Mono), `fyne.io/systray` (pure Go on Linux via D-Bus), Proxmox (`qm` over `ssh pve1`), Traefik conf.d, systemd.

**Spec:** `docs/superpowers/specs/2026-09-01-warphold-core-fleet-design.md` (§3.1 repos/binaries, §3.4 agent + tray, §7 security, §9 UI, §13 milestones M4/M5/M8). **Visual reference (binding for layout, copy, tokens):** `docs/superpowers/design/*.dc.html` + `docs/superpowers/design/README.md`. **Ledger of Plan 1:** carried items are listed in "Carried from Plan 1" below.

## Global Constraints

- Module path stays `github.com/kopia/kopia`. Upstream files that may be touched in this plan, each marked `warphold:`: `internal/server/htmlui_embed.go` (ONE import line — the sixth and last permitted touch), `Makefile`, `.goreleaser.yml`, `README.md`, `go.mod`/`go.sum`. Never edit `repo/`, `snapshot/`, `internal/` otherwise, `cli/app.go`, `cli/command_server_start.go`, `main.go`.
- New Go dependencies allowed: `fyne.io/systray` (tray) only. Run the dependency review (CVE check) before adding it; record the result in the task report.
- `CGO_ENABLED=0` must still build the Linux binary. The tray must not require CGO on Linux.
- Secrets never in logs, responses, git, or the vault. Homelab hostnames and LAN addresses never appear in the GitHub repos: they live in the vault (`Homelab/WarpHold/WarpHold-Full-Dev-Guide.md`, section "Plan 2 concrete values") and in deployment reports only; this plan uses the placeholders `<FLEET_HOST>`, `<FLEET_IP>`, `<GATEWAY_IP>`, `<HOMELAB_DOMAIN>`. A private-name grep (the vault names the pattern) must be empty before every push.
- CodeRabbit gates (project rule): `cr review --agent -t uncommitted` after each task before committing, `cr review --agent -t committed --base-commit <sha>` after every fix, and one whole-branch `cr review --agent --base master` before opening each PR. Fix Critical/Warning locally first.
- Two repos, two PRs: `hodyhq/warphold` (Parts A, B-code, D, and the import swap) and `hodyhq/warphold-ui` (Part C). Keep each PR under ~40 files where possible; the UI repo may need two PRs (design system + shell, then screens).
- Tests offline and pristine; UI tests via vitest; Go tests via `go test ./fleet/... ./agent/... ./cli/...`.
- Every deployment step ends with a live check whose output goes in the report (Working Rule #4). Vault updated after each deployment task (Working Rule #2).
- Commit messages as given; no attribution trailers.

## Carried from Plan 1 (must be closed here)

| Item | Where |
|---|---|
| `Report.SnapshotID` never populated | Task 4 (engine reads the manifest id after a snapshot task) |
| Installed unit lacks `WARPHOLD_STATE_DIR` | Task 4 (unit gets `Environment=` when a non-default state dir is used) |
| `/dl/<binary>` route missing | Task 2 |
| Server-side session revocation; admin management | Task 3 |
| `app/` Electron packaging still kopia-named | Task 1 (documented as not shipped; left as upstream) |
| Partial-activation cleanup docs | Task 1 (README operations note) |
| Spec §3.3 CSRF for Fleet admin routes when the UI lands | Task 11 (SPA sends `X-WarpHold-CSRF`; server double-submit cookie) |

## File structure

```
NOTICE                                   (new) Apache-2.0 attribution to Kopia
README.md                                (rewrite: WarpHold header, fork attribution, quick start, honesty paragraph)
docs/superpowers/UPSTREAM.md             (+ go.mod note already; + app/ note)
fleet/api/dl.go                          /dl/warphold-<os>-<arch>
fleet/api/sessions.go                    server-side sessions (replaces HMAC cookie)
fleet/api/admin_admins.go                admins list/invite/delete/change-password
fleet/api/overview.go                    GET /api/v1/fleet/overview (dashboard aggregate)
fleet/api/csrf.go                        double-submit CSRF for admin routes
fleet/store/sessions.go                  sessions table + queries
fleet/store/schema.sql                   + sessions table (idempotent)
agent/engine/info.go                     engine.json (url, user, password) 0600
agent/engine/localauth.go                /local/session?t= handoff + cookie→basic-auth middleware
agent/engine/snapshotid.go               SnapshotID lookup after a snapshot task
agent/tray/tray.go                       systray menu, notifications
agent/install/autostart.go               XDG autostart entry for the tray
cli/command_agent_tray.go                `agent tray`
cli/command_agent_status.go              `agent status` (prints engine info + sources)
internal/server/htmlui_embed.go          import swap → github.com/hodyhq/warphold-ui   (warphold:)
docs/superpowers/plans/…                 this file
hody vault: Homelab/WarpHold/{WarpHold-Admin-Overview,WarpHold-Full-Dev-Guide,WarpHold-Issues-and-Fixes}.md

hodyhq/warphold-ui (fork of kopia/htmlui):
  htmlui.go, build/                      Go module + committed build output
  src/design/tokens.css, src/design/fonts/*.woff2, src/design/components/*.tsx
  src/AppShell.tsx, src/mode.ts, src/api/fleet.ts
  src/pages/fleet/{Overview,Devices,Device,Groups,Templates,Targets,Settings,Activate,Login}.tsx
  src/pages/agent/AgentHome.tsx
  src/pages/*.jsx                        restyled single-user pages
```

---

## Part A — release basics (repo `hodyhq/warphold`, branch `plan2-basics`)

### Task 1: NOTICE, README, fork attribution, branding hygiene

**Files:**
- Create: `NOTICE`
- Modify: `README.md` (full rewrite of the header; keep the quick start from Plan 1, edited), `docs/superpowers/UPSTREAM.md`

**Interfaces:** none (docs).

- [ ] **Step 1: NOTICE**

```
WarpHold
Copyright 2026 Hodahel Moinzadeh

This product is a fork of Kopia (https://github.com/kopia/kopia),
Copyright the Kopia Authors, licensed under the Apache License, Version 2.0.
The original LICENSE is retained in this repository. Files modified in the
fork are marked with "warphold:" comments; all new code lives under
fleet/, agent/, and cli/command_fleet_*.go / cli/command_agent_*.go.

"Kopia" and the Kopia logo are trademarks of the Kopia project and are not
used to brand WarpHold. WarpHold's own mark lives under icons/warphold*.
```

- [ ] **Step 2: README header**

Replace everything above the first upstream section (down to and including the Kopia badges and the two-line "Kopia is a fast and secure…" intro) with:

```markdown
<p align="center"><img src="icons/warphold.svg" width="96" alt="WarpHold"></p>

# WarpHold

**Backups that hold. For every machine you care about.**

WarpHold is a fork of [Kopia](https://github.com/kopia/kopia) — same engine, same repository format, same client-side encryption — with a rebuilt UI and a **Fleet** mode: enroll machines, push them a backup policy, escrow their keys, and see at a glance whether every one of them is still backing up.

- **Single machine:** `warphold server start` gives you the WarpHold app for this computer.
- **Fleet:** activate Fleet on one server, then enroll Linux laptops and servers with a one-line installer. Windows and macOS agents are planned.
- **Standalone restore, always:** a recovery kit plus stock upstream `kopia` can restore any device with WarpHold completely offline. Fleet is a control plane, never a dependency of your data.

> WarpHold is not affiliated with the Kopia project. See [NOTICE](NOTICE). Upstream changes are merged regularly ([docs/superpowers/UPSTREAM.md](docs/superpowers/UPSTREAM.md)).

## Status

Plan 1 (Fleet control plane + Linux agent) is complete and running; the WarpHold UI and tray land in Plan 2. Screenshots will follow the UI.
```
Then keep the existing "WarpHold Fleet quick start" section (from Plan 1) but move it directly under "Status", add one "Operations notes" subsection after it:

```markdown
### Operations notes
- **Activation is one-shot.** If activation fails half-way (key file or DB present but unusable), delete `<state dir>/seal.key` and `<state dir>/fleet.db` before retrying. WarpHold refuses to overwrite an existing key file on purpose: that file unlocks every escrowed repository password.
- **Electron desktop app (`app/`)** is upstream KopiaUI packaging and is not built or shipped by WarpHold; the WarpHold tray (`warphold agent tray`) replaces it on Linux.
```
Below that, keep the upstream README body (Kopia's feature list, docs links) under a heading `## About the Kopia engine` so the attribution is explicit, and delete the Kopia badge row, the Gurubase badge, and the `![Kopia](icons/kopia.svg)` image.

- [ ] **Step 3: UPSTREAM.md note**

Append: `The Electron packaging under app/ and tools/docker-publish.sh are upstream-only and untouched; they still reference dist/kopia_* and are not part of WarpHold releases.`

- [ ] **Step 4: Verify and commit**

```bash
grep -n "kopia.svg\|Gurubase\|img.shields.io" README.md   # expect nothing
grep -rn "hody\.sh\|10\.99\." README.md NOTICE docs/superpowers/UPSTREAM.md   # expect nothing
git add NOTICE README.md docs/superpowers/UPSTREAM.md && git commit -m "docs: WarpHold README header, NOTICE with Kopia attribution"
```

---

### Task 2: Binary download route `/dl/`

**Files:**
- Create: `fleet/api/dl.go`
- Modify: `fleet/api/server.go` (`Mount` registers it), `fleet/api/enroll.sh.tmpl` (already downloads from `{{.Server}}/dl/warphold-linux-$ARCH`; keep), `README.md` (drop the "binary must be pre-placed" sentence)
- Test: `fleet/api/dl_test.go`

**Interfaces:**
```go
// GET /dl/warphold-<os>-<arch>   (os ∈ linux; arch ∈ amd64|arm64)
// 1. if <os>/<arch> == runtime.GOOS/GOARCH → serve os.Executable() (Content-Type application/octet-stream, Content-Disposition attachment; filename warphold)
// 2. else if <stateDir>/binaries/warphold-<os>-<arch> exists → serve it
// 3. else 404 {"error":"no binary for <os>-<arch>; place one at <stateDir>/binaries/"}
// Gated by requireActivated only (public like /enroll.sh: the binary is not a secret).
```

- [ ] **Step 1: Failing test**

```go
package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDownloadServesOwnBinaryAndStagedOnes(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	res, err := http.Get(h.srv.URL + "/dl/warphold-" + runtime.GOOS + "-" + runtime.GOARCH)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, "application/octet-stream", res.Header.Get("Content-Type"))
	self, _ := os.Executable()
	st, _ := os.Stat(self)
	require.Equal(t, st.Size(), res.ContentLength)

	res, _ = http.Get(h.srv.URL + "/dl/warphold-linux-riscv64")
	require.Equal(t, 404, res.StatusCode)

	staged := filepath.Join(h.stateDir, "binaries")
	require.NoError(t, os.MkdirAll(staged, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staged, "warphold-linux-riscv64"), []byte("ELF-ish"), 0o755))
	res, _ = http.Get(h.srv.URL + "/dl/warphold-linux-riscv64")
	require.Equal(t, 200, res.StatusCode)

	res, _ = http.Get(h.srv.URL + "/dl/warphold-linux-../seal.key")
	require.Equal(t, 404, res.StatusCode)
}
```
(`harness` gains a `stateDir string` field set in `newHarness`.)

- [ ] **Step 2: Run, expect 404s / compile failure on `stateDir`.**

- [ ] **Step 3: Implement `fleet/api/dl.go`**

```go
package api

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/gorilla/mux"
)

var dlName = regexp.MustCompile(`^warphold-(linux)-(amd64|arm64)$`)

func (s *Server) mountDownload(m *mux.Router) {
	m.HandleFunc("/dl/{name}", s.requireActivated(s.handleDownload)).Methods(http.MethodGet)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	mm := dlName.FindStringSubmatch(name)
	if mm == nil {
		writeErr(w, http.StatusNotFound, "unknown binary name")
		return
	}
	path := ""
	if mm[1] == runtime.GOOS && mm[2] == runtime.GOARCH {
		if self, err := os.Executable(); err == nil {
			path = self
		}
	}
	if path == "" {
		cand := filepath.Join(s.paths.StateDir, "binaries", name)
		if st, err := os.Stat(cand); err == nil && st.Mode().IsRegular() {
			path = cand
		}
	}
	if path == "" {
		writeErr(w, http.StatusNotFound, "no binary for "+mm[1]+"-"+mm[2]+"; place one under <state dir>/binaries/")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="warphold"`)
	http.ServeFile(w, r, path)
}
```
Call `s.mountDownload(m)` from `Mount`. The regex is the whole allow-list, so `../` can never reach the filesystem lookup.

- [ ] **Step 4: README** — replace the "Put the `warphold` binary at …" paragraph with: "The installer downloads the binary from your Fleet server (`/dl/warphold-linux-<arch>`). The server offers its own binary for its own OS/arch; for other architectures drop a build into `<state dir>/binaries/warphold-linux-<arch>`."

- [ ] **Step 5: `go test ./fleet/api/ -count=1`, `cr review --agent -t uncommitted`, commit** `feat(fleet): serve the agent binary at /dl/`.

---

### Task 3: Server-side sessions and admin management

**Files:**
- Create: `fleet/store/sessions.go`, `fleet/api/sessions.go`, `fleet/api/admin_admins.go`, `fleet/api/csrf.go`
- Modify: `fleet/store/schema.sql` (append), `fleet/api/auth.go` (remove the HMAC `sessions` type; keep `HashPassword`/`VerifyPassword`/limiter), `fleet/api/server.go` (login/logout/requireAdmin use the store), `fleet/api/admin.go` (mount admins routes)
- Test: `fleet/store/store_test.go` (+sessions), `fleet/api/server_test.go` (harness sends the CSRF header), `fleet/api/admins_test.go`

**Interfaces:**
```sql
CREATE TABLE IF NOT EXISTS sessions (
  id INTEGER PRIMARY KEY, token_hash BLOB NOT NULL UNIQUE, admin_id INTEGER NOT NULL REFERENCES admins(id),
  created_at TEXT NOT NULL, expires_at TEXT NOT NULL, revoked_at TEXT);
```
```go
// store
type Session struct{ ID int64; TokenHash []byte; AdminID int64; CreatedAt, ExpiresAt time.Time; RevokedAt *time.Time }
func (s *Store) CreateSession(ctx, tokenHash []byte, adminID int64, now, expires time.Time) (int64, error)
func (s *Store) SessionByHash(ctx, h []byte) (*Session, error)
func (s *Store) RevokeSession(ctx, id int64, at time.Time) error
func (s *Store) RevokeSessionsForAdmin(ctx, adminID int64, at time.Time) error
func (s *Store) DeleteAdmin(ctx, id int64) error
func (s *Store) UpdateAdminPassword(ctx, id int64, pwHash string) error

// api
//   cookie wh_session = "ws_" + base64url(32 random bytes); stored SHA-256; TTL 12 h; HttpOnly, SameSite=Strict, Secure when requestIsHTTPS
//   cookie wh_csrf    = random 32 bytes hex, NOT HttpOnly; every state-changing admin request must send header X-WarpHold-CSRF equal to it (double submit). GET is exempt.
//   requireAdmin: cookie → SessionByHash → not revoked, not expired → adminID in context.
//   DELETE /api/v1/fleet/session → revoke this session, clear both cookies.
//   GET    /api/v1/fleet/admins            → [{id,email,role,created_at}]
//   POST   /api/v1/fleet/admins            {email,password} → 201 {id}   (owner only; single role today)
//   DELETE /api/v1/fleet/admins/{id}       → 204; revokes that admin's sessions; refuses to delete the last admin (409)
//   POST   /api/v1/fleet/admins/me/password {current,new} → 204; revokes all OTHER sessions of this admin
//   Login limiter unchanged. Sessions of a deleted admin fail immediately (revoked), which is the "server-side revocation" CodeRabbit asked for.
```
The CLI test harness and any existing tests that log in must now also send `X-WarpHold-CSRF` from the `wh_csrf` cookie on POST/PUT/DELETE; update `harness.do` once.

- [ ] **Step 1: Failing tests** — `TestLogoutRevokesServerSide` (login; logout; reuse the old cookie → 401), `TestDeleteAdminRevokesSessions` (two admins; delete B while B is logged in → B's next request 401; deleting the last admin → 409), `TestCSRFRequiredOnMutations` (POST /targets without the header → 403; with header → 201; GET without header → 200), `TestChangePasswordRevokesOtherSessions`.
- [ ] **Step 2: Run, expect failures.**
- [ ] **Step 3: Implement** per the Interfaces block; `csrf.go` = middleware `requireCSRF(next)` applied inside `requireAdmin` for non-GET; issue `wh_csrf` at login.
- [ ] **Step 4: `go test ./fleet/... ./cli/ -run 'TestFleet|TestAgent' -count=1`; `cr review --agent -t uncommitted`; commit** `feat(fleet): server-side sessions, CSRF double-submit, admin management`.

---

### Task 4: Engine info file, local session handoff, SnapshotID, unit environment

**Files:**
- Create: `agent/engine/info.go`, `agent/engine/localauth.go`, `agent/engine/snapshotid.go`, `cli/command_agent_status.go`
- Modify: `agent/engine/headless.go` (write info file; wrap mux with localauth), `agent/run/loop.go` (`WatchOnce` fills `SnapshotID`), `agent/install/systemd.go` (unit `Environment=WARPHOLD_STATE_DIR=…` when `$WARPHOLD_STATE_DIR` is set at install time), `cli/command_agent.go` (register `status`)
- Test: `agent/engine/info_test.go`, `agent/engine/localauth_test.go`, `agent/run/loop_test.go` (+SnapshotID), `agent/install/systemd_test.go`

**Interfaces:**
```go
// engine.json in state.Dir(scope), mode 0600:
type Info struct{ BaseURL, User, Password string; PID int; StartedAt time.Time }
func WriteInfo(scope string, i Info) error; func ReadInfo(scope string) (*Info, error); func RemoveInfo(scope string) error
// StartHeadless writes it after listening; Stop removes it.

// localauth: the tray opens http://127.0.0.1:<port>/local/session?t=<token>
//   token = 32 random bytes hex, generated by StartHeadless, stored in Info as LocalToken; single-use per 60 s window is NOT required (loopback), but it expires with the process.
//   handler: constant-time compare; sets cookie wh_local (HttpOnly, SameSite=Strict, Path=/) = another random value kept in memory; 302 → "/".
//   middleware around the whole mux: if request carries a valid wh_local cookie and no Authorization header, inject `Authorization: Basic base64(user:password)` so Kopia's UI/control handlers accept it. Everything else passes through unchanged (the loop's apiclient still uses basic auth).
// snapshotid: after a finished Snapshot task for source path P, call GET /api/v1/snapshots?userName&host&path=P → serverapi.SnapshotsResponse; take the newest manifest whose StartTime >= task.StartTime; Report.SnapshotID = manifest.ID.
// `warphold agent status [--scope]`: reads engine.json; GET /api/v1/sources via basic auth; prints one line per source: path, status, last snapshot age, next time; exit 2 when the engine is not running.
```
`serverapi.SnapshotsResponse` / `snapshot.Manifest.ID` — confirm names in `internal/serverapi/serverapi.go` and `snapshot/manifest.go`.

- [ ] **Step 1: Failing tests** — info write/read/remove + 0600; localauth: request without cookie to `/api/v1/sources` → 401 from Kopia; `GET /local/session?t=wrong` → 403; with the right token → 302 + cookie; subsequent `/api/v1/sources` with the cookie → 200; loop test: a finished Snapshot task yields a report whose `SnapshotID` equals the fake's manifest id; unit test: `Environment=WARPHOLD_STATE_DIR=/x` present iff the env var was set.
- [ ] **Step 2: Run, expect failures. Step 3: implement. Step 4: `go test ./agent/... ./cli/ -run TestAgent -count=1`; `cr review --agent -t uncommitted`; commit** `feat(agent): engine info file, local session handoff, snapshot ids in reports, agent status`.

---

### Task 5: Part A PR

- [ ] `cr review --agent --base master` on the branch; fix Critical/Warning; re-run on the fix commit.
- [ ] `grep -rn "hody\.sh\|10\.99\." . --exclude-dir=.git --exclude-dir=.superpowers` → empty.
- [ ] Push `plan2-basics`, open PR "Plan 2A: release basics — NOTICE/README, /dl/, server-side sessions, engine info + local auth" against `master`; wait for CodeRabbit; one batched fix commit if needed; Hody merges.

---

## Part B — homelab deployment (infra; documented in the vault, never in the repo)

Conventions (from `Homelab/99-Reference/VM-Cloud-Init-Template.md` and the live Traefik CT): template VMID 9000 (Ubuntu 24.04 cloud-init, `tank`), guest VMID = 100 + last octet, static IP, `root` with the PVE SSH key, Traefik CT 101 routes in `/etc/traefik/conf.d/<name>.yml` (file provider; a single bad YAML file drops every route, so validate after each change), wildcard `*.<HOMELAB_DOMAIN>` already resolves to Traefik, LE wildcard cert via `certResolver: letsencrypt`. Fleet VM: **VMID 140, `fleet`, <FLEET_IP>**, 2 vCPU, 4 GB, 32 GB root + 1 TB data disk on `tank` at `/srv/warphold`.

### Task 6: VM 140 `fleet` and the service

**Files (vault):** `Homelab/WarpHold/WarpHold-Full-Dev-Guide.md` (build log), `Homelab/Planning/Claude-Project-Info.md` (+ row), `Homelab/00-Index/00-Homelab-Index.md` (+ entry)

- [ ] **Step 1: Clone and configure (on pve1)**

```bash
ssh pve1 '
qm clone 9000 140 --name fleet --full --storage tank &&
qm set 140 --memory 4096 --cores 2 --onboot 1 &&
qm resize 140 scsi0 +28G &&
qm set 140 --scsi1 tank:1024 &&
qm set 140 --ciuser root --sshkeys /root/homelab-pve.pub &&
qm set 140 --ipconfig0 ip=<FLEET_IP>/24,gw=<GATEWAY_IP> --nameserver <GATEWAY_IP> --searchdomain <HOMELAB_DOMAIN> &&
qm start 140'
until ssh -o ConnectTimeout=3 -o StrictHostKeyChecking=accept-new root@<FLEET_IP> true 2>/dev/null; do sleep 5; done
ssh root@<FLEET_IP> 'cloud-init status --wait; apt-get update -qq && apt-get install -y -qq qemu-guest-agent curl && systemctl enable --now qemu-guest-agent'
```
Expected: `qm status 140` → running; ssh works; `cloud-init status` → done.

- [ ] **Step 2: Data disk, user, binary**

```bash
ssh root@<FLEET_IP> 'mkfs.ext4 -L warphold /dev/sdb && mkdir -p /srv/warphold && echo "LABEL=warphold /srv/warphold ext4 defaults,noatime 0 2" >> /etc/fstab && mount -a && useradd --system --home /var/lib/warphold --create-home --shell /usr/sbin/nologin warphold && mkdir -p /etc/warphold /srv/warphold/repos && chown -R warphold:warphold /srv/warphold /var/lib/warphold && chmod 750 /var/lib/warphold'
cd ~/.dev/Projects/warphold && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /tmp/warphold-linux-amd64 . && scp /tmp/warphold-linux-amd64 root@<FLEET_IP>:/usr/local/bin/warphold && ssh root@<FLEET_IP> 'chmod 755 /usr/local/bin/warphold && warphold --version'
```

- [ ] **Step 3: Secrets and service**

Generate two 32-byte hex secrets locally, store both in 1Password (vault `hody`, item "WarpHold Fleet (<FLEET_HOST>)", fields `server_password`, `control_password`; the sealing passphrase goes in the same item as `seal_passphrase` after Step 4), then:

```bash
ssh root@<FLEET_IP> 'umask 077; cat > /etc/warphold/env <<EOF
KOPIA_SERVER_PASSWORD=<server_password>
KOPIA_SERVER_CONTROL_PASSWORD=<control_password>
EOF
chown root:warphold /etc/warphold/env; chmod 640 /etc/warphold/env
cat > /etc/systemd/system/warphold.service <<EOF
[Unit]
Description=WarpHold Fleet server
After=network-online.target
Wants=network-online.target

[Service]
User=warphold
Group=warphold
EnvironmentFile=/etc/warphold/env
ExecStart=/usr/local/bin/warphold --config-file /var/lib/warphold/repository.config server start --insecure --address http://<FLEET_IP>:51515 --no-grpc --server-username admin --server-password \${KOPIA_SERVER_PASSWORD} --server-control-password \${KOPIA_SERVER_CONTROL_PASSWORD}
Restart=on-failure
RestartSec=10
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/warphold /srv/warphold
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload'
```
Flags note (found at execution): `--insecure` is required for plain HTTP (the engine refuses to start without TLS otherwise) and `--address` must be a URL. The CLI `fleet activate` path writes no `setup-token` file; only HTTP activation uses one.
Bind note: the service listens on the VM's LAN address so Traefik (CT 101) can reach it; the setup token gates activation, Fleet routes carry their own auth, and the Kopia UI/control APIs are password-protected, so plain HTTP inside the LAN is acceptable until every client also goes through Traefik.

- [ ] **Step 4: Activate**

```bash
ssh root@<FLEET_IP> 'sudo -u warphold WARPHOLD_SEAL_PASSPHRASE="<seal_passphrase>" WARPHOLD_ADMIN_PASSWORD="<admin_password>" warphold --config-file /var/lib/warphold/repository.config fleet activate --email hody@hody.dev && systemctl enable --now warphold && sleep 3 && systemctl --no-pager status warphold | head -5 && curl -s http://<FLEET_IP>:51515/api/v1/fleet/status'
```
Expected: `{"activated":true}`. Store `admin_password` in the same 1Password item. Verify `/var/lib/warphold/fleet/seal.key` is `-rw-------` owned by `warphold`.

- [ ] **Step 5: Traefik route**

Write `/etc/traefik/conf.d/fleet.yml` on CT 101 by copying `plane.yml`'s shape exactly (router `fleet`, `Host(\`<FLEET_HOST>\`)`, `websecure`, `certResolver: letsencrypt`, service url `http://<FLEET_IP>:51515`, `passHostHeader: true`). Write it with `pct push` of a file created locally with the Write tool — never a shell heredoc (the vault's YAML-escaping incident). Then:

```bash
ssh pve1 'pct exec 101 -- sh -c "sleep 3; curl -s -o /dev/null -w %{http_code} http://127.0.0.1:8080/api/http/routers/plane@file; echo; curl -s http://127.0.0.1:8080/api/http/routers/fleet@file | head -c 300"'
curl -s https://<FLEET_HOST>/api/v1/fleet/status
```
Expected: plane router still 200 (proves the file provider did not drop everything), fleet router present, HTTPS status returns `{"activated":true}` with a valid LE cert.

- [ ] **Step 6: Vault ingest** — create `Homelab/WarpHold/WarpHold-Admin-Overview.md` (what/At-a-Glance: VM 140, IP, URL, service user, paths, 1Password item, how to start/stop/upgrade the binary), `WarpHold-Full-Dev-Guide.md` (this build log verbatim with outputs), `WarpHold-Issues-and-Fixes.md` (symptom → source map: "Traefik 404 for everything" → conf.d YAML; "activation 403" → setup token; "agent red after restart" → fixed in Plan 1 final review, etc.). Add the row to `Claude-Project-Info.md` (VM 140 — fleet — <FLEET_IP> — https://<FLEET_HOST>), the index entry, and an `INGEST` line in `hody/00-Log.md`.

---

### Task 7: Targets, template, group, enroll this laptop

- [ ] **Step 1: Admin session over HTTPS** (`curl -c cookies -b cookies`, JSON): `POST /api/v1/fleet/session` with `hody@hody.dev`; capture the `wh_csrf` cookie and send it as `X-WarpHold-CSRF` on every mutation below.
- [ ] **Step 2: Target** `{"name":"fleet-local","kind":"filesystem","path":"/srv/warphold/repos"}` → 201. (B2 target = HUMAN CHECKPOINT: needs a bucket with Object Lock and an admin application key from Hody's B2 account; when provided, `POST /targets` with `kind:"b2"` and expect `object_lock_verified:true`.)
- [ ] **Step 3: Template** "Laptop home": `sources: ["~"]`, policy = the spec §10 excludes as Kopia `files.ignore`, `scheduling: {"intervalSeconds": 3600, "runMissed": true}`, `retention: {"keepHourly":24,"keepDaily":7,"keepWeekly":4,"keepMonthly":6}`, `compression: {"compressorName":"zstd"}`. Verify the exact JSON field names against `snapshot/policy/*.go` before posting (`FilesPolicy.IgnoreRules` is `files.ignore`; check `retention` and `scheduling` keys).
- [ ] **Step 4: Group** "Laptops" → target fleet-local, template Laptop home. **Token** for it (default 1 h).
- [ ] **Step 5: Enroll this laptop for real** (user scope, default state dir): `curl -fsSL https://<FLEET_HOST>/enroll.sh | sh -s -- --token <token>`; expected: binary downloaded from `/dl/`, `agent enroll` done, `agent install` enabled the user unit (linger on). Check `systemctl --user status warphold-agent`, `warphold agent status`, and within ~10 min `GET /api/v1/fleet/agents` → this laptop green with a real home-directory snapshot (first full snapshot of ~60 GB will take longer; the report arrives when it finishes). Queue `snapshot-now` and confirm the report.
- [ ] **Step 6: Cleanup of Plan 1's `/tmp/warphold-live`** on this laptop; leave the real enrollment in place (this is the laptop's backup from now on). Note in the vault that the FW16 fallback laptop gets the same one-liner when Hody boots it.
- [ ] **Step 7: Vault**: Admin-Overview "Day-to-day ops" (how to add a device, rotate a token, read health), Issues-and-Fixes entries for anything hit, `INGEST` line.

---

### Task 8: Monitoring and backups of the Fleet server

- [ ] Uptime Kuma (`ssh monitor`): add an HTTPS keyword monitor for `https://<FLEET_HOST>/api/v1/fleet/status` expecting `"activated":true`, 2-minute interval, notify like the other monitors.
- [ ] PBS: confirm VM 140 is included in the nightly backup job (`ssh pve1 'cat /etc/pve/jobs.cfg'`; if the job enumerates VMIDs, add 140); note that `/srv/warphold/repos` lives on the VM's data disk and is therefore in the PBS backup (Kopia repos are dedup-friendly but large; watch datastore growth).
- [ ] homelab dashboard card: follow `homelab-dashboard-homepage` memory (diff against the live page before pushing).
- [ ] Vault: Admin-Overview "Backup/restore of the Fleet server itself" (PBS restore of VM 140 restores both state and repos; the sealing key file is inside `/var/lib/warphold/fleet/`), `INGEST` line.

---

## Part C — the WarpHold UI (repo `hodyhq/warphold-ui`, then the import swap in `hodyhq/warphold`)

The prototype artboards are the pixel reference: `Main.dc.html` (Fleet: Overview/Devices/Detail/Groups/Policies/Targets/Settings/Add device), `Solo.dc.html` (single machine: Snapshots/History/Browse+Restore/Policies/Tasks/Repository), `Activate.dc.html`, `Agent.dc.html`, `Tray.dc.html`. Tokens and type are in `docs/superpowers/design/README.md`. Where a task says "as in `<file>`", copy layout, spacing, copy and colors from that artboard; only data wiring is new.

### Task 9: Fork `kopia/htmlui` → `hodyhq/warphold-ui`, Go module, build pipeline

**Files (new repo):** `htmlui.go`, `go.mod` (`module github.com/hodyhq/warphold-ui`, `go 1.22`), `build/` (committed), `scripts/release-build.sh`, `index.html`, `public/` (favicon/logo from `icons/warphold*`), `package.json` (name `warphold-ui`), `README.md`, `.coderabbit.yaml` (same as the main repo minus the Go path instructions)

**Interfaces:** `func AssetFile() http.FileSystem` (identical to upstream `htmluibuild`), served by the WarpHold server after Task 16's import swap.

- [ ] **Step 1: Fork and clone**

```bash
gh repo fork kopia/htmlui --fork-name warphold-ui --clone=false
git clone git@github.com:hodyhq/warphold-ui.git ~/.dev/Projects/warphold-ui && cd ~/.dev/Projects/warphold-ui
git remote add upstream https://github.com/kopia/htmlui.git
npm ci && npm run build && npm test   # baseline must be green before any change
```

- [ ] **Step 2: Go module at the repo root**

`htmlui.go`:
```go
// Package warpholdui embeds the built WarpHold web UI.
package warpholdui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed build
var data embed.FS

// AssetFile returns the built UI as an http.FileSystem.
func AssetFile() http.FileSystem {
	f, err := fs.Sub(data, "build")
	if err != nil {
		panic("could not embed warphold ui: " + err.Error())
	}
	return http.FS(f)
}
```
`go.mod`: `module github.com/hodyhq/warphold-ui` / `go 1.22`. Remove `build/` from `.gitignore`; add `scripts/release-build.sh` = `npm ci && npm run build && git add -f build && git commit -m "build: ui $(git rev-parse --short HEAD)"` (the build output is committed on purpose, mirroring upstream's htmluibuild). Verify `go build ./...` in the UI repo and `go vet`.

- [ ] **Step 3: Branding** — `index.html` title `WarpHold`, description "WarpHold — backups that hold", favicon/`logo192.png`/`logo512.png` generated from `icons/warphold-app-1024.png` (copy the PNGs from the main repo's `icons/`), `manifest.json` name/short_name `WarpHold`, theme color `#16181D`, background `#16181D`. Replace `kopia-flat.svg` references in `src/` with `warphold.svg`.

- [ ] **Step 4: Commit** `chore: fork as warphold-ui; Go module; branding` and push to `master` of the fork (this repo's default branch is whatever upstream's is — check with `git branch -r`).

---

### Task 10: Design system (tokens, fonts, components)

**Files:** `src/design/tokens.css`, `src/design/fonts.css` + `src/design/fonts/*.woff2` (OFL: Unbounded 600/800, Space Grotesk 400/500/600, Space Mono 400; download from Google Fonts' GitHub sources, keep each family's OFL.txt next to the files), `src/design/components/{Button,Input,Select,Field,Card,Eyebrow,Kpi,Pill,HealthBar,Strip,Table,Dialog,Toast,Nav}.tsx`, `src/design/components/index.ts`, `tailwind.config.js` + `postcss.config.js` (Tailwind v4 via `@tailwindcss/vite`), `src/design/components/__tests__/*.test.tsx`
**Deps (npm):** `tailwindcss@^4`, `@tailwindcss/vite@^4`, `clsx`. Remove `bootstrap` and `react-bootstrap` only in Task 15 (single-user pages still import them until then).

`src/design/tokens.css` (exact values from the prototype):
```css
@import "tailwindcss";
@theme {
  --color-ground: #16181D;
  --color-panel: #1D2027;
  --color-line: #2A2E36;
  --color-line-strong: #3A3F49;
  --color-ink: #F2F3F5;
  --color-ink-soft: #C7CBD4;
  --color-muted: #9AA0AD;
  --color-dim: #6D7381;
  --color-ember: #FF6A1A;
  --color-ember-soft: #FF8A4C;
  --color-ember-hover: #FFB089;
  --color-good: #2FBF83;
  --color-warn: #F5B942;
  --color-bad: #FF5D5D;
  --color-bad-panel: #1F1B1D;
  --color-warn-panel: #1F1D18;
  --font-display: "Unbounded", "Space Grotesk", system-ui, sans-serif;
  --font-body: "Space Grotesk", system-ui, sans-serif;
  --font-mono: "Space Mono", ui-monospace, monospace;
  --radius-sm: 4px;
  --radius-pill: 999px;
}
body { background: var(--color-ground); color: var(--color-ink); font-family: var(--font-body); font-size: 14px; }
a { color: var(--color-ember-soft); text-decoration: none; } a:hover { color: var(--color-ember-hover); }
```

Component contracts (TypeScript, all `export function` with explicit prop types; use `clsx`; no external UI library):
- `Button({variant:'primary'|'default'|'danger'|'ghost', ...button props})` — `.btn` from the prototype: uppercase 12px 0.06em, radius 4px, primary = ember bg + ground text.
- `Eyebrow({children})` — Space Mono 11px, 0.12em, uppercase, muted.
- `Kpi({label, value, unit?, tone?:'good'|'warn'|'bad'|'ink', sub?})` — Unbounded 800 28px value.
- `Pill({tone, children})`, `HealthBar({tone, height?})` (the 8×28 bar), `Strip({days: ('good'|'warn'|'bad'|'none')[], height?})` (the 30-day heartbeat: flex, gap 2px).
- `Card({children, tone?:'default'|'bad'|'warn'})` — panel bg, 1px line, 20/22px padding, gap 12.
- `Field({label, children})`, `Input`, `Select` — ground bg, line-strong border, 10/12 padding.
- `Table({columns, rows, onRowClick?})` — grid rows with 1px line separators, hover panel; column templates passed as a CSS `grid-template-columns` string.
- `Dialog({open, onClose, title, children})` — the `.overlay` + card from the Add-device panel.
- `Toast` — bottom-right, 5 s, tone.
- `Nav({items:[{to,label}], current})` — uppercase 12px links with the ember underline.

- [ ] **Step 1:** vitest tests first: each component renders and applies its tone class (`Button` primary has `bg-ember`; `Strip` renders 30 cells; `Kpi` shows unit; `Dialog` hidden when `open=false`).
- [ ] **Step 2:** implement; `npm test`, `npm run lint`, `npm run build`.
- [ ] **Step 3:** commit `feat(design): Kinetic tokens, fonts, component set`.

---

### Task 11: App shell, mode detection, Fleet API client, login

**Files:** `src/mode.ts`, `src/api/fleet.ts`, `src/AppShell.tsx`, `src/pages/fleet/Login.tsx`, `src/App.jsx` (routes), tests `src/__tests__/mode.test.ts`, `src/__tests__/fleetApi.test.ts`, `src/pages/fleet/__tests__/Login.test.tsx`

`src/mode.ts`:
```ts
export type Mode = "fleet" | "solo" | "agent";
// GET /api/v1/fleet/status → 200 {activated:true} ⇒ "fleet"; 200 {activated:false} ⇒ "fleet" (show Activate wizard); 404 ⇒ the server has no Fleet routes: it is an agent engine (local) or plain single-user ⇒ "agent" when location.hostname is 127.0.0.1/localhost AND /api/v1/sources succeeds with the local cookie, else "solo".
export async function detectMode(): Promise<{ mode: Mode; activated: boolean }>
```
`src/api/fleet.ts`: typed axios instance (`withCredentials: true`, base `/api/v1/fleet`, interceptor adds `X-WarpHold-CSRF` from the `wh_csrf` cookie on non-GET, 401 → redirect to `/fleet/login`). Functions: `status, activate, login, logout, admins, overview, agents, agent(id), agentCommand(id, kind, source), revokeAgent(id), groups, createGroup, tokens(groupId), createToken(groupId, ttl, uses), revokeToken(id), templates, createTemplate, updateTemplate, targets, createTarget, settings, setSetting, changePassword, deleteAdmin, inviteAdmin, setupTokenHint` — each returning the typed shapes from Plan 1's endpoints (define `types.ts` with `AgentOut`, `Report`, `Group`, `Template`, `Target`, `TokenOut`, `Overview`).

`AppShell.tsx`: the Kinetic shell as in `Main.dc.html` (header: mark + `WARPHOLD`, `Nav`, fleet name from settings, primary action slot; the skewed `#1D2027` panel behind the left column on Overview only). Routes: `/fleet` (Overview), `/fleet/devices`, `/fleet/devices/:id`, `/fleet/groups`, `/fleet/policies`, `/fleet/targets`, `/fleet/settings`, `/fleet/activate`, `/fleet/login`; `/` → `/fleet` in fleet mode, → `/snapshots` in solo mode, → `/agent` in agent mode. Single-user routes unchanged until Task 15.

- [ ] Tests first (mode detection with mocked axios; API client attaches CSRF; Login renders and posts). Implement. `npm test && npm run build`. Commit `feat(ui): app shell, mode detection, Fleet API client, login`.

---

### Task 12: Overview endpoint + Overview screen

**Server (repo warphold, branch `plan2-ui-server`):** `fleet/api/overview.go`, `GET /api/v1/fleet/overview` (admin):
```json
{ "fleet_name": "...", "counts": {"agents":7,"green":5,"yellow":1,"red":1,"unknown":0,"targets":2},
  "stored_bytes": 0, "dedup_ratio": null,
  "last24h": {"completed":41,"failed":1,"buckets":[{"hour":"2026-09-02T00:00:00Z","ok":3,"failed":0}, …24]},
  "latest_failure": {"agent_id":"ag_…","name":"media-nuc","finished_at":"…","stderr":"…"} | null,
  "devices": [ {"id","name","group","health","last":"2 h ago" (server computes relative from last OK), "size_bytes":0, "days":[…30 of "good|warn|bad|none"]} ] }
```
`days[i]` for day `i` (0 = 29 days ago): `good` if any snapshot report with status ok finished that UTC day, `bad` if only errors, `none` otherwise. `stored_bytes`/`dedup_ratio` are `0`/`null` in this plan (Kopia repo stats come with Plan 3's jobs); the UI hides the Stored tile when null. Store: add `ReportsSince(ctx, since time.Time) ([]Report, error)`. Test with seeded reports across days.

**UI:** `src/pages/fleet/Overview.tsx` exactly as `Main.dc.html`'s Overview: headline `N/M protected right now` (Unbounded 64px, ember slash), KPI grid (Protected/Stale/Failing/Stored), the 24 h tick timeline (30 ticks → use the 24 hourly buckets, height ∝ ok count, red when failed>0, ember for the current hour), the latest-failure callout (opens the device), the right column device list with `HealthBar`, name, meta, `Strip`. Polling every 30 s. Test: renders counts and 24 buckets from a fixture; clicking a device navigates.

- [ ] Server: tests → implement → `cr review -t uncommitted` → commit `feat(fleet): overview endpoint`. UI: tests → implement → commit `feat(ui): fleet overview`.

---

### Task 13: Devices list and Device detail

**UI:** `Devices.tsx` (table per `Main.dc.html` Devices: filter chips All/Failing/Stale/<groups>, columns bar·device·group·30 days·last snapshot·size·agent version) and `Device.tsx` (detail: eyebrow group/version/OS/scope, actions Snapshot now / Pause / Resume / Verify (disabled, "runs from Fleet in a later version") / Recovery kit (disabled until Plan 3) / Revoke (confirm dialog), the recovery-kit banner (hidden until Plan 3), three cards (last snapshot + next, stored, 30 days), Sources (from the group's template; Fleet has no per-agent source list yet → show the template sources with "~ expands on the device"), Reports table with click-to-expand raw stderr in the `bad-panel` `<pre>`). Data from `agents`, `agent(id)` (flat shape + `reports`), `groups`, `templates`. Commands via `agentCommand`. Tests: list filters; detail expands stderr; revoke requires confirmation.

- [ ] Tests → implement → commit `feat(ui): devices list and device detail`.

---

### Task 14: Groups, Policy templates, Targets, Settings, Activate

- **Groups.tsx** (cards per `Main.dc.html` Groups): target + template, member count, **Download installer** = shows the one-liner `curl -fsSL <origin>/enroll.sh | sh -s -- --token <token>` after creating a token via a small dialog (TTL select: 1 h default, 24 h, 7 d, 30 d; uses: 1 / unlimited), copy button; **Tokens** dialog lists tokens with expiry/uses/revoke; New group dialog.
- **Templates.tsx**: left list, right form per the prototype: Sources (one path per line), Schedule (select: hourly / daily at HH:MM / manual; "only on AC" is a Plan 3 agent feature → omit), Exclude (one glob per line), Keep (four numbers), Compression (select zstd / none / auto); **Advanced** drawer = a monospace JSON editor of the full Kopia policy object (validated by the server); Save = `updateTemplate`; "push to N devices" wording from the group count.
- **Targets.tsx**: cards; Add target dialog with kind filesystem (path) / B2 (bucket, region label, key id, key); shows `object_lock_verified` pill; never displays keys.
- **Settings.tsx**: Admins card (list, invite dialog, delete with confirm — last admin delete shows the 409 message), Weekly digest card (read-only "Plan 3"), Sealing passphrase card (text + "Change" disabled with explanation: rotation ships with the kit in Plan 3), Agents card (poll interval setting via `setSetting("poll_interval")`, health thresholds read-only).
- **Activate.tsx**: four steps per `Activate.dc.html`; step 1 asks for the **setup token** (with the sentence "Find it in the server log or at <state dir>/setup-token") plus passphrase ×2; step 2 first admin; step 3 storage — first target (filesystem path or B2); step 4 done with the enrollment one-liner. Posts `/activate` with header `X-WarpHold-Setup-Token`.
- Tests per page (render from fixtures; dialogs open/close; API called with expected bodies).

- [ ] Tests → implement → commit `feat(ui): groups, templates, targets, settings, activate wizard`. Open the first UI PR ("warphold-ui: design system, shell, Fleet screens"); CodeRabbit; merge.

---

### Task 15: Restyle the single-user pages

Replace `react-bootstrap` usage in `src/pages/{Snapshots,SnapshotHistory,SnapshotDirectory,SnapshotRestore,SnapshotCreate,Policies,Policy,Tasks,Task,Repository,Preferences}.jsx`, `src/components/*` and `src/forms/*` with the design-system components, following `Solo.dc.html`: Snapshots = headline "Nh since the last good snapshot" + 3 KPIs + current-task card + Sources list with History links; History = table (Taken / Kept because / Files / Size / Browse); Browse = breadcrumb + file table + the restore action card; Policies = cards; Tasks = running card with progress + log + finished rows; Repository = two cards + the "Turn this machine into a Fleet server" card (links to `/fleet/activate`). Keep every existing API call, form validation, and `data-testid` used by upstream's Playwright e2e (`tests/htmlui_e2e_test` in the main repo). The upstream policy editor component (`src/components/policy-editor`) is kept functionally and restyled; it is also what the Templates page's Advanced drawer reuses.
- [ ] Work page by page with `npm test` green after each; remove `bootstrap`/`react-bootstrap` from `package.json` last; `npm run build`; commit per page; open the second UI PR ("warphold-ui: single-user pages restyle"); CodeRabbit; merge; run `scripts/release-build.sh` and tag `v0.1.0`.

---

### Task 16: Wire the UI into the server (import swap) and the agent engine

**Files (repo warphold):** `internal/server/htmlui_embed.go` (the one import line + a `// warphold:` comment), `go.mod` (`require github.com/hodyhq/warphold-ui v0.1.0`), `agent/engine/headless.go` (`srv.ServeStaticFiles(m, warpholdui.AssetFile())` after the API handlers), `tests/htmlui_e2e_test` (title assertion "WarpHold" if it checks the title), `Makefile` (drop the `localhtmlui.work` target or repoint it to `../warphold-ui`)
- [ ] `go get github.com/hodyhq/warphold-ui@v0.1.0`; swap the import; `CGO_ENABLED=0 go build ./... && go test ./fleet/... ./agent/... ./cli/... -count=1`; run `dist/warphold server start --insecure --without-password --address 127.0.0.1:51600 --no-grpc` and open it in Chrome: the WarpHold shell renders in solo mode (screenshot in the report). Commit `feat: serve the WarpHold UI (import swap); agent engine serves the agent page`. This branch (`plan2-ui-server`) also carries Task 12's overview endpoint; PR + CodeRabbit + merge.

---

## Part D — tray and agent page (repo warphold, branch `plan2-tray`)

### Task 17: Agent page (agent mode of the SPA)

**UI:** `src/pages/agent/AgentHome.tsx` per `Agent.dc.html`: headline `OK / last good backup N ago` (or `ATTENTION` in bad tone), Protected size (sum of latest snapshot sizes from `/api/v1/sources`), Next time, the 30-day strip (from `/api/v1/snapshots` per source: days with a snapshot), Sources list, Recent runs (from `/api/v1/tasks`, click to expand the log via `/api/v1/tasks/{id}/logs`), Back up now (`POST /api/v1/sources/upload`), Pause 1 h (`POST /api/v1/control/pause-source` — note Kopia's pause has no duration; show "Resume" afterwards). Vault name = the agent's display name: expose it by having the engine serve `GET /local/info` → `{name, group}` from `agent.json` (add to `localauth.go`). Never show target/bucket/keys.
- [ ] UI tests (fixtures) → implement → UI PR 3 → tag `v0.2.0`; bump the Go module in the main repo.

### Task 18: Tray

**Files:** `agent/tray/tray.go`, `agent/tray/notify.go` (D-Bus `org.freedesktop.Notifications` via `github.com/godbus/dbus/v5`, already an indirect dependency — promote to direct), `agent/install/autostart.go` (`~/.config/autostart/warphold-tray.desktop`: `Exec=<binary> agent tray`, `X-GNOME-Autostart-enabled=true`), `cli/command_agent_tray.go`, `agent/install/systemd.go` (`agent install` also writes the autostart entry in user scope), tests for autostart rendering and the menu model (pure functions building menu items from engine status).
**Dependency gate:** run the dependency review for `fyne.io/systray@v1.12.2` before `go get`; record CVEs/none.
**Behavior (from `Tray.dc.html`):** icon = the mark, tinted ember while uploading, good/warn/bad otherwise (four embedded PNGs at 22 px + 44 px, generated from `icons/warphold.svg` with the tone colors, `//go:embed`); menu: `Laptops · hody-fw13` (vault name, disabled item), `Backing up /home/hody — 38%` (live, from `/api/v1/sources` CurrentTask + upload counters), `Last good backup: today 19:00`, `Next: 21:00`, `Errors this week: N` (opens Details), separator, `Back up now`, `Pause` / `Resume`, `Details…` (opens `http://127.0.0.1:<port>/local/session?t=<token>` with `xdg-open`), separator, `Quit tray`. Poll the engine every 5 s; if `engine.json` is missing → icon dim + "Agent not running" + "Start agent" (runs `systemctl --user start warphold-agent`). Desktop notification on a transition to bad only (once per failure).
- [ ] Tests → implement → `go test ./agent/... -count=1` → live check on this laptop: `warphold agent tray &` while the real agent runs; the Omarchy bar shows the icon; Details opens the agent page logged in; screenshot both. PR + CodeRabbit + merge.

### Task 19: Fleet UI live on <FLEET_HOST>, screenshots, vault, README

- [ ] Build the release binary with the UI embedded (Task 16's module), deploy to VM 140 (`scp` + `systemctl restart warphold`), open `https://<FLEET_HOST>` in Chrome: login → Overview shows this laptop green; take screenshots of Overview, Devices, Device detail, Groups (with the installer one-liner), and the agent page on the laptop; add them to `docs/screenshots/` in the main repo and to the README ("Screenshots" under Status); vault: Admin-Overview gains the UI walkthrough, Full-Dev-Guide the UI deploy steps, Issues-and-Fixes anything hit; `INGEST` line; Claude memory update.
- [ ] Final `cr review --agent --base master` on any remaining branch; PR; merge.

---

## Deferred to Plan 3 (from spec §13 M6–M7, §12 M9)
Recovery kit HTML + acks + banner; verify / test-restore / maintenance / digest jobs and the `verify` agent command; SMTP settings; passphrase rotation; standalone-restore CI test with the pinned upstream `kopia`; Kopia repo stats for the Stored tile; "only on AC" scheduling on the agent; B2 real-bucket reconcile.
