#!/usr/bin/env bash
# Run against a supplied host binary; DEVSY_AGENT_URL may serve its Linux build.
set -euo pipefail
host=${RELAY_HOST_BINARY:-${1:-}}
devsy=$(cd "$(dirname "$host")" && pwd)/$(basename "$host")
root=${RELAY_ROOT:-$(mktemp -d)}
id="relay-$(basename "$root" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9')"
export HOME="$root/home" XDG_CONFIG_HOME="$root/home/config" XDG_STATE_HOME="$root/home/state"
export XDG_DATA_HOME="$root/home/data" XDG_CACHE_HOME="$root/home/cache" XDG_RUNTIME_DIR="$root/home/run" DEVSY_HOME="$root/devsy"
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL="$root/home/gitconfig" GIT_TERMINAL_PROMPT=0
unset GIT_CONFIG_SYSTEM GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS DEVSY_GIT_HELPER_PORT SSH_AUTH_SOCK DEVSY_CONTEXT DEVSY_PROVIDER
owner= second= third= foreign=
if [[ ${1:-} = --assert-clean ]]; then
  [[ ! -d "$root" ]]
  ! docker inspect "$id" >/dev/null 2>&1
  for pid in ${RELAY_SSH_PIDS:-}; do ! kill -0 "$pid" 2>/dev/null; done
  exit
fi
sshbase=(-o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)
cleanup() {
  if [[ -n ${RELAY_ARTIFACT_DIR:-} ]]; then
    mkdir -p "$RELAY_ARTIFACT_DIR"
    cp "$root/"*.log "$RELAY_ARTIFACT_DIR/" 2>/dev/null || true
  fi
  for socket in "$root/a.socket" "$root/b.socket" "$root/c.socket" "$root/foreign.socket"; do
    ssh "${sshbase[@]}" -S "$socket" -O exit root@devsy-workspace >/dev/null 2>&1 || true
  done
  for pid in "$owner" "$second" "$third" "$foreign"; do
    if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  done
  "$devsy" --home "$root/devsy" workspace delete "$id" --force --ignore-not-found > /dev/null 2>&1 || true
  # This exact name belongs only to this invocation.
  docker rm -f "$id" >/dev/null 2>&1 || true
  rm -rf "$root"
  if docker inspect "$id" >/dev/null 2>&1; then
    echo 'Test-owned container remains after cleanup' >&2; return 1
  fi
}
trap cleanup EXIT
if [[ ${1:-} = --cleanup ]]; then exit; fi
mkdir -p "$root/home" "$root/workspace/.devcontainer"
cd "$root/home"
cat > "$root/helper" <<'HELPER'
#!/bin/sh
cat >/dev/null
if test "$1" = get; then printf 'username=relay-test\npassword=relay-test-dummy\n'; fi
HELPER
chmod 700 "$root/helper"
git config --global credential.helper "$root/helper"
git config --global user.name 'Relay Test'
git config --global user.email 'relay@example.invalid'
cat > "$root/workspace/.devcontainer/devcontainer.json" <<CONFIG
{"image":"mcr.microsoft.com/devcontainers/base:ubuntu","remoteUser":"root","runArgs":["--name","$id"]}
CONFIG
run() { "$devsy" --home "$root/devsy" "$@"; }
guest() { docker exec "$id" sh -c "$1"; }
eventually() {
  for ((i=0; i<100; i++)); do if "$@"; then return; fi; sleep .2; done
  echo "Timed out at credential relay assertion: $1" >&2; return 1
}
ready() { guest 'curl --noproxy "*" -fsS --max-time 2 http://localhost:12049/ >/dev/null' 2>/dev/null; }
fill() {
  guest 'printf "protocol=https\nhost=relay.example.invalid\n\n" | GIT_TERMINAL_PROMPT=0 git credential fill 2>/dev/null' |
    grep -qx 'password=relay-test-dummy'
}
run context set -o TELEMETRY=false >/dev/null
run provider add docker >/dev/null
run provider use docker >/dev/null
run workspace up "$root/workspace" --id "$id" --ide-launch skip > "$root/up.log" 2>&1
if [[ -n ${RELAY_AGENT_BINARY:-} ]]; then
  if command -v sha256sum >/dev/null; then expected=$(sha256sum "$RELAY_AGENT_BINARY" | cut -d ' ' -f1); else expected=$(shasum -a 256 "$RELAY_AGENT_BINARY" | cut -d ' ' -f1); fi
  actual=$(guest 'sha256sum /usr/local/bin/devsy' | cut -d ' ' -f1)
  [[ "$actual" = "$expected" ]]
  echo 'PASS environment agent matches supplied build'
fi
proxy="'$devsy' --home '$root/devsy' --log-level debug workspace ssh '$id' --stdio --user root"
ssh "${sshbase[@]}" -o "ProxyCommand=$proxy" -M -S "$root/a.socket" -N root@devsy-workspace > "$root/a.log" 2>&1 & owner=$!
echo "$owner" >> "$root/ssh-pids"
eventually ready
eventually fill
echo 'PASS A owns a working credential relay'
touch "$root/owner-ready"
ssh "${sshbase[@]}" -o "ProxyCommand=$proxy" -M -S "$root/b.socket" -N root@devsy-workspace > "$root/b.log" 2>&1 & second=$!
eventually ssh "${sshbase[@]}" -o ProxyCommand=false -S "$root/b.socket" root@devsy-workspace true
eventually fill
# B has completed its services attempt before A ends, not a race on startup.
eventually grep -q 'another session already provides' "$root/b.log"
ssh "${sshbase[@]}" -S "$root/a.socket" -O exit root@devsy-workspace
wait "$owner" 2>/dev/null || true; owner=
kill -0 "$second"
if [[ ${RELAY_EXPECT_LOSS:-0} = 1 ]]; then
  eventually bash -c '! docker exec "$1" curl --noproxy "*" -fsS --max-time 2 http://localhost:12049/ >/dev/null 2>&1' sh "$id"
  if fill; then echo 'Unexpected credentials after owner exit' >&2; exit 1; fi
  echo 'PASS reproduced loss with B still connected'
else
  eventually fill
  kill -0 "$second"
  echo 'PASS B reacquired credential relay after A exited'
  ssh "${sshbase[@]}" -o "ProxyCommand=$proxy" -M -S "$root/c.socket" -N root@devsy-workspace > "$root/c.log" 2>&1 & third=$!
  eventually grep -q 'another session already provides' "$root/c.log"
  ssh "${sshbase[@]}" -S "$root/b.socket" -O exit root@devsy-workspace
  wait "$second" 2>/dev/null || true; second=
  eventually fill
  echo 'PASS repeated same-user competition reacquires relay'
  other_proxy="'$devsy' --home '$root/devsy' --log-level debug workspace ssh '$id' --stdio --user vscode"
  ssh "${sshbase[@]}" -o "ProxyCommand=$other_proxy" -M -S "$root/foreign.socket" -N vscode@devsy-workspace > "$root/foreign.log" 2>&1 & foreign=$!
  eventually grep -q 'already owned by user root' "$root/foreign.log"
  [[ $(guest 'curl --noproxy "*" -fsS http://localhost:12049/owner') = root ]]
  fill
  ssh "${sshbase[@]}" -S "$root/foreign.socket" -O exit vscode@devsy-workspace
  wait "$foreign" 2>/dev/null || true; foreign=
  [[ $(guest 'curl --noproxy "*" -fsS http://localhost:12049/owner') = root ]]
  echo 'PASS foreign-user contender cannot steal relay and cancels cleanly'
  ssh "${sshbase[@]}" -S "$root/c.socket" -O exit root@devsy-workspace
  wait "$third" 2>/dev/null || true; third=
  eventually bash -c '! docker exec "$1" curl --noproxy "*" -fsS --max-time 2 http://localhost:12049/ >/dev/null 2>&1' sh "$id"
  echo 'PASS canceled sessions leave no credential listener'
fi
