#!/usr/bin/env bash
# Test scripts/install/fleet.sh against a fake release served over HTTP.
#
# The fake release is a real warphold binary built from this tree, packed the
# way goreleaser packs it, with a matching checksums.txt - so the test
# exercises the same download, checksum and extract path a user gets.
#
# The install itself runs against WARPHOLD_INSTALL_ROOT (a temporary
# directory) with --no-systemd, because this machine has no container runtime
# to give the script a throwaway root filesystem. What that costs: the system
# user, the ownership changes and "systemctl enable --now" are not exercised.
# In their place the test runs the ExecStart line out of the generated unit
# directly and proves the server it describes really does come up and write a
# setup token. Run this inside a container (as root, with systemd) and the
# same assertions cover the real thing; that is the only part that changes.
set -euo pipefail

cd "$(dirname "$0")/.."
REPO_DIR="$PWD"
FLEET_SH="$REPO_DIR/scripts/install/fleet.sh"

WORK="$(mktemp -d)"
SERVER_PID=""
APP_PID=""
cleanup() {
  [ -n "$APP_PID" ] && kill "$APP_PID" 2>/dev/null || true
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
bad()  { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

# ---------------------------------------------------------- fake release

case "$(uname -m)" in
  x86_64) RELARCH=x64 ;;
  aarch64|arm64) RELARCH=arm64 ;;
  *) echo "unsupported test architecture $(uname -m)" >&2; exit 1 ;;
esac

VER=0.0.0-test
NAME="warphold-$VER-linux-$RELARCH"
echo "== building the fake release ($NAME)"
mkdir -p "$WORK/stage/$NAME" "$WORK/rel/download/v$VER" "$WORK/rel/download/v$VER-bad"
CGO_ENABLED=0 go build -o "$WORK/stage/$NAME/warphold" "$REPO_DIR"
tar -czf "$WORK/rel/download/v$VER/$NAME.tar.gz" -C "$WORK/stage" "$NAME"
( cd "$WORK/rel/download/v$VER" && sha256sum "$NAME.tar.gz" > checksums.txt )

# The tampered release: same tarball, a checksum that does not match it.
cp "$WORK/rel/download/v$VER/$NAME.tar.gz" "$WORK/rel/download/v$VER-bad/"
sed 's/^./0/' "$WORK/rel/download/v$VER/checksums.txt" > "$WORK/rel/download/v$VER-bad/checksums.txt"

PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
python3 -m http.server "$PORT" --directory "$WORK/rel" >"$WORK/http.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 50); do
  curl -fsS "http://127.0.0.1:$PORT/download/v$VER/checksums.txt" >/dev/null 2>&1 && break
  sleep 0.1
done
export WARPHOLD_RELEASE_BASE="http://127.0.0.1:$PORT"

# ---------------------------------------------------------- 1. --dry-run

echo "== --dry-run writes nothing"
DRYROOT="$WORK/root-dry"
mkdir -p "$DRYROOT"
WARPHOLD_INSTALL_ROOT="$DRYROOT" sh "$FLEET_SH" --version "v$VER" --dry-run --no-systemd > "$WORK/dry.out"
check "dry-run said what it would install" "grep -q 'would then' '$WORK/dry.out'"
check "dry-run named the unit"             "grep -q 'warphold.service' '$WORK/dry.out'"
check "dry-run wrote no files"             "[ -z \"\$(find '$DRYROOT' -mindepth 1 -print -quit)\" ]"

# ---------------------------------------------------------- 2. fresh install

echo "== fresh install (no --bind: loopback, proxy in front)"
ROOT="$WORK/root"
WARPHOLD_INSTALL_ROOT="$ROOT" sh "$FLEET_SH" --version "v$VER" --no-systemd > "$WORK/install.out"
UNIT="$ROOT/etc/systemd/system/warphold.service"
ENVF="$ROOT/etc/warphold/env"
check "binary installed"        "[ -x '$ROOT/usr/local/bin/warphold' ]"
check "state dir is 0750"       "[ \"\$(stat -c %a '$ROOT/var/lib/warphold')\" = 750 ]"
check "hosted dir is 0700"      "[ \"\$(stat -c %a '$ROOT/srv/warphold/hosted')\" = 700 ]"
check "env file is 0640"        "[ \"\$(stat -c %a '$ENVF')\" = 640 ]"
check "server password set"     "grep -q '^KOPIA_SERVER_PASSWORD=.\{20,\}' '$ENVF'"
check "control password set"    "grep -q '^KOPIA_SERVER_CONTROL_PASSWORD=.\{20,\}' '$ENVF'"
check "no password in the unit" "! grep -q 'server-password' '$UNIT'"
check "unit is --insecure"      "grep -q ' --insecure ' '$UNIT'"
check "unit binds loopback"     "grep -q -- '--address http://127.0.0.1:51515' '$UNIT'"
check "unit is --no-grpc"       "grep -q -- '--no-grpc' '$UNIT'"
check "unit is hardened"        "grep -q '^ProtectSystem=strict' '$UNIT' && grep -q '^NoNewPrivileges=true' '$UNIT'"
check "unit runs as warphold"   "grep -q '^User=warphold' '$UNIT'"

# ---------------------------------------------------------- 3. the unit works

echo "== the ExecStart line in the unit actually serves Fleet"
EXECSTART="$(sed -n 's/^ExecStart=//p' "$UNIT")"
set -a
# shellcheck disable=SC1090  # a file this test just generated
. "$ENVF"
set +a
# shellcheck disable=SC2086  # the unit's own words, split on purpose
$EXECSTART >"$WORK/warphold.log" 2>&1 &
APP_PID=$!
STATUS=""
for _ in $(seq 60); do
  STATUS="$(curl -fsS --max-time 2 http://127.0.0.1:51515/api/v1/fleet/status 2>/dev/null || true)"
  [ -n "$STATUS" ] && break
  sleep 0.5
done
check "fleet status answers"  "[ '$STATUS' = '{\"activated\":false}' ]"
check "setup token written"   "[ -s '$ROOT/var/lib/warphold/fleet/setup-token' ]"
kill "$APP_PID" 2>/dev/null || true
wait "$APP_PID" 2>/dev/null || true
APP_PID=""

# ---------------------------------------------------------- 4. idempotent

echo "== re-run upgrades the binary and leaves state alone"
ENV_BEFORE="$(sha256sum "$ENVF")"
TOKEN_BEFORE="$(cat "$ROOT/var/lib/warphold/fleet/setup-token")"
WARPHOLD_INSTALL_ROOT="$ROOT" sh "$FLEET_SH" --version "v$VER" --no-systemd > "$WORK/reinstall.out"
check "re-run succeeded"          "grep -q 'installed $ROOT/usr/local/bin/warphold' '$WORK/reinstall.out'"
check "re-run kept the env file"  "[ \"\$(sha256sum '$ENVF')\" = '$ENV_BEFORE' ]"
check "re-run kept the token"     "[ \"\$(cat '$ROOT/var/lib/warphold/fleet/setup-token')\" = '$TOKEN_BEFORE' ]"
check "re-run said so"            "grep -q 'keeping the existing' '$WORK/reinstall.out'"

# ---------------------------------------------------------- 5. bad checksum

echo "== a tampered checksum aborts before anything is installed"
BADROOT="$WORK/root-bad"
mkdir -p "$BADROOT"
set +e
WARPHOLD_INSTALL_ROOT="$BADROOT" sh "$FLEET_SH" --version "v$VER-bad" --no-systemd > "$WORK/bad.out" 2>&1
RC=$?
set -e
check "exited non-zero"          "[ $RC -ne 0 ]"
check "said checksum mismatch"   "grep -qi 'checksum mismatch' '$WORK/bad.out'"
check "installed nothing"        "[ ! -e '$BADROOT/usr/local/bin/warphold' ]"

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
