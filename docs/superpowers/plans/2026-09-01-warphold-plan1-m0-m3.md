# WarpHold Plan 1 (M0–M3): fork, Fleet server, enrollment, Linux agent

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A forked, renamed `warphold` binary whose Fleet server can enroll a Linux machine, provision it a prefix-isolated repository, push a backup policy to it, and receive its health reports over the API.

**Architecture:** One Go binary from the kopia/kopia fork. Fleet is a new `fleet/` package tree (SQLite state, sealed secrets, HTTP handlers) mounted onto Kopia's existing gorilla/mux router through a 3-line hook. The agent is a set of new `cli/command_agent_*.go` files that start Kopia's own server engine headless on loopback and drive it through Kopia's `apiclient`, so the agent has no coupling to server internals. Fleet ↔ agent is two JSON endpoints (poll, report) over HTTPS with bearer tokens.

**Tech Stack:** Go 1.25.8+ (via mise), kingpin v2 (upstream CLI), gorilla/mux (upstream), `modernc.org/sqlite` (new), `golang.org/x/crypto` argon2 + nacl/secretbox (already a dependency), `github.com/stretchr/testify` (upstream), Kopia's `internal/servertesting` + `tests/testenv` for in-process tests.

**Spec:** `docs/superpowers/specs/2026-09-01-warphold-core-fleet-design.md` (§3 architecture, §4 data model, §5 enrollment, §6 protocol, §7 security). **UI prototype** (visual reference for later plans, and for copy/naming here): `docs/superpowers/design/`.

## Global Constraints

- Module path stays `github.com/kopia/kopia`. Never edit `repo/`, `snapshot/`, `fs/`, or `internal/` (engine, crypto, format). All new code lives in `fleet/`, `agent/`, and new `cli/command_fleet_*.go` / `cli/command_agent_*.go` / `cli/server_hooks.go` files.
- **Upstream files touched, exactly four:** `cli/app.go` (2 fields + 2 setup lines), `cli/command_server_start.go` (3-line hook in `setupHandlers`), `Makefile` (one target), `.goreleaser.yml` (binary name). Nothing else. Every touch is marked with a `// warphold:` comment.
- New dependencies allowed: `modernc.org/sqlite` only. Ask before any other.
- `CGO_ENABLED=0` must still build.
- Secrets (repo passwords, B2 keys, bearer tokens, sealing key) are never logged and never returned by any admin endpoint. Stored only sealed (`fleet/seal`) or hashed (SHA-256 for tokens, argon2id for passwords).
- Every error surfaced to Fleet is the raw text from Kopia, never paraphrased.
- Health: green when newest successful snapshot < 26 h, yellow < 7 d, red otherwise or when the last run failed.
- Enrollment tokens: default TTL 1 h, max 30 d, default `max_uses` 1, `0` = unlimited.
- Tests: `go test ./fleet/... ./agent/... ./cli/...` must pass with no network and no cloud credentials. B2 calls go through an interface with an `httptest` fake.
- Commit after every task with the message given. No Claude co-author trailer (Hody's preference).

## File structure

```
main.go                            (unchanged; still `package main` at repo root)
cli/app.go                         + fleet commandFleet, agent commandAgent (2 lines each)
cli/command_server_start.go        + hook call in setupHandlers (3 lines)
cli/server_hooks.go                RegisterServerHandlers, serverExtraHandlers
cli/command_fleet.go               `fleet` command group; registers the mount hook
cli/command_fleet_activate.go      `fleet activate` (passphrase, first admin)
cli/command_agent.go               `agent` command group
cli/command_agent_enroll.go        `agent enroll --server --token`
cli/command_agent_run.go           `agent run` (headless engine + poll loop)
cli/command_agent_install.go       `agent install` (systemd user unit + linger)
fleet/paths.go                     StateDirFor, Paths
fleet/store/store.go               Open, migrations, Close
fleet/store/schema.sql             embedded DDL
fleet/store/admins.go …            one file per table (admins, targets, templates, groups, agents, tokens, reports, commands, settings)
fleet/seal/seal.go                 argon2id + secretbox, key file
fleet/b2api/client.go              minimal B2 native API client (authorize, list buckets, create/delete key)
fleet/enroll/token.go              enrollment tokens
fleet/enroll/provision.go          repo creation + key provisioning → Bundle
fleet/health/health.go             Status(agent, reports, now)
fleet/api/server.go                Server, Mount, routes
fleet/api/auth.go                  admin passwords, sessions, rate limit
fleet/api/admin_*.go               targets, templates, groups, tokens, agents handlers
fleet/api/agent_endpoints.go       enroll, poll, report
fleet/api/enrollsh.go              /enroll.sh template
agent/state/state.go               agent.json, dirs
agent/poll/client.go               Poll, Report
agent/engine/local.go              Apply policy, run commands, watch tasks via apiclient
agent/install/systemd.go           unit + autostart file rendering
```

---

### Task 1: Toolchain, fork, upstream remote, docs moved in

**Files:**
- Create: `~/.dev/Projects/warphold/` becomes the fork checkout (docs already there are moved into it)
- Create: `docs/superpowers/UPSTREAM.md`

**Interfaces:**
- Produces: a buildable checkout at `~/.dev/Projects/warphold` with remotes `origin` = `github.com/hodyhq/warphold`, `upstream` = `github.com/kopia/kopia`.

- [ ] **Step 1: Install Go with mise and verify**

```bash
mise use -g go@1.26
go version
```
Expected: `go version go1.26.x linux/amd64`.

- [ ] **Step 2: Fork on GitHub and clone next to the docs**

```bash
source ~/.config/environment.d/github.conf; export GH_TOKEN="${GH_TOKEN:-$GITHUB_PERSONAL_ACCESS_TOKEN}"
gh repo fork kopia/kopia --org hodyhq --fork-name warphold --clone=false
cd ~/.dev/Projects && mv warphold warphold-docs
git clone git@github.com:hodyhq/warphold.git warphold
cd warphold && git remote add upstream https://github.com/kopia/kopia.git && git fetch upstream --depth=1 master
cp -r ../warphold-docs/docs ./docs && rm -rf ../warphold-docs
```
Expected: `git remote -v` shows origin and upstream; `ls docs/superpowers` shows `specs design plans`.

- [ ] **Step 3: Build and run the unchanged upstream tree**

```bash
CGO_ENABLED=0 go build -o /tmp/kopia-check . && /tmp/kopia-check --version
```
Expected: prints a version string (e.g. `v0.23.x` or `unknown build`). If `go build` complains about the Go version, run `mise use -g go@<version go.mod asks for>`.

- [ ] **Step 4: Write UPSTREAM.md (the merge procedure)**

```markdown
# Upstream merge procedure

WarpHold tracks kopia/kopia. Merge upstream at least monthly:

    git fetch upstream master
    git merge upstream/master

Expected conflicts only in the four touched files (grep `warphold:` to find every touch):
cli/app.go, cli/command_server_start.go, Makefile, .goreleaser.yml.
After resolving: `CGO_ENABLED=0 go build ./... && go test ./fleet/... ./agent/... ./cli/...`.
```

- [ ] **Step 5: Commit**

```bash
git add docs && git commit -m "docs: WarpHold spec, prototype, plan 1, upstream merge procedure"
git push -u origin main
```

---

### Task 2: Rename the binary to `warphold`

**Files:**
- Modify: `Makefile` (add one target after the `install:` target, line ~57)
- Modify: `.goreleaser.yml` (`builds:` block, line ~3)

**Interfaces:**
- Produces: `make warphold-build` → `dist/warphold`; goreleaser artifacts named `warphold`.

- [ ] **Step 1: Add the Makefile target**

Insert after the `install-noui: install` line:

```make
# warphold: build the renamed binary for the current OS/arch.
warphold-build:
	CGO_ENABLED=0 go build $(KOPIA_BUILD_FLAGS) -tags "$(KOPIA_BUILD_TAGS)" -o dist/warphold$(exe_suffix) .
```

- [ ] **Step 2: Rename in goreleaser**

Add at the top of `.goreleaser.yml` (before `builds:`) and inside the first build entry:

```yaml
project_name: warphold   # warphold: renamed fork of kopia
builds:
- binary: warphold       # warphold:
  env:
  - CGO_ENABLED=0
```
(keep every other key of that build entry as it is).

- [ ] **Step 3: Verify**

```bash
make warphold-build && ./dist/warphold --help | head -3
```
Expected: `usage: warphold [<flags>] <command> [<args> ...]` (kingpin takes the name from `os.Args[0]`).

- [ ] **Step 4: Commit**

```bash
git add Makefile .goreleaser.yml && git commit -m "build: produce the warphold binary"
```

---

### Task 3: Fleet state store (SQLite)

**Files:**
- Create: `fleet/paths.go`, `fleet/store/store.go`, `fleet/store/schema.sql`, `fleet/store/admins.go`, `fleet/store/targets.go`, `fleet/store/templates.go`, `fleet/store/groups.go`, `fleet/store/agents.go`, `fleet/store/tokens.go`, `fleet/store/reports.go`, `fleet/store/commands.go`, `fleet/store/settings.go`
- Test: `fleet/store/store_test.go`

**Interfaces:**
- Produces (used by every later Fleet task):

```go
package fleet
func StateDirFor(configFile string) string           // filepath.Join(filepath.Dir(configFile), "fleet")
type Paths struct{ StateDir, DB, KeyFile string }
func PathsFor(stateDir string) Paths                  // DB=stateDir/fleet.db, KeyFile=stateDir/seal.key

package store
var ErrNotFound = errors.New("not found")
type Store struct{ db *sql.DB }
func Open(path string) (*Store, error)                // creates file, runs schema.sql (idempotent)
func (s *Store) Close() error

type Admin struct{ ID int64; Email, PWHash, Role string; CreatedAt time.Time }
func (s *Store) CreateAdmin(ctx context.Context, email, pwHash string) (int64, error)
func (s *Store) AdminByEmail(ctx context.Context, email string) (*Admin, error)
func (s *Store) AdminByID(ctx context.Context, id int64) (*Admin, error)
func (s *Store) Admins(ctx context.Context) ([]Admin, error)

type Target struct{ ID int64; Name, Kind, Bucket, Region, Path string; SealedAdminKey []byte; ObjectLockVerifiedAt *time.Time; CreatedAt time.Time }
func (s *Store) CreateTarget(ctx context.Context, t *Target) (int64, error)
func (s *Store) Target(ctx context.Context, id int64) (*Target, error)
func (s *Store) Targets(ctx context.Context) ([]Target, error)
func (s *Store) SetTargetObjectLockVerified(ctx context.Context, id int64, at time.Time) error

type Template struct{ ID int64; Name string; Sources []string; PolicyJSON json.RawMessage; CreatedAt time.Time }
func (s *Store) CreateTemplate(ctx context.Context, t *Template) (int64, error)
func (s *Store) UpdateTemplate(ctx context.Context, t *Template) error
func (s *Store) Template(ctx context.Context, id int64) (*Template, error)
func (s *Store) Templates(ctx context.Context) ([]Template, error)

type Group struct{ ID int64; Name string; TargetID, TemplateID int64; CreatedAt time.Time }
func (s *Store) CreateGroup(ctx context.Context, g *Group) (int64, error)
func (s *Store) Group(ctx context.Context, id int64) (*Group, error)
func (s *Store) Groups(ctx context.Context) ([]Group, error)

type Agent struct{ ID, Name, Hostname, OS, Arch, Version, Scope string; GroupID int64; BearerHash []byte; SealedBundle []byte; PolicyETag string; EnrolledAt time.Time; LastSeenAt, RevokedAt *time.Time }
func (s *Store) CreateAgent(ctx context.Context, a *Agent) error
func (s *Store) Agent(ctx context.Context, id string) (*Agent, error)
func (s *Store) AgentByBearerHash(ctx context.Context, h []byte) (*Agent, error)
func (s *Store) Agents(ctx context.Context) ([]Agent, error)
func (s *Store) TouchAgent(ctx context.Context, id string, at time.Time, version, etag string) error
func (s *Store) RevokeAgent(ctx context.Context, id string, at time.Time) error

type Token struct{ ID int64; Hash []byte; GroupID int64; ExpiresAt time.Time; MaxUses, Uses int; RevokedAt *time.Time; CreatedBy int64 }
func (s *Store) CreateToken(ctx context.Context, t *Token) (int64, error)
func (s *Store) TokenByHash(ctx context.Context, h []byte) (*Token, error)
func (s *Store) IncrementTokenUses(ctx context.Context, id int64) error
func (s *Store) RevokeToken(ctx context.Context, id int64, at time.Time) error
func (s *Store) TokensForGroup(ctx context.Context, groupID int64) ([]Token, error)

type Report struct{ ID int64; AgentID, TaskID, Kind, Source string; StartedAt, FinishedAt time.Time; Status string; Bytes, Files int64; SnapshotID, Stderr string }
func (s *Store) AddReport(ctx context.Context, r *Report) (int64, error)   // unique on (agent_id, task_id): duplicate → returns existing id, no error
func (s *Store) ReportsForAgent(ctx context.Context, agentID string, limit int) ([]Report, error)   // newest first
func (s *Store) LatestReports(ctx context.Context) (map[string]Report, error)  // newest report per agent

type Command struct{ ID int64; AgentID, Kind, Source string; CreatedAt time.Time; AckedAt *time.Time }
func (s *Store) AddCommand(ctx context.Context, c *Command) (int64, error)
func (s *Store) PendingCommands(ctx context.Context, agentID string) ([]Command, error)
func (s *Store) AckCommand(ctx context.Context, id int64, at time.Time) error

func (s *Store) Setting(ctx context.Context, key string) (string, error)     // "" if unset
func (s *Store) SetSetting(ctx context.Context, key, value string) error
```

- [ ] **Step 1: Add the dependency**

```bash
go get modernc.org/sqlite@latest && go mod tidy
```
Expected: `go.mod` gains `modernc.org/sqlite`; `go build ./...` still passes.

- [ ] **Step 2: Write the failing test**

`fleet/store/store_test.go`:

```go
package store_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/store"
)

func openTemp(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fleet.db")
	s, err := store.Open(p)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	s, err = store.Open(p) // schema already applied
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

func TestAdminsRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	id, err := s.CreateAdmin(ctx, "hody@hody.dev", "argon2id$fake")
	require.NoError(t, err)
	a, err := s.AdminByEmail(ctx, "hody@hody.dev")
	require.NoError(t, err)
	require.Equal(t, id, a.ID)
	require.Equal(t, "owner", a.Role)
	_, err = s.AdminByEmail(ctx, "nobody@x")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.CreateAdmin(ctx, "hody@hody.dev", "x")
	require.Error(t, err, "email must be unique")
}

func TestGroupChainAndAgents(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	tid, err := s.CreateTarget(ctx, &store.Target{Name: "b2", Kind: "b2", Bucket: "hody-backups", SealedAdminKey: []byte("sealed")})
	require.NoError(t, err)
	tpl, err := s.CreateTemplate(ctx, &store.Template{Name: "Home default", Sources: []string{"~"}, PolicyJSON: json.RawMessage(`{"retention":{"keepHourly":24}}`)})
	require.NoError(t, err)
	gid, err := s.CreateGroup(ctx, &store.Group{Name: "Laptops", TargetID: tid, TemplateID: tpl})
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: "ag_1", Name: "hody-fw13", Hostname: "fw13", OS: "linux", Arch: "amd64", Scope: "user", GroupID: gid, BearerHash: []byte("h"), SealedBundle: []byte("b"), EnrolledAt: now}))
	a, err := s.AgentByBearerHash(ctx, []byte("h"))
	require.NoError(t, err)
	require.Equal(t, "ag_1", a.ID)
	require.Nil(t, a.LastSeenAt)
	require.NoError(t, s.TouchAgent(ctx, "ag_1", now, "0.1.0", "etag1"))
	a, _ = s.Agent(ctx, "ag_1")
	require.Equal(t, now, a.LastSeenAt.UTC())
	require.Equal(t, "etag1", a.PolicyETag)

	got, err := s.Template(ctx, tpl)
	require.NoError(t, err)
	require.Equal(t, []string{"~"}, got.Sources)
	require.JSONEq(t, `{"retention":{"keepHourly":24}}`, string(got.PolicyJSON))
}

func TestReportsDedupeAndLatest(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	r := &store.Report{AgentID: "ag_1", TaskID: "t1", Kind: "snapshot", Source: "/home/hody", StartedAt: now.Add(-time.Minute), FinishedAt: now, Status: "ok", Bytes: 10, Files: 2, SnapshotID: "k1"}
	id1, err := s.AddReport(ctx, r)
	require.NoError(t, err)
	id2, err := s.AddReport(ctx, r)
	require.NoError(t, err)
	require.Equal(t, id1, id2, "same (agent, task) must not duplicate")
	_, err = s.AddReport(ctx, &store.Report{AgentID: "ag_1", TaskID: "t2", Kind: "snapshot", Source: "/home/hody", StartedAt: now, FinishedAt: now.Add(time.Minute), Status: "error", Stderr: "kopia: error: boom"})
	require.NoError(t, err)
	latest, err := s.LatestReports(ctx)
	require.NoError(t, err)
	require.Equal(t, "t2", latest["ag_1"].TaskID)
	rs, err := s.ReportsForAgent(ctx, "ag_1", 10)
	require.NoError(t, err)
	require.Len(t, rs, 2)
	require.Equal(t, "t2", rs[0].TaskID)
}

func TestTokensAndCommandsAndSettings(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	id, err := s.CreateToken(ctx, &store.Token{Hash: []byte("th"), GroupID: 1, ExpiresAt: exp, MaxUses: 1})
	require.NoError(t, err)
	tok, err := s.TokenByHash(ctx, []byte("th"))
	require.NoError(t, err)
	require.Equal(t, id, tok.ID)
	require.NoError(t, s.IncrementTokenUses(ctx, id))
	tok, _ = s.TokenByHash(ctx, []byte("th"))
	require.Equal(t, 1, tok.Uses)

	cid, err := s.AddCommand(ctx, &store.Command{AgentID: "ag_1", Kind: "snapshot-now", Source: "/home/hody"})
	require.NoError(t, err)
	pend, err := s.PendingCommands(ctx, "ag_1")
	require.NoError(t, err)
	require.Len(t, pend, 1)
	require.NoError(t, s.AckCommand(ctx, cid, time.Now()))
	pend, _ = s.PendingCommands(ctx, "ag_1")
	require.Empty(t, pend)

	v, err := s.Setting(ctx, "poll_interval")
	require.NoError(t, err)
	require.Equal(t, "", v)
	require.NoError(t, s.SetSetting(ctx, "poll_interval", "300"))
	v, _ = s.Setting(ctx, "poll_interval")
	require.Equal(t, "300", v)
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./fleet/store/ -run . -v`
Expected: FAIL, `package github.com/kopia/kopia/fleet/store` not found.

- [ ] **Step 4: Write the schema**

`fleet/store/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS admins (
  id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE, pw_hash TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'owner', created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS targets (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL, bucket TEXT NOT NULL DEFAULT '',
  region TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '', sealed_admin_key BLOB,
  object_lock_verified_at TEXT, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS policy_templates (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, sources TEXT NOT NULL, policy_json TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS groups (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, target_id INTEGER NOT NULL REFERENCES targets(id),
  template_id INTEGER NOT NULL REFERENCES policy_templates(id), created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS agents (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, hostname TEXT NOT NULL, os TEXT NOT NULL, arch TEXT NOT NULL,
  version TEXT NOT NULL DEFAULT '', scope TEXT NOT NULL, group_id INTEGER NOT NULL REFERENCES groups(id),
  bearer_hash BLOB NOT NULL UNIQUE, sealed_bundle BLOB NOT NULL, policy_etag TEXT NOT NULL DEFAULT '',
  enrolled_at TEXT NOT NULL, last_seen_at TEXT, revoked_at TEXT);
CREATE TABLE IF NOT EXISTS enrollment_tokens (
  id INTEGER PRIMARY KEY, token_hash BLOB NOT NULL UNIQUE, group_id INTEGER NOT NULL REFERENCES groups(id),
  expires_at TEXT NOT NULL, max_uses INTEGER NOT NULL DEFAULT 1, uses INTEGER NOT NULL DEFAULT 0,
  revoked_at TEXT, created_by INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS reports (
  id INTEGER PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id), task_id TEXT NOT NULL, kind TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '', started_at TEXT NOT NULL, finished_at TEXT NOT NULL, status TEXT NOT NULL,
  bytes INTEGER NOT NULL DEFAULT 0, files INTEGER NOT NULL DEFAULT 0, snapshot_id TEXT NOT NULL DEFAULT '',
  stderr TEXT NOT NULL DEFAULT '', UNIQUE(agent_id, task_id));
CREATE INDEX IF NOT EXISTS reports_agent_finished ON reports(agent_id, finished_at DESC);
CREATE TABLE IF NOT EXISTS commands (
  id INTEGER PRIMARY KEY, agent_id TEXT NOT NULL REFERENCES agents(id), kind TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, acked_at TEXT);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
```

- [ ] **Step 5: Write store.go and paths.go**

`fleet/paths.go`:

```go
// Package fleet holds shared paths for the Fleet control plane.
package fleet

import "path/filepath"

// StateDirFor returns the Fleet state directory next to the repository config file.
func StateDirFor(configFile string) string { return filepath.Join(filepath.Dir(configFile), "fleet") }

// Paths are the files inside a state directory.
type Paths struct{ StateDir, DB, KeyFile string }

// PathsFor derives Paths from a state directory.
func PathsFor(stateDir string) Paths {
	return Paths{StateDir: stateDir, DB: filepath.Join(stateDir, "fleet.db"), KeyFile: filepath.Join(stateDir, "seal.key")}
}
```

`fleet/store/store.go`:

```go
// Package store is the SQLite state of the Fleet control plane.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"
)

//go:embed schema.sql
var schema string

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the database.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // ponytail: single writer; raise with a read pool if the dashboard ever contends
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func tsp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func parseTS(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }

func parseTSP(ns sql.NullString) *time.Time {
	if !ns.Valid {
		return nil
	}
	t := parseTS(ns.String)
	return &t
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) exec(ctx context.Context, q string, args ...any) (int64, error) {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
```

- [ ] **Step 6: Write the per-table files**

`fleet/store/admins.go`:

```go
package store

import (
	"context"
	"time"
)

type Admin struct {
	ID        int64
	Email     string
	PWHash    string
	Role      string
	CreatedAt time.Time
}

func (s *Store) CreateAdmin(ctx context.Context, email, pwHash string) (int64, error) {
	return s.exec(ctx, `INSERT INTO admins(email,pw_hash,role,created_at) VALUES(?,?,'owner',?)`, email, pwHash, ts(time.Now()))
}

const adminCols = `id,email,pw_hash,role,created_at`

func scanAdmin(row interface{ Scan(...any) error }) (*Admin, error) {
	var a Admin
	var c string
	if err := row.Scan(&a.ID, &a.Email, &a.PWHash, &a.Role, &c); err != nil {
		return nil, notFound(err)
	}
	a.CreatedAt = parseTS(c)
	return &a, nil
}

func (s *Store) AdminByEmail(ctx context.Context, email string) (*Admin, error) {
	return scanAdmin(s.db.QueryRowContext(ctx, `SELECT `+adminCols+` FROM admins WHERE email=?`, email))
}

func (s *Store) AdminByID(ctx context.Context, id int64) (*Admin, error) {
	return scanAdmin(s.db.QueryRowContext(ctx, `SELECT `+adminCols+` FROM admins WHERE id=?`, id))
}

func (s *Store) Admins(ctx context.Context) ([]Admin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+adminCols+` FROM admins ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Admin
	for rows.Next() {
		a, err := scanAdmin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}
```

`fleet/store/targets.go`:

```go
package store

import (
	"context"
	"database/sql"
	"time"
)

type Target struct {
	ID                   int64
	Name, Kind           string
	Bucket, Region, Path string
	SealedAdminKey       []byte
	ObjectLockVerifiedAt *time.Time
	CreatedAt            time.Time
}

const targetCols = `id,name,kind,bucket,region,path,sealed_admin_key,object_lock_verified_at,created_at`

func (s *Store) CreateTarget(ctx context.Context, t *Target) (int64, error) {
	return s.exec(ctx, `INSERT INTO targets(name,kind,bucket,region,path,sealed_admin_key,object_lock_verified_at,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		t.Name, t.Kind, t.Bucket, t.Region, t.Path, t.SealedAdminKey, tsp(t.ObjectLockVerifiedAt), ts(time.Now()))
}

func scanTarget(row interface{ Scan(...any) error }) (*Target, error) {
	var t Target
	var olv sql.NullString
	var c string
	if err := row.Scan(&t.ID, &t.Name, &t.Kind, &t.Bucket, &t.Region, &t.Path, &t.SealedAdminKey, &olv, &c); err != nil {
		return nil, notFound(err)
	}
	t.ObjectLockVerifiedAt = parseTSP(olv)
	t.CreatedAt = parseTS(c)
	return &t, nil
}

func (s *Store) Target(ctx context.Context, id int64) (*Target, error) {
	return scanTarget(s.db.QueryRowContext(ctx, `SELECT `+targetCols+` FROM targets WHERE id=?`, id))
}

func (s *Store) Targets(ctx context.Context) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+targetCols+` FROM targets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *Store) SetTargetObjectLockVerified(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE targets SET object_lock_verified_at=? WHERE id=?`, ts(at), id)
	return err
}
```

`fleet/store/templates.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"time"
)

type Template struct {
	ID         int64
	Name       string
	Sources    []string
	PolicyJSON json.RawMessage
	CreatedAt  time.Time
}

const templateCols = `id,name,sources,policy_json,created_at`

func (s *Store) CreateTemplate(ctx context.Context, t *Template) (int64, error) {
	src, _ := json.Marshal(t.Sources)
	return s.exec(ctx, `INSERT INTO policy_templates(name,sources,policy_json,created_at) VALUES(?,?,?,?)`, t.Name, string(src), string(t.PolicyJSON), ts(time.Now()))
}

func (s *Store) UpdateTemplate(ctx context.Context, t *Template) error {
	src, _ := json.Marshal(t.Sources)
	_, err := s.db.ExecContext(ctx, `UPDATE policy_templates SET name=?,sources=?,policy_json=? WHERE id=?`, t.Name, string(src), string(t.PolicyJSON), t.ID)
	return err
}

func scanTemplate(row interface{ Scan(...any) error }) (*Template, error) {
	var t Template
	var src, pol, c string
	if err := row.Scan(&t.ID, &t.Name, &src, &pol, &c); err != nil {
		return nil, notFound(err)
	}
	_ = json.Unmarshal([]byte(src), &t.Sources)
	t.PolicyJSON = json.RawMessage(pol)
	t.CreatedAt = parseTS(c)
	return &t, nil
}

func (s *Store) Template(ctx context.Context, id int64) (*Template, error) {
	return scanTemplate(s.db.QueryRowContext(ctx, `SELECT `+templateCols+` FROM policy_templates WHERE id=?`, id))
}

func (s *Store) Templates(ctx context.Context) ([]Template, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+templateCols+` FROM policy_templates ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}
```

`fleet/store/groups.go`:

```go
package store

import (
	"context"
	"time"
)

type Group struct {
	ID                   int64
	Name                 string
	TargetID, TemplateID int64
	CreatedAt            time.Time
}

func (s *Store) CreateGroup(ctx context.Context, g *Group) (int64, error) {
	return s.exec(ctx, `INSERT INTO groups(name,target_id,template_id,created_at) VALUES(?,?,?,?)`, g.Name, g.TargetID, g.TemplateID, ts(time.Now()))
}

func scanGroup(row interface{ Scan(...any) error }) (*Group, error) {
	var g Group
	var c string
	if err := row.Scan(&g.ID, &g.Name, &g.TargetID, &g.TemplateID, &c); err != nil {
		return nil, notFound(err)
	}
	g.CreatedAt = parseTS(c)
	return &g, nil
}

func (s *Store) Group(ctx context.Context, id int64) (*Group, error) {
	return scanGroup(s.db.QueryRowContext(ctx, `SELECT id,name,target_id,template_id,created_at FROM groups WHERE id=?`, id))
}

func (s *Store) Groups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,target_id,template_id,created_at FROM groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}
```

`fleet/store/agents.go`:

```go
package store

import (
	"context"
	"database/sql"
	"time"
)

type Agent struct {
	ID, Name, Hostname, OS, Arch, Version, Scope string
	GroupID                                      int64
	BearerHash, SealedBundle                     []byte
	PolicyETag                                   string
	EnrolledAt                                   time.Time
	LastSeenAt, RevokedAt                        *time.Time
}

const agentCols = `id,name,hostname,os,arch,version,scope,group_id,bearer_hash,sealed_bundle,policy_etag,enrolled_at,last_seen_at,revoked_at`

func (s *Store) CreateAgent(ctx context.Context, a *Agent) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agents(`+agentCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Hostname, a.OS, a.Arch, a.Version, a.Scope, a.GroupID, a.BearerHash, a.SealedBundle, a.PolicyETag, ts(a.EnrolledAt), tsp(a.LastSeenAt), tsp(a.RevokedAt))
	return err
}

func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var enrolled string
	var seen, revoked sql.NullString
	if err := row.Scan(&a.ID, &a.Name, &a.Hostname, &a.OS, &a.Arch, &a.Version, &a.Scope, &a.GroupID, &a.BearerHash, &a.SealedBundle, &a.PolicyETag, &enrolled, &seen, &revoked); err != nil {
		return nil, notFound(err)
	}
	a.EnrolledAt = parseTS(enrolled)
	a.LastSeenAt = parseTSP(seen)
	a.RevokedAt = parseTSP(revoked)
	return &a, nil
}

func (s *Store) Agent(ctx context.Context, id string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, id))
}

func (s *Store) AgentByBearerHash(ctx context.Context, h []byte) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE bearer_hash=?`, h))
}

func (s *Store) Agents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentCols+` FROM agents ORDER BY enrolled_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *Store) TouchAgent(ctx context.Context, id string, at time.Time, version, etag string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET last_seen_at=?,version=?,policy_etag=? WHERE id=?`, ts(at), version, etag, id)
	return err
}

func (s *Store) RevokeAgent(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET revoked_at=? WHERE id=?`, ts(at), id)
	return err
}
```

`fleet/store/tokens.go`:

```go
package store

import (
	"context"
	"database/sql"
	"time"
)

type Token struct {
	ID            int64
	Hash          []byte
	GroupID       int64
	ExpiresAt     time.Time
	MaxUses, Uses int
	RevokedAt     *time.Time
	CreatedBy     int64
}

const tokenCols = `id,token_hash,group_id,expires_at,max_uses,uses,revoked_at,created_by`

func (s *Store) CreateToken(ctx context.Context, t *Token) (int64, error) {
	return s.exec(ctx, `INSERT INTO enrollment_tokens(token_hash,group_id,expires_at,max_uses,uses,revoked_at,created_by,created_at) VALUES(?,?,?,?,0,NULL,?,?)`,
		t.Hash, t.GroupID, ts(t.ExpiresAt), t.MaxUses, t.CreatedBy, ts(time.Now()))
}

func scanToken(row interface{ Scan(...any) error }) (*Token, error) {
	var t Token
	var exp string
	var rev sql.NullString
	if err := row.Scan(&t.ID, &t.Hash, &t.GroupID, &exp, &t.MaxUses, &t.Uses, &rev, &t.CreatedBy); err != nil {
		return nil, notFound(err)
	}
	t.ExpiresAt = parseTS(exp)
	t.RevokedAt = parseTSP(rev)
	return &t, nil
}

func (s *Store) TokenByHash(ctx context.Context, h []byte) (*Token, error) {
	return scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM enrollment_tokens WHERE token_hash=?`, h))
}

func (s *Store) IncrementTokenUses(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE enrollment_tokens SET uses=uses+1 WHERE id=?`, id)
	return err
}

func (s *Store) RevokeToken(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE enrollment_tokens SET revoked_at=? WHERE id=?`, ts(at), id)
	return err
}

func (s *Store) TokensForGroup(ctx context.Context, groupID int64) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenCols+` FROM enrollment_tokens WHERE group_id=? ORDER BY id DESC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}
```

`fleet/store/reports.go`:

```go
package store

import (
	"context"
	"time"
)

type Report struct {
	ID                            int64
	AgentID, TaskID, Kind, Source string
	StartedAt, FinishedAt         time.Time
	Status                        string
	Bytes, Files                  int64
	SnapshotID, Stderr            string
}

const reportCols = `id,agent_id,task_id,kind,source,started_at,finished_at,status,bytes,files,snapshot_id,stderr`

// AddReport inserts a report; a duplicate (agent_id, task_id) returns the existing id.
func (s *Store) AddReport(ctx context.Context, r *Report) (int64, error) {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO reports(agent_id,task_id,kind,source,started_at,finished_at,status,bytes,files,snapshot_id,stderr) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		r.AgentID, r.TaskID, r.Kind, r.Source, ts(r.StartedAt), ts(r.FinishedAt), r.Status, r.Bytes, r.Files, r.SnapshotID, r.Stderr)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM reports WHERE agent_id=? AND task_id=?`, r.AgentID, r.TaskID).Scan(&id)
	return id, err
}

func scanReport(row interface{ Scan(...any) error }) (*Report, error) {
	var r Report
	var st, fi string
	if err := row.Scan(&r.ID, &r.AgentID, &r.TaskID, &r.Kind, &r.Source, &st, &fi, &r.Status, &r.Bytes, &r.Files, &r.SnapshotID, &r.Stderr); err != nil {
		return nil, notFound(err)
	}
	r.StartedAt, r.FinishedAt = parseTS(st), parseTS(fi)
	return &r, nil
}

func (s *Store) ReportsForAgent(ctx context.Context, agentID string, limit int) ([]Report, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+reportCols+` FROM reports WHERE agent_id=? ORDER BY finished_at DESC, id DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Report
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// LatestReports returns the newest report per agent.
func (s *Store) LatestReports(ctx context.Context) (map[string]Report, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+reportCols+` FROM reports r WHERE id = (SELECT id FROM reports WHERE agent_id=r.agent_id ORDER BY finished_at DESC, id DESC LIMIT 1)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Report{}
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out[r.AgentID] = *r
	}
	return out, rows.Err()
}
```

`fleet/store/commands.go`:

```go
package store

import (
	"context"
	"database/sql"
	"time"
)

type Command struct {
	ID                    int64
	AgentID, Kind, Source string
	CreatedAt             time.Time
	AckedAt               *time.Time
}

func (s *Store) AddCommand(ctx context.Context, c *Command) (int64, error) {
	return s.exec(ctx, `INSERT INTO commands(agent_id,kind,source,created_at) VALUES(?,?,?,?)`, c.AgentID, c.Kind, c.Source, ts(time.Now()))
}

func (s *Store) PendingCommands(ctx context.Context, agentID string) ([]Command, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,agent_id,kind,source,created_at,acked_at FROM commands WHERE agent_id=? AND acked_at IS NULL ORDER BY id`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		var c Command
		var created string
		var acked sql.NullString
		if err := rows.Scan(&c.ID, &c.AgentID, &c.Kind, &c.Source, &created, &acked); err != nil {
			return nil, err
		}
		c.CreatedAt, c.AckedAt = parseTS(created), parseTSP(acked)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AckCommand(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE commands SET acked_at=? WHERE id=?`, ts(at), id)
	return err
}
```

`fleet/store/settings.go`:

```go
package store

import (
	"context"
	"database/sql"
	"errors"
)

func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
```

- [ ] **Step 7: Run the tests**

Run: `go test ./fleet/... -v`
Expected: all five tests PASS.

- [ ] **Step 8: Commit**

```bash
git add go.mod go.sum fleet && git commit -m "feat(fleet): SQLite state store"
```

---

### Task 4: Secret sealing (`fleet/seal`)

**Files:**
- Create: `fleet/seal/seal.go`
- Test: `fleet/seal/seal_test.go`

**Interfaces:**
- Produces:

```go
package seal
type Key [32]byte
func NewSalt() ([]byte, error)                              // 16 random bytes
func Derive(passphrase string, salt []byte) Key             // argon2.IDKey(pass, salt, 3, 64*1024, 4, 32)
func (k Key) Seal(plain []byte) ([]byte, error)             // 24-byte nonce || secretbox
func (k Key) Open(sealed []byte) ([]byte, error)            // ErrTampered on failure
func WriteKeyFile(path string, k Key) error                 // hex, mode 0600, dir 0700
func ReadKeyFile(path string) (Key, error)
var ErrTampered = errors.New("sealed data is corrupt or the key is wrong")
```

- [ ] **Step 1: Write the failing test**

`fleet/seal/seal_test.go`:

```go
package seal_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/seal"
)

func TestDeriveIsDeterministicAndSaltSensitive(t *testing.T) {
	salt, err := seal.NewSalt()
	require.NoError(t, err)
	require.Len(t, salt, 16)
	k1 := seal.Derive("correct horse", salt)
	k2 := seal.Derive("correct horse", salt)
	require.Equal(t, k1, k2)
	salt2, _ := seal.NewSalt()
	require.NotEqual(t, k1, seal.Derive("correct horse", salt2))
}

func TestSealOpenRoundTripAndTamper(t *testing.T) {
	salt, _ := seal.NewSalt()
	k := seal.Derive("pw", salt)
	sealed, err := k.Seal([]byte("repo-password-32-bytes"))
	require.NoError(t, err)
	plain, err := k.Open(sealed)
	require.NoError(t, err)
	require.Equal(t, "repo-password-32-bytes", string(plain))
	sealed[len(sealed)-1] ^= 0xff
	_, err = k.Open(sealed)
	require.ErrorIs(t, err, seal.ErrTampered)
	_, err = seal.Derive("other", salt).Open(sealed[:len(sealed)-1])
	require.Error(t, err)
}

func TestKeyFileIs0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "seal.key")
	salt, _ := seal.NewSalt()
	k := seal.Derive("pw", salt)
	require.NoError(t, seal.WriteKeyFile(p, k))
	st, err := os.Stat(p)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	got, err := seal.ReadKeyFile(p)
	require.NoError(t, err)
	require.Equal(t, k, got)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./fleet/seal/ -v`
Expected: FAIL, package not found.

- [ ] **Step 3: Implement**

`fleet/seal/seal.go`:

```go
// Package seal derives a key from the admin passphrase and seals secrets at rest.
package seal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/nacl/secretbox"
)

// Key is a 256-bit sealing key.
type Key [32]byte

// ErrTampered is returned when sealed data does not authenticate.
var ErrTampered = errors.New("sealed data is corrupt or the key is wrong")

const nonceSize = 24

// NewSalt returns 16 random bytes.
func NewSalt() ([]byte, error) {
	b := make([]byte, 16)
	_, err := io.ReadFull(rand.Reader, b)
	return b, err
}

// Derive derives the key from a passphrase with argon2id (64 MiB, 3 passes, 4 lanes).
func Derive(passphrase string, salt []byte) Key {
	var k Key
	copy(k[:], argon2.IDKey([]byte(passphrase), salt, 3, 64*1024, 4, 32))
	return k
}

// Seal encrypts plain with a fresh nonce: nonce || box.
func (k Key) Seal(plain []byte) ([]byte, error) {
	var nonce [nonceSize]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, err
	}
	key := [32]byte(k)
	return secretbox.Seal(nonce[:], plain, &nonce, &key), nil
}

// Open decrypts data produced by Seal.
func (k Key) Open(sealed []byte) ([]byte, error) {
	if len(sealed) < nonceSize+secretbox.Overhead {
		return nil, ErrTampered
	}
	var nonce [nonceSize]byte
	copy(nonce[:], sealed[:nonceSize])
	key := [32]byte(k)
	out, ok := secretbox.Open(nil, sealed[nonceSize:], &nonce, &key)
	if !ok {
		return nil, ErrTampered
	}
	return out, nil
}

// WriteKeyFile writes the key hex-encoded with mode 0600.
func WriteKeyFile(path string, k Key) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(hex.EncodeToString(k[:])+"\n"), 0o600)
}

// ReadKeyFile reads a key written by WriteKeyFile.
func ReadKeyFile(path string) (Key, error) {
	var k Key
	b, err := os.ReadFile(path)
	if err != nil {
		return k, err
	}
	raw, err := hex.DecodeString(string(trimNL(b)))
	if err != nil || len(raw) != len(k) {
		return k, errors.New("seal key file is malformed")
	}
	copy(k[:], raw)
	return k, nil
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./fleet/seal/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fleet/seal && git commit -m "feat(fleet): argon2id + secretbox sealing"
```

---

### Task 5: Admin auth: passwords, sessions, rate limit (`fleet/api/auth.go`)

**Files:**
- Create: `fleet/api/auth.go`
- Test: `fleet/api/auth_test.go`

**Interfaces:**
- Produces:

```go
package api
func HashPassword(pw string) (string, error)                 // "$argon2id$v=19$m=65536,t=3,p=4$<salt b64>$<hash b64>"
func VerifyPassword(pw, encoded string) bool                 // constant-time
type sessions struct{ secret []byte; ttl time.Duration; now func() time.Time }
func newSessions(secret []byte, ttl time.Duration) *sessions
func (s *sessions) issue(adminID int64) string               // "<adminID>.<expiryUnix>.<hex hmac>"
func (s *sessions) verify(tok string) (adminID int64, ok bool)
const sessionCookie = "wh_session"
type limiter struct{...}                                     // per-key, 5 attempts per minute
func newLimiter(max int, per time.Duration) *limiter
func (l *limiter) allow(key string) bool
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc   // 401 JSON {"error":"unauthorized"} when no valid cookie
```

- [ ] **Step 1: Write the failing test**

`fleet/api/auth_test.go`:

```go
package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPasswordHashVerify(t *testing.T) {
	h, err := HashPassword("s3cret")
	require.NoError(t, err)
	require.Contains(t, h, "$argon2id$")
	require.True(t, VerifyPassword("s3cret", h))
	require.False(t, VerifyPassword("wrong", h))
	require.False(t, VerifyPassword("s3cret", "garbage"))
}

func TestSessionsIssueVerifyExpire(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := newSessions([]byte("secret"), time.Hour)
	s.now = func() time.Time { return now }
	tok := s.issue(42)
	id, ok := s.verify(tok)
	require.True(t, ok)
	require.EqualValues(t, 42, id)
	_, ok = s.verify(tok + "x")
	require.False(t, ok)
	s.now = func() time.Time { return now.Add(2 * time.Hour) }
	_, ok = s.verify(tok)
	require.False(t, ok, "expired")
	other := newSessions([]byte("other"), time.Hour)
	_, ok = other.verify(tok)
	require.False(t, ok, "different secret")
}

func TestLimiter(t *testing.T) {
	l := newLimiter(3, time.Minute)
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		require.True(t, l.allow("1.2.3.4"))
	}
	require.False(t, l.allow("1.2.3.4"))
	require.True(t, l.allow("5.6.7.8"))
	now = now.Add(61 * time.Second)
	require.True(t, l.allow("1.2.3.4"))
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./fleet/api/ -run 'TestPassword|TestSessions|TestLimiter' -v`
Expected: FAIL, undefined symbols.

- [ ] **Step 3: Implement**

`fleet/api/auth.go`:

```go
package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

const sessionCookie = "wh_session"

// HashPassword hashes with argon2id in PHC string format.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}
	h := argon2.IDKey([]byte(pw), salt, 3, 64*1024, 4, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=4$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(h)), nil
}

// VerifyPassword checks pw against a HashPassword string in constant time.
func VerifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, 3, 64*1024, 4, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

type sessions struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

func newSessions(secret []byte, ttl time.Duration) *sessions {
	return &sessions{secret: secret, ttl: ttl, now: time.Now}
}

func (s *sessions) sign(body string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *sessions) issue(adminID int64) string {
	body := strconv.FormatInt(adminID, 10) + "." + strconv.FormatInt(s.now().Add(s.ttl).Unix(), 10)
	return body + "." + s.sign(body)
}

func (s *sessions) verify(tok string) (int64, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return 0, false
	}
	body := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(s.sign(body)), []byte(parts[2])) {
		return 0, false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || s.now().Unix() > exp {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	return id, err == nil
}

type limiter struct {
	mu   sync.Mutex
	max  int
	per  time.Duration
	hits map[string][]time.Time
	now  func() time.Time
}

func newLimiter(max int, per time.Duration) *limiter {
	return &limiter{max: max, per: per, hits: map[string][]time.Time{}, now: time.Now}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var keep []time.Time
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.per {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.max {
		l.hits[key] = keep
		return false
	}
	l.hits[key] = append(keep, now)
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// requireAdmin gates a handler behind a valid session cookie.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if _, ok := s.sess.verify(c.Value); !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}
```

Also create a minimal `fleet/api/server.go` so the package compiles (Task 6 fills it in):

```go
// Package api serves the Fleet control-plane HTTP API.
package api

// Server holds Fleet state for the HTTP handlers.
type Server struct {
	sess *sessions
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./fleet/api/ -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add fleet/api && git commit -m "feat(fleet): admin password hashing, sessions, login limiter"
```

---

### Task 6: Activation, status, login; mount hook; `fleet activate` CLI

**Files:**
- Create: `fleet/api/server.go` (replace the stub), `cli/server_hooks.go`, `cli/command_fleet.go`, `cli/command_fleet_activate.go`
- Modify: `cli/command_server_start.go:320-333` (`setupHandlers`), `cli/app.go:158-160` and `:315-318`
- Test: `fleet/api/server_test.go`

**Interfaces:**
- Produces:

```go
package api
type Server struct { paths fleet.Paths; st *store.Store; key seal.Key; sess *sessions; login *limiter; now func() time.Time; b2 b2api.API /* Task 8 */ }
func New(stateDir string) *Server                        // never fails; lazily loads if activated
func (s *Server) Activated() bool
func (s *Server) Activate(ctx context.Context, passphrase, email, password string) error   // ErrAlreadyActivated
func (s *Server) Mount(m *mux.Router)                    // registers every /api/v1/fleet/* route + /enroll.sh
func (s *Server) Close() error
var ErrAlreadyActivated = errors.New("fleet is already activated")

// routes (M1):
// GET  /api/v1/fleet/status                  → {"activated":bool}
// POST /api/v1/fleet/activate                {passphrase,email,password} → 201 {"admin_id":n} | 409
// POST /api/v1/fleet/session                 {email,password} → 204 + cookie | 401 | 429
// DELETE /api/v1/fleet/session               → 204, clears cookie

package cli
func RegisterServerHandlers(f func(srv *server.Server, m *mux.Router, configFile string))
```

- [ ] **Step 1: Write the failing test**

`fleet/api/server_test.go`:

```go
package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/api"
)

type harness struct {
	t   *testing.T
	srv *httptest.Server
	jar []*http.Cookie
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s := api.New(t.TempDir())
	t.Cleanup(func() { s.Close() })
	m := mux.NewRouter()
	s.Mount(m)
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	return &harness{t: t, srv: ts}
}

func (h *harness) do(method, path string, body any) (*http.Response, map[string]any) {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(h.t, json.NewEncoder(&buf).Encode(body))
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	for _, c := range h.jar {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	if cs := resp.Cookies(); len(cs) > 0 {
		h.jar = cs
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (h *harness) activateAndLogin() {
	h.t.Helper()
	resp, _ := h.do("POST", "/api/v1/fleet/activate", map[string]string{"passphrase": "seal-me", "email": "hody@hody.dev", "password": "pw12345678"})
	require.Equal(h.t, 201, resp.StatusCode)
	resp, _ = h.do("POST", "/api/v1/fleet/session", map[string]string{"email": "hody@hody.dev", "password": "pw12345678"})
	require.Equal(h.t, 204, resp.StatusCode)
}

func TestStatusActivateLogin(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("GET", "/api/v1/fleet/status", nil)
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, false, body["activated"])

	resp, _ = h.do("POST", "/api/v1/fleet/session", map[string]string{"email": "x", "password": "y"})
	require.Equal(t, 409, resp.StatusCode, "cannot log in before activation")

	h.activateAndLogin()
	resp, body = h.do("GET", "/api/v1/fleet/status", nil)
	require.Equal(t, true, body["activated"])

	resp, _ = h.do("POST", "/api/v1/fleet/activate", map[string]string{"passphrase": "again", "email": "a@b", "password": "pw12345678"})
	require.Equal(t, 409, resp.StatusCode)

	resp, _ = h.do("DELETE", "/api/v1/fleet/session", nil)
	require.Equal(t, 204, resp.StatusCode)
}

func TestLoginRateLimitAndBadPassword(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.jar = nil
	for i := 0; i < 5; i++ {
		resp, _ := h.do("POST", "/api/v1/fleet/session", map[string]string{"email": "hody@hody.dev", "password": "wrong"})
		require.Equal(t, 401, resp.StatusCode)
	}
	resp, _ := h.do("POST", "/api/v1/fleet/session", map[string]string{"email": "hody@hody.dev", "password": "pw12345678"})
	require.Equal(t, 429, resp.StatusCode)
}

func TestActivationSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := api.New(dir)
	m := mux.NewRouter()
	s.Mount(m)
	ts := httptest.NewServer(m)
	h := &harness{t: t, srv: ts}
	h.activateAndLogin()
	ts.Close()
	require.NoError(t, s.Close())

	s2 := api.New(dir)
	defer s2.Close()
	require.True(t, s2.Activated(), "key file + db must reopen without the passphrase")
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./fleet/api/ -run 'TestStatus|TestLogin|TestActivation' -v`
Expected: FAIL, `api.New` undefined.

- [ ] **Step 3: Implement server.go**

`fleet/api/server.go`:

```go
// Package api serves the Fleet control-plane HTTP API.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/b2api"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
)

// ErrAlreadyActivated is returned by Activate on an activated Fleet.
var ErrAlreadyActivated = errors.New("fleet is already activated")

const (
	sessionTTL       = 12 * time.Hour
	loginMaxAttempts = 5
	loginWindow      = time.Minute
	maxBody          = 1 << 20
)

// Server holds Fleet state for the HTTP handlers.
type Server struct {
	mu    sync.RWMutex
	paths fleet.Paths
	st    *store.Store
	key   seal.Key
	sess  *sessions
	login *limiter
	now   func() time.Time
	b2    b2api.API
}

// New creates a Server for stateDir; if Fleet was activated before, its state is loaded.
func New(stateDir string) *Server {
	s := &Server{paths: fleet.PathsFor(stateDir), login: newLimiter(loginMaxAttempts, loginWindow), now: time.Now, b2: b2api.New(nil)}
	_ = s.load()
	return s
}

func (s *Server) load() error {
	key, err := seal.ReadKeyFile(s.paths.KeyFile)
	if err != nil {
		return err
	}
	st, err := store.Open(s.paths.DB)
	if err != nil {
		return err
	}
	secret, err := st.Setting(context.Background(), "session_secret")
	if err != nil || secret == "" {
		st.Close()
		return errors.New("session secret missing")
	}
	s.mu.Lock()
	s.key, s.st, s.sess = key, st, newSessions([]byte(secret), sessionTTL)
	s.mu.Unlock()
	return nil
}

// Activated reports whether the store and key are loaded.
func (s *Server) Activated() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st != nil
}

// Close closes the store.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil {
		return nil
	}
	err := s.st.Close()
	s.st = nil
	return err
}

// Activate creates the DB, seals with a key derived from passphrase, and creates the first admin.
func (s *Server) Activate(ctx context.Context, passphrase, email, password string) error {
	if s.Activated() {
		return ErrAlreadyActivated
	}
	if len(passphrase) < 8 || len(password) < 8 || !strings.Contains(email, "@") {
		return errors.New("passphrase and password need 8+ characters and email must be valid")
	}
	salt, err := seal.NewSalt()
	if err != nil {
		return err
	}
	key := seal.Derive(passphrase, salt)
	if err := seal.WriteKeyFile(s.paths.KeyFile, key); err != nil {
		return err
	}
	st, err := store.Open(s.paths.DB)
	if err != nil {
		return err
	}
	pwHash, err := HashPassword(password)
	if err != nil {
		return err
	}
	secret := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return err
	}
	if err := errors.Join(
		st.SetSetting(ctx, "seal_salt", hex.EncodeToString(salt)),
		st.SetSetting(ctx, "session_secret", hex.EncodeToString(secret)),
	); err != nil {
		return err
	}
	if _, err := st.CreateAdmin(ctx, email, pwHash); err != nil {
		return err
	}
	st.Close()
	return s.load()
}

// Mount registers all Fleet routes.
func (s *Server) Mount(m *mux.Router) {
	m.HandleFunc("/api/v1/fleet/status", s.handleStatus).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/activate", s.handleActivate).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/session", s.handleLogin).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/session", s.handleLogout).Methods(http.MethodDelete)
	s.mountAdmin(m)  // Task 7
	s.mountAgent(m)  // Tasks 11, 14
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(v)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"activated": s.Activated()})
}

func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	var in struct{ Passphrase, Email, Password string }
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	if err := s.Activate(r.Context(), in.Passphrase, in.Email, in.Password); err != nil {
		if errors.Is(err, ErrAlreadyActivated) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a, _ := s.st.AdminByEmail(r.Context(), in.Email)
	writeJSON(w, http.StatusCreated, map[string]any{"admin_id": a.ID})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.Activated() {
		writeErr(w, http.StatusConflict, "fleet is not activated")
		return
	}
	if !s.login.allow(clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, wait a minute")
		return
	}
	var in struct{ Email, Password string }
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	a, err := s.st.AdminByEmail(r.Context(), in.Email)
	if err != nil || !VerifyPassword(in.Password, a.PWHash) {
		writeErr(w, http.StatusUnauthorized, "wrong email or password")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: s.sess.issue(a.ID), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: int(sessionTTL.Seconds())})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

// requireActivated wraps admin handlers so they 409 before activation.
func (s *Server) requireActivated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Activated() {
			writeErr(w, http.StatusConflict, "fleet is not activated")
			return
		}
		next(w, r)
	}
}

```

Until Tasks 7/8/11/14 exist, add temporary stubs so the package compiles (delete each when its task replaces it):

`fleet/api/admin_stub.go`:
```go
package api

import "github.com/gorilla/mux"

func (s *Server) mountAdmin(_ *mux.Router) {}
func (s *Server) mountAgent(_ *mux.Router) {}
```

`fleet/b2api/client.go` stub (Task 8 replaces it):
```go
// Package b2api is a minimal Backblaze B2 native API client.
package b2api

import "net/http"

// API is what Fleet needs from B2.
type API interface{}

// New returns the real client (Task 8).
func New(_ *http.Client) API { return nil }
```

- [ ] **Step 4: Run tests**

Run: `go test ./fleet/api/ -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Add the server hook (upstream touch #2)**

`cli/server_hooks.go` (new):

```go
package cli

import (
	"github.com/gorilla/mux"

	"github.com/kopia/kopia/internal/server"
)

// warphold: extra handler registration for the Fleet control plane.
var serverExtraHandlers []func(srv *server.Server, m *mux.Router, configFile string)

// RegisterServerHandlers adds a function that mounts routes when `server start` builds its router.
func RegisterServerHandlers(f func(srv *server.Server, m *mux.Router, configFile string)) {
	serverExtraHandlers = append(serverExtraHandlers, f)
}
```

In `cli/command_server_start.go`, `setupHandlers` becomes:

```go
func (c *commandServerStart) setupHandlers(srv *server.Server, m *mux.Router) {
	if c.serverStartControlAPI {
		srv.SetupControlAPIHandlers(m)
	}

	if c.serverStartUI {
		srv.SetupHTMLUIAPIHandlers(m)

		if c.serverStartHTMLPath != "" {
			srv.ServeStaticFiles(m, http.Dir(c.serverStartHTMLPath))
		} else {
			srv.ServeStaticFiles(m, server.AssetFile())
		}
	}

	for _, h := range serverExtraHandlers { // warphold: fleet routes
		h(srv, m, c.svc.repositoryConfigFileName())
	}
}
```

Note: the hook must run **before** `ServeStaticFiles`'s catch-all would shadow `/enroll.sh`; gorilla/mux matches routes in registration order, so move the `for` loop to the **top** of `setupHandlers` (above the control-API block). Do that.

- [ ] **Step 6: Fleet command group and `fleet activate`**

`cli/command_fleet.go`:

```go
package cli

import (
	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/api"
	"github.com/kopia/kopia/internal/server"
)

// commandFleet groups the Fleet control-plane commands.
type commandFleet struct {
	activate commandFleetActivate
}

func (c *commandFleet) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("fleet", "WarpHold Fleet: manage enrolled machines.")
	c.activate.setup(svc, cmd)

	RegisterServerHandlers(func(_ *server.Server, m *mux.Router, configFile string) {
		api.New(fleet.StateDirFor(configFile)).Mount(m)
	})
}
```

`cli/command_fleet_activate.go`:

```go
package cli

import (
	"context"
	"fmt"

	"github.com/alecthomas/kingpin/v2"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/api"
)

type commandFleetActivate struct {
	email      string
	password   string
	passphrase string
	svc        appServices
	out        textOutput
}

func (c *commandFleetActivate) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("activate", "Turn this WarpHold into a Fleet server (creates the state DB and the first admin).")
	cmd.Flag("email", "First admin email").Required().StringVar(&c.email)
	cmd.Flag("admin-password", "First admin password (8+ chars)").Envar(svc.EnvName("WARPHOLD_ADMIN_PASSWORD")).StringVar(&c.password)
	cmd.Flag("passphrase", "Sealing passphrase (8+ chars); prompted if omitted").Envar(svc.EnvName("WARPHOLD_SEAL_PASSPHRASE")).StringVar(&c.passphrase)
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandFleetActivate) run(ctx context.Context) error {
	if c.passphrase == "" {
		p, err := askPass(c.out.stdout(), "Sealing passphrase: ")
		if err != nil {
			return err
		}
		c.passphrase = p
	}
	if c.password == "" {
		p, err := askPass(c.out.stdout(), "Admin password: ")
		if err != nil {
			return err
		}
		c.password = p
	}
	s := api.New(fleet.StateDirFor(c.svc.repositoryConfigFileName()))
	defer s.Close()
	if err := s.Activate(ctx, c.passphrase, c.email, c.password); err != nil {
		return errors.Wrap(err, "activate")
	}
	fmt.Fprintln(c.out.stdout(), "Fleet is on. Start the server with 'warphold server start' and sign in at /api/v1/fleet/session.") //nolint:errcheck
	return nil
}

var _ = kingpin.Version // keep import if unused on some platforms
```

`askPass` already exists in `cli/password.go` (upstream) with signature `askPass(out io.Writer, prompt string) (string, error)`; if the name differs on current master, grep `func askPass` and use that name. `svc.repositoryConfigFileName()` is on `appServices`; if it is only on `advancedAppServices`, change the field type to `advancedAppServices`.

Register in `cli/app.go` (upstream touch #1): add fields after `notification commandNotification`:

```go
	fleet commandFleet // warphold:
	agent commandAgent // warphold:
```
and after `c.repository.setup(c, app)`:
```go
	c.fleet.setup(c, app) // warphold:
	c.agent.setup(c, app) // warphold:
```
`commandAgent` arrives in Task 12; until then add a one-line stub `cli/command_agent.go`:
```go
package cli

type commandAgent struct{}

func (c *commandAgent) setup(_ advancedAppServices, _ commandParent) {}
```

- [ ] **Step 7: Verify the wiring end to end**

```bash
CGO_ENABLED=0 go build -o dist/warphold . && \
dist/warphold --config-file /tmp/whtest/repository.config fleet activate --email hody@hody.dev --admin-password pw12345678 --passphrase seal-me-please && \
ls -la /tmp/whtest/fleet && \
(dist/warphold --config-file /tmp/whtest/repository.config server start --insecure --without-password --address 127.0.0.1:51599 --ui=false --grpc=false & sleep 3; curl -s 127.0.0.1:51599/api/v1/fleet/status; kill %1)
```
Expected: `fleet.db` and `seal.key` (mode `-rw-------`) exist; curl prints `{"activated":true}`. The server starts without a repository connected (`--without-password` needs `--insecure`); Fleet routes answer regardless.

- [ ] **Step 8: Run all tests and commit**

```bash
go test ./fleet/... ./cli/ -run 'Fleet|Status|Login|Activation' && git add cli fleet && git commit -m "feat(fleet): activation, admin login, server mount hook, fleet activate command"
```

---

### Task 7: Admin CRUD: targets, templates, groups (`fleet/api/admin_*.go`)

**Files:**
- Create: `fleet/api/admin_targets.go`, `fleet/api/admin_templates.go`, `fleet/api/admin_groups.go`, `fleet/api/admin.go`
- Delete: the `mountAdmin` line from `fleet/api/admin_stub.go`
- Test: `fleet/api/admin_test.go`

**Interfaces:**
- Routes (all `requireActivated` + `requireAdmin`):

```
POST /api/v1/fleet/targets    {name, kind:"b2"|"filesystem", bucket?, region?, path?, key_id?, key?}  → 201 {id, object_lock_verified:bool}
GET  /api/v1/fleet/targets    → [{id,name,kind,bucket,region,path,object_lock_verified_at}]   (never keys)
POST /api/v1/fleet/templates  {name, sources:[...], policy:{...kopia policy...}} → 201 {id}
PUT  /api/v1/fleet/templates/{id}  same body → 204
GET  /api/v1/fleet/templates  → [{id,name,sources,policy}]
POST /api/v1/fleet/groups     {name, target_id, template_id} → 201 {id}
GET  /api/v1/fleet/groups     → [{id,name,target_id,template_id}]
```
- Produces for later tasks: `func (s *Server) targetCreds(ctx, t *store.Target) (keyID, key string, err error)` (unseals `SealedAdminKey`, stored as JSON `{"key_id":..,"key":..}`).
- For `kind:"b2"` the handler calls `s.b2.Authorize` + `s.b2.BucketInfo` (Task 8) to verify credentials and read Object Lock; before Task 8 lands, the handler treats `s.b2 == nil` as "unverified" and sets `object_lock_verified:false`. Task 8 fills it in and adds the test.

- [ ] **Step 1: Write the failing test**

`fleet/api/admin_test.go`:

```go
package api_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminCRUDRequiresLoginAndRoundTrips(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do("GET", "/api/v1/fleet/targets", nil)
	require.Equal(t, 409, resp.StatusCode, "not activated")
	h.activateAndLogin()
	saved := h.jar
	h.jar = nil
	resp, _ = h.do("GET", "/api/v1/fleet/targets", nil)
	require.Equal(t, 401, resp.StatusCode)
	h.jar = saved

	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "local", "kind": "filesystem", "path": t.TempDir()})
	require.Equal(t, 201, resp.StatusCode)
	tid := body["id"].(float64)

	resp, body = h.do("POST", "/api/v1/fleet/templates", map[string]any{"name": "Home default", "sources": []string{"~"}, "policy": map[string]any{"retention": map[string]any{"keepHourly": 24}}})
	require.Equal(t, 201, resp.StatusCode)
	tpl := body["id"].(float64)

	resp, body = h.do("POST", "/api/v1/fleet/groups", map[string]any{"name": "Laptops", "target_id": tid, "template_id": tpl})
	require.Equal(t, 201, resp.StatusCode)

	resp, _ = h.do("POST", "/api/v1/fleet/groups", map[string]any{"name": "Bad", "target_id": 999, "template_id": tpl})
	require.Equal(t, 400, resp.StatusCode, "unknown target")

	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/fleet/templates", nil)
	for _, c := range h.jar {
		req.AddCookie(c)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	var list []map[string]any
	require.NoError(t, json.NewDecoder(res.Body).Decode(&list))
	require.Len(t, list, 1)
	require.Equal(t, "Home default", list[0]["name"])
	require.Equal(t, []any{"~"}, list[0]["sources"])
}
```
(add `"encoding/json"` and `"net/http"` to the imports of this test file.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./fleet/api/ -run TestAdminCRUD -v`
Expected: FAIL with 404s (routes missing).

- [ ] **Step 3: Implement**

`fleet/api/admin.go`:

```go
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet/store"
)

func (s *Server) mountAdmin(m *mux.Router) {
	adm := func(h http.HandlerFunc) http.HandlerFunc { return s.requireActivated(s.requireAdmin(h)) }
	m.HandleFunc("/api/v1/fleet/targets", adm(s.handleTargetCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/targets", adm(s.handleTargetList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/templates", adm(s.handleTemplateCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/templates/{id}", adm(s.handleTemplateUpdate)).Methods(http.MethodPut)
	m.HandleFunc("/api/v1/fleet/templates", adm(s.handleTemplateList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/groups", adm(s.handleGroupCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/groups", adm(s.handleGroupList)).Methods(http.MethodGet)
	s.mountAdminEnrollment(m, adm) // Task 9/11: tokens + agents
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	return id, err == nil
}

type targetCreds struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key"`
}

func (s *Server) sealCreds(c targetCreds) ([]byte, error) {
	b, _ := json.Marshal(c)
	return s.key.Seal(b)
}

// targetCreds unseals the admin credentials of a target.
func (s *Server) targetCreds(_ context.Context, t *store.Target) (string, string, error) {
	if len(t.SealedAdminKey) == 0 {
		return "", "", nil
	}
	b, err := s.key.Open(t.SealedAdminKey)
	if err != nil {
		return "", "", err
	}
	var c targetCreds
	if err := json.Unmarshal(b, &c); err != nil {
		return "", "", err
	}
	return c.KeyID, c.Key, nil
}
```

`fleet/api/admin_targets.go`:

```go
package api

import (
	"net/http"
	"os"
	"time"

	"github.com/kopia/kopia/fleet/store"
)

type targetOut struct {
	ID                   int64      `json:"id"`
	Name                 string     `json:"name"`
	Kind                 string     `json:"kind"`
	Bucket               string     `json:"bucket,omitempty"`
	Region               string     `json:"region,omitempty"`
	Path                 string     `json:"path,omitempty"`
	ObjectLockVerifiedAt *time.Time `json:"object_lock_verified_at,omitempty"`
}

func toTargetOut(t store.Target) targetOut {
	return targetOut{ID: t.ID, Name: t.Name, Kind: t.Kind, Bucket: t.Bucket, Region: t.Region, Path: t.Path, ObjectLockVerifiedAt: t.ObjectLockVerifiedAt}
}

func (s *Server) handleTargetCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name, Kind, Bucket, Region, Path, KeyID, Key string
	}
	if err := decode(r, &in); err != nil || in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name and kind are required")
		return
	}
	t := &store.Target{Name: in.Name, Kind: in.Kind, Bucket: in.Bucket, Region: in.Region, Path: in.Path}
	verified := false
	switch in.Kind {
	case "filesystem":
		if in.Path == "" {
			writeErr(w, http.StatusBadRequest, "path is required for filesystem targets")
			return
		}
		if err := os.MkdirAll(in.Path, 0o700); err != nil {
			writeErr(w, http.StatusBadRequest, "cannot create path: "+err.Error())
			return
		}
	case "b2":
		if in.Bucket == "" || in.KeyID == "" || in.Key == "" {
			writeErr(w, http.StatusBadRequest, "bucket, key_id and key are required for b2 targets")
			return
		}
		sealed, err := s.sealCreds(targetCreds{KeyID: in.KeyID, Key: in.Key})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		t.SealedAdminKey = sealed
		if s.b2 != nil {
			info, err := s.b2.BucketInfo(r.Context(), in.KeyID, in.Key, in.Bucket) // Task 8
			if err != nil {
				writeErr(w, http.StatusBadRequest, "b2: "+err.Error())
				return
			}
			if info.ObjectLockEnabled {
				now := s.now()
				t.ObjectLockVerifiedAt = &now
				verified = true
			}
		}
	default:
		writeErr(w, http.StatusBadRequest, "kind must be b2 or filesystem")
		return
	}
	id, err := s.st.CreateTarget(r.Context(), t)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "object_lock_verified": verified})
}

func (s *Server) handleTargetList(w http.ResponseWriter, r *http.Request) {
	ts, err := s.st.Targets(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]targetOut, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTargetOut(t))
	}
	writeJSON(w, http.StatusOK, out)
}
```

`fleet/api/admin_templates.go`:

```go
package api

import (
	"encoding/json"
	"net/http"

	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/snapshot/policy"
)

type templateIn struct {
	Name    string          `json:"name"`
	Sources []string        `json:"sources"`
	Policy  json.RawMessage `json:"policy"`
}

type templateOut struct {
	ID      int64           `json:"id"`
	Name    string          `json:"name"`
	Sources []string        `json:"sources"`
	Policy  json.RawMessage `json:"policy"`
}

func (in *templateIn) validate() error {
	if in.Name == "" || len(in.Sources) == 0 {
		return errMsg("name and at least one source are required")
	}
	var p policy.Policy
	if len(in.Policy) == 0 {
		in.Policy = json.RawMessage(`{}`)
	}
	return json.Unmarshal(in.Policy, &p) // must be a Kopia policy object
}

type errMsg string

func (e errMsg) Error() string { return string(e) }

func (s *Server) handleTemplateCreate(w http.ResponseWriter, r *http.Request) {
	var in templateIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.st.CreateTemplate(r.Context(), &store.Template{Name: in.Name, Sources: in.Sources, PolicyJSON: in.Policy})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleTemplateUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in templateIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.st.Template(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "template not found")
		return
	}
	if err := s.st.UpdateTemplate(r.Context(), &store.Template{ID: id, Name: in.Name, Sources: in.Sources, PolicyJSON: in.Policy}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTemplateList(w http.ResponseWriter, r *http.Request) {
	ts, err := s.st.Templates(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]templateOut, 0, len(ts))
	for _, t := range ts {
		out = append(out, templateOut{ID: t.ID, Name: t.Name, Sources: t.Sources, Policy: t.PolicyJSON})
	}
	writeJSON(w, http.StatusOK, out)
}
```

`fleet/api/admin_groups.go`:

```go
package api

import (
	"net/http"

	"github.com/kopia/kopia/fleet/store"
)

type groupOut struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	TargetID   int64  `json:"target_id"`
	TemplateID int64  `json:"template_id"`
}

func (s *Server) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string `json:"name"`
		TargetID   int64  `json:"target_id"`
		TemplateID int64  `json:"template_id"`
	}
	if err := decode(r, &in); err != nil || in.Name == "" {
		writeErr(w, http.StatusBadRequest, "name, target_id and template_id are required")
		return
	}
	if _, err := s.st.Target(r.Context(), in.TargetID); err != nil {
		writeErr(w, http.StatusBadRequest, "unknown target_id")
		return
	}
	if _, err := s.st.Template(r.Context(), in.TemplateID); err != nil {
		writeErr(w, http.StatusBadRequest, "unknown template_id")
		return
	}
	id, err := s.st.CreateGroup(r.Context(), &store.Group{Name: in.Name, TargetID: in.TargetID, TemplateID: in.TemplateID})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleGroupList(w http.ResponseWriter, r *http.Request) {
	gs, err := s.st.Groups(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]groupOut, 0, len(gs))
	for _, g := range gs {
		out = append(out, groupOut{ID: g.ID, Name: g.Name, TargetID: g.TargetID, TemplateID: g.TemplateID})
	}
	writeJSON(w, http.StatusOK, out)
}
```

Add to `fleet/api/admin_stub.go` (replacing `mountAdmin`): `func (s *Server) mountAdminEnrollment(_ *mux.Router, _ func(http.HandlerFunc) http.HandlerFunc) {}` (Task 9 replaces). Extend the `b2api.API` stub with `BucketInfo(ctx context.Context, keyID, key, bucket string) (BucketInfo, error)` and `type BucketInfo struct{ ID string; ObjectLockEnabled bool }` so this compiles; `New(nil)` still returns `nil`.

- [ ] **Step 4: Run tests**

Run: `go test ./fleet/api/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fleet/api fleet/b2api && git commit -m "feat(fleet): admin CRUD for targets, templates, groups"
```

---

### Task 8: Minimal B2 native API client (`fleet/b2api`)

**Files:**
- Create: `fleet/b2api/client.go` (replaces stub)
- Test: `fleet/b2api/client_test.go`

**Interfaces:**
- Produces:

```go
package b2api
type BucketInfo struct{ ID string; ObjectLockEnabled bool }
type CreatedKey struct{ KeyID, Key string }
type API interface {
	BucketInfo(ctx context.Context, keyID, key, bucket string) (BucketInfo, error)
	CreateKey(ctx context.Context, keyID, key string, req KeyRequest) (CreatedKey, error)
	DeleteKey(ctx context.Context, keyID, key, targetKeyID string) error
}
type KeyRequest struct{ Name, BucketID, NamePrefix string; Capabilities []string }
var WriterCaps = []string{"listBuckets", "listFiles", "readFiles", "writeFiles"}   // never deleteFiles
var ReaderCaps = []string{"listBuckets", "listFiles", "readFiles"}
type Client struct{ http *http.Client; base string }   // base overridable for tests; default https://api.backblazeb2.com
func New(h *http.Client) *Client
func (c *Client) WithBase(u string) *Client
```
B2 endpoints used (v3): `GET  {base}/b2api/v3/b2_authorize_account` (Basic auth keyID:key → `{accountId, apiInfo:{storageApi:{apiUrl}}, authorizationToken}`), `POST {apiUrl}/b2api/v3/b2_list_buckets` `{accountId, bucketName}` → `{buckets:[{bucketId, fileLockConfiguration:{value:{isFileLockEnabled}}}]}`, `POST {apiUrl}/b2api/v3/b2_create_key` `{accountId, capabilities, keyName, bucketId, namePrefix}` → `{applicationKeyId, applicationKey}`, `POST {apiUrl}/b2api/v3/b2_delete_key` `{applicationKeyId}`.

- [ ] **Step 1: Write the failing test (httptest fake B2)**

`fleet/b2api/client_test.go`:

```go
package b2api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/b2api"
)

func fakeB2(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var calls []map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/b2api/v3/b2_authorize_account":
			u, p, ok := r.BasicAuth()
			if !ok || u != "adminKeyId" || p != "adminKey" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"accountId": "acct1", "authorizationToken": "tok1", "apiInfo": map[string]any{"storageApi": map[string]any{"apiUrl": srv.URL}}})
		case "/b2api/v3/b2_list_buckets", "/b2api/v3/b2_create_key", "/b2api/v3/b2_delete_key":
			if r.Header.Get("Authorization") != "tok1" {
				w.WriteHeader(401)
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			body["_path"] = r.URL.Path
			calls = append(calls, body)
			switch r.URL.Path {
			case "/b2api/v3/b2_list_buckets":
				json.NewEncoder(w).Encode(map[string]any{"buckets": []any{map[string]any{"bucketId": "bkt1", "bucketName": body["bucketName"], "fileLockConfiguration": map[string]any{"value": map[string]any{"isFileLockEnabled": true}}}}})
			case "/b2api/v3/b2_create_key":
				json.NewEncoder(w).Encode(map[string]any{"applicationKeyId": "newKeyId", "applicationKey": "newKey"})
			default:
				json.NewEncoder(w).Encode(map[string]any{})
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestBucketInfoCreateDeleteKey(t *testing.T) {
	srv, calls := fakeB2(t)
	c := b2api.New(srv.Client()).WithBase(srv.URL)
	ctx := context.Background()

	info, err := c.BucketInfo(ctx, "adminKeyId", "adminKey", "hody-backups")
	require.NoError(t, err)
	require.Equal(t, "bkt1", info.ID)
	require.True(t, info.ObjectLockEnabled)

	k, err := c.CreateKey(ctx, "adminKeyId", "adminKey", b2api.KeyRequest{Name: "warphold-ag1-writer", BucketID: "bkt1", NamePrefix: "agents/ag1/", Capabilities: b2api.WriterCaps})
	require.NoError(t, err)
	require.Equal(t, b2api.CreatedKey{KeyID: "newKeyId", Key: "newKey"}, k)
	created := (*calls)[1]
	require.Equal(t, "/b2api/v3/b2_create_key", created["_path"])
	require.Equal(t, "agents/ag1/", created["namePrefix"])
	require.NotContains(t, created["capabilities"], "deleteFiles")

	require.NoError(t, c.DeleteKey(ctx, "adminKeyId", "adminKey", "newKeyId"))
	require.Equal(t, "newKeyId", (*calls)[2]["applicationKeyId"])

	_, err = c.BucketInfo(ctx, "bad", "creds", "hody-backups")
	require.Error(t, err)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./fleet/b2api/ -v`
Expected: FAIL (`WithBase`, `BucketInfo` undefined).

- [ ] **Step 3: Implement**

`fleet/b2api/client.go`:

```go
// Package b2api is the minimal Backblaze B2 native API client Fleet needs
// to provision per-agent application keys and verify Object Lock.
package b2api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// BucketInfo is what Fleet needs to know about a bucket.
type BucketInfo struct {
	ID                string
	ObjectLockEnabled bool
}

// CreatedKey is a freshly created application key.
type CreatedKey struct{ KeyID, Key string }

// KeyRequest describes a scoped application key.
type KeyRequest struct {
	Name, BucketID, NamePrefix string
	Capabilities               []string
}

// WriterCaps lets an agent write and read its own prefix but never delete.
var WriterCaps = []string{"listBuckets", "listFiles", "readFiles", "writeFiles"}

// ReaderCaps is for the recovery kit.
var ReaderCaps = []string{"listBuckets", "listFiles", "readFiles"}

// API is the subset Fleet uses (faked in tests).
type API interface {
	BucketInfo(ctx context.Context, keyID, key, bucket string) (BucketInfo, error)
	CreateKey(ctx context.Context, keyID, key string, req KeyRequest) (CreatedKey, error)
	DeleteKey(ctx context.Context, keyID, key, targetKeyID string) error
}

// Client talks to B2 over HTTPS.
type Client struct {
	http *http.Client
	base string
}

// New returns a Client; nil h uses http.DefaultClient.
func New(h *http.Client) *Client {
	if h == nil {
		h = http.DefaultClient
	}
	return &Client{http: h, base: "https://api.backblazeb2.com"}
}

// WithBase overrides the authorize endpoint base (tests).
func (c *Client) WithBase(u string) *Client { c.base = u; return c }

type session struct {
	AccountID string `json:"accountId"`
	Token     string `json:"authorizationToken"`
	APIInfo   struct {
		StorageAPI struct {
			APIURL string `json:"apiUrl"`
		} `json:"storageApi"`
	} `json:"apiInfo"`
}

func (c *Client) authorize(ctx context.Context, keyID, key string) (*session, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/b2api/v3/b2_authorize_account", nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(keyID, key)
	var s session
	if err := c.do(req, &s); err != nil {
		return nil, fmt.Errorf("authorize: %w", err)
	}
	return &s, nil
}

func (c *Client) call(ctx context.Context, s *session, op string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.APIInfo.StorageAPI.APIURL+"/b2api/v3/"+op, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", s.Token)
	req.Header.Set("Content-Type", "application/json")
	if err := c.do(req, out); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("b2 returned %d: %s", resp.StatusCode, string(raw))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// BucketInfo authorizes with the admin key and looks the bucket up by name.
func (c *Client) BucketInfo(ctx context.Context, keyID, key, bucket string) (BucketInfo, error) {
	s, err := c.authorize(ctx, keyID, key)
	if err != nil {
		return BucketInfo{}, err
	}
	var out struct {
		Buckets []struct {
			ID   string `json:"bucketId"`
			Name string `json:"bucketName"`
			Lock struct {
				Value struct {
					Enabled bool `json:"isFileLockEnabled"`
				} `json:"value"`
			} `json:"fileLockConfiguration"`
		} `json:"buckets"`
	}
	if err := c.call(ctx, s, "b2_list_buckets", map[string]string{"accountId": s.AccountID, "bucketName": bucket}, &out); err != nil {
		return BucketInfo{}, err
	}
	for _, b := range out.Buckets {
		if b.Name == bucket {
			return BucketInfo{ID: b.ID, ObjectLockEnabled: b.Lock.Value.Enabled}, nil
		}
	}
	return BucketInfo{}, fmt.Errorf("bucket %q not found or not visible to this key", bucket)
}

// CreateKey creates a scoped application key.
func (c *Client) CreateKey(ctx context.Context, keyID, key string, r KeyRequest) (CreatedKey, error) {
	s, err := c.authorize(ctx, keyID, key)
	if err != nil {
		return CreatedKey{}, err
	}
	var out struct {
		ID  string `json:"applicationKeyId"`
		Key string `json:"applicationKey"`
	}
	body := map[string]any{"accountId": s.AccountID, "capabilities": r.Capabilities, "keyName": r.Name, "bucketId": r.BucketID, "namePrefix": r.NamePrefix}
	if err := c.call(ctx, s, "b2_create_key", body, &out); err != nil {
		return CreatedKey{}, err
	}
	return CreatedKey{KeyID: out.ID, Key: out.Key}, nil
}

// DeleteKey deletes an application key (used on revoke).
func (c *Client) DeleteKey(ctx context.Context, keyID, key, targetKeyID string) error {
	s, err := c.authorize(ctx, keyID, key)
	if err != nil {
		return err
	}
	return c.call(ctx, s, "b2_delete_key", map[string]string{"applicationKeyId": targetKeyID}, nil)
}
```

Change `fleet/api/server.go` `New` to use `b2: b2api.New(nil)` (it already does) and the field type to `b2api.API`. The `nil` check in `handleTargetCreate` is now dead; keep the interface check but it always runs. Add to `fleet/api/admin_test.go`:

```go
func TestB2TargetVerifiesObjectLock(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	h.s.SetB2ForTesting(fakeB2API{lock: true})
	resp, body := h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "b2", "kind": "b2", "bucket": "hody-backups", "key_id": "k", "key": "s"})
	require.Equal(t, 201, resp.StatusCode)
	require.Equal(t, true, body["object_lock_verified"])
	resp, list := h.doList("GET", "/api/v1/fleet/targets")
	require.Equal(t, 200, resp.StatusCode)
	_, hasKey := list[0]["key"]
	require.False(t, hasKey, "keys never leave the server")
}
```
with, in `server_test.go`, `harness` gaining field `s *api.Server` (set in `newHarness`), a `doList` helper that decodes `[]map[string]any`, and this fake in the test package:

```go
type fakeB2API struct{ lock bool; created []b2api.KeyRequest; deleted []string }
func (f fakeB2API) BucketInfo(_ context.Context, _, _, _ string) (b2api.BucketInfo, error) { return b2api.BucketInfo{ID: "bkt1", ObjectLockEnabled: f.lock}, nil }
func (f fakeB2API) CreateKey(_ context.Context, _, _ string, r b2api.KeyRequest) (b2api.CreatedKey, error) { return b2api.CreatedKey{KeyID: "kid-" + r.Name, Key: "sec-" + r.Name}, nil }
func (f fakeB2API) DeleteKey(_ context.Context, _, _, _ string) error { return nil }
```
and in `fleet/api/server.go`:
```go
// SetB2ForTesting swaps the B2 client.
func (s *Server) SetB2ForTesting(b b2api.API) { s.b2 = b }
```

- [ ] **Step 4: Run tests**

Run: `go test ./fleet/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fleet && git commit -m "feat(fleet): B2 native API client for key provisioning and Object Lock check"
```

---

### Task 9: Enrollment tokens (`fleet/enroll/token.go` + admin endpoints)

**Files:**
- Create: `fleet/enroll/token.go`, `fleet/api/admin_tokens.go`
- Modify: `fleet/api/admin_stub.go` (remove `mountAdminEnrollment` stub once `admin_tokens.go` defines it)
- Test: `fleet/enroll/token_test.go`, add to `fleet/api/admin_test.go`

**Interfaces:**
- Produces:

```go
package enroll
const (DefaultTTL = time.Hour; MaxTTL = 30 * 24 * time.Hour)
var (ErrTokenInvalid = errors.New("enrollment token is invalid, expired, revoked, or used up"); ErrTTLTooLong = errors.New("token lifetime exceeds 30 days"))
func NewToken() (plain string, hash []byte, err error)     // plain = "wh_" + base64url(24 random bytes); hash = sha256(plain)
func HashToken(plain string) []byte
type Tokens struct{ st *store.Store; now func() time.Time }
func NewTokens(st *store.Store) *Tokens
func (t *Tokens) Issue(ctx, groupID int64, ttl time.Duration, maxUses int, by int64) (plain string, tok *store.Token, err error)   // ttl<=0 → DefaultTTL; maxUses<0 → 1; 0 = unlimited
func (t *Tokens) Consume(ctx, plain string) (*store.Token, error)    // validates + increments uses atomically enough for one Fleet process (mutex)

// admin routes:
// POST /api/v1/fleet/tokens  {group_id, ttl_seconds?, max_uses?} → 201 {token, expires_at, max_uses}
// GET  /api/v1/fleet/groups/{id}/tokens → [{id, expires_at, max_uses, uses, revoked_at}]
// POST /api/v1/fleet/tokens/{id}/revoke → 204
```

- [ ] **Step 1: Write the failing test**

`fleet/enroll/token_test.go`:

```go
package enroll_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/fleet/store"
)

func TestTokenLifecycle(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	require.NoError(t, err)
	defer st.Close()
	ctx := context.Background()
	tk := enroll.NewTokens(st)
	now := time.Now()
	tk.SetNowForTesting(func() time.Time { return now })

	plain, tok, err := tk.Issue(ctx, 1, 0, -1, 7)
	require.NoError(t, err)
	require.True(t, len(plain) > 20 && plain[:3] == "wh_")
	require.Equal(t, 1, tok.MaxUses)
	require.WithinDuration(t, now.Add(enroll.DefaultTTL), tok.ExpiresAt, time.Second)

	got, err := tk.Consume(ctx, plain)
	require.NoError(t, err)
	require.Equal(t, tok.ID, got.ID)
	_, err = tk.Consume(ctx, plain)
	require.ErrorIs(t, err, enroll.ErrTokenInvalid, "single use")

	_, err = tk.Consume(ctx, "wh_nope")
	require.ErrorIs(t, err, enroll.ErrTokenInvalid)

	_, _, err = tk.Issue(ctx, 1, 31*24*time.Hour, 1, 7)
	require.ErrorIs(t, err, enroll.ErrTTLTooLong)

	multi, _, err := tk.Issue(ctx, 1, 2*time.Hour, 0, 7)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		_, err = tk.Consume(ctx, multi)
		require.NoError(t, err, "unlimited uses")
	}
	now = now.Add(3 * time.Hour)
	_, err = tk.Consume(ctx, multi)
	require.ErrorIs(t, err, enroll.ErrTokenInvalid, "expired")
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./fleet/enroll/ -v`
Expected: FAIL, package missing.

- [ ] **Step 3: Implement**

`fleet/enroll/token.go`:

```go
// Package enroll implements enrollment tokens and per-agent provisioning.
package enroll

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/kopia/kopia/fleet/store"
)

const (
	// DefaultTTL is the token lifetime when none is given.
	DefaultTTL = time.Hour
	// MaxTTL is the longest an admin may set.
	MaxTTL = 30 * 24 * time.Hour
)

var (
	ErrTokenInvalid = errors.New("enrollment token is invalid, expired, revoked, or used up")
	ErrTTLTooLong   = errors.New("token lifetime exceeds 30 days")
)

// NewToken returns a random token and its hash.
func NewToken() (string, []byte, error) {
	b := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", nil, err
	}
	plain := "wh_" + base64.RawURLEncoding.EncodeToString(b)
	return plain, HashToken(plain), nil
}

// HashToken hashes a token for storage/lookup.
func HashToken(plain string) []byte {
	h := sha256.Sum256([]byte(plain))
	return h[:]
}

// Tokens issues and consumes enrollment tokens.
type Tokens struct {
	st  *store.Store
	now func() time.Time
	mu  sync.Mutex
}

// NewTokens wraps a store.
func NewTokens(st *store.Store) *Tokens { return &Tokens{st: st, now: time.Now} }

// SetNowForTesting overrides the clock.
func (t *Tokens) SetNowForTesting(f func() time.Time) { t.now = f }

// Issue creates a token for a group. ttl<=0 → DefaultTTL; maxUses<0 → 1; 0 → unlimited.
func (t *Tokens) Issue(ctx context.Context, groupID int64, ttl time.Duration, maxUses int, by int64) (string, *store.Token, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > MaxTTL {
		return "", nil, ErrTTLTooLong
	}
	if maxUses < 0 {
		maxUses = 1
	}
	plain, hash, err := NewToken()
	if err != nil {
		return "", nil, err
	}
	tok := &store.Token{Hash: hash, GroupID: groupID, ExpiresAt: t.now().Add(ttl), MaxUses: maxUses, CreatedBy: by}
	id, err := t.st.CreateToken(ctx, tok)
	if err != nil {
		return "", nil, err
	}
	tok.ID = id
	return plain, tok, nil
}

// Consume validates a token and counts one use.
func (t *Tokens) Consume(ctx context.Context, plain string) (*store.Token, error) {
	t.mu.Lock() // ponytail: process-wide lock; fine for one Fleet server
	defer t.mu.Unlock()
	tok, err := t.st.TokenByHash(ctx, HashToken(plain))
	if err != nil {
		return nil, ErrTokenInvalid
	}
	now := t.now()
	if tok.RevokedAt != nil || now.After(tok.ExpiresAt) || (tok.MaxUses > 0 && tok.Uses >= tok.MaxUses) {
		return nil, ErrTokenInvalid
	}
	if err := t.st.IncrementTokenUses(ctx, tok.ID); err != nil {
		return nil, err
	}
	tok.Uses++
	return tok, nil
}
```

`fleet/api/admin_tokens.go`:

```go
package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet/enroll"
)

func (s *Server) mountAdminEnrollment(m *mux.Router, adm func(http.HandlerFunc) http.HandlerFunc) {
	m.HandleFunc("/api/v1/fleet/tokens", adm(s.handleTokenCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/groups/{id}/tokens", adm(s.handleTokenList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/tokens/{id}/revoke", adm(s.handleTokenRevoke)).Methods(http.MethodPost)
	s.mountAdminAgents(m, adm) // Task 11
}

func (s *Server) tokens() *enroll.Tokens {
	tk := enroll.NewTokens(s.st)
	tk.SetNowForTesting(s.now)
	return tk
}

func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GroupID    int64 `json:"group_id"`
		TTLSeconds int64 `json:"ttl_seconds"`
		MaxUses    *int  `json:"max_uses"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	if _, err := s.st.Group(r.Context(), in.GroupID); err != nil {
		writeErr(w, http.StatusBadRequest, "unknown group_id")
		return
	}
	maxUses := -1
	if in.MaxUses != nil {
		maxUses = *in.MaxUses
	}
	c, _ := r.Cookie(sessionCookie)
	adminID, _ := s.sess.verify(c.Value)
	plain, tok, err := s.tokens().Issue(r.Context(), in.GroupID, time.Duration(in.TTLSeconds)*time.Second, maxUses, adminID)
	if err != nil {
		if errors.Is(err, enroll.ErrTTLTooLong) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": tok.ID, "token": plain, "expires_at": tok.ExpiresAt, "max_uses": tok.MaxUses})
}

func (s *Server) handleTokenList(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	ts, err := s.st.TokensForGroup(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, map[string]any{"id": t.ID, "expires_at": t.ExpiresAt, "max_uses": t.MaxUses, "uses": t.Uses, "revoked_at": t.RevokedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.st.RevokeToken(r.Context(), id, s.now()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Add a stub `func (s *Server) mountAdminAgents(_ *mux.Router, _ func(http.HandlerFunc) http.HandlerFunc) {}` to `admin_stub.go` (Task 11 replaces).

Add to `fleet/api/admin_test.go`:

```go
func TestTokensDefaultsAndLimits(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	gid := h.mkGroup(t) // helper: filesystem target + template + group, returns group id
	resp, body := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})
	require.Equal(t, 201, resp.StatusCode)
	require.Equal(t, float64(1), body["max_uses"])
	require.True(t, strings.HasPrefix(body["token"].(string), "wh_"))
	resp, _ = h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid, "ttl_seconds": 31 * 86400})
	require.Equal(t, 400, resp.StatusCode)
	resp, _ = h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid, "ttl_seconds": 7 * 86400, "max_uses": 0})
	require.Equal(t, 201, resp.StatusCode)
}
```
with `mkGroup` in `server_test.go`:

```go
func (h *harness) mkGroup(t *testing.T) float64 {
	t.Helper()
	_, tg := h.do("POST", "/api/v1/fleet/targets", map[string]any{"name": "local", "kind": "filesystem", "path": t.TempDir()})
	_, tp := h.do("POST", "/api/v1/fleet/templates", map[string]any{"name": "Home default", "sources": []string{"~"}, "policy": map[string]any{}})
	_, g := h.do("POST", "/api/v1/fleet/groups", map[string]any{"name": "Laptops", "target_id": tg["id"], "template_id": tp["id"]})
	return g["id"].(float64)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./fleet/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fleet && git commit -m "feat(fleet): enrollment tokens (1h default, 30d max, multi-use)"
```

---

### Task 10: Provisioning: repo creation + scoped keys → Bundle (`fleet/enroll/provision.go`)

**Files:**
- Create: `fleet/enroll/provision.go`
- Test: `fleet/enroll/provision_test.go`

**Interfaces:**
- Produces:

```go
package enroll
// Bundle is everything an agent needs to connect, and what Fleet escrows.
type Bundle struct {
	ConnectToken string `json:"connect_token"`   // repo.EncodeToken(password, ConnectionInfo) → agent connects with this alone
	Password     string `json:"password"`
	Prefix       string `json:"prefix"`          // "agents/<id>/" (b2) or absolute dir (filesystem)
	WriterKeyID  string `json:"writer_key_id,omitempty"`
	WriterKey    string `json:"writer_key,omitempty"`
	ReaderKeyID  string `json:"reader_key_id,omitempty"`
	ReaderKey    string `json:"reader_key,omitempty"`
}
type TargetSpec struct{ Kind, Bucket, Path, AdminKeyID, AdminKey string }
type Provisioner struct{ B2 b2api.API; Owner string }   // Owner = maintenance owner, e.g. "fleet@<hostname>"
func (p *Provisioner) Provision(ctx context.Context, t TargetSpec, agentID string) (*Bundle, error)
func (p *Provisioner) Revoke(ctx context.Context, t TargetSpec, b *Bundle) error   // deletes both B2 keys; no-op for filesystem
func NewPassword() (string, error)                                                  // 32 random bytes, base64url
```
Steps inside `Provision`: build the **admin** `blob.ConnectionInfo` (b2 with admin key + prefix, or filesystem path) → `blob.NewStorage(ctx, ci, true)` → `repo.Initialize(ctx, st, &repo.NewRepositoryOptions{}, password)` → connect to a temp config file with `repo.Connect` → `repo.Open` → `repo.WriteSession` to `maintenance.SetParams(owner=p.Owner)` → close → build the **agent** ConnectionInfo (writer key) → `repo.EncodeToken(password, agentCI)`.

- [ ] **Step 1: Write the failing test (filesystem target, real Kopia repo)**

`fleet/enroll/provision_test.go`:

```go
package enroll_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/maintenance"
)

func TestProvisionFilesystemCreatesConnectableRepo(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	p := &enroll.Provisioner{Owner: "fleet@test"}
	b, err := p.Provision(ctx, enroll.TargetSpec{Kind: "filesystem", Path: root}, "ag_1")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "agents", "ag_1"), b.Prefix)
	require.Len(t, b.Password, 43)
	require.Empty(t, b.WriterKeyID)

	// The agent side: connect using only the token.
	ci, pw, err := repo.DecodeToken(b.ConnectToken)
	require.NoError(t, err)
	require.Equal(t, b.Password, pw)
	st, err := blob.NewStorage(ctx, ci, false)
	require.NoError(t, err)
	cfg := filepath.Join(t.TempDir(), "repository.config")
	require.NoError(t, repo.Connect(ctx, cfg, st, pw, &repo.ConnectOptions{}))
	r, err := repo.Open(ctx, cfg, pw, nil)
	require.NoError(t, err)
	defer r.Close(ctx)
	params, err := maintenance.GetParams(ctx, r)
	require.NoError(t, err)
	require.Equal(t, "fleet@test", params.Owner)

	// A second agent gets its own directory.
	b2, err := p.Provision(ctx, enroll.TargetSpec{Kind: "filesystem", Path: root}, "ag_2")
	require.NoError(t, err)
	require.NotEqual(t, b.Prefix, b2.Prefix)
	require.NotEqual(t, b.Password, b2.Password)
}

func TestProvisionB2UsesWriterKeyInTokenAndReaderKeyInBundle(t *testing.T) {
	ctx := context.Background()
	fake := &fakeB2{}
	p := &enroll.Provisioner{B2: fake, Owner: "fleet@test", InitializeForTesting: func(context.Context, blob.ConnectionInfo, string) error { return nil }}
	b, err := p.Provision(ctx, enroll.TargetSpec{Kind: "b2", Bucket: "hody-backups", AdminKeyID: "adm", AdminKey: "sec"}, "ag_9")
	require.NoError(t, err)
	require.Equal(t, "agents/ag_9/", b.Prefix)
	require.Len(t, fake.created, 2)
	require.Equal(t, "warphold-ag_9-writer", fake.created[0].Name)
	require.NotContains(t, fake.created[0].Capabilities, "deleteFiles")
	require.Equal(t, "warphold-ag_9-reader", fake.created[1].Name)
	require.NotContains(t, fake.created[1].Capabilities, "writeFiles")
	ci, _, err := repo.DecodeToken(b.ConnectToken)
	require.NoError(t, err)
	require.Equal(t, "b2", ci.Type)
	require.Equal(t, b.WriterKeyID, "kid-warphold-ag_9-writer")
	require.Equal(t, b.ReaderKeyID, "kid-warphold-ag_9-reader")

	require.NoError(t, p.Revoke(ctx, enroll.TargetSpec{Kind: "b2", AdminKeyID: "adm", AdminKey: "sec"}, b))
	require.ElementsMatch(t, []string{b.WriterKeyID, b.ReaderKeyID}, fake.deleted)
}

type fakeB2 struct {
	created []b2api.KeyRequest
	deleted []string
}

func (f *fakeB2) BucketInfo(context.Context, string, string, string) (b2api.BucketInfo, error) {
	return b2api.BucketInfo{ID: "bkt1", ObjectLockEnabled: true}, nil
}
func (f *fakeB2) CreateKey(_ context.Context, _, _ string, r b2api.KeyRequest) (b2api.CreatedKey, error) {
	f.created = append(f.created, r)
	return b2api.CreatedKey{KeyID: "kid-" + r.Name, Key: "sec-" + r.Name}, nil
}
func (f *fakeB2) DeleteKey(_ context.Context, _, _, id string) error { f.deleted = append(f.deleted, id); return nil }
```
(add `"github.com/kopia/kopia/fleet/b2api"` to imports.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./fleet/enroll/ -run TestProvision -v`
Expected: FAIL, `Provisioner` undefined.

- [ ] **Step 3: Implement**

`fleet/enroll/provision.go`:

```go
package enroll

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/kopia/kopia/fleet/b2api"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/b2"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/maintenance"
)

// Bundle is everything an agent needs to connect, and what Fleet escrows.
type Bundle struct {
	ConnectToken string `json:"connect_token"`
	Password     string `json:"password"`
	Prefix       string `json:"prefix"`
	WriterKeyID  string `json:"writer_key_id,omitempty"`
	WriterKey    string `json:"writer_key,omitempty"`
	ReaderKeyID  string `json:"reader_key_id,omitempty"`
	ReaderKey    string `json:"reader_key,omitempty"`
}

// TargetSpec is the unsealed view of a target.
type TargetSpec struct {
	Kind, Bucket, Path, AdminKeyID, AdminKey string
}

// Provisioner creates per-agent repositories and credentials.
type Provisioner struct {
	B2    b2api.API
	Owner string
	// InitializeForTesting replaces repository creation in unit tests that have no storage.
	InitializeForTesting func(ctx context.Context, ci blob.ConnectionInfo, password string) error
}

// NewPassword returns 32 random bytes, base64url (43 chars).
func NewPassword() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Provision creates the agent's repository and returns its bundle.
func (p *Provisioner) Provision(ctx context.Context, t TargetSpec, agentID string) (*Bundle, error) {
	password, err := NewPassword()
	if err != nil {
		return nil, err
	}
	b := &Bundle{Password: password}
	var adminCI, agentCI blob.ConnectionInfo

	switch t.Kind {
	case "filesystem":
		dir := filepath.Join(t.Path, "agents", agentID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		b.Prefix = dir
		adminCI = blob.ConnectionInfo{Type: "filesystem", Config: &filesystem.Options{Path: dir}}
		agentCI = adminCI
	case "b2":
		if p.B2 == nil {
			return nil, errors.New("b2 client not configured")
		}
		info, err := p.B2.BucketInfo(ctx, t.AdminKeyID, t.AdminKey, t.Bucket)
		if err != nil {
			return nil, err
		}
		b.Prefix = "agents/" + agentID + "/"
		w, err := p.B2.CreateKey(ctx, t.AdminKeyID, t.AdminKey, b2api.KeyRequest{Name: "warphold-" + agentID + "-writer", BucketID: info.ID, NamePrefix: b.Prefix, Capabilities: b2api.WriterCaps})
		if err != nil {
			return nil, err
		}
		r, err := p.B2.CreateKey(ctx, t.AdminKeyID, t.AdminKey, b2api.KeyRequest{Name: "warphold-" + agentID + "-reader", BucketID: info.ID, NamePrefix: b.Prefix, Capabilities: b2api.ReaderCaps})
		if err != nil {
			_ = p.B2.DeleteKey(ctx, t.AdminKeyID, t.AdminKey, w.KeyID)
			return nil, err
		}
		b.WriterKeyID, b.WriterKey, b.ReaderKeyID, b.ReaderKey = w.KeyID, w.Key, r.KeyID, r.Key
		adminCI = blob.ConnectionInfo{Type: "b2", Config: &b2.Options{BucketName: t.Bucket, Prefix: b.Prefix, KeyID: t.AdminKeyID, Key: t.AdminKey}}
		agentCI = blob.ConnectionInfo{Type: "b2", Config: &b2.Options{BucketName: t.Bucket, Prefix: b.Prefix, KeyID: w.KeyID, Key: w.Key}}
	default:
		return nil, errors.New("unsupported target kind " + t.Kind)
	}

	init := p.initialize
	if p.InitializeForTesting != nil {
		init = p.InitializeForTesting
	}
	if err := init(ctx, adminCI, password); err != nil {
		return nil, err
	}
	tok, err := repo.EncodeToken(password, agentCI)
	if err != nil {
		return nil, err
	}
	b.ConnectToken = tok
	return b, nil
}

func (p *Provisioner) initialize(ctx context.Context, ci blob.ConnectionInfo, password string) error {
	st, err := blob.NewStorage(ctx, ci, true)
	if err != nil {
		return err
	}
	defer st.Close(ctx) //nolint:errcheck
	if err := repo.Initialize(ctx, st, &repo.NewRepositoryOptions{}, password); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "warphold-provision-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	cfg := filepath.Join(tmp, "repository.config")
	if err := repo.Connect(ctx, cfg, st, password, &repo.ConnectOptions{}); err != nil {
		return err
	}
	r, err := repo.Open(ctx, cfg, password, nil)
	if err != nil {
		return err
	}
	defer r.Close(ctx) //nolint:errcheck
	return repo.WriteSession(ctx, r, repo.WriteSessionOptions{Purpose: "fleet-provision"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		params, err := maintenance.GetParams(ctx, w)
		if err != nil {
			return err
		}
		params.Owner = p.Owner
		return maintenance.SetParams(ctx, w, params)
	})
}

// Revoke deletes the agent's B2 keys. Filesystem targets keep their data.
func (p *Provisioner) Revoke(ctx context.Context, t TargetSpec, b *Bundle) error {
	if t.Kind != "b2" || p.B2 == nil {
		return nil
	}
	return errors.Join(
		p.B2.DeleteKey(ctx, t.AdminKeyID, t.AdminKey, b.WriterKeyID),
		p.B2.DeleteKey(ctx, t.AdminKeyID, t.AdminKey, b.ReaderKeyID),
	)
}
```

If `repo.Connect`'s cache options require a cache directory, pass `&repo.ConnectOptions{CachingOptions: content.CachingOptions{CacheDirectory: filepath.Join(tmp, "cache")}}` (import `github.com/kopia/kopia/repo/content`). Check by running the test.

- [ ] **Step 4: Run tests**

Run: `go test ./fleet/enroll/ -v`
Expected: PASS (both). The filesystem test creates a real Kopia repository; it takes a few seconds.

- [ ] **Step 5: Commit**

```bash
git add fleet/enroll && git commit -m "feat(fleet): provision per-agent repos with scoped B2 keys, Fleet as maintenance owner"
```

---

### Task 11: Enroll endpoint, agent admin endpoints, /enroll.sh

**Files:**
- Create: `fleet/api/agent_endpoints.go`, `fleet/api/admin_agents.go`, `fleet/api/enrollsh.go`, `fleet/api/enroll.sh.tmpl`
- Delete: `fleet/api/admin_stub.go`
- Test: `fleet/api/enroll_test.go`

**Interfaces:**
- Produces:

```
POST /api/v1/fleet/enroll   {token, hostname, os, arch, version, scope}
   → 201 {agent_id, bearer, name, connect_token, poll_interval_seconds}   | 403 {"error": "enrollment token is invalid…"}
GET  /enroll.sh?token=wh_…  → text/x-shellscript (installer; Task 17 fills the body, this task serves the template)
GET  /api/v1/fleet/agents            → [{id,name,hostname,os,arch,version,scope,group_id,enrolled_at,last_seen_at,revoked_at,health}]  (health from Task 14; "unknown" until then)
GET  /api/v1/fleet/agents/{id}       → same object + reports:[last 20]
POST /api/v1/fleet/agents/{id}/revoke   → 204 (deletes B2 keys, marks revoked; polls thereafter get 401)
POST /api/v1/fleet/agents/{id}/commands {kind:"snapshot-now"|"pause"|"resume"|"verify", source?} → 201 {id}
```
- Go: `func (s *Server) bundleFor(ctx, a *store.Agent) (*enroll.Bundle, error)` (unseal), `func (s *Server) specFor(ctx, t *store.Target) (enroll.TargetSpec, error)`, `func NewBearer() (plain string, hash []byte, error)` (same shape as tokens, prefix `wa_`), `func (s *Server) provisioner() *enroll.Provisioner` (Owner = `"fleet@" + os.Hostname()`).
- Agent IDs: `"ag_" + 10 random base32 chars`. Agent display name defaults to `hostname`.

- [ ] **Step 1: Write the failing test**

`fleet/api/enroll_test.go`:

```go
package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo"
)

func TestEnrollHappyPathAndRevoke(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	gid := h.mkGroup(t)
	_, tok := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})

	admin := h.jar
	h.jar = nil // enrollment needs no session
	resp, body := h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok["token"], "hostname": "fw13", "os": "linux", "arch": "amd64", "version": "0.1.0", "scope": "user"})
	require.Equal(t, 201, resp.StatusCode, body)
	require.True(t, strings.HasPrefix(body["agent_id"].(string), "ag_"))
	require.True(t, strings.HasPrefix(body["bearer"].(string), "wa_"))
	require.Equal(t, "fw13", body["name"])
	_, pw, err := repo.DecodeToken(body["connect_token"].(string))
	require.NoError(t, err)
	require.Len(t, pw, 43)
	require.Equal(t, float64(300), body["poll_interval_seconds"])

	resp, _ = h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok["token"], "hostname": "again", "os": "linux", "arch": "amd64", "scope": "user"})
	require.Equal(t, 403, resp.StatusCode, "single-use token")

	h.jar = admin
	resp, list := h.doList("GET", "/api/v1/fleet/agents")
	require.Equal(t, 200, resp.StatusCode)
	require.Len(t, list, 1)
	_, leaks := list[0]["connect_token"]
	require.False(t, leaks)
	id := list[0]["id"].(string)

	resp, _ = h.do("POST", "/api/v1/fleet/agents/"+id+"/commands", map[string]any{"kind": "snapshot-now", "source": "~"})
	require.Equal(t, 201, resp.StatusCode)
	resp, _ = h.do("POST", "/api/v1/fleet/agents/"+id+"/commands", map[string]any{"kind": "rm-rf"})
	require.Equal(t, 400, resp.StatusCode)

	resp, _ = h.do("POST", "/api/v1/fleet/agents/"+id+"/revoke", nil)
	require.Equal(t, 204, resp.StatusCode)
	_, detail := h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	require.NotNil(t, detail["revoked_at"])
}

func TestEnrollShIsServed(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	res, err := http.Get(h.srv.URL + "/enroll.sh?token=wh_abc")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, 200, res.StatusCode)
	require.Contains(t, res.Header.Get("Content-Type"), "text/x-shellscript")
	buf := make([]byte, 4096)
	n, _ := res.Body.Read(buf)
	require.Contains(t, string(buf[:n]), "warphold agent enroll")
	require.Contains(t, string(buf[:n]), "--token wh_abc")
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./fleet/api/ -run 'TestEnroll' -v`
Expected: FAIL with 404s.

- [ ] **Step 3: Implement**

`fleet/api/agent_endpoints.go` (the enroll half; poll/report arrive in Task 14 in the same file):

```go
package api

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/fleet/store"
)

const defaultPollSeconds = 300

func (s *Server) mountAgent(m *mux.Router) {
	m.HandleFunc("/api/v1/fleet/enroll", s.requireActivated(s.handleEnroll)).Methods(http.MethodPost)
	m.HandleFunc("/enroll.sh", s.requireActivated(s.handleEnrollSh)).Methods(http.MethodGet)
	s.mountAgentPoll(m) // Task 14
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return "ag_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:10], nil
}

// NewBearer returns an agent bearer token and its hash.
func NewBearer() (string, []byte, error) {
	plain, hash, err := enroll.NewToken()
	if err != nil {
		return "", nil, err
	}
	plain = "wa_" + plain[3:]
	return plain, enroll.HashToken(plain), nil
}

func (s *Server) provisioner() *enroll.Provisioner {
	host, _ := os.Hostname()
	return &enroll.Provisioner{B2: s.b2, Owner: "fleet@" + host}
}

func (s *Server) specFor(ctx context.Context, t *store.Target) (enroll.TargetSpec, error) {
	kid, key, err := s.targetCreds(ctx, t)
	if err != nil {
		return enroll.TargetSpec{}, err
	}
	return enroll.TargetSpec{Kind: t.Kind, Bucket: t.Bucket, Path: t.Path, AdminKeyID: kid, AdminKey: key}, nil
}

func (s *Server) bundleFor(_ context.Context, a *store.Agent) (*enroll.Bundle, error) {
	raw, err := s.key.Open(a.SealedBundle)
	if err != nil {
		return nil, err
	}
	var b enroll.Bundle
	return &b, json.Unmarshal(raw, &b)
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token, Hostname, OS, Arch, Version, Scope string }
	if err := decode(r, &in); err != nil || in.Token == "" || in.Hostname == "" {
		writeErr(w, http.StatusBadRequest, "token and hostname are required")
		return
	}
	if in.Scope == "" {
		in.Scope = "user"
	}
	ctx := r.Context()
	tok, err := s.tokens().Consume(ctx, in.Token)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	group, err := s.st.Group(ctx, tok.GroupID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "token's group is gone")
		return
	}
	target, err := s.st.Target(ctx, group.TargetID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "group's target is gone")
		return
	}
	spec, err := s.specFor(ctx, target)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, err := newID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	bundle, err := s.provisioner().Provision(ctx, spec, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provisioning failed: "+err.Error())
		return
	}
	sealedBundle, err := json.Marshal(bundle)
	if err == nil {
		sealedBundle, err = s.key.Seal(sealedBundle)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	bearer, bearerHash, err := NewBearer()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a := &store.Agent{ID: id, Name: in.Hostname, Hostname: in.Hostname, OS: in.OS, Arch: in.Arch, Version: in.Version, Scope: in.Scope, GroupID: group.ID, BearerHash: bearerHash, SealedBundle: sealedBundle, EnrolledAt: s.now()}
	if err := s.st.CreateAgent(ctx, a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"agent_id": id, "bearer": bearer, "name": a.Name,
		"connect_token": bundle.ConnectToken, "poll_interval_seconds": s.pollInterval(ctx),
	})
}

func (s *Server) pollInterval(ctx context.Context) int {
	v, _ := s.st.Setting(ctx, "poll_interval")
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return defaultPollSeconds
}
```
(imports for this file: `context, crypto/rand, encoding/base32, encoding/json, io, net/http, os, strconv, strings, github.com/gorilla/mux, fleet/enroll, fleet/store`.)

`fleet/api/enroll.sh.tmpl`:

```sh
#!/bin/sh
# WarpHold agent installer. Generated by the Fleet server at {{.Server}}.
set -eu
TOKEN="{{.Token}}"
SERVER="{{.Server}}"
SCOPE="${WARPHOLD_SCOPE:-user}"
while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; shift 2 ;;
    --token=*) TOKEN="${1#--token=}"; shift ;;
    --scope) SCOPE="$2"; shift 2 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -n "$TOKEN" ] || { echo "usage: sh -s -- --token <token>" >&2; exit 2; }
ARCH="$(uname -m)"; case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; *) echo "unsupported arch $ARCH" >&2; exit 1 ;; esac
if [ "$SCOPE" = "system" ]; then BIN=/usr/local/bin; else BIN="${HOME}/.local/bin"; fi
mkdir -p "$BIN"
echo "Downloading warphold ({{.Version}}, linux/$ARCH)…"
curl -fsSL "{{.Server}}/dl/warphold-linux-$ARCH" -o "$BIN/warphold.tmp" && chmod +x "$BIN/warphold.tmp" && mv "$BIN/warphold.tmp" "$BIN/warphold"
"$BIN/warphold" agent enroll --server "$SERVER" --token "$TOKEN" --scope "$SCOPE"
"$BIN/warphold" agent install --scope "$SCOPE"
echo "Enrolled. The agent is running and will report to $SERVER."
```

`fleet/api/enrollsh.go`:

```go
package api

import (
	_ "embed"
	"net/http"
	"text/template"

	"github.com/kopia/kopia/repo"
)

//go:embed enroll.sh.tmpl
var enrollShSrc string

var enrollSh = template.Must(template.New("enroll.sh").Parse(enrollShSrc))

func (s *Server) handleEnrollSh(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_ = enrollSh.Execute(w, map[string]string{
		"Server":  scheme + "://" + r.Host,
		"Token":   r.URL.Query().Get("token"),
		"Version": repo.BuildVersion,
	})
}
```
The `/dl/warphold-linux-<arch>` download route is out of scope for this plan (Plan 2 adds it with the release pipeline); until then the installer is exercised with a pre-copied binary in the e2e test of Task 17.

`fleet/api/admin_agents.go`:

```go
package api

import (
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet/store"
)

var allowedCommands = map[string]bool{"snapshot-now": true, "pause": true, "resume": true, "verify": true}

func (s *Server) mountAdminAgents(m *mux.Router, adm func(http.HandlerFunc) http.HandlerFunc) {
	m.HandleFunc("/api/v1/fleet/agents", adm(s.handleAgentList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/agents/{id}", adm(s.handleAgentGet)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/agents/{id}/revoke", adm(s.handleAgentRevoke)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/agents/{id}/commands", adm(s.handleAgentCommand)).Methods(http.MethodPost)
}

type agentOut struct {
	ID, Name, Hostname, OS, Arch, Version, Scope string
	GroupID                                      int64      `json:"group_id"`
	EnrolledAt                                   time.Time  `json:"enrolled_at"`
	LastSeenAt                                   *time.Time `json:"last_seen_at"`
	RevokedAt                                    *time.Time `json:"revoked_at"`
	Health                                       string     `json:"health"`
}

func (s *Server) agentOut(a store.Agent, latest *store.Report) agentOut {
	return agentOut{ID: a.ID, Name: a.Name, Hostname: a.Hostname, OS: a.OS, Arch: a.Arch, Version: a.Version, Scope: a.Scope, GroupID: a.GroupID, EnrolledAt: a.EnrolledAt, LastSeenAt: a.LastSeenAt, RevokedAt: a.RevokedAt, Health: s.healthOf(a, latest)}
}

// healthOf is replaced by fleet/health in Task 14; until then every agent is "unknown".
func (s *Server) healthOf(_ store.Agent, _ *store.Report) string { return "unknown" }

func (s *Server) handleAgentList(w http.ResponseWriter, r *http.Request) {
	as, err := s.st.Agents(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	latest, _ := s.st.LatestReports(r.Context())
	out := make([]agentOut, 0, len(as))
	for _, a := range as {
		var lr *store.Report
		if x, ok := latest[a.ID]; ok {
			lr = &x
		}
		out = append(out, s.agentOut(a, lr))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAgentGet(w http.ResponseWriter, r *http.Request) {
	a, err := s.st.Agent(r.Context(), mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}
	reports, _ := s.st.ReportsForAgent(r.Context(), a.ID, 20)
	var lr *store.Report
	if len(reports) > 0 {
		lr = &reports[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": s.agentOut(*a, lr), "reports": reports})
}

func (s *Server) handleAgentRevoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := s.st.Agent(ctx, mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}
	g, err := s.st.Group(ctx, a.GroupID)
	if err == nil {
		if t, err := s.st.Target(ctx, g.TargetID); err == nil {
			if spec, err := s.specFor(ctx, t); err == nil {
				if b, err := s.bundleFor(ctx, a); err == nil {
					_ = s.provisioner().Revoke(ctx, spec, b) // best effort; keys may already be gone
				}
			}
		}
	}
	if err := s.st.RevokeAgent(ctx, a.ID, s.now()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentCommand(w http.ResponseWriter, r *http.Request) {
	var in struct{ Kind, Source string }
	if err := decode(r, &in); err != nil || !allowedCommands[in.Kind] {
		writeErr(w, http.StatusBadRequest, "kind must be one of snapshot-now, pause, resume, verify")
		return
	}
	a, err := s.st.Agent(r.Context(), mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}
	id, err := s.st.AddCommand(r.Context(), &store.Command{AgentID: a.ID, Kind: in.Kind, Source: in.Source})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}
```

Delete `fleet/api/admin_stub.go` and add `func (s *Server) mountAgentPoll(_ *mux.Router) {}` temporarily at the bottom of `agent_endpoints.go` (Task 14 replaces it).

- [ ] **Step 4: Run tests**

Run: `go test ./fleet/... -v`
Expected: PASS. `TestEnrollHappyPathAndRevoke` creates a real filesystem repo; allow a few seconds.

- [ ] **Step 5: Commit**

```bash
git add fleet && git commit -m "feat(fleet): enrollment endpoint, agent admin endpoints, enroll.sh"
```

---

### Task 12: Agent state + `agent enroll`

**Files:**
- Create: `agent/state/state.go`, `cli/command_agent.go` (replace stub), `cli/command_agent_enroll.go`
- Test: `agent/state/state_test.go`, `cli/command_agent_enroll_test.go`

**Interfaces:**
- Produces:

```go
package state
type Config struct {
	Server       string `json:"server"`
	AgentID      string `json:"agent_id"`
	Bearer       string `json:"bearer"`
	Name         string `json:"name"`
	PollInterval int    `json:"poll_interval_seconds"`
	Scope        string `json:"scope"`
	ETag         string `json:"policy_etag"`
}
func Dir(scope string) string            // user: $XDG_CONFIG_HOME/warphold or ~/.config/warphold; system: /etc/warphold. $WARPHOLD_STATE_DIR overrides both (tests).
func CacheDir(scope string) string       // user: $XDG_CACHE_HOME/warphold or ~/.cache/warphold; system: /var/cache/warphold
func RepoConfigPath(scope string) string // Dir(scope)/repository.config
func Load(scope string) (*Config, error)
func Save(scope string, c *Config) error // 0600, dir 0700
```

`warphold agent enroll --server URL --token T [--scope user|system] [--name N]`:
1. `POST {server}/api/v1/fleet/enroll` with `{token, hostname, os, arch, version, scope}`.
2. `repo.DecodeToken(connect_token)` → `blob.NewStorage(ctx, ci, false)` → `repo.Connect(ctx, RepoConfigPath(scope), st, password, &repo.ConnectOptions{CachingOptions: content.CachingOptions{CacheDirectory: CacheDir(scope)}})`.
3. `state.Save(scope, &Config{...})`.
4. Prints `Enrolled as <name> (<agent_id>).`

- [ ] **Step 1: Write the failing tests**

`agent/state/state_test.go`:

```go
package state_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/state"
)

func TestSaveLoad0600(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WARPHOLD_STATE_DIR", dir)
	c := &state.Config{Server: "https://fleet.example", AgentID: "ag_1", Bearer: "wa_x", Name: "fw13", PollInterval: 300, Scope: "user"}
	require.NoError(t, state.Save("user", c))
	st, err := os.Stat(filepath.Join(dir, "agent.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	got, err := state.Load("user")
	require.NoError(t, err)
	require.Equal(t, c, got)
	require.Equal(t, filepath.Join(dir, "repository.config"), state.RepoConfigPath("user"))
}

func TestDirsWithoutOverride(t *testing.T) {
	t.Setenv("WARPHOLD_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	require.Equal(t, "/tmp/xdg/warphold", state.Dir("user"))
	require.Equal(t, "/etc/warphold", state.Dir("system"))
	require.Equal(t, "/var/cache/warphold", state.CacheDir("system"))
}
```

`cli/command_agent_enroll_test.go`:

```go
package cli_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/fleet/api"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/tests/testenv"
)

// fleetForTest activates a Fleet with one filesystem group and returns its URL and a fresh token.
func fleetForTest(t *testing.T) (string, string) {
	t.Helper()
	s := api.New(t.TempDir())
	t.Cleanup(func() { s.Close() })
	m := mux.NewRouter()
	s.Mount(m)
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	ctx := context.Background()
	require.NoError(t, s.Activate(ctx, "seal-me-please", "hody@hody.dev", "pw12345678"))
	tid, tpl, gid := s.SeedGroupForTesting(ctx, t.TempDir(), []string{"~"}, `{"retention":{"keepLatest":3}}`)
	_ = tid
	_ = tpl
	plain := s.IssueTokenForTesting(ctx, gid)
	return ts.URL, plain
}

func TestAgentEnrollWritesStateAndConnectsRepo(t *testing.T) {
	url, tok := fleetForTest(t)
	stateDir := t.TempDir()
	t.Setenv("WARPHOLD_STATE_DIR", stateDir)
	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)
	e.RunAndExpectSuccess(t, "agent", "enroll", "--server", url, "--token", tok, "--scope", "user")

	cfg, err := state.Load("user")
	require.NoError(t, err)
	require.Equal(t, url, cfg.Server)
	require.NotEmpty(t, cfg.Bearer)
	_, err = repo.Open(context.Background(), filepath.Join(stateDir, "repository.config"), "", nil)
	require.Error(t, err, "password is not persisted in the config file")
	raw, _ := json.Marshal(cfg)
	require.NotContains(t, string(raw), "connect_token")
}
```
Add to `fleet/api/server.go` two test seams (exported, suffixed `ForTesting`, no security impact since they need a live `*Server`):

```go
// SeedGroupForTesting creates a filesystem target, a template and a group.
func (s *Server) SeedGroupForTesting(ctx context.Context, path string, sources []string, policyJSON string) (targetID, templateID, groupID int64) {
	targetID, _ = s.st.CreateTarget(ctx, &store.Target{Name: "local", Kind: "filesystem", Path: path})
	templateID, _ = s.st.CreateTemplate(ctx, &store.Template{Name: "test", Sources: sources, PolicyJSON: json.RawMessage(policyJSON)})
	groupID, _ = s.st.CreateGroup(ctx, &store.Group{Name: "Test", TargetID: targetID, TemplateID: templateID})
	return
}

// IssueTokenForTesting issues a default token for a group.
func (s *Server) IssueTokenForTesting(ctx context.Context, groupID int64) string {
	plain, _, _ := s.tokens().Issue(ctx, groupID, 0, -1, 0)
	return plain
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./agent/state/ ./cli/ -run 'TestSaveLoad|TestDirs|TestAgentEnroll' -v`
Expected: FAIL (packages/commands missing).

- [ ] **Step 3: Implement state**

`agent/state/state.go`:

```go
// Package state persists the agent's enrollment on disk.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is agent.json.
type Config struct {
	Server       string `json:"server"`
	AgentID      string `json:"agent_id"`
	Bearer       string `json:"bearer"`
	Name         string `json:"name"`
	PollInterval int    `json:"poll_interval_seconds"`
	Scope        string `json:"scope"`
	ETag         string `json:"policy_etag"`
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// Dir is where agent.json and repository.config live.
func Dir(scope string) string {
	if d := os.Getenv("WARPHOLD_STATE_DIR"); d != "" {
		return d
	}
	if scope == "system" {
		return "/etc/warphold"
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "warphold")
	}
	return filepath.Join(home(), ".config", "warphold")
}

// CacheDir is the Kopia cache directory.
func CacheDir(scope string) string {
	if d := os.Getenv("WARPHOLD_STATE_DIR"); d != "" {
		return filepath.Join(d, "cache")
	}
	if scope == "system" {
		return "/var/cache/warphold"
	}
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "warphold")
	}
	return filepath.Join(home(), ".cache", "warphold")
}

// RepoConfigPath is the Kopia config file for the agent's repository.
func RepoConfigPath(scope string) string { return filepath.Join(Dir(scope), "repository.config") }

func file(scope string) string { return filepath.Join(Dir(scope), "agent.json") }

// Load reads agent.json.
func Load(scope string) (*Config, error) {
	b, err := os.ReadFile(file(scope))
	if err != nil {
		return nil, err
	}
	var c Config
	return &c, json.Unmarshal(b, &c)
}

// Save writes agent.json with mode 0600.
func Save(scope string, c *Config) error {
	if err := os.MkdirAll(Dir(scope), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file(scope), b, 0o600)
}
```

- [ ] **Step 4: Implement the command group and enroll**

`cli/command_agent.go`:

```go
package cli

// commandAgent groups the device-side commands.
type commandAgent struct {
	enroll  commandAgentEnroll
	run     commandAgentRun     // Task 16
	install commandAgentInstall // Task 17
}

func (c *commandAgent) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("agent", "WarpHold agent: this machine as a Fleet-managed device.")
	c.enroll.setup(svc, cmd)
	c.run.setup(svc, cmd)
	c.install.setup(svc, cmd)
}
```
(add empty `commandAgentRun` / `commandAgentInstall` structs with no-op `setup` in `cli/command_agent_run.go` and `cli/command_agent_install.go` until Tasks 16/17.)

`cli/command_agent_enroll.go`:

```go
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
)

type commandAgentEnroll struct {
	server string
	token  string
	scope  string
	name   string
	svc    appServices
	out    textOutput
}

func (c *commandAgentEnroll) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("enroll", "Enroll this machine into a Fleet using an enrollment token.")
	cmd.Flag("server", "Fleet server URL, e.g. https://fleet.example").Required().StringVar(&c.server)
	cmd.Flag("token", "Enrollment token").Required().StringVar(&c.token)
	cmd.Flag("scope", "user (backs up $HOME) or system (root, backs up system paths)").Default("user").EnumVar(&c.scope, "user", "system")
	cmd.Flag("name", "Display name (defaults to hostname)").StringVar(&c.name)
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

type enrollResponse struct {
	AgentID      string `json:"agent_id"`
	Bearer       string `json:"bearer"`
	Name         string `json:"name"`
	ConnectToken string `json:"connect_token"`
	PollInterval int    `json:"poll_interval_seconds"`
}

func (c *commandAgentEnroll) run(ctx context.Context) error {
	host, _ := os.Hostname()
	body, _ := json.Marshal(map[string]string{"token": c.token, "hostname": host, "os": runtime.GOOS, "arch": runtime.GOARCH, "version": repo.BuildVersion, "scope": c.scope})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.server, "/")+"/api/v1/fleet/enroll", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req) // provisioning creates a repo; can take a while on B2
	if err != nil {
		return errors.Wrap(err, "cannot reach the Fleet server")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return errors.Errorf("enrollment refused (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var er enrollResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return errors.Wrap(err, "malformed enrollment response")
	}

	ci, password, err := repo.DecodeToken(er.ConnectToken)
	if err != nil {
		return errors.Wrap(err, "malformed connect token")
	}
	st, err := blob.NewStorage(ctx, ci, false)
	if err != nil {
		return errors.Wrap(err, "cannot open repository storage")
	}
	defer st.Close(ctx) //nolint:errcheck
	if err := os.MkdirAll(state.CacheDir(c.scope), 0o700); err != nil {
		return err
	}
	if err := repo.Connect(ctx, state.RepoConfigPath(c.scope), st, password, &repo.ConnectOptions{
		CachingOptions: content.CachingOptions{CacheDirectory: state.CacheDir(c.scope)},
	}); err != nil {
		return errors.Wrap(err, "cannot connect to repository")
	}
	if err := c.svc.passwordPersistenceStrategy().PersistPassword(ctx, state.RepoConfigPath(c.scope), password); err != nil {
		return errors.Wrap(err, "cannot persist repository password")
	}
	name := c.name
	if name == "" {
		name = er.Name
	}
	if err := state.Save(c.scope, &state.Config{Server: strings.TrimRight(c.server, "/"), AgentID: er.AgentID, Bearer: er.Bearer, Name: name, PollInterval: er.PollInterval, Scope: c.scope}); err != nil {
		return err
	}
	fmt.Fprintf(c.out.stdout(), "Enrolled as %s (%s).\n", name, er.AgentID) //nolint:errcheck
	return nil
}
```
`passwordPersistenceStrategy()` is on `App`; if `appServices` does not expose it, use `advancedAppServices` for `svc`. If `PersistPassword`'s name differs on current master, grep `internal/passwordpersist` for the method that stores a password for a config file and use that. (The persisted password is what lets `agent run` open the repo without a prompt; it lives in the OS keyring or a `0600` file next to the config, upstream's choice.)

- [ ] **Step 5: Run tests**

Run: `go test ./agent/... ./cli/ -run 'TestSaveLoad|TestDirs|TestAgentEnroll' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add agent cli fleet/api/server.go && git commit -m "feat(agent): enroll command and on-disk state"
```

---

### Task 13: Agent poll/report client (`agent/poll`)

**Files:**
- Create: `agent/poll/client.go`
- Test: `agent/poll/client_test.go`

**Interfaces:**
- Produces (the wire types are shared with Fleet; define them here and import from `fleet/api` to avoid drift):

```go
package poll
type Heartbeat struct {
	Version       string `json:"version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	DiskFreeBytes uint64 `json:"disk_free_bytes"`
	RepoConnected bool   `json:"repo_connected"`
	EngineStatus  string `json:"engine_status"`   // idle|uploading|paused
}
type Source struct{ Path string `json:"path"`; Policy json.RawMessage `json:"policy"` }
type Command struct{ ID int64 `json:"id"`; Kind string `json:"kind"`; Source string `json:"source"` }
type PolicyDoc struct {
	ETag                string    `json:"etag"`
	Name                string    `json:"name"`
	Sources             []Source  `json:"sources"`
	Commands            []Command `json:"commands"`
	PollIntervalSeconds int       `json:"poll_interval_seconds"`
}
type Report struct {
	TaskID     string    `json:"task_id"`
	Kind       string    `json:"kind"`      // snapshot|verify|restore|command
	CommandID  int64     `json:"command_id,omitempty"`
	Source     string    `json:"source"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Status     string    `json:"status"`    // ok|error|cancelled
	Bytes      int64     `json:"bytes"`
	Files      int64     `json:"files"`
	SnapshotID string    `json:"snapshot_id,omitempty"`
	Stderr     string    `json:"stderr,omitempty"`
}
type Client struct{ Server, Bearer string; HTTP *http.Client }
var ErrRevoked = errors.New("this agent was revoked by the Fleet server")
func (c *Client) Poll(ctx context.Context, hb Heartbeat, etag string) (*PolicyDoc, error)   // nil,nil on 304; ErrRevoked on 401
func (c *Client) Report(ctx context.Context, r Report) error
func Jitter(base time.Duration) time.Duration   // base ± 60s, never < 30s
```

- [ ] **Step 1: Write the failing test**

`agent/poll/client_test.go`:

```go
package poll_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/poll"
)

func TestPollEtagAndRevoked(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer wa_1", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/v1/fleet/agent/poll":
			json.NewDecoder(r.Body).Decode(&gotBody)
			if gotBody["etag"] == "e1" {
				w.WriteHeader(304)
				return
			}
			if gotBody["etag"] == "revoked" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"etag": "e1", "name": "fw13", "sources": []map[string]any{{"path": "/home/hody", "policy": map[string]any{}}}, "commands": []any{}, "poll_interval_seconds": 300})
		case "/api/v1/fleet/agent/report":
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	c := &poll.Client{Server: srv.URL, Bearer: "wa_1"}
	doc, err := c.Poll(context.Background(), poll.Heartbeat{Version: "0.1.0"}, "")
	require.NoError(t, err)
	require.Equal(t, "e1", doc.ETag)
	require.Equal(t, "/home/hody", doc.Sources[0].Path)
	require.Equal(t, "0.1.0", gotBody["heartbeat"].(map[string]any)["version"])

	doc, err = c.Poll(context.Background(), poll.Heartbeat{}, "e1")
	require.NoError(t, err)
	require.Nil(t, doc, "304 means unchanged")

	_, err = c.Poll(context.Background(), poll.Heartbeat{}, "revoked")
	require.ErrorIs(t, err, poll.ErrRevoked)

	require.NoError(t, c.Report(context.Background(), poll.Report{TaskID: "t1", Kind: "snapshot", Status: "ok", StartedAt: time.Now(), FinishedAt: time.Now()}))
}

func TestJitterBounds(t *testing.T) {
	for i := 0; i < 100; i++ {
		d := poll.Jitter(5 * time.Minute)
		require.True(t, d >= 4*time.Minute && d <= 6*time.Minute, d)
	}
	require.GreaterOrEqual(t, poll.Jitter(10*time.Second), 30*time.Second)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./agent/poll/ -v`
Expected: FAIL, package missing.

- [ ] **Step 3: Implement**

`agent/poll/client.go`:

```go
// Package poll is the agent's client for the Fleet poll/report endpoints.
package poll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

type Heartbeat struct {
	Version       string `json:"version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	DiskFreeBytes uint64 `json:"disk_free_bytes"`
	RepoConnected bool   `json:"repo_connected"`
	EngineStatus  string `json:"engine_status"`
}

type Source struct {
	Path   string          `json:"path"`
	Policy json.RawMessage `json:"policy"`
}

type Command struct {
	ID     int64  `json:"id"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
}

type PolicyDoc struct {
	ETag                string    `json:"etag"`
	Name                string    `json:"name"`
	Sources             []Source  `json:"sources"`
	Commands            []Command `json:"commands"`
	PollIntervalSeconds int       `json:"poll_interval_seconds"`
}

type Report struct {
	TaskID     string    `json:"task_id"`
	Kind       string    `json:"kind"`
	CommandID  int64     `json:"command_id,omitempty"`
	Source     string    `json:"source"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Status     string    `json:"status"`
	Bytes      int64     `json:"bytes"`
	Files      int64     `json:"files"`
	SnapshotID string    `json:"snapshot_id,omitempty"`
	Stderr     string    `json:"stderr,omitempty"`
}

// ErrRevoked means the Fleet no longer accepts this agent's bearer token.
var ErrRevoked = errors.New("this agent was revoked by the Fleet server")

// Client talks to one Fleet server.
type Client struct {
	Server string
	Bearer string
	HTTP   *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *Client) post(ctx context.Context, path string, body any) (*http.Response, []byte, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Server, "/")+path, bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp, raw, nil
}

// Poll sends a heartbeat and returns the policy document, or nil when unchanged.
func (c *Client) Poll(ctx context.Context, hb Heartbeat, etag string) (*PolicyDoc, error) {
	resp, raw, err := c.post(ctx, "/api/v1/fleet/agent/poll", map[string]any{"etag": etag, "heartbeat": hb})
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, nil
	case http.StatusUnauthorized:
		return nil, ErrRevoked
	case http.StatusOK:
		var doc PolicyDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("malformed policy document: %w", err)
		}
		return &doc, nil
	default:
		return nil, fmt.Errorf("fleet returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

// Report posts one finished task.
func (c *Client) Report(ctx context.Context, r Report) error {
	resp, raw, err := c.post(ctx, "/api/v1/fleet/agent/report", r)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrRevoked
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("fleet returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// Jitter returns base ± 60 s, never below 30 s.
func Jitter(base time.Duration) time.Duration {
	d := base + time.Duration(rand.IntN(121)-60)*time.Second
	if d < 30*time.Second {
		return 30 * time.Second
	}
	return d
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./agent/poll/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/poll && git commit -m "feat(agent): poll/report client"
```

---

### Task 14: Fleet poll/report endpoints + health (`fleet/health`, `fleet/api/agent_endpoints.go`)

**Files:**
- Create: `fleet/health/health.go`, `fleet/api/policydoc.go`
- Modify: `fleet/api/agent_endpoints.go` (replace the `mountAgentPoll` stub), `fleet/api/admin_agents.go` (`healthOf`)
- Test: `fleet/health/health_test.go`, `fleet/api/poll_test.go`

**Interfaces:**
- Produces:

```go
package health
const (Green = "green"; Yellow = "yellow"; Red = "red"; Unknown = "unknown")
type Input struct{ LastOK *time.Time; LastRunFailed bool; Revoked bool }
func Status(in Input, now time.Time) string
//   Revoked → "revoked"; no LastOK → Unknown (or Red if LastRunFailed); LastRunFailed → Red; age<26h → Green; age<7d → Yellow; else Red

package api
// POST /api/v1/fleet/agent/poll    Authorization: Bearer <wa_…>
//   body {etag, heartbeat}. 401 if bearer unknown or agent revoked. 304 if etag matches and no pending commands.
//   200 → poll.PolicyDoc; the doc ETag is sha256(template.id + template.policy_json + template.sources + agent.name)[:16]
//   Side effects: TouchAgent(last_seen, version, etag returned).
// POST /api/v1/fleet/agent/report  Authorization: Bearer
//   body poll.Report → 204. Stores report; if command_id set, AckCommand.
func (s *Server) policyDocFor(ctx, a *store.Agent) (*poll.PolicyDoc, error)
```
The doc's `sources[].path` comes from the template with `~` expanded **by the agent** (Task 15), not here; Fleet passes paths verbatim.

- [ ] **Step 1: Write the failing tests**

`fleet/health/health_test.go`:

```go
package health_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/health"
)

func TestStatus(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	require.Equal(t, health.Unknown, health.Status(health.Input{}, now))
	require.Equal(t, health.Red, health.Status(health.Input{LastRunFailed: true}, now))
	require.Equal(t, health.Green, health.Status(health.Input{LastOK: at(2 * time.Hour)}, now))
	require.Equal(t, health.Green, health.Status(health.Input{LastOK: at(25 * time.Hour)}, now))
	require.Equal(t, health.Yellow, health.Status(health.Input{LastOK: at(27 * time.Hour)}, now))
	require.Equal(t, health.Yellow, health.Status(health.Input{LastOK: at(6 * 24 * time.Hour)}, now))
	require.Equal(t, health.Red, health.Status(health.Input{LastOK: at(8 * 24 * time.Hour)}, now))
	require.Equal(t, health.Red, health.Status(health.Input{LastOK: at(time.Hour), LastRunFailed: true}, now))
	require.Equal(t, "revoked", health.Status(health.Input{LastOK: at(time.Hour), Revoked: true}, now))
}
```

`fleet/api/poll_test.go`:

```go
package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/poll"
)

func enrollAgent(t *testing.T, h *harness) (id string, bearer string) {
	t.Helper()
	gid := h.mkGroup(t)
	_, tok := h.do("POST", "/api/v1/fleet/tokens", map[string]any{"group_id": gid})
	admin := h.jar
	h.jar = nil
	resp, body := h.do("POST", "/api/v1/fleet/enroll", map[string]any{"token": tok["token"], "hostname": "fw13", "os": "linux", "arch": "amd64", "version": "0.1.0", "scope": "user"})
	require.Equal(t, 201, resp.StatusCode)
	h.jar = admin
	return body["agent_id"].(string), body["bearer"].(string)
}

func TestPollReportHealth(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()
	id, bearer := enrollAgent(t, h)
	c := &poll.Client{Server: h.srv.URL, Bearer: bearer}
	ctx := t.Context()

	doc, err := c.Poll(ctx, poll.Heartbeat{Version: "0.1.1"}, "")
	require.NoError(t, err)
	require.Equal(t, "fw13", doc.Name)
	require.Equal(t, "~", doc.Sources[0].Path)
	require.JSONEq(t, `{}`, string(doc.Sources[0].Policy))
	require.Equal(t, 300, doc.PollIntervalSeconds)
	require.NotEmpty(t, doc.ETag)

	again, err := c.Poll(ctx, poll.Heartbeat{}, doc.ETag)
	require.NoError(t, err)
	require.Nil(t, again)

	// a pending command breaks the 304
	resp, _ := h.do("POST", "/api/v1/fleet/agents/"+id+"/commands", map[string]any{"kind": "snapshot-now", "source": "~"})
	require.Equal(t, 201, resp.StatusCode)
	withCmd, err := c.Poll(ctx, poll.Heartbeat{}, doc.ETag)
	require.NoError(t, err)
	require.Len(t, withCmd.Commands, 1)

	now := time.Now()
	require.NoError(t, c.Report(ctx, poll.Report{TaskID: "t1", Kind: "command", CommandID: withCmd.Commands[0].ID, Source: "~", StartedAt: now.Add(-time.Minute), FinishedAt: now, Status: "ok", SnapshotID: "k1", Bytes: 5, Files: 1}))
	after, err := c.Poll(ctx, poll.Heartbeat{}, doc.ETag)
	require.NoError(t, err)
	require.Nil(t, after, "command acknowledged, back to 304")

	_, detail := h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	agent := detail["agent"].(map[string]any)
	require.Equal(t, "green", agent["health"])
	require.Equal(t, "0.1.1", agent["version"])
	require.NotNil(t, agent["last_seen_at"])

	require.NoError(t, c.Report(ctx, poll.Report{TaskID: "t2", Kind: "snapshot", Source: "~", StartedAt: now, FinishedAt: now.Add(time.Second), Status: "error", Stderr: "kopia: error: unable to write blob"}))
	_, detail = h.do("GET", "/api/v1/fleet/agents/"+id, nil)
	require.Equal(t, "red", detail["agent"].(map[string]any)["health"])
	reports := detail["reports"].([]any)
	require.Equal(t, "kopia: error: unable to write blob", reports[0].(map[string]any)["Stderr"])

	// template change → new etag
	_, tpls := h.doList("GET", "/api/v1/fleet/templates")
	tplID := tpls[0]["id"].(float64)
	req, _ := http.NewRequest("PUT", h.srv.URL+"/api/v1/fleet/templates/"+jsonNum(tplID), jsonBody(map[string]any{"name": "Home default", "sources": []string{"~", "/etc"}, "policy": map[string]any{"retention": map[string]any{"keepLatest": 3}}}))
	req.Header.Set("Content-Type", "application/json")
	for _, ck := range h.jar {
		req.AddCookie(ck)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, 204, res.StatusCode)
	changed, err := c.Poll(ctx, poll.Heartbeat{}, doc.ETag)
	require.NoError(t, err)
	require.NotNil(t, changed)
	require.Len(t, changed.Sources, 2)

	// revoke → 401
	resp, _ = h.do("POST", "/api/v1/fleet/agents/"+id+"/revoke", nil)
	require.Equal(t, 204, resp.StatusCode)
	_, err = c.Poll(ctx, poll.Heartbeat{}, "")
	require.ErrorIs(t, err, poll.ErrRevoked)
	_ = json.Marshal
}
```
Add tiny helpers to `server_test.go`: `jsonNum(f float64) string` (`strconv.FormatInt(int64(f),10)`) and `jsonBody(v any) io.Reader` (marshal into a `bytes.Reader`).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./fleet/health/ ./fleet/api/ -run 'TestStatus|TestPollReport' -v`
Expected: FAIL.

- [ ] **Step 3: Implement health**

`fleet/health/health.go`:

```go
// Package health turns report history into a traffic light.
package health

import "time"

const (
	Green   = "green"
	Yellow  = "yellow"
	Red     = "red"
	Unknown = "unknown"
	Revoked = "revoked"

	greenFor  = 26 * time.Hour
	yellowFor = 7 * 24 * time.Hour
)

// Input is what health is computed from.
type Input struct {
	LastOK        *time.Time
	LastRunFailed bool
	Revoked       bool
}

// Status returns green/yellow/red/unknown/revoked.
func Status(in Input, now time.Time) string {
	switch {
	case in.Revoked:
		return Revoked
	case in.LastRunFailed:
		return Red
	case in.LastOK == nil:
		return Unknown
	}
	age := now.Sub(*in.LastOK)
	switch {
	case age < greenFor:
		return Green
	case age < yellowFor:
		return Yellow
	default:
		return Red
	}
}
```

- [ ] **Step 4: Implement the policy document and endpoints**

`fleet/api/policydoc.go`:

```go
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/fleet/store"
)

// policyDocFor renders the agent's group template into the wire document (commands are added by the caller).
func (s *Server) policyDocFor(ctx context.Context, a *store.Agent) (*poll.PolicyDoc, error) {
	g, err := s.st.Group(ctx, a.GroupID)
	if err != nil {
		return nil, err
	}
	tpl, err := s.st.Template(ctx, g.TemplateID)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(tpl.ID, 10)))
	h.Write(tpl.PolicyJSON)
	srcJSON, _ := json.Marshal(tpl.Sources)
	h.Write(srcJSON)
	h.Write([]byte(a.Name))
	doc := &poll.PolicyDoc{ETag: hex.EncodeToString(h.Sum(nil))[:16], Name: a.Name, Commands: []poll.Command{}, PollIntervalSeconds: s.pollInterval(ctx)}
	for _, p := range tpl.Sources {
		doc.Sources = append(doc.Sources, poll.Source{Path: p, Policy: tpl.PolicyJSON})
	}
	return doc, nil
}
```

Replace the `mountAgentPoll` stub in `fleet/api/agent_endpoints.go` with:

```go
func (s *Server) mountAgentPoll(m *mux.Router) {
	m.HandleFunc("/api/v1/fleet/agent/poll", s.requireActivated(s.requireAgent(s.handlePoll))).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/agent/report", s.requireActivated(s.requireAgent(s.handleReport))).Methods(http.MethodPost)
}

type agentKey struct{}

// requireAgent authenticates the bearer token and rejects revoked agents.
func (s *Server) requireAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer == "" || bearer == r.Header.Get("Authorization") {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		a, err := s.st.AgentByBearerHash(r.Context(), enroll.HashToken(bearer))
		if err != nil || a.RevokedAt != nil {
			writeErr(w, http.StatusUnauthorized, "unknown or revoked agent")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, a)))
	}
}

func agentFrom(r *http.Request) *store.Agent { return r.Context().Value(agentKey{}).(*store.Agent) }

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	var in struct {
		ETag      string         `json:"etag"`
		Heartbeat poll.Heartbeat `json:"heartbeat"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	ctx := r.Context()
	doc, err := s.policyDocFor(ctx, a)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	version := in.Heartbeat.Version
	if version == "" {
		version = a.Version
	}
	_ = s.st.TouchAgent(ctx, a.ID, s.now(), version, doc.ETag)
	pending, _ := s.st.PendingCommands(ctx, a.ID)
	for _, c := range pending {
		doc.Commands = append(doc.Commands, poll.Command{ID: c.ID, Kind: c.Kind, Source: c.Source})
	}
	if in.ETag == doc.ETag && len(pending) == 0 {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	var in poll.Report
	if err := decode(r, &in); err != nil || in.TaskID == "" || in.Kind == "" || in.Status == "" {
		writeErr(w, http.StatusBadRequest, "task_id, kind and status are required")
		return
	}
	ctx := r.Context()
	if _, err := s.st.AddReport(ctx, &store.Report{AgentID: a.ID, TaskID: in.TaskID, Kind: in.Kind, Source: in.Source, StartedAt: in.StartedAt, FinishedAt: in.FinishedAt, Status: in.Status, Bytes: in.Bytes, Files: in.Files, SnapshotID: in.SnapshotID, Stderr: in.Stderr}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if in.CommandID != 0 {
		_ = s.st.AckCommand(ctx, in.CommandID, s.now())
	}
	_ = s.st.TouchAgent(ctx, a.ID, s.now(), a.Version, a.PolicyETag)
	w.WriteHeader(http.StatusNoContent)
}
```
(add `"github.com/kopia/kopia/agent/poll"` to that file's imports.)

Replace `healthOf` in `fleet/api/admin_agents.go`:

```go
func (s *Server) healthOf(a store.Agent, latest *store.Report) string {
	in := health.Input{Revoked: a.RevokedAt != nil}
	if latest != nil {
		in.LastRunFailed = latest.Status == "error"
	}
	if ok, err := s.st.LastOKReport(context.Background(), a.ID); err == nil && ok != nil {
		t := ok.FinishedAt
		in.LastOK = &t
	}
	return health.Status(in, s.now())
}
```
and add to `fleet/store/reports.go`:

```go
// LastOKReport returns the newest successful snapshot report for an agent, or nil.
func (s *Store) LastOKReport(ctx context.Context, agentID string) (*Report, error) {
	r, err := scanReport(s.db.QueryRowContext(ctx, `SELECT `+reportCols+` FROM reports WHERE agent_id=? AND status='ok' AND kind IN ('snapshot','command') ORDER BY finished_at DESC, id DESC LIMIT 1`, agentID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return r, err
}
```
(import `errors` there.)

- [ ] **Step 5: Run tests**

Run: `go test ./fleet/... -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add fleet && git commit -m "feat(fleet): agent poll/report endpoints, ETag policy docs, health computation"
```

---

### Task 15: Headless engine + local apply (`agent/engine`)

**Files:**
- Create: `agent/engine/headless.go`, `agent/engine/local.go`
- Test: `agent/engine/engine_test.go`

**Interfaces:**
- Produces:

```go
package engine
// Headless runs Kopia's server engine on loopback with no static UI, CSRF off, one random user.
type Headless struct{ BaseURL, User, Password string /* private: srv *server.Server, http *http.Server */ }
func StartHeadless(ctx context.Context, configFile, repoPassword, prefsDir string) (*Headless, error)
func (h *Headless) Client() (*apiclient.KopiaAPIClient, error)
func (h *Headless) Stop(ctx context.Context) error

// Local drives a Headless (or any Kopia server) through the HTTP API.
type Local struct{ API *apiclient.KopiaAPIClient; Host, User string }   // Host/User = the SourceInfo identity of this machine
func NewLocal(api *apiclient.KopiaAPIClient) (*Local, error)            // reads /api/v1/sources → LocalHost/LocalUsername
func ExpandHome(p string) string                                       // "~" or "~/x" → $HOME…
func (l *Local) Apply(ctx context.Context, sources []poll.Source) error // POST /api/v1/sources per path (createSnapshot=false) + PUT /api/v1/policy; DELETE policy for sources no longer listed
func (l *Local) Snapshot(ctx context.Context, path string) error        // POST /api/v1/sources/upload?userName&host&path
func (l *Local) Pause(ctx, path) error / Resume(ctx, path) error         // POST /api/v1/control/pause-source|resume-source (control API)
func (l *Local) Sources(ctx) ([]*serverapi.SourceStatus, error)
func (l *Local) Tasks(ctx) ([]uitask.Info, error)                        // GET /api/v1/tasks
func (l *Local) TaskLog(ctx, id string) (string, error)                  // GET /api/v1/tasks/{id}/logs → joined lines (raw)
func ToReport(t uitask.Info, source string) poll.Report                  // maps SUCCESS/FAILED/CANCELED → ok/error/cancelled; Kind "snapshot" when t.Kind=="Snapshot" else strings.ToLower(t.Kind); Bytes from counters["Hashed Bytes"]+["Cached Bytes"] if present; Files from counters["Hashed Files"]+["Cached Files"]
```

- [ ] **Step 1: Write the failing test**

`agent/engine/engine_test.go`:

```go
package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/internal/uitask"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
)

// provisionedRepo makes a filesystem repo the way Fleet does and connects a config to it.
func provisionedRepo(t *testing.T) (configFile, password string) {
	t.Helper()
	ctx := context.Background()
	p := &enroll.Provisioner{Owner: "fleet@test"}
	b, err := p.Provision(ctx, enroll.TargetSpec{Kind: "filesystem", Path: t.TempDir()}, "ag_e")
	require.NoError(t, err)
	ci, pw, err := repo.DecodeToken(b.ConnectToken)
	require.NoError(t, err)
	st, err := blob.NewStorage(ctx, ci, false)
	require.NoError(t, err)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "repository.config")
	require.NoError(t, repo.Connect(ctx, cfg, st, pw, &repo.ConnectOptions{CachingOptions: content.CachingOptions{CacheDirectory: filepath.Join(dir, "cache")}}))
	return cfg, pw
}

func TestApplySnapshotAndReport(t *testing.T) {
	ctx := context.Background()
	cfg, pw := provisionedRepo(t)
	h, err := engine.StartHeadless(ctx, cfg, pw, t.TempDir())
	require.NoError(t, err)
	defer h.Stop(ctx)
	api, err := h.Client()
	require.NoError(t, err)
	l, err := engine.NewLocal(api)
	require.NoError(t, err)
	require.NotEmpty(t, l.Host)

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o600))
	pol, _ := json.Marshal(map[string]any{"retention": map[string]any{"keepLatest": 2}, "scheduling": map[string]any{"manual": true}})
	require.NoError(t, l.Apply(ctx, []poll.Source{{Path: src, Policy: pol}}))

	// the policy landed in the repository
	r, err := repo.Open(ctx, cfg, pw, nil)
	require.NoError(t, err)
	got, err := policy.GetDefinedPolicy(ctx, r, snapshot.SourceInfo{Host: l.Host, UserName: l.User, Path: src})
	require.NoError(t, err)
	require.EqualValues(t, 2, *got.RetentionPolicy.KeepLatest)
	r.Close(ctx)

	require.NoError(t, l.Snapshot(ctx, src))
	var done []uitask.Info
	require.Eventually(t, func() bool {
		tasks, err := l.Tasks(ctx)
		if err != nil {
			return false
		}
		done = nil
		for _, tk := range tasks {
			if tk.EndTime != nil && tk.Kind == "Snapshot" {
				done = append(done, tk)
			}
		}
		return len(done) == 1
	}, 60*time.Second, 200*time.Millisecond)
	rep := engine.ToReport(done[0], src)
	require.Equal(t, "ok", rep.Status)
	require.Equal(t, "snapshot", rep.Kind)
	require.Equal(t, src, rep.Source)
	require.Greater(t, rep.Files, int64(0))

	// removing the source deletes its policy
	require.NoError(t, l.Apply(ctx, nil))
	r, _ = repo.Open(ctx, cfg, pw, nil)
	_, err = policy.GetDefinedPolicy(ctx, r, snapshot.SourceInfo{Host: l.Host, UserName: l.User, Path: src})
	require.ErrorIs(t, err, policy.ErrPolicyNotFound)
	r.Close(ctx)

	require.Equal(t, filepath.Join(os.Getenv("HOME"), "x"), engine.ExpandHome("~/x"))
}
```
(`internal/uitask` is importable from anywhere inside this module, tests included.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./agent/engine/ -v`
Expected: FAIL, package missing.

- [ ] **Step 3: Implement headless.go**

`agent/engine/headless.go`:

```go
// Package engine runs Kopia's server engine headless and drives it over its HTTP API.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/auth"
	"github.com/kopia/kopia/internal/passwordpersist"
	"github.com/kopia/kopia/internal/server"
	"github.com/kopia/kopia/repo"
)

const headlessUser = "warphold-agent"

// Headless is Kopia's engine on loopback: scheduler, uploads, tasks; no static UI.
type Headless struct {
	BaseURL  string
	User     string
	Password string

	srv  *server.Server
	http *http.Server
	ln   net.Listener
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// StartHeadless opens the repository at configFile and serves the control + UI API on 127.0.0.1:0.
func StartHeadless(ctx context.Context, configFile, repoPassword, prefsDir string) (*Headless, error) {
	h := &Headless{User: headlessUser, Password: randomHex(32)}
	srv, err := server.New(ctx, &server.Options{
		ConfigFile:             configFile,
		ConnectOptions:         &repo.ConnectOptions{},
		RefreshInterval:        4 * time.Hour,
		Authenticator:          auth.AuthenticateSingleUser(h.User, h.Password),
		Authorizer:             auth.DefaultAuthorizer(),
		PasswordPersist:        passwordpersist.None(),
		UIUser:                 h.User,
		ServerControlUser:      h.User,
		UIPreferencesFile:      filepath.Join(prefsDir, "ui-preferences.json"),
		DisableCSRFTokenChecks: true, // loopback + random per-process password; CSRF is for browsers
		MinMaintenanceInterval: 24 * time.Hour,
	})
	if err != nil {
		return nil, errors.Wrap(err, "server.New")
	}
	h.srv = srv
	open := func(ctx context.Context) (repo.Repository, error) {
		return repo.Open(ctx, configFile, repoPassword, &repo.Options{})
	}
	if _, err := srv.InitRepositoryAsync(ctx, "Open", open, true); err != nil {
		return nil, errors.Wrap(err, "open repository")
	}
	m := mux.NewRouter()
	srv.SetupControlAPIHandlers(m)
	srv.SetupHTMLUIAPIHandlers(m)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	h.ln = ln
	h.BaseURL = "http://" + ln.Addr().String()
	h.http = &http.Server{Handler: m, ReadHeaderTimeout: 15 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { _ = h.http.Serve(ln) }()
	return h, nil
}

// Client returns an API client authenticated as the headless user.
func (h *Headless) Client() (*apiclient.KopiaAPIClient, error) {
	return apiclient.NewKopiaAPIClient(apiclient.Options{BaseURL: h.BaseURL, Username: h.User, Password: h.Password})
}

// Stop shuts the HTTP server and disconnects the repository.
func (h *Headless) Stop(ctx context.Context) error {
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := h.http.Shutdown(ctx2)
	return errors.Join(err, h.srv.SetRepository(ctx, nil))
}
```
If `passwordpersist.None()` does not exist, grep `internal/passwordpersist` for the no-op strategy (`passwordpersist.None` / `NoPersist`) and use it; the engine never needs to persist because the password is passed in.

- [ ] **Step 4: Implement local.go**

`agent/engine/local.go`:

```go
package engine

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/serverapi"
	"github.com/kopia/kopia/internal/uitask"
	"github.com/kopia/kopia/snapshot/policy"
)

// Local drives a Kopia server for this machine's sources.
type Local struct {
	API  *apiclient.KopiaAPIClient
	Host string
	User string
}

// NewLocal learns the local host/user identity from the server.
func NewLocal(api *apiclient.KopiaAPIClient) (*Local, error) {
	var sr serverapi.SourcesResponse
	if err := api.Get(context.Background(), "sources", nil, &sr); err != nil {
		return nil, errors.Wrap(err, "sources")
	}
	return &Local{API: api, Host: sr.LocalHost, User: sr.LocalUsername}, nil
}

// ExpandHome turns "~" and "~/x" into absolute paths.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func (l *Local) sourceQuery(path string) string {
	q := url.Values{}
	q.Set("userName", l.User)
	q.Set("host", l.Host)
	q.Set("path", path)
	return q.Encode()
}

// Sources lists the server's sources.
func (l *Local) Sources(ctx context.Context) ([]*serverapi.SourceStatus, error) {
	var sr serverapi.SourcesResponse
	if err := l.API.Get(ctx, "sources", nil, &sr); err != nil {
		return nil, err
	}
	return sr.Sources, nil
}

// Apply makes the server's source set and policies match the document.
func (l *Local) Apply(ctx context.Context, sources []poll.Source) error {
	want := map[string]bool{}
	for _, s := range sources {
		path := ExpandHome(s.Path)
		want[path] = true
		var pol policy.Policy
		if len(s.Policy) > 0 {
			if err := json.Unmarshal(s.Policy, &pol); err != nil {
				return errors.Wrapf(err, "policy for %s", s.Path)
			}
		}
		var resp serverapi.CreateSnapshotSourceResponse
		if err := l.API.Post(ctx, "sources", &serverapi.CreateSnapshotSourceRequest{Path: path, CreateSnapshot: false, Policy: &pol}, &resp); err != nil {
			return errors.Wrapf(err, "add source %s", path)
		}
		if err := l.API.Put(ctx, "policy?"+l.sourceQuery(path), &pol, &serverapi.Empty{}); err != nil {
			return errors.Wrapf(err, "set policy %s", path)
		}
	}
	existing, err := l.Sources(ctx)
	if err != nil {
		return err
	}
	for _, s := range existing {
		if s.Source.Host != l.Host || s.Source.UserName != l.User || want[s.Source.Path] {
			continue
		}
		if err := l.API.Delete(ctx, "policy?"+l.sourceQuery(s.Source.Path), nil, nil, &serverapi.Empty{}); err != nil {
			return errors.Wrapf(err, "remove policy %s", s.Source.Path)
		}
	}
	return l.API.Post(ctx, "refresh", &serverapi.Empty{}, &serverapi.Empty{})
}

// Snapshot starts a snapshot of path now.
func (l *Local) Snapshot(ctx context.Context, path string) error {
	var resp serverapi.MultipleSourceActionResponse
	return l.API.Post(ctx, "sources/upload?"+l.sourceQuery(ExpandHome(path)), &serverapi.Empty{}, &resp)
}

// Pause pauses scheduled snapshots of path (all sources when path is empty).
func (l *Local) Pause(ctx context.Context, path string) error {
	return l.sourceAction(ctx, "control/pause-source", path)
}

// Resume resumes scheduled snapshots of path.
func (l *Local) Resume(ctx context.Context, path string) error {
	return l.sourceAction(ctx, "control/resume-source", path)
}

func (l *Local) sourceAction(ctx context.Context, op, path string) error {
	var resp serverapi.MultipleSourceActionResponse
	q := ""
	if path != "" {
		q = "?" + l.sourceQuery(ExpandHome(path))
	}
	return l.API.Post(ctx, op+q, &serverapi.Empty{}, &resp)
}

// Tasks lists all tasks the server knows about.
func (l *Local) Tasks(ctx context.Context) ([]uitask.Info, error) {
	var tr serverapi.TaskListResponse
	if err := l.API.Get(ctx, "tasks", nil, &tr); err != nil {
		return nil, err
	}
	return tr.Tasks, nil
}

// TaskLog returns the raw log lines of a task, newline-joined.
func (l *Local) TaskLog(ctx context.Context, id string) (string, error) {
	var out struct {
		Logs []json.RawMessage `json:"logs"`
	}
	if err := l.API.Get(ctx, "tasks/"+id+"/logs", nil, &out); err != nil {
		return "", err
	}
	lines := make([]string, 0, len(out.Logs))
	for _, raw := range out.Logs {
		var e struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Msg != "" {
			lines = append(lines, e.Msg)
		} else {
			lines = append(lines, string(raw))
		}
	}
	return strings.Join(lines, "\n"), nil
}

func counter(t uitask.Info, names ...string) int64 {
	var n int64
	for _, name := range names {
		if c, ok := t.Counters[name]; ok {
			n += c.Value
		}
	}
	return n
}

// ToReport converts a finished task into a Fleet report.
func ToReport(t uitask.Info, source string) poll.Report {
	r := poll.Report{TaskID: t.TaskID, Source: source, StartedAt: t.StartTime, Stderr: t.ErrorMessage}
	if t.EndTime != nil {
		r.FinishedAt = *t.EndTime
	}
	switch t.Status {
	case uitask.StatusSuccess:
		r.Status = "ok"
	case uitask.StatusCanceled:
		r.Status = "cancelled"
	default:
		r.Status = "error"
	}
	if t.Kind == "Snapshot" {
		r.Kind = "snapshot"
	} else {
		r.Kind = strings.ToLower(t.Kind)
	}
	r.Bytes = counter(t, "Hashed Bytes", "Cached Bytes")
	r.Files = counter(t, "Hashed Files", "Cached Files")
	return r
}
```
Check the exact counter names by running one snapshot and printing `t.Counters` in the test; upstream's upload counters are named in `internal/uitask` consumers (`snapshot/upload/upload_progress.go`). Adjust the two strings, not the shape. The task's `Description` contains the path (`"Snapshot user@host:/path"`); when a report needs the source and the caller does not know it, parse it from there.

- [ ] **Step 5: Run tests**

Run: `go test ./agent/engine/ -v -run TestApply`
Expected: PASS. If `sources/upload` returns 400 because the query needs a different parameter name, read `internal/server/api_snapshots.go:handleUpload` and match it; do not change the server.

- [ ] **Step 6: Commit**

```bash
git add agent/engine && git commit -m "feat(agent): headless Kopia engine and local API driver"
```

---

### Task 16: `agent run` (poll loop + task watcher)

**Files:**
- Create: `agent/run/loop.go`, `cli/command_agent_run.go` (replace stub)
- Test: `agent/run/loop_test.go`, `cli/command_agent_run_test.go`

**Interfaces:**
- Produces:

```go
package run
type Deps struct {
	Fleet   *poll.Client
	Local   LocalEngine                          // interface satisfied by *engine.Local (Apply, Snapshot, Pause, Resume, Tasks, TaskLog, Status)
	State   *state.Config                        // Scope + ETag persisted here
	Now     func() time.Time
	Log     func(format string, args ...any)
}
type Loop struct{ d Deps; seen map[string]bool /* reported task IDs */ }
func New(d Deps) *Loop
func (l *Loop) PollOnce(ctx context.Context) error   // heartbeat → poll → Apply on new ETag → execute commands → report command results
func (l *Loop) WatchOnce(ctx context.Context) error  // list tasks; report every finished, unseen task; remember it
func (l *Loop) Run(ctx context.Context, once bool) error  // once: PollOnce + WatchOnce then return; else loop: poll every Jitter(interval), watch every 10 s; return poll.ErrRevoked when revoked
```
- `warphold agent run [--scope user|system] [--once]`: loads state, gets the repo password via the app's password-persistence strategy for `state.RepoConfigPath(scope)`, `StartHeadless`, builds `Local`, runs the loop, stops the engine on exit. Exit code 3 on `ErrRevoked` (systemd `RestartPreventExitStatus=3`, Task 17).

- [ ] **Step 1: Write the failing tests**

`agent/run/loop_test.go` (fake Fleet via httptest, fake Local via a small interface):

```go
package run_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/agent/run"
	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/uitask"
)

type fakeLocal struct {
	mu       sync.Mutex
	applied  [][]poll.Source
	snapshot []string
	tasks    []uitask.Info
}

func (f *fakeLocal) Apply(_ context.Context, s []poll.Source) error { f.mu.Lock(); defer f.mu.Unlock(); f.applied = append(f.applied, s); return nil }
func (f *fakeLocal) Snapshot(_ context.Context, p string) error   { f.mu.Lock(); defer f.mu.Unlock(); f.snapshot = append(f.snapshot, p); return nil }
func (f *fakeLocal) Pause(context.Context, string) error          { return nil }
func (f *fakeLocal) Resume(context.Context, string) error         { return nil }
func (f *fakeLocal) Tasks(context.Context) ([]uitask.Info, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.tasks, nil }
func (f *fakeLocal) TaskLog(context.Context, string) (string, error) { return "log", nil }
func (f *fakeLocal) Status(context.Context) (string, bool) { return "idle", true }

func TestPollAppliesOnNewEtagAndRunsCommands(t *testing.T) {
	var reports []poll.Report
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/fleet/agent/poll":
			polls++
			var in struct{ ETag string `json:"etag"` }
			json.NewDecoder(r.Body).Decode(&in)
			if in.ETag == "e1" && polls > 1 {
				w.WriteHeader(304)
				return
			}
			json.NewEncoder(w).Encode(poll.PolicyDoc{ETag: "e1", Name: "fw13", Sources: []poll.Source{{Path: "/data", Policy: json.RawMessage(`{}`)}}, Commands: []poll.Command{{ID: 7, Kind: "snapshot-now", Source: "/data"}}, PollIntervalSeconds: 300})
		case "/api/v1/fleet/agent/report":
			var rep poll.Report
			json.NewDecoder(r.Body).Decode(&rep)
			reports = append(reports, rep)
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	t.Setenv("WARPHOLD_STATE_DIR", t.TempDir())
	st := &state.Config{Server: srv.URL, Bearer: "wa_1", Scope: "user"}
	fl := &fakeLocal{}
	l := run.New(run.Deps{Fleet: &poll.Client{Server: srv.URL, Bearer: "wa_1"}, Local: fl, State: st, Now: time.Now, Log: t.Logf})

	require.NoError(t, l.PollOnce(context.Background()))
	require.Len(t, fl.applied, 1)
	require.Equal(t, []string{"/data"}, fl.snapshot)
	require.Len(t, reports, 1)
	require.EqualValues(t, 7, reports[0].CommandID)
	require.Equal(t, "ok", reports[0].Status)
	saved, _ := state.Load("user")
	require.Equal(t, "e1", saved.ETag)

	require.NoError(t, l.PollOnce(context.Background()))
	require.Len(t, fl.applied, 1, "304 → no re-apply")
}

func TestWatchReportsFinishedTasksOnce(t *testing.T) {
	var reports []poll.Report
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep poll.Report
		json.NewDecoder(r.Body).Decode(&rep)
		reports = append(reports, rep)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	end := time.Now()
	fl := &fakeLocal{tasks: []uitask.Info{
		{TaskID: "t1", Kind: "Snapshot", Description: "Snapshot hody@fw13:/data", StartTime: end.Add(-time.Minute), EndTime: &end, Status: uitask.StatusSuccess},
		{TaskID: "t2", Kind: "Snapshot", Description: "Snapshot hody@fw13:/data", StartTime: end, Status: uitask.StatusRunning},
		{TaskID: "t3", Kind: "Maintenance", Description: "Maintenance", StartTime: end.Add(-time.Minute), EndTime: &end, Status: uitask.StatusFailed, ErrorMessage: "kopia: error: boom"},
	}}
	l := run.New(run.Deps{Fleet: &poll.Client{Server: srv.URL, Bearer: "wa_1"}, Local: fl, State: &state.Config{Scope: "user"}, Now: time.Now, Log: t.Logf})
	require.NoError(t, l.WatchOnce(context.Background()))
	require.Len(t, reports, 2)
	require.Equal(t, "/data", reports[0].Source)
	require.Equal(t, "error", reports[1].Status)
	require.Equal(t, "kopia: error: boom", reports[1].Stderr)
	require.NoError(t, l.WatchOnce(context.Background()))
	require.Len(t, reports, 2, "already reported")
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./agent/run/ -v`
Expected: FAIL, package missing.

- [ ] **Step 3: Implement the loop**

`agent/run/loop.go`:

```go
// Package run is the agent's main loop: poll Fleet, apply policy, report tasks.
package run

import (
	"context"
	"errors"
	"regexp"
	"runtime"
	"strconv"
	"time"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/uitask"
	"github.com/kopia/kopia/repo"
)

// LocalEngine is what the loop needs from engine.Local (interface so tests can fake it).
type LocalEngine interface {
	Apply(ctx context.Context, sources []poll.Source) error
	Snapshot(ctx context.Context, path string) error
	Pause(ctx context.Context, path string) error
	Resume(ctx context.Context, path string) error
	Tasks(ctx context.Context) ([]uitask.Info, error)
	TaskLog(ctx context.Context, id string) (string, error)
	Status(ctx context.Context) (engineStatus string, repoConnected bool)
}

// Deps wires the loop.
type Deps struct {
	Fleet *poll.Client
	Local LocalEngine
	State *state.Config
	Now   func() time.Time
	Log   func(format string, args ...any)
}

// Loop is the agent's control loop.
type Loop struct {
	d    Deps
	seen map[string]bool
}

// New creates a Loop.
func New(d Deps) *Loop {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = func(string, ...any) {}
	}
	return &Loop{d: d, seen: map[string]bool{}}
}

func (l *Loop) heartbeat(ctx context.Context) poll.Heartbeat {
	status, connected := l.d.Local.Status(ctx)
	return poll.Heartbeat{Version: repo.BuildVersion, OS: runtime.GOOS, Arch: runtime.GOARCH, RepoConnected: connected, EngineStatus: status}
}

// PollOnce does one poll cycle.
func (l *Loop) PollOnce(ctx context.Context) error {
	doc, err := l.d.Fleet.Poll(ctx, l.heartbeat(ctx), l.d.State.ETag)
	if err != nil {
		return err
	}
	if doc == nil {
		return nil
	}
	if doc.ETag != l.d.State.ETag {
		if err := l.d.Local.Apply(ctx, doc.Sources); err != nil {
			return err
		}
		l.d.State.ETag = doc.ETag
		if doc.PollIntervalSeconds > 0 {
			l.d.State.PollInterval = doc.PollIntervalSeconds
		}
		if err := state.Save(l.d.State.Scope, l.d.State); err != nil {
			l.d.Log("cannot save state: %v", err)
		}
	}
	for _, c := range doc.Commands {
		started := l.d.Now()
		var cerr error
		switch c.Kind {
		case "snapshot-now":
			cerr = l.d.Local.Snapshot(ctx, c.Source)
		case "pause":
			cerr = l.d.Local.Pause(ctx, c.Source)
		case "resume":
			cerr = l.d.Local.Resume(ctx, c.Source)
		case "verify":
			cerr = errors.New("verify runs from the Fleet server in this version") // Plan 3 (M7)
		default:
			cerr = errors.New("unknown command " + c.Kind)
		}
		rep := poll.Report{TaskID: "cmd-" + strconv.FormatInt(c.ID, 10), Kind: "command", CommandID: c.ID, Source: c.Source, StartedAt: started, FinishedAt: l.d.Now(), Status: "ok"}
		if cerr != nil {
			rep.Status, rep.Stderr = "error", cerr.Error()
		}
		if err := l.d.Fleet.Report(ctx, rep); err != nil {
			return err
		}
	}
	return nil
}

var descPath = regexp.MustCompile(`^Snapshot [^:]+:(.+)$`)

// WatchOnce reports every finished task not yet reported.
func (l *Loop) WatchOnce(ctx context.Context) error {
	tasks, err := l.d.Local.Tasks(ctx)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.EndTime == nil || l.seen[t.TaskID] {
			continue
		}
		source := ""
		if m := descPath.FindStringSubmatch(t.Description); m != nil {
			source = m[1]
		}
		rep := engine.ToReport(t, source)
		if rep.Status == "error" && rep.Stderr == "" {
			rep.Stderr, _ = l.d.Local.TaskLog(ctx, t.TaskID)
		}
		if err := l.d.Fleet.Report(ctx, rep); err != nil {
			return err
		}
		l.seen[t.TaskID] = true
	}
	return nil
}

// Run loops until ctx ends or the agent is revoked. once = one poll + one watch.
func (l *Loop) Run(ctx context.Context, once bool) error {
	if once {
		if err := l.PollOnce(ctx); err != nil {
			return err
		}
		return l.WatchOnce(ctx)
	}
	interval := time.Duration(l.d.State.PollInterval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	pollT := time.NewTimer(0)
	watchT := time.NewTicker(10 * time.Second)
	defer pollT.Stop()
	defer watchT.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pollT.C:
			if err := l.PollOnce(ctx); err != nil {
				if errors.Is(err, poll.ErrRevoked) {
					return err
				}
				l.d.Log("poll: %v", err)
			}
			interval = time.Duration(l.d.State.PollInterval) * time.Second
			pollT.Reset(poll.Jitter(interval))
		case <-watchT.C:
			if err := l.WatchOnce(ctx); err != nil {
				if errors.Is(err, poll.ErrRevoked) {
					return err
				}
				l.d.Log("watch: %v", err)
			}
		}
	}
}
```
No import cycle: engine imports poll; run imports engine, poll and state. Add `Status(ctx) (string, bool)` to `engine.Local`: `Sources()` succeeds → connected `true`; status `"uploading"` if any source has `CurrentTask != ""`, `"paused"` if all are `PAUSED`, else `"idle"`.

- [ ] **Step 4: Implement the command**

`cli/command_agent_run.go`:

```go
package cli

import (
	"context"
	"os"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/agent/run"
	"github.com/kopia/kopia/agent/state"
)

type commandAgentRun struct {
	scope string
	once  bool
	svc   advancedAppServices
	out   textOutput
}

func (c *commandAgentRun) setup(svc advancedAppServices, parent commandParent) {
	cmd := parent.Command("run", "Run the agent: back up on schedule and report to the Fleet server.")
	cmd.Flag("scope", "user or system").Default("user").EnumVar(&c.scope, "user", "system")
	cmd.Flag("once", "Poll once, report once, exit (for tests and cron).").Hidden().BoolVar(&c.once)
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandAgentRun) run(ctx context.Context) error {
	st, err := state.Load(c.scope)
	if err != nil {
		return errors.Wrap(err, "not enrolled; run 'warphold agent enroll' first")
	}
	cfg := state.RepoConfigPath(c.scope)
	password, err := c.svc.passwordPersistenceStrategy().GetPassword(ctx, cfg)
	if err != nil {
		return errors.Wrap(err, "repository password not found; re-enroll")
	}
	h, err := engine.StartHeadless(ctx, cfg, password, state.Dir(c.scope))
	if err != nil {
		return err
	}
	defer h.Stop(context.WithoutCancel(ctx)) //nolint:errcheck
	api, err := h.Client()
	if err != nil {
		return err
	}
	local, err := engine.NewLocal(api)
	if err != nil {
		return err
	}
	loop := run.New(run.Deps{Fleet: &poll.Client{Server: st.Server, Bearer: st.Bearer}, Local: local, State: st, Log: log(ctx).Warnf})
	err = loop.Run(ctx, c.once)
	if errors.Is(err, poll.ErrRevoked) {
		log(ctx).Error("this agent was revoked by the Fleet server; not restarting")
		os.Exit(3) //nolint:forbidigo
	}
	return err
}
```
`GetPassword` is the read method of upstream's `passwordpersist.Strategy`; confirm the name in `internal/passwordpersist/passwordpersist.go`.

- [ ] **Step 5: End-to-end test through the CLI**

`cli/command_agent_run_test.go`:

```go
package cli_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/tests/testenv"
)

func TestAgentRunOnceAppliesPolicyAndReports(t *testing.T) {
	url, tok := fleetForTest(t)
	stateDir := t.TempDir()
	t.Setenv("WARPHOLD_STATE_DIR", stateDir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "note.txt"), []byte("x"), 0o600))

	runner := testenv.NewInProcRunner(t)
	e := testenv.NewCLITest(t, nil, runner)
	e.RunAndExpectSuccess(t, "agent", "enroll", "--server", url, "--token", tok)
	e.RunAndExpectSuccess(t, "agent", "run", "--once")

	st, err := state.Load("user")
	require.NoError(t, err)
	require.NotEmpty(t, st.ETag)

	pw := passwordFor(t, e, state.RepoConfigPath("user"))
	r, err := repo.Open(context.Background(), state.RepoConfigPath("user"), pw, nil)
	require.NoError(t, err)
	defer r.Close(context.Background())
	pols, err := policy.ListPolicies(context.Background(), r)
	require.NoError(t, err)
	var found bool
	for _, p := range pols {
		if p.Target.Path == home {
			found = true
			require.EqualValues(t, 3, *p.RetentionPolicy.KeepLatest)
		}
	}
	require.True(t, found, "template source '~' expanded to HOME and applied")
	_ = snapshot.SourceInfo{}
	_ = http.StatusOK
}
```
`passwordFor` reads the persisted password the same way `agent run` does (call `passwordpersist` directly with the strategy the in-proc runner uses; the runner sets `KOPIA_PASSWORD`-free file persistence under its own config dir, so read `filepath.Join(filepath.Dir(cfg), ...)` per `internal/passwordpersist/file.go`). If that proves fiddly, assert instead through the Fleet API that the agent's `last_seen_at` is set and `policy_etag` equals `st.ETag` (expose `AgentForTesting(ctx, id)` on `api.Server`), which is what `--once` guarantees.

- [ ] **Step 6: Run tests**

Run: `go test ./agent/... ./cli/ -run 'TestPollApplies|TestWatchReports|TestAgentRunOnce' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add agent cli && git commit -m "feat(agent): run loop with policy apply, command execution, task reporting"
```

---

### Task 17: `agent install` (systemd user unit, linger)

**Files:**
- Create: `agent/install/systemd.go`, `cli/command_agent_install.go` (replace stub)
- Test: `agent/install/systemd_test.go`

**Interfaces:**
- Produces:

```go
package install
type Plan struct{ Files map[string]string; Commands [][]string }   // path → contents; commands to run in order
func Systemd(scope, binary string) Plan
// user:   ~/.config/systemd/user/warphold-agent.service ; commands: systemctl --user daemon-reload; systemctl --user enable --now warphold-agent; loginctl enable-linger <user>
// system: /etc/systemd/system/warphold-agent.service   ; commands: systemctl daemon-reload; systemctl enable --now warphold-agent
func Apply(p Plan, runCmd func(name string, args ...string) error) error   // writes files 0644 (dirs 0755) then runs commands
```
Unit content (user scope):

```ini
[Unit]
Description=WarpHold backup agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart={{binary}} agent run --scope user
Restart=on-failure
RestartSec=30
RestartPreventExitStatus=3
Nice=19
IOSchedulingClass=idle

[Install]
WantedBy=default.target
```
System scope: same with `--scope system`, `WantedBy=multi-user.target`, and `User=root` implied.

- [ ] **Step 1: Write the failing test**

`agent/install/systemd_test.go`:

```go
package install_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/install"
)

func TestSystemdPlans(t *testing.T) {
	t.Setenv("HOME", "/home/hody")
	t.Setenv("XDG_CONFIG_HOME", "")
	p := install.Systemd("user", "/home/hody/.local/bin/warphold")
	unit, ok := p.Files["/home/hody/.config/systemd/user/warphold-agent.service"]
	require.True(t, ok)
	require.Contains(t, unit, "ExecStart=/home/hody/.local/bin/warphold agent run --scope user")
	require.Contains(t, unit, "RestartPreventExitStatus=3")
	require.Contains(t, unit, "WantedBy=default.target")
	require.Equal(t, [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", "warphold-agent"}, {"loginctl", "enable-linger"}}, p.Commands)

	s := install.Systemd("system", "/usr/local/bin/warphold")
	unit = s.Files["/etc/systemd/system/warphold-agent.service"]
	require.Contains(t, unit, "--scope system")
	require.Contains(t, unit, "WantedBy=multi-user.target")
	require.Equal(t, [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", "warphold-agent"}}, s.Commands)
}

func TestApplyWritesAndRuns(t *testing.T) {
	dir := t.TempDir()
	p := install.Plan{Files: map[string]string{filepath.Join(dir, "a", "x.service"): "unit"}, Commands: [][]string{{"systemctl", "daemon-reload"}}}
	var ran []string
	require.NoError(t, install.Apply(p, func(name string, args ...string) error { ran = append(ran, name+" "+strings.Join(args, " ")); return nil }))
	require.Equal(t, []string{"systemctl daemon-reload"}, ran)
	require.FileExists(t, filepath.Join(dir, "a", "x.service"))
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./agent/install/ -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`agent/install/systemd.go`:

```go
// Package install writes service units so the agent runs at boot.
package install

import (
	"os"
	"path/filepath"
	"strings"
)

// Plan is what an install will do, so it can be printed (--dry-run) or applied.
type Plan struct {
	Files    map[string]string
	Commands [][]string
}

const unitTmpl = `[Unit]
Description=WarpHold backup agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s agent run --scope %s
Restart=on-failure
RestartSec=30
RestartPreventExitStatus=3
Nice=19
IOSchedulingClass=idle

[Install]
WantedBy=%s
`

// Systemd returns the plan for a scope.
func Systemd(scope, binary string) Plan {
	if scope == "system" {
		return Plan{
			Files:    map[string]string{"/etc/systemd/system/warphold-agent.service": unit(binary, "system", "multi-user.target")},
			Commands: [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", "warphold-agent"}},
		}
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		home, _ := os.UserHomeDir()
		cfg = filepath.Join(home, ".config")
	}
	return Plan{
		Files:    map[string]string{filepath.Join(cfg, "systemd", "user", "warphold-agent.service"): unit(binary, "user", "default.target")},
		Commands: [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", "warphold-agent"}, {"loginctl", "enable-linger"}},
	}
}

func unit(binary, scope, wantedBy string) string {
	return strings.TrimSpace(sprintf(unitTmpl, binary, scope, wantedBy)) + "\n"
}

// Apply writes the files then runs the commands.
func Apply(p Plan, runCmd func(name string, args ...string) error) error {
	for path, content := range p.Files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	for _, c := range p.Commands {
		if err := runCmd(c[0], c[1:]...); err != nil {
			return err
		}
	}
	return nil
}
```
(`sprintf` is `fmt.Sprintf`; import `fmt`.)

`cli/command_agent_install.go`:

```go
package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/kopia/kopia/agent/install"
)

type commandAgentInstall struct {
	scope  string
	dryRun bool
	svc    appServices
	out    textOutput
}

func (c *commandAgentInstall) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("install", "Install the agent as a systemd service that starts at boot.")
	cmd.Flag("scope", "user or system").Default("user").EnumVar(&c.scope, "user", "system")
	cmd.Flag("dry-run", "Print what would be written and run").BoolVar(&c.dryRun)
	c.svc = svc
	c.out.setup(svc)
	cmd.Action(svc.noRepositoryAction(c.run))
}

func (c *commandAgentInstall) run(_ context.Context) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	p := install.Systemd(c.scope, bin)
	if c.dryRun {
		for path, content := range p.Files {
			fmt.Fprintf(c.out.stdout(), "--- %s\n%s\n", path, content) //nolint:errcheck
		}
		for _, cmd := range p.Commands {
			fmt.Fprintln(c.out.stdout(), "$", cmd) //nolint:errcheck
		}
		return nil
	}
	return install.Apply(p, func(name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Stdout, cmd.Stderr = c.out.stdout(), c.out.stderr()
		return cmd.Run()
	})
}
```

- [ ] **Step 4: Run tests and a dry run**

```bash
go test ./agent/install/ -v && CGO_ENABLED=0 go build -o dist/warphold . && dist/warphold agent install --dry-run
```
Expected: tests PASS; dry run prints the unit and three commands.

- [ ] **Step 5: Commit**

```bash
git add agent/install cli/command_agent_install.go && git commit -m "feat(agent): systemd install with linger and no-restart on revoke"
```

---

### Task 18: Live verification on real machines, then docs and vault

**Files:**
- Modify: `docs/superpowers/specs/2026-09-01-warphold-core-fleet-design.md` §3.2 (four touched files, hook described)
- Create: `README.md` section "Fleet quick start" (append to upstream README, above "Contributing")
- Modify (vault): `~/.dev/hody/Projects/WarpHold/WarpHold — Project Brief.md` status, `~/.dev/hody/00-Log.md` (INGEST line), Claude memory `warphold-project.md`

- [ ] **Step 1: Run the whole suite**

```bash
CGO_ENABLED=0 go build ./... && go test ./fleet/... ./agent/... ./cli/ -run 'Fleet|Agent|Status|Login|Activation|Admin|Enroll|Poll|Watch|Systemd|Apply|Token|Provision|Seal|Store' -count=1
```
Expected: all PASS.

- [ ] **Step 2: Live check with two real machines (Working Rule #4)**

On the homelab server that will host Fleet (a Proxmox VM; create it per the homelab VM pipeline if none exists yet), with a filesystem target for this milestone:

```bash
warphold --config-file /var/lib/warphold/repository.config fleet activate --email hody@hody.dev
warphold --config-file /var/lib/warphold/repository.config server start --insecure --without-password --address 0.0.0.0:51515 --ui=false --grpc=false
```
Then from the laptop, create target/template/group/token through the API with curl (session cookie from `POST /api/v1/fleet/session`), and:

```bash
curl -fsSL http://<fleet>:51515/enroll.sh?token=<T> | sed 's#curl -fsSL .*/dl/.*#cp ./dist/warphold "$BIN/warphold"#' | sh
systemctl --user status warphold-agent
curl -s -b cookies http://<fleet>:51515/api/v1/fleet/agents | jq
```
Expected, shown in the report before saying "done": the agent's `health` turns `green` after the first snapshot, `last_seen_at` is recent, and `POST /agents/{id}/commands {kind:"snapshot-now"}` produces a new report within a minute. HTTPS in front of the Fleet server (Traefik/step-ca) is Plan 2's job; note that the bearer token travels in the clear until then and only use this on the LAN.

- [ ] **Step 3: Docs**

Spec §3.2: change "Upstream files touched, and nothing else:" to the four-file list from Global Constraints above with one line on the `setupHandlers` hook. README "Fleet quick start": the three commands from Step 2 plus the enroll one-liner, and the plain statement that the Fleet admin can decrypt every device's backups.

- [ ] **Step 4: Vault ingest**

Project brief status line: "M0–M3 built <date>: fleet activation, enrollment, agent run/install; live-verified on <server> + <laptop>." Prepend an `INGEST` line to `hody/00-Log.md` naming the plan file, the four upstream touches, and the live-check result. Update Claude memory `warphold-project.md`: repo cloned at `~/.dev/Projects/warphold`, Go via mise, what works, what Plan 2 covers.

- [ ] **Step 5: Commit and push**

```bash
git add -A && git commit -m "docs: plan 1 complete; fleet quick start; spec touch list" && git push
```

---

## Deferred to later plans (from the spec)

| Spec item | Plan |
|---|---|
| `warphold-ui` fork, UI import swap, design system, Fleet screens, Activate wizard UI, agent-mode UI, `/dl/` binary route, HTTPS in front of Fleet, release pipeline | Plan 2 (M4–M5 + tray) |
| Tray (`fyne.io/systray`), XDG autostart, desktop notification on red | Plan 2 |
| Recovery kit HTML, kit_acks endpoints, nag banner, escrow regenerate | Plan 3 (M6) |
| verify / test-restore / maintenance / digest jobs, `verify` command on agents, SMTP settings | Plan 3 (M7) |
| Restyle of single-user screens | Plan 4 (M8) |
| Standalone-restore CI test with the pinned upstream `kopia` binary, README restore runbook, vault triad | Plan 4 (M9) |
| Multi-use installer download per group (needs `/dl/`) | Plan 2 |
