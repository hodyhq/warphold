package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/kopia/kopia/agent/poll"
	"github.com/kopia/kopia/fleet/store"
)

// policyDocFor renders the agent's group template into the wire document (commands are added by the caller).
func (s *Server) policyDocFor(ctx context.Context, a *store.Agent) (*poll.PolicyDoc, error) {
	g, err := s.store().Group(ctx, a.GroupID)
	if err != nil {
		return nil, err
	}
	tpl, err := s.store().Template(ctx, g.TemplateID)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(tpl.ID, 10)))
	h.Write(tpl.PolicyJSON)
	srcJSON, _ := json.Marshal(tpl.Sources)
	h.Write(srcJSON)
	h.Write([]byte(a.Name))
	// The interval is part of the document, so it must be part of the hash:
	// otherwise handlePoll answers 304 after an admin changes it and the new
	// interval never reaches an enrolled agent.
	interval := s.pollInterval(ctx)
	h.Write([]byte(strconv.Itoa(interval)))
	doc := &poll.PolicyDoc{ETag: hex.EncodeToString(h.Sum(nil))[:16], Name: a.Name, Commands: []poll.Command{}, PollIntervalSeconds: interval}
	for _, p := range tpl.Sources {
		doc.Sources = append(doc.Sources, poll.Source{Path: p, Policy: tpl.PolicyJSON})
	}
	return doc, nil
}
