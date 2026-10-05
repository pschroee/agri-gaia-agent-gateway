# Images of a slot (E9). Build context is poc/ (because of agw-artifact and agw-exec).
#
#   --target pi    agwpoc/agw-pi      pi container: pi, pi-subagents, rpiv-todo, extensions.
#                                     No shell, no Python, no /workspace content.
#   --target exec  agwpoc/agw-basis   Execution sandbox: Python, Typst, tools, agw-artifact.
#                                     No pi. All of the agent's tools run here.

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/agw-artifact ./cmd/agw-artifact
COPY cmd/agw-exec ./cmd/agw-exec
COPY internal/execproto ./internal/execproto
COPY internal/platform ./internal/platform
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agw-artifact ./cmd/agw-artifact \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agw-exec ./cmd/agw-exec

# ---------------------------------------------------------------------------
# pi container. Node.js LTS (24, active LTS since October 2025); pi requires at least 22.19.
FROM node:24-bookworm-slim AS pi
ARG PI_VERSION=0.87.1
RUN npm install -g @earendil-works/pi-coding-agent@${PI_VERSION} \
 && npm cache clean --force
# The agent's task list: tool `todo` from rpiv-todo (choice justified in docs/design.md,
# "Tasks"). The state lives in the tool results of the pi session, not in files.
# Before pi-subagents, because `pi install` via npm removes unlisted packages in the same prefix.
ARG RPIV_TODO_VERSION=2.11.0
# Web search: web_search and web_extract from pi-searxng-suite (MIT, unchanged). The requests go
# through the orchestrator's web proxy to our own SearXNG or to the web, only with internet;
# web-gate.ts hides the tools without internet (docs/design.md, "Web search").
ARG SEARXNG_SUITE_VERSION=0.2.3
# Messages between the sessions of a slot (main agent and subagents): pi-intercom (MIT),
# tool intercom; the broker runs in the pi container over a Unix socket in the home. pi install
# fetches the package and its dependencies (tsx); our own copy third_party/pi-intercom then
# replaces the package's files (change: VENDORED.md).
ARG PI_INTERCOM_VERSION=0.15.0
RUN PI_CODING_AGENT_DIR=/opt/agw/pihome pi install npm:@juicesharp/rpiv-todo@${RPIV_TODO_VERSION} \
 && PI_CODING_AGENT_DIR=/opt/agw/pihome pi install npm:pi-searxng-suite@${SEARXNG_SUITE_VERSION} \
 && PI_CODING_AGENT_DIR=/opt/agw/pihome pi install npm:pi-intercom@${PI_INTERCOM_VERSION} \
 && test -f /opt/agw/pihome/npm/node_modules/pi-intercom/index.ts \
 && test -f /opt/agw/pihome/npm/node_modules/@juicesharp/rpiv-todo/index.ts \
 && test -f /opt/agw/pihome/npm/node_modules/pi-searxng-suite/index.ts \
 && npm cache clean --force
COPY third_party/pi-intercom /opt/agw/pihome/npm/node_modules/pi-intercom
RUN grep -q '"version": "0.15.0"' /opt/agw/pihome/npm/node_modules/pi-intercom/package.json \
 && grep -q "PI_INTERCOM_REFUSE_WHEN_BUSY" /opt/agw/pihome/npm/node_modules/pi-intercom/index.ts
COPY images/agw-basis/ext /opt/agw/ext
# Subagents: our own copy of pi-subagents in the repo (third_party/pi-subagents, 0.73.1 with one
# change, see third_party/pi-subagents/VENDORED.md): it takes the worker for workflowScript from
# the module in PI_SUBAGENTS_WORKFLOW_WORKER, otherwise from node:worker_threads. It lives where
# `pi install` would put it; at runtime pi loads it explicitly via -e. The runtime
# dependencies come via `npm ci` with pinned integrity from
# images/agw-basis/pi-subagents-deps (package-lock.json); the check afterwards aborts the build
# if version, dependencies or the workflowWorkerModule interface do not match.
ARG PI_SUBAGENTS_VERSION=0.73.1-agw.2
COPY third_party/pi-subagents /opt/agw/pihome/npm/node_modules/pi-subagents
COPY images/agw-basis/pi-subagents-deps /tmp/pi-subagents-deps
RUN cd /tmp/pi-subagents-deps \
 && npm ci --omit=dev --omit=peer --ignore-scripts --no-audit --no-fund \
 && rm -rf /opt/agw/pihome/npm/node_modules/pi-subagents/node_modules \
 && mv node_modules /opt/agw/pihome/npm/node_modules/pi-subagents/node_modules \
 && PI_SUBAGENTS_VERSION=${PI_SUBAGENTS_VERSION} node /tmp/pi-subagents-deps/check.mjs \
 && cd / && rm -rf /tmp/pi-subagents-deps \
 && npm cache clean --force
# workflowScript does not run in the pi process but in the execution sandbox (E9): the worker
# comes from remote-worker.mjs, which starts it there through the socket. Fixed in the image,
# because the setting belongs to exactly this copy and this remote-worker.mjs; the agent cannot
# reach pi's environment. The guard in exec-bridge.ts only allows workflowScript if
# pi-subagents reports this path as workflowWorkerModule.
ENV PI_SUBAGENTS_WORKFLOW_WORKER=/opt/agw/ext/remote-worker.mjs
# Output files of pi-subagents live in the pi container, which the subagents' tools
# (execution sandbox) do not see: children return the result in the response, pi-subagents
# stores it itself (own change 4 in third_party/pi-subagents/VENDORED.md).
ENV PI_SUBAGENTS_OUTPUT_INLINE=1
# pi reads skills at startup (list in the system prompt); the agent reads the content with read,
# i.e. in the execution sandbox, which has the same files.
COPY images/agw-basis/skills /opt/agw/skills
COPY --from=build /out/agw-exec /usr/local/bin/agw-exec
# /workspace is an empty, read-only directory here: pi's working directory (the
# system prompt names it), but without the agent's files.
RUN useradd --uid 10001 --no-create-home --home-dir /home/agent --shell /usr/sbin/nologin agent \
 && mkdir -p /workspace /agent /run/agw /home/agent && chown agent:agent /agent /home/agent
# No shell (P3): pi needs none, and a tool that misses the redirection cannot run anything
# here. Must be the last RUN step.
RUN rm -f /bin/sh /bin/bash /bin/dash /bin/rbash /usr/bin/sh /usr/bin/bash /usr/bin/dash /usr/bin/rbash
LABEL org.opencontainers.image.title="agw-pi" agwpoc.pi-version="${PI_VERSION}" agwpoc.pi-subagents-version="${PI_SUBAGENTS_VERSION}" agwpoc.rpiv-todo-version="${RPIV_TODO_VERSION}" agwpoc.searxng-suite-version="${SEARXNG_SUITE_VERSION}" agwpoc.pi-intercom-version="${PI_INTERCOM_VERSION}"
ENV HOME=/home/agent
USER agent
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/agw-exec", "pi-entry"]

# ---------------------------------------------------------------------------
# Execution sandbox: Debian, Node (for the agent's node/npm), Python and common tools.
FROM node:24-bookworm-slim AS exec
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      python3 python3-pip python3-venv curl jq ripgrep git ca-certificates procps \
      unzip zip tar gzip xz-utils bzip2 file \
      poppler-utils binutils \
 && rm -rf /var/lib/apt/lists/* \
 && pdfinfo -v 2>&1 | head -1 && strings --version | head -1
# Common data packages preinstalled: internet is off in the sandbox by default,
# so pip install mostly does not work. System-wide, because ~/.local is a tmpfs at runtime.
# matplotlib draws without a display (Agg); the agent saves charts as PNG and shows
# them via Markdown (skill charts). The font cache is created on the first import in
# ~/.cache/matplotlib (tmpfs, writable, about 0.5 s).
RUN pip3 install --no-cache-dir --break-system-packages numpy pandas matplotlib jinja2 plotly kaleido openpyxl \
 && MPLBACKEND=Agg python3 -c "import numpy, pandas, matplotlib.pyplot"
ENV MPLBACKEND=Agg
# Typst (pinned version) and a fixed selection of packages. The packages are
# loaded at build time into a cache that is read-only at runtime: exactly these
# work in the sandbox, others cannot be fetched.
ARG TYPST_VERSION=0.14.2
ARG TARGETARCH
COPY images/agw-basis/typst/packages.txt /opt/typst/packages.txt
RUN case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      arm64) T=aarch64 ;; amd64) T=x86_64 ;; *) echo "architecture not supported" >&2; exit 1 ;; \
    esac \
 && curl -fsSL "https://github.com/typst/typst/releases/download/v${TYPST_VERSION}/typst-${T}-unknown-linux-musl.tar.xz" \
    | tar -xJ -C /tmp \
 && install -m 0755 /tmp/typst-${T}-unknown-linux-musl/typst /usr/local/bin/typst \
 && rm -rf /tmp/typst-* \
 && cd /tmp && grep -vE '^\s*(#|$)' /opt/typst/packages.txt \
    | awk '{print "#import \"@preview/" $1 "\""}' > warm.typ && echo "ok" >> warm.typ \
 && TYPST_PACKAGE_CACHE_PATH=/opt/typst/packages typst compile warm.typ warm.pdf \
 && rm -f warm.typ warm.pdf \
 && typst --version
ENV TYPST_PACKAGE_CACHE_PATH=/opt/typst/packages
# Mermaid CLI (mmdc) for diagrams as files (PNG, SVG, PDF), e.g. for artifacts and Typst; the web UI
# renders mermaid blocks itself. Same Mermaid major version as in the web UI. Chromium comes from
# Debian instead of Puppeteer's download. The mmdc command is a wrapper that passes the
# Puppeteer settings: --no-sandbox, because the container is the boundary and Chromium's
# own sandbox would need namespaces; --disable-dev-shm-usage, because /dev/shm has only 64 MB.
ARG MERMAID_CLI_VERSION=12.0.0
RUN apt-get update \
 && apt-get install -y --no-install-recommends chromium fonts-liberation fonts-noto-core \
 && rm -rf /var/lib/apt/lists/* \
 && PUPPETEER_SKIP_DOWNLOAD=1 npm install -g --no-fund --no-audit @mermaid-js/mermaid-cli@${MERMAID_CLI_VERSION} \
 && npm cache clean --force \
 && mkdir -p /opt/agw/mmdc \
 && printf '%s\n' '{"executablePath":"/usr/bin/chromium","args":["--no-sandbox","--disable-gpu","--disable-dev-shm-usage"]}' \
      > /opt/agw/mmdc/puppeteer.json \
 && mv /usr/local/bin/mmdc /usr/local/bin/mmdc-real \
 && printf '%s\n' '#!/bin/sh' 'exec /usr/local/bin/mmdc-real -p /opt/agw/mmdc/puppeteer.json "$@"' > /usr/local/bin/mmdc \
 && chmod 0755 /usr/local/bin/mmdc \
 && printf 'flowchart LR\n  A["Größe prüfen"] --> B["fertig"]\n' > /tmp/t.mmd \
 && HOME=/tmp mmdc -q -i /tmp/t.mmd -o /tmp/t.png && test -s /tmp/t.png && rm -f /tmp/t.mmd /tmp/t.png
# The same Chromium writes images for plotly (kaleido 1.x finds the browser via BROWSER_PATH).
ENV BROWSER_PATH=/usr/bin/chromium
# fd for the find tool (M2): pi calls fd with fixed arguments and otherwise downloads it itself;
# here is a pinned version (the one pi 0.87.1 pins for darwin/x64).
ARG FD_VERSION=10.3.0
RUN case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      arm64) T=aarch64 ;; amd64) T=x86_64 ;; *) echo "architecture not supported" >&2; exit 1 ;; \
    esac \
 && curl -fsSL "https://github.com/sharkdp/fd/releases/download/v${FD_VERSION}/fd-v${FD_VERSION}-${T}-unknown-linux-musl.tar.gz" \
    | tar -xz -C /tmp \
 && install -m 0755 /tmp/fd-v${FD_VERSION}-${T}-unknown-linux-musl/fd /usr/local/bin/fd \
 && rm -rf /tmp/fd-* \
 && fd --version
# Dedicated unprivileged user; the home is in the tmpfs at runtime.
RUN useradd --uid 10001 --create-home --shell /bin/bash agent \
 && mkdir -p /workspace /run/agw && chown agent:agent /workspace
COPY --from=build /out/agw-artifact /usr/local/bin/agw-artifact
COPY --from=build /out/agw-exec /usr/local/bin/agw-exec
RUN ln -s agw-artifact /usr/local/bin/agw-internet \
 && ln -s agw-artifact /usr/local/bin/agw-platform
COPY images/agw-basis/skills /opt/agw/skills
# Runtime for workflowScript (pi-subagents worker in its own Node process) together with the parser
# the worker loads (acorn from the copy of pi-subagents, so in the same version).
COPY images/agw-basis/workflow /opt/agw/workflow
COPY --from=pi /opt/agw/pihome/npm/node_modules/pi-subagents/node_modules/acorn /opt/agw/workflow/node_modules/acorn
# The agent's npm install -g lands in ~/.local (tmpfs, in PATH); /usr/local is read-only.
ENV NPM_CONFIG_PREFIX=/home/agent/.local NPM_CONFIG_UPDATE_NOTIFIER=false \
    PIP_BREAK_SYSTEM_PACKAGES=1 PIP_USER=1 \
    PATH=/home/agent/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
LABEL org.opencontainers.image.title="agw-basis"
USER agent
WORKDIR /workspace
# PID 1 does nothing and reaps orphaned processes; the orchestrator starts tools via
# agw-exec serve (via docker exec as root without capabilities except SETUID/SETGID).
ENTRYPOINT ["/usr/local/bin/agw-exec", "idle"]

# ---------------------------------------------------------------------------
# Tests only (TestBridgeParity): execution sandbox plus pi and the extensions, so that pi's
# built-in tools and the redirection run on the same file tree. Never for a slot.
FROM exec AS parity
COPY --from=pi /usr/local/lib/node_modules/@earendil-works /opt/pi/node_modules/@earendil-works
COPY --from=pi /opt/agw/ext /opt/pi/ext
# The copy of pi-subagents, so that guard.test.mjs checks the workflowWorkerModule interface on the
# real module (with and without PI_SUBAGENTS_WORKFLOW_WORKER; the variable is not set here),
# plus the extensions at the pi image's path for the case with the variable.
COPY --from=pi /opt/agw/pihome/npm/node_modules/pi-subagents /opt/pi/node_modules/pi-subagents
COPY --from=pi /opt/agw/ext /opt/agw/ext
COPY images/agw-basis/test /opt/pi/test
LABEL org.opencontainers.image.title="agw-parity"
