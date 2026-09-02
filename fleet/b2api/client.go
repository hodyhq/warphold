// Package b2api is a minimal Backblaze B2 native API client.
package b2api

import "net/http"

// API is what Fleet needs from B2.
type API interface{}

// New returns the real client (Task 8).
func New(_ *http.Client) API { return nil }
