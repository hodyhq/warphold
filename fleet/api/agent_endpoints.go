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
	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/fleet/store"
)

const defaultPollSeconds = 300

// revokeTimeout bounds the detached B2 key hand-back after a failed enrollment.
const revokeTimeout = 30 * time.Second

// maxReportStderr caps the agent-supplied stderr stored per report.
const maxReportStderr = 8 << 10

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

// agentFailed answers an authenticated agent with a fixed message and records
// the failing stage plus a safe error category in the server log. Store errors
// quote SQL, file paths and column values; an agent is a device on someone's
// LAN, not an operator, so it gets none of that.
func agentFailed(w http.ResponseWriter, stage string, err error) {
	log.Printf("warphold fleet: agent request failed at stage %s (%s)", stage, errCategory(err))
	writeErr(w, http.StatusInternalServerError, "internal error")
}

// adminFailed answers a signed-in admin with a fixed message and records the
// failing stage plus a safe error category in the server log. Store and
// provisioning errors quote SQL, file paths, column values and B2 response
// text; the admin UI has no use for any of it and the browser is the wrong
// place to leak it, so the operator reads the stage out of the fleet log.
func adminFailed(w http.ResponseWriter, stage string, err error) {
	log.Printf("warphold fleet: admin request failed at stage %s (%s)", stage, errCategory(err))
	writeErr(w, http.StatusInternalServerError, "internal error")
}

func (s *Server) mountAgent(m *mux.Router) {
	m.HandleFunc("/api/v1/fleet/enroll", s.requireHost(s.requireActivated(s.handleEnroll))).Methods(http.MethodPost)
	m.HandleFunc("/enroll.sh", s.requireHost(s.requireActivated(s.handleEnrollSh))).Methods(http.MethodGet)
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

// errPublicURLUnset is what specFor answers for a hosted target while
// public_url is empty: the device would be handed an S3 endpoint we cannot
// name. Enrollment turns it into a 409, the same answer token issuing gives.
var errPublicURLUnset = errors.New("public_url is not set")

func (s *Server) provisioner(ctx context.Context, t *store.Target) *enroll.Provisioner {
	// The maintenance owner devices must never be: it is the public host when
	// there is one, so a repository names the Fleet that owns it rather than
	// whatever the server's hostname happens to be (spec 7.1 step 5).
	host, _ := os.Hostname()
	if u, ok := s.PublicURL(ctx); ok {
		host = hostOnly(u.Host)
	}
	p := &enroll.Provisioner{B2: s.b2, Owner: "fleet@" + host, Store: s.store(), SealKey: s.sealKey(), Now: s.now}
	// A cloud-direct target has no root path to provision into: the fleet
	// writes through to the customer's bucket. Provisioning borrows the very
	// backend the gateway serves this target's devices from, so both agree on
	// the bucket, the root prefix and the credentials by construction.
	if t != nil && t.Kind == "hosted" && t.StorageMode == "cloud" {
		p.HostedCloudStore = func(ctx context.Context) (gateway.ObjectStore, error) { return s.targetStore(ctx, *t) }
	}
	return p
}

func (s *Server) specFor(ctx context.Context, t *store.Target) (enroll.TargetSpec, error) {
	kid, key, err := s.targetCreds(ctx, t)
	if err != nil {
		return enroll.TargetSpec{}, err
	}
	spec := enroll.TargetSpec{Kind: t.Kind, Bucket: t.Bucket, Path: t.Path, AdminKeyID: kid, AdminKey: key}
	if t.Kind == "hosted" {
		u, ok := s.PublicURL(ctx)
		if !ok {
			return enroll.TargetSpec{}, errPublicURLUnset
		}
		spec.StorageMode, spec.HostedRoot = t.StorageMode, t.Path
		spec.PublicHost, spec.TLS = u.Host, u.Scheme == "https"
		// The same region the mounted gateway verifies signatures against;
		// mountGateway leaves Config.Region empty, so this is that default.
		spec.Region = gateway.DefaultRegion
	}
	return spec, nil
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
		// Only a rejected token is the enroller's fault; a store failure is
		// ours and must not be reported as "your token is bad".
		if errors.Is(err, enroll.ErrTokenInvalid) {
			enrollFailed(w, http.StatusForbidden, "invalid or expired token", "token consume", err)
			return
		}
		enrollFailed(w, http.StatusInternalServerError, "enrollment failed", "token consume", err)
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
		if errors.Is(err, errPublicURLUnset) {
			enrollFailed(w, http.StatusConflict, "set the public URL before enrolling devices on a hosted target", "target spec", err)
			return
		}
		enrollFailed(w, http.StatusInternalServerError, "enrollment failed", "target spec", err)
		return
	}
	id, err := newID()
	if err != nil {
		enrollFailed(w, http.StatusInternalServerError, "enrollment failed", "id", err)
		return
	}
	if s.enrollIDHook != nil {
		s.enrollIDHook(id)
	}
	bearer, bearerHash, err := NewBearer()
	if err != nil {
		enrollFailed(w, http.StatusInternalServerError, "enrollment failed", "bearer", err)
		return
	}
	// The agent row goes in before provisioning: a hosted device's gateway
	// credential is a device_keys row whose agent_id references it, and the
	// foreign key is enforced. The bundle lands in a moment, once there is
	// one; until then the row is unusable - its bearer has not been returned.
	a := &store.Agent{ID: id, Name: in.Hostname, Hostname: in.Hostname, OS: in.OS, Arch: in.Arch, Version: in.Version, Scope: in.Scope, GroupID: group.ID, BearerHash: bearerHash, SealedBundle: []byte{}, EnrolledAt: s.now()}
	if err := s.store().CreateAgent(ctx, a); err != nil {
		enrollFailed(w, http.StatusInternalServerError, "enrollment failed", "create agent", err)
		return
	}
	// Every failure from here on must hand back what has been minted - the
	// agent's B2 keys, its gateway key, the half-finished row - or they
	// outlive an enrollment that never completed while the one-shot token is
	// already spent.
	prov := s.provisioner(ctx, target)
	enrolled := false
	var bundle *enroll.Bundle
	defer func() {
		if enrolled {
			return
		}
		// Detached from the request: a client that hangs up mid-enrollment
		// cancels ctx, and that is precisely when the keys need handing back.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
		defer cancel()
		if bundle != nil {
			if err := prov.Revoke(rctx, spec, bundle); err != nil {
				// Agent id + stage is what makes an orphaned B2 key findable;
				// the error text itself may quote key material, so only its
				// category is logged.
				log.Printf("warphold fleet: enroll %s failed at stage revoke: b2 keys may be orphaned (%s)", id, errCategory(err))
			}
		}
		// Removes the gateway key with the agent, so a failed enrollment
		// leaves no credential and no half-agent behind.
		if err := s.store().DeleteAgent(rctx, id); err != nil {
			log.Printf("warphold fleet: enroll %s failed at stage cleanup: agent row may remain (%s)", id, errCategory(err))
		}
		// And the repository it was given, which nothing else would collect:
		// the reap job only ever sees agents that finished enrolling.
		if err := enroll.RemoveHostedRepository(spec, id); err != nil {
			log.Printf("warphold fleet: enroll %s failed at stage cleanup: repository may remain (%s)", id, errCategory(err))
		}
	}()
	bundle, err = prov.Provision(ctx, spec, id)
	if err != nil {
		enrollFailed(w, http.StatusBadGateway, "provisioning failed", "provision", err)
		return
	}
	sealedBundle, err := json.Marshal(bundle)
	if err == nil {
		sealedBundle, err = s.sealKey().Seal(sealedBundle)
	}
	if err != nil {
		enrollFailed(w, http.StatusInternalServerError, "enrollment failed", "seal bundle", err)
		return
	}
	if err := s.store().SetAgentBundle(ctx, id, sealedBundle); err != nil {
		enrollFailed(w, http.StatusInternalServerError, "enrollment failed", "store bundle", err)
		return
	}
	enrolled = true
	writeJSON(w, http.StatusCreated, map[string]any{
		"agent_id": id, "bearer": bearer, "name": a.Name,
		"connect_token": bundle.ConnectToken, "poll_interval_seconds": s.pollInterval(ctx),
	})
}

func (s *Server) pollInterval(ctx context.Context) int {
	v, _ := s.store().Setting(ctx, pollIntervalSetting)
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return defaultPollSeconds
}

func (s *Server) mountAgentPoll(m *mux.Router) {
	m.HandleFunc("/api/v1/fleet/agent/poll", s.requireHost(s.requireActivated(s.requireAgent(s.handlePoll)))).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/agent/report", s.requireHost(s.requireActivated(s.requireAgent(s.handleReport)))).Methods(http.MethodPost)
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
		agentFailed(w, "policy doc", err)
		return
	}
	version := in.Heartbeat.Version
	if version == "" {
		version = a.Version
	}
	// in.ETag, not doc.ETag: the agent has not applied this poll's policy yet.
	// The applied version is whatever the agent reports back on its next poll,
	// so recording doc.ETag here would show the fleet a policy as applied that
	// the agent may never receive (304, dropped connection, crash on apply).
	_ = s.store().TouchAgent(ctx, a.ID, s.now(), version, in.ETag)
	// Before the 304 decision, not after: a failed lookup leaves pending empty,
	// which would make an unchanged-policy poll answer 304 and silently drop
	// the agent's queued commands until something else changed the ETag.
	pending, err := s.store().PendingCommands(ctx, a.ID)
	if err != nil {
		agentFailed(w, "pending commands", err)
		return
	}
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
	// Stderr is agent-controlled free text shown to admins; cap it so a chatty
	// or hostile agent cannot bloat the store or the overview payload.
	if len(in.Stderr) > maxReportStderr {
		in.Stderr = in.Stderr[:maxReportStderr]
	}
	if _, err := s.store().AddReport(ctx, &store.Report{AgentID: a.ID, TaskID: in.TaskID, Kind: in.Kind, Source: in.Source, StartedAt: in.StartedAt, FinishedAt: in.FinishedAt, Status: in.Status, Bytes: in.Bytes, Files: in.Files, SnapshotID: in.SnapshotID, Stderr: in.Stderr}); err != nil {
		agentFailed(w, "add report", err)
		return
	}
	if in.CommandID != 0 {
		// The report is already stored above; a retried report for an
		// already-acked command hits ErrNotFound here and that is fine
		// (idempotent). Any other error means the command stays pending and
		// gets redelivered, so the agent must not see a success response.
		if err := s.store().AckCommand(ctx, in.CommandID, a.ID, s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
			agentFailed(w, "ack command", err)
			return
		}
	}
	_ = s.store().TouchAgent(ctx, a.ID, s.now(), a.Version, a.PolicyETag)
	w.WriteHeader(http.StatusNoContent)
}
