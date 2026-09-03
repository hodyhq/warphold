package cli

import (
	"context"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/internal/server"
)

// warphold: extra handler registration for the Fleet control plane. A handler
// that returns an error stops `server start`: Fleet state that cannot be used
// safely (an unresolved pending sealing key) must not be served on.
var serverExtraHandlers []func(ctx context.Context, srv *server.Server, m *mux.Router, configFile string) error

// RegisterServerHandlers adds a function that mounts routes when `server start` builds its router.
func RegisterServerHandlers(f func(ctx context.Context, srv *server.Server, m *mux.Router, configFile string) error) {
	serverExtraHandlers = append(serverExtraHandlers, f)
}
