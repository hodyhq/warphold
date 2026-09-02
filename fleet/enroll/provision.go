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

		w, err := p.B2.CreateKey(ctx, t.AdminKeyID, t.AdminKey, b2api.KeyRequest{
			Name: "warphold-" + agentID + "-writer", BucketID: info.ID, NamePrefix: b.Prefix, Capabilities: b2api.WriterCaps,
		})
		if err != nil {
			return nil, err
		}

		r, err := p.B2.CreateKey(ctx, t.AdminKeyID, t.AdminKey, b2api.KeyRequest{
			Name: "warphold-" + agentID + "-reader", BucketID: info.ID, NamePrefix: b.Prefix, Capabilities: b2api.ReaderCaps,
		})
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

	initFn := p.initialize
	if p.InitializeForTesting != nil {
		initFn = p.InitializeForTesting
	}

	if err := initFn(ctx, adminCI, password); err != nil {
		// Both B2 keys already exist as live credentials at this point; revoke
		// them rather than leaking scoped keys nobody else will clean up.
		if t.Kind == "b2" {
			_ = p.Revoke(ctx, t, b)
		}

		return nil, err
	}

	tok, err := repo.EncodeToken(password, agentCI)
	if err != nil {
		if t.Kind == "b2" {
			_ = p.Revoke(ctx, t, b)
		}

		return nil, err
	}

	b.ConnectToken = tok

	return b, nil
}

// initialize creates the repository, connects to a scratch config, and sets
// Fleet as the maintenance owner.
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
