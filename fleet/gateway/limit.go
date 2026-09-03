package gateway

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Rate limit per device (spec §11): 50 requests/second sustained, burst 200.
// minio-go retries on 503 SlowDown, so a throttled device slows down rather
// than failing its snapshot.
const (
	// DefaultRatePerSecond and DefaultRateBurst are the per-device token
	// bucket (spec §11).
	DefaultRatePerSecond = 50
	DefaultRateBurst     = 200

	// The pre-auth limiter is keyed by client IP and sits ahead of SigV4, so
	// its only job is to stop an unauthenticated flood from buying HMAC work.
	// It is deliberately looser than the per-device one: a whole fleet can
	// share one IP behind a proxy that does not set X-Forwarded-For.
	// DefaultIPRatePerSecond and DefaultIPRateBurst are the pre-auth bucket.
	DefaultIPRatePerSecond = 200
	DefaultIPRateBurst     = 800

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
		ratePerSecond = DefaultRatePerSecond
	}

	if burst <= 0 {
		burst = DefaultRateBurst
	}

	return &limiter{rate: ratePerSecond, burst: burst, buckets: map[string]*bucket{}}
}

// newIPLimiter is newLimiter with the looser pre-auth defaults.
func newIPLimiter(ratePerSecond, burst float64) *limiter {
	if ratePerSecond <= 0 {
		ratePerSecond = DefaultIPRatePerSecond
	}

	if burst <= 0 {
		burst = DefaultIPRateBurst
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

// ClientIP is the address the pre-auth limiter keys on, and the one place
// X-Forwarded-For is interpreted anywhere in WarpHold.
//
// The peer address is used unless the peer itself is inside one of the
// operator's trusted CIDRs. Only then is X-Forwarded-For read, and then only
// right to left: hops appended by trusted proxies are skipped and the first
// untrusted address from the right is the client. Walking from the right is
// what makes the header unforgeable -- a client can prepend anything it likes
// to the left of the chain, but it cannot make its own hop, which the nearest
// trusted proxy appends, disappear.
//
// Fails closed to the peer address: no trusted CIDRs, a peer outside them, an
// unparseable hop, or a chain that is trusted end to end all yield the peer.
func ClientIP(r *http.Request, trusted []net.IPNet) string {
	peer := peerIP(r)

	if len(trusted) == 0 || !isTrusted(net.ParseIP(peer), trusted) {
		return peer
	}

	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.Trim(strings.TrimSpace(hops[i]), "[]"))
		if ip == nil {
			// A malformed chain says nothing reliable about any hop in it.
			return peer
		}

		if !isTrusted(ip, trusted) {
			return ip.String()
		}
	}

	return peer
}

// peerIP is the address of the socket, with any port stripped.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

func isTrusted(ip net.IP, trusted []net.IPNet) bool {
	if ip == nil {
		return false
	}

	for i := range trusted {
		if trusted[i].Contains(ip) {
			return true
		}
	}

	return false
}

// ParseTrustedProxies reads a comma-separated list of CIDRs (a bare address is
// taken as a single-host CIDR) into the form ClientIP wants. It is all or
// nothing: one bad entry fails the whole list, because a half-applied proxy
// list is how a header becomes trusted from the wrong place.
func ParseTrustedProxies(raw string) ([]net.IPNet, error) {
	var out []net.IPNet

	for field := range strings.SplitSeq(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}

		if !strings.Contains(field, "/") {
			ip := net.ParseIP(strings.Trim(field, "[]"))
			if ip == nil {
				return nil, fmt.Errorf("%q is not an IP address or CIDR", field)
			}

			bits := 8 * net.IPv6len
			if ip.To4() != nil {
				bits = 8 * net.IPv4len
			}

			out = append(out, net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})

			continue
		}

		_, n, err := net.ParseCIDR(field)
		if err != nil {
			return nil, fmt.Errorf("%q is not a CIDR", field)
		}

		out = append(out, *n)
	}

	return out, nil
}
