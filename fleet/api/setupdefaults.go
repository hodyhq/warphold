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
// the enrollment command for that group, complete with a fresh token, or "" if
// there was nothing to do or no public URL to enroll against.
//
// Idempotent by the bluntest rule available: it does nothing at all once any
// target exists, so re-running setup cannot produce a second "Fleet disk".
func (s *Server) SetupDefaults(ctx context.Context, publicURL, storage, hostedRoot string) (string, error) {
	st := s.store()
	if st == nil {
		return "", errors.New("fleet is not activated")
	}

	switch storage {
	case "", "disk":
	case "cloud":
		return "", ErrCloudNeedsWizard
	default:
		return "", errors.New("storage must be disk or cloud")
	}

	if publicURL != "" {
		if err := s.SetPublicURL(ctx, publicURL); err != nil {
			return "", err
		}
	}

	targets, err := st.Targets(ctx)
	if err != nil {
		return "", err
	}

	if len(targets) > 0 {
		return "", nil
	}

	if hostedRoot == "" {
		return "", errors.New("a hosted root directory is required")
	}

	if err := os.MkdirAll(hostedRoot, setupHostedRootMode); err != nil {
		return "", err
	}

	now := s.now()

	targetID, err := st.CreateTarget(ctx, &store.Target{
		Name: setupTargetName, Kind: "hosted", StorageMode: "disk", Path: hostedRoot, CreatedAt: now,
	})
	if err != nil {
		return "", err
	}

	templateID, err := st.CreateTemplate(ctx, &store.Template{
		Name: setupTemplateName, Sources: setupHomeSources, PolicyJSON: setupHomePolicy, CreatedAt: now,
	})
	if err != nil {
		return "", err
	}

	groupID, err := st.CreateGroup(ctx, &store.Group{
		Name: setupGroupName, TargetID: targetID, TemplateID: templateID, CreatedAt: now,
	})
	if err != nil {
		return "", err
	}

	return s.enrollmentCommand(ctx, groupID)
}

// enrollmentCommand issues one enrollment token for a group and returns the
// command a new device runs. The token goes in the environment rather than in
// argv, so it is not visible in "ps" on the enrolling machine; the script
// documents the --token form too.
func (s *Server) enrollmentCommand(ctx context.Context, groupID int64) (string, error) {
	u, ok := s.PublicURL(ctx)
	if !ok {
		return "", nil
	}

	plain, _, err := s.tokens().Issue(ctx, groupID, 0, -1, 0)
	if err != nil {
		return "", err
	}

	return `WARPHOLD_ENROLL_TOKEN=` + plain + ` sh -c "$(curl -fsSL ` + u.String() + `/enroll.sh)"`, nil
}
