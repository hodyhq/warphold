package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
)

// fleetUser is the Fleet's Kopia username. Together with the host it forms the
// maintenance owner ("fleet@<host>", spec §7.1): agents run with
// --no-auto-maintenance, so the Fleet is the only process that maintains a
// device repository, exactly once per repository.
const fleetUser = "fleet"

// publicURLSetting is the setting the Fleet's own hostname comes from; it is
// the same key fleet/api writes. Kept here rather than imported because
// fleet/api imports this package, not the other way round.
const publicURLSetting = "public_url"

// logf writes one line to the Fleet log. The jobs row is what an admin reads;
// the log is for what does not belong in a row.
func logf(format string, args ...any) { log.Printf("warphold fleet: "+format, args...) }

// openedRepo is a repository handle plus the scratch directory holding the
// config file it had to be connected through. Kopia can only open a repository
// from a config file, so every job builds a throwaway one.
type openedRepo struct {
	repo.Repository

	tmp string
}

// close releases the repository and the scratch directory. It takes its own
// context because the job's may already be cancelled.
func (o *openedRepo) close(ctx context.Context) {
	if err := o.Repository.Close(ctx); err != nil {
		logf("closing a device repository: %v", err)
	}

	if err := os.RemoveAll(o.tmp); err != nil {
		logf("removing a job scratch directory: %v", err)
	}
}

// openAgentRepo opens one agent's repository from the bundle Fleet escrowed at
// enrollment: the sealed bundle holds the connect token, and the token carries
// the connection info and the repository password (fleet/enroll). This is the
// only credential path - jobs never ask an agent for anything.
//
// The connection is uncached (an empty ConnectOptions means no content cache),
// so a job leaves nothing behind on the Fleet server but the scratch config it
// removes on close.
func openAgentRepo(ctx context.Context, k seal.Key, a store.Agent, id identity, readOnly bool) (*openedRepo, error) {
	plain, err := k.Open(a.SealedBundle)
	if err != nil {
		// Never the underlying error: it is about the sealing key, and the
		// row is shown in the UI.
		return nil, errors.New("unsealing the escrowed bundle failed")
	}

	var b enroll.Bundle
	if err := json.Unmarshal(plain, &b); err != nil {
		return nil, errors.New("the escrowed bundle is malformed")
	}

	ci, password, err := repo.DecodeToken(b.ConnectToken)
	if err != nil {
		// The token embeds the password, so its text never reaches the row.
		return nil, errors.New("the escrowed connect token is malformed")
	}

	tmp, err := os.MkdirTemp("", "warphold-job-*")
	if err != nil {
		return nil, fmt.Errorf("scratch directory: %w", err)
	}

	bst, err := blob.NewStorage(ctx, ci, false)
	if err != nil {
		os.RemoveAll(tmp) //nolint:errcheck // best effort on an error path

		return nil, fmt.Errorf("opening the repository storage: %w", err)
	}

	cfg := filepath.Join(tmp, "repository.config")
	err = repo.Connect(ctx, cfg, bst, password, &repo.ConnectOptions{
		ClientOptions: repo.ClientOptions{Username: id.user, Hostname: id.host, ReadOnly: readOnly},
	})

	// repo.Open builds its own storage from the config file, so this one has
	// done its job either way.
	bst.Close(ctx) //nolint:errcheck // read side only

	if err != nil {
		os.RemoveAll(tmp) //nolint:errcheck

		return nil, fmt.Errorf("connecting to the repository: %w", err)
	}

	rep, err := repo.Open(ctx, cfg, password, &repo.Options{})
	if err != nil {
		os.RemoveAll(tmp) //nolint:errcheck

		return nil, fmt.Errorf("opening the repository: %w", err)
	}

	return &openedRepo{Repository: rep, tmp: tmp}, nil
}

// identity is who the Fleet is to a device repository.
type identity struct{ user, host string }

func (i identity) owner() string { return i.user + "@" + i.host }

// fleetIdentity is the Fleet's Kopia identity: the host half comes from
// public_url when it is set, so the owner string survives the server moving,
// and falls back to the OS hostname - which is what fleet/enroll stamps into a
// repository it provisions.
func fleetIdentity(ctx context.Context, st *store.Store) identity {
	host, _ := os.Hostname()

	if v, err := st.Setting(ctx, publicURLSetting); err == nil && v != "" {
		if u, err := url.Parse(v); err == nil && u.Hostname() != "" {
			host = u.Hostname()
		}
	}

	if host == "" {
		host = "fleet"
	}

	return identity{user: fleetUser, host: host}
}

// jobAgents is the set of agents one job covers: the agent named on the row,
// or every live agent for a fleet-wide run. A revoked agent is skipped - its
// repository is the reap job's business, not verify's.
func jobAgents(ctx context.Context, st *store.Store, j store.Job) ([]store.Agent, error) {
	if j.AgentID != "" {
		a, err := st.Agent(ctx, j.AgentID)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", j.AgentID, err)
		}

		return []store.Agent{*a}, nil
	}

	all, err := st.Agents(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing agents: %w", err)
	}

	live := all[:0]

	for _, a := range all {
		if a.RevokedAt == nil {
			live = append(live, a)
		}
	}

	return live, nil
}

// sweep accumulates one fleet-wide run: what it covered, and the first
// failures in the shape the jobs UI renders (spec §7, "the real error").
type sweep struct {
	verb  string
	total int
	ok    int
	extra string
	errs  []string
}

func (s *sweep) fail(who string, err error) {
	s.errs = append(s.errs, who+": "+err.Error())
}

// detail is the row's detail string: counts first, then the first failures.
func (s *sweep) detail() string {
	d := fmt.Sprintf("%s %d/%d ok; %d failed", s.verb, s.ok, s.total, len(s.errs))
	if s.extra != "" {
		d += ", " + s.extra
	}

	if len(s.errs) == 0 {
		return d
	}

	shown := s.errs
	if len(shown) > detailErrors {
		shown = shown[:detailErrors]
	}

	d += ": " + strings.Join(shown, "; ")
	if n := len(s.errs) - len(shown); n > 0 {
		d += fmt.Sprintf(" (+%d more)", n)
	}

	return d
}

func (s *sweep) err() error {
	if len(s.errs) == 0 {
		return nil
	}

	return errors.New(s.verb + " did not complete for every device")
}

// perAgent is the shape all three repository jobs share: open each agent's
// repository, do one thing to it, and keep going when a device fails.
func perAgent(st *store.Store, k seal.Key, verb string, readOnly bool, fn func(context.Context, repo.Repository, store.Agent) error) Runner {
	return func(ctx context.Context, j store.Job) (string, error) {
		s := &sweep{verb: verb}

		agents, err := jobAgents(ctx, st, j)
		if err != nil {
			return "", err
		}

		id := fleetIdentity(ctx, st)

		for _, a := range agents {
			if err := ctx.Err(); err != nil {
				// Shutting down or out of time. Say so rather than reporting
				// a partial sweep as a clean one.
				s.fail(verb, err)

				break
			}

			s.total++

			// One device at a time, each on its own clock: an unreachable
			// provider must not spend the whole job's timeout on device one.
			dctx, cancel := context.WithTimeout(ctx, deviceDeadline)

			r, err := openAgentRepo(dctx, k, a, id, readOnly)
			if err != nil {
				cancel()
				s.fail(a.ID, err)

				continue
			}

			err = fn(dctx, r.Repository, a)

			// The close context is detached: the repository still has to be
			// released when the job ran out of its timeout.
			r.close(context.WithoutCancel(ctx))
			cancel()

			if err != nil {
				s.fail(a.ID, err)

				continue
			}

			s.ok++
		}

		return s.detail(), s.err()
	}
}

// deviceDeadline is how long one device gets inside a fleet-wide sweep. The
// scheduler's per-job timeout still bounds the whole run.
const deviceDeadline = 30 * time.Minute

// runnerFor is every job kind the Fleet server runs. It is the one list:
// Runners builds them, HasKind validates a request against it.
var runnerFor = map[string]func(*store.Store, seal.Key) Runner{
	"mirror":       Mirror,
	"verify":       Verify,
	"test-restore": TestRestore,
	"maintenance":  Maintenance,
	"reap":         Reap,
}

// Runners is every runner, keyed by kind. The scheduler enqueues the
// interval-driven kinds itself (see intervals); the jobs API can enqueue any
// of them, for one agent or fleet-wide, on demand.
func Runners(st *store.Store, k seal.Key) map[string]Runner {
	out := make(map[string]Runner, len(runnerFor))
	for kind, make := range runnerFor {
		out[kind] = make(st, k)
	}

	return out
}

// HasKind reports whether kind is a job this Fleet knows how to run.
func HasKind(kind string) bool { _, ok := runnerFor[kind]; return ok }

// KindList is every kind, sorted - the API uses it to say what it accepts.
func KindList() []string {
	out := make([]string, 0, len(runnerFor))
	for kind := range runnerFor {
		out = append(out, kind)
	}

	sort.Strings(out)

	return out
}
