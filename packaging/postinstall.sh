#!/bin/sh
# warphold: deb/rpm postinstall hook (nfpm scripts.postinstall in .goreleaser.yml).
# Intentionally does nothing but print next steps — the package is the
# binary and the app, nothing more. It never creates the `warphold` system
# user, never creates /var/lib/warphold, and never starts, enables, or
# configures a service. All server provisioning (user, dirs, service) is
# fleet.sh's job only, run explicitly by the operator.
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
