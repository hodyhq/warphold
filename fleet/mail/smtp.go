// Package mail sends Fleet's outbound email (the test message and the weekly
// digest) over SMTP, with defaults shaped for SMTP2GO.
package mail

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Config is one SMTP account. It is what the settings screen edits and what
// Send dials.
type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	TLS      bool
	// MessageIDHost is the right-hand side of the Message-ID: the Fleet's
	// public host when one is configured, "localhost" otherwise. It is not an
	// SMTP setting of its own - Load fills it from public_url.
	MessageIDHost string
}

// implicitTLSPort speaks TLS from the first byte (SMTPS); every other port
// starts in the clear and upgrades with STARTTLS. It is a var so a test can
// point the SMTPS path at a stub on an unprivileged port.
var implicitTLSPort = 465

const (
	// timeout bounds the dial and every command: a wedged SMTP server must
	// not hold a job (or an admin's test-send request) open indefinitely.
	timeout = 10 * time.Second
)

// AllowedPorts are the ports the settings screen offers, SMTP2GO's default first.
var AllowedPorts = []int{2525, 587, 465}

// rootCAs overrides the system trust store. It is nil outside tests; the
// verifying path is the only one this package ever takes in production.
var rootCAs *x509.CertPool

// Defaults are SMTP2GO-shaped: its relay host, its firewall-friendly port, TLS on.
func Defaults() Config {
	return Config{Host: "mail.smtp2go.com", Port: 2525, TLS: true}
}

// Send delivers one multipart/alternative message. It verifies the server
// certificate, and it never hands the password to a server it has not
// negotiated TLS with, whatever the settings say.
func Send(ctx context.Context, c Config, to []string, subject, textBody, htmlBody string) error {
	// The message is built (and so validated) before anything is dialled, so
	// a rejected header never reaches a server.
	msg, from, rcpt, err := buildMessage(c, to, subject, textBody, htmlBody)
	if err != nil {
		return err
	}
	if c.Host == "" {
		return errors.New("smtp_host is not set")
	}
	if c.Port == implicitTLSPort && !c.TLS {
		return errors.New("port 465 is implicit TLS and cannot be used with TLS off")
	}

	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port)))
	if err != nil {
		return err
	}
	// A cancelled context has to reach a blocked read, which only closing the
	// connection can do; net/smtp has no context-aware API.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close() //nolint:errcheck
		case <-done:
		}
	}()

	var netConn net.Conn = &deadlineConn{Conn: conn}
	secure := false
	if c.Port == implicitTLSPort {
		tc := tls.Client(netConn, tlsConfig(c.Host))
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close() //nolint:errcheck
			return err
		}
		netConn, secure = tc, true
	}

	cl, err := smtp.NewClient(netConn, c.Host)
	if err != nil {
		conn.Close() //nolint:errcheck
		return err
	}
	defer cl.Close() //nolint:errcheck

	if !secure && c.TLS {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errors.New("the server does not offer STARTTLS; turn smtp_tls off only if you trust the network")
		}
		if err := cl.StartTLS(tlsConfig(c.Host)); err != nil {
			return err
		}
		secure = true
	}
	if c.Username != "" {
		if !secure {
			return errors.New("refusing to send the SMTP password over an unencrypted connection")
		}
		if err := cl.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return err
		}
	}
	if err := cl.Mail(from); err != nil {
		return err
	}
	for _, a := range rcpt {
		if err := cl.Rcpt(a); err != nil {
			return err
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}

func tlsConfig(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: rootCAs}
}

// deadlineConn gives every read and write its own timeout, so a server that
// accepts the connection and then stalls mid-conversation still fails fast.
type deadlineConn struct{ net.Conn }

func (c *deadlineConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(timeout))
	return c.Conn.Read(b)
}

func (c *deadlineConn) Write(b []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(timeout))
	return c.Conn.Write(b)
}

// parseAddress validates one address and rejects the CR/LF that would let a
// stored setting inject extra headers.
func parseAddress(field, v string) (*netmail.Address, error) {
	if strings.ContainsAny(v, "\r\n") {
		return nil, fmt.Errorf("%s must not contain a line break", field)
	}
	a, err := netmail.ParseAddress(strings.TrimSpace(v))
	if err != nil {
		return nil, fmt.Errorf("%s is not a valid email address", field)
	}
	return a, nil
}

func parseAddresses(to []string) ([]*netmail.Address, error) {
	if len(to) == 0 {
		return nil, errors.New("no recipients")
	}
	out := make([]*netmail.Address, 0, len(to))
	for _, v := range to {
		a, err := parseAddress("recipient", v)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// buildMessage renders the RFC 5322 message: headers, then a
// multipart/alternative body with the text part first, as the standard wants.
// It also returns the envelope Send needs - the bare From and recipient
// addresses - so the addresses are parsed and validated exactly once.
func buildMessage(c Config, to []string, subject, textBody, htmlBody string) (msg []byte, envFrom string, envRcpt []string, err error) {
	if strings.ContainsAny(subject, "\r\n") {
		return nil, "", nil, errors.New("subject must not contain a line break")
	}
	if textBody == "" && htmlBody == "" {
		return nil, "", nil, errors.New("message has no body")
	}
	from, err := parseAddress("smtp_from", c.From)
	if err != nil {
		return nil, "", nil, err
	}
	rcpt, err := parseAddresses(to)
	if err != nil {
		return nil, "", nil, err
	}
	list := make([]string, 0, len(rcpt))
	envRcpt = make([]string, 0, len(rcpt))
	for _, a := range rcpt {
		list = append(list, formatAddress(a))
		envRcpt = append(envRcpt, a.Address)
	}

	var body strings.Builder
	mp := multipart.NewWriter(&body)
	add := func(contentType, content string) error {
		if content == "" {
			return nil
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", contentType+`; charset="utf-8"`)
		h.Set("Content-Transfer-Encoding", "quoted-printable")
		p, err := mp.CreatePart(h)
		if err != nil {
			return err
		}
		qp := quotedprintable.NewWriter(p)
		if _, err := io.WriteString(qp, content); err != nil {
			return err
		}
		return qp.Close()
	}
	if err := add("text/plain", textBody); err != nil {
		return nil, "", nil, err
	}
	if err := add("text/html", htmlBody); err != nil {
		return nil, "", nil, err
	}
	if err := mp.Close(); err != nil {
		return nil, "", nil, err
	}

	host := c.MessageIDHost
	if host == "" {
		host = "localhost"
	}
	id, err := messageID(host)
	if err != nil {
		return nil, "", nil, err
	}
	var out strings.Builder
	for _, h := range [][2]string{
		{"Date", time.Now().Format(time.RFC1123Z)},
		{"Message-ID", id},
		{"From", formatAddress(from)},
		{"To", strings.Join(list, ", ")},
		{"Subject", mime.QEncoding.Encode("utf-8", subject)},
		{"MIME-Version", "1.0"},
		{"Content-Type", "multipart/alternative; boundary=" + mp.Boundary()},
	} {
		out.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	out.WriteString("\r\n")
	out.WriteString(body.String())
	return []byte(out.String()), from.Address, envRcpt, nil
}

// formatAddress renders a bare address as itself; net/mail's String would
// wrap it in angle brackets, which is legal but reads like a bug in a header.
func formatAddress(a *netmail.Address) string {
	if a.Name == "" {
		return a.Address
	}
	return a.String()
}

func messageID(host string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	if strings.ContainsAny(host, "\r\n <>@") {
		host = "localhost"
	}
	return "<" + hex.EncodeToString(b) + "@" + host + ">", nil
}
