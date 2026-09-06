package api_test

import (
	"context"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet"
	"github.com/kopia/kopia/fleet/mail"
	"github.com/kopia/kopia/fleet/store"
)

// smtpSettings is a complete, valid PUT body.
func smtpSettings() map[string]any {
	return map[string]any{
		"smtp_host": "mail.example.com", "smtp_port": 587, "smtp_username": "u",
		"smtp_password": "s3cret", "smtp_from": "fleet@example.com", "smtp_tls": true,
	}
}

func TestSMTPSettingsSealThePasswordAndNeverReturnIt(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.activateAndLogin()

	_, body := h.do("GET", "/api/v1/fleet/settings", nil)
	require.Equal(t, "mail.smtp2go.com", body["smtp_host"], "SMTP2GO-shaped default")
	require.Equal(t, float64(2525), body["smtp_port"])
	require.Equal(t, true, body["smtp_tls"])
	require.Equal(t, false, body["smtp_password_set"])

	resp, body := h.do("PUT", "/api/v1/fleet/settings", smtpSettings())
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, "mail.example.com", body["smtp_host"])
	require.Equal(t, float64(587), body["smtp_port"])
	require.Equal(t, "fleet@example.com", body["smtp_from"])
	require.Equal(t, true, body["smtp_password_set"])
	require.NotContains(t, body, "smtp_password")

	for k, v := range body {
		require.NotEqual(t, "s3cret", v, "key %s leaks the password", k)
	}

	// At rest it is sealed, and it unseals back to what was sent.
	st, err := store.Open(fleet.PathsFor(h.stateDir).DB)
	require.NoError(t, err)

	defer st.Close()

	stored, err := st.Setting(ctx, "sealed_smtp_password")
	require.NoError(t, err)
	require.NotEmpty(t, stored)
	require.NotContains(t, stored, "s3cret")
	require.Empty(t, mustSetting(t, st, "smtp_password"), "the plaintext key is never written")

	cfg, err := h.s.MailConfigForTesting(ctx)
	require.NoError(t, err)
	require.Equal(t, "s3cret", cfg.Password)
	require.Equal(t, "mail.example.com", cfg.Host)

	// "" leaves the password alone; null clears it.
	_, body = h.do("PUT", "/api/v1/fleet/settings", map[string]any{"smtp_password": ""})
	require.Equal(t, true, body["smtp_password_set"])

	cfg, err = h.s.MailConfigForTesting(ctx)
	require.NoError(t, err)
	require.Equal(t, "s3cret", cfg.Password)

	_, body = h.do("PUT", "/api/v1/fleet/settings", map[string]any{"smtp_password": nil})
	require.Equal(t, false, body["smtp_password_set"])

	cfg, err = h.s.MailConfigForTesting(ctx)
	require.NoError(t, err)
	require.Empty(t, cfg.Password)
}

func mustSetting(t *testing.T, st *store.Store, key string) string {
	t.Helper()

	v, err := st.Setting(context.Background(), key)
	require.NoError(t, err)

	return v
}

func TestSMTPSettingsRejectBadValues(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	for name, in := range map[string]map[string]any{
		"unknown smtp key":  {"smtp_sealed_password": "x"},
		"sealed key direct": {"sealed_smtp_password": "x"},
		"port not offered":  {"smtp_port": 25},
		"port not a number": {"smtp_port": "587"},
		"host with scheme":  {"smtp_host": "smtp://mail.example.com"},
		"host with space":   {"smtp_host": "mail example com"},
		"host not a string": {"smtp_host": 7},
		"from not an addr":  {"smtp_from": "not-an-address"},
		"from injected":     {"smtp_from": "a@example.com\r\nBcc: b@example.com"},
		"username injected": {"smtp_username": "u\nX: y"},
		"tls not a bool":    {"smtp_tls": "yes"},
		"password not text": {"smtp_password": 7},
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := h.do("PUT", "/api/v1/fleet/settings", in)
			require.Equal(t, 400, resp.StatusCode)
			require.NotEmpty(t, body["error"])
		})
	}
}

func TestSMTPTestSendReportsTheRawErrorAndIsRateLimited(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	// Admin-only, like every other admin POST.
	saved := h.jar
	h.jar = nil
	resp, _ := h.do("POST", "/api/v1/fleet/settings/smtp/test", map[string]any{"to": "ops@example.com"})
	require.Equal(t, 401, resp.StatusCode)

	h.jar = saved

	// A CSRF-less call is rejected before anything is sent.
	req := h.newRequest("POST", "/api/v1/fleet/settings/smtp/test", jsonBody(map[string]any{"to": "ops@example.com"}))
	req.Header.Del(csrfHeaderName)
	noCSRF, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	noCSRF.Body.Close()
	require.Equal(t, 403, noCSRF.StatusCode)

	// Point at a port nothing answers on: the dial error comes back verbatim.
	resp, body := h.do("PUT", "/api/v1/fleet/settings", map[string]any{"smtp_host": "127.0.0.1", "smtp_port": 465, "smtp_from": "fleet@example.com"})
	require.Equal(t, 200, resp.StatusCode, body)

	resp, body = h.do("POST", "/api/v1/fleet/settings/smtp/test", map[string]any{"to": "ops@example.com"})
	require.Equal(t, 400, resp.StatusCode)
	require.Contains(t, body["error"], "127.0.0.1:465", "the raw SMTP error")

	resp, body = h.do("POST", "/api/v1/fleet/settings/smtp/test", map[string]any{"to": "ops@example.com"})
	require.Equal(t, 429, resp.StatusCode, body)
}

// A sealed password that will not open (a restored DB, a rotated passphrase)
// must not take the settings screen down with it: the read path never
// unseals, so GET still answers with every other setting and "set".
func TestCorruptSealedPasswordLeavesTheSettingsReadable(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.activateAndLogin()
	resp, body := h.do("PUT", "/api/v1/fleet/settings", smtpSettings())
	require.Equal(t, 200, resp.StatusCode, body)

	st, err := store.Open(fleet.PathsFor(h.stateDir).DB)
	require.NoError(t, err)

	defer st.Close()

	sealed, err := st.Setting(ctx, "sealed_smtp_password")
	require.NoError(t, err)
	// Flip the last byte of the ciphertext: it is now unopenable but present.
	// XOR rather than an assignment, so it always differs from what was there.
	raw, err := hex.DecodeString(sealed)
	require.NoError(t, err)

	raw[len(raw)-1] ^= 0xff
	require.NoError(t, st.SetSetting(ctx, "sealed_smtp_password", hex.EncodeToString(raw)))

	resp, body = h.do("GET", "/api/v1/fleet/settings", nil)
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, true, body["smtp_password_set"], "the row is there, whatever it holds")
	require.Equal(t, "mail.example.com", body["smtp_host"], "every other setting is intact")
	require.Equal(t, float64(587), body["smtp_port"])
	require.Equal(t, "fleet@example.com", body["smtp_from"])

	// A PUT that touches something else must not unseal it either.
	resp, body = h.do("PUT", "/api/v1/fleet/settings", map[string]any{"smtp_username": "u2"})
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, "u2", body["smtp_username"])

	// The send path is the one that has to say what is wrong.
	resp, body = h.do("POST", "/api/v1/fleet/settings/smtp/test", map[string]any{"to": "ops@example.com"})
	require.Equal(t, 500, resp.StatusCode)
	require.Contains(t, body["error"], "could not be unsealed")
	require.Contains(t, body["error"], "re-enter it in Settings")
}

// Some relays quote the credentials they just rejected. Neither half may
// reach the admin's browser (or the fleet log) in the surfaced error.
func TestTestSendRedactsBothCredentialsFromTheServersError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Rejected on the greeting, quoting both credentials back.
			c.Write([]byte("554 5.7.1 rejected: user=leaky-user pass=leaky-pass\r\n")) //nolint:errcheck
			c.Close()                                                                  //nolint:errcheck
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	// The port whitelist is what the UI offers; widen it for the stub.
	restore := mail.AllowedPorts
	mail.AllowedPorts = append(append([]int{}, restore...), port)
	t.Cleanup(func() { mail.AllowedPorts = restore })

	h := newHarness(t)
	h.activateAndLogin()
	resp, body := h.do("PUT", "/api/v1/fleet/settings", map[string]any{
		"smtp_host": "127.0.0.1", "smtp_port": port, "smtp_username": "leaky-user",
		"smtp_password": "leaky-pass", "smtp_from": "fleet@example.com",
	})
	require.Equal(t, 200, resp.StatusCode, body)

	resp, body = h.do("POST", "/api/v1/fleet/settings/smtp/test", map[string]any{"to": "ops@example.com"})
	require.Equal(t, 400, resp.StatusCode)

	msg, _ := body["error"].(string)
	require.Contains(t, msg, "554", "the raw error still reaches the admin")
	require.NotContains(t, msg, "leaky-pass")
	require.NotContains(t, msg, "leaky-user")
	require.Equal(t, 2, strings.Count(msg, "[redacted]"))
}
