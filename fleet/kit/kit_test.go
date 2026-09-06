package kit_test

import (
	"bytes"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/kit"
)

func hostedData() kit.Data {
	return kit.Data{
		DeviceName: "fw16", DeviceID: "ag_abc123", TargetKind: "hosted",
		Endpoint: "https://fleet.example.com", Bucket: "warphold", Prefix: "ag_abc123/", Region: "warphold",
		RepoPassword: "pw-xUZ7_password", ReadKeyID: "WHREADONLYKEYID12345", ReadKey: "ro-secret-value",
		Generated: time.Date(2026, 9, 2, 10, 30, 0, 0, time.UTC),
	}
}

func render(t *testing.T, d kit.Data) string {
	t.Helper()

	var b bytes.Buffer
	require.NoError(t, kit.Render(&b, d))

	return b.String()
}

// externalAsset matches a reference that would make the page fetch something
// over the network. The kit has to print from a machine with no network, so
// there must be none -- not a font, not a stylesheet, not an image.
var externalAsset = regexp.MustCompile(`(?i)(?:src|href|action)\s*=\s*["']?\s*(?:https?:)?//|url\(\s*["']?\s*(?:https?:)?//|@import`)

func TestRenderIsSelfContainedAndPrintsEverything(t *testing.T) {
	d := hostedData()
	out := render(t, d)

	for _, want := range []string{
		d.DeviceName, d.DeviceID, d.TargetKind, d.Prefix, d.Bucket, d.Region,
		d.RepoPassword, d.ReadKeyID, d.ReadKey,
		"2026-09-02 10:30 UTC",
		"only copy of these secrets", // the sealing note
		"What to do first",           // the one-line runbook
	} {
		require.Contains(t, out, want)
	}

	// The commands are printed verbatim; the page is HTML, so the placeholder's
	// angle brackets appear escaped in the source and unescaped on paper.
	require.Len(t, kit.Commands(d), 3)

	for _, c := range kit.Commands(d) {
		require.Contains(t, out, html.EscapeString(c))
	}

	require.NotRegexp(t, externalAsset, out, "the page must fetch nothing")
	require.NotContains(t, strings.ToLower(out), "<script")
	// The only scheme on the page is the endpoint fact; the connect command
	// prints the bare host, which is what minio-go accepts.
	require.Equal(t, 1, strings.Count(out, "https://"), "only the endpoint carries a scheme")
	require.NotContains(t, out, "--endpoint https://")
}

// Every flag is upstream's, checked against cli/storage_*.go (spec 14.4).
func TestCommandsUseVerifiedUpstreamFlags(t *testing.T) {
	t.Run("hosted", func(t *testing.T) {
		c := kit.Commands(hostedData())
		require.Equal(t,
			"kopia repository connect s3 --bucket warphold --prefix ag_abc123/ --endpoint fleet.example.com"+
				" --access-key WHREADONLYKEYID12345 --secret-access-key ro-secret-value --region warphold",
			c[0])
		require.Equal(t, "kopia snapshot list", c[1])
		require.True(t, strings.HasPrefix(c[2], "kopia restore <snapshot-id> "))
	})

	t.Run("hosted over plain http gets no --disable-tls: it can never reach the gateway", func(t *testing.T) {
		d := hostedData()
		d.Endpoint = "http://192.0.2.5:8080"
		c := kit.Commands(d)
		require.Contains(t, c[0], "--endpoint 192.0.2.5:8080")
		require.NotContains(t, c[0], "--disable-tls")
		require.True(t, strings.HasSuffix(c[0], "--region warphold"))

		out := render(t, d)
		require.Contains(t, out, "cannot talk to the WarpHold gateway without TLS")
	})

	t.Run("b2", func(t *testing.T) {
		c := kit.Commands(kit.Data{
			TargetKind: "b2", Bucket: "hody-backups", Prefix: "agents/ag_abc123/",
			ReadKeyID: "b2kid", ReadKey: "b2secret",
		})
		require.Equal(t,
			"kopia repository connect b2 --bucket hody-backups --prefix agents/ag_abc123/ --key-id b2kid --key b2secret",
			c[0])
	})

	t.Run("filesystem", func(t *testing.T) {
		c := kit.Commands(kit.Data{TargetKind: "filesystem", Path: "/srv/warphold/agents/ag_abc123"})
		require.Equal(t, "kopia repository connect filesystem --path /srv/warphold/agents/ag_abc123", c[0])
	})

	require.Nil(t, kit.Commands(kit.Data{TargetKind: "martian"}))
}

// A device name is admin-supplied text; it must not be able to inject markup.
func TestRenderEscapesDeviceName(t *testing.T) {
	d := hostedData()
	d.DeviceName = `<script>alert(1)</script>`
	out := render(t, d)
	require.NotContains(t, strings.ToLower(out), "<script")
	require.Contains(t, out, "&lt;script&gt;")
}

// The kit is meant to be pasted into a shell, and a filesystem target's path,
// a bucket name and a region are all operator-typed. An unquoted space
// silently connects somewhere else; an unquoted metacharacter runs something
// else entirely.
func TestCommandsQuoteValuesForTheShell(t *testing.T) {
	c := kit.Commands(kit.Data{
		TargetKind: "filesystem",
		Path:       "/srv/backups/Hody's Laptop; rm -rf /",
	})
	require.Equal(t,
		`kopia repository connect filesystem --path '/srv/backups/Hody'\''s Laptop; rm -rf /'`,
		c[0])

	// A value with nothing special in it stays bare, so the common kit is still
	// the plain command an operator can read at a glance.
	c = kit.Commands(kit.Data{
		TargetKind: "b2", Bucket: "warphold-offsite", Prefix: "agents/ag_1/",
		ReadKeyID: "b2kid", ReadKey: "b2secret",
	})
	require.Equal(t,
		"kopia repository connect b2 --bucket warphold-offsite --prefix agents/ag_1/ --key-id b2kid --key b2secret",
		c[0])

	// An empty value prints no flag at all: a bare "--region" would swallow
	// whatever flag came next as its argument.
	c = kit.Commands(kit.Data{
		TargetKind: "hosted", Bucket: "warphold", Prefix: "ag_1/",
		Endpoint: "https://fleet.example.com", ReadKeyID: "k", ReadKey: "s",
	})
	require.NotContains(t, c[0], "--region")
	require.Contains(t, c[0], "--endpoint fleet.example.com")
}
