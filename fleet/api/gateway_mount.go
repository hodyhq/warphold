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

	// The old backends are handles nothing will ask for again -- a directory fd
	// for a disk target, an HTTP client for a cloud-direct one -- so closing
	// them here is what keeps an activation cycle (or a reopen after Close)
	// from leaking one per hosted target.
	//
	// Invariant: closeStores is the ONLY place a cached store is closed, and it
	// runs only when the *store.Store behind the gateway is swapped (New /
	// Activate / a reopen after Close) or when the Server itself is closed --
	// never under a live request. A request resolves its backend through
	// targetStore, which hands out a handle from this same map under the same
	// lock; if a swap could happen mid-request the handle would be closed out
	// from under it.
	closeStores(s.gwDeps.stores)

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

	return s.targetStore(ctx, *t)
}

// targetStore returns the cached ObjectStore for t, creating it on first use.
//
// Both hosted storage modes are served: "disk" from the local root, "cloud" by
// writing through to the customer's own bucket with the fleet's admin key
// (cloudStoreFor, which unseals the credentials). Every other kind is
// unsupported and answers 501.
func (s *Server) targetStore(ctx context.Context, t store.Target) (gateway.ObjectStore, error) {
	if t.Kind != "hosted" || (t.StorageMode != "disk" && t.StorageMode != "cloud") {
		return nil, fmt.Errorf("%w: target %q is %s/%s", gateway.ErrUnsupportedStorageMode, t.Name, t.Kind, t.StorageMode)
	}

	// Only a disk target has a root; a cloud-direct one is addressed by bucket.
	if t.StorageMode == "disk" && t.Path == "" {
		return nil, fmt.Errorf("hosted target %q has no path", t.Name)
	}

	// Build the gateway first. Enrollment borrows a cloud-direct target's
	// backend to provision the repository, and that can be the very first call
	// into this cache -- before any device request has created the map this
	// writes into, and before gwDeps.st names the store the handles belong to.
	// Skipping it panicked on a nil map, and then let the first device request
	// close the handle provisioning had just cached.
	s.gateway()

	s.gwDeps.mu.Lock()
	defer s.gwDeps.mu.Unlock()

	if cached, ok := s.gwDeps.stores[t.ID]; ok {
		return cached, nil
	}

	var (
		objs gateway.ObjectStore
		err  error
	)

	if t.StorageMode == "cloud" {
		objs, err = s.cloudStoreFor(ctx, &t)
	} else {
		objs, err = gateway.NewLocal(t.Path, gateway.LocalOptions{})
	}

	if err != nil {
		return nil, fmt.Errorf("opening storage for target %q: %w", t.Name, err)
	}

	s.gwDeps.stores[t.ID] = objs

	return objs, nil
}

// closeStores releases every cached backend that holds a handle or a
// connection. The two backends spell Close differently: the local one closes a
// directory fd (io.Closer), the cloud one takes a context and drops its idle
// connections, so both shapes are accepted rather than only one.
//
// It is called with gwDeps.mu held, which is safe: Close only releases a
// handle, it makes no call back into the Server.
func closeStores(m map[int64]gateway.ObjectStore) {
	for id, objs := range m {
		var err error

		switch c := objs.(type) {
		case interface{ Close(context.Context) error }:
			err = c.Close(context.Background())
		case io.Closer:
			err = c.Close()
		default:
			continue
		}

		if err != nil {
			log.Printf("warphold fleet: closing the store of target %d: %v", id, err)
		}
	}
}

// closeGatewayStores releases every cached backend and empties the cache, so a
// closed Server leaves no provider connection and no spool file behind.
// Server.Close calls it before it takes s.mu: gateway() holds gwDeps.mu while
// it reads s.mu (sealKey, the rate settings), so gwDeps.mu must never be taken
// underneath s.mu.
func (s *Server) closeGatewayStores() {
	s.gwDeps.mu.Lock()
	defer s.gwDeps.mu.Unlock()

	closeStores(s.gwDeps.stores)

	// An empty map rather than nil: gateway() rebuilds on the next request and
	// a racing targetStore must never write into a nil map.
	s.gwDeps.stores = map[int64]gateway.ObjectStore{}
	s.gwDeps.gw, s.gwDeps.st = nil, nil
}
