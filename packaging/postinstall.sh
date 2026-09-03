#!/bin/sh
# warphold: deb/rpm postinstall hook (nfpm scripts.postinstall in .goreleaser.yml).
# Intentionally does nothing but print next steps — a package postinstall
# must never start, enable, or configure a service on its own; the operator
# chooses desktop vs. server mode explicitly.
set -e

cat <<'EOF'
warphold installed.

Next steps:
  - Desktop / single machine: run `warphold app install` to set up the
    local app and start backing up this machine.
  - Fleet server: run
      curl -fsSL https://get.warphold.com/fleet.sh | sh
    to install and configure the Fleet server on this machine.
EOF
