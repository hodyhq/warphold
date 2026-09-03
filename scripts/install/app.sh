#!/bin/sh
# WarpHold single-machine installer.
#
#   curl -fsSL https://get.warphold.com/app.sh -o app.sh && sh app.sh
#
# Installs the warphold binary for this user (~/.local/bin), and, when this
# machine is already enrolled with a Fleet server, the user-scope agent
# service and the tray autostart entry through "warphold agent install".
#
# This installs the app for one machine and nothing else: no service account,
# no machine-wide configuration, no control plane. A WarpHold server for a
# household or an office is a different installer.
#
# Every download is verified against the release's checksums.txt before
# anything is installed; a mismatch aborts before a single file is written.
# The trust anchor for that checksum is the TLS connection to the release
# host; verifying the detached signature of checksums.txt lands with the
# release signing key.
# Re-running upgrades the binary and leaves configuration and state alone.
#
# Options:
#   --version <tag>   install this release instead of the latest
#   --system          install to /usr/local/bin for every user (needs root)
#   --dry-run         print what would be done, write nothing
#   --no-open         do not open the app in a browser
#
# Environment:
#   WARPHOLD_RELEASE_BASE   release asset base URL (default: GitHub releases)
#   WARPHOLD_VERSION        same as --version
#   WARPHOLD_INSTALL_ROOT   prefix the --system paths with this directory
#                           (test-only). A user-scope install is relocated
#                           with HOME / XDG_CONFIG_HOME instead.
set -eu

REPO="hodyhq/warphold"
RELEASE_BASE="${WARPHOLD_RELEASE_BASE:-https://github.com/$REPO/releases}"
VERSION="${WARPHOLD_VERSION:-}"
ROOT="${WARPHOLD_INSTALL_ROOT:-}"
SCOPE=user
DRY=0
OPEN=1
LOCAL_URL="http://127.0.0.1:51515"

say() { printf '%s\n' "$*"; }
die() { printf 'app.sh: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
    --version=*) VERSION="${1#--version=}"; shift ;;
    --system) SCOPE=system; shift ;;
    --dry-run) DRY=1; shift ;;
    --no-open) OPEN=0; shift ;;
    -h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument $1 (try --help)" ;;
  esac
done

[ "$(uname -s)" = Linux ] || die "this installer is for Linux; WarpHold's macOS and Windows apps are not out yet"

for tool in curl tar sha256sum install; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

if [ "$SCOPE" = system ]; then
  [ "$(id -u)" -eq 0 ] || [ -n "$ROOT" ] || die "--system installs for every user; run it as root"
  BIN_DIR="$ROOT/usr/local/bin"
else
  [ "$(id -u)" -ne 0 ] || die "run this as the person who will use WarpHold, not as root (use --system for a machine-wide install)"
  [ -n "${HOME:-}" ] || die "HOME is not set"
  BIN_DIR="$HOME/.local/bin"
fi

# Where "warphold agent enroll" keeps this machine's enrollment, mirroring
# agent/state: WARPHOLD_STATE_DIR wins, then XDG_CONFIG_HOME, then ~/.config.
if [ "$SCOPE" = system ]; then
  STATE_DIR="${WARPHOLD_STATE_DIR:-$ROOT/etc/warphold}"
else
  STATE_DIR="${WARPHOLD_STATE_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/warphold}"
fi

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
      VERSION="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
        sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
      [ -n "$VERSION" ] || die "cannot resolve the latest release; pass --version"
      ;;
    *) die "WARPHOLD_RELEASE_BASE is set, so the version cannot be resolved; pass --version" ;;
  esac
fi

DL="$RELEASE_BASE/download/$VERSION"
say "Installing WarpHold $VERSION from $DL"

curl -fsSL "$DL/checksums.txt" -o "$TMP/checksums.txt" || die "cannot download $DL/checksums.txt"

# The asset name comes out of checksums.txt rather than being built from a
# template here, so a change to the release naming cannot make this script
# download the wrong file or a file that does not exist.
ASSET="$(awk '{ print $NF }' "$TMP/checksums.txt" | sed 's/^\*//' |
  grep -E "^warphold-.*linux[-_]$ARCH_RE\.tar\.gz$" || true)"
[ -n "$ASSET" ] || die "no linux tarball for this architecture in $DL/checksums.txt"
[ "$(printf '%s\n' "$ASSET" | wc -l)" -eq 1 ] || die "more than one candidate tarball in checksums.txt: $ASSET"

curl -fsSL "$DL/$ASSET" -o "$TMP/$ASSET" || die "cannot download $DL/$ASSET"

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

# ------------------------------------------------------------------ install

if [ "$DRY" = 1 ]; then
  say ""
  say "--dry-run: verified $ASSET, and would then:"
  say "+ install -m 0755 <extracted warphold> $BIN_DIR/warphold"
  if [ -f "$STATE_DIR/agent.json" ]; then
    say "+ $BIN_DIR/warphold agent install --scope $SCOPE"
  else
    say "+ print how to enroll or how to start the app (this machine is not enrolled)"
  fi
  exit 0
fi

install -d -m 0755 "$BIN_DIR"
# Install through a temporary name in the same directory: replacing a running
# binary in place fails with ETXTBSY.
install -m 0755 "$NEWBIN" "$BIN_DIR/warphold.new"
mv "$BIN_DIR/warphold.new" "$BIN_DIR/warphold"
say "+ installed $BIN_DIR/warphold"

case ":${PATH:-}:" in
  *":$BIN_DIR:"*) ;;
  *) say ""
     say "note: $BIN_DIR is not in your PATH. Add this to your shell profile:"
     say "    export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac

# The service that "agent install" writes runs "warphold agent run", which
# needs this machine's enrollment: installing it before that would leave a
# unit restarting into the same error until systemd gives up. So the service
# and the tray entry are written once the machine is enrolled - which is also
# what the enrollment one-liner does on its own.
if [ -f "$STATE_DIR/agent.json" ]; then
  say "+ $BIN_DIR/warphold agent install --scope $SCOPE"
  if ! "$BIN_DIR/warphold" agent install --scope "$SCOPE"; then
    say ""
    say "warning: the service and tray files were written, but systemd would not"
    say "         start them. Re-run once your session is up:"
    say "             $BIN_DIR/warphold agent install --scope $SCOPE"
  fi
else
  say ""
  say "This machine is not backing anything up yet. Either:"
  say "  - run the app for this machine alone:"
  say "        $BIN_DIR/warphold server start --insecure --address $LOCAL_URL"
  say "  - or join it to a WarpHold server, with the one-line command that"
  say "    server shows under \"Add device\"."
fi

# Only open something that is actually there: the app is a command this user
# runs, not a service this script started.
if [ "$OPEN" = 1 ] && [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ] &&
   command -v xdg-open >/dev/null 2>&1 &&
   curl -fsS --max-time 1 "$LOCAL_URL" >/dev/null 2>&1; then
  say "+ opening $LOCAL_URL"
  xdg-open "$LOCAL_URL" >/dev/null 2>&1 || true
fi

say ""
say "WarpHold $VERSION installed."
