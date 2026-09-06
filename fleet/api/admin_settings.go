package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	netmail "net/mail"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kopia/kopia/fleet/gateway"
	"github.com/kopia/kopia/fleet/jobs"
	"github.com/kopia/kopia/fleet/mail"
)

// pollIntervalSetting is how often an agent checks in, in seconds; it is read
// by pollInterval and handed to the agent in its policy document.
const pollIntervalSetting = "poll_interval"

// smtpPasswordField is the write-only name the API takes for the SMTP
// password; it is stored sealed under mail.PasswordKey and read back only as
// the smtp_password_set flag.
const smtpPasswordField = "smtp_password"

const (
	maxFleetNameLen = 64
	// maxSMTPFieldLen bounds the free-text SMTP fields; a hostname or a
	// username longer than this is a paste accident.
	maxSMTPFieldLen = 255
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
	PublicURL    string `json:"public_url"`

	RevokedRetentionDays int `json:"revoked_retention_days"`

	TrustedProxies string `json:"trusted_proxies"`

	GatewayIPRate      int    `json:"gateway_ip_rate"`
	GatewayIPBurst     int    `json:"gateway_ip_burst"`
	GatewayDeviceRate  int    `json:"gateway_device_rate"`
	GatewayDeviceBurst int    `json:"gateway_device_burst"`
	SMTPHost           string `json:"smtp_host"`
	SMTPPort           int    `json:"smtp_port"`
	SMTPUsername       string `json:"smtp_username"`
	SMTPFrom           string `json:"smtp_from"`
	SMTPTLS            bool   `json:"smtp_tls"`
	// SMTPPasswordSet is all the UI ever learns about the password: the
	// value itself is sealed at rest and never leaves the server.
	SMTPPasswordSet bool `json:"smtp_password_set"`

	// JobIntervals is every scheduled job's cadence in seconds, keyed by the
	// setting name the scheduler reads it from, and carrying the default for
	// a kind nobody has configured. It is one object rather than a field per
	// kind on purpose: it is generated from fleet/jobs' own interval table, so
	// a job added there shows up here - and becomes writable below - with no
	// second edit that could be forgotten.
	JobIntervals map[string]int `json:"job_intervals"`
}

func (s *Server) currentSettings(ctx context.Context) (settingsOut, error) {
	name, err := s.store().Setting(ctx, fleetNameSetting)
	if err != nil {
		return settingsOut{}, err
	}

	pub, err := s.store().Setting(ctx, publicURLSetting)
	if err != nil {
		return settingsOut{}, err
	}

	proxies, err := s.store().Setting(ctx, trustedProxiesSetting)
	if err != nil {
		return settingsOut{}, err
	}

	// Settings applies the SMTP2GO-shaped defaults, so a fleet that has never
	// touched the mail settings still shows the host and port it would use.
	// It takes no sealing key: the settings screen reports whether a password
	// is stored, never what it is, so a corrupt or wrongly-keyed sealed value
	// cannot take the page down with it.
	sm, err := mail.Settings(ctx, s.store())
	if err != nil {
		return settingsOut{}, err
	}

	pwSet, err := mail.PasswordSet(ctx, s.store())
	if err != nil {
		return settingsOut{}, err
	}

	return settingsOut{
		FleetName:            name,
		PollInterval:         s.pollInterval(ctx),
		PublicURL:            pub,
		RevokedRetentionDays: s.revokedRetentionDays(ctx),
		TrustedProxies:       proxies,
		GatewayIPRate:        int(s.rateSetting(ctx, gatewayIPRateSetting)),
		GatewayIPBurst:       int(s.rateSetting(ctx, gatewayIPBurstSetting)),
		GatewayDeviceRate:    int(s.rateSetting(ctx, gatewayDeviceRateSetting)),
		GatewayDeviceBurst:   int(s.rateSetting(ctx, gatewayDeviceBurstSetting)),
		SMTPHost:             sm.Host,
		SMTPPort:             sm.Port,
		SMTPUsername:         sm.Username,
		SMTPFrom:             sm.From,
		SMTPTLS:              sm.TLS,
		SMTPPasswordSet:      pwSet,
		JobIntervals:         jobs.IntervalSeconds(ctx, s.store()),
	}, nil
}

// revokedRetentionDays reads the retention window the reap job will apply.
// The clamp lives in fleet/jobs, which owns the setting and the job that acts
// on it, so the API and the reaper cannot disagree about how long a revoked
// device's repository survives.
func (s *Server) revokedRetentionDays(ctx context.Context) int {
	return jobs.RevokedRetentionDays(ctx, s.store())
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
	// "verify" is a flag on the call, not a setting: it asks the server to
	// prove public_url reaches this Fleet before storing it.
	verify := false
	if raw, ok := in["verify"]; ok {
		if err := json.Unmarshal(raw, &verify); err != nil {
			writeErr(w, http.StatusBadRequest, "verify must be true or false")
			return
		}

		delete(in, "verify")
	}

	writes := make(map[string]string, len(in))
	for key, raw := range in {
		switch key {
		case publicURLSetting:
			var raw2 string
			if err := json.Unmarshal(raw, &raw2); err != nil {
				writeErr(w, http.StatusBadRequest, "public_url must be a string")
				return
			}

			u, err := parsePublicURL(raw2)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}

			if verify {
				if err := s.verifyPublicURL(r.Context(), u); err != nil {
					if pe, ok := errors.AsType[*proxyError](err); ok {
						writeJSON(w, http.StatusBadRequest, map[string]any{"error": pe.Error(), "proxy_requirements": proxyRequirements})
						return
					}

					adminFailed(w, "verify public_url", err)

					return
				}
			}

			writes[key] = u.String()
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
		case jobs.RevokedRetentionSetting:
			var days int
			if err := json.Unmarshal(raw, &days); err != nil {
				writeErr(w, http.StatusBadRequest, "revoked_retention_days must be a whole number of days")
				return
			}

			if days < jobs.MinRetentionDays || days > jobs.MaxRetentionDays {
				writeErr(w, http.StatusBadRequest, "revoked_retention_days must be between "+strconv.Itoa(jobs.MinRetentionDays)+" and "+strconv.Itoa(jobs.MaxRetentionDays))
				return
			}

			writes[key] = strconv.Itoa(days)
		case trustedProxiesSetting:
			var list string
			if err := json.Unmarshal(raw, &list); err != nil {
				writeErr(w, http.StatusBadRequest, "trusted_proxies must be a string")
				return
			}

			list = strings.TrimSpace(list)
			if list != "" {
				if _, err := gateway.ParseTrustedProxies(list); err != nil {
					// The offending value is not echoed; the example says what
					// the shape must be, which is what an admin needs.
					writeErr(w, http.StatusBadRequest,
						"trusted_proxies must be a comma-separated list of CIDRs or addresses, like \"10.0.0.0/8, 192.168.1.7\"")

					return
				}
			}

			writes[key] = list
		case mail.HostKey:
			var host string
			if err := json.Unmarshal(raw, &host); err != nil {
				writeErr(w, http.StatusBadRequest, "smtp_host must be a string")
				return
			}

			host = strings.TrimSpace(host)
			// A host, not a URL: anything with a scheme, a path, a port or
			// whitespace in it would be dialed verbatim and fail obscurely.
			if len(host) > maxSMTPFieldLen || strings.ContainsAny(host, " \t\r\n/:@") {
				writeErr(w, http.StatusBadRequest, "smtp_host must be a hostname, with no scheme, port or path")
				return
			}

			writes[key] = host
		case mail.PortKey:
			var port int
			if err := json.Unmarshal(raw, &port); err != nil {
				writeErr(w, http.StatusBadRequest, "smtp_port must be a number")
				return
			}

			if !slices.Contains(mail.AllowedPorts, port) {
				writeErr(w, http.StatusBadRequest, "smtp_port must be one of 2525, 587 or 465")
				return
			}

			writes[key] = strconv.Itoa(port)
		case mail.UsernameKey:
			var user string
			if err := json.Unmarshal(raw, &user); err != nil {
				writeErr(w, http.StatusBadRequest, "smtp_username must be a string")
				return
			}

			user = strings.TrimSpace(user)
			if len(user) > maxSMTPFieldLen || strings.ContainsAny(user, "\r\n") {
				writeErr(w, http.StatusBadRequest, "smtp_username must be a single line of at most "+strconv.Itoa(maxSMTPFieldLen)+" characters")
				return
			}

			writes[key] = user
		case mail.FromKey:
			var from string
			if err := json.Unmarshal(raw, &from); err != nil {
				writeErr(w, http.StatusBadRequest, "smtp_from must be a string")
				return
			}

			from = strings.TrimSpace(from)
			if from != "" {
				if _, err := netmail.ParseAddress(from); err != nil || strings.ContainsAny(from, "\r\n") {
					writeErr(w, http.StatusBadRequest, "smtp_from must be an email address")
					return
				}
			}

			writes[key] = from
		case mail.TLSKey:
			var on bool
			if err := json.Unmarshal(raw, &on); err != nil {
				writeErr(w, http.StatusBadRequest, "smtp_tls must be true or false")
				return
			}

			writes[key] = strconv.FormatBool(on)
		case smtpPasswordField:
			// null clears the password; "" means "leave what is stored
			// alone", so a UI that round-trips the form does not have to
			// re-send a secret it was never given.
			if string(raw) != "null" {
				var pw string
				if err := json.Unmarshal(raw, &pw); err != nil {
					writeErr(w, http.StatusBadRequest, "smtp_password must be a string or null")
					return
				}

				if pw == "" {
					continue
				}

				sealed, err := mail.SealPassword(s.sealKey(), pw)
				if err != nil {
					adminFailed(w, "seal smtp password", err)
					return
				}

				writes[mail.PasswordKey] = sealed

				continue
			}

			writes[mail.PasswordKey] = ""
		default:
			if iv, isInterval := jobs.IntervalSettings()[key]; isInterval {
				var secs int
				if err := json.Unmarshal(raw, &secs); err != nil {
					writeErr(w, http.StatusBadRequest, key+" must be a whole number of seconds")
					return
				}

				// The scheduler's own floor, not a second opinion: a value it
				// would silently clamp is rejected here instead, so the number
				// the settings screen shows back is the number that runs.
				if secs < iv.MinSeconds || secs > iv.MaxSeconds {
					writeErr(w, http.StatusBadRequest,
						key+" must be between "+strconv.Itoa(iv.MinSeconds)+" and "+strconv.Itoa(iv.MaxSeconds)+" seconds")

					return
				}

				writes[key] = strconv.Itoa(secs)

				continue
			}

			spec, isRate := rateSettings[key]
			if !isRate {
				writeErr(w, http.StatusBadRequest, "unknown setting: "+key)
				return
			}

			var n int
			if err := json.Unmarshal(raw, &n); err != nil {
				writeErr(w, http.StatusBadRequest, key+" must be a whole number")
				return
			}

			if n < spec.min || n > spec.max {
				writeErr(w, http.StatusBadRequest, key+" must be between "+strconv.Itoa(spec.min)+" and "+strconv.Itoa(spec.max))
				return
			}

			writes[key] = strconv.Itoa(n)
		}
	}

	if err := s.store().SetSettings(r.Context(), writes); err != nil {
		adminFailed(w, "write settings", err)
		return
	}

	// The parsed trusted-proxy list is memoized; a write must take effect on
	// the next request rather than at restart.
	if _, ok := writes[trustedProxiesSetting]; ok {
		s.invalidateTrustedProxies()
	}

	out, err := s.currentSettings(r.Context())
	if err != nil {
		adminFailed(w, "read settings", err)
		return
	}

	writeJSON(w, http.StatusOK, out)
}
