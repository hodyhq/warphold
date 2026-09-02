# Upstream merge procedure

WarpHold tracks kopia/kopia. Merge upstream at least monthly:

    git fetch upstream master
    git merge upstream/master

Expected conflicts only in the five touched files (grep `warphold:` to find every touch):
cli/app.go, cli/command_server_start.go, main.go, Makefile, .goreleaser.yml.

Plus `go.mod` / `go.sum`: the fork adds its own dependencies (modernc.org/sqlite and
friends) while upstream bumps versions in the same hunks, so both conflict routinely.
Resolve by keeping both sides' requirements, then `go mod tidy`.
After resolving: `CGO_ENABLED=0 go build ./... && go test ./fleet/... ./agent/... ./cli/...`.

The Electron packaging under app/ and tools/docker-publish.sh are upstream-only and untouched; they still reference dist/kopia_* and are not part of WarpHold releases.
