// Package b2api is a minimal Backblaze B2 native API client.
package b2api

import (
	"context"
	"net/http"
)

// BucketInfo describes a B2 bucket as needed for Object Lock verification.
type BucketInfo struct {
	ID                string
	ObjectLockEnabled bool
}

// API is what Fleet needs from B2.
type API interface {
	BucketInfo(ctx context.Context, keyID, key, bucket string) (BucketInfo, error)
}

// New returns the real client (Task 8).
func New(_ *http.Client) API { return nil }
