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

// Settings reads the non-secret SMTP settings over Defaults. It takes no
// sealing key on purpose: the settings screen must never unseal the password,
// so a wrong, rotated or corrupt sealed value cannot take the whole page down
// with it - PasswordSet reports presence, and only the sending paths open it.
func Settings(ctx context.Context, st *store.Store) (Config, error) {
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
	tlsOn, pub := get(TLSKey), get(publicURLKey)
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
	if pub != "" {
		if u, err := url.Parse(pub); err == nil {
			c.MessageIDHost = u.Hostname()
		}
	}
	return c, nil
}

// Load is Settings plus the unsealed password: the sending paths only. An
// unopenable password is an error rather than an empty one, because sending
// with a silently blank password fails at the server with a misleading message.
func Load(ctx context.Context, st *store.Store, k seal.Key) (Config, error) {
	c, err := Settings(ctx, st)
	if err != nil {
		return Config{}, err
	}
	sealed, err := st.Setting(ctx, PasswordKey)
	if err != nil {
		return Config{}, err
	}
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
	return c, nil
}

// PasswordSet reports whether a sealed password is stored, without unsealing
// it: the settings GET says "set" or "not set" and never the value.
func PasswordSet(ctx context.Context, st *store.Store) (bool, error) {
	v, err := st.Setting(ctx, PasswordKey)
	return v != "", err
}
