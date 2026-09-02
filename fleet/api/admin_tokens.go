package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet/enroll"
)

func (s *Server) mountAdminEnrollment(m *mux.Router, adm func(http.HandlerFunc) http.HandlerFunc) {
	m.HandleFunc("/api/v1/fleet/tokens", adm(s.handleTokenCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/groups/{id}/tokens", adm(s.handleTokenList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/tokens/{id}/revoke", adm(s.handleTokenRevoke)).Methods(http.MethodPost)
	s.mountAdminAgents(m, adm) // Task 11
}

func (s *Server) tokens() *enroll.Tokens {
	tk := enroll.NewTokens(s.st)
	tk.SetNowForTesting(s.now)
	return tk
}

func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GroupID    int64 `json:"group_id"`
		TTLSeconds int64 `json:"ttl_seconds"`
		MaxUses    *int  `json:"max_uses"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	if _, err := s.store().Group(r.Context(), in.GroupID); err != nil {
		writeErr(w, http.StatusBadRequest, "unknown group_id")
		return
	}
	maxUses := -1
	if in.MaxUses != nil {
		maxUses = *in.MaxUses
	}
	// requireAdmin already verified the session cookie, so it is present here.
	c, _ := r.Cookie(sessionCookie)
	adminID, _ := s.signer().verify(c.Value)
	plain, tok, err := s.tokens().Issue(r.Context(), in.GroupID, time.Duration(in.TTLSeconds)*time.Second, maxUses, adminID)
	if err != nil {
		if errors.Is(err, enroll.ErrTTLTooLong) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		adminFailed(w, "issue token", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": tok.ID, "token": plain, "expires_at": tok.ExpiresAt, "max_uses": tok.MaxUses})
}

func (s *Server) handleTokenList(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	ts, err := s.store().TokensForGroup(r.Context(), id)
	if err != nil {
		adminFailed(w, "list tokens", err)
		return
	}
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, map[string]any{"id": t.ID, "expires_at": t.ExpiresAt, "max_uses": t.MaxUses, "uses": t.Uses, "revoked_at": t.RevokedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.store().RevokeToken(r.Context(), id, s.now()); err != nil {
		adminFailed(w, "revoke token", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
