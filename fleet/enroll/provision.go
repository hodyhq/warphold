package enroll

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kopia/kopia/fleet/b2api"
	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/b2"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/blob/s3"
	"github.com/kopia/kopia/repo/maintenance"
)

// Bundle is everything an agent needs to connect, and what Fleet escrows.
//
// The hosted fields describe the same S3 endpoint that is already encoded in
// ConnectToken; they are here so the recovery kit (and a human) can rebuild the
// connection without decoding the token. Endpoint carries its scheme; the
// connection info inside the token carries the bare host, which is what
// minio-go wants.
type Bundle struct {
	ConnectToken string `json:"connect_token"`
	Password     string `json:"password"`
	Prefix       string `json:"prefix"`
	WriterKeyID  string `json:"writer_key_id,omitempty"`
	WriterKey    string `json:"writer_key,omitempty"`
	ReaderKeyID  string `json:"reader_key_id,omitempty"`
	ReaderKey    string `json:"reader_key,omitempty"`
	GatewayKeyID string `json:"gateway_key_id,omitempty"`
	GatewayKey   string `json:"gateway_key,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	Bucket       string `json:"bucket,omitempty"`
	Region       string `json:"region,omitempty"`
}

// TargetSpec is the unsealed view of a target.
type TargetSpec struct {
	Kind, Bucket, Path, AdminKeyID, AdminKey string

	// Hosted targets only (spec 7.1). StorageMode is "disk" or "cloud";
	// HostedRoot is the target's on-disk root; PublicHost is the host[:port]
	// of public_url, TLS reports whether it is https, and Region is the region
	// the gateway advertises.
	StorageMode, HostedRoot, PublicHost, Region string
	TLS                                         bool
}

// Provisioner creates per-agent repositories and credentials.
type Provisioner struct {
	B2    b2api.API
	Owner string

	// Store and SealKey are required for hosted targets: the device's gateway
	// credential is a row in device_keys with its secret sealed at rest.
	Store   *store.Store
	SealKey seal.Key

	// Now is the clock, for the device_keys timestamp. Defaults to time.Now.
	Now func() time.Time

	// InitializeForTesting replaces repository creation in unit tests that have no storage.
	InitializeForTesting func(ctx context.Context, ci blob.ConnectionInfo, password string) error
}

func (p *Provisioner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}

	return time.Now()
}

// NewGatewayCredentials returns an S3-shaped key pair for the device gateway:
// the access key id is "WH" plus 18 base32 characters (20 in all, the length
// an AWS access key id has), the secret is 40 base64url characters. Both come
// from crypto/rand.
func NewGatewayCredentials() (accessKeyID, secret string, err error) {
	b := make([]byte, 30)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", "", err
	}

	// rand.Text is RFC 4648 base32 without padding, drawn from crypto/rand.
	return "WH" + rand.Text()[:18], base64.RawURLEncoding.EncodeToString(b), nil
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

	case "hosted":
		ci, err := p.provisionHosted(ctx, t, agentID, b)
		if err != nil {
			return nil, err
		}

		adminCI, agentCI = ci.admin, ci.agent

	default:
		return nil, errors.New("unsupported target kind " + t.Kind)
	}

	initFn := p.initialize
	if p.InitializeForTesting != nil {
		initFn = p.InitializeForTesting
	}

	if err := initFn(ctx, adminCI, password); err != nil {
		p.rollback(ctx, t, agentID, b)

		return nil, err
	}

	tok, err := repo.EncodeToken(password, agentCI)
	if err != nil {
		p.rollback(ctx, t, agentID, b)

		return nil, err
	}

	b.ConnectToken = tok

	return b, nil
}

// connInfos is the pair of connection infos a hosted target needs: the
// server's own direct handle on the bytes, and the device's S3 view of them.
type connInfos struct{ admin, agent blob.ConnectionInfo }

// provisionHosted mints the device's gateway credential, records it, and
// returns the two connection infos of spec 7.1 steps 3 and 4.
func (p *Provisioner) provisionHosted(ctx context.Context, t TargetSpec, agentID string, b *Bundle) (connInfos, error) {
	switch {
	case t.StorageMode != "disk":
		// "cloud" is the cloud-direct backing store of M2.
		return connInfos{}, errors.New("hosted storage mode " + t.StorageMode + " is not supported yet")
	case t.HostedRoot == "":
		return connInfos{}, errors.New("hosted target has no root directory")
	case t.PublicHost == "":
		return connInfos{}, errors.New("the public URL must be set before enrolling a device on a hosted target")
	case p.Store == nil:
		return connInfos{}, errors.New("hosted provisioning needs a store")
	}

	akid, secret, err := NewGatewayCredentials()
	if err != nil {
		return connInfos{}, err
	}

	sealed, err := p.SealKey.Seal([]byte(secret))
	if err != nil {
		return connInfos{}, err
	}

	// Exactly "<agent-id>/": the gateway confines every key of this credential
	// to this prefix, and it checks that the prefix is the agent's own.
	prefix := agentID + "/"

	if err := p.Store.CreateDeviceKey(ctx, &store.DeviceKey{
		AccessKeyID: akid, AgentID: agentID, SealedSecret: sealed, Prefix: prefix, CreatedAt: p.now(),
	}); err != nil {
		return connInfos{}, err
	}

	region := t.Region
	if region == "" {
		region = gateway.BucketName
	}

	scheme := "http://"
	if t.TLS {
		scheme = "https://"
	}

	b.Prefix, b.GatewayKeyID, b.GatewayKey = prefix, akid, secret
	b.Endpoint, b.Bucket, b.Region = scheme+t.PublicHost, gateway.BucketName, region

	return connInfos{
		// The server writes the repository through the same store the gateway
		// serves - flat keys, no sharding - not over its own HTTP endpoint.
		admin: blob.ConnectionInfo{Type: gateway.HostedStorageType, Config: &gateway.HostedOptions{Root: t.HostedRoot, Prefix: prefix}},
		agent: blob.ConnectionInfo{Type: "s3", Config: &s3.Options{
			BucketName:      gateway.BucketName,
			Prefix:          prefix,
			Endpoint:        t.PublicHost,
			DoNotUseTLS:     !t.TLS,
			AccessKeyID:     akid,
			SecretAccessKey: secret,
			Region:          region,
		}},
	}, nil
}

// rollback hands back credentials that Provision minted before it failed. The
// repository directory, if one was created, is left alone: it holds no data
// and the reap job owns removal (D6).
func (p *Provisioner) rollback(ctx context.Context, t TargetSpec, agentID string, b *Bundle) {
	switch t.Kind {
	case "b2":
		// Both B2 keys already exist as live credentials at this point; revoke
		// them rather than leaking scoped keys nobody else will clean up.
		_ = p.Revoke(ctx, t, b)
	case "hosted":
		if p.Store != nil && b.GatewayKeyID != "" {
			_, _ = p.Store.DisableDeviceKeysForAgent(ctx, agentID, p.now())
		}
	}
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

	// Revoke also runs on a half-finished Provision, where one or both key
	// ids are still empty; b2_delete_key with an empty applicationKeyId is an
	// error that says nothing.
	var errs []error

	for _, id := range []string{b.WriterKeyID, b.ReaderKeyID} {
		if id == "" {
			continue
		}

		errs = append(errs, p.B2.DeleteKey(ctx, t.AdminKeyID, t.AdminKey, id))
	}

	return errors.Join(errs...)
}
