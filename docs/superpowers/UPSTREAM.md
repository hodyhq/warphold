# Upstream merge procedure

WarpHold tracks kopia/kopia. Merge upstream at least monthly:

    git fetch upstream master
    git merge upstream/master

Expected conflicts only in the four touched files (grep `warphold:` to find every touch):
cli/app.go, cli/command_server_start.go, Makefile, .goreleaser.yml.
After resolving: `CGO_ENABLED=0 go build ./... && go test ./fleet/... ./agent/... ./cli/...`.
