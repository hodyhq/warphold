package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kopia/kopia/fleet/mail"
	"github.com/kopia/kopia/fleet/seal"
)

const (
	// smtpTestWindow throttles the test send to one per admin per window. It
	// is an authenticated endpoint, so this is not an abuse control: it stops
	// an impatient double-click from queueing two connections to a relay that
	// counts them, and a held-down button from hammering it.
	smtpTestWindow  = 10 * time.Second
	smtpTestSubject = "WarpHold test email"
	smtpTestText    = "This is a test message from your WarpHold Fleet server. If you are reading it, the SMTP settings work."
	smtpTestHTML    = "<p>This is a test message from your WarpHold Fleet server.</p><p>If you are reading it, the SMTP settings work.</p>"
)

// handleSMTPTest sends one message with the stored settings and reports the
// server's own error verbatim on failure: "550 5.7.1 relaying denied" is the
// whole diagnosis, and the generic adminFailed message would throw it away.
// The password is stripped from that text in case the server echoed it back.
func (s *Server) handleSMTPTest(w http.ResponseWriter, r *http.Request) {
	if !s.smtpTest.allow(strconv.FormatInt(adminFrom(r), 10)) {
		writeErr(w, http.StatusTooManyRequests, "wait a few seconds before sending another test email")
		return
	}
	var in struct {
		To string `json:"to"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	cfg, err := mail.Load(r.Context(), s.store(), s.sealKey())
	if err != nil {
		// The one error worth naming: the stored password does not open with
		// this fleet's key (a restored DB, a rotated passphrase). The fix is
		// to re-enter it, and no other message would say so.
		if errors.Is(err, seal.ErrTampered) {
			writeErr(w, http.StatusInternalServerError, "the stored SMTP password could not be unsealed; re-enter it in Settings")
			return
		}
		adminFailed(w, "read smtp settings", err)
		return
	}
	if err := mail.Send(r.Context(), cfg, []string{in.To}, smtpTestSubject, smtpTestText, smtpTestHTML); err != nil {
		writeErr(w, http.StatusBadRequest, redactCredentials(err.Error(), cfg.Username, cfg.Password))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true})
}

// redactCredentials keeps both halves of the SMTP login out of an error text
// that quotes the command it failed on - some relays echo the rejected
// username, and a few echo the whole AUTH argument.
func redactCredentials(msg, username, password string) string {
	for _, secret := range []string{password, username} {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	return msg
}
