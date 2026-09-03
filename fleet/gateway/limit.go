package gateway

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Rate limit per device (spec §11): 50 requests/second sustained, burst 200.
// minio-go retries on 503 SlowDown, so a throttled device slows down rather
// than failing its snapshot.
const (
	defaultRatePerSecond = 50
	defaultRateBurst     = 200

	// The pre-auth limiter is keyed by client IP and sits ahead of SigV4, so
	// its only job is to stop an unauthenticated flood from buying HMAC work.
	// It is deliberately looser than the per-device one: a whole fleet can
	// share one IP behind a proxy that does not set X-Forwarded-For.
	defaultIPRatePerSecond = 200
	defaultIPRateBurst     = 800

	// idleEvict is how long an unused bucket is kept before the next sweep
	// drops it, and sweepEvery is how often a sweep runs. Both exist only to
	// bound the map for a fleet whose devices come and go.
	idleEvict  = 10 * time.Minute
	sweepEvery = time.Minute
)

type bucket struct {
	tokens float64
	last   time.Time
}

// limiter is a token bucket per access key id. Entries are created on the
// first authenticated request for a key and swept when idle, so an
// unauthenticated flood can never grow the map.
//
// ponytail: one mutex over the whole limiter map; shard it if 1000 devices
// ever contend measurably.
type limiter struct {
	rate, burst float64

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

func newLimiter(ratePerSecond, burst float64) *limiter {
	if ratePerSecond <= 0 {
		ratePerSecond = defaultRatePerSecond
	}

	if burst <= 0 {
		burst = defaultRateBurst
	}

	return &limiter{rate: ratePerSecond, burst: burst, buckets: map[string]*bucket{}}
}

// newIPLimiter is newLimiter with the looser pre-auth defaults.
func newIPLimiter(ratePerSecond, burst float64) *limiter {
	if ratePerSecond <= 0 {
		ratePerSecond = defaultIPRatePerSecond
	}

	if burst <= 0 {
		burst = defaultIPRateBurst
	}

	return newLimiter(ratePerSecond, burst)
}

// allow takes one token for id, reporting whether it was available.
func (l *limiter) allow(id string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	b := l.buckets[id]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[id] = b
	}

	if d := now.Sub(b.last); d > 0 {
		b.tokens = min(l.burst, b.tokens+d.Seconds()*l.rate)
	}

	b.last = now

	if b.tokens < 1 {
		return false
	}

	b.tokens--

	return true
}

// sweepLocked drops buckets that have been idle and full, at most once per
// sweepEvery. Inline, so there is no goroutine to start or stop.
func (l *limiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < sweepEvery {
		return
	}

	l.lastSweep = now

	for id, b := range l.buckets {
		if now.Sub(b.last) > idleEvict {
			delete(l.buckets, id)
		}
	}
}

// clientIP is the address the pre-auth limiter keys on: the peer as this
// process sees it. X-Forwarded-For is deliberately NOT read -- anything that
// can reach the gateway on loopback could otherwise forge it and pick its own
// limiter bucket -- which matches fleet/api's own clientIP; a configured
// trusted-proxy list is Plan 2 for both.
//
// ponytail: behind a reverse proxy the whole fleet shares one bucket, which is
// why the pre-auth defaults are generous and the real per-device limit runs
// after the signature check. Add the trusted-proxy list if a deployment ever
// needs per-client pre-auth limiting.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}
