package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	// publicURLSetting is the absolute URL every device, browser and proxy
	// reaches this Fleet on. Nothing derives it from the Host header once it
	// is set, so a device is always told the same origin the gateway signs
	// for. See spec 6.
	publicURLSetting = "public_url"
	// instanceIDSetting identifies this Fleet in the (unauthenticated) status
	// response. It is a random opaque id, not a secret: it exists so the
	// public-URL probe can tell "the URL answers" from "the URL answers, and
	// it is us" - otherwise a URL pointing at somebody else's WarpHold would
	// validate.
	instanceIDSetting = "instance_id"

	// publicURLProbeTimeout bounds the end-to-end probe. A reverse proxy that
	// needs longer than this to serve a static JSON status is misconfigured.
	publicURLProbeTimeout = 5 * time.Second
)

// proxyRequirements is the operator-facing list shown whenever the end-to-end
// probe fails: nearly every failure is one of these five (spec 6).
var proxyRequirements = []string{
	"forward the Host header unchanged",
	"forward the full path, including /s3/",
	"do not buffer request bodies",
	"allow a request body of at least 5 GiB",
	"allow a read timeout of at least 30 minutes",
}

// proxyError marks a failure of the end-to-end probe, as opposed to a syntax
// error in the URL itself. Only these carry proxyRequirements back to the
// caller - printing the proxy checklist under "that is not a URL" is noise.
type proxyError struct{ msg string }

func (e *proxyError) Error() string { return e.msg }

// isLoopbackHost reports whether host (no port) names this machine. It is the
// one exemption from "public_url must be https": a developer running Fleet on
// localhost has no certificate and no proxy.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// hostOnly strips any port from an HTTP Host header value.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}

// parsePublicURL validates and normalizes a public URL: absolute, https (or
// http only for loopback), host lowercased, and nothing after the host - a
// path, query or fragment would silently vanish from every URL built by
// concatenation ("<public_url>/enroll.sh"), so it is rejected rather than
// dropped.
func parsePublicURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("public_url is not a URL")
	}
	if u.Host == "" || u.Opaque != "" {
		return nil, errors.New("public_url must be absolute, like https://fleet.example.com")
	}
	if u.User != nil {
		return nil, errors.New("public_url must not contain credentials")
	}
	// An internationalized host would never match an Origin header, which a
	// browser always punycodes before sending, so it is rejected here rather
	// than failing later as a silent CSRF mismatch.
	for _, r := range u.Host {
		if r > unicode.MaxASCII {
			return nil, errors.New("public_url must use the punycode form of an internationalized host name")
		}
	}
	u.Host = strings.ToLower(u.Host)
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(hostOnly(u.Host)) {
			return nil, errors.New("public_url must use https:// unless it points at loopback")
		}
	default:
		return nil, errors.New("public_url must start with https://")
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("public_url must have no path, query or fragment")
	}
	// A default port is dropped, so https://h:443 and https://h are one
	// origin: a browser never puts the default port in an Origin header, and
	// keeping it here would make every CSRF check fail.
	if port := u.Port(); port == "443" && u.Scheme == "https" || port == "80" && u.Scheme == "http" {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]" // an IPv6 literal keeps its brackets
		}
	}
	u.Path, u.RawPath, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", "", "", false, "", ""
	return u, nil
}

// PublicURL returns the configured public URL, or ok=false when it is unset
// (or the Fleet is not activated). Callers that build a device-facing URL -
// the enrollment script, the /dl/ links, the S3 endpoint in the sealed bundle
// - use this in preference to anything derived from the request.
func (s *Server) PublicURL(ctx context.Context) (*url.URL, bool) {
	st := s.store()
	if st == nil {
		return nil, false
	}
	raw, err := st.Setting(ctx, publicURLSetting)
	if err != nil || raw == "" {
		return nil, false
	}
	u, err := parsePublicURL(raw)
	if err != nil {
		return nil, false
	}
	return u, true
}

// SetPublicURL validates raw and stores it. It does not probe; callers that
// want the end-to-end check (the settings PUT with "verify", the wizard) run
// verifyPublicURL first.
func (s *Server) SetPublicURL(ctx context.Context, raw string) error {
	st := s.store()
	if st == nil {
		return errors.New("fleet is not activated")
	}
	u, err := parsePublicURL(raw)
	if err != nil {
		return err
	}
	return st.SetSetting(ctx, publicURLSetting, u.String())
}

// instanceID returns this Fleet's opaque identity, generating it on first use
// so a Fleet activated before this setting existed also has one.
func (s *Server) instanceID(ctx context.Context) (string, error) {
	st := s.store()
	if st == nil {
		return "", errors.New("fleet is not activated")
	}
	s.instanceMu.Lock()
	defer s.instanceMu.Unlock()
	id, err := st.Setting(ctx, instanceIDSetting)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	id = hex.EncodeToString(b)
	if err := st.SetSetting(ctx, instanceIDSetting, id); err != nil {
		return "", err
	}
	return id, nil
}

// verifyPublicURL checks u end to end: this process fetches
// <u>/api/v1/fleet/status through the network - proxy, TLS and all - and
// requires an activated WarpHold whose instance id is ours. A regex cannot
// tell a typo'd hostname from a working one, and this is the only check that
// notices a proxy that swallows the path or answers for a different Fleet.
func (s *Server) verifyPublicURL(ctx context.Context, u *url.URL) error {
	want, err := s.instanceID(ctx)
	if err != nil {
		return err
	}
	hc := &http.Client{
		Timeout: publicURLProbeTimeout,
		// Redirects are not followed: a proxy that redirects the status
		// endpoint will redirect the agent's uploads too, and the device
		// would be told an origin that is not the one that answers.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return probePublicURL(ctx, hc, u, want)
}

// probePublicURL deliberately fetches an operator-supplied URL from inside
// this process. That is the feature, not an oversight: a Fleet behind a LAN
// proxy or on loopback is a supported deployment, so refusing private and
// link-local destinations would reject the very setups this exists to
// validate. What bounds it: only an authenticated admin can trigger it, it is
// a GET, it follows no redirects, it is capped at publicURLProbeTimeout, and
// nothing from the response body is ever echoed back to the caller.
func probePublicURL(ctx context.Context, hc *http.Client, u *url.URL, wantInstance string) error {
	ctx, cancel := context.WithTimeout(ctx, publicURLProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String()+"/api/v1/fleet/status", nil)
	if err != nil {
		return &proxyError{"cannot build a request for " + u.String()}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// The admin gets a generic message - the underlying error carries the
		// resolved address and TLS details - but an operator debugging a
		// proxy needs the real cause, so it goes to the log once per attempt.
		log.Printf("warphold fleet: public_url probe of %s failed: %v", u, err)
		return &proxyError{u.String() + " could not be reached from this server"}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &proxyError{u.String() + "/api/v1/fleet/status answered HTTP " + strconv.Itoa(resp.StatusCode) + ", not 200 OK"}
	}
	var st struct {
		Activated  bool   `json:"activated"`
		InstanceID string `json:"instance_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&st); err != nil || !st.Activated {
		if err != nil {
			log.Printf("warphold fleet: public_url probe of %s failed: %v", u, err)
		}
		return &proxyError{u.String() + " did not answer as an activated WarpHold Fleet"}
	}
	if st.InstanceID != wantInstance {
		return &proxyError{u.String() + " reached a different WarpHold Fleet, not this one"}
	}
	return nil
}

// requireHost rejects a request whose Host header names neither the
// configured public URL nor a loopback address. Fleet hands devices absolute
// URLs built from public_url; a request arriving under some other name is
// either a misrouted proxy or a rebinding attempt, and answering it would
// mint links for a host we do not control. Loopback stays exempt so a local
// curl (and the health check) keeps working, and the whole check is skipped
// until public_url is set.
func (s *Server) requireHost(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u, ok := s.PublicURL(r.Context()); ok {
			got := hostOnly(r.Host)
			if !strings.EqualFold(got, hostOnly(u.Host)) && !isLoopbackHost(got) {
				// The configured host is named: a 421 with no expected value
				// is undiagnosable, and public_url is admin-set, not secret.
				writeErr(w, http.StatusMisdirectedRequest, "request Host does not match the configured public URL; expected Host "+hostOnly(u.Host))
				return
			}
		}
		next(w, r)
	}
}
