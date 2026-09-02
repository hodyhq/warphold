package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDownloadServesOwnBinaryAndStagedOnes(t *testing.T) {
	h := newHarness(t)
	h.activateAndLogin()

	// The server only ever offers its own executable for its own GOOS/GOARCH
	// (dl.go's regex fixes os to "linux"), so this path only exercises the
	// self-serve branch on a Linux runner; elsewhere it exercises the same
	// staged-binary path as the riscv64 case below.
	if runtime.GOOS == "linux" {
		res, err := http.Get(h.srv.URL + "/dl/warphold-linux-" + runtime.GOARCH)
		require.NoError(t, err)
		defer res.Body.Close()
		require.Equal(t, 200, res.StatusCode)
		require.Equal(t, "application/octet-stream", res.Header.Get("Content-Type"))
		self, _ := os.Executable()
		st, _ := os.Stat(self)
		require.Equal(t, st.Size(), res.ContentLength)
	} else {
		staged := filepath.Join(h.stateDir, "binaries")
		require.NoError(t, os.MkdirAll(staged, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(staged, "warphold-linux-"+runtime.GOARCH), []byte("ELF-ish"), 0o755))
		res, err := http.Get(h.srv.URL + "/dl/warphold-linux-" + runtime.GOARCH)
		require.NoError(t, err)
		defer res.Body.Close()
		require.Equal(t, 200, res.StatusCode)
		require.Equal(t, "application/octet-stream", res.Header.Get("Content-Type"))
	}

	res, _ := http.Get(h.srv.URL + "/dl/warphold-linux-riscv64")
	require.Equal(t, 404, res.StatusCode)

	staged := filepath.Join(h.stateDir, "binaries")
	require.NoError(t, os.MkdirAll(staged, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staged, "warphold-linux-riscv64"), []byte("ELF-ish"), 0o755))
	res, _ = http.Get(h.srv.URL + "/dl/warphold-linux-riscv64")
	require.Equal(t, 200, res.StatusCode)

	res, _ = http.Get(h.srv.URL + "/dl/warphold-linux-../seal.key")
	require.Equal(t, 404, res.StatusCode)
}
