// Package kit renders the printable recovery kit: one self-contained HTML page
// that lets someone restore a device's backups with a stock upstream Kopia
// binary and nothing from WarpHold (spec 9).
//
// The page has no external resource of any kind -- no font, no image, no
// script, no stylesheet link -- because it has to print correctly from a
// machine with no network, years from now.
package kit

import (
	_ "embed"
	"html/template"
	"io"
	"strings"
	"time"
)

// Data is everything the page prints. The secrets in it are never logged and
// the rendered page is never written to disk by the server.
type Data struct {
	DeviceName, DeviceID, TargetKind, Endpoint, Bucket, Prefix, Path string

	// Region is the S3 region a hosted target's gateway advertises; it is what
	// --region must name for the signature to verify.
	Region string

	RepoPassword, ReadKeyID, ReadKey string

	// Commands are the literal, copy-pasteable connect/list/restore lines,
	// normally filled by Commands.
	Commands []string

	// Note is an extra warning printed above Commands, normally filled by
	// Render. It is non-empty only for a hosted target behind a plain-http
	// Fleet URL, where no connect command can actually work.
	Note      string
	Generated time.Time
}

// Commands returns the literal upstream-Kopia lines for d's target kind.
//
// Every flag here is verified against the upstream flag definitions rather
// than remembered (spec 14.4): cli/storage_s3.go declares --bucket, --endpoint,
// --region, --access-key, --secret-access-key, --prefix and --disable-tls;
// cli/storage_b2.go declares --bucket, --key-id, --key and --prefix;
// cli/storage_filesystem.go declares --path. This is the one artefact that has
// to still work with an unfamiliar binary, so it prints nothing it invented.
//
// --disable-tls is a verified flag but is never emitted for a hosted target:
// see the comment in the "hosted" case below.
// flag renders " --name value", or nothing at all when value is empty: an
// empty value would print a bare flag and let it swallow the NEXT flag as its
// argument.
func flag(name, value string) string {
	if value == "" {
		return ""
	}

	return " --" + name + " " + shellQuote(value)
}

// shellQuote makes a value safe to paste into a POSIX shell. The kit exists to
// be pasted, and several of these values are operator-typed rather than
// server-minted -- a filesystem target's path, a bucket name, a region -- so a
// space silently connects somewhere else and a metacharacter runs something
// else entirely.
//
// The test is an allowlist, not a list of dangerous characters: a denylist that
// forgets one prints a command that does something other than what the kit
// says.
func shellQuote(v string) string {
	safe := func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return true
		default:
			return strings.ContainsRune("._:/@=+,-", r)
		}
	}

	if v != "" && strings.IndexFunc(v, func(r rune) bool { return !safe(r) }) < 0 {
		return v
	}

	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func Commands(d Data) []string {
	var connect string

	switch d.TargetKind {
	case "hosted":
		connect = "kopia repository connect s3" +
			flag("bucket", d.Bucket) +
			flag("prefix", d.Prefix) +
			flag("endpoint", hostOf(d.Endpoint)) +
			flag("access-key", d.ReadKeyID) +
			flag("secret-access-key", d.ReadKey) +
			flag("region", d.Region)
		// --disable-tls is never printed here: the gateway's sigv4 verifier
		// refuses aws-chunked streaming signatures by design
		// (fleet/gateway/sigv4.go, ErrStreamingUnsupported), and minio-go's S3
		// client -- what this connect command drives -- only sends
		// aws-chunked over plain HTTP. So a plain-http connect command can
		// never actually reach the gateway; hostedHTTPNote flags the real fix
		// instead of printing a dead command.

	case "b2":
		connect = "kopia repository connect b2" +
			flag("bucket", d.Bucket) +
			flag("prefix", d.Prefix) +
			flag("key-id", d.ReadKeyID) +
			flag("key", d.ReadKey)

	case "filesystem":
		connect = "kopia repository connect filesystem" + flag("path", d.Path)

	default:
		return nil
	}

	return []string{
		connect,
		"kopia snapshot list",
		"kopia restore <snapshot-id> /path/to/restore/into",
	}
}

// hostOf strips the scheme from an endpoint: minio-go, which is what Kopia's
// s3 provider uses, rejects an --endpoint that carries one.
func hostOf(endpoint string) string {
	if _, host, ok := strings.Cut(endpoint, "://"); ok {
		return host
	}

	return endpoint
}

// hostedHTTPNote returns the warning printed in place of a connect command
// for a hosted target whose Fleet is reachable only over plain HTTP: stock
// Kopia's S3 client can never write to or read from such a gateway (see the
// comment in Commands), so the kit tells the reader how to fix the Fleet
// instead of handing them a command that will fail.
func hostedHTTPNote(d Data) string {
	if d.TargetKind != "hosted" || !strings.HasPrefix(d.Endpoint, "http://") {
		return ""
	}

	return "This Fleet is served over plain HTTP; stock Kopia's S3 client cannot talk to the WarpHold gateway without TLS. " +
		"Put the Fleet behind HTTPS (or use --root-ca-pem-path with a self-signed cert) before restoring."
}

//go:embed kit.html.tmpl
var pageTmpl string

var page = template.Must(template.New("kit").Funcs(template.FuncMap{
	"stamp": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
}).Parse(pageTmpl))

// Render writes a single self-contained print-ready HTML page: inline CSS, no
// external font, image or script, so it prints from a machine with no network.
func Render(w io.Writer, d Data) error {
	if d.Commands == nil {
		d.Commands = Commands(d)
	}

	if d.Note == "" {
		d.Note = hostedHTTPNote(d)
	}

	return page.Execute(w, d)
}
