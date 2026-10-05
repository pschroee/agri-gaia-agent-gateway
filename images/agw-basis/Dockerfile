# Abbilder eines Platzes (E9). Baukontext ist poc/ (wegen agw-artifact und agw-exec).
#
#   --target pi    agwpoc/agw-pi      Container von pi: pi, pi-subagents, rpiv-todo, Extensions.
#                                     Ohne Shell, ohne Python, ohne /workspace-Inhalt.
#   --target exec  agwpoc/agw-basis   Ausführungs-Sandbox: Python, Typst, Werkzeuge, agw-artifact.
#                                     Ohne pi. Hier laufen alle Werkzeuge des Agenten.

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
# Container von pi. Node.js LTS (24, aktive LTS seit Oktober 2025); pi verlangt mindestens 22.19.
FROM node:24-bookworm-slim AS pi
ARG PI_VERSION=0.87.1
RUN npm install -g @earendil-works/pi-coding-agent@${PI_VERSION} \
 && npm cache clean --force
# Aufgabenliste des Agenten: Werkzeug `todo` aus rpiv-todo (Wahl begründet in poc/README.md,
# „Aufgaben“). Der Zustand steckt in den Werkzeugergebnissen der pi-Sitzung, nicht in Dateien.
# Vor pi-subagents, weil `pi install` per npm im selben Präfix nicht aufgeführte Pakete entfernt.
ARG RPIV_TODO_VERSION=2.11.0
# Websuche: web_search und web_extract aus pi-searxng-suite (MIT, unverändert). Die Anfragen gehen
# über den Web-Proxy des Orchestrators an den eigenen SearXNG bzw. ins Web, nur mit Internet;
# web-gate.ts blendet die Werkzeuge ohne Internet aus (poc/README.md, „Websuche“).
ARG SEARXNG_SUITE_VERSION=0.2.3
# Nachrichten zwischen den Sitzungen eines Platzes (Haupt- und Subagenten): pi-intercom (MIT),
# Werkzeug intercom; der Broker läuft im Container von pi über einen Unix-Socket im Home. pi install
# holt Paket und Abhängigkeiten (tsx); die Dateien des Pakets ersetzt danach die eigene Kopie
# third_party/pi-intercom (Änderung: VENDORED.md).
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
# Subagenten: eigene Kopie von pi-subagents im Repo (third_party/pi-subagents, 0.73.1 mit einer
# Änderung, siehe third_party/pi-subagents/VENDORED.md): Den Worker für workflowScript nimmt sie aus
# dem Modul in PI_SUBAGENTS_WORKFLOW_WORKER, sonst aus node:worker_threads. Sie liegt, wo
# `pi install` sie anlegen würde; zur Laufzeit lädt pi sie ausdrücklich per -e. Die
# Laufzeitabhängigkeiten kommen per `npm ci` mit fester Integrität aus
# images/agw-basis/pi-subagents-deps (package-lock.json); die Prüfung danach bricht den Bau ab,
# wenn Version, Abhängigkeiten oder die Schnittstelle workflowWorkerModule nicht stimmen.
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
# workflowScript läuft nicht im pi-Prozess, sondern in der Ausführungs-Sandbox (E9): Der Worker
# kommt aus remote-worker.mjs, das ihn über den Socket dort startet. Fest im Abbild, weil die
# Einstellung zu genau dieser Kopie und diesem remote-worker.mjs gehört; der Agent erreicht die
# Umgebung von pi nicht. Der Wächter in exec-bridge.ts gibt workflowScript nur frei, wenn
# pi-subagents diesen Pfad als workflowWorkerModule meldet.
ENV PI_SUBAGENTS_WORKFLOW_WORKER=/opt/agw/ext/remote-worker.mjs
# Ausgabedateien von pi-subagents liegen im Container von pi, den die Werkzeuge der Subagenten
# (Ausführungs-Sandbox) nicht sehen: Kinder geben das Ergebnis in der Antwort zurück, pi-subagents
# speichert es selbst (eigene Änderung 4 in third_party/pi-subagents/VENDORED.md).
ENV PI_SUBAGENTS_OUTPUT_INLINE=1
# Skills liest pi beim Start (Liste im Systemprompt); den Inhalt liest der Agent mit read, also
# in der Ausführungs-Sandbox, die dieselben Dateien hat.
COPY images/agw-basis/skills /opt/agw/skills
COPY --from=build /out/agw-exec /usr/local/bin/agw-exec
# /workspace ist hier ein leeres, schreibgeschütztes Verzeichnis: pis Arbeitsverzeichnis (der
# Systemprompt nennt es), aber ohne Dateien des Agenten.
RUN useradd --uid 10001 --no-create-home --home-dir /home/agent --shell /usr/sbin/nologin agent \
 && mkdir -p /workspace /agent /run/agw /home/agent && chown agent:agent /agent /home/agent
# Keine Shell (P3): pi braucht keine, und ein Werkzeug, das die Umleitung verfehlt, kann hier
# nichts ausführen. Muss der letzte Schritt mit RUN sein.
RUN rm -f /bin/sh /bin/bash /bin/dash /bin/rbash /usr/bin/sh /usr/bin/bash /usr/bin/dash /usr/bin/rbash
LABEL org.opencontainers.image.title="agw-pi" agwpoc.pi-version="${PI_VERSION}" agwpoc.pi-subagents-version="${PI_SUBAGENTS_VERSION}" agwpoc.rpiv-todo-version="${RPIV_TODO_VERSION}" agwpoc.searxng-suite-version="${SEARXNG_SUITE_VERSION}" agwpoc.pi-intercom-version="${PI_INTERCOM_VERSION}"
ENV HOME=/home/agent
USER agent
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/agw-exec", "pi-entry"]

# ---------------------------------------------------------------------------
# Ausführungs-Sandbox: Debian, Node (für node/npm des Agenten), Python und übliche Werkzeuge.
FROM node:24-bookworm-slim AS exec
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      python3 python3-pip python3-venv curl jq ripgrep git ca-certificates procps \
      unzip zip tar gzip xz-utils bzip2 file \
      poppler-utils binutils \
 && rm -rf /var/lib/apt/lists/* \
 && pdfinfo -v 2>&1 | head -1 && strings --version | head -1
# Übliche Datenpakete vorinstalliert: Internet ist in der Sandbox standardmäßig aus,
# pip install geht also meist nicht. Systemweit, weil ~/.local zur Laufzeit ein tmpfs ist.
# matplotlib zeichnet ohne Bildschirm (Agg); Diagramme speichert der Agent als PNG und zeigt
# sie per Markdown (Skill diagramme). Der Schriften-Cache entsteht beim ersten Import in
# ~/.cache/matplotlib (tmpfs, beschreibbar, rund 0,5 s).
RUN pip3 install --no-cache-dir --break-system-packages numpy pandas matplotlib jinja2 plotly kaleido openpyxl \
 && MPLBACKEND=Agg python3 -c "import numpy, pandas, matplotlib.pyplot"
ENV MPLBACKEND=Agg
# Typst (festgelegte Version) und eine feste Auswahl an Paketen. Die Pakete werden
# beim Bau in einen Cache geladen, der zur Laufzeit schreibgeschützt ist: In der
# Sandbox funktionieren genau diese, andere lassen sich nicht nachladen.
ARG TYPST_VERSION=0.14.2
ARG TARGETARCH
COPY images/agw-basis/typst/packages.txt /opt/typst/packages.txt
RUN case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      arm64) T=aarch64 ;; amd64) T=x86_64 ;; *) echo "Architektur nicht unterstützt" >&2; exit 1 ;; \
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
# Mermaid-CLI (mmdc) für Diagramme als Datei (PNG, SVG, PDF), etwa für Artefakte und Typst; die Web-UI
# zeichnet mermaid-Blöcke selbst. Gleiche Mermaid-Hauptversion wie in der Web-UI. Chromium kommt aus
# Debian statt aus dem Download von Puppeteer. Der Aufruf mmdc ist ein Wrapper, der die
# Puppeteer-Einstellungen mitgibt: --no-sandbox, weil der Container die Grenze ist und Chromiums
# eigene Sandbox Namespaces bräuchte; --disable-dev-shm-usage, weil /dev/shm nur 64 MB hat.
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
# Dasselbe Chromium schreibt für plotly Bilder (kaleido 1.x sucht den Browser über BROWSER_PATH).
ENV BROWSER_PATH=/usr/bin/chromium
# fd für das Werkzeug find (M2): pi ruft fd mit festen Argumenten auf und lädt es sonst selbst
# herunter; hier liegt eine feste Version (die, auf die pi 0.87.1 für darwin/x64 festgelegt ist).
ARG FD_VERSION=10.3.0
RUN case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      arm64) T=aarch64 ;; amd64) T=x86_64 ;; *) echo "Architektur nicht unterstützt" >&2; exit 1 ;; \
    esac \
 && curl -fsSL "https://github.com/sharkdp/fd/releases/download/v${FD_VERSION}/fd-v${FD_VERSION}-${T}-unknown-linux-musl.tar.gz" \
    | tar -xz -C /tmp \
 && install -m 0755 /tmp/fd-v${FD_VERSION}-${T}-unknown-linux-musl/fd /usr/local/bin/fd \
 && rm -rf /tmp/fd-* \
 && fd --version
# Eigener unprivilegierter Nutzer; das Home liegt zur Laufzeit im tmpfs.
RUN useradd --uid 10001 --create-home --shell /bin/bash agent \
 && mkdir -p /workspace /run/agw && chown agent:agent /workspace
COPY --from=build /out/agw-artifact /usr/local/bin/agw-artifact
COPY --from=build /out/agw-exec /usr/local/bin/agw-exec
RUN ln -s agw-artifact /usr/local/bin/agw-internet \
 && ln -s agw-artifact /usr/local/bin/agw-platform
COPY images/agw-basis/skills /opt/agw/skills
# Laufzeit für workflowScript (Worker von pi-subagents in eigenem Node-Prozess) samt dem Parser,
# den der Worker lädt (acorn aus der Kopie von pi-subagents, also in derselben Version).
COPY images/agw-basis/workflow /opt/agw/workflow
COPY --from=pi /opt/agw/pihome/npm/node_modules/pi-subagents/node_modules/acorn /opt/agw/workflow/node_modules/acorn
# npm install -g des Agenten landet in ~/.local (tmpfs, im PATH); /usr/local ist schreibgeschützt.
ENV NPM_CONFIG_PREFIX=/home/agent/.local NPM_CONFIG_UPDATE_NOTIFIER=false \
    PIP_BREAK_SYSTEM_PACKAGES=1 PIP_USER=1 \
    PATH=/home/agent/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
LABEL org.opencontainers.image.title="agw-basis"
USER agent
WORKDIR /workspace
# PID 1 tut nichts und räumt verwaiste Prozesse ab; Werkzeuge startet der Orchestrator über
# agw-exec serve (per docker exec als root ohne Capabilities außer SETUID/SETGID).
ENTRYPOINT ["/usr/local/bin/agw-exec", "idle"]

# ---------------------------------------------------------------------------
# Nur für Tests (TestBridgeParity): Ausführungs-Sandbox plus pi und die Extensions, damit pis
# eingebaute Werkzeuge und die Umleitung am selben Dateibaum laufen. Nie für einen Platz.
FROM exec AS parity
COPY --from=pi /usr/local/lib/node_modules/@earendil-works /opt/pi/node_modules/@earendil-works
COPY --from=pi /opt/agw/ext /opt/pi/ext
# Die Kopie von pi-subagents, damit guard.test.mjs die Schnittstelle workflowWorkerModule am echten
# Modul prüft (mit und ohne PI_SUBAGENTS_WORKFLOW_WORKER; hier ist die Variable nicht gesetzt),
# dazu die Extensions am Pfad des pi-Abbilds für den Fall mit Variable.
COPY --from=pi /opt/agw/pihome/npm/node_modules/pi-subagents /opt/pi/node_modules/pi-subagents
COPY --from=pi /opt/agw/ext /opt/agw/ext
COPY images/agw-basis/test /opt/pi/test
LABEL org.opencontainers.image.title="agw-parity"
