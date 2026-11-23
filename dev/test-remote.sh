#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Philipp Schröer
#
# SPDX-License-Identifier: MIT
#
# Runs the stages of `./dev.sh test` (default run: Go with Postgres, then the web checks) on a remote Linux
# machine that only has Docker (no Go, no Node, no checkout). `./dev.sh test` on the development machine stays
# the main check; this script is for the times that machine must not take the load.
#
#   dev/test-remote.sh [options] <ssh-host> [<git-ref>]
#
#   <git-ref>        commit to test, default HEAD; shipped with `git archive`, so exactly that commit is tested
#   --tmux <name>    start the run in a detached tmux session on the host (survives an SSH drop) and return
#   --no-cache       remove the Go and npm cache volumes (prefix agwtr-cache-) before the run
#   --dry-run        only list what would run; does not connect to the host
#   -h, --help       this text
#
# Environment (defaults in brackets):
#   AGW_TR_CPUS [2]  AGW_TR_MEM_GO [4g]  AGW_TR_MEM_NODE [2g]  AGW_TR_MEM_PG [1g]   limits per container
#   AGW_TR_SUBNET_BASE [40]  third octet where the search for a free 10.231.X.0/24 starts (20 candidates)
#   GO_UNIT_TIMEOUT [5m]     time limit of the Go stage, as in dev.sh
#   AGW_TR_DOCKER            docker command on the host; default: docker, or sudo -n docker if that is needed
#
# What runs on the host (same order as `./dev.sh test`): .env from .env.example -> Postgres 17 in tmpfs ->
# web/dist built once (needed by web/embed.go) -> gofmt check + `go test -timeout 5m ./...` -> `npm test -- --run`.
# Everything runs in containers with the label agw-test-remote=<run id>, in an own network in 10.231.x, never in
# Docker's default range (a network there can hide the platform API, see docs/design.md). Cleanup removes only
# objects with that label. Log of every stage: ~/agw-test-remote/logs/<run id>/ on the host; the work directory
# ~/agw-test-remote/<ref>-<time> is removed at the end.
#
# Not identical to dev.sh: no caffeinate (Linux), RustFS is not started (only the --docker stages use it), the
# Go test cache lives in a volume of its own, Linux instead of macOS (some tests may behave differently).
set -euo pipefail

# ---- pinned images (same pins as compose.yaml and Dockerfile; keep in step) -------------------------------------
PG_IMAGE='postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24'
GO_IMAGE='golang:1.26-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af'
NODE_IMAGE='node:22-bookworm-slim@sha256:de170b39439eb0e5a3444ad7d60b015614a381e10fecae2e30ec9e735122f38d'
CACHE_PREFIX=agwtr-cache
LABEL_KEY=agw-test-remote
ROOT_REL=agw-test-remote

CPUS=${AGW_TR_CPUS:-2}
MEM_GO=${AGW_TR_MEM_GO:-4g}
MEM_NODE=${AGW_TR_MEM_NODE:-2g}
MEM_PG=${AGW_TR_MEM_PG:-1g}
GO_UNIT_TIMEOUT=${GO_UNIT_TIMEOUT:-5m}
SUBNET_BASE=${AGW_TR_SUBNET_BASE:-40}

info() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }

usage() { sed -n '/^#   dev\/test-remote/,/^# What runs/p' "$0" | sed '$d; s/^# \{0,1\}//'; }

# ======================================================================================================
# Part 2: runs ON the host (called as `test-remote.sh --on-host <run-id> <workdir> <logdir> [--no-cache]`)
# ======================================================================================================
on_host() {
  local run=$1 work=$2 logs=$3 nocache=${4:-}
  local D net pg subnet
  mkdir -p "$logs"
  if [[ -n ${AGW_TR_DOCKER:-} ]]; then D=$AGW_TR_DOCKER
  elif docker info >/dev/null 2>&1; then D=docker
  elif sudo -n docker info >/dev/null 2>&1; then D='sudo -n docker'
  else warn "no access to Docker (tried docker and sudo -n docker)"; return 2; fi
  # shellcheck disable=SC2086
  dk() { $D "$@"; }
  local lbl="$LABEL_KEY=$run"
  net="agwtr-$run"; pg="agwtr-pg-$run"
  local uid gid; uid=$(id -u); gid=$(id -g)

  cleanup() {
    local rc=$?
    set +e
    local ids nets
    ids=$(dk ps -aq --filter "label=$lbl")
    # shellcheck disable=SC2086
    [[ -n $ids ]] && { info "removing containers of run $run"; dk rm -f $ids >/dev/null; }
    nets=$(dk network ls -q --filter "label=$lbl")
    # shellcheck disable=SC2086
    [[ -n $nets ]] && dk network rm $nets >/dev/null
    rm -rf "$work"
    echo "$rc" >"$logs/exit-code"
    if [[ $rc -eq 0 ]]; then info "ALL GREEN; logs: $logs"; else warn "FAILED (exit $rc); logs: $logs"; fi
  }
  trap cleanup EXIT
  trap 'exit 130' INT TERM HUP

  if [[ $nocache == --no-cache ]]; then
    info "removing cache volumes ($CACHE_PREFIX-*)"
    dk volume rm -f "$CACHE_PREFIX-gomod" "$CACHE_PREFIX-gocache" "$CACHE_PREFIX-npm" >/dev/null || true
  fi
  local v
  for v in gomod gocache npm; do
    dk volume create --label "agw-test-remote-cache=1" "$CACHE_PREFIX-$v" >/dev/null
  done
  # volumes start root-owned; the test containers run as the calling user
  dk run --rm --label "$lbl" --user 0 -v "$CACHE_PREFIX-gomod:/c/gomod" -v "$CACHE_PREFIX-gocache:/c/gocache" \
    -v "$CACHE_PREFIX-npm:/c/npm" --entrypoint chown "$NODE_IMAGE" -R "$uid:$gid" /c >/dev/null

  # own network: first free /24 in 10.231.X.0/24, X from SUBNET_BASE (slots and the stack use other parts of 10.231)
  local used x created=""
  # shellcheck disable=SC2046
  used=$(dk network ls -q | xargs -r $D network inspect --format '{{range .IPAM.Config}}{{.Subnet}} {{end}}' 2>/dev/null || true)
  for ((x = SUBNET_BASE; x < SUBNET_BASE + 20; x++)); do
    subnet="10.231.$x.0/24"
    grep -qF "10.231.$x." <<<"$used" && continue
    if dk network create --label "$lbl" --subnet "$subnet" "$net" >/dev/null 2>&1; then created=$subnet; break; fi
  done
  [[ -n $created ]] || { warn "no free 10.231.X.0/24 network found from $SUBNET_BASE on"; return 2; }
  info "network $net ($created), run $run"

  # .env as ensure_env in dev.sh creates it
  (cd "$work" && cp .env.example .env &&
    sed -i \
      -e "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 16)/" \
      -e "s/^RUSTFS_ACCESS_KEY=.*/RUSTFS_ACCESS_KEY=agw$(openssl rand -hex 8)/" \
      -e "s/^RUSTFS_SECRET_KEY=.*/RUSTFS_SECRET_KEY=$(openssl rand -hex 20)/" \
      -e "s/^AGW_API_TOKEN=.*/AGW_API_TOKEN=$(openssl rand -hex 24)/" \
      -e "s/^SEARXNG_SECRET=.*/SEARXNG_SECRET=$(openssl rand -hex 24)/" .env)
  local pw; pw=$(sed -n 's/^POSTGRES_PASSWORD=//p' "$work/.env")

  info "starting Postgres (data in tmpfs)"
  dk run -d --name "$pg" --label "$lbl" --network "$net" --network-alias pg --memory "$MEM_PG" --cpus "$CPUS" \
    --tmpfs /var/lib/postgresql/data:rw,size=768m \
    -e POSTGRES_USER=agwpoc -e POSTGRES_DB=agwpoc -e POSTGRES_PASSWORD="$pw" "$PG_IMAGE" >/dev/null
  local i
  for i in $(seq 1 40); do
    dk exec "$pg" pg_isready -U agwpoc -d agwpoc >/dev/null 2>&1 && break
    [[ $i == 40 ]] && { warn "Postgres did not become ready"; dk logs "$pg" >"$logs/postgres.log" 2>&1; return 1; }
    sleep 2
  done

  # common container flags; the work directory is mounted at /src
  local common=(--rm --label "$lbl" --network "$net" --cpus "$CPUS" --user "$uid:$gid" -v "$work:/src" -w /src
    -e HOME=/tmp)
  local gocommon=("${common[@]}" --memory "$MEM_GO" -v "$CACHE_PREFIX-gomod:/go/pkg/mod"
    -v "$CACHE_PREFIX-gocache:/gocache" -v "$logs:/logs" -e GOCACHE=/gocache -e GOFLAGS=-buildvcs=false
    --env-file "$work/.env" -e GO_UNIT_TIMEOUT="$GO_UNIT_TIMEOUT")
  local nodecommon=("${common[@]}" --memory "$MEM_NODE" -v "$CACHE_PREFIX-npm:/npmcache" -e npm_config_cache=/npmcache)

  local t0 rc n=0 filter
  # stage <name> <logfile> <docker run args…>: the log is complete, the terminal shows what does not match $filter
  stage() {
    local name=$1 log=$2; shift 2
    n=$((n + 1)); t0=$SECONDS
    info "$name"
    set +e
    dk run "$@" 2>&1 | tee "$logs/$n-$log.log" | grep --line-buffered -vE "$filter"
    rc=${PIPESTATUS[0]}
    set -e
    if [[ $rc -ne 0 ]]; then warn "$name failed ($((SECONDS - t0)) s); complete output: $logs/$n-$log.log"; return "$rc"; fi
    info "$name: ok ($((SECONDS - t0)) s)"
  }

  # as ensure_dist: web/embed.go embeds web/dist, which is not in the archive
  filter='^$'
  stage "web UI build (needed by web/embed.go)" web-build "${nodecommon[@]}" "$NODE_IMAGE" \
    sh -c 'cd web && npm install --no-audit --no-fund >/dev/null && npm run build >/dev/null' || return $?

  # as go_unit in dev.sh: gofmt check, then go test; "ok"/"?" lines are not shown on the terminal
  filter='^(ok|\?) '
  stage "Go tests (unit, Postgres, cached, without -race; without Docker)" go-unit "${gocommon[@]}" \
    -e AGW_CONFORMANCE_REPORT=/logs/conformance.md \
    -e "AGW_TEST_DATABASE_URL=postgres://agwpoc:$pw@pg:5432/agwpoc?sslmode=disable" "$GO_IMAGE" \
    sh -c 'unformatted="$(gofmt -l internal cmd 2>/dev/null)"
      if [ -n "$unformatted" ]; then echo "gofmt needed:"; echo "$unformatted"; exit 1; fi
      go test -timeout "$GO_UNIT_TIMEOUT" ./...' || return $?

  # as web_unit in dev.sh
  filter='^$'
  stage "web tests" web-unit "${nodecommon[@]}" "$NODE_IMAGE" \
    bash -c 'cd web && npm test -- --run 2>&1 | grep --line-buffered -E "Test Files|Tests |FAIL|✗|×"; exit "${PIPESTATUS[0]}"' || return $?
}

# ======================================================================================================
# Part 1: runs on the calling machine
# ======================================================================================================
main() {
  local host="" ref=HEAD tmux="" nocache="" dry=false
  while [[ $# -gt 0 ]]; do
    case $1 in
      --on-host) shift; on_host "$@"; return $? ;;
      --tmux) tmux=${2:?--tmux needs a session name}; shift 2 ;;
      --no-cache) nocache=--no-cache; shift ;;
      --dry-run) dry=true; shift ;;
      -h|--help) usage; return 0 ;;
      -*) warn "unknown option $1"; usage; return 2 ;;
      *) if [[ -z $host ]]; then host=$1; else ref=$1; fi; shift ;;
    esac
  done
  [[ -n $host ]] || { usage; return 2; }
  [[ $tmux =~ ^[A-Za-z0-9_.-]*$ ]] || { warn "tmux name: only A-Z a-z 0-9 _ . -"; return 2; }

  cd "$(dirname "$0")/.."
  local sha short run work logs
  sha=$(git rev-parse --verify "$ref^{commit}") || return 2
  short=$(git rev-parse --short "$sha")
  run="$short-$(date +%Y%m%d-%H%M%S)"
  work="\$HOME/$ROOT_REL/$run"
  logs="\$HOME/$ROOT_REL/logs/$run"

  info "stages that would run for commit $short ($ref):"
  printf '   %s\n' \
    "archive of $sha -> $work (removed at the end)" \
    ".env from .env.example; Postgres ($PG_IMAGE) in tmpfs, own network 10.231.X.0/24 from X=$SUBNET_BASE" \
    "web UI build (npm install, npm run build) in $NODE_IMAGE" \
    "Go tests: gofmt -l internal cmd; go test -timeout $GO_UNIT_TIMEOUT ./... in $GO_IMAGE" \
    "web tests: npm test -- --run" \
    "limits per container: --cpus $CPUS, memory go $MEM_GO, node $MEM_NODE, postgres $MEM_PG" \
    "logs: $logs${tmux:+; detached in tmux session $tmux}${nocache:+; cache volumes removed first}"
  if $dry; then return 0; fi

  info "shipping $short"
  # shellcheck disable=SC2029
  git archive --format=tar "$sha" | ssh "$host" "mkdir -p $work && tar -x -C $work"
  local remote="bash $work/dev/test-remote.sh --on-host $run $work $logs $nocache"
  if [[ -n $tmux ]]; then
    # shellcheck disable=SC2029
    ssh "$host" "mkdir -p $logs && tmux new-session -d -s '$tmux' \"$remote 2>&1 | tee $logs/run.log\""
    cat <<EOT
Started in tmux session '$tmux' on $host (run $run).
  attach:  ssh -t $host tmux attach -t $tmux      (detach with Ctrl-b d; the session ends with the run)
  state:   ssh $host 'cat $logs/exit-code'         (appears at the end: 0 = green)
  log:     ssh $host 'tail -n 50 $logs/run.log'
  fetch:   scp -r $host:'$ROOT_REL/logs/$run' .    (stage logs, conformance report)
  clean:   ssh $host 'rm -rf $logs'
EOT
    return 0
  fi
  # shellcheck disable=SC2029
  ssh -t "$host" "bash -c 'mkdir -p $logs && set -o pipefail && $remote 2>&1 | tee $logs/run.log'"
}

main "$@"
