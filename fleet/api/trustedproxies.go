package api

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"

	"github.com/kopia/kopia/fleet/gateway"
)

const (
	// trustedProxiesSetting is a comma-separated list of CIDRs whose
	// X-Forwarded-For header WarpHold believes. Empty -- the default -- means
	// the peer address is used everywhere and the header is ignored, which is
	// the only safe answer without knowing the deployment.
	trustedProxiesSetting = "trusted_proxies"

	// trustedProxiesEnv seeds the setting for `server start` on a Fleet whose
	// database is not reachable yet (first boot behind a proxy). The stored
	// setting wins when it is non-empty.
	trustedProxiesEnv = "WARPHOLD_TRUSTED_PROXIES"

	// The gateway's rate limits, as settings. Zero or absent means the
	// gateway's own default.
	gatewayIPRateSetting      = "gateway_ip_rate"
	gatewayIPBurstSetting     = "gateway_ip_burst"
	gatewayDeviceRateSetting  = "gateway_device_rate"
	gatewayDeviceBurstSetting = "gateway_device_burst"
)

// rateSettings are the four gateway limits: an operator-settable whole number
// with a range and the gateway's own constant as the default. The bounds are
// wide on purpose -- this is a safety valve for a big or a slow fleet, not a
// tuning knob -- but they exclude 0, which would wedge every device out.
var rateSettings = map[string]struct{ min, max, def int }{
	gatewayIPRateSetting:      {1, 100_000, gateway.DefaultIPRatePerSecond},
	gatewayIPBurstSetting:     {1, 1_000_000, gateway.DefaultIPRateBurst},
	gatewayDeviceRateSetting:  {1, 100_000, gateway.DefaultRatePerSecond},
	gatewayDeviceBurstSetting: {1, 1_000_000, gateway.DefaultRateBurst},
}

// trustedProxies returns the parsed CIDRs, or nil.
//
// The value is parsed once and memoized: the login limiter asks for it on
// every attempt, and an unauthenticated flood must not become one database
// read per request. handleSettingsUpdate and load invalidate the memo, so a
// change still takes effect without a restart.
//
// It fails closed -- a value that does not parse is treated as "no trusted
// proxies", so a typo makes WarpHold ignore X-Forwarded-For rather than
// believe it from everywhere.
func (s *Server) trustedProxies(ctx context.Context) []net.IPNet {
	s.tpMu.Lock()
	defer s.tpMu.Unlock()

	if s.tpLoaded {
		return s.tpNets
	}

	raw := ""
	if st := s.store(); st != nil {
		raw, _ = st.Setting(ctx, trustedProxiesSetting)
	}

	if raw == "" {
		raw = os.Getenv(trustedProxiesEnv)
	}

	s.tpNets, s.tpLoaded = nil, true

	if raw == "" {
		return nil
	}

	nets, err := gateway.ParseTrustedProxies(raw)
	if err != nil {
		// The value is not echoed: it is operator-supplied text, and a log
		// file is not where it should be reproduced. The settings API shows it
		// back to the admin who set it.
		log.Printf("warphold fleet: %s is not a valid CIDR list; X-Forwarded-For will not be trusted", trustedProxiesSetting)
		return nil
	}

	s.tpNets = nets

	return nets
}

// invalidateTrustedProxies drops the memo, so the next read re-parses.
func (s *Server) invalidateTrustedProxies() {
	s.tpMu.Lock()
	s.tpLoaded = false
	s.tpMu.Unlock()
}

// clientIP is the address the login limiter keys on. It is the gateway's
// ClientIP with this Fleet's trusted-proxy list, so the two limiters agree on
// who the client is and X-Forwarded-For is interpreted in exactly one place.
func (s *Server) clientIP(r *http.Request) string {
	return gateway.ClientIP(r, s.trustedProxies(r.Context()))
}

// rateSetting reads one of rateSettings, falling back to its default when it
// is unset or outside its range.
func (s *Server) rateSetting(ctx context.Context, key string) float64 {
	spec := rateSettings[key]

	st := s.store()
	if st == nil {
		return float64(spec.def)
	}

	raw, _ := st.Setting(ctx, key)
	if n, err := strconv.Atoi(raw); err == nil && n >= spec.min && n <= spec.max {
		return float64(n)
	}

	return float64(spec.def)
}
