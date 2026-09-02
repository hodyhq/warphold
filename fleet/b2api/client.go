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
	"time"
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

var _ API = (*Client)(nil)

// defaultTimeout bounds a B2 request when the caller supplies neither its own
// client nor a deadline; http.DefaultClient has no timeout, so a stalled
// connection would otherwise block an admin or enrollment request forever.
const defaultTimeout = 30 * time.Second

// New returns a Client; nil h uses a client with defaultTimeout.
func New(h *http.Client) *Client {
	if h == nil {
		h = &http.Client{Timeout: defaultTimeout}
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
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
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
