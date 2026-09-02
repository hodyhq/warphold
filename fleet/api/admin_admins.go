package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kopia/kopia/fleet/store"
)

// minPasswordLen matches the activation rule, so the second admin is held to
// the same bar as the first.
const minPasswordLen = 8

type adminOut struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) handleAdminList(w http.ResponseWriter, r *http.Request) {
	as, err := s.store().Admins(r.Context())
	if err != nil {
		adminFailed(w, "list admins", err)
		return
	}
	out := make([]adminOut, 0, len(as))
	for _, a := range as {
		// a.PWHash is deliberately not copied across.
		out = append(out, adminOut{ID: a.ID, Email: a.Email, Role: a.Role, CreatedAt: a.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAdminCreate adds an admin. There is one role today ("owner"), so any
// signed-in admin may add or remove admins; when a reader role arrives this is
// where the permission check goes.
func (s *Server) handleAdminCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil || !strings.Contains(in.Email, "@") || len(in.Password) < minPasswordLen {
		writeErr(w, http.StatusBadRequest, "email must be valid and password needs 8+ characters")
		return
	}
	st := s.store()
	_, err := st.AdminByEmail(r.Context(), in.Email)
	switch {
	case err == nil:
		writeErr(w, http.StatusConflict, "an admin with that email already exists")
		return
	case !errors.Is(err, store.ErrNotFound):
		adminFailed(w, "look up admin", err)
		return
	}
	hash, err := HashPassword(in.Password)
	if err != nil {
		adminFailed(w, "hash password", err)
		return
	}
	id, err := st.CreateAdmin(r.Context(), in.Email, hash, s.now())
	if err != nil {
		adminFailed(w, "create admin", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// handleAdminDelete removes an admin. The store refuses to delete the last
// one, and the delete cascades onto that admin's sessions, so a browser the
// deleted admin left signed in is locked out on its next request.
func (s *Server) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	switch err := s.store().DeleteAdmin(r.Context(), id); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrLastAdmin):
		writeErr(w, http.StatusConflict, "cannot delete the last admin")
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	default:
		adminFailed(w, "delete admin", err)
	}
}

// handleAdminPassword changes the signed-in admin's own password and signs
// every other browser of that admin out: a password change is what an admin
// does after a laptop goes missing, so the old sessions have to go with it.
func (s *Server) handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decode(r, &in); err != nil || len(in.New) < minPasswordLen {
		writeErr(w, http.StatusBadRequest, "new password needs 8+ characters")
		return
	}
	sess := sessionFrom(r)
	if sess == nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	st := s.store()
	a, err := st.AdminByID(r.Context(), sess.AdminID)
	if err != nil {
		adminFailed(w, "load admin", err)
		return
	}
	if !VerifyPassword(in.Current, a.PWHash) {
		writeErr(w, http.StatusUnauthorized, "wrong password")
		return
	}
	hash, err := HashPassword(in.New)
	if err != nil {
		adminFailed(w, "hash password", err)
		return
	}
	if err := st.UpdateAdminPassword(r.Context(), a.ID, hash); err != nil {
		adminFailed(w, "update password", err)
		return
	}
	if err := st.RevokeSessionsForAdminExcept(r.Context(), a.ID, sess.ID, s.now()); err != nil {
		adminFailed(w, "revoke sessions", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
