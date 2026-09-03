// Package gateway serves the device-facing S3 endpoint of the Fleet server.
package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
)

const (
	// cacheTTL bounds how long a revoked key can keep working if the revoke path
	// somehow misses its Invalidate call.
	cacheTTL = 5 * time.Minute
	// cacheSize caps the cache; it is cleared wholesale when full.
	cacheSize = 4096
)

type keyEntry struct {
	agentID, prefix, secret string
	readOnly                bool
	expires                 time.Time
}

// Keys resolves gateway access key ids to a device, unsealing the secret once
// and caching it for at most cacheTTL. Secrets live in memory only; they are
// never logged.
type Keys struct {
	st  *store.Store
	key seal.Key
	now func() time.Time

	mu    sync.Mutex
	gen   uint64 // bumped by Invalidate; a lookup that raced it does not cache
	cache map[string]keyEntry
}

// NewKeys wraps a store and the fleet sealing key.
func NewKeys(st *store.Store, k seal.Key) *Keys {
	return &Keys{st: st, key: k, now: time.Now, cache: map[string]keyEntry{}}
}

// SetNowForTesting overrides the clock.
func (k *Keys) SetNowForTesting(f func() time.Time) { k.now = f }

// Lookup resolves an access key id. ok is false for an unknown, disabled or
// unsealable key -- the caller must not distinguish the three.
func (k *Keys) Lookup(ctx context.Context, accessKeyID string) (agentID, prefix, secret string, readOnly, ok bool) {
	now := k.now()

	k.mu.Lock()
	gen := k.gen
	e, hit := k.cache[accessKeyID]
	k.mu.Unlock()

	if hit && now.Before(e.expires) {
		return e.agentID, e.prefix, e.secret, e.readOnly, true
	}

	row, err := k.st.DeviceKey(ctx, accessKeyID)
	if err != nil {
		k.forget(accessKeyID)
		return "", "", "", false, false
	}

	plain, err := k.key.Open(row.SealedSecret)
	if err != nil {
		k.forget(accessKeyID)
		return "", "", "", false, false
	}

	e = keyEntry{agentID: row.AgentID, prefix: row.Prefix, secret: string(plain), readOnly: row.ReadOnly, expires: now.Add(cacheTTL)}

	k.mu.Lock()
	// Only cache when no Invalidate ran while the store was being read: this
	// row may predate a revocation and must not be resurrected for a whole TTL.
	if k.gen == gen {
		// ponytail: clear-when-full, not an LRU. The working set is one entry per
		// enrolled device; swap in a real LRU if a fleet ever outgrows cacheSize.
		if len(k.cache) >= cacheSize {
			k.cache = make(map[string]keyEntry, cacheSize)
		}
		k.cache[accessKeyID] = e
	}
	k.mu.Unlock()

	return e.agentID, e.prefix, e.secret, e.readOnly, true
}

// Invalidate drops every cached key of an agent, so a revoke takes effect on
// the next request rather than at TTL expiry.
func (k *Keys) Invalidate(agentID string) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.gen++

	for id, e := range k.cache {
		if e.agentID == agentID {
			delete(k.cache, id)
		}
	}
}

func (k *Keys) forget(accessKeyID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.cache, accessKeyID)
}
