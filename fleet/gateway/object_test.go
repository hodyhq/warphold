package gateway_test

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fleet/gateway"
)

func TestNormalizeKeyConfinesToPrefix(t *testing.T) {
	const p = "abc123/"

	for _, bad := range []string{
		"", "/abc123/x", "abc123/../../etc/passwd", "abc123//x", "other/x",
		"abc123/" + strings.Repeat("a", 1100), "abc123/x\x00y", "abc123/.",
	} {
		if _, err := gateway.NormalizeKey(bad, p); err == nil {
			t.Errorf("NormalizeKey(%q) = nil error, want error", bad)
		}
	}

	got, err := gateway.NormalizeKey("abc123/p1234abcd", p)
	require.NoError(t, err)
	require.Equal(t, "abc123/p1234abcd", got)
}

// malformedKeys are structurally invalid whatever the prefix: traversal,
// control bytes, separators a filesystem would reinterpret, or oversized. Both
// NormalizeKey and the backends must reject every one of them.
var malformedKeys = []string{
	"", ".", "..", "/", "//", "/abc123/x",
	"abc123/..", "abc123/../x", "abc123/../../etc/passwd",
	"../abc123/x",
	"abc123/..\\..\\windows\\system32", `abc123\..\x`, `abc123/x\y`,
	"abc123//x", "abc123/x//y", "abc123/x/", "abc123/./x", "abc123/x/./y",
	"abc123/x\x00y", "abc123/\x00", "abc123/x\ny", "abc123/x\ry", "abc123/x\x7fy",
	"abc123/x\x01y", "abc123/\t", "abc123/" + strings.Repeat("a", 1100),
	strings.Repeat("../", 40) + "etc/passwd",
	`\\server\share\x`, "abc123/x/../../../y",
	// the key space is flat: <device-id>/<blob-name> and nothing deeper
	"abc123", "abc123/a/b", "abc123/sub/x/y",
	// .tmp is where partial writes are staged, so no device may own it
	".tmp/x", ".tmp/",
	// invalid UTF-8, an overlong encoding of '/', and C1 controls
	"abc123/\xff", "abc123/x\xc0\xafy", "abc123/x\u0085y", "abc123/x\u009fy",
}

// hostileKeys adds the keys that are well formed but belong to somebody else,
// which only NormalizeKey (which knows the prefix) can reject.
var hostileKeys = append(append([]string{}, malformedKeys...),
	"other/x", "abc1234/x", "abc12/x", "ABC123/x", "c:/abc123/x",
	"..%2f..%2fetc/passwd")

func TestNormalizeKeyRejectsHostileKeys(t *testing.T) {
	const p = "abc123/"

	for _, bad := range hostileKeys {
		if _, err := gateway.NormalizeKey(bad, p); err == nil {
			t.Errorf("NormalizeKey(%q) = nil error, want error", bad)
		}
	}
}

// FuzzNormalizeKey asserts the property the whole storage boundary rests on:
// any key NormalizeKey accepts stays inside the prefix and, once joined to a
// root directory, stays inside that root.
func FuzzNormalizeKey(f *testing.F) {
	for _, k := range hostileKeys {
		f.Add(k)
	}

	// Percent-encoded traversal is decoded by the HTTP layer before it reaches
	// us; if it arrives still encoded it is an ordinary (ugly) name that cannot
	// escape, so it is a fuzz seed rather than a must-reject case.
	for _, k := range []string{
		"abc123/%2e%2e/%2e%2e/etc/passwd", "abc123/..a", "abc123/a..b", "abc123/...",
		"abc123/p1234abcd", "abc123/kopia.repository", "abc123/x/y/z",
		"abc123/", "abc123", "abc123/" + strings.Repeat("s", 300), "abc123/.tmp",
		"abc123/\xc0\xaf", "abc123/\x80", "abc123/\u0085", ".tmp/x", "abc123/a/b",
	} {
		f.Add(k)
	}

	const p = "abc123/"

	root := filepath.FromSlash("/srv/warphold/hosted")

	f.Fuzz(func(t *testing.T, key string) {
		got, err := gateway.NormalizeKey(key, p)
		if err != nil {
			return
		}

		require.Equal(t, key, got, "NormalizeKey rewrote the key")
		require.True(t, strings.HasPrefix(got, p), "accepted key outside the prefix")
		require.True(t, utf8.ValidString(got), "accepted key is not valid UTF-8")
		require.Equal(t, 1, strings.Count(got, "/"), "accepted key is not <device-id>/<blob-name>")
		require.NotContains(t, got, `\`)
		require.LessOrEqual(t, len(got), 1024)

		joined := filepath.Join(root, filepath.FromSlash(got))
		require.True(t, strings.HasPrefix(joined, root+string(filepath.Separator)),
			"accepted key escapes the root: %q -> %q", got, joined)
		require.Equal(t, joined, filepath.Clean(joined), "accepted key is not already clean")
	})
}

func TestNormalizeKeyRejectsUnterminatedPrefix(t *testing.T) {
	// "abc123" would otherwise admit "abc1234/x" - another device's keys.
	_, err := gateway.NormalizeKey("abc1234/x", "abc123")
	require.ErrorIs(t, err, gateway.ErrBadKey)

	_, err = gateway.NormalizeKey("abc123/x", "abc123")
	require.ErrorIs(t, err, gateway.ErrBadKey)

	got, err := gateway.NormalizeKey("abc123/x", "abc123/")
	require.NoError(t, err)
	require.Equal(t, "abc123/x", got)
}

func TestNormalizeKeyEmptyPrefixStillValidates(t *testing.T) {
	got, err := gateway.NormalizeKey("dev1/kopia.repository", "")
	require.NoError(t, err)
	require.Equal(t, "dev1/kopia.repository", got)

	for _, bad := range []string{"../x", "kopia.repository", "dev1/a/b", ".tmp/x"} {
		_, err := gateway.NormalizeKey(bad, "")
		require.ErrorIsf(t, err, gateway.ErrBadKey, "NormalizeKey(%q, \"\")", bad)
	}
}

func TestNormalizeKeyRejectsReservedPrefix(t *testing.T) {
	_, err := gateway.NormalizeKey(".tmp/x", ".tmp/")
	require.ErrorIs(t, err, gateway.ErrBadKey)
}
