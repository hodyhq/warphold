#!/bin/sh
# warphold: deb/rpm postremove hook (nfpm scripts.postremove in .goreleaser.yml).
# No-op: the package never creates any user-owned state (no system user, no
# /var/lib/warphold — that's fleet.sh's job only), so there is nothing to
# clean up here. Repository data, config, and any `warphold app`/`fleet`
# setup remain untouched on removal.
exit 0
