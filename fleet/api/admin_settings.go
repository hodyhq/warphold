package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// pollIntervalSetting is how often an agent checks in, in seconds; it is read
// by pollInterval and handed to the agent in its policy document.
const pollIntervalSetting = "poll_interval"

const (
	maxFleetNameLen = 64
	// The bounds keep a fleet from either hammering the server or going
	// effectively silent; the agent's own default sits inside them.
	minPollSeconds = 15
	maxPollSeconds = 3600
)

// settingsOut is the whole of the settings API surface. The settings table
// also holds seal_salt, which the escrow depends on, so the endpoint reads and
// writes these two keys by name rather than passing the table through.
type settingsOut struct {
	FleetName    string `json:"fleet_name"`
	PollInterval int    `json:"poll_interval"`
}

func (s *Server) currentSettings(ctx context.Context) (settingsOut, error) {
	name, err := s.store().Setting(ctx, fleetNameSetting)
	if err != nil {
		return settingsOut{}, err
	}
	return settingsOut{FleetName: name, PollInterval: s.pollInterval(ctx)}, nil
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	out, err := s.currentSettings(r.Context())
	if err != nil {
		adminFailed(w, "read settings", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSettingsUpdate applies a partial object: only the keys present are
// written, and any key outside the whitelist is an error rather than a
// silently ignored field, so a typo cannot look like a saved setting.
func (s *Server) handleSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	var in map[string]json.RawMessage
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	writes := make(map[string]string, len(in))
	for key, raw := range in {
		switch key {
		case fleetNameSetting:
			var name string
			if err := json.Unmarshal(raw, &name); err != nil {
				writeErr(w, http.StatusBadRequest, "fleet_name must be a string")
				return
			}
			name = strings.TrimSpace(name)
			if utf8.RuneCountInString(name) > maxFleetNameLen {
				writeErr(w, http.StatusBadRequest, "fleet_name must be at most "+strconv.Itoa(maxFleetNameLen)+" characters")
				return
			}
			writes[key] = name
		case pollIntervalSetting:
			var secs int
			if err := json.Unmarshal(raw, &secs); err != nil {
				writeErr(w, http.StatusBadRequest, "poll_interval must be a whole number of seconds")
				return
			}
			if secs < minPollSeconds || secs > maxPollSeconds {
				writeErr(w, http.StatusBadRequest, "poll_interval must be between "+strconv.Itoa(minPollSeconds)+" and "+strconv.Itoa(maxPollSeconds)+" seconds")
				return
			}
			writes[key] = strconv.Itoa(secs)
		default:
			writeErr(w, http.StatusBadRequest, "unknown setting: "+key)
			return
		}
	}
	if err := s.store().SetSettings(r.Context(), writes); err != nil {
		adminFailed(w, "write settings", err)
		return
	}
	out, err := s.currentSettings(r.Context())
	if err != nil {
		adminFailed(w, "read settings", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
