package api

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/agent/poll"
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
	raw, err := s.sealKey().Open(a.SealedBundle)
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
	group, err := s.store().Group(ctx, tok.GroupID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "token's group is gone")
		return
	}
	target, err := s.store().Target(ctx, group.TargetID)
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
	prov := s.provisioner()
	bundle, err := prov.Provision(ctx, spec, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provisioning failed: "+err.Error())
		return
	}
	// Provision has already created the agent's B2 keys. Every failure from
	// here on must hand them back, or the keys outlive an enrollment that
	// never completed while the one-shot token is already spent.
	enrolled := false
	defer func() {
		if enrolled {
			return
		}
		if err := prov.Revoke(ctx, spec, bundle); err != nil {
			log.Printf("warphold fleet: enroll %s failed and its b2 keys could not be revoked: %v", id, err)
		}
	}()
	sealedBundle, err := json.Marshal(bundle)
	if err == nil {
		sealedBundle, err = s.sealKey().Seal(sealedBundle)
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
	if err := s.store().CreateAgent(ctx, a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	enrolled = true
	writeJSON(w, http.StatusCreated, map[string]any{
		"agent_id": id, "bearer": bearer, "name": a.Name,
		"connect_token": bundle.ConnectToken, "poll_interval_seconds": s.pollInterval(ctx),
	})
}

func (s *Server) pollInterval(ctx context.Context) int {
	v, _ := s.store().Setting(ctx, "poll_interval")
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return defaultPollSeconds
}

func (s *Server) mountAgentPoll(m *mux.Router) {
	m.HandleFunc("/api/v1/fleet/agent/poll", s.requireActivated(s.requireAgent(s.handlePoll))).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/agent/report", s.requireActivated(s.requireAgent(s.handleReport))).Methods(http.MethodPost)
}

type agentKey struct{}

// requireAgent authenticates the bearer token and rejects revoked agents.
func (s *Server) requireAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer == "" || bearer == r.Header.Get("Authorization") {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		a, err := s.store().AgentByBearerHash(r.Context(), enroll.HashToken(bearer))
		if err != nil || a.RevokedAt != nil {
			writeErr(w, http.StatusUnauthorized, "unknown or revoked agent")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, a)))
	}
}

func agentFrom(r *http.Request) *store.Agent { return r.Context().Value(agentKey{}).(*store.Agent) }

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	var in struct {
		ETag      string         `json:"etag"`
		Heartbeat poll.Heartbeat `json:"heartbeat"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	ctx := r.Context()
	doc, err := s.policyDocFor(ctx, a)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	version := in.Heartbeat.Version
	if version == "" {
		version = a.Version
	}
	_ = s.store().TouchAgent(ctx, a.ID, s.now(), version, doc.ETag)
	pending, _ := s.store().PendingCommands(ctx, a.ID)
	for _, c := range pending {
		doc.Commands = append(doc.Commands, poll.Command{ID: c.ID, Kind: c.Kind, Source: c.Source})
	}
	if in.ETag == doc.ETag && len(pending) == 0 {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	var in poll.Report
	if err := decode(r, &in); err != nil || in.TaskID == "" || in.Kind == "" || in.Status == "" {
		writeErr(w, http.StatusBadRequest, "task_id, kind and status are required")
		return
	}
	ctx := r.Context()
	if in.CommandID != 0 {
		owner, err := s.store().CommandAgentID(ctx, in.CommandID)
		if err != nil || owner != a.ID {
			writeErr(w, http.StatusBadRequest, "command_id does not belong to this agent")
			return
		}
	}
	if _, err := s.store().AddReport(ctx, &store.Report{AgentID: a.ID, TaskID: in.TaskID, Kind: in.Kind, Source: in.Source, StartedAt: in.StartedAt, FinishedAt: in.FinishedAt, Status: in.Status, Bytes: in.Bytes, Files: in.Files, SnapshotID: in.SnapshotID, Stderr: in.Stderr}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if in.CommandID != 0 {
		_ = s.store().AckCommand(ctx, in.CommandID, a.ID, s.now())
	}
	_ = s.store().TouchAgent(ctx, a.ID, s.now(), a.Version, a.PolicyETag)
	w.WriteHeader(http.StatusNoContent)
}
