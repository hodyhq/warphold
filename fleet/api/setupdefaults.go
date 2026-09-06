package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/kopia/kopia/fleet/store"
)

const (
	// The names a fresh install starts with. They are ordinary rows: an
	// operator can rename or delete any of them afterwards.
	setupTargetName   = "Fleet disk"
	setupTemplateName = "Home"
	setupGroupName    = "Devices"

	// setupHostedRootMode is 0750, not 0700: the directory holds one
	// subdirectory per device and only the service account writes there, but
	// the group bit lets an operator add a backup-reader account without
	// re-permissioning the tree.
	setupHostedRootMode = 0o750
)

// setupHomeSources and setupHomePolicy are what a device backs up until
// somebody says otherwise: the user's home directory, kept on a
// grandfather-father-son ladder rather than "every snapshot forever".
var (
	setupHomeSources = []string{"~"}
	setupHomePolicy  = json.RawMessage(`{"retention":{"keepLatest":10,"keepHourly":24,"keepDaily":14,"keepWeekly":8,"keepMonthly":12,"keepAnnual":2}}`)
)

// ErrCloudNeedsWizard is returned when cloud-direct storage is asked for
// somewhere that cannot collect and verify the credentials it needs.
var ErrCloudNeedsWizard = errors.New(`cloud storage needs bucket credentials and an Object Lock check; activate with disk storage and add the cloud target in the dashboard`)

// SetupDefaults finishes a fresh install: the first hosted target, the policy
// template its devices inherit, and the group that binds the two - so a new
// Fleet server can enroll a device without anyone opening a screen. It returns
// the enrollment command for that group (with no secret in it) and a fresh
// token for the caller to show separately from the command; both are "" if
// there was nothing to do or no public URL to enroll against.
//
// Idempotent per row rather than all-or-nothing: SQLite gives no transaction
// across these three inserts here, so a run that dies after the target would
// otherwise leave a target with no group and a "some target exists, skip
// everything" rule would never repair it. Each row is created only if the one
// this function creates is missing. A fleet that already has targets none of
// which are ours belongs to an operator, and is left completely alone.
func (s *Server) SetupDefaults(ctx context.Context, publicURL, storage, hostedRoot string) (command, token string, err error) {
	st := s.store()
	if st == nil {
		return "", "", errors.New("fleet is not activated")
	}

	switch storage {
	case "", "disk":
	case "cloud":
		return "", "", ErrCloudNeedsWizard
	default:
		return "", "", errors.New("storage must be disk or cloud")
	}

	if publicURL != "" {
		if err := s.SetPublicURL(ctx, publicURL); err != nil {
			return "", "", err
		}
	}

	targets, err := st.Targets(ctx)
	if err != nil {
		return "", "", err
	}

	targetID := int64(0)
	for _, t := range targets {
		if t.Name == setupTargetName {
			targetID = t.ID
		}
	}

	if targetID == 0 && len(targets) > 0 {
		// Somebody has already configured this fleet by hand.
		return "", "", nil
	}

	now := s.now()

	if targetID == 0 {
		if hostedRoot == "" {
			return "", "", errors.New("a hosted root directory is required")
		}

		if err := ensureHostedRoot(hostedRoot); err != nil {
			return "", "", err
		}

		if targetID, err = st.CreateTarget(ctx, &store.Target{
			Name: setupTargetName, Kind: "hosted", StorageMode: "disk", Path: hostedRoot, CreatedAt: now,
		}); err != nil {
			return "", "", err
		}
	}

	templates, err := st.Templates(ctx)
	if err != nil {
		return "", "", err
	}

	templateID := int64(0)
	for _, t := range templates {
		if t.Name == setupTemplateName {
			templateID = t.ID
		}
	}

	if templateID == 0 {
		if templateID, err = st.CreateTemplate(ctx, &store.Template{
			Name: setupTemplateName, Sources: setupHomeSources, PolicyJSON: setupHomePolicy, CreatedAt: now,
		}); err != nil {
			return "", "", err
		}
	}

	groups, err := st.Groups(ctx)
	if err != nil {
		return "", "", err
	}

	for _, g := range groups {
		if g.TargetID == targetID {
			// The fleet can already enroll into this target; a second token
			// here would be a surprise, not a service.
			return "", "", nil
		}
	}

	groupID, err := st.CreateGroup(ctx, &store.Group{
		Name: setupGroupName, TargetID: targetID, TemplateID: templateID, CreatedAt: now,
	})
	if err != nil {
		return "", "", err
	}

	return s.enrollmentCommand(ctx, groupID)
}

// ensureHostedRoot creates the directory devices' backups land in, and refuses
// one that already exists as anything but a real directory: a symlink or a
// file there would send - or fail - every device's data somewhere nobody
// chose. Ownership and mode of an existing directory are the installer's
// business, not this function's.
func ensureHostedRoot(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case err == nil && fi.Mode()&os.ModeSymlink != 0:
		return errors.New("hosted root " + path + " is a symlink; point it at a real directory")
	case err == nil && !fi.IsDir():
		return errors.New("hosted root " + path + " exists and is not a directory")
	}

	return os.MkdirAll(path, setupHostedRootMode)
}

// enrollmentCommand issues one enrollment token for a group and returns the
// command a new device runs plus the token, kept apart so a caller never has
// to fold a secret into a string destined for a terminal, a log file or a
// service journal. The token also goes in the environment rather than argv
// when it reaches enroll.sh, so it is not visible in "ps" either.
func (s *Server) enrollmentCommand(ctx context.Context, groupID int64) (command, token string, err error) {
	u, ok := s.PublicURL(ctx)
	if !ok {
		return "", "", nil
	}

	plain, _, err := s.tokens().Issue(ctx, groupID, 0, -1, 0)
	if err != nil {
		return "", "", err
	}

	// Same shape the dashboard shows: the command never carries the token
	// (shell history); the script prompts for it, or reads WARPHOLD_ENROLL_TOKEN.
	return "curl -fsSL " + u.String() + "/enroll.sh | sh", plain, nil
}
