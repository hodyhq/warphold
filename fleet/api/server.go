// Package api serves the Fleet control-plane HTTP API.
package api

// Server holds Fleet state for the HTTP handlers.
type Server struct {
	sess *sessions
}
