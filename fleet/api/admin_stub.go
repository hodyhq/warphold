package api

import (
	"net/http"

	"github.com/gorilla/mux"
)

func (s *Server) mountAgent(_ *mux.Router) {}

func (s *Server) mountAdminAgents(_ *mux.Router, _ func(http.HandlerFunc) http.HandlerFunc) {} // Task 11 replaces
