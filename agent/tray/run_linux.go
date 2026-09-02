//go:build linux

package tray

import (
	"context"
	"os/exec"
	"time"

	"fyne.io/systray"
	"github.com/pkg/errors"
)

// Poll cadence: every 5 seconds while the engine answers, backing off to at
// most a minute while it does not, so a stopped agent costs one failed
// connection a minute instead of twelve.
const (
	defaultPoll = 5 * time.Second
	maxPoll     = 60 * time.Second
)

// actionTimeout bounds a menu action's call into the engine, and pollTimeout
// one status poll, so a wedged engine cannot leave the menu unresponsive or
// stop the tray from quitting.
const (
	actionTimeout = 30 * time.Second
	pollTimeout   = 10 * time.Second
)

// Run shows the tray and blocks until the user picks "Quit tray" or ctx is
// canceled.
func Run(ctx context.Context, opts Options) error {
	if opts.Poll <= 0 {
		opts.Poll = defaultPoll
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	t := &tray{c: &client{scope: opts.Scope}, opts: opts, cancel: cancel}

	// The menu is built synchronously inside onReady: the host asks for the
	// layout as soon as onReady returns, and a menu built later from the poll
	// goroutine would show up empty on that first GetLayout.
	onReady := func() {
		t.render(Build(Status{Vault: VaultLabel("", "")}, time.Now()))

		go t.loop(ctx)
	}

	// systray.Run blocks on the desktop's event loop and returns once
	// systray.Quit is called, which onExit and the loop below both trigger.
	systray.Run(onReady, cancel)

	return nil
}

// tray owns the menu items; every field is touched only by loop.
type tray struct {
	c      *client
	opts   Options
	cancel context.CancelFunc
	items  map[Kind]*systray.MenuItem
	last   Model
	watch  ToneWatcher
}

// build creates the menu once, in model order. systray has no way to insert
// an item later, so every kind is created up front and shown or hidden as the
// model says.
func (t *tray) build(m Model) {
	t.items = map[Kind]*systray.MenuItem{}

	for _, it := range m.Items {
		switch it.Kind {
		case KindSep1, KindSep2:
			systray.AddSeparator()
		default:
			t.items[it.Kind] = systray.AddMenuItem(it.Label, "")
		}
	}
}

// render applies only what changed since the last model. Every systray call
// emits a LayoutUpdated signal to the host panel, so relabelling twelve
// unchanged items every five seconds is ~30 signals a poll for nothing.
func (t *tray) render(m Model) {
	first := t.items == nil
	if first {
		t.build(m)
	}

	if first || m.Tone != t.last.Tone {
		if icon, err := Icon(m.Tone, panelIconSize); err == nil {
			systray.SetIcon(icon)
		}
	}

	if first || m.Tooltip != t.last.Tooltip {
		systray.SetTooltip(m.Tooltip)
	}

	for i, it := range m.Items {
		mi := t.items[it.Kind]
		if mi == nil {
			continue
		}

		prev, known := t.prevItem(i, it.Kind)

		if !known || it.Label != prev.Label {
			mi.SetTitle(it.Label)
		}

		if !known || it.Hidden != prev.Hidden {
			if it.Hidden {
				mi.Hide()
			} else {
				mi.Show()
			}
		}

		if !known || it.Enabled != prev.Enabled {
			if it.Enabled {
				mi.Enable()
			} else {
				mi.Disable()
			}
		}
	}

	t.last = m
}

// prevItem returns what was last rendered at this position. Build returns the
// same kinds in the same order on every poll, so the position is enough; the
// kind is checked anyway so a future model change degrades into a full
// re-render rather than mislabelled items.
func (t *tray) prevItem(i int, k Kind) (Item, bool) {
	if i < len(t.last.Items) && t.last.Items[i].Kind == k {
		return t.last.Items[i], true
	}

	return Item{}, false
}

// loop is the tray's only goroutine: it polls, renders, and serves the menu
// until the context is canceled or "Quit tray" is picked. Quitting cancels
// the context, which ends this goroutine and returns systray.Run.
func (t *tray) loop(ctx context.Context) {
	defer systray.Quit()

	// The first render creates the menu, so poll once before waiting.
	m, paused, _ := t.poll(ctx) //nolint:errcheck

	t.render(m)

	interval := t.opts.Poll
	timer := time.NewTimer(interval)

	defer timer.Stop()

	for {
		var err error

		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			var perr error

			m, paused, perr = t.poll(ctx)
			t.render(m)
			t.maybeNotify(m)

			if perr != nil {
				interval = min(2*interval, maxPoll)
			} else {
				interval = t.opts.Poll
			}

			timer.Reset(interval)

			continue

		case <-t.items[KindBackupNow].ClickedCh:
			err = t.withTimeout(ctx, t.c.backupNow)

		case <-t.items[KindPauseResume].ClickedCh:
			want := !paused
			err = t.withTimeout(ctx, func(ctx context.Context) error { return t.c.setPaused(ctx, want) })

		case <-t.items[KindErrors].ClickedCh:
			err = t.openDetails()

		case <-t.items[KindDetails].ClickedCh:
			err = t.openDetails()

		case <-t.items[KindStartAgent].ClickedCh:
			err = t.withTimeout(ctx, t.startAgent)

		case <-t.items[KindQuit].ClickedCh:
			return
		}

		if err != nil {
			_ = notify("WarpHold", err.Error())
		}

		// An action changes what the next menu should say, so poll now
		// instead of waiting out the rest of the interval.
		timer.Reset(100 * time.Millisecond)
	}
}

// poll asks the engine for its state under a bounded context and renders the
// answer into a model. The returned error means the poll failed, which the
// loop backs off on; a simply stopped agent is not an error.
func (t *tray) poll(ctx context.Context) (Model, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	s, err := t.c.status(ctx)

	return Build(s, time.Now()), allPaused(s), err
}

func (t *tray) withTimeout(ctx context.Context, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, actionTimeout)
	defer cancel()

	return f(ctx)
}

// maybeNotify raises one desktop notification per failure, never repeating
// while the failure persists.
func (t *tray) maybeNotify(m Model) {
	if !t.watch.Notify(m.Tone) {
		return
	}

	_ = notify("WarpHold backup failed", "A backup on this machine failed. Open Details for the log.")
}

// openDetails hands the browser the engine's local session URL. That URL
// carries the engine's local token, so it goes to xdg-open and nowhere else -
// it is never logged or put in a notification.
func (t *tray) openDetails() error {
	u, err := t.c.detailsURL()
	if err != nil {
		return err
	}

	cmd := exec.Command("xdg-open", u) //nolint:gosec
	if err := cmd.Start(); err != nil {
		return errors.New("unable to open the details page in a browser")
	}

	// Reap xdg-open once it has handed off to the browser, so a tray that
	// runs for weeks does not collect a zombie per click.
	go func() { _ = cmd.Wait() }()

	return nil
}

// startAgent starts the systemd unit 'agent install' wrote.
func (t *tray) startAgent(ctx context.Context) error {
	args := []string{"--user", "start", "warphold-agent"}
	if t.opts.Scope == "system" {
		args = []string{"start", "warphold-agent"}
	}

	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return errors.Wrapf(err, "unable to start the agent: %s", string(out))
	}

	return nil
}
