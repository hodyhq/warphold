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

// enrollShToken and enrollShHost bound what handleEnrollSh will interpolate
// into the shell script it serves: both values are attacker-controlled
// (query string, Host header) and go straight into a POSIX-sh template, so
// anything outside these shapes is rejected rather than escaped.
var (
	enrollShToken = regexp.MustCompile(`^wh_[A-Za-z0-9_-]{20,64}$`)
	enrollShHost  = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+)(:[0-9]{1,5})?$`)
)

func (s *Server) handleEnrollSh(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if !enrollShToken.MatchString(token) || !enrollShHost.MatchString(r.Host) {
		writeErr(w, http.StatusBadRequest, "invalid token or host")
		return
	}
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_ = enrollSh.Execute(w, map[string]string{
		"Server":  scheme + "://" + r.Host,
		"Token":   token,
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
