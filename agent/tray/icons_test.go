package tray_test

import (
	"bytes"
	"image/png"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/agent/tray"
)

// TestIconsEmbedded pins that every tone the model can produce has an
// embedded icon at both sizes: a missing one would leave the tray with no
// icon at all on the panel, which is invisible rather than noisy.
func TestIconsEmbedded(t *testing.T) {
	for _, tone := range []tray.Tone{tray.ToneGood, tray.ToneEmber, tray.ToneWarn, tray.ToneBad, tray.ToneDim} {
		for _, size := range []int{22, 44} {
			b, err := tray.Icon(tone, size)
			require.NoError(t, err)

			cfg, err := png.DecodeConfig(bytes.NewReader(b))
			require.NoError(t, err, "tone %v at %d is not a PNG", tone, size)
			require.Equal(t, size, cfg.Width)
			require.Equal(t, size, cfg.Height)
		}
	}

	_, err := tray.Icon("nope", 22)
	require.Error(t, err)
}
