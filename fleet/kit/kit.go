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
	Commands  []string
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
func Commands(d Data) []string {
	var connect string

	switch d.TargetKind {
	case "hosted":
		connect = "kopia repository connect s3" +
			" --bucket " + d.Bucket +
			" --prefix " + d.Prefix +
			" --endpoint " + hostOf(d.Endpoint) +
			" --access-key " + d.ReadKeyID +
			" --secret-access-key " + d.ReadKey +
			" --region " + d.Region
		// minio-go talks TLS unless told otherwise, so the flag appears only
		// for a plain-http endpoint -- and then it is required.
		if strings.HasPrefix(d.Endpoint, "http://") {
			connect += " --disable-tls"
		}

	case "b2":
		connect = "kopia repository connect b2" +
			" --bucket " + d.Bucket +
			" --prefix " + d.Prefix +
			" --key-id " + d.ReadKeyID +
			" --key " + d.ReadKey

	case "filesystem":
		connect = "kopia repository connect filesystem --path " + d.Path

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

	return page.Execute(w, d)
}
