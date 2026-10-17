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
pids=()
names=(a b c d foreign)
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
  for name in "${names[@]}"; do
    socket="$root/$name.socket"
    ssh "${sshbase[@]}" -S "$socket" -O exit root@devsy-workspace >/dev/null 2>&1 || true
  done
  for pid in "${pids[@]}"; do
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
# Linux setup chowns the bind mount to root. Keep these fixture directories
# removable by the runner; their enclosing mktemp directory remains private.
chmod 700 "$root"
chmod a+rwx "$root/workspace" "$root/workspace/.devcontainer"
cd "$root/home"
cat > "$root/helper" <<'HELPER'
#!/bin/sh
cat >/dev/null
if test "$1" = get; then printf 'username=relay-test\npassword=relay-test-%s-dummy\n' "${RELAY_TEST_SESSION:-a}"; fi
HELPER
chmod 700 "$root/helper"
git config --global credential.helper "$root/helper"
git config --global user.name 'Relay Test'
git config --global user.email 'relay@example.invalid'
cat > "$root/workspace/.devcontainer/devcontainer.json" <<CONFIG
{"image":"mcr.microsoft.com/devcontainers/base:ubuntu","remoteUser":"root","runArgs":["--platform","linux/amd64","--name","$id"]}
CONFIG
run() { "$devsy" --home "$root/devsy" "$@"; }
guest() { docker exec "$id" sh -c "$1"; }
eventually() {
  for ((i=0; i<100; i++)); do if "$@"; then return; fi; sleep .2; done
  echo "Timed out at credential relay assertion: $1" >&2; return 1
}
ready() { guest 'curl --noproxy "*" -fsS --max-time 2 http://localhost:12049/ >/dev/null' 2>/dev/null; }
fill() {
  docker exec -u "${1:-root}" "$id" sh -c 'printf "protocol=https\nhost=relay.example.invalid\n\n" | GIT_TERMINAL_PROMPT=0 git credential fill 2>/dev/null' |
    grep -qx "password=relay-test-${2:-a}-dummy"
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
start_session() {
  local index=$1 user=$2 name=${names[$1]}
  local proxy="env RELAY_TEST_SESSION=$name '$devsy' --home '$root/devsy' --log-level debug workspace ssh '$id' --stdio --user $user"
  ssh "${sshbase[@]}" -o "ProxyCommand=$proxy" -M -S "$root/$name.socket" -N "$user@devsy-workspace" > "$root/$name.log" 2>&1 &
  pids[$index]=$!
  echo "$!" >> "$root/ssh-pids"
  eventually ssh "${sshbase[@]}" -o ProxyCommand=false -S "$root/$name.socket" "$user@devsy-workspace" true
}
stop_session() {
  local index=$1 user=$2 name=${names[$1]}
  ssh "${sshbase[@]}" -S "$root/$name.socket" -O exit "$user@devsy-workspace"
  wait "${pids[$index]}" 2>/dev/null || true
  pids[$index]=''
}
processes() {
  guest 'ps -ww -eo pid,args' > "$root/processes.log"
  awk 'index($0, "/devsy internal agent container credentials-server ") {print $1}' "$root/processes.log" | sort -n
}
assert_single_start() {
  local name=${names[$1]} count
  count=$(grep -c 'execute SSH server command:.*credentials-server' "$root/$name.log")
  if [[ $count != 1 ]]; then echo "FAIL $name recreated its services command $count times" >&2; return 1; fi
}
winner() {
  for index in 1 2 3; do
    if [[ -n ${pids[$index]} ]] && grep -q 'credentials server started' "$root/${names[$index]}.log"; then
      echo "$index"; return
    fi
  done
  return 1
}
start_session 0 root
eventually ready
eventually fill
echo 'PASS A owns a working credential relay'
for index in 1 2 3; do
  start_session "$index" root
  eventually grep -q 'another session already provides' "$root/${names[$index]}.log"
done
before=$(processes)
# This exceeds multiple retry intervals; competing connections must persist.
sleep 3
for index in 1 2 3; do assert_single_start "$index"; done
[[ $(printf '%s\n' "$before" | wc -l | tr -d ' ') = 4 ]]
[[ $(processes) = "$before" ]]
for index in 1 2 3; do
  ssh "${sshbase[@]}" -o ProxyCommand=false -S "$root/${names[$index]}.socket" root@devsy-workspace true
  eventually grep -q 'received ping from agent' "$root/${names[$index]}.log"
done
echo 'PASS B/C/D keep stable remote processes, initialized RPC, and responsive SSH while waiting'
stop_session 1 root
only_processes() { [[ $(processes | wc -l | tr -d ' ') = "$1" ]]; }
eventually only_processes 3
fill
start_session 1 root
eventually grep -q 'another session already provides' "$root/b.log"
eventually only_processes 4
echo 'PASS disconnected idle waiter exits without disturbing the owner'
touch "$root/waiters-ready"
if [[ ${RELAY_WAIT_FOR_CANCEL:-0} = 1 ]]; then
  while :; do sleep 1; done
fi
stop_session 0 root
eventually winner >/dev/null
eventually fill root "${names[$(winner)]}"
for round in 1 2; do
  index=$(winner)
  stop_session "$index" root
  eventually winner >/dev/null
  eventually fill root "${names[$(winner)]}"
done
echo 'PASS repeated ownership handoff among B/C/D restores actual Git credentials'
index=$(winner)
start_session 4 vscode
eventually grep -q 'already owned by user root' "$root/foreign.log"
[[ $(guest 'curl --noproxy "*" -fsS http://localhost:12049/owner') = root ]]
fill root "${names[$index]}"
sleep 3
assert_single_start 4
stop_session "$index" root
eventually fill vscode foreign
[[ $(guest 'curl --noproxy "*" -fsS http://localhost:12049/owner') = vscode ]]
echo 'PASS foreign user waits without takeover, then acquires after release'
stop_session 4 vscode
eventually bash -c '! docker exec "$1" curl --noproxy "*" -fsS --max-time 2 http://localhost:12049/ >/dev/null 2>&1' sh "$id"
echo 'PASS canceled sessions leave no credential listener'
