package api

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/fleet/store"
)

// gatewayDeps is the gateway's per-activation state: the key cache and one
// ObjectStore per hosted target, both rebuilt when the store behind them is
// replaced (activation, or a reopen after Close).
type gatewayDeps struct {
	mu     sync.Mutex
	st     *store.Store
	gw     *gateway.Gateway
	stores map[int64]gateway.ObjectStore
}

// mountGateway registers the device-facing S3 endpoint. It is mounted whenever
// Fleet is served, activated or not: before activation there are no device keys,
// so every request is an unknown key and answers 403, which is exactly what a
// revoked device sees too.
func (s *Server) mountGateway(m *mux.Router) {
	// requireHost like every other Fleet route: once public_url is set, a
	// request arriving under another Host is 421, so a device that resolved a
	// stale or spoofed name never reaches the signature check.
	h := s.requireHost(s.serveGateway)
	m.Path("/" + gateway.BucketName).Handler(h)
	m.PathPrefix(gateway.PathPrefix).Handler(h)
}

// serveGateway defers building the Gateway until the first request, because the
// key cache needs the store and the sealing key that activation creates.
func (s *Server) serveGateway(w http.ResponseWriter, r *http.Request) {
	g := s.gateway()
	if g == nil {
		notActivatedGateway.ServeHTTP(w, r)
		return
	}

	g.ServeHTTP(w, r)
}

var notActivatedGateway = gateway.NotActivatedHandler()

// gateway returns the Gateway for the current store, or nil before activation.
func (s *Server) gateway() *gateway.Gateway {
	st := s.store()
	if st == nil {
		return nil
	}

	s.gwDeps.mu.Lock()
	defer s.gwDeps.mu.Unlock()

	if s.gwDeps.gw != nil && s.gwDeps.st == st {
		return s.gwDeps.gw
	}

	// The old backends are handles on directories nothing will ask for again;
	// closing them here is what keeps an activation cycle (or a reopen after
	// Close) from leaking one directory fd per hosted target.
	//
	// Invariant: this loop is the ONLY place a cached store is closed, and it
	// runs only when the *store.Store behind the gateway is swapped (New /
	// Activate / a reopen after Close) -- never under a live request. A request
	// resolves its backend through targetStore, which hands out a handle from
	// this same map under the same lock; if a swap could happen mid-request the
	// handle would be closed out from under it.
	for id, objs := range s.gwDeps.stores {
		if c, ok := objs.(io.Closer); ok {
			if err := c.Close(); err != nil {
				log.Printf("warphold fleet: closing the hosted store of target %d: %v", id, err)
			}
		}
	}

	s.gwDeps.st = st
	s.gwDeps.stores = map[int64]gateway.ObjectStore{}
	// The limits and the trusted-proxy list are a snapshot: the Gateway is
	// rebuilt when the store behind it changes, so a settings change applies
	// at restart. That is the same lifetime a reverse proxy's own config has.
	ctx := context.Background()

	s.gwDeps.gw = gateway.NewGateway(gateway.Config{
		Keys:            gateway.NewKeys(st, s.sealKey()),
		StoreFor:        s.storeForAgent,
		Now:             s.now,
		TrustedProxies:  s.trustedProxies(ctx),
		IPRatePerSecond: s.rateSetting(ctx, gatewayIPRateSetting),
		IPRateBurst:     s.rateSetting(ctx, gatewayIPBurstSetting),
		RatePerSecond:   s.rateSetting(ctx, gatewayDeviceRateSetting),
		RateBurst:       s.rateSetting(ctx, gatewayDeviceBurstSetting),
	})

	return s.gwDeps.gw
}

// storeForAgent resolves a verified device to the store behind its target:
// agent -> group -> target. The store is cached per target, so all of a
// target's devices share one backend handle.
func (s *Server) storeForAgent(ctx context.Context, agentID string) (gateway.ObjectStore, error) {
	st := s.store()
	if st == nil {
		return nil, fmt.Errorf("%w: fleet is not activated", gateway.ErrAccessDenied)
	}

	a, err := st.Agent(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("agent %s: %w", agentID, err)
	}

	if a.RevokedAt != nil {
		// A revoked device must be indistinguishable from an unknown one, so
		// this is the same 403 an unknown credential gets, not a 501.
		return nil, fmt.Errorf("%w: agent %s is revoked", gateway.ErrAccessDenied, agentID)
	}

	g, err := st.Group(ctx, a.GroupID)
	if err != nil {
		return nil, fmt.Errorf("group %d: %w", a.GroupID, err)
	}

	t, err := st.Target(ctx, g.TargetID)
	if err != nil {
		return nil, fmt.Errorf("target %d: %w", g.TargetID, err)
	}

	return s.targetStore(*t)
}

// targetStore returns the cached ObjectStore for t, creating it on first use.
//
// Only hosted targets in "disk" storage mode are served today; "cloud" is the
// mirror/cloud-direct work of M2 and answers 501 until then.
func (s *Server) targetStore(t store.Target) (gateway.ObjectStore, error) {
	if t.Kind != "hosted" || t.StorageMode != "disk" {
		return nil, fmt.Errorf("%w: target %q is %s/%s", gateway.ErrUnsupportedStorageMode, t.Name, t.Kind, t.StorageMode)
	}

	if t.Path == "" {
		return nil, fmt.Errorf("hosted target %q has no path", t.Name)
	}

	s.gwDeps.mu.Lock()
	defer s.gwDeps.mu.Unlock()

	if cached, ok := s.gwDeps.stores[t.ID]; ok {
		return cached, nil
	}

	objs, err := gateway.NewLocal(t.Path, gateway.LocalOptions{})
	if err != nil {
		return nil, fmt.Errorf("opening hosted root for target %q: %w", t.Name, err)
	}

	s.gwDeps.stores[t.ID] = objs

	return objs, nil
}
