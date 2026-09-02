package cli

import (
	"github.com/gorilla/mux"

	"github.com/kopia/kopia/internal/server"
)

// warphold: extra handler registration for the Fleet control plane.
var serverExtraHandlers []func(srv *server.Server, m *mux.Router, configFile string)

// RegisterServerHandlers adds a function that mounts routes when `server start` builds its router.
func RegisterServerHandlers(f func(srv *server.Server, m *mux.Router, configFile string)) {
	serverExtraHandlers = append(serverExtraHandlers, f)
}
