package api

import (
	_ "embed"
	"net/http"
	"text/template"

	"github.com/kopia/kopia/repo"
)

//go:embed enroll.sh.tmpl
var enrollShSrc string

var enrollSh = template.Must(template.New("enroll.sh").Parse(enrollShSrc))

func (s *Server) handleEnrollSh(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_ = enrollSh.Execute(w, map[string]string{
		"Server":  scheme + "://" + r.Host,
		"Token":   r.URL.Query().Get("token"),
		"Version": repo.BuildVersion,
	})
}
