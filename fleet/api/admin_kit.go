package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/kopia/kopia/fleet/enroll"
	"github.com/kopia/kopia/fleet/kit"
	"github.com/kopia/kopia/fleet/store"
)

func (s *Server) mountAdminKit(m *mux.Router, adm func(http.HandlerFunc) http.HandlerFunc) {
	// sealHeld on both: each reaches readOnlyKey, whose mint branch seals the
	// new credential and then writes it to device_keys. A rotation committing
	// between those two statements would leave the row sealed under the
	// retired key AFTER Reseal had already swept device_keys, and nothing
	// re-seals it later -- the printed kit's credential would 403 forever,
	// with no error at the moment it was created. Same span, same one-line
	// guard as handleTargetCreate and PUT /targets/{id}/mirror.
	m.HandleFunc("/api/v1/fleet/agents/{id}/kit", adm(s.sealHeld(s.handleAgentKit))).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/fleet/agents/{id}/kit/regenerate", adm(s.sealHeld(s.handleAgentKitRegenerate))).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/fleet/agents/{id}/kit/ack", adm(s.handleAgentKitAck)).Methods(http.MethodPost)
}

// handleAgentKit renders the printable recovery kit. It is a GET, so it is
// exempt from the CSRF double submit, and it is safe to repeat: a hosted
// device's read-only gateway key is minted once and then reused, so the page
// is identical every time until an admin regenerates it. That is what makes a
// rate limit unnecessary -- nothing is minted on a repeat GET. A first GET does
// mint a key, which is only safe on a GET because the admin session cookie is
// SameSite=Strict: a cross-site navigation never carries it here (pinned in
// admins_test.go). A revoked device still gets a kit on purpose (restore after
// revoke is a real need); the M5 reap job refuses one once retired_at is set.
//
// The page is never persisted server-side and never logged; it exists only in
// the response body.
func (s *Server) handleAgentKit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	a, err := s.store().Agent(ctx, mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}

	d, err := s.kitData(ctx, a, false)
	if err != nil {
		adminFailed(w, "build recovery kit", err)
		return
	}

	var buf bytes.Buffer
	if err := kit.Render(&buf, d); err != nil {
		adminFailed(w, "render recovery kit", err)
		return
	}

	// no-store, not no-cache: a page carrying the repository password must not
	// sit in a disk cache or in a proxy on the way back.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `inline; filename="warphold-recovery-kit.html"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The page carries the repository password, so it must not be framable and
	// must load nothing: its own inline stylesheet is the only resource there
	// is, which is what makes this policy as tight as it looks.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// handleAgentKitRegenerate disables the device's current read-only key and
// mints a fresh one, so a kit that walked out of the building can be retired
// without touching the credential the device is backing up with.
func (s *Server) handleAgentKitRegenerate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	a, err := s.store().Agent(ctx, mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}

	// Only a hosted target has a per-device read-only key to invalidate. A
	// filesystem or b2 kit carries the target's own reader credential, which
	// this cannot rotate, so answering 204 would tell an admin who just lost a
	// printed kit that it had been retired when nothing had happened.
	t, err := s.targetForAgent(ctx, *a)
	if err != nil {
		adminFailed(w, "read the device's target", err)
		return
	}

	if t.Kind != "hosted" {
		writeErr(w, http.StatusConflict, "only devices on a hosted target have a kit key to regenerate")
		return
	}

	if _, err := s.kitData(ctx, a, true); err != nil {
		adminFailed(w, "regenerate recovery kit key", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleAgentKitAck records that this admin holds the printed kit.
func (s *Server) handleAgentKitAck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	a, err := s.store().Agent(ctx, mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}

	if err := s.store().SetKitAck(ctx, a.ID, adminFrom(r), s.now()); err != nil {
		adminFailed(w, "record kit ack", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// kitData assembles the page's contents from the agent's sealed bundle and its
// target. rotate forces a fresh read-only key for a hosted target.
func (s *Server) kitData(ctx context.Context, a *store.Agent, rotate bool) (kit.Data, error) {
	t, err := s.targetForAgent(ctx, *a)
	if err != nil {
		return kit.Data{}, err
	}

	b, err := s.bundleFor(ctx, a)
	if err != nil {
		return kit.Data{}, err
	}

	d := kit.Data{
		DeviceName: a.Name, DeviceID: a.ID, TargetKind: t.Kind,
		Prefix: b.Prefix, RepoPassword: b.Password, Generated: s.now(),
	}

	switch t.Kind {
	case "hosted":
		d.Endpoint, d.Bucket, d.Region = b.Endpoint, b.Bucket, b.Region
		// A new read-only credential, not the device's writing key: the key on
		// paper must not be able to write to or delete the backups it restores.
		if d.ReadKeyID, d.ReadKey, err = s.readOnlyKey(ctx, a.ID, b.Prefix, rotate); err != nil {
			return kit.Data{}, err
		}

	case "b2":
		d.Bucket, d.ReadKeyID, d.ReadKey = t.Bucket, b.ReaderKeyID, b.ReaderKey

	case "filesystem":
		// A filesystem target's "prefix" is the repository directory itself.
		d.Path, d.Prefix = b.Prefix, ""

	default:
		return kit.Data{}, errors.New("no recovery kit for target kind " + t.Kind)
	}

	d.Commands = kit.Commands(d)

	return d, nil
}

// readOnlyKey returns the agent's read-only gateway credential, minting one on
// first use. rotate disables whatever is there and mints a replacement.
//
// ponytail: the get-or-create is not one transaction, so two GETs racing on a
// device with no kit yet can mint two read-only keys. Both are read-only and
// confined to the same prefix, so the outcome is a spare credential rather
// than a hole, and regenerate disables every active read-only key, not just
// the one it read. Make it a single INSERT ... RETURNING under a transaction
// if a duplicate ever matters.
func (s *Server) readOnlyKey(ctx context.Context, agentID, prefix string, rotate bool) (string, string, error) {
	keys, err := s.store().DeviceKeysForAgent(ctx, agentID)
	if err != nil {
		return "", "", err
	}

	for _, k := range keys {
		if !k.ReadOnly || k.DisabledAt != nil {
			continue
		}

		if rotate {
			if err := s.store().DisableDeviceKey(ctx, k.AccessKeyID, s.now()); err != nil {
				return "", "", err
			}

			continue
		}

		secret, err := s.sealKey().Open(k.SealedSecret)
		if err != nil {
			return "", "", err
		}

		return k.AccessKeyID, string(secret), nil
	}

	if rotate {
		// The gateway caches an unsealed key for up to its TTL; the retired one
		// must stop working now, not in five minutes.
		if gw := s.gateway(); gw != nil {
			gw.InvalidateKeys(agentID)
		}
	}

	akid, secret, err := enroll.NewGatewayCredentials()
	if err != nil {
		return "", "", err
	}

	sealed, err := s.sealKey().Seal([]byte(secret))
	if err != nil {
		return "", "", err
	}

	if err := s.store().CreateDeviceKey(ctx, &store.DeviceKey{
		AccessKeyID: akid, AgentID: agentID, SealedSecret: sealed,
		Prefix: prefix, ReadOnly: true, CreatedAt: s.now(),
	}); err != nil {
		return "", "", err
	}

	return akid, secret, nil
}
