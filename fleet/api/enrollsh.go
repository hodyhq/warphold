package api

import (
	_ "embed"
	"net/http"
	"regexp"
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
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_ = enrollSh.Execute(w, map[string]string{
		"Server":  scheme + "://" + r.Host,
		"Token":   token,
		"Version": repo.BuildVersion,
	})
}
