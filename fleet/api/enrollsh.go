package api

import (
	_ "embed"
	"net/http"
	"regexp"
	"strings"
	"text/template"

	"github.com/kopia/kopia/repo"
)

//go:embed enroll.sh.tmpl
var enrollShSrc string

var enrollSh = template.Must(template.New("enroll.sh").Parse(enrollShSrc))

// enrollShHost bounds the one attacker-controlled value handleEnrollSh
// interpolates into the shell script it serves: the Host header goes straight
// into a POSIX-sh template, so anything outside this shape is rejected rather
// than escaped. The enrollment token is deliberately NOT templated in — the
// script is static and takes `--token` from its arguments, so the token never
// travels in a URL or in an HTTP response body.
var enrollShHost = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+)(:[0-9]{1,5})?$`)

// handleEnrollSh serves a static installer. It is intentionally unauthenticated:
// the script contains no secret (the enrollment token is supplied by the device
// itself via `sh -s -- --token …`, never embedded here and never placed in a
// URL), and a fresh, unenrolled device has no admin session to authenticate with.
func (s *Server) handleEnrollSh(w http.ResponseWriter, r *http.Request) {
	if !enrollShHost.MatchString(r.Host) {
		writeErr(w, http.StatusBadRequest, "invalid host")
		return
	}
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_ = enrollSh.Execute(w, map[string]string{
		"Server":  scheme + "://" + r.Host,
		"Version": repo.BuildVersion,
	})
}

// requestIsHTTPS reports whether the client reached the server over HTTPS,
// either directly or through a TLS-terminating reverse proxy that sets
// X-Forwarded-Proto. Fleet is expected to run behind such a proxy, where
// r.TLS alone is always nil; the header is only as trustworthy as whatever
// terminates TLS in front of this process, and getting it wrong drops the
// Secure attribute on the session cookie rather than granting anything.
func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
