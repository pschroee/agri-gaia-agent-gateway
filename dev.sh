#!/usr/bin/env bash
# Development script for the PoC (stage 1). Can be called from any directory.
#
#   ./dev.sh init     create .env (if missing), build images, fetch web UI dependencies
#   ./dev.sh start    build the agw-basis image and start the orchestrator, Postgres, RustFS and the
#                     package caches (npm-cache, pip-cache), with
#                     hot reload: web UI via Vite on :18484, the orchestrator rebuilds itself in the
#                     container on Go changes. ./dev.sh start --prod builds the fixed image
#   ./dev.sh stop     stop everything and remove sandboxes (data is kept)
#   ./dev.sh status   show services, sandboxes and pool
#   ./dev.sh logs     follow the orchestrator log
#   ./dev.sh test     fast tests (Go with Postgres, without Docker, with Go's test cache, without -race; web);
#                     the everyday and pre-push check
#   ./dev.sh test --full   the same stages without the test cache (-count=1) and with -race, about 30 s; on request
#   ./dev.sh test --docker additionally Docker integration, S3 and slot tests (off by default since
#                     issue #27: they start real containers and load the machine, about 7 min);
#                     --dry-run with any of these only lists the stages it would run
#   ./dev.sh e2e      end-to-end tests with a real model (costs a few cents); starts the
#                     orchestrator with a low compaction threshold for it and normally again afterwards
#   ./dev.sh cli ...  CLI agw against the running orchestrator (e.g. ./dev.sh cli pool)
#   ./dev.sh reset    delete EVERYTHING: containers, sandboxes, volumes (chats, artifacts), networks, images
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

need() { command -v "$1" >/dev/null || { warn "$1 is missing"; exit 1; }; }

# Mode of the last start: hot (default) or prod. All Compose calls use the
# same files, otherwise e.g. ./dev.sh e2e would replace the hot-reload container.
mode() { cat "$DEVDIR/mode" 2>/dev/null || echo prod; }
dc() {
  if [[ "$(mode)" == hot ]]; then docker compose -f compose.yaml -f compose.hot.yaml "$@"
  else docker compose "$@"; fi
}
build_flag() { [[ "$(mode)" == hot ]] || echo --build; }

ensure_dist() {
  # web/embed.go embeds web/dist; without a built UI the orchestrator cannot be built.
  [[ -f web/dist/index.html ]] && return
  [[ -d web/node_modules ]] || (cd web && npm install --no-audit --no-fund >/dev/null)
  info "building the web UI once (for the embedded version)"
  (cd web && npm run build >/dev/null)
}

wait_api() {
  local i
  for i in $(seq 1 240); do
    curl -fsS -o /dev/null "http://127.0.0.1:$HTTP_PORT/" 2>/dev/null && return 0
    sleep 1
  done
  warn "orchestrator does not respond, see ./dev.sh logs"; return 1
}

vite_running() { [[ -f "$DEVDIR/vite.pid" ]] && kill -0 "$(cat "$DEVDIR/vite.pid")" 2>/dev/null; }

start_vite() {
  vite_running && { info "Vite is already running"; return; }
  [[ -d web/node_modules ]] || (cd web && npm install --no-audit --no-fund >/dev/null)
  info "starting Vite (hot reload of the web UI) on 127.0.0.1:$VITE_PORT"
  # set -m: own process group, so that stop also ends Vite's child processes
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
  warn "Vite does not start, see $DEVDIR/vite.log"
}

stop_vite() {
  vite_running || { rm -f "$DEVDIR/vite.pid"; return 0; }
  info "stopping Vite"
  kill -TERM -- "-$(cat "$DEVDIR/vite.pid")" 2>/dev/null || kill -TERM "$(cat "$DEVDIR/vite.pid")" 2>/dev/null || true
  rm -f "$DEVDIR/vite.pid"
}

ensure_env() {
  if [[ ! -f .env ]]; then
    info ".env is missing, creating it from .env.example with random values"
    cp .env.example .env
    sed -i.bak \
      -e "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 16)/" \
      -e "s/^RUSTFS_ACCESS_KEY=.*/RUSTFS_ACCESS_KEY=agw$(openssl rand -hex 8)/" \
      -e "s/^RUSTFS_SECRET_KEY=.*/RUSTFS_SECRET_KEY=$(openssl rand -hex 20)/" .env
    rm -f .env.bak
    warn "enter DEEPSEEK_API_KEY in .env"
  fi
  if ! grep -q '^AGW_API_TOKEN=.\{32,\}' .env; then
    info "creating AGW_API_TOKEN in .env"
    sed -i.bak '/^AGW_API_TOKEN=/d' .env && rm -f .env.bak
    echo "AGW_API_TOKEN=$(openssl rand -hex 24)" >> .env
  fi
  if ! grep -q '^SEARXNG_SECRET=.\{32,\}' .env; then
    info "creating SEARXNG_SECRET in .env"
    sed -i.bak '/^SEARXNG_SECRET=/d' .env && rm -f .env.bak
    echo "SEARXNG_SECRET=$(openssl rand -hex 24)" >> .env
  fi
  if grep -q '^DEEPSEEK_API_KEY=sk-\.\.\.$' .env; then
    warn "DEEPSEEK_API_KEY in .env is still the placeholder"
  fi
}

build_sandbox_image() {
  # E9: two images from one Dockerfile, execution sandbox and pi's container.
  info "building sandbox images $IMAGE (execution) and $PI_IMAGE (pi)"
  docker build -q -f images/agw-basis/Dockerfile --target exec -t "$IMAGE" . >/dev/null
  docker build -q -f images/agw-basis/Dockerfile --target pi -t "$PI_IMAGE" . >/dev/null
}

remove_sandboxes() {
  local ids nets
  ids=$(docker ps -aq --filter "label=$LABEL")
  if [[ -n "$ids" ]]; then
    info "removing $(wc -w <<<"$ids" | tr -d ' ') sandbox(es)"
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
  info "Go dependencies"
  go mod download
  if [[ -f web/package.json ]]; then
    info "web UI dependencies"
    (cd web && npm install --no-audit --no-fund)
  fi
  build_sandbox_image
  info "building orchestrator"
  docker compose build -q orchestrator
  info "done. Start with ./dev.sh start"
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
    info "starting orchestrator (hot reload from source), Postgres, RustFS and package caches"
    dc up -d --wait --remove-orphans
    info "waiting for the first orchestrator build (about a minute the first time)"
    wait_api
    start_vite
    info "web UI with hot reload (log in once): http://127.0.0.1:$VITE_PORT/login?token=${token}"
    info "embedded version without hot reload: http://127.0.0.1:$HTTP_PORT (state of web/dist)"
    info "the container rebuilds Go changes itself, follow with ./dev.sh logs"
  else
    info "starting orchestrator (fixed image), Postgres, RustFS and package caches"
    dc up -d --build --wait --remove-orphans
    wait_api
    info "web UI (log in once): http://127.0.0.1:$HTTP_PORT/login?token=${token}"
  fi
  info "RustFS console: http://127.0.0.1:${AGW_S3_CONSOLE_PORT:-18483}"
}

cmd_stop() {
  stop_vite
  info "stopping services"
  dc stop
  remove_sandboxes
}

cmd_status() {
  dc ps
  echo
  if [[ "$(mode)" == hot ]]; then
    if vite_running; then info "hot reload on, Vite: http://127.0.0.1:$VITE_PORT"
    else warn "hot reload on, but Vite is not running (./dev.sh start)"; fi
    echo
  fi
  info "sandboxes"
  docker ps -a --filter "label=$LABEL" --format 'table {{.Names}}\t{{.Status}}\t{{.Label "agwpoc.variant"}}\t{{.Networks}}'
  echo
  if curl -fsS "http://127.0.0.1:${AGW_HTTP_PORT:-18480}/" >/dev/null 2>&1; then
    cmd_cli pool
  fi
}

cmd_logs() { dc logs -f --tail=200 orchestrator; }

# net_hygiene: removes empty test networks of aborted runs and warns when a Docker network covers the
# address of the platform API. Docker assigns free /16s in order from 172.17–172.31; the instance's API
# is at 172.25.198.41 in the VPN. A network in 172.25.0.0/16 routes it into nowhere inside the Docker VM
# (happened twice on 2026-10-05). The networks of the PoC and the tests are therefore fixed in 10.231.x.
net_hygiene() {
  local n
  for n in $(docker network ls --format '{{.Name}}' --filter name=agwpoc_test_); do
    if [[ "$(docker network inspect "$n" --format '{{len .Containers}}' 2>/dev/null)" == 0 ]]; then
      docker network rm "$n" >/dev/null 2>&1 && info "removed empty test network $n"
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
      warn "Docker network $n ($sub) covers the platform API $host ($ip); it is then unreachable from containers. Remove: docker network rm $n"
    done
}

# stage <name> <command…>: one test stage with its duration; output comes line by line (grep --line-buffered),
# otherwise a long run would look like a hang until the end. The terminal only shows a filtered view; the
# complete, unfiltered output of each stage goes to $STAGE_LOG under .dev/test-logs/<run>/ (ignored), so a
# failure message is never lost. The stage functions write it with tee before their filter.
TEST_LOG_DIR=""
STAGE_LOG=/dev/null
stage_no=0
stage() {
  local name=$1; shift
  local t0=$SECONDS
  stage_no=$((stage_no + 1))
  if [[ -n "$TEST_LOG_DIR" ]]; then
    STAGE_LOG="$TEST_LOG_DIR/$stage_no-$(printf '%s' "$name" | tr -cs 'A-Za-z0-9' '-' | sed 's/^-//; s/-$//' | cut -c1-40).log"
    : >"$STAGE_LOG"
  fi
  info "$name"
  "$@"
  local rc=$?
  if [[ $rc -ne 0 ]]; then
    warn "$name failed ($((SECONDS - t0)) s)"
    [[ "$STAGE_LOG" != /dev/null ]] && warn "complete output of the stage: $STAGE_LOG"
    return $rc
  fi
  info "$name: ok ($((SECONDS - t0)) s)"
}

# new_test_logs: one directory per run under .dev/test-logs; only the last 10 runs are kept.
new_test_logs() {
  TEST_LOG_DIR="$PWD/.dev/test-logs/$(date +%Y%m%d-%H%M%S)"
  mkdir -p "$TEST_LOG_DIR"
  ls -1d "$PWD"/.dev/test-logs/*/ 2>/dev/null | sort -r | tail -n +11 | xargs -r rm -rf
}

# GO_UNIT_FLAGS: empty for ./dev.sh test (Go's test cache, no -race), "-race -count=1" for --full.
GO_UNIT_FLAGS=""
go_unit() {
  # The cache key includes the env vars a test reads (AGW_TEST_DATABASE_URL) and embedded files (schema.sql), but
  # not the state of Postgres itself; the tests create their own schema, so that state should not matter.
  AGW_TEST_DATABASE_URL="postgres://agwpoc:${POSTGRES_PASSWORD}@127.0.0.1:${AGW_PG_PORT:-18482}/agwpoc?sslmode=disable" \
    go test $GO_UNIT_FLAGS ./... 2>&1 | tee -a "$STAGE_LOG" | grep --line-buffered -vE '^(ok|\?) ' ; return "${PIPESTATUS[0]}"
}

go_docker() {
  # Only the packages with Docker tests, so they do not compete with all the others for Docker.
  AGW_DOCKER_TESTS=1 \
  AGW_TEST_DATABASE_URL="postgres://agwpoc:${POSTGRES_PASSWORD}@127.0.0.1:${AGW_PG_PORT:-18482}/agwpoc?sslmode=disable" \
    sh -c 'go test -count=1 ./internal/sandbox/ && go test -count=1 -run TestWorkspaceRoundTripInSandbox ./internal/chat/' 2>&1 \
    | tee -a "$STAGE_LOG" | grep --line-buffered -E '^(ok|FAIL|---|panic)|_test.go:'; return "${PIPESTATUS[0]}"
}

go_s3() {
  docker run --rm --network agwpoc_intern -v "$PWD":/src -v "$(go env GOMODCACHE)":/go/pkg/mod -w /src \
    -e AGW_TEST_S3_ENDPOINT=rustfs:9000 -e RUSTFS_ACCESS_KEY -e RUSTFS_SECRET_KEY \
    golang:1.26-bookworm go test -count=1 -run S3 ./internal/artifacts/ 2>&1 | tee -a "$STAGE_LOG"
  return "${PIPESTATUS[0]}"
}

go_slots() {
  # E9: a whole slot (pi without a shell, execution sandbox, exec-bridge.ts) with a scripted
  # model, plus the parity of the redirection with pi's built-in tools (test image
  # agw-parity). In a Go container because Unix sockets on the Mac only work inside the Docker VM.
  docker build -q -f images/agw-basis/Dockerfile --target parity -t agwpoc/agw-parity:dev . >>"$STAGE_LOG" 2> >(tee -a "$STAGE_LOG" >&2) || return 1
  docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v agwpoc_sockets:/run/agw \
    -v "$PWD":/src:ro -v agwpoc_gomod:/go/pkg/mod -v agwpoc_gocache:/root/.cache/go-build -w /src \
    -e AGW_E9_IN_DOCKER=1 -e GOFLAGS=-buildvcs=false -e AGW_IMAGE="$IMAGE" -e AGW_PI_IMAGE="$PI_IMAGE" \
    golang:1.26-bookworm go test -count=1 -v -run 'TestSlotE9|TestSlotBackground|TestSlotForeground|TestSlotWebSearch|TestSlotSubagentTalk|TestSlotSubagentIntercom|TestBridgeParity' ./internal/worker/ 2> >(tee -a "$STAGE_LOG" >&2) \
    | tee -a "$STAGE_LOG" | grep --line-buffered -E '^(=== RUN|--- |PASS|FAIL|ok|panic)|_test.go:'
  return "${PIPESTATUS[0]}"
}

web_unit() { (cd web && npm test -- --run 2>&1 | tee -a "$STAGE_LOG" | grep --line-buffered -E 'Test Files|Tests |FAIL|✗|×'; exit "${PIPESTATUS[0]}"); }

cmd_test() {
  # Default: Go with the test cache and without -race, plus web. --full: the same stages without the cache and
  # with -race. --docker: additionally Docker integration, S3 and slot tests, which are off by default (issue #27,
  # decision of the author: they start real containers and load the machine). --dry-run: only list the stages.
  local full=false docker=false dry=false arg
  for arg in "$@"; do
    case "$arg" in
      --full) full=true ;;
      --docker) docker=true ;;
      --dry-run) dry=true ;;
      *) warn "unknown option for test: $arg (known: --full, --docker, --dry-run)"; exit 2 ;;
    esac
  done
  local go_name="Go tests (unit, Postgres, cached, without -race; without Docker)"
  if $full; then GO_UNIT_FLAGS="-race -count=1"; go_name="Go tests (unit, Postgres, -race, uncached; without Docker)"; fi
  local run=("$go_name" "web tests") skipped=()
  if $docker; then
    run+=("Docker integration (sandbox, workspace)" "S3 integration (in the Docker network)"
      "slot tests with a scripted model (E9, background, subagents, parity)")
  else
    skipped+=("Docker integration" "S3 integration" "slot tests")
  fi
  if $dry; then
    info "stages that would run:"; printf '   %s\n' "${run[@]}"
    [[ ${#skipped[@]} -gt 0 ]] && { info "skipped:"; printf '   %s\n' "${skipped[@]}"; }
    return 0
  fi
  ensure_env
  set -a; . ./.env; set +a
  local t0=$SECONDS
  if $docker && [[ "$(mode)" == hot ]]; then
    warn "hot reload is running: do not save any Go file during the Docker tests, otherwise the orchestrator rebuilds and disturbs the tests."
  fi
  net_hygiene
  new_test_logs
  dc up -d --wait postgres rustfs >/dev/null
  ensure_dist
  stage "$go_name" go_unit || exit 1
  if [[ -f web/package.json ]]; then stage "web tests" web_unit || exit 1; fi
  if $docker; then
    build_sandbox_image
    stage "Docker integration (sandbox, workspace)" go_docker || exit 1
    stage "S3 integration (in the Docker network)" go_s3 || exit 1
    stage "slot tests with a scripted model (E9, background, subagents, parity)" go_slots || exit 1
  fi
  info "all tests green ($((SECONDS - t0)) s); complete output in $TEST_LOG_DIR"
  if [[ ${#skipped[@]} -gt 0 ]]; then
    local list; list=$(printf '%s, ' "${skipped[@]}"); list=${list%, }
    info "skipped: $list (off by default, issue #27); run them with ./dev.sh test --docker"
  fi
}

cmd_e2e() {
  ensure_env
  build_sandbox_image
  info "starting orchestrator with a low compaction threshold (kicks in at about 10,000 tokens)"
  ensure_dist
  AGW_COMPACT_RESERVE_TOKENS=990000 AGW_COMPACT_KEEP_RECENT_TOKENS=2000 AGW_POOL_SIZE=2 \
    dc up -d $(build_flag) --wait >/dev/null
  wait_api
  local rc=0
  set -a; . ./.env; set +a
  AGW_E2E=1 go test -count=1 -v -timeout 45m ./e2e/ "$@" || rc=$?
  dc logs --no-color orchestrator > e2e/last-run.log 2>&1 || true
  if grep -q -E "panic|fatal error" e2e/last-run.log; then
    warn "orchestrator crashed during the tests, see e2e/last-run.log"; rc=1
  fi
  info "orchestrator log: e2e/last-run.log"
  info "restarting orchestrator with normal settings"
  dc up -d --wait >/dev/null
  wait_api || true
  return $rc
}

cmd_cli() { set -a; . ./.env; set +a; go run ./cmd/agw "$@"; }

cmd_reset() {
  if [[ "${1:-}" != "-y" ]]; then
    read -r -p "Really delete EVERYTHING (chats, artifacts, package caches, volumes, networks, images)? [y/N] " a
    [[ "$a" == "y" || "$a" == "Y" ]] || { info "aborted"; exit 0; }
  fi
  stop_vite
  remove_sandboxes
  info "removing services, volumes and networks"
  docker compose -f compose.yaml -f compose.hot.yaml down -v --remove-orphans --rmi local
  for v in agwpoc_pg agwpoc_s3 agwpoc_sockets agwpoc_gomod agwpoc_gocache agwpoc_npmcache agwpoc_pipcache; do docker volume rm -f "$v" >/dev/null 2>&1 || true; done
  # workspaces from test runs (agwpoc_test_ws_*)
  docker volume ls -q --filter name=agwpoc_test_ | xargs -r docker volume rm -f >/dev/null 2>&1 || true
  for n in agwpoc_intern agwpoc_sandbox agwpoc_egress agwpoc_pkg agwpoc_search; do docker network rm "$n" >/dev/null 2>&1 || true; done
  info "removing images"
  docker image rm -f "$IMAGE" "$PI_IMAGE" agwpoc/agw-parity:dev agwpoc/orchestrator:dev >/dev/null 2>&1 || true
  rm -rf web/dist "$DEVDIR"
  info "reset done. .env is kept."
}

case "${1:-}" in
  init) cmd_init ;;
  start) shift; cmd_start "${1:-}" ;;
  stop) cmd_stop ;;
  status) cmd_status ;;
  logs) cmd_logs ;;
  test) shift; cmd_test "$@" ;;
  e2e) shift; cmd_e2e "$@" ;;
  cli) shift; cmd_cli "$@" ;;
  reset|clean) shift; cmd_reset "${1:-}" ;;
  *) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
