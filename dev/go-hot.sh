#!/usr/bin/env bash
# Hot reload for the orchestrator (./dev.sh start). Runs as PID 1 in the
# container golang:1.26-bookworm with the source under /src (read-only).
# Rebuilds on every change to Go files, go.mod/go.sum or schema.sql and
# restarts the orchestrator. If the build fails, the old version keeps running.
set -u
cd /src
BIN=/tmp/orchestrator
pid=

log() { printf '[hot] %s\n' "$*"; }

changed() {
  find cmd internal web/embed.go go.mod go.sum \
    \( -name '*.go' ! -name '*_test.go' -o -name '*.sql' -o -name 'go.mod' -o -name 'go.sum' \) \
    -newer /tmp/stamp -print -quit 2>/dev/null
}

build() {
  local t0=$SECONDS
  if go build -buildvcs=false -o "$BIN.neu" ./cmd/orchestrator; then
    mv "$BIN.neu" "$BIN"
    log "built in $((SECONDS - t0)) s"
    return 0
  fi
  log "build failed, waiting for the next change"
  return 1
}

# The slot images (agw-basis, agw-pi) are built by ./dev.sh start, not here. Go code that expects a
# new file in them (an extension, a skill) makes every slot start fail against the old image (issue
# #55: -e /opt/agw/ext/web-tools.ts on a two-day-old agw-pi, 1 110 failed starts in five hours).
images_changed() {
  find images third_party -name node_modules -prune -o -type f -newer /tmp/img-stamp -print -quit 2>/dev/null
}

warn_images() {
  local f
  f=$(images_changed)
  [[ -n "$f" ]] || return 0
  touch /tmp/img-stamp
  log "WARNING: $f changed; hot reload does not rebuild the slot images. Run ./dev.sh start to rebuild them, otherwise new slots may fail to start (see \"pool slot discarded before first use\")."
}

start() { "$BIN" & pid=$!; log "orchestrator running (PID $pid)"; }

stop() {
  [[ -n "$pid" ]] || return 0
  kill -TERM "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
  pid=
}

trap 'stop; exit 0' TERM INT

touch /tmp/stamp /tmp/img-stamp
until build; do
  while [[ -z "$(changed)" ]]; do sleep 1; done
  touch /tmp/stamp
done
start

while true; do
  sleep 1 & wait $!
  warn_images
  [[ -n "$(changed)" ]] || continue
  sleep 0.3 # coalesce several saved files
  touch /tmp/stamp
  log "change detected, rebuilding"
  if build; then
    log "restarting orchestrator (running chats go idle and can be resumed)"
    stop
    start
  fi
done
