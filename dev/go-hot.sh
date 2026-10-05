#!/usr/bin/env bash
# Hot Reload für den Orchestrator (./dev.sh start). Läuft als PID 1 im
# Container golang:1.26-bookworm mit dem Quelltext unter /src (schreibgeschützt).
# Baut bei jeder Änderung an Go-Dateien, go.mod/go.sum oder schema.sql neu und
# startet den Orchestrator neu. Scheitert der Bau, läuft die alte Fassung weiter.
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
    log "gebaut in $((SECONDS - t0)) s"
    return 0
  fi
  log "Bau fehlgeschlagen, warte auf die nächste Änderung"
  return 1
}

start() { "$BIN" & pid=$!; log "Orchestrator läuft (PID $pid)"; }

stop() {
  [[ -n "$pid" ]] || return 0
  kill -TERM "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
  pid=
}

trap 'stop; exit 0' TERM INT

touch /tmp/stamp
until build; do
  while [[ -z "$(changed)" ]]; do sleep 1; done
  touch /tmp/stamp
done
start

while true; do
  sleep 1 & wait $!
  [[ -n "$(changed)" ]] || continue
  sleep 0.3 # mehrere gespeicherte Dateien zusammenfassen
  touch /tmp/stamp
  log "Änderung erkannt, baue neu"
  if build; then
    log "starte Orchestrator neu (laufende Chats ruhen und lassen sich fortsetzen)"
    stop
    start
  fi
done
