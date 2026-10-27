# Images of a slot (E9). Build context is poc/ (because of agw-artifact and agw-exec).
#
#   --target pi    agwpoc/agw-pi      pi container: pi, pi-subagents, rpiv-todo, extensions.
#                                     No shell, no Python, no /workspace content.
#   --target exec  agwpoc/agw-basis   Execution sandbox: Python, Typst, tools, agw-artifact.
#                                     No pi. All of the agent's tools run here.

FROM golang:1.26-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/agw-artifact ./cmd/agw-artifact
COPY cmd/agw-exec ./cmd/agw-exec
COPY internal/execproto ./internal/execproto
COPY internal/platform ./internal/platform
COPY internal/toolset ./internal/toolset
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agw-artifact ./cmd/agw-artifact \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agw-exec ./cmd/agw-exec

# ---------------------------------------------------------------------------
# pi container. Node.js LTS (24, active LTS since October 2025); pi requires at least 22.19.
FROM node:24-bookworm-slim@sha256:55fb5f5a527ea2c5c4a95f963e48ef90aa38c7b346054fdc8ee9957a2b55d205 AS pi
ARG PI_VERSION=0.87.1
RUN npm install -g @earendil-works/pi-coding-agent@${PI_VERSION} \
 && npm cache clean --force
# The agent's task list: tool `todo` from rpiv-todo (choice justified in docs/design.md,
# "Tasks"). The state lives in the tool results of the pi session, not in files.
ARG RPIV_TODO_VERSION=2.11.0
# Web search: web_search and web_extract from searxng-suite (MIT, unchanged; published as pi-searxng-suite until the
# author unpublished it on 2026-10-25 and released the same code as @blazer2k/searxng-suite, sources under src/). The requests go
# through the orchestrator's web proxy to our own SearXNG or to the web, only with internet;
# ext/web-tools.ts loads it and sets fuller tool texts; web-gate.ts hides the tools without internet (docs/design.md, "Web search").
# Our own copy third_party/searxng-suite, because the package can be unpublished at any time (#242).
ARG SEARXNG_SUITE_VERSION=0.2.3
# Messages between the sessions of a slot (main agent and subagents): pi-intercom (MIT),
# tool intercom; the broker runs in the pi container over a Unix socket in the home. Our own copy
# third_party/pi-intercom (change: VENDORED.md); only its dependency tsx comes from npm.
ARG PI_INTERCOM_VERSION=0.15.0
# Where `pi install` would put the packages: /opt/agw/pihome/npm. Nothing is fetched by `pi install` any more (#242):
# rpiv-todo and the dependencies of the two copies below come from images/agw-basis/pi-packages (package-lock.json
# with integrity, npm ci), the copies from third_party. At runtime pi loads the extensions explicitly via -e.
COPY images/agw-basis/pi-packages /opt/agw/pihome/npm
RUN cd /opt/agw/pihome/npm \
 && npm ci --omit=dev --ignore-scripts --no-audit --no-fund \
 && npm cache clean --force
COPY third_party/searxng-suite /opt/agw/pihome/npm/node_modules/@blazer2k/searxng-suite
COPY third_party/pi-intercom /opt/agw/pihome/npm/node_modules/pi-intercom
RUN grep -q '"version": "0.15.0"' /opt/agw/pihome/npm/node_modules/pi-intercom/package.json \
 && grep -q "PI_INTERCOM_REFUSE_WHEN_BUSY" /opt/agw/pihome/npm/node_modules/pi-intercom/index.ts \
 && node /opt/agw/pihome/npm/check.mjs \
 && rm /opt/agw/pihome/npm/check.mjs
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
# platform-exp is generated from platform here (X0.10): the experiment slots must not read which
# protection level they run under.
RUN node /opt/agw/skills/build-platform-exp.mjs && rm /opt/agw/skills/build-platform-exp.mjs
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
FROM node:24-bookworm-slim@sha256:55fb5f5a527ea2c5c4a95f963e48ef90aa38c7b346054fdc8ee9957a2b55d205 AS exec
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      python3 python3-pip python3-venv curl jq ripgrep git ca-certificates procps \
      unzip zip tar gzip xz-utils bzip2 file \
      poppler-utils binutils strace \
 && rm -rf /var/lib/apt/lists/* \
 && pdfinfo -v 2>&1 | head -1 && strings --version | head -1 && strace -V | head -1
# Common data packages preinstalled: internet is off in the sandbox by default,
# so pip install mostly does not work. System-wide, because ~/.local is a tmpfs at runtime.
# matplotlib draws without a display (Agg); the agent saves charts as PNG and shows
# them via Markdown (skill charts). The font cache is created on the first import in
# ~/.cache/matplotlib (tmpfs, writable, about 0.5 s).
# All Python packages come from one lock with hashes (#242): images/agw-basis/python/requirements.lock lists every
# package, direct or transitive, with its exact version and the sha256 of its wheels (CPython 3.11, x86_64 and aarch64);
# --require-hashes makes pip refuse anything that is missing or different, --no-deps and --only-binary keep it from
# resolving or building. To bump see the header of the lock and python/hash-lock.py.
# onnxruntime (R0.15): the result checks run a model in a container of this image with it, and a rebuild must not pull
# another version (unpinned broke the build once, see CLAUDE.md). The image has run with 1.30.0 (the version
# markitdown's dependencies needed) since October 2026; the earlier pin 1.20.1 was overridden by it and is gone. Side
# effect: the agent can load ONNX models in its sandbox as well.
# plotly, kaleido and choreographer (#229): the Chromium network check depends on kaleido's switches and its MathJax
# host. nbformat, nbclient and ipykernel (issue #193): the same versions as the notebook server image, so that a
# notebook tested here runs the same there. markitdown converts documents to Markdown for the agent (skill documents):
# Word (docx), Excel (xlsx, xls), PowerPoint (pptx), Outlook (msg), PDF as a fallback to pdftotext; HTML, CSV, EPUB,
# JSON and XML need no extra. Internet is off in the sandbox, so it has to be in the image. The check converts a
# generated workbook.
ARG MARKITDOWN_VERSION=0.1.8
COPY images/agw-basis/python/requirements.lock /tmp/requirements.lock
RUN pip3 install --no-cache-dir --break-system-packages --require-hashes --no-deps --only-binary :all: \
      -r /tmp/requirements.lock \
 && rm -f /tmp/requirements.lock \
 && pip3 check \
 && MPLBACKEND=Agg python3 -c "import numpy, pandas, matplotlib.pyplot" \
 && python3 -c "import onnxruntime, PIL; print('onnxruntime', onnxruntime.__version__)" \
 && python3 -c "import nbformat, nbclient, ipykernel; print('nbclient', nbclient.__version__)" \
 && cd /tmp \
 && python3 -c "import openpyxl; wb = openpyxl.Workbook(); wb.active.append(['Pen', 'Pigs']); wb.active.append(['B3', 42]); wb.save('t.xlsx')" \
 && markitdown t.xlsx | grep -q '| B3 | 42 |' \
 && rm -f t.xlsx \
 && markitdown --version | grep -q "${MARKITDOWN_VERSION}"
ENV MPLBACKEND=Agg
# agw-run-notebook is the starter the notebook server uses, too (one file, embedded in the gateway).
COPY internal/notebook/launcher/agw_run_notebook.py /usr/local/bin/agw-run-notebook
RUN chmod 0755 /usr/local/bin/agw-run-notebook
# Typst (pinned version) and a fixed selection of packages. The packages are
# loaded at build time into a cache that is read-only at runtime: exactly these
# work in the sandbox, others cannot be fetched.
# The tarball is checked against the sha256 of the release asset (#242), per architecture.
ARG TYPST_VERSION=0.14.2
ARG TYPST_SHA256_AMD64=a6044cbad2a954deb921167e257e120ac0a16b20339ec01121194ff9d394996d
ARG TYPST_SHA256_ARM64=491b101aa40a3a7ea82a3f8a6232cabb4e6a7e233810082e5ac812d43fdcd47a
ARG TARGETARCH
COPY images/agw-basis/typst/packages.txt /opt/typst/packages.txt
RUN case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      arm64) T=aarch64 S="${TYPST_SHA256_ARM64}" ;; amd64) T=x86_64 S="${TYPST_SHA256_AMD64}" ;; \
      *) echo "architecture not supported" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/typst.tar.xz "https://github.com/typst/typst/releases/download/v${TYPST_VERSION}/typst-${T}-unknown-linux-musl.tar.xz" \
 && echo "${S}  /tmp/typst.tar.xz" | sha256sum -c - \
 && tar -xJf /tmp/typst.tar.xz -C /tmp \
 && install -m 0755 /tmp/typst-${T}-unknown-linux-musl/typst /usr/local/bin/typst \
 && rm -rf /tmp/typst-* /tmp/typst.tar.xz \
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
# The whole dependency tree comes from images/agw-basis/mmdc/package-lock.json (npm ci, integrity; #242), installed
# into the layout `npm install -g` would give (the package in /usr/local/lib/node_modules/@mermaid-js/mermaid-cli with its
# dependencies in its own node_modules), so the package list the gateway reports shows one package, not hundreds.
# puppeteer is the peer dependency of mermaid-cli, pinned in the same package.json.
ARG MERMAID_CLI_VERSION=12.0.0
COPY images/agw-basis/mmdc/package.json images/agw-basis/mmdc/package-lock.json /tmp/mmdc/
RUN apt-get update \
 && apt-get install -y --no-install-recommends chromium fonts-liberation fonts-noto-core \
 && rm -rf /var/lib/apt/lists/* \
 && cd /tmp/mmdc \
 && PUPPETEER_SKIP_DOWNLOAD=1 npm ci --omit=dev --ignore-scripts --no-fund --no-audit \
 && mkdir -p /usr/local/lib/node_modules/@mermaid-js \
 && mv node_modules/@mermaid-js/mermaid-cli /usr/local/lib/node_modules/@mermaid-js/mermaid-cli \
 && mv node_modules /usr/local/lib/node_modules/@mermaid-js/mermaid-cli/node_modules \
 && chmod 0755 /usr/local/lib/node_modules/@mermaid-js/mermaid-cli/src/cli.js \
 && ln -s ../lib/node_modules/@mermaid-js/mermaid-cli/src/cli.js /usr/local/bin/mmdc-real \
 && grep -q "\"version\": \"${MERMAID_CLI_VERSION}\"" /usr/local/lib/node_modules/@mermaid-js/mermaid-cli/package.json \
 && cd / && rm -rf /tmp/mmdc \
 && npm cache clean --force \
 && mkdir -p /opt/agw/mmdc \
 && printf '%s\n' '{"executablePath":"/usr/bin/chromium","args":["--no-sandbox","--disable-gpu","--disable-dev-shm-usage"]}' \
      > /opt/agw/mmdc/puppeteer.json \
 && printf '%s\n' '#!/bin/sh' 'exec /usr/local/bin/mmdc-real -p /opt/agw/mmdc/puppeteer.json "$@"' > /usr/local/bin/mmdc \
 && chmod 0755 /usr/local/bin/mmdc \
 && printf 'flowchart LR\n  A["Größe prüfen"] --> B["fertig"]\n' > /tmp/t.mmd \
 && HOME=/tmp mmdc -q -i /tmp/t.mmd -o /tmp/t.png && test -s /tmp/t.png && rm -f /tmp/t.mmd /tmp/t.png
# mermaid-check: mmdc with the web UI's settings (securityLevel strict, no HTML labels) plus the rule "no HTML
# tags in a diagram", so the agent finds before answering what the UI would not render (issue #59, skill
# mermaid). The build check renders one diagram and expects a parse error and an HTML tag to fail.
COPY images/agw-basis/mmdc/ui-config.json /opt/agw/mmdc/ui-config.json
COPY images/agw-basis/mmdc/mermaid-check /usr/local/bin/mermaid-check
RUN chmod 0644 /opt/agw/mmdc/ui-config.json && chmod 0755 /usr/local/bin/mermaid-check \
 && printf 'flowchart LR\n  A["Größe (m²)"] --> B["fertig"]\n' | HOME=/tmp mermaid-check - \
 && ! printf 'flowchart LR\n  A[Training (CPU)] --> B\n' | HOME=/tmp mermaid-check - 2>/dev/null \
 && ! printf 'flowchart LR\n  A[a<br/>b] --> B\n' | HOME=/tmp mermaid-check - 2>/dev/null
# The same Chromium writes images for plotly (kaleido 1.x finds the browser via BROWSER_PATH).
ENV BROWSER_PATH=/usr/bin/chromium
# Chromium without background networking, failing closed (issue #229): one file read by Debian's launcher for
# every start, plus a managed policy (sign-in, network time, sync, metrics, component updates). netcheck.py proves it
# at build time and stays in the image for diagnosis. Chromium 155 still asks Google hosts at start, not every time
# the same ones (update.googleapis.com from the on-device model installer despite --disable-component-update and the
# policy; under load also accounts.google.com or android.clients.google.com), and kaleido's page asks
# cdnjs.cloudflare.com (MathJax) and start.duckduckgo.com (Debian's default search). They end at the dead proxy port
# and never reach the relay, so --allow names them; any other host, or anything at the env proxy, fails the build.
COPY images/agw-basis/chromium/agw-offline /etc/chromium.d/agw-offline
COPY images/agw-basis/chromium/agw-offline.json /etc/chromium/policies/managed/agw-offline.json
COPY images/agw-basis/chromium/netcheck.py /opt/agw/chromium/netcheck.py
RUN rm -f /etc/chromium.d/extensions /etc/chromium.d/apikeys \
 && chmod 0644 /etc/chromium.d/agw-offline /etc/chromium/policies/managed/agw-offline.json /opt/agw/chromium/netcheck.py \
 && cd /tmp && printf 'flowchart LR\n  A["Größe"] --> B["fertig"]\n' > n.mmd \
 && HOME=/tmp python3 /opt/agw/chromium/netcheck.py --allow .google.com,.googleapis.com -- mmdc -q -i n.mmd -o n.png \
 && HOME=/tmp python3 /opt/agw/chromium/netcheck.py --allow .google.com,.googleapis.com -- mermaid-check n.mmd \
 && HOME=/tmp python3 /opt/agw/chromium/netcheck.py --allow .google.com,.googleapis.com -- timeout 60 chromium --headless --no-sandbox --disable-gpu --dump-dom about:blank \
 && HOME=/tmp python3 /opt/agw/chromium/netcheck.py --allow .google.com,.googleapis.com,cdnjs.cloudflare.com,start.duckduckgo.com -- python3 -c "import plotly.graph_objects as go; go.Figure(go.Bar(y=[1, 2])).write_image('/tmp/n-plotly.png')" \
 && test -s n.png && test -s n-plotly.png && rm -f n.mmd n.png n-plotly.png
# fd for the find tool (M2): pi calls fd with fixed arguments and otherwise downloads it itself;
# here is a pinned version (the one pi 0.87.1 pins for darwin/x64).
# The tarball is checked against the sha256 of the release asset (#242), per architecture.
ARG FD_VERSION=10.3.0
ARG FD_SHA256_AMD64=2b6bfaae8c48f12050813c2ffe1884c61ea26e750d803df9c9114550a314cd14
ARG FD_SHA256_ARM64=996b9b1366433b211cb3bbedba91c9dbce2431842144d925428ead0adf32020b
RUN case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      arm64) T=aarch64 S="${FD_SHA256_ARM64}" ;; amd64) T=x86_64 S="${FD_SHA256_AMD64}" ;; \
      *) echo "architecture not supported" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/fd.tar.gz "https://github.com/sharkdp/fd/releases/download/v${FD_VERSION}/fd-v${FD_VERSION}-${T}-unknown-linux-musl.tar.gz" \
 && echo "${S}  /tmp/fd.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/fd.tar.gz -C /tmp \
 && install -m 0755 /tmp/fd-v${FD_VERSION}-${T}-unknown-linux-musl/fd /usr/local/bin/fd \
 && rm -rf /tmp/fd-* /tmp/fd.tar.gz \
 && fd --version
# Dedicated unprivileged user; the home is in the tmpfs at runtime.
RUN useradd --uid 10001 --create-home --shell /bin/bash agent \
 && mkdir -p /workspace /run/agw && chown agent:agent /workspace
COPY --from=build /out/agw-artifact /usr/local/bin/agw-artifact
COPY --from=build /out/agw-exec /usr/local/bin/agw-exec
RUN ln -s agw-artifact /usr/local/bin/agw-internet \
 && ln -s agw-artifact /usr/local/bin/agw-platform
COPY images/agw-basis/skills /opt/agw/skills
# Same generated skill as in the pi image (the agent reads it here).
RUN node /opt/agw/skills/build-platform-exp.mjs && rm /opt/agw/skills/build-platform-exp.mjs
# The check script of the result checks (R0): the gateway runs it in a sealed container of this image,
# with the model and the inputs on stdin. It is part of the image, so the agent can read it but not
# change it; the gateway alone decides what goes in.
COPY images/agw-basis/check /opt/agw/check
RUN chmod 0644 /opt/agw/check/predict.py
# Runtime for workflowScript (pi-subagents worker in its own Node process) together with the parser
# the worker loads (acorn from the copy of pi-subagents, so in the same version).
COPY images/agw-basis/workflow /opt/agw/workflow
COPY --from=pi /opt/agw/pihome/npm/node_modules/pi-subagents/node_modules/acorn /opt/agw/workflow/node_modules/acorn
# The agent's npm install -g lands in ~/.local (tmpfs, in PATH); /usr/local is read-only.
ENV NPM_CONFIG_PREFIX=/home/agent/.local NPM_CONFIG_UPDATE_NOTIFIER=false \
    PIP_BREAK_SYSTEM_PACKAGES=1 PIP_USER=1 \
    PATH=/home/agent/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
LABEL org.opencontainers.image.title="agw-basis" agwpoc.markitdown-version="${MARKITDOWN_VERSION}"
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
