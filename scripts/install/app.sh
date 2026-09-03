#!/bin/sh
# WarpHold single-machine installer.
#
#   curl -fsSL https://get.warphold.com/app.sh -o app.sh && sh app.sh
#
# Installs the warphold binary for this user (~/.local/bin) and a service that
# backs this machine up:
#   - not enrolled anywhere: "warphold app install" - the standalone app, its
#     own local engine and the tray, and the app is opened in a browser.
#   - already enrolled with a Fleet server: "warphold agent install" - the
#     agent service and the tray, exactly as the enrollment one-liner does.
#
# This installs the release tarball on every distribution, deliberately: the
# deb and the rpm are separate release assets with their own install path.
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

say() { printf '%s\n' "$*"; }
die() { printf 'app.sh: %s\n' "$*" >&2; exit 1; }

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
    --system) SCOPE=system; shift ;;
    --dry-run) DRY=1; shift ;;
    --no-open) OPEN=0; shift ;;
    # Print the header comment, however long it grows: every line from the
    # second to the first that is not a comment.
    -h|--help) awk 'NR > 1 { if (!/^#/) exit; sub(/^# ?/, ""); print }' "$0"; exit 0 ;;
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

# ------------------------------------------------------------------ install

if [ "$DRY" = 1 ]; then
  say ""
  say "--dry-run: verified $ASSET, and would then:"
  say "+ install -m 0755 <extracted warphold> $BIN_DIR/warphold"
  if [ -f "$STATE_DIR/agent.json" ]; then
    say "+ $BIN_DIR/warphold agent install --scope $SCOPE"
  else
    say "+ $BIN_DIR/warphold app install"
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
     say "note: $BIN_DIR is not in your PATH. Add it to your shell profile:"
     case "${SHELL:-}" in
       */fish) say "    fish_add_path $BIN_DIR" ;;
       *) say "    export PATH=\"$BIN_DIR:\$PATH\""
          say "  or, in fish:"
          say "    fish_add_path $BIN_DIR" ;;
     esac ;;
esac

# Which service this machine gets depends on what it is. "agent run" needs an
# enrollment, so installing that unit on a machine that has none would leave
# systemd restarting into the same error until it gives up; "app run" needs
# nothing, and is the single-machine product.
if [ -f "$STATE_DIR/agent.json" ]; then
  MODE=agent
else
  MODE=app
fi

if [ "$MODE" = agent ]; then
  say "+ $BIN_DIR/warphold agent install --scope $SCOPE"
  if ! "$BIN_DIR/warphold" agent install --scope "$SCOPE"; then
    say ""
    say "warning: the service and tray files were written, but systemd would not"
    say "         start them. Re-run once your session is up:"
    say "             $BIN_DIR/warphold agent install --scope $SCOPE"
  fi
else
  say "+ $BIN_DIR/warphold app install"
  if ! "$BIN_DIR/warphold" app install; then
    say ""
    say "warning: the service and tray files were written, but systemd would not"
    say "         start them. Re-run once your session is up:"
    say "             $BIN_DIR/warphold app install"
  fi
fi

# The engine listens on a loopback port it picks at startup, and the URL that
# opens it carries a one-process session token, so the URL is asked for rather
# than assumed - and handed to the browser rather than printed here.
app_url() {
  i=0
  while [ "$i" -lt 40 ]; do
    if url="$("$BIN_DIR/warphold" app url 2>/dev/null)" && [ -n "$url" ]; then
      printf '%s\n' "$url"
      return 0
    fi
    i=$((i + 1))
    sleep 0.25
  done
  return 1
}

if [ "$MODE" = app ] && [ "$OPEN" = 1 ] &&
   [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ] &&
   command -v xdg-open >/dev/null 2>&1; then
  if URL="$(app_url)"; then
    say "+ opening WarpHold in your browser"
    xdg-open "$URL" >/dev/null 2>&1 || true
  else
    say ""
    say "note: the app service did not come up in time. Check it with:"
    say "    systemctl --user status warphold-app"
  fi
fi

say ""
if [ "$MODE" = app ]; then
  say "Open WarpHold any time with:"
  say "    $BIN_DIR/warphold app url        # prints the link; open it in a browser"
  say "The tray icon appears at your next login. To start it now:"
  say "    $BIN_DIR/warphold agent tray --scope app &"
  say ""
  say "To join this machine to a WarpHold server later, use the one-line"
  say "command that server shows under \"Add device\"."
else
  say "The tray icon appears at your next login; \"Details...\" in its menu opens"
  say "this machine's backups."
fi

say ""
say "WarpHold $VERSION installed."
