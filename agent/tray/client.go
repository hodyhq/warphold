package tray

import (
	"context"
	stderrors "errors"
	"io/fs"
	"net/url"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/agent/engine"
	"github.com/kopia/kopia/agent/state"
	"github.com/kopia/kopia/internal/apiclient"
	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/uitask"
)

// Options configures the tray.
type Options struct {
	// Scope is the agent scope whose engine.json and agent.json to read.
	Scope string
	// Poll is the engine poll interval; zero means the default 5s.
	Poll time.Duration
}

// errorWindow is how far back "Errors this week" counts.
const errorWindow = 7 * 24 * time.Hour

// client is the tray's connection to the locally running engine. It caches
// the engine.Local built from engine.json and drops it whenever the engine
// goes away, so a restarted agent (new port, new credentials) is picked up on
// the next poll.
type client struct {
	scope string

	baseURL string
	local   *engine.Local
}

// vault is the label the menu shows for this machine. It reads agent.json,
// which holds no secret worth withholding here - only the agent's name. The
// Fleet group is not part of the enrollment the agent stores, so the label is
// the agent name alone until the enroll response carries one.
func (c *client) vault() string {
	cfg, err := state.Load(c.scope)
	if err != nil {
		return VaultLabel("", "")
	}

	return VaultLabel("", cfg.Name)
}

// connect returns the engine connection, or nil when no engine is running.
func (c *client) connect(ctx context.Context) (*engine.Local, error) {
	info, err := engine.ReadInfo(c.scope)
	if err != nil {
		c.forget()

		if stderrors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}

		return nil, errors.Wrap(err, "unable to read engine.json")
	}

	if c.local != nil && c.baseURL == info.BaseURL {
		return c.local, nil
	}

	api, err := apiclient.NewKopiaAPIClient(apiclient.Options{BaseURL: info.BaseURL, Username: info.User, Password: info.Password})
	if err != nil {
		return nil, errors.Wrap(err, "bad engine address in engine.json")
	}

	local, err := engine.NewLocal(ctx, api)
	if err != nil {
		// engine.json points at a loopback port; nothing answering there means
		// the process that wrote it is gone.
		return nil, nil //nolint:nilerr
	}

	c.local, c.baseURL = local, info.BaseURL

	return local, nil
}

func (c *client) forget() { c.local, c.baseURL = nil, "" }

// status polls the engine once. A stopped agent is not an error - it is the
// "Agent not running" model, and the tray keeps polling at its normal
// interval. A returned error means the poll itself failed (an unreadable
// engine.json, an engine that stopped answering), which is what the caller
// backs off on.
func (c *client) status(ctx context.Context) (Status, error) {
	s := Status{Vault: c.vault()}

	local, err := c.connect(ctx)
	if err != nil {
		return s, err
	}

	if local == nil {
		return s, nil
	}

	sources, err := local.Sources(ctx)
	if err != nil {
		c.forget()

		return s, errors.Wrap(err, "the agent engine is not reachable")
	}

	s.Running, s.Sources = true, sources
	s.ErrorsThisWeek = c.recentErrors(ctx, local)

	return s, nil
}

// recentErrors counts snapshot tasks that failed inside the error window. The
// engine keeps its task list in memory, so a restarted agent starts the count
// from zero - the Fleet server, not the tray, is the durable record.
func (c *client) recentErrors(ctx context.Context, local *engine.Local) int {
	tasks, err := local.Tasks(ctx)
	if err != nil {
		return 0
	}

	cutoff := clock.Now().Add(-errorWindow)

	var n int

	for _, t := range tasks {
		if t.Status != uitask.StatusFailed || t.Kind != "Snapshot" {
			continue
		}

		if t.EndTime != nil && t.EndTime.Before(cutoff) {
			continue
		}

		n++
	}

	return n
}

// detailsURL is the one place the engine's local token is used: it goes into
// the URL handed to the browser and is never logged or shown in the menu.
//
// The URL does become the argv of the xdg-open child, which is world-readable
// in /proc for the moment it runs. That is accepted: the token only unlocks a
// loopback engine, and any process that could read that argv could read
// engine.json itself.
func (c *client) detailsURL() (string, error) {
	info, err := engine.ReadInfo(c.scope)
	if err != nil {
		return "", errors.Wrap(err, "the agent engine is not running")
	}

	return info.BaseURL + "/local/session?t=" + url.QueryEscape(info.LocalToken), nil
}

// backupNow starts a snapshot of every configured source.
func (c *client) backupNow(ctx context.Context) error {
	local, err := c.connect(ctx)
	if err != nil || local == nil {
		return errors.New("the agent engine is not running")
	}

	sources, err := local.Sources(ctx)
	if err != nil {
		return err
	}

	paths := make([]string, 0, len(sources))
	for _, s := range sources {
		paths = append(paths, s.Source.Path)
	}

	return snapshotAll(paths, func(path string) error { return local.Snapshot(ctx, path) })
}

// snapshotAll starts a snapshot of every path and reports every failure: one
// source the engine refuses (a path that went away, a policy error) must not
// stop the rest of the machine from backing up.
func snapshotAll(paths []string, snapshot func(string) error) error {
	var errs []error

	for _, p := range paths {
		if err := snapshot(p); err != nil {
			errs = append(errs, errors.Wrapf(err, "unable to back up %s", p))
		}
	}

	return stderrors.Join(errs...)
}

// setPaused pauses or resumes every source.
func (c *client) setPaused(ctx context.Context, paused bool) error {
	local, err := c.connect(ctx)
	if err != nil || local == nil {
		return errors.New("the agent engine is not running")
	}

	if paused {
		return local.Pause(ctx, "")
	}

	return local.Resume(ctx, "")
}
