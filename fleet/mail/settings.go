package mail

import (
	"context"
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"

	"github.com/kopia/kopia/fleet/seal"
	"github.com/kopia/kopia/fleet/store"
)

// The settings keys this package owns. The password is stored sealed, under a
// name of its own, so nothing can confuse the ciphertext for a plaintext
// setting: the API takes "smtp_password" and writes PasswordKey.
const (
	HostKey     = "smtp_host"
	PortKey     = "smtp_port"
	UsernameKey = "smtp_username"
	PasswordKey = "sealed_smtp_password"
	FromKey     = "smtp_from"
	TLSKey      = "smtp_tls"

	// publicURLKey is read, never written, here: it is what the Message-ID is
	// stamped with. It belongs to the api package (spec 6).
	publicURLKey = "public_url"
)

// Sender is Send bound to the stored settings, so the digest job can be
// tested without an SMTP server.
type Sender func(ctx context.Context, to []string, subject, textBody, htmlBody string) error

// SenderFor returns a Sender that reads the settings on each call, so a
// change in the UI takes effect on the next email without a restart.
func SenderFor(st *store.Store, k seal.Key) Sender {
	return func(ctx context.Context, to []string, subject, textBody, htmlBody string) error {
		c, err := Load(ctx, st, k)
		if err != nil {
			return err
		}
		return Send(ctx, c, to, subject, textBody, htmlBody)
	}
}

// SealPassword seals pw for storage under PasswordKey; "" clears it.
func SealPassword(k seal.Key, pw string) (string, error) {
	if pw == "" {
		return "", nil
	}
	b, err := k.Seal([]byte(pw))
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Load reads the stored SMTP settings over Defaults and unseals the password.
// An unopenable password is an error rather than an empty one: sending with a
// silently blank password would fail at the server with a misleading message.
func Load(ctx context.Context, st *store.Store, k seal.Key) (Config, error) {
	if st == nil {
		return Config{}, errors.New("fleet is not activated")
	}
	var firstErr error
	get := func(key string) string {
		v, err := st.Setting(ctx, key)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return v
	}
	host, port, user, from := get(HostKey), get(PortKey), get(UsernameKey), get(FromKey)
	tlsOn, sealed, pub := get(TLSKey), get(PasswordKey), get(publicURLKey)
	if firstErr != nil {
		return Config{}, firstErr
	}

	c := Defaults()
	if host != "" {
		c.Host = host
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil {
			return Config{}, errors.New("stored smtp_port is not a number")
		}
		c.Port = n
	}
	if tlsOn != "" {
		c.TLS = tlsOn == "true"
	}
	c.Username, c.From = user, from
	if sealed != "" {
		raw, err := hex.DecodeString(sealed)
		if err != nil {
			return Config{}, errors.New("stored SMTP password is malformed")
		}
		plain, err := k.Open(raw)
		if err != nil {
			return Config{}, err
		}
		c.Password = string(plain)
	}
	if pub != "" {
		if u, err := url.Parse(pub); err == nil {
			c.MessageIDHost = u.Hostname()
		}
	}
	return c, nil
}

// PasswordSet reports whether a sealed password is stored, without unsealing
// it: the settings GET says "set" or "not set" and never the value.
func PasswordSet(ctx context.Context, st *store.Store) (bool, error) {
	v, err := st.Setting(ctx, PasswordKey)
	return v != "", err
}
