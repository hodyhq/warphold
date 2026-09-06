package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// selfSigned returns a certificate for 127.0.0.1 and a pool trusting it, so
// the tests exercise the real verifying TLS path rather than skipping it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "smtp stub"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	require.NoError(t, err)

	pool := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool.AddCert(parsed)

	return cert, pool
}

// stub is an in-process SMTP server: plain on connect, STARTTLS on request,
// AUTH PLAIN accepted or rejected as the test asks.
type stub struct {
	port    int
	authOK  bool
	tlsConf *tls.Config

	mu         sync.Mutex
	noSTARTTLS bool
	auth       string
	from       string
	rcpt       []string
	data       string
}

func startStub(t *testing.T, authOK bool) *stub {
	t.Helper()
	cert, pool := selfSigned(t)
	rootCAs = pool
	t.Cleanup(func() { rootCAs = nil })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	s := &stub{authOK: authOK, tlsConf: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}

	s.port = ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go s.handle(c)
		}
	}()

	return s
}

func (s *stub) config() Config {
	return Config{Host: "127.0.0.1", Port: s.port, Username: "user", Password: "pw", From: "fleet@example.com", TLS: true}
}

func (s *stub) handle(c net.Conn) {
	defer c.Close()

	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	r, w := bufio.NewReader(c), bufio.NewWriter(c)
	say := func(lines ...string) {
		for _, l := range lines {
			w.WriteString(l + "\r\n") //nolint:errcheck
		}

		w.Flush() //nolint:errcheck
	}
	say("220 stub ESMTP")

	secure := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimRight(line, "\r\n")

		cmd, rest, _ := strings.Cut(line, " ")
		switch strings.ToUpper(cmd) {
		case "EHLO", "HELO":
			s.mu.Lock()
			plainStub := s.noSTARTTLS
			s.mu.Unlock()

			if secure || plainStub {
				say("250-stub", "250 AUTH PLAIN")
			} else {
				say("250-stub", "250-STARTTLS", "250 AUTH PLAIN")
			}
		case "STARTTLS":
			say("220 Ready to start TLS")

			tc := tls.Server(c, s.tlsConf)
			if err := tc.Handshake(); err != nil {
				return
			}

			c, secure = tc, true
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			r, w = bufio.NewReader(c), bufio.NewWriter(c)
		case "AUTH":
			_, b64, _ := strings.Cut(rest, " ")
			raw, _ := base64.StdEncoding.DecodeString(b64)

			s.mu.Lock()
			s.auth = string(raw)
			s.mu.Unlock()

			if s.authOK {
				say("235 2.7.0 Authentication successful")
			} else {
				say("535 5.7.8 Username and Password not accepted")
			}
		case "MAIL":
			s.mu.Lock()
			s.from = addrOf(rest)
			s.mu.Unlock()
			say("250 2.1.0 Ok")
		case "RCPT":
			s.mu.Lock()
			s.rcpt = append(s.rcpt, addrOf(rest))
			s.mu.Unlock()
			say("250 2.1.5 Ok")
		case "DATA":
			say("354 End data with <CR><LF>.<CR><LF>")

			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}

				if l == ".\r\n" {
					break
				}

				b.WriteString(l)
			}

			s.mu.Lock()
			s.data = b.String()
			s.mu.Unlock()
			say("250 2.0.0 Ok: queued")
		case "QUIT":
			say("221 2.0.0 Bye")
			return
		default:
			say("502 5.5.2 Unrecognized command")
		}
	}
}

// addrOf pulls the address out of "FROM:<a@b>" / "TO:<a@b>".
func addrOf(s string) string {
	if i := strings.Index(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j > 0 {
			return s[i+1 : i+j]
		}
	}

	return s
}

func (s *stub) snapshot() (auth, from string, rcpt []string, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.auth, s.from, append([]string(nil), s.rcpt...), s.data
}

func TestSendDeliversEnvelopeAuthAndBothParts(t *testing.T) {
	s := startStub(t, true)
	c := s.config()
	c.MessageIDHost = "fleet.example.com"
	require.NoError(t, Send(context.Background(), c, []string{"ops@example.com", "Second <two@example.com>"}, "WarpHold test", "plain body", "<p>html body</p>"))

	auth, from, rcpt, data := s.snapshot()
	require.Equal(t, "\x00user\x00pw", auth, "AUTH PLAIN over TLS")
	require.Equal(t, "fleet@example.com", from)
	require.Equal(t, []string{"ops@example.com", "two@example.com"}, rcpt)

	require.Contains(t, data, "From: fleet@example.com")
	require.Contains(t, data, `To: ops@example.com, "Second" <two@example.com>`)
	require.Contains(t, data, "Subject: WarpHold test")
	require.Contains(t, data, "@fleet.example.com>", "Message-ID carries the public host")
	require.Contains(t, data, "Date: ")
	require.Contains(t, data, "MIME-Version: 1.0")
	require.Contains(t, data, "Content-Type: multipart/alternative")
	require.Contains(t, data, `Content-Type: text/plain; charset="utf-8"`)
	require.Contains(t, data, `Content-Type: text/html; charset="utf-8"`)
	require.Contains(t, data, "plain body")
	require.Contains(t, data, "<p>html body</p>")
}

// The SMTPS path never negotiates STARTTLS: the stub is wrapped in TLS from
// the first byte and advertises no such extension.
func TestSendOverImplicitTLS(t *testing.T) {
	cert, pool := selfSigned(t)
	rootCAs = pool
	t.Cleanup(func() { rootCAs = nil })

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	s := &stub{authOK: true}

	s.port = ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Already encrypted: handle() only ever sees the post-TLS half.
			go func() { s.mu.Lock(); s.noSTARTTLS = true; s.mu.Unlock(); s.handle(c) }()
		}
	}()

	implicitTLSPort = s.port
	t.Cleanup(func() { implicitTLSPort = 465 })

	require.NoError(t, Send(context.Background(), s.config(), []string{"ops@example.com"}, "hi", "t", "<p>h</p>"))
	auth, from, _, data := s.snapshot()
	require.Equal(t, "\x00user\x00pw", auth)
	require.Equal(t, "fleet@example.com", from)
	require.Contains(t, data, "Subject: hi")
}

func TestImplicitTLSPortRefusesTLSOff(t *testing.T) {
	c := Config{Host: "127.0.0.1", Port: 465, From: "fleet@example.com"}
	require.ErrorContains(t, Send(context.Background(), c, []string{"ops@example.com"}, "hi", "t", "<p>h</p>"), "implicit TLS")
}

func TestSendSurfacesTheServersRawError(t *testing.T) {
	s := startStub(t, false)
	err := Send(context.Background(), s.config(), []string{"ops@example.com"}, "hi", "t", "<p>h</p>")
	require.Error(t, err)
	require.Contains(t, err.Error(), "535")
	require.Contains(t, err.Error(), "Username and Password not accepted")
}

func TestSendRejectsHeaderInjection(t *testing.T) {
	s := startStub(t, true)

	base := s.config()
	for name, mut := range map[string]func(*Config, *string, *[]string){
		"subject": func(_ *Config, subj *string, _ *[]string) { *subj = "hi\r\nBcc: evil@example.com" },
		"from":    func(c *Config, _ *string, _ *[]string) { c.From = "fleet@example.com\r\nBcc: evil@example.com" },
		"to":      func(_ *Config, _ *string, to *[]string) { *to = []string{"ops@example.com\nBcc: evil@example.com"} },
	} {
		t.Run(name, func(t *testing.T) {
			c, subj, to := base, "hi", []string{"ops@example.com"}
			mut(&c, &subj, &to)
			require.Error(t, Send(context.Background(), c, to, subj, "t", "<p>h</p>"))
		})
	}

	_, from, _, data := s.snapshot()
	require.Empty(t, from, "nothing reached the server")
	require.Empty(t, data)
}

// A password must never be handed to a server over a plaintext connection,
// whatever the settings say.
func TestSendRefusesToAuthenticateWithoutTLS(t *testing.T) {
	s := startStub(t, true)
	c := s.config()
	c.TLS = false
	err := Send(context.Background(), c, []string{"ops@example.com"}, "hi", "t", "<p>h</p>")
	require.ErrorContains(t, err, "unencrypted")

	auth, _, _, _ := s.snapshot()
	require.Empty(t, auth)
}

func TestSendRequiresSTARTTLSWhenTLSIsOn(t *testing.T) {
	// A server that never advertises STARTTLS: net.Listen with a stub that
	// only greets and answers EHLO without the extension.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()

		r, w := bufio.NewReader(c), bufio.NewWriter(c)
		w.WriteString("220 stub ESMTP\r\n") //nolint:errcheck
		w.Flush()                           //nolint:errcheck

		for {
			if _, err := r.ReadString('\n'); err != nil {
				return
			}

			w.WriteString("250 stub\r\n") //nolint:errcheck
			w.Flush()                     //nolint:errcheck
		}
	}()

	c := Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, From: "fleet@example.com", TLS: true}
	require.ErrorContains(t, Send(context.Background(), c, []string{"ops@example.com"}, "hi", "t", "<p>h</p>"), "STARTTLS")
}

func TestSubjectIsRFC2047EncodedOnlyWhenItNeedsIt(t *testing.T) {
	ascii, from, rcpt, err := buildMessage(Config{From: "fleet@example.com"}, []string{"ops@example.com"}, "Weekly digest", "t", "<p>h</p>")
	require.NoError(t, err)
	require.Equal(t, "fleet@example.com", from, "the envelope is parsed once, here")
	require.Equal(t, []string{"ops@example.com"}, rcpt)
	require.Contains(t, string(ascii), "Subject: Weekly digest")
	require.Contains(t, string(ascii), "@localhost>", "no public host configured")

	utf8, _, _, err := buildMessage(Config{From: "fleet@example.com"}, []string{"ops@example.com"}, "Wöchentlich", "t", "<p>h</p>")
	require.NoError(t, err)
	require.Contains(t, string(utf8), "Subject: =?utf-8?")
	require.NotContains(t, string(utf8), "Wöchentlich")
}

func TestDefaultsAreSMTP2GOShaped(t *testing.T) {
	d := Defaults()
	require.Equal(t, "mail.smtp2go.com", d.Host)
	require.Equal(t, 2525, d.Port)
	require.True(t, d.TLS)
	require.Empty(t, d.Password)
	require.Equal(t, []int{2525, 587, 465}, AllowedPorts)
}
