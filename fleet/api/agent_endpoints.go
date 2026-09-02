package api

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/fleet/store"
)

const defaultPollSeconds = 300

func (s *Server) mountAgent(m *mux.Router) {
	m.HandleFunc("/api/v1/fleet/enroll", s.requireActivated(s.handleEnroll)).Methods(http.MethodPost)
	m.HandleFunc("/enroll.sh", s.requireActivated(s.handleEnrollSh)).Methods(http.MethodGet)
	s.mountAgentPoll(m) // Task 14
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return "ag_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:10], nil
}

// NewBearer returns an agent bearer token and its hash.
func NewBearer() (string, []byte, error) {
	plain, _, err := enroll.NewToken()
	if err != nil {
		return "", nil, err
	}
	plain = "wa_" + plain[3:]
	return plain, enroll.HashToken(plain), nil
}

func (s *Server) provisioner() *enroll.Provisioner {
	host, _ := os.Hostname()
	return &enroll.Provisioner{B2: s.b2, Owner: "fleet@" + host}
}

func (s *Server) specFor(ctx context.Context, t *store.Target) (enroll.TargetSpec, error) {
	kid, key, err := s.targetCreds(ctx, t)
	if err != nil {
		return enroll.TargetSpec{}, err
	}
	return enroll.TargetSpec{Kind: t.Kind, Bucket: t.Bucket, Path: t.Path, AdminKeyID: kid, AdminKey: key}, nil
}

func (s *Server) bundleFor(_ context.Context, a *store.Agent) (*enroll.Bundle, error) {
	raw, err := s.key.Open(a.SealedBundle)
	if err != nil {
		return nil, err
	}
	var b enroll.Bundle
	return &b, json.Unmarshal(raw, &b)
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token, Hostname, OS, Arch, Version, Scope string }
	if err := decode(r, &in); err != nil || in.Token == "" || in.Hostname == "" {
		writeErr(w, http.StatusBadRequest, "token and hostname are required")
		return
	}
	if in.Scope == "" {
		in.Scope = "user"
	}
	ctx := r.Context()
	tok, err := s.tokens().Consume(ctx, in.Token)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	group, err := s.st.Group(ctx, tok.GroupID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "token's group is gone")
		return
	}
	target, err := s.st.Target(ctx, group.TargetID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "group's target is gone")
		return
	}
	spec, err := s.specFor(ctx, target)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, err := newID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	bundle, err := s.provisioner().Provision(ctx, spec, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provisioning failed: "+err.Error())
		return
	}
	sealedBundle, err := json.Marshal(bundle)
	if err == nil {
		sealedBundle, err = s.key.Seal(sealedBundle)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	bearer, bearerHash, err := NewBearer()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a := &store.Agent{ID: id, Name: in.Hostname, Hostname: in.Hostname, OS: in.OS, Arch: in.Arch, Version: in.Version, Scope: in.Scope, GroupID: group.ID, BearerHash: bearerHash, SealedBundle: sealedBundle, EnrolledAt: s.now()}
	if err := s.st.CreateAgent(ctx, a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"agent_id": id, "bearer": bearer, "name": a.Name,
		"connect_token": bundle.ConnectToken, "poll_interval_seconds": s.pollInterval(ctx),
	})
}

func (s *Server) pollInterval(ctx context.Context) int {
	v, _ := s.st.Setting(ctx, "poll_interval")
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return defaultPollSeconds
}

// mountAgentPoll is a no-op until Task 14 adds agent polling/reporting.
func (s *Server) mountAgentPoll(_ *mux.Router) {}
