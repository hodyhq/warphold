package api

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/gorilla/mux"
)

// dlName is the whole allow-list for GET /dl/{name}: os is fixed to linux,
// arch may be any bare alphanumeric token (not just the amd64/arm64 the
// server itself can serve - an operator may stage a build for another arch
// under <state dir>/binaries/), so a path-traversal payload like
// "warphold-linux-../x" never reaches the filesystem lookup below.
var dlName = regexp.MustCompile(`^warphold-(linux)-([a-z0-9]+)$`)

func (s *Server) mountDownload(m *mux.Router) {
	m.HandleFunc("/dl/{name}", s.requireHost(s.requireActivated(s.handleDownload))).Methods(http.MethodGet)
}

// handleDownload serves the WarpHold agent binary for <os>-<arch>: the
// server's own running executable when it matches runtime.GOOS/GOARCH,
// otherwise a build staged at <state dir>/binaries/warphold-<os>-<arch>.
// Gated by requireActivated only; the binary is not a secret, same as
// /enroll.sh.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	mm := dlName.FindStringSubmatch(name)
	if mm == nil {
		writeErr(w, http.StatusNotFound, "unknown binary name")
		return
	}
	path := ""
	if mm[1] == runtime.GOOS && mm[2] == runtime.GOARCH {
		if self, err := os.Executable(); err == nil {
			path = self
		}
	}
	if path == "" {
		cand := filepath.Join(s.paths.StateDir, "binaries", name)
		if st, err := os.Stat(cand); err == nil && st.Mode().IsRegular() {
			path = cand
		}
	}
	if path == "" {
		writeErr(w, http.StatusNotFound, "no binary for "+mm[1]+"-"+mm[2]+"; place one at <state dir>/binaries/")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="warphold"`)
	http.ServeFile(w, r, path)
}
