package gateway

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAllowDeleteMatchesTheSpike pins the delete allowlist to the blob names
// docs/RECONCILE-append-only.md actually recorded, independently of HTTP.
func TestAllowDeleteMatchesTheSpike(t *testing.T) {
	// Every DELETE run A of the spike issued.
	for _, k := range []string{
		"s35391b98f24603bae4bd9a4e4e5408e2-s7cd827313c8b69b0144",
		"s3c3e403825b960e0fa5b074dc87161bf-s0a2c2b13a477b485144",
		"sf1063f400a6a3c5c6ca600639a16670c-s35333dd17da937cc144",
		"sc4821adb8dc5474901bcd5873432511e-sdb64a4c743afb694144",
		"s99cdbbb8405eade150a4ec431d4e8485-s9ab58a6109481cd5144",
		"sd8bfa36bd7e312b51240f6575dcc83d6-sdc1a9714350e9074144",
		"s1031ec102af79ff964d101d637c96383-s1d91d1fb50da18a3144",
		"s9cf7969cd92d7efac62578ec022b5b6f-s6bf4599439c9ef28144",
	} {
		require.True(t, allowDelete(k), "session marker %q must be deletable", k)
	}
}

func TestAllowDeleteRejectsEverythingElse(t *testing.T) {
	for _, k := range []string{
		// "xs" is the single-epoch compaction prefix: an unanchored "s" match
		// would let it through (spike section 7.3).
		"xs1234567890abcdef1234",
		"xn0_1234567890abcdef12",
		"xe1234567890abcdef1234",
		"pdeadbeefdeadbeefdeadbeefdeadbeef",
		"q1234567890abcdef1234567890abcdef",
		"n1234567890abcdef1234567890abcdef",
		"kopia.repository",
		"kopia.repository.backup.20260902",
		"kopia.blobcfg",
		"kopia.maintenance",
		".storageconfig",
		"_log_20260902_120000",
		"s",                 // too short to be a session marker
		"sdeadbeef",         // 8 hex, under the 16 the pattern requires
		"Sdeadbeefdeadbeef", // capital S is not the session prefix
		"",
	} {
		require.False(t, allowDelete(k), "%q must not be deletable", k)
	}
}

// TestDeviceOverwriteIsAlwaysFalse states the second half of the allowlist:
// allowOverwrite is empty, so the device path passes a constant false and no
// key class can replace another.
func TestDeviceOverwriteIsAlwaysFalse(t *testing.T) {
	require.False(t, deviceOverwrite, "a device PUT must never reach the store with overwrite=true")
}

func TestKeyClassNamesTheBlobClass(t *testing.T) {
	// The classes are a closed set: a log line must never carry any part of a
	// blob name.
	for key, want := range map[string]string{
		"s35391b98f24603bae4bd9a4e4e5408e2-s7cd8": "s",
		"xn0_1234":                         "xn",
		"xs1234":                           "xs",
		"pdeadbeef":                        "p",
		"q1234":                            "q",
		"kopia.repository":                 "kopia-meta",
		"kopia.maintenance":                "kopia-meta",
		"kopia.repository.backup.20260902": "kopia-meta",
		".storageconfig":                   "dot",
		"_log_20260902":                    "_log_",
		"":                                 "",
	} {
		require.Equal(t, want, keyClass(key), "key %q", key)
	}
}
