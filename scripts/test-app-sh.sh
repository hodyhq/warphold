#!/usr/bin/env bash
# Test scripts/install/app.sh against a fake release served over HTTP.
#
# Same shape as scripts/test-fleet-sh.sh: a real warphold binary built from
# this tree, packed the way goreleaser packs it, with a matching
# checksums.txt. The install is relocated with HOME / XDG_CONFIG_HOME (a
# user-scope install needs nothing else) plus WARPHOLD_INSTALL_ROOT, which
# here exists only so the test can prove nothing lands outside the home
# directory. With no container runtime on this machine there is no throwaway
# root filesystem; in a container the same assertions would additionally
# prove that a real user session enables the unit.
#
# The last section is the D11 boundary: app.sh installs one machine's app and
# has no server shaping in it at all.
#
# The two install paths are both exercised: a machine that is not enrolled
# gets the standalone app service ("app run"), and one that is enrolled gets
# the agent service ("agent run").
set -euo pipefail

cd "$(dirname "$0")/.."
REPO_DIR="$PWD"
APP_SH="$REPO_DIR/scripts/install/app.sh"

WORK="$(mktemp -d)"
SERVER_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
bad()  { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

# app.sh always runs in a temporary home, and never with a session bus: what
# it hands to "warphold agent install" must not reach the real user's systemd.
FAKEHOME="$WORK/home"
mkdir -p "$FAKEHOME/.config" "$WORK/root"
run_app() {
  env HOME="$FAKEHOME" XDG_CONFIG_HOME="$FAKEHOME/.config" \
      XDG_RUNTIME_DIR="$WORK/norun" DBUS_SESSION_BUS_ADDRESS="unix:path=$WORK/nobus" \
      WARPHOLD_INSTALL_ROOT="$WORK/root" WARPHOLD_RELEASE_BASE="$RELEASE_BASE" \
      PATH="/usr/bin:/bin" \
      sh "$APP_SH" "$@"
}

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

cp "$WORK/rel/download/v$VER/$NAME.tar.gz" "$WORK/rel/download/v$VER-bad/"
sed 's/^./0/' "$WORK/rel/download/v$VER/checksums.txt" > "$WORK/rel/download/v$VER-bad/checksums.txt"

PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$WORK/rel" >"$WORK/http.log" 2>&1 &
SERVER_PID=$!
RELEASE_BASE="http://127.0.0.1:$PORT"
for _ in $(seq 50); do
  curl -fsS "$RELEASE_BASE/download/v$VER/checksums.txt" >/dev/null 2>&1 && break
  sleep 0.1
done

# ---------------------------------------------------------- 1. --dry-run

echo "== --dry-run writes nothing"
run_app --version "v$VER" --dry-run --no-open > "$WORK/dry.out"
check "dry-run said what it would install" "grep -q 'would then' '$WORK/dry.out'"
check "dry-run named the app install"      "grep -q 'warphold app install' '$WORK/dry.out'"
check "dry-run wrote no binary"            "[ ! -e '$FAKEHOME/.local/bin/warphold' ]"
check "dry-run wrote nothing at all"       "[ -z \"\$(find '$FAKEHOME/.local' '$WORK/root' -mindepth 1 -print -quit 2>/dev/null)\" ]"

# ---------------------------------------------------------- 2. not enrolled

echo "== fresh install on a machine that is not enrolled installs the standalone app"
run_app --version "v$VER" --no-open > "$WORK/install.out" 2>&1
BIN="$FAKEHOME/.local/bin/warphold"
APP_UNIT="$FAKEHOME/.config/systemd/user/warphold-app.service"
APP_TRAY="$FAKEHOME/.config/autostart/warphold-app-tray.desktop"
AGENT_TRAY="$FAKEHOME/.config/autostart/warphold-tray.desktop"
check "binary installed in ~/.local/bin" "[ -x '$BIN' ]"
check "binary is 0755"                   "[ \"\$(stat -c %a '$BIN')\" = 755 ]"
check "PATH hint printed"                "grep -q 'is not in your PATH' '$WORK/install.out'"
check "app unit written"                 "[ -f '$APP_UNIT' ]"
check "unit runs the app engine"         "grep -qE 'ExecStart=\"[^\"]*warphold\" app run$' '$APP_UNIT'"
check "unit restarts on failure"         "grep -q 'Restart=on-failure' '$APP_UNIT'"
check "unit is a login-scope service"    "grep -q 'WantedBy=default.target' '$APP_UNIT'"
check "tray autostart written"           "[ -f '$APP_TRAY' ]"
check "tray watches the app engine"      "grep -q 'agent tray --scope app' '$APP_TRAY'"
check "no agent unit while unenrolled"   "[ ! -e '$FAKEHOME/.config/systemd/user/warphold-agent.service' ]"
check "told how to open the app"         "grep -q 'warphold app url' '$WORK/install.out'"
# The token in the URL is handed to a browser, never printed by the installer.
check "no session token printed"         "! grep -q 'local/session' '$WORK/install.out'"

# ---------------------------------------------------------- 3. enrolled

echo "== install on a machine that is enrolled writes the unit and the tray entry"
mkdir -p "$FAKEHOME/.config/warphold"
cat > "$FAKEHOME/.config/warphold/agent.json" <<'EOF'
{"server":"https://fleet.example.com","agent_id":"a1","bearer":"t","name":"test","poll_interval_seconds":60,"scope":"user","policy_etag":""}
EOF
run_app --version "v$VER" --no-open > "$WORK/enrolled.out" 2>&1
check "user unit written"      "[ -f '$FAKEHOME/.config/systemd/user/warphold-agent.service' ]"
check "unit runs the agent"    "grep -q 'agent run --scope user' '$FAKEHOME/.config/systemd/user/warphold-agent.service'"
check "tray autostart written" "[ -f '$AGENT_TRAY' ]"
check "tray entry runs tray"   "grep -q 'agent tray' '$AGENT_TRAY'"
check "agent tray is its own entry" "! grep -q -- '--scope app' '$AGENT_TRAY'"
check "the app tray survived"       "[ -f '$APP_TRAY' ]"

# ---------------------------------------------------------- 4. idempotent

echo "== re-run upgrades the binary and leaves state alone"
STATE_BEFORE="$(sha256sum "$FAKEHOME/.config/warphold/agent.json")"
run_app --version "v$VER" --no-open > "$WORK/reinstall.out" 2>&1
check "re-run succeeded"           "grep -q 'installed $BIN' '$WORK/reinstall.out'"
check "re-run kept the enrollment" "[ \"\$(sha256sum '$FAKEHOME/.config/warphold/agent.json')\" = '$STATE_BEFORE' ]"

# ---------------------------------------------------------- 5. bad checksum

echo "== a tampered checksum aborts before anything is installed"
rm -f "$BIN"
set +e
run_app --version "v$VER-bad" --no-open > "$WORK/bad.out" 2>&1
RC=$?
set -e
check "exited non-zero"        "[ $RC -ne 0 ]"
check "said checksum mismatch" "grep -qi 'checksum mismatch' '$WORK/bad.out'"
check "installed nothing"      "[ ! -e '$BIN' ]"

# ---------------------------------------------------------- 6. D11 boundary

echo "== app.sh has no server shaping in it (D11)"
check "no activation command" "! grep -q 'fleet activate' '$APP_SH'"
check "no system user"        "! grep -q 'useradd' '$APP_SH'"
check "no system unit dir"    "! grep -q '/etc/systemd/system' '$APP_SH'"
check "no setup token"        "! grep -q 'setup-token' '$APP_SH'"
check "no server env file"    "! grep -q 'KOPIA_SERVER' '$APP_SH'"
check "wrote nothing outside the home directory" "[ -z \"\$(find '$WORK/root' -mindepth 1 -print -quit)\" ]"

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
