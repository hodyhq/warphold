#!/usr/bin/env bash
#
# Standalone-restore proof (spec 12): a device's backups come back with a
# PINNED UPSTREAM `kopia` binary, driven only by what the printable recovery
# kit prints. WarpHold is not on PATH and not in the working directory while
# the restore runs, so anything the restore needed from WarpHold would fail
# here rather than years from now.
#
# bash, not POSIX sh: this is a CI script, and it uses arrays, ${var/from/to}
# and process substitution. It needs curl, openssl, jq, tar, sha256sum and diff.
#
# What it does, end to end:
#   build warphold -> fleet activate -> server start on loopback ->
#   set public_url -> hosted disk target + template + group + token ->
#   agent enroll -> snapshot a generated tree -> GET the kit HTML ->
#   scrape the password and the three commands out of the page ->
#   download+verify pinned upstream kopia -> run the scraped commands ->
#   diff -r the restore against the source -> prove the read key cannot write.
#
# The Fleet runs over TLS on 127.0.0.1 with a self-signed certificate, and
# it has to: the gateway refuses aws-chunked streaming payload signing
# (fleet/gateway/sigv4.go, ErrStreamingUnsupported), which is exactly what
# minio-go uses over plain HTTP, so a snapshot cannot be written to an
# http:// endpoint at all. The kit's `--disable-tls` shape is therefore not
# reachable, and the http fallback the plan allowed for is not an option.
#
# The certificate is trusted through SSL_CERT_FILE, not through a flag: the
# commands the kit prints are run VERBATIM, and the sandbox simply trusts the
# Fleet's CA the way any real machine trusts the Fleet's real certificate.
# Passing `--root-ca-pem-path` would have meant running a command the kit
# does not print, which is the one thing this test exists to rule out.

set -euo pipefail

# The upstream release this fork tracks. The fork's base is upstream master
# just after v0.23.1 (repository format version 3, unchanged since v0.11), so
# v0.23.1 is the newest published binary that can read what this tree writes.
# The digests are the linux lines of that release's checksums.txt:
#   https://github.com/kopia/kopia/releases/download/v0.23.1/checksums.txt
# They are pinned here rather than fetched, so a rewritten checksums.txt does
# not silently re-point the proof at a different binary.
KOPIA_VERSION="${KOPIA_VERSION:-0.23.1}"
KOPIA_SHA256_linux_x64="416d0f84a3dbb321a8b2d8f0997b1a0a6e915babe79ee76fa6e4d2bd1e1c5178"
KOPIA_SHA256_linux_arm64="a4ffbc019e0b0f932e2632054e73ec521dc1e80172a00095369c53ecf4e5a6cb"

CACHE_DIR="${KOPIA_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/warphold-standalone-restore}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

ADMIN_EMAIL="ci@warphold.invalid"
ADMIN_PASSWORD="standalone-restore-admin-pw"
SEAL_PASSPHRASE="standalone-restore-seal-passphrase"

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo; echo "== $*"; }

WORK="$(mktemp -d)"
SERVER_PID=""

cleanup() {
  local rc=$?
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  # CI asks for the kit and the logs when this fails; everything else in
  # $WORK is scratch and holds test-only credentials.
  if [[ -n "${STANDALONE_RESTORE_LOGDIR:-}" ]]; then
    mkdir -p "$STANDALONE_RESTORE_LOGDIR"
    cp -f "$WORK"/*.log "$WORK/kit.html" "$STANDALONE_RESTORE_LOGDIR/" 2>/dev/null || true
  fi
  rm -rf "$WORK"
  exit "$rc"
}
trap cleanup EXIT

for t in curl openssl jq tar sha256sum diff go; do
  command -v "$t" >/dev/null || fail "$t is required"
done

# ---------------------------------------------------------------- fixture ---
# A few MB of deterministic content: text that compresses, a "binary" file
# that does not, an empty file and a nested directory.
make_source_tree() {
  local root=$1
  mkdir -p "$root/docs" "$root/nested/deep"

  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    seq 1 20000 | sed "s/^/file-$i line /" >"$root/docs/file-$i.txt"
  done

  seq 1 5000 | sed 's/^/deep /' >"$root/nested/deep/notes.txt"
  # head first, tr second: the other order kills tr with SIGPIPE, which
  # `set -o pipefail` turns into a spurious failure.
  head -c 1048576 /dev/zero | tr '\0' '\376' >"$root/nested/blob.bin"
  : >"$root/empty"
}

# ------------------------------------------------------------ pinned kopia ---
# Downloads (once) and verifies the pinned upstream release, and echoes the
# directory holding it. That directory holds `kopia` and nothing else: it is
# the whole PATH the restore runs with.
fetch_upstream_kopia() {
  local os arch plat sha dir tgz
  os="$(uname -s)"; arch="$(uname -m)"

  case "$os/$arch" in
    Linux/x86_64) plat="linux-x64";   sha="$KOPIA_SHA256_linux_x64" ;;
    Linux/aarch64) plat="linux-arm64"; sha="$KOPIA_SHA256_linux_arm64" ;;
    *) fail "no pinned upstream kopia digest for $os/$arch (add one from the release's checksums.txt)" ;;
  esac

  dir="$CACHE_DIR/kopia-$KOPIA_VERSION-$plat"
  if [[ -x "$dir/kopia" ]]; then
    echo "$dir"
    return
  fi

  # A $dir.tmp left by an interrupted run must not be promoted into the cache
  # by the mv below: a truncated extract would then be trusted as the pinned,
  # verified binary on every later run.
  rm -rf "$dir" "$dir.tmp"
  mkdir -p "$dir.tmp"
  tgz="$dir.tmp/kopia.tar.gz"
  curl -fsSL --retry 3 -o "$tgz" \
    "https://github.com/kopia/kopia/releases/download/v$KOPIA_VERSION/kopia-$KOPIA_VERSION-$plat.tar.gz" \
    || fail "cannot download upstream kopia $KOPIA_VERSION"

  echo "$sha  $tgz" | sha256sum -c - >/dev/null \
    || fail "upstream kopia $KOPIA_VERSION did not match its pinned sha256"

  tar -xzf "$tgz" -C "$dir.tmp" --strip-components=1
  rm -f "$tgz"
  # Only the binary: this directory becomes PATH, so nothing else may live here.
  find "$dir.tmp" -mindepth 1 ! -name kopia -delete
  mv "$dir.tmp" "$dir"

  echo "$dir"
}

# --------------------------------------------------------------- warphold ---
step "building warphold from this tree"
mkdir -p "$WORK/bin"
( cd "$REPO_ROOT" && go build -o "$WORK/bin/warphold" . )
WARPHOLD="$WORK/bin/warphold"

CONFIG="$WORK/kopia.config"
HOSTED_ROOT="$WORK/hosted"
AGENT_STATE="$WORK/agent-state"
SOURCE="$WORK/source"
RESTORED="$WORK/restored"
SANDBOX="$WORK/sandbox"
mkdir -p "$HOSTED_ROOT" "$AGENT_STATE" "$SOURCE" "$SANDBOX/tmp"

step "activating fleet"
"$WARPHOLD" --config-file="$CONFIG" fleet activate \
  --email "$ADMIN_EMAIL" \
  --admin-password "$ADMIN_PASSWORD" \
  --passphrase "$SEAL_PASSPHRASE" >"$WORK/activate.log" 2>&1 \
  || { cat "$WORK/activate.log"; fail "fleet activate"; }

step "minting a self-signed certificate for 127.0.0.1"
# Self-signed, so the certificate is its own CA and one PEM serves as both
# the server's chain and the trust anchor every client is pointed at.
openssl req -x509 -newkey rsa:2048 -sha256 -days 1 -nodes \
  -keyout "$WORK/tls.key" -out "$WORK/tls.crt" \
  -subj "/CN=127.0.0.1" -addext "subjectAltName=IP:127.0.0.1" \
  >"$WORK/openssl.log" 2>&1 || { cat "$WORK/openssl.log"; fail "openssl req"; }
CA="$WORK/tls.crt"

step "starting the fleet server on loopback over TLS"
# SSL_CERT_FILE: the server fetches its own public_url to verify it, so it
# has to trust the certificate it is serving.
SSL_CERT_FILE="$CA" "$WARPHOLD" --config-file="$CONFIG" server start \
  --insecure --without-password --no-ui --no-grpc \
  --address=127.0.0.1:0 \
  --tls-cert-file="$WORK/tls.crt" --tls-key-file="$WORK/tls.key" \
  --server-control-password=standalone-restore-control \
  >"$WORK/server.log" 2>&1 &
SERVER_PID=$!

BASE=""
for _ in $(seq 1 100); do
  BASE="$(sed -n 's/^SERVER ADDRESS: //p' "$WORK/server.log" | head -n1)"
  [[ -n "$BASE" ]] && break
  kill -0 "$SERVER_PID" 2>/dev/null || { cat "$WORK/server.log"; fail "server exited during startup"; }
  sleep 0.2
done
[[ -n "$BASE" ]] || { cat "$WORK/server.log"; fail "server never reported its address"; }
echo "fleet at $BASE"

# ------------------------------------------------------------- admin calls ---
JAR="$WORK/cookies"

# api METHOD PATH [json-body] -> body on stdout, non-2xx is fatal.
api() {
  local method=$1 path=$2 body=${3:-} csrf out code
  local args=(-sS --cacert "$CA" -o "$WORK/api.out" -w '%{http_code}' -b "$JAR" -c "$JAR"
              -X "$method" "$BASE$path")
  # The double-submit token: read the wh_csrf cookie out of the jar the way
  # the admin UI reads it out of the browser. Absent before the first login.
  csrf="$( [[ -f "$JAR" ]] && awk '$6 == "wh_csrf" { print $7 }' "$JAR" | tail -n1 || true )"
  [[ -n "$csrf" ]] && args+=(-H "X-WarpHold-CSRF: $csrf")
  [[ -n "$body" ]] && args+=(-H 'Content-Type: application/json' -d "$body")

  code="$(curl "${args[@]}")"
  out="$(cat "$WORK/api.out")"
  [[ "$code" == 2* ]] || fail "$method $path -> HTTP $code: $out"
  printf '%s' "$out"
}

step "signing in and configuring the fleet"
api POST /api/v1/fleet/session \
  "$(jq -nc --arg e "$ADMIN_EMAIL" --arg p "$ADMIN_PASSWORD" '{email:$e,password:$p}')" >/dev/null

# public_url must be set before a token can be issued, and it is what the
# kit's --endpoint is built from. verify:true makes the server prove the URL
# reaches this very Fleet before storing it.
api PUT /api/v1/fleet/settings \
  "$(jq -nc --arg u "$BASE" '{public_url:$u,verify:true}')" >/dev/null

TARGET_ID="$(api POST /api/v1/fleet/targets \
  "$(jq -nc --arg p "$HOSTED_ROOT" \
     '{name:"ci-hosted",kind:"hosted",storage_mode:"disk",path:$p}')" | jq -r .id)"

TEMPLATE_ID="$(api POST /api/v1/fleet/templates \
  "$(jq -nc --arg s "$SOURCE" '{name:"ci-template",sources:[$s],policy:{}}')" | jq -r .id)"

GROUP_ID="$(api POST /api/v1/fleet/groups \
  "$(jq -nc --argjson t "$TARGET_ID" --argjson p "$TEMPLATE_ID" \
     '{name:"ci-group",target_id:$t,template_id:$p}')" | jq -r .id)"

ENROLL_TOKEN="$(api POST /api/v1/fleet/tokens \
  "$(jq -nc --argjson g "$GROUP_ID" '{group_id:$g,ttl_seconds:3600}')" | jq -r .token)"

step "enrolling a device"
SSL_CERT_FILE="$CA" WARPHOLD_STATE_DIR="$AGENT_STATE" WARPHOLD_ENROLL_TOKEN="$ENROLL_TOKEN" \
  "$WARPHOLD" agent enroll --server "$BASE" --scope user --name ci-device \
  >"$WORK/enroll.log" 2>&1 || { cat "$WORK/enroll.log"; fail "agent enroll"; }

AGENT_ID="$(jq -r .agent_id "$AGENT_STATE/agent.json")"
[[ -n "$AGENT_ID" && "$AGENT_ID" != null ]] || fail "no agent id after enroll"
echo "enrolled as $AGENT_ID"

step "snapshotting a generated tree through the gateway"
make_source_tree "$SOURCE"
SSL_CERT_FILE="$CA" WARPHOLD_STATE_DIR="$AGENT_STATE" \
  "$WARPHOLD" --config-file="$AGENT_STATE/repository.config" snapshot create "$SOURCE" \
  >"$WORK/snapshot.log" 2>&1 || { cat "$WORK/snapshot.log"; fail "snapshot create"; }

# ------------------------------------------------------------------- kit ----
step "fetching the printable recovery kit and reading it the way a human does"
api GET "/api/v1/fleet/agents/$AGENT_ID/kit" >"$WORK/kit.html"

# html/template escapes what it prints, so undo the five entities it emits
# before treating a line as a shell command.
unescape() { sed -e 's/&#34;/"/g' -e "s/&#39;/'/g" -e 's/&lt;/</g' -e 's/&gt;/>/g' -e 's/&amp;/\&/g'; }

REPO_PASSWORD="$(sed -n 's|.*<p class="secret">\(.*\)</p>.*|\1|p' "$WORK/kit.html" | unescape)"
[[ -n "$REPO_PASSWORD" ]] || fail "no repository password on the kit page"

mapfile -t KIT_COMMANDS < <(sed -n 's|^<pre>\(.*\)</pre>$|\1|p' "$WORK/kit.html" | unescape)
[[ ${#KIT_COMMANDS[@]} -eq 3 ]] || fail "expected 3 commands on the kit page, got ${#KIT_COMMANDS[@]}"

CONNECT_CMD="${KIT_COMMANDS[0]}"
LIST_CMD="${KIT_COMMANDS[1]}"
RESTORE_CMD="${KIT_COMMANDS[2]}"

# Every command on the page must be an upstream `kopia` invocation: a line
# that started with anything else would already be a WarpHold dependency.
for c in "$CONNECT_CMD" "$LIST_CMD" "$RESTORE_CMD"; do
  [[ "$c" == "kopia "* ]] || fail "kit printed a non-kopia command: $c"
done
[[ "$CONNECT_CMD" == "kopia repository connect s3 "* ]] || fail "unexpected connect command: $CONNECT_CMD"
echo "kit connect: $(sed 's/--secret-access-key [^ ]*/--secret-access-key <redacted>/' <<<"$CONNECT_CMD")"

# ------------------------------------------------- restore, upstream only ----
step "downloading the pinned upstream kopia"
KOPIA_DIR="$(fetch_upstream_kopia)"
[[ "$(ls "$KOPIA_DIR")" == "kopia" ]] || fail "$KOPIA_DIR must hold nothing but the kopia binary"

# Every upstream command runs here: cwd is a directory with no warphold
# binary, PATH is the single directory holding the pinned kopia, and the
# environment is otherwise empty.
kit_run() {
  ( cd "$SANDBOX" && env -i \
      HOME="$SANDBOX" \
      PATH="$KOPIA_DIR" \
      TMPDIR="$SANDBOX/tmp" \
      SSL_CERT_FILE="$CA" \
      KOPIA_PASSWORD="$REPO_PASSWORD" \
      KOPIA_CHECK_FOR_UPDATES=false \
      /bin/sh -c "$1" )
}

kit_run 'command -v warphold' >/dev/null 2>&1 \
  && fail "warphold is reachable from the restore sandbox; the proof is void"
kit_run 'command -v kopia' >/dev/null || fail "the pinned kopia is not on the sandbox PATH"

echo "upstream binary: $(kit_run 'kopia --version')"

step "running the kit's commands"
kit_run "$CONNECT_CMD" >"$WORK/connect.log" 2>&1 || { cat "$WORK/connect.log"; fail "kit connect command"; }
kit_run "$LIST_CMD" >"$WORK/list.log" 2>&1 || { cat "$WORK/list.log"; fail "kit list command"; }
cat "$WORK/list.log"

SNAPSHOT_ID="$(grep -oE '\b[A-Za-z]?[0-9a-f]{32}\b' "$WORK/list.log" | head -n1)"
[[ -n "$SNAPSHOT_ID" ]] || fail "no snapshot id in the output of '$LIST_CMD'"

# The page prints placeholders and tells the reader to replace them; this is
# that substitution, nothing more.
RESTORE_RUN="${RESTORE_CMD/<snapshot-id>/$SNAPSHOT_ID}"
RESTORE_RUN="${RESTORE_RUN//\/path\/to\/restore\/into/$RESTORED}"
echo "restore: $RESTORE_RUN"
kit_run "$RESTORE_RUN" >"$WORK/restore.log" 2>&1 || { cat "$WORK/restore.log"; fail "kit restore command"; }

step "comparing the restored tree with the source"
diff -r "$SOURCE" "$RESTORED" >"$WORK/diff.log" 2>&1 \
  || { cat "$WORK/diff.log"; fail "restored tree differs from the source"; }
echo "diff -r is empty over $(find "$SOURCE" -type f | wc -l) files"

step "proving the kit's key is read-only"
mkdir -p "$SANDBOX/write-probe"
echo nope >"$SANDBOX/write-probe/nope.txt"
if kit_run "kopia snapshot create $SANDBOX/write-probe" >"$WORK/write-probe.log" 2>&1; then
  cat "$WORK/write-probe.log"
  fail "the recovery kit's read-only key was able to write a snapshot"
fi
# It has to fail *because the key is read-only* -- any other failure would
# pass this check while proving nothing about the credential.
grep -q 'this key is read-only' "$WORK/write-probe.log" \
  || { cat "$WORK/write-probe.log"; fail "snapshot create failed, but not because the key is read-only"; }
echo "snapshot create with the kit's key was refused:"
grep 'this key is read-only' "$WORK/write-probe.log" | tail -n1

echo
echo "PASS: a hosted-target snapshot restored with upstream kopia $KOPIA_VERSION using only the recovery kit."
