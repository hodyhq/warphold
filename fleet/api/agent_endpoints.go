package api

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/fleet/store"
)

const defaultPollSeconds = 300

// revokeTimeout bounds the detached B2 key hand-back after a failed enrollment.
const revokeTimeout = 30 * time.Second

// errCategory describes err without quoting it. Enrollment errors carry token
// material, B2 key ids and provisioning URLs in their text, and the fleet log
// is not a secret store, so only the concrete Go type of the error (or a
// coarse classification for the context errors) is ever logged.
func errCategory(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "context.Canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context.DeadlineExceeded"
	}
	return fmt.Sprintf("%T", err)
}

// enrollFailed answers an unauthenticated enroller with a fixed message and
// records the failing stage plus a safe error category in the server log. The
// endpoint runs before the agent has any credential, so neither the enroller
// nor the log gets internal error text.
func enrollFailed(w http.ResponseWriter, status int, public, stage string, err error) {
	log.Printf("warphold fleet: enroll failed at stage %s (%s)", stage, errCategory(err))
	writeErr(w, status, public)
}

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
		enrollFailed(w, http.StatusForbidden, "invalid or expired token", "token consume", err)
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
		enrollFailed(w, http.StatusInternalServerError, "enrollment failed", "target spec", err)
		return
	}
	id, err := newID()
	if err != nil {
		enrollFailed(w, http.StatusInternalServerError, "enrollment failed", "id", err)
		return
	}
	prov := s.provisioner()
	bundle, err := prov.Provision(ctx, spec, id)
	if err != nil {
		enrollFailed(w, http.StatusBadGateway, "provisioning failed", "provision", err)
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
		// Detached from the request: a client that hangs up mid-enrollment
		// cancels ctx, and that is precisely when the keys need handing back.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
		defer cancel()
		if err := prov.Revoke(rctx, spec, bundle); err != nil {
			// Agent id + stage is what makes an orphaned B2 key findable;
			// the error text itself may quote key material, so only its
			// category is logged.
			log.Printf("warphold fleet: enroll %s failed at stage revoke: b2 keys may be orphaned (%s)", id, errCategory(err))
		}
	}()
	sealedBundle, err := json.Marshal(bundle)
	if err == nil {
		sealedBundle, err = s.sealKey().Seal(sealedBundle)
	}
	if err != nil {
		enrollFailed(w, http.StatusInternalServerError, "enrollment failed", "seal bundle", err)
		return
	}
	bearer, bearerHash, err := NewBearer()
	if err != nil {
		enrollFailed(w, http.StatusInternalServerError, "enrollment failed", "bearer", err)
		return
	}
	a := &store.Agent{ID: id, Name: in.Hostname, Hostname: in.Hostname, OS: in.OS, Arch: in.Arch, Version: in.Version, Scope: in.Scope, GroupID: group.ID, BearerHash: bearerHash, SealedBundle: sealedBundle, EnrolledAt: s.now()}
	if err := s.store().CreateAgent(ctx, a); err != nil {
		enrollFailed(w, http.StatusInternalServerError, "enrollment failed", "create agent", err)
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
