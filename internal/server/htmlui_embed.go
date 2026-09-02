//go:build !nohtmlui

package server

import (
	"net/http"

	warpholdui "github.com/hodyhq/warphold-ui" // warphold: serve the WarpHold UI instead of upstream htmluibuild
)

// AssetFile exposes HTML UI files.
func AssetFile() http.FileSystem {
	return warpholdui.AssetFile()
}
