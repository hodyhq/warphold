package api

import "github.com/gorilla/mux"

func (s *Server) mountAdmin(_ *mux.Router) {}
func (s *Server) mountAgent(_ *mux.Router) {}
