#!/bin/sh
# WarpHold Fleet server installer.
#
#   curl -fsSL https://get.warphold.com/fleet.sh -o fleet.sh && sh fleet.sh
#
# Installs the warphold binary, creates the "warphold" system user and its
# directories, writes /etc/warphold/env and a systemd unit that binds the
# server to the LAN behind a reverse proxy, starts it, and prints the URL and
# the one-time setup token.
#
# Every download is verified against the release's checksums.txt before
# anything is installed; a mismatch aborts before a single file is written.
# The trust anchor for that checksum is the TLS connection to the release
# host: checksums.txt comes from the same release as the artifact, so it
# proves the download was not corrupted, not that it was not substituted at
# the source. Verifying the detached signature of checksums.txt lands with
# the release signing key.
#
# This installs the release tarball on every distribution, deliberately: the
# deb and the rpm are separate release assets with their own install path, so
# there is no packaging-family detection here to get wrong.
#
# Re-running upgrades the binary and rewrites the unit. It never touches
# /etc/warphold/env, the state directory or the data directory: the generated
# server passwords and the Fleet database survive an upgrade.
#
# Options:
#   --version <tag>   install this release instead of the latest
#   --bind <addr>     address the server listens on (default: 127.0.0.1, which
#                     needs the reverse proxy on this same host)
#   --dry-run         print what would be done, write nothing
#   --no-systemd      write the unit but do not enable or start it
#
# Environment:
#   WARPHOLD_RELEASE_BASE   release asset base URL (default: GitHub releases)
#   WARPHOLD_VERSION        same as --version
#   WARPHOLD_INSTALL_ROOT   prefix every path with this directory. Test-only:
#                           it also skips the root check, the system user and
#                           the ownership changes, none of which are possible
#                           unprivileged.
#   WARPHOLD_SETUP_PUBLIC_URL / _EMAIL / _PASSWORD / _PASSPHRASE
#                           when all four are set, activate non-interactively
#                           instead of printing the setup token. The secrets
#                           are handed to the child in its environment, never
#                           in its arguments, so they stay out of "ps".
set -eu

REPO="hodyhq/warphold"
RELEASE_BASE="${WARPHOLD_RELEASE_BASE:-https://github.com/$REPO/releases}"
VERSION="${WARPHOLD_VERSION:-}"
ROOT="${WARPHOLD_INSTALL_ROOT:-}"
PORT=51515
BIND=""
DRY=0
NO_SYSTEMD=0
SVC_USER=warphold

say() { printf '%s\n' "$*"; }
die() { printf 'fleet.sh: %s\n' "$*" >&2; exit 1; }
run() { printf '+ %s\n' "$*"; [ "$DRY" = 1 ] || "$@"; }

# as_service_user runs a command as the account the service runs as. Anything
# that writes into the state or data directory has to: activation creates
# fleet.db, seal.key and the host repository under directories already chowned
# to $SVC_USER, and a file root writes there is a file the service cannot
# rewrite afterwards.
#
# Secrets reach the child through the environment of the "env" that invokes
# this - runuser and su both keep a non-login environment - so they are never
# an argument (visible in "ps") and never a file (readable on disk).
#
# Under WARPHOLD_INSTALL_ROOT there is no service user and no privilege to
# drop, so the command runs as-is.
as_service_user() {
  if [ -n "$ROOT" ]; then
    "$@"
  elif command -v runuser >/dev/null 2>&1; then
    runuser -u "$SVC_USER" -- "$@"
  else
    # su takes one shell string, so every argument is single-quoted into it.
    q=''
    for a in "$@"; do
      q="$q '$(printf '%s' "$a" | sed "s/'/'\\\\''/g")'"
    done
    su -s /bin/sh -c "$q" "$SVC_USER"
  fi
}

# fetch downloads a URL to stdout, refusing a plaintext or downgraded
# redirect for anything that starts out over TLS: --proto '=https' allows only
# https for the request and every redirect it follows, and --tlsv1.2 sets the
# floor. A WARPHOLD_RELEASE_BASE that is not https:// - a mirror on the LAN,
# this repository's own test server - is fetched without them, because it is
# the operator's own trust decision and not something this script can improve.
fetch() {
  case "$1" in
    https://*) curl -fsSL --proto '=https' --tlsv1.2 "$1" ;;
    *) curl -fsSL "$1" ;;
  esac
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
    --version=*) VERSION="${1#--version=}"; shift ;;
    --bind) [ $# -ge 2 ] || die "--bind needs a value"; BIND="$2"; shift 2 ;;
    --bind=*) BIND="${1#--bind=}"; shift ;;
    --dry-run) DRY=1; shift ;;
    --no-systemd) NO_SYSTEMD=1; shift ;;
    # Print the header comment, however long it grows: every line from the
    # second to the first that is not a comment.
    -h|--help) awk 'NR > 1 { if (!/^#/) exit; sub(/^# ?/, ""); print }' "$0"; exit 0 ;;
    *) die "unknown argument $1 (try --help)" ;;
  esac
done

[ "$(uname -s)" = Linux ] || die "this installer is for Linux; WarpHold publishes no other server build"

for tool in curl tar sha256sum install; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

# The whole install needs root: a system user and a unit in
# /etc/systemd/system are not writable any other way. WARPHOLD_INSTALL_ROOT
# relocates every path, so the check does not apply to it.
if [ -z "$ROOT" ] && [ "$(id -u)" -ne 0 ]; then
  die "run this as root (sudo sh fleet.sh); it creates a system user and a systemd unit"
fi

BIN_DIR="$ROOT/usr/local/bin"
STATE_DIR="$ROOT/var/lib/warphold"
DATA_DIR="$ROOT/srv/warphold"
ETC_DIR="$ROOT/etc/warphold"
ENV_FILE="$ETC_DIR/env"
UNIT_DIR="$ROOT/etc/systemd/system"
UNIT_FILE="$UNIT_DIR/warphold.service"
# The paths the unit and the server see. Under WARPHOLD_INSTALL_ROOT they are
# the prefixed ones, because that is where the binary will actually look.
CONFIG_FILE="$STATE_DIR/repository.config"
SETUP_TOKEN_FILE="$STATE_DIR/fleet/setup-token"

# Loopback by default: the server speaks plain HTTP (a TLS reverse proxy is
# assumed in front of it), and defaulting to a LAN address would put an
# unencrypted control plane, its enrollment tokens and its setup token on the
# network of whoever ran the one-liner. A proxy on another host needs
# --bind <this host's LAN address> and a firewall that lets only the proxy
# reach the port.
[ -n "$BIND" ] || BIND=127.0.0.1
SERVER_URL="http://$BIND:$PORT"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
trap 'rm -rf "$TMP"; exit 130' INT HUP TERM

# ---------------------------------------------------------------- download

case "$(uname -m)" in
  x86_64|amd64) ARCH_RE='(x64|amd64)' ;;
  aarch64|arm64) ARCH_RE='(arm64|aarch64)' ;;
  *) die "unsupported architecture $(uname -m); WarpHold publishes linux amd64 and arm64" ;;
esac

if [ -z "$VERSION" ]; then
  case "$RELEASE_BASE" in
    https://github.com/*)
      say "Resolving the latest release..."
      VERSION="$(fetch "https://api.github.com/repos/$REPO/releases/latest" |
        sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
      [ -n "$VERSION" ] || die "cannot resolve the latest release; pass --version"
      ;;
    *) die "WARPHOLD_RELEASE_BASE is set, so the version cannot be resolved; pass --version" ;;
  esac
fi

DL="$RELEASE_BASE/download/$VERSION"
say "Installing WarpHold $VERSION from $DL"

fetch "$DL/checksums.txt" > "$TMP/checksums.txt" || die "cannot download $DL/checksums.txt"

# The asset name comes out of checksums.txt rather than being built from a
# template here, so a change to the release naming cannot make this script
# download the wrong file or a file that does not exist.
ASSET="$(awk '{ print $NF }' "$TMP/checksums.txt" | sed 's/^\*//' |
  grep -E "^warphold-.*linux[-_]$ARCH_RE\.tar\.gz$" || true)"
[ -n "$ASSET" ] || die "no linux tarball for this architecture in $DL/checksums.txt"
[ "$(printf '%s\n' "$ASSET" | wc -l)" -eq 1 ] || die "more than one candidate tarball in checksums.txt: $ASSET"

fetch "$DL/$ASSET" > "$TMP/$ASSET" || die "cannot download $DL/$ASSET"

# Verify against the single expected line, not the whole file: sha256sum -c
# over checksums.txt would report the other assets as missing and, with
# --ignore-missing, would succeed on a file list that never included ours.
grep -E "[ *]$ASSET\$" "$TMP/checksums.txt" > "$TMP/expected.sha256" ||
  die "$ASSET has no checksum line in checksums.txt"
( cd "$TMP" && sha256sum -c expected.sha256 ) ||
  die "checksum mismatch for $ASSET - nothing was installed"

tar -xzf "$TMP/$ASSET" -C "$TMP"
NEWBIN="$(find "$TMP" -type f -name warphold -perm -u+x | head -1)"
[ -n "$NEWBIN" ] || die "no warphold binary inside $ASSET"

if [ "$DRY" = 1 ]; then
  say ""
  say "--dry-run: verified $ASSET, and would then:"
fi

# ------------------------------------------------------------------ install

if [ -z "$ROOT" ]; then
  if id -u "$SVC_USER" >/dev/null 2>&1; then
    say "+ system user $SVC_USER already exists"
  else
    NOLOGIN=/usr/sbin/nologin
    [ -x "$NOLOGIN" ] || NOLOGIN=/sbin/nologin
    run useradd --system --home-dir "$STATE_DIR" --shell "$NOLOGIN" "$SVC_USER"
  fi
fi

run install -d -m 0755 "$BIN_DIR"
run install -d -m 0750 "$STATE_DIR"
run install -d -m 0755 "$DATA_DIR"
run install -d -m 0700 "$DATA_DIR/hosted"
run install -d -m 0750 "$ETC_DIR"

# Install through a temporary name in the same directory: replacing a running
# binary in place fails with ETXTBSY, and a partial copy of the server would
# be worse than a failed upgrade.
if [ "$DRY" = 1 ]; then
  say "+ install -m 0755 <extracted warphold> $BIN_DIR/warphold"
else
  install -m 0755 "$NEWBIN" "$BIN_DIR/warphold.new"
  mv "$BIN_DIR/warphold.new" "$BIN_DIR/warphold"
  say "+ installed $BIN_DIR/warphold"
fi

# The two server passwords are generated once and never rotated by a re-run:
# rotating them silently would lock out anything already configured with them.
if [ -f "$ENV_FILE" ]; then
  say "+ keeping the existing $ENV_FILE"
elif [ "$DRY" = 1 ]; then
  say "+ write $ENV_FILE (0640 root:$SVC_USER) with generated server passwords"
else
  ( umask 077
    { printf 'KOPIA_SERVER_PASSWORD=%s\n' "$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
      printf 'KOPIA_SERVER_CONTROL_PASSWORD=%s\n' "$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
    } > "$ENV_FILE" )
  chmod 0640 "$ENV_FILE"
  say "+ wrote $ENV_FILE"
fi

# Passwords reach the server through EnvironmentFile only. Passing them as
# --server-password / --server-control-password would put them in the unit
# file and in every "ps" listing on the host.
if [ "$DRY" = 1 ]; then
  say "+ write $UNIT_FILE (ExecStart ... server start --insecure --address $SERVER_URL --no-grpc)"
else
  install -d -m 0755 "$UNIT_DIR"
  cat > "$UNIT_FILE" <<EOF
[Unit]
Description=WarpHold Fleet server
After=network-online.target
Wants=network-online.target

[Service]
User=$SVC_USER
Group=$SVC_USER
EnvironmentFile=$ENV_FILE
ExecStart=$BIN_DIR/warphold --config-file $CONFIG_FILE server start --insecure --address $SERVER_URL --no-grpc --server-username admin
Restart=on-failure
RestartSec=10
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=$STATE_DIR $DATA_DIR
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF
  chmod 0644 "$UNIT_FILE"
  say "+ wrote $UNIT_FILE"
fi

if [ -z "$ROOT" ]; then
  run chown -R "$SVC_USER:$SVC_USER" "$STATE_DIR" "$DATA_DIR"
  run chown "root:$SVC_USER" "$ENV_FILE"
fi

if [ "$DRY" = 1 ]; then
  say "+ systemctl daemon-reload && systemctl enable --now warphold"
  say "+ wait for $SERVER_URL/api/v1/fleet/status, then print the setup token"
  exit 0
fi

if [ "$NO_SYSTEMD" = 1 ]; then
  say ""
  say "--no-systemd: the unit was written but not started. Start it with:"
  say "    systemctl daemon-reload && systemctl enable --now warphold"
  exit 0
fi

run systemctl daemon-reload
run systemctl enable --now warphold

# ---------------------------------------------------------------- wait, print

say "Waiting for the server to answer on $SERVER_URL ..."
i=0
until curl -fsS --max-time 3 "$SERVER_URL/api/v1/fleet/status" >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -lt 60 ] || die "the server did not answer within 60s; check 'journalctl -u warphold'"
  sleep 1
done

PROXY_NOTE="The server speaks plain HTTP on $SERVER_URL because a TLS reverse proxy is
assumed in front of it. That proxy must:
  - terminate HTTPS for the public URL and forward to $SERVER_URL
  - set X-Forwarded-Proto: https
  - pass the Host header through unchanged
  - be the only thing that can reach port $PORT (firewall the rest off)
$SERVER_URL is reachable from this host only, so the proxy has to run here
too. To put it on another machine, re-run with --bind <this host's LAN
address>: the proxy-to-Fleet hop is then unencrypted and carries enrollment
tokens and the setup token, so keep it on a trusted network and firewall the
port to the proxy alone."

if [ -n "${WARPHOLD_SETUP_PUBLIC_URL:-}" ] && [ -n "${WARPHOLD_SETUP_EMAIL:-}" ] &&
   [ -n "${WARPHOLD_SETUP_PASSWORD:-}" ] && [ -n "${WARPHOLD_SETUP_PASSPHRASE:-}" ]; then
  say "Activating non-interactively as $WARPHOLD_SETUP_EMAIL ..."
  set -- --config-file "$CONFIG_FILE" fleet activate --email "$WARPHOLD_SETUP_EMAIL"
  # --public-url lands with the setup work; until then activation succeeds
  # without it and the public URL is set in the wizard.
  if "$BIN_DIR/warphold" fleet activate --help 2>&1 | grep -q -- --public-url; then
    set -- "$@" --public-url "$WARPHOLD_SETUP_PUBLIC_URL"
  else
    say "note: this build has no --public-url yet; set it in the dashboard afterwards."
  fi
  env WARPHOLD_ADMIN_PASSWORD="$WARPHOLD_SETUP_PASSWORD" \
      WARPHOLD_SEAL_PASSPHRASE="$WARPHOLD_SETUP_PASSPHRASE" \
      as_service_user "$BIN_DIR/warphold" "$@"
  say ""
  say "Fleet is activated. Sign in at $WARPHOLD_SETUP_PUBLIC_URL"
  say ""
  say "$PROXY_NOTE"
  exit 0
fi

say "Waiting for the setup token ..."
i=0
until [ -s "$SETUP_TOKEN_FILE" ]; do
  i=$((i + 1))
  [ "$i" -lt 30 ] || die "the server did not write $SETUP_TOKEN_FILE; check 'journalctl -u warphold'"
  sleep 1
done

say ""
say "WarpHold Fleet $VERSION is running."
say ""
say "  1. Open:        $SERVER_URL"
say "  2. Setup token: $(cat "$SETUP_TOKEN_FILE")"
say "     (from        $SETUP_TOKEN_FILE - it is deleted once activation succeeds)"
say ""
say "$PROXY_NOTE"
say ""
say "To activate without the browser, set all four of these and re-run this script:"
say "    WARPHOLD_SETUP_PUBLIC_URL=https://fleet.example.com \\"
say "    WARPHOLD_SETUP_EMAIL=admin@example.com \\"
say "    WARPHOLD_SETUP_PASSWORD=... WARPHOLD_SETUP_PASSPHRASE=... sh fleet.sh"
say "which runs:"
say "    runuser -u $SVC_USER -- warphold --config-file $CONFIG_FILE fleet activate \\"
say "        --email <email> --public-url <public url>"
say "(as $SVC_USER, so the database, the seal key and the host repository stay"
say " writable by the service)"
say "with the password and passphrase read from the environment, so neither"
say "appears in 'ps'."
