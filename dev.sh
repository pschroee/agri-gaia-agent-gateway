#!/usr/bin/env bash
# Entwicklungsskript für den PoC (Stufe 1). Aufruf aus beliebigem Verzeichnis.
#
#   ./dev.sh init     .env anlegen (falls fehlt), Abbilder bauen, Web-UI-Abhängigkeiten holen
#   ./dev.sh start    Abbild agw-basis bauen und Orchestrator, Postgres, RustFS und die Paket-
#                     Zwischenspeicher (npm-cache, pip-cache) starten, mit
#                     Hot Reload: Web-UI über Vite auf :18484, Orchestrator baut sich bei
#                     Go-Änderungen im Container neu. ./dev.sh start --prod baut das feste Abbild
#   ./dev.sh stop     alles anhalten und Sandboxen abbauen (Daten bleiben erhalten)
#   ./dev.sh status   Dienste, Sandboxen und Pool anzeigen
#   ./dev.sh logs     Protokoll des Orchestrators verfolgen
#   ./dev.sh test     schnelle Tests (Go mit Postgres, ohne Docker; Web), rund 30 s
#   ./dev.sh test --full   zusätzlich Docker-Integration, S3 und Platztests, rund 5 min (vor dem Push)
#   ./dev.sh e2e      Ende-zu-Ende-Tests mit echtem Modell (kostet Cent-Beträge); startet den
#                     Orchestrator dafür mit niedriger Kompaktierungsschwelle und danach wieder normal
#   ./dev.sh cli ...  CLI agw gegen den laufenden Orchestrator (z. B. ./dev.sh cli pool)
#   ./dev.sh reset    ALLES löschen: Container, Sandboxen, Volumes (Chats, Artefakte), Netze, Abbilder
set -euo pipefail

cd "$(dirname "$0")"
PROJECT=agwpoc
IMAGE=${AGW_IMAGE:-agwpoc/agw-basis:dev}
PI_IMAGE=${AGW_PI_IMAGE:-agwpoc/agw-pi:dev}
LABEL=agwpoc.managed=true
DEVDIR=.dev
VITE_PORT=${AGW_VITE_PORT:-18484}
HTTP_PORT=${AGW_HTTP_PORT:-18480}

info() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }

need() { command -v "$1" >/dev/null || { warn "$1 fehlt"; exit 1; }; }

# Betriebsart des letzten Starts: hot (Standard) oder prod. Alle Compose-Aufrufe
# nutzen dieselben Dateien, sonst ersetzte etwa ./dev.sh e2e den Hot-Reload-Container.
mode() { cat "$DEVDIR/mode" 2>/dev/null || echo prod; }
dc() {
  if [[ "$(mode)" == hot ]]; then docker compose -f compose.yaml -f compose.hot.yaml "$@"
  else docker compose "$@"; fi
}
build_flag() { [[ "$(mode)" == hot ]] || echo --build; }

ensure_dist() {
  # web/embed.go bettet web/dist ein; ohne gebaute UI lässt sich der Orchestrator nicht bauen.
  [[ -f web/dist/index.html ]] && return
  [[ -d web/node_modules ]] || (cd web && npm install --no-audit --no-fund >/dev/null)
  info "baue Web-UI einmalig (für die eingebettete Fassung)"
  (cd web && npm run build >/dev/null)
}

wait_api() {
  local i
  for i in $(seq 1 240); do
    curl -fsS -o /dev/null "http://127.0.0.1:$HTTP_PORT/" 2>/dev/null && return 0
    sleep 1
  done
  warn "Orchestrator antwortet nicht, siehe ./dev.sh logs"; return 1
}

vite_running() { [[ -f "$DEVDIR/vite.pid" ]] && kill -0 "$(cat "$DEVDIR/vite.pid")" 2>/dev/null; }

start_vite() {
  vite_running && { info "Vite läuft bereits"; return; }
  [[ -d web/node_modules ]] || (cd web && npm install --no-audit --no-fund >/dev/null)
  info "starte Vite (Hot Reload der Web-UI) auf 127.0.0.1:$VITE_PORT"
  # set -m: eigene Prozessgruppe, damit stop auch die Kindprozesse von Vite beendet
  set -m
  (cd web && exec ./node_modules/.bin/vite --host 127.0.0.1 --port "$VITE_PORT" --strictPort) \
    >"$DEVDIR/vite.log" 2>&1 </dev/null &
  echo $! >"$DEVDIR/vite.pid"
  set +m
  local i
  for i in $(seq 1 30); do
    curl -fsS -o /dev/null "http://127.0.0.1:$VITE_PORT/" 2>/dev/null && return 0
    sleep 0.5
  done
  warn "Vite startet nicht, siehe poc/$DEVDIR/vite.log"
}

stop_vite() {
  vite_running || { rm -f "$DEVDIR/vite.pid"; return 0; }
  info "halte Vite an"
  kill -TERM -- "-$(cat "$DEVDIR/vite.pid")" 2>/dev/null || kill -TERM "$(cat "$DEVDIR/vite.pid")" 2>/dev/null || true
  rm -f "$DEVDIR/vite.pid"
}

ensure_env() {
  if [[ ! -f .env ]]; then
    info ".env fehlt, lege sie aus .env.example mit Zufallswerten an"
    cp .env.example .env
    sed -i.bak \
      -e "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 16)/" \
      -e "s/^RUSTFS_ACCESS_KEY=.*/RUSTFS_ACCESS_KEY=agw$(openssl rand -hex 8)/" \
      -e "s/^RUSTFS_SECRET_KEY=.*/RUSTFS_SECRET_KEY=$(openssl rand -hex 20)/" .env
    rm -f .env.bak
    warn "DEEPSEEK_API_KEY in poc/.env eintragen"
  fi
  if ! grep -q '^AGW_API_TOKEN=.\{32,\}' .env; then
    info "lege AGW_API_TOKEN in .env an"
    sed -i.bak '/^AGW_API_TOKEN=/d' .env && rm -f .env.bak
    echo "AGW_API_TOKEN=$(openssl rand -hex 24)" >> .env
  fi
  if ! grep -q '^SEARXNG_SECRET=.\{32,\}' .env; then
    info "lege SEARXNG_SECRET in .env an"
    sed -i.bak '/^SEARXNG_SECRET=/d' .env && rm -f .env.bak
    echo "SEARXNG_SECRET=$(openssl rand -hex 24)" >> .env
  fi
  if grep -q '^DEEPSEEK_API_KEY=sk-\.\.\.$' .env; then
    warn "DEEPSEEK_API_KEY in poc/.env ist noch der Platzhalter"
  fi
}

build_sandbox_image() {
  # E9: zwei Abbilder aus einem Dockerfile, Ausführungs-Sandbox und Container von pi.
  info "baue Sandbox-Abbilder $IMAGE (Ausführung) und $PI_IMAGE (pi)"
  docker build -q -f images/agw-basis/Dockerfile --target exec -t "$IMAGE" . >/dev/null
  docker build -q -f images/agw-basis/Dockerfile --target pi -t "$PI_IMAGE" . >/dev/null
}

remove_sandboxes() {
  local ids nets
  ids=$(docker ps -aq --filter "label=$LABEL")
  if [[ -n "$ids" ]]; then
    info "entferne $(wc -w <<<"$ids" | tr -d ' ') Sandbox(en)"
    docker rm -f $ids >/dev/null
  fi
  nets=$(docker network ls -q --filter "label=agwpoc.slotnet")
  for n in $nets; do
    for c in $(docker network inspect -f '{{range $k, $v := .Containers}}{{$k}} {{end}}' "$n"); do
      docker network disconnect -f "$n" "$c" >/dev/null 2>&1 || true
    done
    docker network rm "$n" >/dev/null 2>&1 || true
  done
}

cmd_init() {
  need docker; need go; need npm
  ensure_env
  info "Go-Abhängigkeiten"
  go mod download
  if [[ -f web/package.json ]]; then
    info "Web-UI-Abhängigkeiten"
    (cd web && npm install --no-audit --no-fund)
  fi
  build_sandbox_image
  info "baue Orchestrator"
  docker compose build -q orchestrator
  info "fertig. Starten mit ./dev.sh start"
}

cmd_start() {
  local m=hot
  [[ "${1:-}" == "--prod" ]] && m=prod
  ensure_env
  (set -a; . ./.env; set +a; net_hygiene)
  build_sandbox_image
  mkdir -p "$DEVDIR"
  [[ "$(mode)" != "$m" ]] && stop_vite
  echo "$m" >"$DEVDIR/mode"
  local token; token=$(sed -n 's/^AGW_API_TOKEN=//p' .env)
  if [[ "$m" == hot ]]; then
    ensure_dist
    info "starte Orchestrator (Hot Reload aus dem Quelltext), Postgres, RustFS und Paket-Zwischenspeicher"
    dc up -d --wait --remove-orphans
    info "warte auf den ersten Bau des Orchestrators (beim ersten Mal rund eine Minute)"
    wait_api
    start_vite
    info "Web-UI mit Hot Reload (einmal anmelden): http://127.0.0.1:$VITE_PORT/login?token=${token}"
    info "eingebettete Fassung ohne Hot Reload: http://127.0.0.1:$HTTP_PORT (Stand von web/dist)"
    info "Go-Änderungen baut der Container selbst neu, verfolgen mit ./dev.sh logs"
  else
    info "starte Orchestrator (festes Abbild), Postgres, RustFS und Paket-Zwischenspeicher"
    dc up -d --build --wait --remove-orphans
    wait_api
    info "Web-UI (einmal anmelden): http://127.0.0.1:$HTTP_PORT/login?token=${token}"
  fi
  info "RustFS-Konsole: http://127.0.0.1:${AGW_S3_CONSOLE_PORT:-18483}"
}

cmd_stop() {
  stop_vite
  info "halte Dienste an"
  dc stop
  remove_sandboxes
}

cmd_status() {
  dc ps
  echo
  if [[ "$(mode)" == hot ]]; then
    if vite_running; then info "Hot Reload an, Vite: http://127.0.0.1:$VITE_PORT"
    else warn "Hot Reload an, Vite läuft aber nicht (./dev.sh start)"; fi
    echo
  fi
  info "Sandboxen"
  docker ps -a --filter "label=$LABEL" --format 'table {{.Names}}\t{{.Status}}\t{{.Label "agwpoc.variant"}}\t{{.Networks}}'
  echo
  if curl -fsS "http://127.0.0.1:${AGW_HTTP_PORT:-18480}/" >/dev/null 2>&1; then
    cmd_cli pool
  fi
}

cmd_logs() { dc logs -f --tail=200 orchestrator; }

# net_hygiene: entfernt leere Testnetze abgebrochener Läufe und warnt, wenn ein Docker-Netz die Adresse
# der Plattform-API überdeckt. Docker vergibt freie /16 der Reihe nach aus 172.17–172.31; die API der
# Instanz liegt im VPN bei 172.25.198.41. Ein Netz in 172.25.0.0/16 leitet sie in der Docker-VM ins Leere
# (am 05.10.2026 zweimal passiert). Die Netze des PoC und der Tests liegen deshalb fest in 10.231.x.
net_hygiene() {
  local n
  for n in $(docker network ls --format '{{.Name}}' --filter name=agwpoc_test_); do
    if [[ "$(docker network inspect "$n" --format '{{len .Containers}}' 2>/dev/null)" == 0 ]]; then
      docker network rm "$n" >/dev/null 2>&1 && info "leeres Testnetz $n entfernt"
    fi
  done
  local url=${AGW_PLATFORM_API_URL:-}
  [[ -z $url ]] && return 0
  local host=${url#*://}; host=${host%%/*}; host=${host%%:*}
  local ip
  ip=$(dig +short "$host" 2>/dev/null | grep -E '^[0-9]+(\.[0-9]+){3}$' | tail -1)
  [[ -z $ip ]] && return 0
  docker network ls -q | xargs docker network inspect --format '{{.Name}} {{range .IPAM.Config}}{{.Subnet}} {{end}}' 2>/dev/null |
    python3 -c 'import sys, ipaddress
ip = ipaddress.ip_address(sys.argv[1])
for line in sys.stdin:
    parts = line.split()
    for s in parts[1:]:
        if "/" in s and ":" not in s and ip in ipaddress.ip_network(s, strict=False):
            print(parts[0], s)' "$ip" |
    while read -r n sub; do
      warn "Docker-Netz $n ($sub) überdeckt die Plattform-API $host ($ip); aus Containern ist sie dann unerreichbar. Entfernen: docker network rm $n"
    done
}

# stage <name> <befehl…>: eine Teststufe mit Dauer; die Ausgabe kommt zeilenweise (grep --line-buffered),
# sonst sähe ein langer Lauf bis zum Ende wie ein Hänger aus.
stage() {
  local name=$1; shift
  local t0=$SECONDS
  info "$name"
  "$@"
  local rc=$?
  if [[ $rc -ne 0 ]]; then warn "$name fehlgeschlagen ($((SECONDS - t0)) s)"; return $rc; fi
  info "$name: ok ($((SECONDS - t0)) s)"
}

go_unit() {
  AGW_TEST_DATABASE_URL="postgres://agwpoc:${POSTGRES_PASSWORD}@127.0.0.1:${AGW_PG_PORT:-18482}/agwpoc?sslmode=disable" \
    go test -race -count=1 ./... 2>&1 | grep --line-buffered -vE '^(ok|\?) ' ; return "${PIPESTATUS[0]}"
}

go_docker() {
  # Nur die Pakete mit Docker-Tests, damit sie nicht mit allen übrigen um Docker konkurrieren.
  AGW_DOCKER_TESTS=1 \
  AGW_TEST_DATABASE_URL="postgres://agwpoc:${POSTGRES_PASSWORD}@127.0.0.1:${AGW_PG_PORT:-18482}/agwpoc?sslmode=disable" \
    sh -c 'go test -count=1 ./internal/sandbox/ && go test -count=1 -run TestWorkspaceRoundTripInSandbox ./internal/chat/' 2>&1 \
    | grep --line-buffered -E '^(ok|FAIL|---|panic)|_test.go:'; return "${PIPESTATUS[0]}"
}

go_s3() {
  docker run --rm --network agwpoc_intern -v "$PWD":/src -v "$(go env GOMODCACHE)":/go/pkg/mod -w /src \
    -e AGW_TEST_S3_ENDPOINT=rustfs:9000 -e RUSTFS_ACCESS_KEY -e RUSTFS_SECRET_KEY \
    golang:1.26-bookworm go test -count=1 -run S3 ./internal/artifacts/
}

go_slots() {
  # E9: ein ganzer Platz (pi ohne Shell, Ausführungs-Sandbox, exec-bridge.ts) mit geskriptetem
  # Modell, dazu der Gleichlauf der Umleitung mit pis eingebauten Werkzeugen (Test-Abbild
  # agw-parity). Im Go-Container, weil Unix-Sockets auf dem Mac nur innerhalb der Docker-VM gehen.
  docker build -q -f images/agw-basis/Dockerfile --target parity -t agwpoc/agw-parity:dev . >/dev/null || return 1
  docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v agwpoc_sockets:/run/agw \
    -v "$PWD":/src:ro -v agwpoc_gomod:/go/pkg/mod -v agwpoc_gocache:/root/.cache/go-build -w /src \
    -e AGW_E9_IN_DOCKER=1 -e GOFLAGS=-buildvcs=false -e AGW_IMAGE="$IMAGE" -e AGW_PI_IMAGE="$PI_IMAGE" \
    golang:1.26-bookworm go test -count=1 -v -run 'TestSlotE9|TestSlotBackground|TestSlotForeground|TestSlotWebSearch|TestSlotSubagentTalk|TestSlotSubagentIntercom|TestBridgeParity' ./internal/worker/ \
    | grep --line-buffered -E '^(=== RUN|--- |PASS|FAIL|ok|panic)|_test.go:'
  return "${PIPESTATUS[0]}"
}

web_unit() { (cd web && npm test -- --run 2>&1 | grep --line-buffered -E 'Test Files|Tests |FAIL|✗|×'; exit "${PIPESTATUS[0]}"); }

cmd_test() {
  local full=false
  [[ "${1:-}" == "--full" ]] && full=true
  ensure_env
  set -a; . ./.env; set +a
  local t0=$SECONDS
  if $full && [[ "$(mode)" == hot ]]; then
    warn "Hot Reload läuft: Während der Docker-Tests keine Go-Datei speichern, sonst baut der Orchestrator neu und stört die Tests."
  fi
  net_hygiene
  dc up -d --wait postgres rustfs >/dev/null
  ensure_dist
  stage "Go-Tests (Unit, Postgres, -race; ohne Docker)" go_unit || exit 1
  if [[ -f web/package.json ]]; then stage "Web-Tests" web_unit || exit 1; fi
  if $full; then
    build_sandbox_image
    stage "Docker-Integration (Sandbox, Arbeitsbereich)" go_docker || exit 1
    stage "S3-Integration (im Docker-Netz)" go_s3 || exit 1
    stage "Platztests mit geskriptetem Modell (E9, Hintergrund, Subagenten, Gleichlauf)" go_slots || exit 1
  else
    info "Docker-, S3- und Platztests übersprungen; vor dem Push: ./dev.sh test --full"
  fi
  info "alle Tests grün ($((SECONDS - t0)) s)"
}

cmd_e2e() {
  ensure_env
  build_sandbox_image
  info "starte Orchestrator mit niedriger Kompaktierungsschwelle (greift ab rund 10.000 Tokens)"
  ensure_dist
  AGW_COMPACT_RESERVE_TOKENS=990000 AGW_COMPACT_KEEP_RECENT_TOKENS=2000 AGW_POOL_SIZE_CLI=2 AGW_POOL_SIZE_MCP=1 \
    dc up -d $(build_flag) --wait >/dev/null
  wait_api
  local rc=0
  set -a; . ./.env; set +a
  AGW_E2E=1 go test -count=1 -v -timeout 45m ./e2e/ "$@" || rc=$?
  dc logs --no-color orchestrator > e2e/letzter-lauf.log 2>&1 || true
  if grep -q -E "panic|fatal error" e2e/letzter-lauf.log; then
    warn "Orchestrator ist während der Tests abgestürzt, siehe e2e/letzter-lauf.log"; rc=1
  fi
  info "Protokoll des Orchestrators: e2e/letzter-lauf.log"
  info "starte Orchestrator wieder mit normalen Einstellungen"
  dc up -d --wait >/dev/null
  wait_api || true
  return $rc
}

cmd_cli() { set -a; . ./.env; set +a; go run ./cmd/agw "$@"; }

cmd_reset() {
  if [[ "${1:-}" != "-y" ]]; then
    read -r -p "Wirklich ALLES löschen (Chats, Artefakte, Paket-Zwischenspeicher, Volumes, Netze, Abbilder)? [j/N] " a
    [[ "$a" == "j" || "$a" == "J" ]] || { info "abgebrochen"; exit 0; }
  fi
  stop_vite
  remove_sandboxes
  info "entferne Dienste, Volumes und Netze"
  docker compose -f compose.yaml -f compose.hot.yaml down -v --remove-orphans --rmi local
  for v in agwpoc_pg agwpoc_s3 agwpoc_sockets agwpoc_gomod agwpoc_gocache agwpoc_npmcache agwpoc_pipcache; do docker volume rm -f "$v" >/dev/null 2>&1 || true; done
  # Arbeitsbereiche aus Test-Läufen (agwpoc_test_ws_*)
  docker volume ls -q --filter name=agwpoc_test_ | xargs -r docker volume rm -f >/dev/null 2>&1 || true
  for n in agwpoc_intern agwpoc_sandbox agwpoc_egress agwpoc_pkg agwpoc_search; do docker network rm "$n" >/dev/null 2>&1 || true; done
  info "entferne Abbilder"
  docker image rm -f "$IMAGE" "$PI_IMAGE" agwpoc/agw-parity:dev agwpoc/orchestrator:dev >/dev/null 2>&1 || true
  rm -rf web/dist "$DEVDIR"
  info "zurückgesetzt. .env bleibt erhalten."
}

case "${1:-}" in
  init) cmd_init ;;
  start) shift; cmd_start "${1:-}" ;;
  stop) cmd_stop ;;
  status) cmd_status ;;
  logs) cmd_logs ;;
  test) shift; cmd_test "${1:-}" ;;
  e2e) shift; cmd_e2e "$@" ;;
  cli) shift; cmd_cli "$@" ;;
  reset|clean) shift; cmd_reset "${1:-}" ;;
  *) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
