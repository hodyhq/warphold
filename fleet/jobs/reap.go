package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
)

// RevokedRetentionSetting is how long a revoked device's repository is kept
// before the reap job removes it, in whole days.
//
// This package owns the setting name and its bounds, and fleet/api validates
// writes against them (fleet/api already imports fleet/jobs, so the rule can
// only live on this side of the edge). One clamp, one place: a bad row must
// not mean two different retention windows depending on who read it.
const RevokedRetentionSetting = "revoked_retention_days"

const (
	// DefaultRetentionDays is deliberately generous: a revocation is often a
	// mistake or a rebuild, and the data is gone for good afterwards.
	DefaultRetentionDays = 30
	// MinRetentionDays is the floor: zero would destroy the repository before
	// anyone could say the revocation was a mistake.
	MinRetentionDays = 1
	MaxRetentionDays = 3650
)

// RevokedRetentionDays reads the retention window in whole days, falling back
// to the default for an unset, unparsable or out-of-range value: a bad row
// must not shorten the window a device's repository is protected by.
func RevokedRetentionDays(ctx context.Context, st *store.Store) int {
	v, err := st.Setting(ctx, RevokedRetentionSetting)
	if err != nil || v == "" {
		return DefaultRetentionDays
	}

	if n, err := strconv.Atoi(v); err == nil && n >= MinRetentionDays && n <= MaxRetentionDays {
		return n
	}

	return DefaultRetentionDays
}

// errNoLocalData is the "nothing here to delete" case: the device's bytes live
// in a bucket this job does not touch (a B2 target's Object Lock would refuse
// anyway, and the device's keys are revoked).
var errNoLocalData = errors.New("no local repository directory")

// Reap returns the runner for the "reap" job (D6): once a revoked device has
// been revoked for longer than revoked_retention_days, remove its repository
// directory and its gateway keys, and stamp retired_at.
//
// It is a sweep, not a per-agent trigger, so it also catches devices revoked
// before the scheduler existed; a job that names an agent reaps only that one.
// It is idempotent: an already retired agent, a directory that is already
// gone, and an agent that has been un-revoked are all no-ops.
func Reap(st *store.Store, k seal.Key) Runner {
	return func(ctx context.Context, j store.Job) (string, error) {
		s := &sweep{verb: "reaped"}
		now := time.Now()
		window := retentionWindow(ctx, st)

		agents, err := reapCandidates(ctx, st, j)
		if err != nil {
			return "", err
		}

		var waiting, kept int

		for _, a := range agents {
			if err := ctx.Err(); err != nil {
				s.fail("reap", err)

				break
			}

			if now.Sub(*a.RevokedAt) < window {
				waiting++

				continue
			}

			s.total++

			switch err := reapAgent(ctx, st, a, now); {
			case errors.Is(err, errNoLocalData):
				kept++

				s.ok++
			case err != nil:
				s.fail(a.ID, err)
			default:
				s.ok++
			}
		}

		var notes []string
		if waiting > 0 {
			notes = append(notes, strconv.Itoa(waiting)+" still inside the retention window")
		}

		if kept > 0 {
			notes = append(notes, strconv.Itoa(kept)+" with no local repository")
		}

		s.extra = strings.Join(notes, ", ")

		return s.detail(), s.err()
	}
}

// reapCandidates is every revoked, not yet retired agent - or the one the job
// names, which must still be revoked: reaping a device that was brought back
// would delete a live repository.
func reapCandidates(ctx context.Context, st *store.Store, j store.Job) ([]store.Agent, error) {
	if j.AgentID != "" {
		a, err := st.Agent(ctx, j.AgentID)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", j.AgentID, err)
		}

		if a.RevokedAt == nil {
			return nil, fmt.Errorf("agent %s is not revoked", a.ID)
		}

		if a.RetiredAt != nil {
			return nil, nil
		}

		return []store.Agent{*a}, nil
	}

	all, err := st.Agents(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing agents: %w", err)
	}

	out := all[:0]

	for _, a := range all {
		if a.RevokedAt != nil && a.RetiredAt == nil {
			out = append(out, a)
		}
	}

	return out, nil
}

// reapAgent removes one device's repository and credentials, then retires it.
// The order matters: the credential dies first, the bytes second, and the row
// is only stamped once both are gone, so a failure leaves the job to retry.
func reapAgent(ctx context.Context, st *store.Store, a store.Agent, now time.Time) error {
	dir, dirErr := agentRepoDir(ctx, st, a)
	if dirErr != nil && !errors.Is(dirErr, errNoLocalData) {
		return dirErr
	}

	if _, err := st.DeleteDeviceKeysForAgent(ctx, a.ID); err != nil {
		return fmt.Errorf("deleting the gateway keys: %w", err)
	}

	if dirErr == nil {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("removing the repository: %w", err)
		}
	}

	if err := st.RetireAgent(ctx, a.ID, now); err != nil {
		return fmt.Errorf("recording the retirement: %w", err)
	}

	return dirErr
}

// agentRepoDir is where an agent's repository lives on this server, and it is
// the only path the reap ever unlinks. Both layouts are the ones that created
// them: fleet/enroll's "<path>/agents/<id>" for a filesystem target, and the
// gateway's flat "<path>/<id>" for a hosted disk target.
func agentRepoDir(ctx context.Context, st *store.Store, a store.Agent) (string, error) {
	// The id is server-minted, but this unlinks a tree: check it rather than
	// trust it.
	if !enroll.IsSafeAgentDir(a.ID) {
		return "", fmt.Errorf("refusing to reap agent id %q", a.ID)
	}

	g, err := st.Group(ctx, a.GroupID)
	if err != nil {
		return "", fmt.Errorf("the agent's group: %w", err)
	}

	t, err := st.Target(ctx, g.TargetID)
	if err != nil {
		return "", fmt.Errorf("the group's target: %w", err)
	}

	switch {
	case t.Kind == "filesystem":
		if t.Path == "" {
			// Without this an empty path would make "agents/<id>" relative to
			// the server's working directory.
			return "", errors.New("the target has no path")
		}

		return filepath.Join(t.Path, "agents", a.ID), nil

	case t.Kind == "hosted" && t.StorageMode == "disk":
		if t.Path == "" {
			return "", errors.New("the target has no path")
		}

		return filepath.Join(t.Path, a.ID), nil

	default:
		return "", errNoLocalData
	}
}

// retentionWindow is revoked_retention_days as a duration. Anything unset,
// unparsable or out of range falls back to the default - a bad value must
// never shorten the window.
func retentionWindow(ctx context.Context, st *store.Store) time.Duration {
	return time.Duration(RevokedRetentionDays(ctx, st)) * 24 * time.Hour
}
