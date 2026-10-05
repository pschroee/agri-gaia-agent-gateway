# PoC: Agent in isolierter Sandbox, Orchestrator in Go

> **Stand 29.09.2026, Entscheidung des Verfassers.** Dieser Ordner hält fest, wie der erste PoC
> gebaut wird. **Stufe 1 ist umgesetzt** (siehe *Stufe 1 (umgesetzt)* direkt unten); die
> Abschnitte danach beschreiben den Zielaufbau. Die Wahl von pi als Harness ist in
> [`docs/poc-pi.md`](../docs/poc-pi.md) begründet; die Rolle des PoC in der Arbeit steht in
> [`masterarbeit/gliederung.md`](../masterarbeit/gliederung.md) (5.1, 5.3, 5.4, 6.2, 6.3).
>
> **Nicht verwechseln:** `prototyp/` ist der Oberflächenentwurf, `poc/` das lauffähige System.

## Stufe 1 (umgesetzt)

Stand 29.09.2026. Lauffähig auf dem Mac mit Docker Desktop; getestet Ende zu Ende mit DeepSeek V4.1
Flash (`deepseek/deepseek-flash`). Seit dem 05.10.2026 mit Anbindung an die Plattform über MCP und CLI,
Token-Austausch je Chat und Uploads (siehe *Plattform-Anbindung (direkt)*). Die nächsten Schritte
(Delegation, REST-Variante, Chat in der Plattform) stehen in
[`plan-delegation-rest-plattform.md`](plan-delegation-rest-plattform.md).

### Starten

```bash
cd poc
./dev.sh init      # .env aus .env.example (Zufallswerte), Abbilder bauen, npm install
# DEEPSEEK_API_KEY in poc/.env eintragen (.env ist nie versioniert, .env.example schon)
./dev.sh start     # Sandbox-Abbild bauen, Orchestrator + Postgres + RustFS + Paket-Zwischenspeicher starten, mit Hot Reload;
                   # gibt den Anmeldelink der Web-UI aus (/login?token=…)
./dev.sh start --prod   # dasselbe mit dem festen Orchestrator-Abbild, ohne Hot Reload
./dev.sh status    # Dienste, Sandboxen, Pool
./dev.sh cli run "Schreibe ein Python-Skript …"     # CLI agw, Antwort gestreamt
./dev.sh test      # schnelle Tests, rund 25 s: Go (Unit, Postgres, -race) und Web (Vitest)
./dev.sh test --full  # zusätzlich Docker-Integration, S3 und Platztests, rund 5 min (vor dem Push)
./dev.sh e2e       # Ende-zu-Ende mit echtem Modell (Cent-Beträge), inkl. Auto-Kompaktierung
./dev.sh stop      # anhalten, Sandboxen abbauen, Daten bleiben
./dev.sh reset     # ALLES löschen (Volumes mit Chats und Artefakten, Netze, Abbilder); fragt nach
```

| Was | Adresse |
|---|---|
| Web-UI mit Hot Reload (nach `./dev.sh start`) | <http://127.0.0.1:18484>, Vite reicht `/api` und `/login` an den Orchestrator weiter |
| Web-UI und API | <http://127.0.0.1:18480>, einmal über `/login?token=<AGW_API_TOKEN>` anmelden (API-Vertrag: [`API.md`](API.md)); CLI und Tests senden das Token als `Bearer` |
| RustFS-Konsole | <http://127.0.0.1:18483> (Zugang aus `.env`) |
| Postgres | `127.0.0.1:18482`, Nutzer und Datenbank `agwpoc` |
| LLM-Proxy | `orchestrator:18481`, nur im Sandbox-Netz |
| Paket-Zwischenspeicher | `http://npm-cache:4873/` (Verdaccio) und `http://pip-cache:5000/index/` (proxpi), **nur aus einer Sandbox mit Internet**; keine Ports am Host |

**Hot Reload.** `./dev.sh start` betreibt den Orchestrator aus dem Quelltext: Der Container
(`golang:1.26-bookworm`, `compose.hot.yaml`) bindet `poc/` schreibgeschützt ein, und
`dev/go-hot.sh` baut bei jeder Änderung an Go-Dateien, `go.mod`/`go.sum` oder `schema.sql` neu
(mit Build-Cache im Volume rund 1 s) und startet den Orchestrator neu. Scheitert der Bau, läuft
die alte Fassung weiter. Ein Neustart lässt laufende Chats ruhen, sie lassen sich fortsetzen. Die
Web-UI liefert Vite auf `:18484` mit Hot Module Replacement; das Anmelde-Cookie gilt für beide
Ports, weil Cookies nicht nach Port unterscheiden. Unter `:18480` steht weiter die eingebettete
Fassung (Stand von `web/dist`). Die Betriebsart merkt sich `.dev/mode`, damit `stop`, `logs`,
`test` und `e2e` dieselben Compose-Dateien verwenden.

Alle Ports, Netze (`10.231.18.0/24` intern, `10.231.20.0/24` Egress, `10.231.21.0/24` Ausgang der Paket-Zwischenspeicher, je Platz ein `/28` aus `10.231.128.0/17`), Volumes und Namen tragen das Präfix `agwpoc` und sind
bewusst ungewöhnlich gewählt, damit nichts mit anderen Diensten kollidiert.

### Bestandteile

| Pfad | Inhalt |
|---|---|
| `cmd/orchestrator` | verdrahtet alles; räumt beim Start Sandboxen früherer Läufe ab und setzt aktive Chats auf *ruhend* |
| `cmd/agw` | CLI gegen die API (`run`, `chat …`, `pool`, `approve`, `watch` …) |
| `cmd/agw-artifact` | Hilfs-CLI **in** der Ausführungs-Sandbox, spricht HTTP über deren Socket |
| `cmd/agw-exec` | statischer Helfer in beiden Containern eines Platzes (E9): Überwacher der Ausführungs-Sandbox (samt Operation `workflow` für Workflow-Skripte), Entrypoint von pi ohne Shell, Sitzungen und Subagenten lesen |
| `internal/execproto`, `internal/execbox` | Protokoll zum Überwacher und Client des Orchestrators (eine Verbindung je Platz) |
| `internal/bgtask` | Hintergrundaufgaben eines Platzes (bash mit `run_in_background`): Start, Mitlesen der Ausgabe bis zum Ende, Abruf, Stopp |
| `internal/fakellm` | geskriptetes Modell für den Platz-Integrationstest (E9) |
| `internal/rpc` | pi-RPC-Client (Framing nur an `\n`, lange Zeilen, übergroße Zeilen werden gemeldet) |
| `internal/sandbox` | gehärtete Container über die Docker-API, `exec`, langlebiges `exec` für den Überwacher, Internet-Schalter |
| `internal/pool` | Warm-Pool je Variante, Einmalvergabe |
| `internal/worker` | ein Platz = zwei Container (pi, Ausführungs-Sandbox) + zwei Sockets; Werkzeugumfang je Variante |
| `internal/sock` | Sockets je Platz: bei pi `/tool/*` (E9, auch `/tool/workflow`) und `/mcp`, in der Ausführungs-Sandbox `/artifacts`, `/internet` und `/mcp` (`ping`, `list_artifacts`, `upload_artifact`) |
| `internal/chat` | Chats, Ereignisse, Sitzungen, Ruhen/Fortsetzen, Arbeitsbereich je Chat, Artefakte, Bestätigung |
| `internal/store` | Postgres (Chats, Nachrichten, Sitzung, Artefakte, Bestätigungen, Socket-Protokoll) |
| `internal/artifacts` | RustFS (S3) und Bestätigungs-Broker |
| `internal/llmproxy` | Proxy zum Modellanbieter mit Schlüssel und Modell-Positivliste |
| `internal/api` | HTTP-API, SSE, Sperre für Anfragen aus den Sandbox-Netzen, eingebettete UI |
| `web/` | React 19, Vite, Tailwind 4, shadcn/ui |
| `images/agw-basis` | ein Dockerfile, zwei Abbilder (E9). `agw-pi` (`--target pi`): Node.js 24 LTS, pi 0.87.1, pi-subagents als eigene Kopie aus `third_party/pi-subagents` (0.73.1-agw.2, Abhängigkeiten per `npm ci` aus `pi-subagents-deps/package-lock.json`), rpiv-todo 2.11.0, Extensions, Skills, `agw-exec`; **ohne Shell, ohne `git` und ohne Python**; `PI_SUBAGENTS_WORKFLOW_WORKER` zeigt auf `ext/remote-worker.mjs` (`workflowScript`, E9). `agw-basis` (`--target exec`, Ausführungs-Sandbox): Debian (`node:24-bookworm-slim`), Python 3.11 mit numpy, pandas, matplotlib, jinja2, plotly, openpyxl; Typst 0.14.2 mit fester Paketauswahl (`typst/packages.txt`); Skills `writing-typst`, `diagramme` (matplotlib, `MPLBACKEND=Agg`) u. a.; curl, jq, ripgrep, fd (für `find` wie pi), git, unzip, zip, tar, xz, bzip2, file, poppler-utils (`pdfinfo`, `pdftotext`, `pdftoppm`), binutils (`strings`); `agw-artifact`, `agw-exec`, Laufzeit für Workflow-Skripte (`workflow/runner.cjs`); **ohne pi**. Dazu das Test-Abbild `agw-parity` (`--target parity`, Ausführungs-Sandbox plus pi) nur für den Gleichlauftest |
| `third_party/pi-subagents` | eigene Kopie von pi-subagents 0.73.1 (MIT) als `0.73.1-agw.2` mit zwei Änderungen (Herkunft des Workers für `workflowScript` einstellbar; Ausgabe der Kinder in der Antwort statt als Datei); Herkunft, Änderung und Tests in `VENDORED.md`. Das pi-Abbild baut daraus, die Abhängigkeiten legt `images/agw-basis/pi-subagents-deps` mit Lockfile fest |
| `pkgcache/verdaccio.yaml` | Einstellungen des npm-Zwischenspeichers (nur lesen, keine Anmeldung, kein Veröffentlichen) |
| `models.json` | Modellkatalog: Modelle, Tarif (Spitzenzeiten), optional eigene Preise |
| `e2e/` | Ende-zu-Ende-Tests gegen den laufenden Stack (`./dev.sh e2e`) |

### Herkunft: was eigen ist und was übernommen

Für die Begutachtung soll jederzeit erkennbar sein, was im PoC eigene Arbeit ist und was aus
fremden Quellen stammt. Grundsatz: **Fremder Code kommt unverändert in einem eigenen Commit ins
Repo; eigene Änderungen daran folgen in einem getrennten Commit** und sind in einer Datei neben dem
Code beschrieben. Was nur beim Bau installiert wird, liegt nicht im Repo.

| Teil | Herkunft | Im Repo? | Eigene Änderungen |
|---|---|---|---|
| Orchestrator (`cmd/`, `internal/`), `agw-exec`, CLI `agw`, Web-UI (`web/src` außer `components/ui`), Extensions `exec-bridge.ts`, `mcp.ts`, `remote-worker.mjs`, `workflow/runner.cjs`, Abbilder, `compose.yaml`, `dev.sh`, alle Tests | **eigen** | ja | – |
| Hintergrundaufgaben (`internal/bgtask`, Operation `bg` in `agw-exec`, `bg_output`/`bg_stop` in `exec-bridge.ts`, `web/src/components/BackgroundTasks.tsx`) | **eigen**, Vorbild ist das Verhalten von Claude Code; die Erweiterung `pi-background-tasks` (npm, ISC) ist **nicht** übernommen (siehe *Hintergrundaufgaben*) | ja | – |
| `third_party/pi-subagents` | npm-Paket `pi-subagents` 0.73.1 (MIT) | ja | Original in Commit `d21c5c0`, eigene Änderungen in `8dfbd4a`; beschrieben in [`VENDORED.md`](third_party/pi-subagents/VENDORED.md), sichtbar mit `git diff d21c5c0 8dfbd4a -- poc/third_party/pi-subagents` |
| `web/src/components/ui/*` | Vorlagen von shadcn/ui (MIT), vom shadcn-CLI erzeugt | ja | nicht getrennt festgehalten: Die Bausteine kamen zusammen mit eigenem Code ins Repo (ab Commit `ebcff73`); Abweichungen von den Vorlagen lassen sich nur durch erneutes Erzeugen und Vergleichen feststellen |
| `images/agw-basis/skills/writing-typst/SKILL.md` | Skill des Verfassers (aus `~/.claude/skills`), ergänzt um den Abschnitt „In dieser Sandbox“ | ja | der genannte Abschnitt |
| `images/agw-basis/skills/writing-typst/references/` | Dokumentation des Typst-Projekts (github.com/typst/typst, Ordner `docs`, Apache-2.0) | ja | keine |
| Websuche: Web-Proxy (`internal/webproxy`), `web-gate.ts`, `searxng/settings.yml`, Tabelle `web_requests` | **eigen** | ja | – |
| `pi-searxng-suite` 0.2.3 (MIT, Werkzeuge `web_search` und `web_extract`) | npm | nein, beim Bau per `pi install` (feste Version) | keine; Ein- und Ausblenden und Weiterleitung übernehmen `web-gate.ts` und der Web-Proxy |
| `third_party/pi-intercom` | npm-Paket `pi-intercom` 0.15.0 (MIT) | ja | Original in Commit `4ce9ca6`, eigene Änderung in `31a8495`; beschrieben in [`VENDORED.md`](third_party/pi-intercom/VENDORED.md), sichtbar mit `git diff 4ce9ca6 31a8495 -- poc/third_party/pi-intercom` |
| pi (`@earendil-works/pi-coding-agent` 0.87.1), `@juicesharp/rpiv-todo` 2.11.0, Python- und npm-Pakete, Typst und Typst-Pakete | npm, PyPI, GitHub | nein, beim Bau installiert (feste Versionen im Dockerfile) | keine |
| npm-Abhängigkeiten der Web-UI (`web/package.json`, u. a. React, react-markdown, shadcn/radix, `mermaid` 12.0.0 (MIT) und `dompurify` 3.4.16 (MPL-2.0 oder Apache-2.0) für die Diagramme, `pdfjs-dist` 6.3.289 (Apache-2.0) für die PDF-Vorschau) | npm | nein, nur `package.json` und `package-lock.json`; `mermaid` und `dompurify` feste Versionen, beim Bau in einen eigenen, erst bei Bedarf geladenen Chunk von `web/dist` gebündelt | keine |
| Postgres, RustFS, Verdaccio, proxpi, SearXNG (AGPL-3.0, eigener Dienst, unverändert), Node-, Go- und Debian-Abbilder | Docker Hub u. a. | nein, nur per Name/Digest in `compose.yaml` bzw. Dockerfile | Konfiguration eigen (`compose.yaml`, `pkgcache/verdaccio.yaml`, `searxng/settings.yml`) |

### Abgleich mit den Entscheidungen

| # | Stand in Stufe 1 |
|---|---|
| E1 | gehärtete Container umgesetzt: `--read-only`, `--cap-drop=ALL`, `no-new-privileges`, uid 10001, Speicher-, CPU- und PID-Grenze. Seit E9 zwei je Platz: Container von pi mit tmpfs für `/agent`, `/home/agent`, `/tmp`; Ausführungs-Sandbox mit tmpfs für `/workspace`, `/home/agent`, `/tmp` und zusätzlich `SETUID`/`SETGID` und `KILL` nur für ihren Überwacher (Prozesse des Agenten haben keine Capabilities; `KILL` braucht die Notbremse bei erschöpftem Prozesslimit). gVisor entfällt für die Arbeit (05.10.2026, Ausblick) |
| E2 | umgesetzt: pi im RPC-Modus in der Sandbox |
| E3 | **abweichend:** Der Container von pi hängt an einem **eigenen** internen Netz `agwpoc_slot_<platz>` (`internal: true`), in dem nur er und der Orchestrator sind; darüber erreicht er nur den LLM-Proxy und (seit 30.09.2026) den Web-Proxy für die Websuche, der nur bei eingeschaltetem Internet durchlässt (*Websuche*). Die Ausführungs-Sandbox (E9) hat ein eigenes internes Netz `agwpoc_slot_<platz>_x` ohne Orchestrator; sie erreicht weder API noch Proxy. **Internet ist standardmäßig aus.** Der Agent kann es mit Begründung erbitten (`agw-internet`, MCP `request_internet`); der Nutzer bestätigt in der UI, erst dann verbindet der Orchestrator die Ausführungs-Sandbox mit `agwpoc_egress` und hängt die Paket-Zwischenspeicher an ihr Netz. Der Nutzer kann den Schalter je Chat auch selbst umlegen. Der Modellverkehr geht über das interne Netz, **nicht** über den Socket |
| E4 | umgesetzt für Artefakte und MCP: ein Socket je Platz im Volume `agwpoc_sockets`, per Subpath nur das eigene Verzeichnis eingebunden; der Chat ergibt sich allein aus dem Socket; unzugewiesene Plätze antworten „nicht zugewiesen" |
| E5 | umgesetzt: Orchestrator in Go, hält Schlüssel, Sitzungen und Protokoll |
| E6 | `agw-basis` umgesetzt, per Tag statt Digest (für Stufe 1 ausreichend); `agw-ml` fehlt noch |
| E7 | umgesetzt: Zielgröße je Variante (`AGW_POOL_SIZE_CLI/MCP/BEIDE`), Einmalvergabe, Nachfüllen |
| E8 | umgesetzt über Postgres statt Host-Verzeichnis (so wie die spätere Integration es vorsieht): Sitzungsdatei wird nach jedem Durchgang gesichert, beim Fortsetzen per `exec` in die frische Sandbox gelegt und per `switch_session` geladen; dazu `/workspace` als Archiv in RustFS (*Arbeitsbereich je Chat*) |
| E9 | **umgesetzt** (29.09.2026): Ein Platz besteht aus zwei Containern. pi läuft ohne Shell; alle Werkzeuge (`bash`, `read`, `write`, `edit`, `grep`, `find`, `ls`) von Hauptagent und Subagenten leitet `exec-bridge.ts` über den Socket an den Orchestrator, der sie in der Ausführungs-Sandbox ausführt und in `tool_executions` protokolliert; der Proxy liest die angeforderten `toolCallId`s mit, und beide werden abgeglichen. Ein Wächter sperrt eigene Agenten und fremde Laufzeiten (Umgehungen aus dem Prototyp); die Skripte von `workflowScript` laufen in der Ausführungs-Sandbox statt im pi-Prozess, damit sind Ketten und parallele Subagenten wieder möglich. Die Befunde von Code- und Security-Review sind eingearbeitet. Einzelheiten, Messwerte, Reviews und Abweichungen: [`e9-ausfuehrungs-sandbox.md`](e9-ausfuehrungs-sandbox.md) |

### Standardumgebung

- **Arbeitsverzeichnis, Home und `/tmp` sind ausführbar** (tmpfs mit `exec`). Docker hängt tmpfs
  sonst mit `noexec` ein; dann ließen sich kompilierte Python-Pakete aus `~/.local` nicht laden
  („failed to map segment from shared object“). `noexec` schützt hier nichts, weil der Agent mit
  `bash` ohnehin Code ausführt.
- **Vorinstalliert statt nachgeladen:** Internet ist standardmäßig aus, `pip install` und
  `npm install` gehen also meist nicht. Mit Internet laufen sie über die Paket-Zwischenspeicher
  (siehe unten). Globale npm-Pakete landen in `~/.local` (`NPM_CONFIG_PREFIX`),
  das im `PATH` steht.
- **Typst nur mit eingebauten Paketen:** Die Pakete aus `images/agw-basis/typst/packages.txt`
  werden beim Bau in `/opt/typst/packages` geladen (`TYPST_PACKAGE_CACHE_PATH`); der Ort ist zur
  Laufzeit schreibgeschützt. Andere Pakete lassen sich deshalb auch mit Internet nicht nachladen
  (geprüft im Integrationstest). Neue Pakete gehören in die Liste und ins Abbild.
- **Diagramme mit matplotlib** (Skill `diagramme`, Varianten `cli` und `beide`): numpy, pandas und
  matplotlib sind fest im Abbild, `MPLBACKEND=Agg` ist gesetzt; der Schriften-Cache entsteht beim
  ersten Import im beschreibbaren Home (rund 0,5 s). Der Skill gibt Voreinstellungen vor (Größe,
  Achsen mit Einheit, Dezimalkomma per Formatter, weil es kein deutsches Locale gibt, `tab10`
  bzw. `viridis`) und zeigt das PNG per Markdown (siehe *Anzeige-Bilder im Chat*). plotly ist
  samt kaleido installiert und schreibt PNG über das Chromium der Sandbox (`BROWSER_PATH`); der Skill
  bevorzugt trotzdem matplotlib. **Mermaid als Datei** (seit 30.09.2026): `mmdc` (Mermaid-CLI 12, wie
  die Web-UI) mit Chromium aus Debian, als Wrapper mit `--no-sandbox` und `--disable-dev-shm-usage`
  (`/opt/agw/mmdc/puppeteer.json`); etwa 1 s je Diagramm, ohne Internet, als uid 10001 bei
  schreibgeschütztem Dateisystem geprüft. Der Systemhinweis nennt es für `cli` und `beide` als Notlösung,
  der Skill `mermaid` beschreibt den Aufruf. Chromium und mermaid-cli machen das Abbild rund 1,1 GB
  größer (810 MB → 1,9 GB). Für Diagramme in Typst verweist er auf cetz/cetz-plot und lilaq. Der
  Docker-Integrationstest erzeugt ohne Internet ein matplotlib-PNG und prüft die Magic Bytes.

### Varianten in Stufe 1

| Variante | Werkzeuge |
|---|---|
| `cli` | Standardwerkzeuge von pi, `pi-subagents`, Aufgabenliste `todo`, Skills `artifacts` (`agw-artifact`), `internet` (`agw-internet`), `platform` (`agw-platform`), `writing-typst` und `diagramme` |
| `mcp` | `--tools read,write,ls,mcp_ping,mcp_list_artifacts,mcp_upload_artifact,mcp_request_internet,mcp_platform_*,todo`: **kein** `bash`, **keine** Subagenten |
| `api` | nur `platform_http` (REST-API der Plattform über den Socket), dazu `todo`, `web_search`, `web_extract`: **kein** `bash`, **keine** Datei-Werkzeuge |
| `beide` | beides |

Die MCP-Variante bekommt keine Subagenten, weil der Subagent `worker` sonst `bash` wieder
mitbrächte und den Handlungsraum der Variante umginge.

Seit E9 lädt jede Variante `exec-bridge.ts`: Die Werkzeuge, die sie hat (in der MCP-Variante
`read`, `write`, `ls`), laufen über den Orchestrator in der Ausführungs-Sandbox. Damit haben alle
Varianten dieselbe Beobachtungsstelle, was der Anbindungsvergleich braucht. Der Werkzeugumfang je
Variante ist derselbe wie vorher.

### Modelle, Preise und Tarif

- **Preise kommen aus pis eigenem Modellregister** (`get_available_models`, pi 0.87.1 kennt
  `deepseek-flash` samt Preisen und DeepSeek-spezifischen `compat`-Einstellungen). Der Katalog
  markiert solche Modelle mit `pi_builtin`; die erzeugte `models.json` leitet dann nur `baseUrl`
  und Schlüssel auf den Proxy um und definiert das Modell **nicht** neu. Eigene Preise im Katalog
  haben Vorrang (für Modelle, die pi nicht kennt).
- **Spitzen- und Nebentarif:** pi kennt je Modell nur einen Preis. Der Orchestrator rechnet jede
  Antwort nach dem Tarif des Anbieters zum Zeitpunkt der Antwort neu ab (DeepSeek: Mo–Fr
  01:00–04:00 und 06:00–10:00 UTC Spitze, sonst halber Preis) und speichert Kosten und Tarif je
  Antwort; pis eigener Wert bleibt zum Vergleich in `usage.cost`. Chinesische Feiertage (dort
  Nebentarif) sind nicht bekannt und gelten als Spitze.
- **Maßgeblich sind die Kosten am LLM-Proxy.** Er ordnet jeden Modellaufruf über die Quelladresse
  im Platz-Netz einem Chat zu, liest Tokens, Antwort-ID und angeforderte Werkzeugaufrufe aus der
  Antwort des Anbieters und rechnet nach Tarif ab (Tabelle `llm_calls`). Damit sind Subagenten,
  Kompaktierungen und direkte Aufrufe aus der Sandbox enthalten; der Anteil außerhalb der
  Antworten der Hauptsitzung steht als `cost_other` am Chat. Aufrufe von Adressen, die keinem
  zugewiesenen Platz gehören, weist der Proxy ab.
- **Cache:** DeepSeek cached Präfixe automatisch. pis Systemprompt ist stabil (keine Uhrzeit, `cwd`
  immer `/workspace`), der Zusatz des Orchestrators statisch; gemessen treffen schon die ersten
  Antworten eines neuen Chats rund 90 % der Eingabe im Cache. Die UI zeigt die Quote je Antwort.

### Kontext, Befehle und Kompaktierung

- **Kontextauslastung** aus `get_session_stats.contextUsage`, nach jedem Durchgang und jeder
  Kompaktierung gespeichert (auch für ruhende Chats sichtbar); die UI zeigt einen Ring mit Tooltip.
- **Slash-Befehle:** eingebaut `/compact [Anweisungen]` und `/autocompact on|off`; dazu pis
  Befehle (`get_commands`: Skills wie `/skill:artifacts`, Prompt-Vorlagen, Extension-Befehle),
  die als Nachricht an pi gehen.
- **Kompaktierung** (manuell und automatisch, Schalter je Chat): Jede wird als eigener Eintrag
  gespeichert und **abgerechnet** (die Zusammenfassung ist ein eigener Modellaufruf, der in keiner
  Antwort auftaucht). Schwelle über `AGW_COMPACT_RESERVE_TOKENS` (pi: `compaction.reserveTokens`).
  Manuelle Zusammenfassungen bekommen die Anweisung „auf Deutsch“; die automatische schreibt pi
  auf Englisch, weil sich ihr Prompt über RPC nicht beeinflussen lässt.

### Subagenten: sichtbar, abgerechnet, begrenzt

**Subagenten dürfen, was der Hauptagent darf** (Entscheidung des Verfassers, 30.09.2026): Alle
eingebauten Agenten von pi-subagents (auch `researcher`, `reviewer`, `evidence-auditor`) bekommen über
`agentOverrides` dieselben Werkzeuge (`worker.SubagentTools`): `read`, `grep`, `find`, `ls`, `bash`,
`edit`, `write`, `bg_output`, `bg_stop`, `web_search`, `web_extract`, `contact_supervisor`; ausgenommen
sind weitere Subagenten (Tiefe 1) und die Aufgabenliste. Auslöser war ein Lauf mit vier `researcher`,
die nur `read`, `write` und `contact_supervisor` hatten: Ihre Werkzeuge `fetch_content` usw. stammen aus
einem anderen Paket (pi-web-access), und per `-e` geladene Erweiterungen gibt pi-subagents nicht weiter.
Die Websuche kommt deshalb samt `web-gate.ts` über `defaultSubagentOnlyExtensions`. Kind-Sitzungen im
Vordergrund laufen im Node-Prozess von pi und teilen dessen offene Tunnel zum Web-Proxy; ihre Anfragen
stehen dann unter dem Tunnel des Hauptagenten in `web_requests` (`TestSlotWebSearch` belegt die Suche
eines `researcher` am nachgebildeten SearXNG).

**Name und Zustand je Lauf** (30.09.2026). Die Sitzungsordner der Kinder tragen eine eigene Kennung, nicht
die Laufkennung von pi-subagents; die Namen aus `subagent-artifacts/<Lauf>_<agent>_input.md` ließen sich
deshalb bei Läufen im Hintergrund nicht zuordnen, und alle hießen nur „Subagent“. Die Verbindung steht
in den Statusdateien `/tmp/pi-subagents-uid-*/async-subagent-runs/<Lauf>/status.json` (Feld `sessionFile`
je Schritt, dazu `agent`, `workflowKey` bzw. `label` und `state`). `agw-exec poll-subagents` liest sie mit,
der Orchestrator speichert sie in `subagent_runs` (Ereignis `subagent_run`), und die UI zeigt den Namen
aus dem Workflow (etwa `reid`) samt Agent. Der Status richtet sich nach `state` von pi-subagents; vorher
wurde er aus den Einträgen geschätzt und stand bei Läufen im Hintergrund auf „ohne Antwort beendet“,
sobald der Hauptagent ruhte, obwohl sie weiterarbeiteten. Ohne Statusdatei (Vordergrund) kommt der Agent
aus `session_info` der Kind-Sitzung („researcher: …“). Alles aus der Sandbox, also nicht fälschungssicher.
Der Systemhinweis bittet um kurze, sprechende Schlüssel.

**Ausgabepfade der Kinder.** pi-subagents schreibt Kindern einen Ausgabepfad unter
`/agent/sessions/subagent-artifacts/outputs/…` vor, den ihre Werkzeuge in der Ausführungs-Sandbox nicht
erreichen; im Chat „Recherche Schwanzbeißen“ scheiterten daran alle drei `researcher` mit `mkdir: … Read-only
file system`. Die eigene Änderung 4 an der Kopie (`PI_SUBAGENTS_OUTPUT_INLINE=1`, `VENDORED.md`) lässt sie
das Ergebnis stattdessen in der Antwort zurückgeben; pi-subagents speichert es selbst.

**Im Hintergrund und Durchgänge von pi.** Der Systemhinweis legt dem Agenten nahe, längere
Subagenten-Arbeit mit `async: true` zu starten; dann ist er sofort frei, und der Nutzer kann weiterreden.
Ist ein Subagent fertig, stellt pi-subagents eine Nachricht (Rolle `custom`) ein und beginnt damit selbst
einen Durchgang (`triggerTurn`). Der Orchestrator erkennt solche Durchgänge an `agent_start` ohne
eigenen Auftrag und legt sie als **Weckruf** an (`chat_turns`, `trigger` wake, Herkunft system, Quelle
`pi`); die Meldung steht mit dieser Herkunft im Verlauf (UI: graue Zeile „Meldung der Subagenten“), und
die Antworten gehören zu diesem Durchgang, nicht zum vorigen Auftrag des Nutzers. Die Grenze
`AGW_AUTO_TURNS_MAX` gilt auch hier: darüber bricht der Orchestrator den Durchgang ab und hält
Eingereihtes zurück. Vorher ordnete er solche Antworten dem letzten Auftrag des Nutzers zu.

**Haupt- und Subagenten sprechen miteinander** (pi-subagents, ohne eigene Erweiterung): Ein Subagent fragt
mit `contact_supervisor` (`need_decision`, `progress_update`), die Anfrage kommt als Meldung (Rolle
`custom`, `subagent_supervisor_request`, mit `replyTo`) beim Hauptagenten an und beginnt, wenn er ruht,
einen Durchgang (siehe oben); er antwortet mit `subagent_supervisor` (`action: "reply"`). Einen laufenden
Subagenten im Hintergrund lenkt er mit `subagent` (`action: "steer"`, im Wächter erlaubt).

**Subagenten sprechen auch direkt miteinander** (pi-intercom 0.15.0, eigene Kopie in
`third_party/pi-intercom`, Änderung in `VENDORED.md`): Haupt- und alle Subagenten haben das Werkzeug
`intercom` (`list`, `send`, `ask`, `reply`); der Broker läuft im Container von pi über einen Unix-Socket
im Home, also je Platz, und ein Chat erreicht keinen anderen (die Funktion über Rechnergrenzen braucht
`ssh`, das es dort nicht gibt). Das Original lehnt Nachrichten an beschäftigte Sitzungen ohne Oberfläche
ab, und Subagenten sind genau das; die Kopie schleust sie wie bei Sitzungen mit Oberfläche per `steer`
ein. Diese Nachrichten laufen im Container von pi zwischen den Sitzungen und nicht über den
Orchestrator; sichtbar sind sie in den Sitzungsdateien (Subagenten-Ansicht) und beim Hauptagenten als
Meldung im Verlauf. Belegt durch `TestSlotSubagentTalk` (Frage, Antwort, Lenkung) und
`TestSlotSubagentIntercom` (Subagent A findet B mit `list` und schreibt ihm, B sieht die Nachricht während
der Arbeit); `fakellm` ersetzt dafür `{{replyTo}}`, `{{runId}}` und `{{peer}}`.

**Das Werkzeug `subagent` ist von Anfang an aktiv** (`--exclude-tools subagents_enable`).
pi-subagents blendet es sonst hinter einem Schalter `subagents_enable` aus; dessen Aufruf ändert die
Werkzeugliste mitten im Chat, kostet einen Modellaufruf und lässt den Präfix-Cache verfallen.
Gemessen am selben E2E-Test (29.09.2026): vorher 8 Modellaufrufe und 2 874 µ$, davon ein einzelner
Aufruf mit 8 607 Tokens ohne Cache (1 328 µ$); nachher 6 Aufrufe und 1 233 µ$ (−57 %). Der längere
Präfix mit der Beschreibung von `subagent` kommt schon beim ersten Aufruf aus dem Cache, weil er für
alle Chats gleich ist.

- **Sichtbar:** `pi-subagents` schreibt je Lauf (Vorder- wie Hintergrund) eine eigene Sitzungsdatei
  (`/agent/sessions/<Hauptsitzung>/<Lauf>/run-N/session.jsonl`). Der Orchestrator liest sie alle
  2 s nach (`agw-exec poll-subagents` per `exec` im Container von pi), speichert Auftrag, Werkzeugaufrufe, Ergebnisse und Texte in
  `subagent_entries` und schickt sie als SSE-Ereignis `subagent`. Diese Angaben stammen aus der
  Container von pi, den der Agent seit E9 nicht mehr erreicht; *belegt* sind sie trotzdem nur über
  Stellen außerhalb: Ein Werkzeugaufruf gilt als belegt, wenn der Orchestrator ihn ausgeführt hat
  (`tool_executions`, E9), ein Eintrag sonst als *am Proxy belegt*, wenn seine
  `responseId` am Proxy erfasst ist.
- **Darstellung in der Web-UI:** Neben dem Chattitel öffnet „n Subagenten ⌄“ die Liste der Läufe
  (Status, erste Zeile des Auftrags, Tokens vom Proxy bzw. Werkzeugaufrufe, Dauer); die Detailansicht
  eines Laufs trägt Brotkrumen „Chat / Lauf ⇕“ mit demselben Umschalter und statt des Eingabefelds
  den Hinweis, dass ein Subagent keine Rückfragen annimmt; der Seitenreiter „Subagenten“ zeigt den
  Hauptagenten mit den Läufen als gestrichelt verbundenen Baum, ab vier Läufen je Ebene zu einer
  aufklappbaren Gruppenkarte zusammengefasst (`web/src/lib/subagent-overview.ts`).
- **Abgerechnet:** über den Proxy (siehe oben).
- **Grenze `max_subagents` je Chat** (Vorgabe 5, höchstens 20, in UI und CLI einstellbar, wirkt
  sofort; `AGW_MAX_SUBAGENTS_DEFAULT`, `AGW_MAX_SUBAGENTS_LIMIT`). pi-subagents führt höchstens vier
  Subagenten gleichzeitig aus; mit dem Hauptagenten sind das fünf pi-Prozesse zu je rund 75 bis
  170 MiB, deshalb hat der Container von pi 2 GiB Speicher (`AGW_SANDBOX_MEMORY_MB`; die
  Ausführungs-Sandbox `AGW_EXEC_MEMORY_MB`, ebenfalls 2 GiB):
  1. *hart am Proxy:* höchstens `1 + max_subagents` gleichzeitige Modellaufrufe des Chats, sonst 429
     (`socket_calls.op = agent_limit`). Das galt vor E9 auch für Aufrufe an pi vorbei, etwa ein zweites,
     vom Agenten selbst gestartetes `pi` oder `curl`; seit E9 erreicht nur noch pi den Proxy.
  2. *hart durch Überwachung:* Sobald mehr Läufe gestartet sind als erlaubt, bricht der Orchestrator
     den Durchgang ab und beendet alle node-Prozesse im Container von pi außer pi selbst
     (`agw-exec kill-node`, `op = subagent_limit`).
  3. *kooperativ:* dieselbe Grenze in der Konfiguration von `pi-subagents`
     (`maxSubagentSpawnsPerSession` u. a.), dazu `PI_SUBAGENT_MAX_DEPTH=1` (keine verschachtelten
     Subagenten). Vor E9 konnte der Agent diese Ebene in der Sandbox ändern; seither liegt die
     Konfiguration im Container von pi außerhalb seiner Reichweite. Sie dient weiter vor allem
     der verständlichen Rückmeldung.
  4. *Wächter (E9):* Subagenten gibt es einzeln (`agent` + `task`, eingebaute Agenten, im
     Vorder- oder Hintergrund) und über `workflowScript`, also als Kette (`runs.run`) oder
     parallel (`runs.all`). Das Skript läuft seit E9 in der Ausführungs-Sandbox, nicht im
     pi-Prozess; jede Anfrage des Skripts prüft der Wächter wie einen einzelnen Subagenten, die
     Läufe startet weiter pi. Eigene Agenten, fremde Laufzeiten, `workflowScriptPath`,
     `runs.host` und `watchdog_diff` sperrt `exec-bridge.ts`
     ([`e9-ausfuehrungs-sandbox.md`](e9-ausfuehrungs-sandbox.md), *Der Wächter* und
     *`workflowScript`*).
- **Rückfragen von Extensions** (`extension_ui_request`, etwa „mehr Subagenten erlauben?“) beantwortet
  der Orchestrator mit Ablehnen und protokolliert sie (`op = extension_ui`); sonst warteten sie ewig.

Belegt im E2E-Satz: `TestSubagentsVisibleAndBilled`, `TestSubagentLimitEnforced` (bei Grenze 0
griffen Proxy und Überwachung nacheinander), `TestProxyConcurrencyLimitHard`, dazu
`TestE9WorkflowParallelSubagents` (drei parallele Läufe über `workflowScript`, 4 belegt, 1 ohne
Sandbox, 0 auffällig, die Läufe überlappten sich um 2,7 s; Lauf vom 29.09.2026).

### Aufgaben (Aufgabenliste des Agenten)

Bei Arbeiten mit mehreren Schritten führt der Agent eine Aufgabenliste mit dem Werkzeug `todo` der
pi-Erweiterung **`@juicesharp/rpiv-todo` 2.11.0** (MIT). Geprüft am 29.09.2026 (Quelltext per
`npm pack` gelesen, Downloads der letzten 30 Tage über `api.npmjs.org`):

| Paket | Lizenz | Downloads/30 T. | pi 0.87.1 | Werkzeuge und Ergebnis | Zustand | Urteil |
|---|---|---|---|---|---|---|
| **`@juicesharp/rpiv-todo` 2.11.0** | MIT | 191 944 | Peers `*` | **ein** Werkzeug `todo` (`action` create/update/list/get/delete/clear); **jedes** Ergebnis trägt in `details` den vollständigen Stand (`tasks`, `nextId`), auch bei Fehlern | nur in den Werkzeugergebnissen der pi-Sitzung, keine Dateien | **gewählt** |
| `@tintinweb/pi-tasks` 0.9.0 | MIT | 5 408 | ≥ 0.80.5 | sieben Werkzeuge (`TaskCreate` … `TaskExecute`) mit langen Beschreibungen; Ergebnisse nur Text, `details` leer | Datei `.pi/tasks/tasks-<Sitzung>.json` im Projektordner | Stand nicht aus dem RPC-Strom rekonstruierbar, Dateien im Arbeitsbereich, viele Tokens |
| `pi-tasks` 0.2.7 | MIT | 1 823 | keine Peers | zwölf Werkzeuge (Plan, Nachweis, Entscheidung …) | eigene Sitzungseinträge (`appendEntry`), nicht im RPC-Strom | zu schwer, Zustand für die UI unsichtbar |
| `@capdiem/pi-todo` 0.3.2 | MIT | 2 217 | Peers `*` | ein Werkzeug, ganze Liste je Aufruf (spart Aufrufe) | Werkzeugergebnisse | nur minifiziert veröffentlicht (schwer prüfbar); schreibt eine Erinnerung per `before_agent_start` in den Systemprompt der nächsten Runde, was den Präfix-Cache bricht |
| `@zhushanwen/pi-todo` 0.9.8 | MIT | 2 343 | Peers `^0.84.4` (schließt 0.87 aus) | ein Werkzeug, `details` mit Liste | Werkzeugergebnisse | Versionsbereich passt nicht |

Den Ausschlag geben bei rpiv-todo der vollständige Stand in jedem Ergebnis (die UI braucht keine
eigene Nachbildung der Logik), der Zustand in der Sitzung (sie wird in Postgres gesichert und beim
Fortsetzen eingespielt; rpiv-todo baut die Liste bei `session_start` und nach Kompaktierungen aus dem
letzten Ergebnis neu auf), kein Netz und keine Rückfrage. Die Anzeige über `setWidget` geht im
RPC-Modus als `extension_ui_request` hinaus und wird vom Orchestrator verworfen; das Werkzeug
funktioniert ohne sie. Nachteil: ein Aufruf je Änderung (anlegen, auf `in_progress`, auf
`completed`), bei drei Aufgaben also rund neun Aufrufe; die Werkzeugbeschreibung samt acht
Leitsätzen kostet rund 800 Tokens je Anfrage (gemessen am ersten Modellaufruf, liegt im Cache).
Der Befehl `/todos` erscheint in der Befehlsliste, meldet aber nur über `notify` und bleibt in der
Web-UI ohne Wirkung.

**Einbau.** Im Abbild wie pi-subagents per `pi install` in `/opt/agw/pihome` (feste Version,
`RPIV_TODO_VERSION`), geladen per `-e …/@juicesharp/rpiv-todo/index.ts` in **allen drei**
Varianten (`PiArgs`). In der MCP-Variante steht `todo` zusätzlich in `--tools`; das Werkzeug
verändert nur seinen eigenen Stand in der Sitzung, erweitert den Handlungsraum also nicht, und
alle Varianten planen gleich, was den Anbindungsvergleich nicht verzerrt.

**Subagenten bekommen `todo` nicht** (am System geprüft: Der Subagent nannte als Werkzeuge `read`,
`grep`, `find`, `ls`, `bash`, `edit`, `write`, `contact_supervisor`). pi-subagents gibt Kindern nur
„ambiente“ Erweiterungen aus den Einstellungen und eigene Laufzeit-Erweiterungen mit, nicht die per
`-e` geladenen des Elternteils. Die Detailansicht eines Laufs zeigt trotzdem eine Liste, falls ein
Lauf `todo` aufruft (aus den Argumenten nachgespielt, weil die Subagenten-Einträge kein `details`
tragen).

**Darstellung in der Web-UI** (`web/src/lib/tasks.ts`, `components/Tasks.tsx`): Die Liste wird aus
den todo-Aufrufen des Verlaufs in dessen Reihenfolge rekonstruiert, das letzte Ergebnis mit
`details` gewinnt; ohne `details` werden die Argumente erfolgreicher Aufrufe nachgespielt. Live kommt
`details` aus `tool_execution_end.result`, nach dem Neuladen aus den gespeicherten
`toolResult`-Nachrichten (die API liefert sie unverändert). Anzeige:

1. Chatkopf: „3/7 Aufgaben“ mit Fortschrittsring (Spinner, solange eine Aufgabe in Arbeit ist),
   nur mit Aufgaben; ein Klick öffnet die Liste (offen ○, in Arbeit mit Spinner und `activeForm`,
   erledigt abgehakt und durchgestrichen).
2. Seitenreiter „Aufgaben (n)“ mit Beschreibung, Abhängigkeiten und Bearbeiter.
3. Im Verlauf werden aufeinanderfolgende todo-Aufrufe einer Antwort zu einer Karte „Aufgaben
   aktualisiert: 2 erledigt, 1 in Arbeit“ zusammengefasst; aufgeklappt zeigt sie die Änderungen
   („#2 Tests: erledigt“) und den Stand danach. Abgewiesene Aufrufe sind markiert.
4. Subagenten-Detailansicht: eigene Liste oben, falls der Lauf `todo` aufgerufen hat.

Belegt durch `TestAgentTaskList` (E2E, MCP-Variante: drei Aufgaben, neun todo-Ergebnisse, jedes mit
vollständigem Stand, am Ende alle erledigt), `TestPiArgsTodo` und `web/src/lib/tasks.test.ts`.

### Paket-Zwischenspeicher (npm, pip)

Was Agenten mit `npm install` oder `pip install` nachladen, sammeln zwei Compose-Dienste, damit
es nicht jedes Mal neu aus dem Internet kommt. **Erreichbar sind sie nur, solange der Chat
Internet hat**; am Grundsatz „Internet aus = kein Weg nach draußen“ ändert sich nichts.

| Dienst | Abbild | Volume | in der Sandbox |
|---|---|---|---|
| `npm-cache` | `verdaccio/verdaccio:6.10.4` (per Digest) | `agwpoc_npmcache` | `NPM_CONFIG_REGISTRY=http://npm-cache:4873/` |
| `pip-cache` | `epicwink/proxpi:1.3.0` (per Digest) | `agwpoc_pipcache` | `PIP_INDEX_URL=http://pip-cache:5000/index/`, `PIP_TRUSTED_HOST=pip-cache` |

**Warum diese beiden.** Verdaccio ist der übliche npm-Zwischenspeicher und läuft mit einer kurzen
Einstellungsdatei (`pkgcache/verdaccio.yaml`). Er ist dort **nur lesend**: Anmelden
(`max_users: -1`) und Veröffentlichen sind gesperrt, sonst könnte ein Agent Pakete für andere
Chats unterschieben; die Web-Oberfläche ist aus (am laufenden Dienst geprüft: 409, 401, 404).
Für pip ist proxpi gewählt statt devpi: Es ist ein reiner Zwischenspeicher ohne Upload, ohne
Nutzer und ohne Datenbank, wird über Umgebungsvariablen eingestellt und ist gepflegt (1.3.0 vom
Mai 2026, letzte Änderung im Repository September 2026). devpi-server kann Pakete annehmen und
bräuchte dafür eigene Sperren. proxpi leitet pip standardmäßig nach 0,9 s an
`files.pythonhosted.org` weiter, wenn der Abruf länger dauert; die Datei käme dann am
Zwischenspeicher vorbei. `PROXPI_DOWNLOAD_TIMEOUT=120` verhindert das. Beide laufen mit
schreibgeschütztem Wurzeldateisystem, ohne Capabilities und mit `no-new-privileges`.

**Netz und Kopplung an den Internet-Schalter.** Beide Dienste hängen nur am Compose-Netz
`agwpoc_pkg` (`10.231.21.0/24`, mit Internet, ohne ICC), nicht an `agwpoc_intern`: Postgres,
RustFS und die Nutzer-API erreichen sie darüber nicht; `10.231.21.0/24` steht zusätzlich in
`AGW_BLOCKED_SUBNETS`. Ins Egress-Netz können sie nicht, denn **ohne ICC verwirft Docker dort
jeden Verkehr zwischen Containern** (geprüft: Der Name löst auf, die Verbindung läuft in die
Zeitgrenze). Deshalb hängt `SetInternet` sie beim Einschalten unter den Aliasen `npm-cache` und
`pip-cache` an das **Platz-Netz** der Sandbox und beim Abschalten wieder ab; scheitert das Lösen,
meldet der Schalter einen Fehler. Die Kopplung sitzt damit an genau der Stelle, die auch das
Egress-Netz verbindet, und gilt ebenso beim Fortsetzen in frischer Sandbox. Wird ein Platz mit
Internet abgebaut oder startet der Orchestrator neu, löst `RemoveSlotNetwork` die
Zwischenspeicher mit. Ohne Internet ist der Name nicht auflösbar: pip scheitert nach rund 8 s
(fünf Wiederholungen), npm nach rund 2 s (`NPM_CONFIG_FETCH_RETRIES=1` und kurze Wartezeiten
statt rund 70 s mit den Standardwerten).

Welche Container gemeint sind, sagen `AGW_NPM_CACHE` und `AGW_PIP_CACHE` in `compose.yaml`
(Container-Namen von Compose); leer schaltet den jeweiligen Zwischenspeicher samt Umgebung in
der Sandbox ab. Fehlt ein Dienst, bleibt das Internet trotzdem an, und Installationen über ihn
scheitern.

**Abwägung.** Ein Zwischenspeicher hängt an den Platz-Netzen **aller** Sandboxen, die gerade
Internet haben, also auch neben dem Orchestrator und dessen LLM-Proxy. Sandboxen erreichen sich
darüber nicht, weil ein Container nicht routet; wer aber Verdaccio oder proxpi übernimmt, stünde
in mehreren Platz-Netzen. Ein eigenes Netz je Sandbox nur für die Zwischenspeicher vermiede die
Nachbarschaft zum Orchestrator, kostete aber ein weiteres Netz je Platz; für Stufe 1 ist das
nicht umgesetzt. Außerdem teilen sich alle Chats denselben Bestand: Ob ein Paket schon im
Zwischenspeicher liegt, verrät die Antwortzeit (ein schmaler verdeckter Kanal zwischen Chats,
die beide Internet haben und damit ohnehin Wege nach draußen). Die Zwischenspeicher selbst
erreichen wie jede Sandbox mit Internet auch LAN und Host-Dienste (M3). Erzeugt Compose einen
der Dienste neu (geänderte Einstellung), verliert er die angehängten Platz-Netze; Chats mit
Internet erreichen ihn dann erst nach erneutem Umschalten wieder. Ein bloßes `compose up` ohne
Änderung lässt ihn stehen (geprüft).

### Bestätigung durch den Nutzer

Drei Arten: **Artefakt-Upload**, **Internetzugang** und **schreibender Plattform-Aufruf** (siehe
*Plattform-Anbindung (direkt)*). Jeder Upload eines Artefakts (über `agw-artifact upload` oder `mcp_upload_artifact`) wird im
Orchestrator angehalten: Datei als *ausstehend* in RustFS, Eintrag in `approvals`, Karte in der
Web-UI. Der Aufruf des Agenten wartet, bis der Nutzer bestätigt oder ablehnt (Standard 10 min,
danach gilt die Anfrage als abgelaufen). Durchgesetzt wird das am Socket, nicht in pi. Ein Chat
mit offener Bestätigung ruht nicht.

**Wer über den Socket fragt** (30.09.2026). Neben `PI_AGW_TOOL_CALL_ID` setzt der Orchestrator in jedes
`bash` auch `PI_AGW_SESSION`: `main` beim Hauptagenten, beim Subagenten die Laufkennung (wie
`tool_executions.session`). `agw-artifact` und `agw-internet` schicken beides als `X-Agw-Tool-Call` und
`X-Agw-Session` zurück (Upload, Internet, `list`, `get`); unbekannte Formen verwirft der Socket.
`approvals` und `socket_calls` tragen dazu `session` und `tool_call_id`. Die UI zeigt an Bestätigungen
und im Socket-Protokoll eine Marke „Subagent <Name>“ mit Link auf dessen Ansicht; die Subagenten-Ansicht
zeigt offene Bestätigungen des Chats (vorher dort gar nicht sichtbar) und die Artefakte unter dem
Werkzeugaufruf des Subagenten. Wie die Kennung des Aufrufs dient die Sitzung nur der Anzeige: Ein Agent
kann sie in seiner Shell ändern. Fälschungssicher bleibt der Abgleich über `tool_executions`. MCP haben
nur Hauptagenten; Aufrufe über `/tool/upload` bekommen die Sitzung aus der geprüften Sitzungsdatei.

**Ergebnisse im Verlauf** (30.09.2026). Ein bestätigtes Artefakt erscheint direkt unter dem
Werkzeugaufruf, der es hochgeladen hat, nicht nur im Seitenreiter: Bilder (PNG, JPEG, GIF, WebP) als
Vorschau, die ein Klick vergrößert, PDFs als Karte, die ein Klick im großen Dialog öffnet (eigener
Betrachter mit pdf.js: Seiten auf hellem Grund, scrollbar, Seitenzahl, Zoom; Download oben rechts wie
bei Bildern), alles mit Download. Dafür trägt
`artifacts.tool_call_id` die Kennung des Aufrufs: bei MCP aus der Anfrage an `/tool/upload`, bei der CLI
aus der Umgebungsvariable `PI_AGW_TOOL_CALL_ID`, die der Orchestrator jedem `bash` (auch im
Hintergrund) nach der Prüfung setzt und die `agw-artifact` als Kopf `X-Agw-Tool-Call` zurückschickt.
Die Kennung dient nur der Anzeige; der Agent könnte sie bei der CLI verändern. Der Download-Endpunkt
bleibt `attachment`: Die PDF-Vorschau lädt die Datei in der UI und zeichnet sie mit pdf.js auf Canvas,
damit kein Inhalt des Agenten als Seite der Anwendung geöffnet wird. Der Betrachter des Browsers im
iframe schied aus: Sein dunkler Rand lässt sich nicht umfärben, und unter der CSP der Web-UI
(`default-src 'self'`) wäre ein `blob:`-iframe blockiert (lief nur im Vite-Server ohne CSP). pdf.js 6
braucht kein `eval`, sein Worker kommt von derselben Herkunft; Betrachter und Worker sind eigene,
erst beim ersten PDF geladene Teile von `web/dist` (433 kB bzw. 1,3 MB). Eingeordnet
wird nach Inhaltstyp **und** Endung (`lib/artifactPreview.ts`); SVG und alles andere erscheinen als Datei.
Ein Upload mit gleichem Namen überschreibt das Artefakt und erscheint dann beim neueren Aufruf.

### Anzeige-Bilder im Chat

Bilder, die der Agent in einer Antwort per Markdown zeigt (`![Grafik](/workspace/plot.png)`),
erscheinen im Chat als Vorschau (höchstens 320 px hoch); ein Klick öffnet die Großansicht mit
Download. Dieselbe Großansicht haben Bild-Anhänge in der Nutzerblase. Der Systemhinweis und der
Skill `diagramme` sagen dem Agenten, wie er Bilder zeigt.

**Welche Adressen die UI lädt** (`web/src/lib/images.ts`, `internal/chat/images.go`):

| Form | Verhalten |
|---|---|
| `http(s)://…`, `//…`, jedes andere Schema, Pfade außerhalb der drei Orte | **nie geladen**, nur als Text gezeigt (wie bisher, K1) |
| `data:image/…;base64,…` mit PNG, JPEG, GIF oder WebP | angezeigt; bleibt im Browser, CSP erlaubt `data:` |
| SVG (als Datei oder `data:`) | nie, SVG kann Skript enthalten |
| lokaler Pfad unter `/workspace`, `/tmp`, `/home/agent` (relativ gilt ab `/workspace`) | über `GET /api/chats/{id}/images?path=…&msg=…`, erst wenn die Antwort fertig ist |

**Warum das sicher bleibt.** Das Problem aus K1 ist, dass der Browser eine *fremde* Adresse lädt
und damit Daten aus der Sandbox im Pfad oder in der Abfrage nach draußen trägt, ohne dass der
Nutzer Internet bestätigt hat. Bei lokalen Pfaden lädt der Browser nur vom Orchestrator
(`img-src 'self' data:`), und der Orchestrator holt die Datei selbst aus der Sandbox:

- **Pfadprüfung zweimal:** Der Orchestrator bereinigt den Pfad (`path.Clean`, keine Steuerzeichen,
  kein Schema) und nimmt nur `/workspace`, `/tmp` und `/home/agent`; `/agent` (pi-Konfiguration,
  Sitzungen, seit E9 ohnehin in einem anderen Container) und alles andere nicht. In der
  Ausführungs-Sandbox löst `realpath -e` Symlinks auf, und das Ziel
  muss wieder unter einem der drei Orte liegen, sonst wird nichts gelesen. Gelesen wird als
  Agent-Nutzer (uid 10001) mit `head -c <Grenze+1>`, also nie mehr als die Grenze.
- **Nur Rasterbilder:** Der Typ kommt aus den Magic Bytes (PNG, JPEG, GIF, WebP), nie aus der
  Endung; alles andere wird abgewiesen. Ausgeliefert mit genau diesem `Content-Type`,
  `X-Content-Type-Options: nosniff`, `Cache-Control: private`. Grenze `AGW_IMAGE_MAX_MB`
  (Standard 10).
- **Verbleibende Lücke:** Zwischen `realpath` und `head` könnte der Agent einen Symlink tauschen
  (TOCTOU). Er käme damit nur an Dateien, die er ohnehin lesen darf, und nur, wenn sie wie ein Bild
  beginnen; das Ergebnis geht an die angemeldete UI, nicht nach draußen.

**Kein Artefakt.** Ein Artefakt-Upload ist ein Ergebnis, das die Sandbox verlässt und dauerhaft
beim Orchestrator liegt; deshalb braucht er die Bestätigung durch den Nutzer. Anzeige-Bilder
verlassen die Plattform nicht, sie gehen nur an die angemeldete UI dessen, der den Chat führt. Sie
sind eine eigene Art (Tabelle `chat_images`, S3-Präfix `<chat>/images/`) und erscheinen nicht in
der Artefaktliste. Wer die Datei behalten oder weitergeben will, lädt sie als Artefakt hoch.

**Sichtbar, auch wenn der Chat ruht.** Nach jeder fertigen Antwort (`message_end`) sucht der
Orchestrator Markdown-Bildverweise mit lokalem Pfad (höchstens 20, nicht in Code) und sichert die
Bilder im Hintergrund in S3; Fehler werden nur protokolliert. Ruhen und Beenden warten eine
laufende Sicherung ab. Ein Abruf liefert aus S3, sonst bei aktivem Chat aus der Sandbox (und
sichert dabei). Je Antwort und Pfad gilt die erste Sicherung: Überschreibt der Agent `plot.png`
später, zeigt die ältere Antwort weiter ihre Fassung.

**Kennung der Antwort (`msg`):** pis `responseId` der Antwort, sonst `ts-<timestamp>`. Beide stehen
in der Nachricht selbst, sind also live bei `message_end` und nach dem Neuladen aus Postgres gleich
(die Position `seq` kennt die UI live nicht). Solange eine Antwort noch gestreamt wird, zeigt die
UI statt des Bildes einen Platzhalter. Die Subagenten-Detailansicht zeigt Bilder weiterhin nur als
Platzhalter.

Belegt durch Unit-Tests (Pfadprüfung, Magic Bytes, Verweise aus Markdown, Handler, Manager mit
Attrappe: gesichert, nach dem Ruhen abrufbar, Nicht-Bild und zu große Datei abgewiesen), Vitest
(`lib/images.test.ts`, `Markdown.test.tsx`) und `TestAgentImageShownAndKept` im E2E-Satz.

### Mermaid-Diagramme und Laufzeiten

**Mermaid.** Ein Codeblock mit Sprache `mermaid` in einer Antwort erscheint als Diagramm
(`web/src/lib/mermaid.ts`, `components/MermaidDiagram.tsx`, Skill `mermaid`). Die Bibliothek lädt erst beim
ersten Diagramm (dynamischer Import, eigener Chunk). Beim Streamen bleibt ein noch offener Block Quelltext;
gezeichnet wird, sobald er geschlossen oder die Antwort fertig ist. Bei einem Syntaxfehler steht der
Quelltext mit dem Hinweis „Diagramm konnte nicht gerendert werden“ da. Große Diagramme (über 4 000 Zeichen oder
150 Kanten) und alle ab dem sechsten einer Nachricht zeichnet die UI erst auf Klick („Großes Diagramm, zum
Zeichnen klicken“); ein großes Diagramm hielt den Browser rund 3,4 s auf (Review 3, N4). Sicherheit: `securityLevel: "strict"`
(keine click-Direktiven, kein HTML in Labels), `htmlLabels: false`, und das Diagramm darf diese Schlüssel
nicht per `%%{init}%%` ändern (`secure`). Das SVG wird danach noch einmal mit DOMPurify gesäubert (kein
Skript, keine Ereignis-Attribute, `href` und `url()` nur auf `#id`, kein `@import`) und nur als `<img>` mit
`data:`-Adresse eingebunden; in diesem Bild-Kontext führt der Browser kein Skript aus und lädt nichts nach.
Die CSP bleibt unverändert: Die gebauten Chunks enthalten kein `eval`; das einzige `Function(…)` ist lodashs
`Function("return this")`, das im Browser nie erreicht wird, weil `self` existiert.

**Laufzeiten** (`web/src/lib/runtime.ts`). Während eines Laufs zeigt die Statuszeile unter dem Verlauf
Tätigkeit und Dauer („Führt bash aus … 1:05“), Chatkopf und Chatliste „Läuft · 12 s“, jede Werkzeugkarte
ihre Dauer (laufende zählen mit), und die Antwort, die einen Lauf beendet, ihre Gesamtdauer in der
Fußzeile. Live stammen die Zeiten aus dem Empfang von `agent_start`, `tool_execution_start|end` und
`message_end`. Nach dem Neuladen aus `created_at`: Gesamtdauer von der Nutzernachricht bis zur letzten
Antwort des Laufs, Werkzeugdauer nur, wenn die Antwort genau einen Aufruf enthielt (sonst, falls vorhanden,
die Ausführungszeit laut Orchestrator aus `tool_executions`). Was sich nicht belegen lässt, bleibt leer.
In der Chatliste kennt die UI den Beginn nur für den geöffneten Chat, solange der Orchestrator
`running_since` nicht mitliefert.

### Arbeitsbereich je Chat

Dateien, die im Chat entstehen (Grafiken, Skripte, Zwischenergebnisse), überstehen das Ruhen
(Wunsch des Verfassers, 29.09.2026). Der Orchestrator sichert dazu `/workspace` je Chat und spielt
es beim Fortsetzen in die frische Sandbox ein. Nachinstallierte Pakete bleiben **bewusst nicht**
erhalten (Entscheidung des Verfassers): Sie sind aus dem Netz jederzeit wieder zu holen, blähen
die Sicherung auf und gehören nicht zum Ergebnis eines Chats.

| Pfad | Bleibt erhalten? | Warum |
|---|---|---|
| `/workspace` | **ja**, bis `AGW_WORKSPACE_MAX_MB` (Standard 200 MB, Summe der Dateigrößen) | Arbeitsdateien des Agenten; gesichert nach jedem Lauf und beim Ruhen |
| `/workspace/inputs/` | ja, aber **nicht** aus der Sicherung | kommt beim Fortsetzen wie bisher aus den Eingaben des Nutzers (RustFS); das Archiv enthält es nicht, das Einspielen verwirft Einträge darunter |
| Ordner `node_modules`, `.venv`, `__pycache__`, `.cache` (in jeder Tiefe unter `/workspace`) | nein | Pakete und Caches, neu erzeugbar |
| `/tmp` | nein | Zwischenablage, flüchtig wie auf jedem Rechner |
| `/home/agent` (samt `pip install --user`, `npm install -g` in `~/.local`, Schriften-Cache von matplotlib) | nein | nachinstallierte Pakete bleiben nicht erhalten |
| laufende Prozesse, Umgebungsvariablen der Shell | nein | die Sandbox wird abgebaut |
| `/agent` (pi, Sitzungen) | Sitzung ja, über E8 | liegt seit E9 im Container von pi, nicht in der Ausführungs-Sandbox |
| Artefakte | ja, dauerhaft beim Orchestrator | das Ergebnis, das den Chat verlässt; Upload nur mit Bestätigung |

**Ablauf.**

- *Sichern* nach jedem abgeschlossenen Lauf (`agent_settled`, im Hintergrundjob des Chats hinter der
  Sitzung) und beim Ruhen und Herunterfahren vor dem Abbau. Ein Skript (`bash`, als uid 10001) bildet zuerst einen Fingerabdruck
  über Typ, Name, Größe, Zeit und Rechte aller Dateien, Ordner und Symlinks (ohne die Ausschlüsse);
  ist er gleich dem der letzten Sicherung, passiert nichts weiter. Sonst packt es
  `tar --format=posix -czf -` (ohne `./inputs` und die Ausschlüsse) und schreibt es über `exec` zum
  Orchestrator; `head -c` begrenzt die Menge hart auf 2 × Grenze + 16 MiB. Ablage in RustFS unter
  `<chat>/workspace.tar.gz` (überschrieben, keine Versionen), Angaben in Postgres (`chat_workspaces`:
  Größe, Archivgröße, Dateizahl, SHA-256, Fingerabdruck, Zeitpunkt).
- *Über der Grenze* wird nicht gepackt. Die letzte gültige Sicherung bleibt, das Protokoll meldet
  `Arbeitsbereich NICHT gesichert`, der Chat bekommt einmal je Stand ein Ereignis `error`
  („Arbeitsbereich nicht gesichert: …“, die UI zeigt es als Warnung), und das Feld `workspace`
  trägt `skipped_reason`.
- *Einspielen* beim Fortsetzen in `attach`, nach `switch_session` und **vor** den Eingaben und dem
  ersten Auftrag. Sichern und Einspielen sind je Chat gesperrt; das Ruhen wartet eine laufende
  Sicherung ab.
- *Einspielen scheitert* (Ablage fehlt, Prüfsumme falsch, `tar` bricht ab): Der Chat bekommt ein
  Ereignis `error`, und **in dieser Sandbox wird nicht gesichert**, damit ein leerer Arbeitsbereich
  die gültige Sicherung nicht überschreibt.
- `AGW_WORKSPACE_MAX_MB=0` schaltet Sichern und Einspielen ab.

**Das Archiv ist vom Agenten gestaltbar**, denn es stammt aus seiner Sandbox. Der Orchestrator
packt es deshalb nicht blind aus, sondern liest es selbst (`archive/tar`) und reicht nur
unbedenkliche Einträge als neues tar an `tar -xf - --no-same-owner` in der Sandbox weiter:
normale Dateien, Ordner, Symlinks (als Symlink, nie verfolgt) und harte Links auf Dateien aus
demselben Archiv. Verworfen werden absolute Pfade, `..`, Steuerzeichen, alles unter `inputs/`,
alles **unter einem Symlink** aus demselben Archiv (sonst schriebe tar durch ihn hindurch),
FIFOs und Geräte sowie setuid-/setgid-Bits. Die Summe der Dateigrößen ist beim Auspacken
begrenzt. Selbst ohne diese Prüfung bliebe der Schaden in der neuen Sandbox desselben Chats,
weil als uid 10001 ausgepackt wird.

**Was der Agent darüber weiß:** Der Systemhinweis nennt, was erhalten bleibt und was nicht, und
sagt, dass er nachinstallierte Pakete nach dem Fortsetzen neu installieren muss und Dateien, die er
später braucht, unter `/workspace` statt `/tmp` ablegt. Die Grenze steht dort als „Standard
200 MB“; wer `AGW_WORKSPACE_MAX_MB` ändert, passt den Satz an (der Hinweis ist statisch, damit
der Präfix-Cache greift).

**Sichtbar:** Seitenreiter *Artefakte*, Abschnitt *Arbeitsbereich* („Gesichert: 1,2 MiB,
14 Dateien, 17:05“, bei Auslassung eine Warnung), `agw chat show` („Arbeitsbereich gesichert: …“),
API-Feld `workspace` am Chat.

Belegt durch Unit-Tests (Filter gegen `..`, absolute Pfade, Schreiben durch Symlinks, FIFOs,
harte Links nach außen; Grenze beim Entpacken; Ausschlüsse im Skript), Manager-Tests mit Attrappe
gegen Postgres (nach dem Lauf gesichert, beim Fortsetzen vor dem ersten Auftrag eingespielt,
Grenze hält die letzte Sicherung, gescheitertes Einspielen sichert nicht), Store-Test,
`TestWorkspaceRoundTripInSandbox` gegen Docker (gehärtete Sandbox: Rechte, Symlinks, Ausschlüsse,
`/tmp` und Home nicht dabei, Fingerabdruck nach dem Einspielen gleich, Angriffsarchiv schreibt
nicht nach `/home/agent`) und `TestWorkspaceSurvivesSuspend` im E2E-Satz.

### Senden, Warteschlange und Fortsetzen

Wunsch des Verfassers (29.09.2026): Eine gesendete Nachricht soll sofort sichtbar sein, das
Fortsetzen eines ruhenden Chats nicht „kaputt“ aussehen, und Nachrichten während eines Laufs sollen
sich einreihen und wieder entfernen lassen (wie in Claude Code).

- **Sofort sichtbar.** Die UI zeigt die Nachricht beim Senden (samt Anhängen) im Verlauf; pis
  Nutzernachricht ersetzt sie ohne Doppelung. Scheitert das Senden, bleibt sie als „Nicht gesendet“
  stehen, und Text und Anhänge kommen zurück ins Eingabefeld: Der Orchestrator hat dann nichts an pi
  gegeben, nichts gespeichert und nichts eingereiht.
- **Fortsetzen mit Schritten.** `POST /messages` bleibt synchron (die Antwort kommt nach dem
  Fortsetzen und dem `prompt`); die Schritte meldet der Orchestrator währenddessen als SSE-Ereignis
  `resume` (`acquire`, `session`, `settings`, `workspace`, `inputs`, dann `ready` oder `failed`, in
  der Reihenfolge von `attach`). Der Grund für synchron statt Hintergrund: Die UI hält den
  Ereignisstrom des Chats auch im Ruhezustand offen, die Schritte erreichen sie also vor der
  Antwort; ein Fehler kommt weiter als HTTP-Fehler zum Aufrufer, und CLI und Tests bleiben
  unverändert. Chatkopf und Chatliste zeigen „wird fortgesetzt“ (`resuming` am Chat). Der Block im
  Verlauf klappt danach auf eine Zeile zusammen („Fortgesetzt in frischer Sandbox · 0,4 s“); er ist
  nur live bekannt und steht nach einem Neuladen der Seite nicht mehr da.
- **Warteschlange im Orchestrator, nicht in pi.** Arbeitet pi, wird fortgesetzt oder ist ein anderer
  Auftrag unterwegs, reiht der Orchestrator die Nachricht in Postgres ein (`chat_queue`, übersteht
  einen Neustart), statt sie wie früher per `steer` an pi zu geben, wo sie sich nicht zurückholen
  ließ. Beim Laufende (`agent_settled`, **nach** dem Sichern von Sitzung und Arbeitsbereich) gehen
  alle offenen Einträge **gemeinsam als eine Nutzernachricht** an pi, als Absätze in Reihenfolge,
  Anhänge in einem Block; der Verlauf zeigt damit genau den Text, den pi bekam. Meldungen des Orchestrators
  (Systemeinträge) stehen darin in ihrer eigenen Hülle vor dem Text des Nutzers, und die gespeicherte Nachricht
  nennt ihre Herkunft (*Hintergrundaufgaben*, Review 3). **Seit 30.09.2026 wird eingeschleust**, sobald
  ein Werkzeug läuft oder startet: Der Orchestrator gibt die offenen Einträge dann per `prompt` mit
  `streamingBehavior: "steer"` an pi, und pi fügt sie nach den laufenden Werkzeugen und vor dem nächsten
  Modellaufruf in denselben Durchgang ein (Verhalten wie Claude Code). Solange das Modell nur schreibt,
  bleiben sie eingereiht und entfernbar; endet der Durchgang ohne weiteres Werkzeug, gehen sie wie oben
  mit dem Laufende. Eingeschleust wird nur, wenn mindestens ein Eintrag vom Nutzer stammt; reine
  Meldungen des Orchestrators gehen weiter über das Laufende und zählen dort als Weckruf. Vor einem
  Abbruch und nach dem Laufende holt der Orchestrator mit `clear_queue` zurück, was pi noch nicht
  eingefügt hat (**pi setzt nach `abort` Eingereihtes fort**), und öffnet die Einträge wieder
  (`restored`, nach einem Abbruch zurückgehalten). Hat pi nichts mehr eingereiht, aber einen neuen
  Lauf begonnen, kam der Auftrag zu spät für den alten Lauf und bleibt, wie er ist. Entfernen geht,
  solange ein Eintrag nicht übergeben ist (danach 409). Nach einem Abbruch hält der Orchestrator die
  Warteschlange zurück (`queue_held`, `hold_reason: "abort"`): Sie geht mit der nächsten Nachricht mit oder
  über „Jetzt senden“. Ein Abbruch, während ein Auftrag noch unterwegs ist (etwa beim
  Fortsetzen), hält auch diesen Auftrag zurück. Antwortet pi auf `prompt` nicht in der Frist, hat den Auftrag
  aber angenommen, nimmt der Orchestrator nichts zurück (sonst ginge er doppelt an pi). Übergebene Zeilen bleiben
  in `chat_queue` stehen (Entscheidung Review 3: für die Auswertung nachvollziehbar, klein, verschwinden mit dem
  Chat). Die UI zeigt die Einträge über dem Eingabefeld mit X zum Entfernen.
  CLI: `agw chat send` meldet „Eingereiht“, `agw chat queue <id> [--send]`, `agw chat unqueue`.

Belegt durch Manager-Tests mit Attrappe (Schritte in Reihenfolge vor der ersten Antwort, Fehlerfall
ohne gesendete Nachricht; einreihen, entfernen, gemeinsame Übergabe, zurückgehalten nach Abbruch,
Neustart aus Postgres), Store-Test, Vitest (`lib/outbox.test.ts`) und
`TestResumeStepsBeforeAnswer` sowie `TestQueueWhileRunning` im E2E-Satz.

### Websuche

Wunsch des Verfassers (30.09.2026): Websuche wie in anderen Agenten, mit **eigenem SearXNG** im Stack,
und die Werkzeuge nur mit Internet. Umgesetzt mit `pi-searxng-suite` 0.2.3 (npm, MIT, unverändert):
`web_search` fragt SearXNG (JSON), `web_extract` holt eine Adresse und wandelt HTML, Text, PDF und
Bilder um. Beide laufen als Extension **im Container von pi** und rufen `fetch` auf.

- **Weg nach draußen:** pi hat kein Internet. Node schickt HTTP und HTTPS über den **Web-Proxy des
  Orchestrators** (`internal/webproxy`, Port 18486, im Platz-Netz unter `orchestrator`), eingestellt
  mit `NODE_USE_ENV_PROXY=1`, `HTTP(S)_PROXY` und `NO_PROXY=orchestrator` (der LLM-Proxy bleibt direkt).
  `PI_OFFLINE=1` und `PI_TELEMETRY=0` halten pis eigene Abrufe vom Proxy fern.
- **Der Proxy lässt nur durch,** wenn die Quelladresse einem aktiven Platz gehört (Zuordnung wie am
  LLM-Proxy) **und der Chat Internet hat** (derselbe Schalter wie für die Ausführungs-Sandbox). Ziele:
  der eigene SearXNG (Name `searxng`) oder öffentliche Adressen auf Port 80 und 443. Aufgelöst wird im
  Proxy, verbunden mit genau der geprüften Adresse (kein DNS-Rebinding); private, Loopback-,
  Link-Local-, CGNAT- und die Netze des Stacks (`AGW_BLOCKED_SUBNETS`) sind gesperrt, ebenso ein Name,
  der auch nur teilweise auf eine solche Adresse zeigt. **Node tunnelt auch einfaches HTTP per
  `CONNECT`** (gemessen am 30.09.2026); der Proxy erlaubt `CONNECT` deshalb auf 80 und 443 und zu SearXNG.
- **Protokoll:** jede Anfrage, auch eine abgewiesene, in `web_requests` (Chat, Methode, Ziel, Port,
  Status, Bytes, Dauer, Grund der Abweisung), SSE `web_request`, API `GET /api/chats/{id}/web_requests`.
  Ein Tunnel steht ab dem Aufbau darin; Bytes und Dauer kommen beim Schließen dazu (Node hält Tunnel
  offen). Bei HTTPS ist nur das Ziel sichtbar, nicht der Pfad. Ein offener Tunnel prüft den Schalter
  alle 2 s erneut und schließt sich, sobald das Internet aus ist (`denied`: „tunnel closed: internet
  switched off“); sonst hätte Node nach dem Abschalten noch bis zu 5 min durch einen offenen Tunnel
  senden können (Sicherheitsreview 30.09.2026, als Härtung eingestuft).
- **Nur mit Internet angeboten:** `web-gate.ts` fragt den Schalter am Socket ab (`GET /tool/internet`)
  und blendet die beiden Werkzeuge mit `pi.setActiveTools` ein oder aus: einblenden vor jedem
  Modellaufruf (`turn_start`, pi übernimmt rein additive Änderungen, also auch mitten im Durchgang
  nach einer Bestätigung), ausblenden nur zwischen zwei Aufträgen (`before_agent_start`). Die
  Werkzeugliste ändert sich damit nur beim Umschalten; der Präfix-Cache verfällt dann einmal.
  Durchgesetzt wird der Schalter am Proxy, nicht in dieser Extension.
- **SearXNG** (`searxng/searxng`, per Digest festgelegt) hängt nur am Netz `agwpoc_search` zusammen mit
  dem Orchestrator, läuft als uid 977 mit schreibgeschütztem Dateisystem, ohne Capabilities, ohne Port
  am Host; `searxng/settings.yml` schaltet JSON ein und Limiter, Bild-Proxy und öffentlichen Betrieb
  aus. Den Geheimschlüssel (`SEARXNG_SECRET`) legt `./dev.sh` in `.env` an.
- **Alle drei Varianten** bekommen die Werkzeuge (in `mcp` stehen sie in `--tools`), damit der Vergleich
  der Anbindungen nicht an der Websuche hängt. Subagenten bekommen sie nicht (pi-subagents gibt per
  `-e` geladene Erweiterungen nicht weiter).
- Belegt durch `internal/webproxy` (Schalter, Sperren, CONNECT, SearXNG), `TestSlotWebSearch` (echter
  pi: ohne Internet „not found“, nach dem Einschalten mitten im Durchgang Suche und Abruf über den
  Proxy) und einen Durchlauf mit `deepseek-flash` gegen den eigenen SearXNG am 30.09.2026.

### Hintergrundaufgaben

Wunsch des Verfassers (29.09.2026): lange Befehle im Hintergrund wie in Claude Code. Der Agent startet einen
Befehl mit `bash` und `run_in_background: true`, das Werkzeug kehrt sofort zurück, und der Agent wird
benachrichtigt, wenn der Befehl endet.

**Laufenden Befehl stoppen oder in den Hintergrund schieben** (30.09.2026, wie Strg+B in Claude Code).
Ein laufendes `bash` im Vordergrund hat in der Web-UI zwei Knöpfe. *Stoppen* bricht den Befehl ab; der
Agent bekommt „Command stopped by the user“ samt bisheriger Ausgabe und arbeitet weiter (nicht wie ein
Abbruch des ganzen Durchgangs). *In den Hintergrund* lässt den Prozess weiterlaufen: `bgtask.Adopt`
übernimmt ihn mit der bisherigen Ausgabe als `bg-N` (dieselbe `tool_call_id`, Ausgabedatei ist die des
Vordergrundbefehls), der Werkzeugaufruf endet sofort mit „The user moved this command to the background
as bg-N …“, und das Ende meldet der Orchestrator wie bei jeder Hintergrundaufgabe. Dafür läuft ein
Vordergrundbefehl in `sock` in einem eigenen Kontext (Register `sock.Foreground` je Platz, Codes
`stopped` und `backgrounded` an die Bridge), und seine **Zeitgrenze hält der Orchestrator**, nicht mehr
`agw-exec`: Nach einer Umwandlung gilt sie nicht mehr. Die Meldung „Command timed out after N seconds“
bleibt wortgleich (Gleichlauf mit pi geprüft). Belegt durch `TestForeground*` (sock), `TestAdopt`
(bgtask) und `TestSlotForegroundControl` mit echtem pi und echter Bridge.

**Warum eigen.** Die fertige Erweiterung `pi-background-tasks` (npm, ISC) passt nicht: Ihre peerDependencies
reichen nur bis pi 0.84 (der PoC nutzt 0.87.1), und sie startet die Prozesse im Container von pi, wo es nach E9
bewusst keine Shell gibt. Die Umsetzung geht deshalb den Weg von E9: Der Orchestrator führt auch
Hintergrundaufgaben in der Ausführungs-Sandbox aus und protokolliert sie.

**Werkzeuge** (Varianten `cli` und `beide`; die MCP-Variante hat kein `bash`):

| Werkzeug | Parameter | Ergebnis |
|---|---|---|
| `bash` | zusätzlich `run_in_background: boolean` | „Background task bg-3 started. You will be notified when it ends; do not poll or sleep. Output so far: bg_output bg-3 (full log /tmp/agw-bg/bg-3.log); stop it with bg_stop bg-3.“ |
| `bg_output` | `id`, `tail_lines?` (Standard 2 000) | eine Zeile zum Stand (läuft seit …, Exit-Code, gestoppt …), dann das Ende der Ausgabe, gekürzt wie `bash` (2 000 Zeilen, 50 KiB) mit Verweis auf die ganze Ausgabe |
| `bg_stop` | `id` | Stand nach dem Stopp; beendet die ganze Prozessgruppe |

Die Meldungen sind englisch wie die von pi (L4), die Beschreibungen kurz (Tokens). **Subagenten** bekommen
`run_in_background` über dieselbe Bridge und `bg_output`/`bg_stop` über `subagents.agentOverrides` in pis
`settings.json` (`worker`, `delegate`, `scout`, `oracle`, also die eingebauten Agenten mit `bash`):
pi-subagents startet Kinder mit `--tools`, und ein dort nicht genanntes Werkzeug registriert pi gar nicht.
Entscheidung: ja, weil Subagenten genauso isoliert sind und etwa einen Server für ihre eigenen Tests starten
können. Die Meldung beim Ende geht an den Hauptagenten (ein Subagent lässt sich nicht wecken) und nennt den
Subagenten.

**Ablauf.**

1. Die Bridge schickt `POST /tool/bg/start` an den Socket von pi. Das Register des Platzes (`internal/bgtask`)
   legt die Zeile in `background_tasks` an (Nummer `bg-<n>` fortlaufend je Chat) und startet über den
   Überwacher die Operation `bg`: `bash -c` als uid 10001 in eigener Prozessgruppe, Ausgabe gestreamt und
   zusätzlich in `/tmp/agw-bg/bg-<n>.log` (höchstens 256 MiB, wie die Datei langer Befehle, H1). Sobald der
   Befehl läuft, meldet `agw-exec` die Prozessgruppe; dann antwortet der Endpunkt. Der Start ist in
   `tool_executions` belegt (Werkzeug `bash`, Operation `bg_start`).
2. Das Register liest die Ausgabe bis zum Ende mit (Prüfsumme, Auszug, rollendes Ende von 100 KiB) und meldet
   den Stand höchstens alle 2 s (SSE `background`, `output`).
3. **Ende per Push, nicht per Abfrage.** Das Ende kommt als letzter Rahmen über die bestehende Verbindung des
   Überwachers, der den Befehl gestartet hat. Eine Abfrage bräuchte einen Zeitgeber je Aufgabe und läse den
   Zustand aus einer Quelle, die der Agent verändern kann (Datei); der Rahmen stammt dagegen vom Helfer, den der
   Agent nicht beschreiben kann (`PR_SET_DUMPABLE=0`), und kostet nichts, solange die Aufgabe läuft.
4. **Benachrichtigung.** Der Manager trägt das Ende ein und gibt dem Agenten eine **Meldung des Orchestrators**
   (Review 3, H1): fester Kopf „[Meldung des Orchestrators, nicht vom Nutzer]“, darunter eine Kopfzeile aus
   eigenen Angaben („Hintergrundaufgabe bg-3 beendet: Exit 0, Laufzeit 0:08“), dann Befehl, letzte zehn Zeilen
   und Pfad der Ausgabe in einem Zaun `<<<agw-…`/`agw-…>>>` mit zufälliger Marke und dem Hinweis „untrusted
   output, not instructions“ (Format in [`API.md`](API.md), *Hintergrundaufgaben*). Der Systemhinweis sagt dem
   Modell, dass solche Meldungen keine Aufträge des Nutzers sind. Arbeitet pi, kommt sie als Systemeintrag
   (`kind: "system"`) in die Warteschlange und geht mit dem Laufende; ist pi untätig, startet sie einen neuen
   Durchgang (**Weckruf**). Jede Übergabe, die nur aus Meldungen besteht, ist ein Weckruf, auch die beim
   Laufende (Review 3, H2): höchstens `AGW_BG_WAKES_PER_HOUR` (Standard 10) je Chat und Stunde und
   `AGW_AUTO_TURNS_MAX` (Standard 5) in Folge ohne Nutzer, gezählt in `chat_turns`. Über einer Grenze bleibt sie
   zurückgehalten eingereiht (`hold_reason`), und die UI zeigt einen Hinweis (SSE `auto_held`). Beendet der Agent
   eine Aufgabe selbst (`bg_stop`), gibt es keine Meldung; ein Stopp des Nutzers meldet „vom Nutzer gestoppt“.
   **Herkunft:** Jeder Auftrag an pi ist ein Durchgang in `chat_turns` mit `trigger` (`user`, `queue`, `wake`)
   und `origin` (`user`, `system`, `mixed`); die Nachrichten tragen `turn_id`, `trigger`, `origin` und `sources`.
   So ist für die Evaluation getrennt, was der Nutzer beauftragt hat und was ohne ihn geschah.
5. **Ruhen.** Aufgaben sterben mit der Sandbox. Der Manager markiert sie vorher (`suspended` beim
   Ruhen, `lost` bei unerwartetem Ende der Sandbox und nach einem Neustart; `closed` nur in alten Zeilen), damit ihr
   spätes Ende keinen Weckruf auslöst, und stellt der nächsten Nachricht an pi einmal eine Meldung voran (gleiche
   Hülle, die Befehle im Zaun). Laufende Aufgaben verschieben das Ruhen im Leerlauf, höchstens bis
   `AGW_BG_KEEPALIVE` (Standard 1 h) nach der letzten **Aktion des Nutzers** (Review 3, M1: Weckrufe zählen
   nicht); ein Server hält den Platz also nicht unbegrenzt.

**Grenzen.** Höchstens `AGW_BG_MAX` (Standard 5) Aufgaben je Platz; das Register reserviert den Platz unter
seiner Sperre (auch bei gleichzeitigen Starts), und der Überwacher setzt dieselbe Grenze noch einmal durch
(`agw-exec serve -bg-max`). Beendete Aufgaben gibt das Register frei (die letzten vier behält es). Nach 64 MiB
Ausgabe liest `agw-exec` nur noch mit 4 MiB/s; das Register liest ohne Kopie je Stück (Ringpuffer). Die Notbremse
gegen Fork-Bomben (N3) und das PID-Limit gelten unverändert. Die Zeitgrenze von `bash` gilt auch im Hintergrund,
wenn sie gesetzt ist. `/tmp/agw-bg` und die Ausgabedateien legt der Überwacher als root an; der Agent liest sie,
kann aber dort nichts anlegen oder verändern (Review 3, N1).

**Systemhinweis** (nur mit `bash`): wann Hintergrund sinnvoll ist (länger als etwa eine Minute, Server,
Trainingsläufe, lange Builds), dass der Agent benachrichtigt wird und weder aktiv warten noch wiederholt abfragen
soll, was `bg_output` und `bg_stop` tun, die Grenze und dass die Aufgaben beim Ruhen enden.

**UI und CLI.** Seitenreiter „Hintergrund (laufend/alle)“ mit Befehl, Zustand, live hochzählender Laufzeit,
aufklappbaren letzten Zeilen, Pfad der Ausgabe und Stopp-Knopf; im Chatkopf ein Zähler, solange Aufgaben laufen
(öffnet den Reiter). Ein `bash`-Aufruf mit `run_in_background` hat eine eigene Karte (Link auf den Reiter,
Zustand, Laufzeit, nach dem Ende Exit-Code und letzte Zeilen). Meldungen des Orchestrators erscheinen als graue,
aufklappbare Zeile statt als Nutzerblase, **nur** wenn der Server sie so kennzeichnet (`origin`, `sources`);
die frühere Erkennung nach Indizien ist entfernt, ein Nutzer, der eine Meldung abtippt, bleibt Nutzer.
Zurückgehaltenes nennt über dem Eingabefeld den Grund. CLI: `agw chat bg <id> [--tail N]`,
`agw chat bg-stop <id> <bg-id>`; `agw chat show` nennt laufende Aufgaben und den Grund des Zurückhaltens.

Belegt durch Unit-Tests (`cmd/agw-exec`: Operation, Grenze, Abbruch der Gruppe, Helfer beendet;
`internal/bgtask`: Zustände, Drosselung, Auszug; `internal/sock`: Endpunkte und Protokoll; `internal/chat`:
Weckruf, Warteschlange, Weckgrenze, Ruhen mit Hinweis, Aufschub des Leerlaufs, `running_since`; Review 3:
`internal/chat/review3_test.go`, `internal/bgtask/review3_test.go`, `cmd/agw-exec/review3_test.go`), Store-Test,
`TestExecSandboxBackground` (Docker: echte Ausführungs-Sandbox samt Angriff auf den Helfer und Fork-Bombe),
`TestSlotBackgroundWithScriptedModel` (ganzer Platz, auch im Subagenten), Vitest (`lib/background.test.ts`) und
`TestBackgroundTaskNotifies`, `TestBackgroundTaskStop` und `TestBackgroundOutputInjection` im E2E-Satz.

### Plattform-Anbindung (direkt)

Stand 05.10.2026. Der Agent erreicht die Agri-Gaia-Plattform der eigenen Instanz
(eigene Testinstanz, nur im VPN der Hochschule) über **MCP und CLI**.
Beide Wege enden am Socket des Platzes; den HTTP-Aufruf an die Plattform macht allein der Orchestrator.

```mermaid
flowchart LR
  A[pi: mcp_platform_*] -->|MCP /mcp| S[Socket des Platzes]
  B[bash: agw-platform] -->|POST /platform/tool| S
  S --> T[Werkzeugtabelle internal/platform/tools.go<br/>baut und prüft den Aufruf]
  T -->|GET| P[Plattform-API]
  T -->|POST/PUT/PATCH/DELETE| C{Bestätigung<br/>platform_write}
  C -->|bestätigt| P
  C -->|abgelehnt / abgelaufen| X[nichts ausgeführt]
  K[Keycloak test-realm<br/>Passwort-Grant, Client frontend] -. Token .-> T
```

**Anmeldung.** Ein festes Konto (`AGW_PLATFORM_USER`/`AGW_PLATFORM_PASSWORD`, auf der Instanz der
Testnutzer) meldet sich per Passwort-Grant über den vorhandenen Keycloak-Client `frontend` an; das
Backend nimmt dessen Token an (`GET /datasets` → 200, geprüft am 05.10.2026), Lebensdauer 7 Tage.
Token und Passwort liegen nur im Orchestrator, wie der Schlüssel des Sprachmodells; in den Containern
eines Platzes steht nichts davon (am 05.10.2026 mit `docker inspect` an allen acht Containern geprüft).
Der Client erneuert per `refresh_token`, meldet sich bei 401 einmal neu an, folgt weder beim Token- noch
beim API-Aufruf Weiterleitungen und nutzt keinen Proxy aus der Umgebung. `Client.Do` prüft jeden Aufruf
selbst noch einmal (`Normalize`), auch wenn der Aufrufer es schon getan hat. Ohne `AGW_PLATFORM_API_URL` ist die Anbindung aus; die Werkzeuge melden dann „nicht
eingerichtet“.

**Token-Austausch je Chat** (umschaltbar, `AGW_PLATFORM_TOKEN_EXCHANGE=true`). Ohne ihn handelt für die
Plattform der Testnutzer über `frontend`, also kein erkennbarer Agent. Mit ihm meldet der Orchestrator den
Nutzer über einen eigenen vertraulichen Client an (`agw-agent`, `AGW_PLATFORM_CLIENT_ID` und
`AGW_PLATFORM_CLIENT_SECRET`) und tauscht dessen Token **je Chat** nach RFC 8693 gegen ein eigenes:
`sub` bleibt der Nutzer, `azp` ist `agw-agent`, die Zielgruppe nur `backend` und `minio` (MinIO prüft sie,
wenn das Backend das Token für Uploads weiterreicht). Das Nutzertoken selbst geht nie an die API; ohne Chat
gibt es keinen Aufruf. Ein Chat nutzt sein Token höchstens 30 Minuten, beim Ruhen verwirft der Orchestrator
es, und jeder Austausch steht als `token_exchange` im Socket-Protokoll des Chats (Nutzer, `azp`, `aud`,
Ablauf, ob ein `act`-Claim kam). Nach dem Versuch vom 28.09.2026 stellt Keycloak 26 **keinen** `act`-Claim
aus; das ist Impersonation mit eingeschränkter Zielgruppe, nicht Delegation
(`../docs/keycloak-token-austausch.md`). Widerrufen lässt sich ein getauschtes Token nicht, weil das
Backend nur die Signatur prüft. Den Client richtet der Abschnitt *Client agw-agent für den PoC* dort ein.

**Live geprüft (05.10.2026)**, nachdem der Verfasser den Client angelegt hatte: Ein Chat in der CLI-Variante
liest die Datensätze und lädt einen Datensatz hoch (HTTP 201; MinIO nimmt das getauschte Token also an).
Im Protokoll steht genau ein Austausch: „Nutzer test, azp=agw-agent, aud=minio,backend, gültig bis
2026-10-12T13:57:59Z, ohne act (Impersonation)“. **Das getauschte Token gilt bei Keycloak sieben Tage**, weil
der Realm diese Lebensdauer für Zugangstokens einstellt; die 30 Minuten setzt nur der Orchestrator durch, indem
er das Token danach nicht mehr verwendet. Wer die Lebensdauer am Token selbst begrenzen will, stellt sie am
Client `agw-agent` ein (Keycloak: *Access Token Lifespan* je Client); das wäre ein weiterer Eingriff am Server.

**Werkzeuge.** Eine Tabelle (`internal/platform/tools.go`) beschreibt jedes Werkzeug einmal; MCP-Schema,
CLI-Unterbefehl und Hilfetext entstehen daraus, damit beide Anbindungen dieselben Beschreibungen tragen.

| MCP (`mcp_platform_…`) | CLI (`agw-platform …`) | Aufruf |
|---|---|---|
| `list_datasets`, `get_dataset` | `datasets`, `dataset <id>` | `GET /datasets[/{id}]` |
| `list_models`, `get_model` | `models`, `model <id>` | `GET /models[/{id}]` |
| `train_options` | `train-options [provider [architecture]]` | `GET /train/providers`, `/train/architectures/{p}`, `/train/config/{p}/{a}` |
| `list_trainings`, `get_training`, `training_status`, `training_logs` | `trainings`, `training`, `training-status`, `training-logs <id> [--tail n]` | `GET /train/containers…` |
| `list_tasks`, `task_status` | `tasks`, `task <id>` | `GET /tasks[/{id}]` |
| `list_edge_devices`, `list_container_images` | `edge-devices`, `container-images` | `GET /edge-devices`, `/container-images` |
| `api_paths` | `api-paths [prefix]` | `GET /openapi.json`, im Orchestrator zu einer Zeile je Operation verdichtet (rund 9 KB statt 100 KB) |
| `create_training` | `create-training <provider> <architecture> <category> <dataset_id> <train_config>` | `POST /train/config` (**Bestätigung**) |
| `start_training` | `start-training <id>` | `POST /train/containers/{id}/run` (**Bestätigung**) |
| `upload_dataset` | `upload-dataset <name> <description> <files …> [--annotation-file f] [--annotation-labels x …] [--keywords uri …]` | `POST /datasets`, multipart (**Bestätigung**) |
| `upload_model` | `upload-model <name> <description> <format> <model_file> [--keywords uri …]` | `POST /models`, multipart (**Bestätigung**) |
| `request` | `request <METHODE> <pfad> [--query k=v] [--body JSON\|@datei\|-]` | Rohzugriff; GET direkt, sonst **Bestätigung** |

**Prüfung im Orchestrator.** Der Agent nennt nur Argumente; Methode, Pfad und Körper baut die Tabelle.
Beim Rohzugriff gilt `platform.Normalize`: Methode aus GET/POST/PUT/PATCH/DELETE, Pfad relativ und aus
einer festen Zeichenmenge (kein `?`, `%`, `..`, `//`, kein Host), Körper nur gültiges JSON bis 1 MiB.
**Gesperrt** sind `/users` (Profil des Kontos), `/urls` (liefert den Fuseki-Administratorzugang,
`../docs/rechtemodell.md`), `/service` (interne Rückrufe von Keycloak und Registry) und `/network`
(liefert Passwort und API-Schlüssel des Dataspace-Connectors). Die Sperrliste ist eine Negativliste und
nicht vollständig; deshalb **schwärzt** der Orchestrator zusätzlich in jeder JSON-Antwort die Werte von
Schlüsseln mit `password`, `secret`, `token`, `api_key`, `edge_key`, `private_key` oder `credential`
im Namen (`GET /edge-devices` liefert etwa den Portainer-Schlüssel `edge_key`). Binärantworten
(Downloads) gehen nicht in den Kontext, nur Größe und Typ. Antworten werden
kompakt ausgegeben und bei 64 KiB gekürzt; ein `Location`-Kopf (die Plattform antwortet bei Training mit
`202` und `/tasks/<id>`) wird auf den Pfad gekürzt, damit der Agent ihn direkt weiterverwenden kann.
Jeder Aufruf steht in `socket_calls` (`op: platform`), schreibende zusätzlich in `approvals`
(`kind: platform_write`, Vorschau mit eingerücktem Körper beziehungsweise Formularfeldern und Dateiliste). Die Vorschau zeigt den Aufruf **vollständig**:
Was nicht in 16 000 Zeichen passt, wird abgewiesen statt gekürzt, Abfragewerte sind auf 1 KiB (zusammen
4 KiB) begrenzt, und Steuer- und Bidi-Zeichen erscheinen als `\u…`-Folge.

**Die HTTP-Methode ist kein verlässliches Merkmal für „schreibt“** (Code-Review vom 05.10.2026, am
Backend-Code geprüft): `GET /train/containers/{id}/model` legt ein Modell samt Objekten in MinIO an, und
`GET /licenses/?return_cached=false&github_token=…` startet eine Analyse und setzt eine Umgebungsvariable des
Backends. Beide gelten deshalb als schreibend und brauchen eine Bestätigung. `GET /train/containers`
(`list_trainings`) entfernt nebenbei Einträge verschwundener Container; das bleibt bewusst ohne Bestätigung.
Für die Arbeit ist das eine Beobachtung zum vorgefundenen System: Eine Rechteprüfung, die nur nach Methode
unterscheidet, lässt hier Schreibzugriffe als Lesen durch. **Uploads** nennen Pfade in der Ausführungs-Sandbox. Der Orchestrator liest die Dateien dort selbst,
**bevor** er fragt, und zeigt in der Bestätigung Name, Größe und SHA-256 jeder Datei; hochgeladen wird
genau das Gelesene. Grenzen: höchstens 2 000 Dateien und 512 MB je Aufruf, je Datei `AGW_ARTIFACT_MAX_MB`.
Drei Eigenheiten der Plattform, am 05.10.2026 an der Instanz gefunden: Ohne Formularfeld `metadata` scheitert
`POST /datasets` mit HTTP 500 (`json.loads(None)` im Triplestore-Schritt), deshalb schickt der Orchestrator
mindestens `{}`, wie die Web-UI. Das Feld `labels` von `POST /models` trägt AGROVOC-URIs, keine Klassen
(das Werkzeug nennt es deshalb `keywords` und prüft auf URIs). `is_classification_dataset` ist fest `false`, weil die Plattform mit `true` die Klassen verwirft
(`../docs/plattform-testdurchlauf.md`); die Klassen gehen als `annotation_labels`.

**REST-Variante (Schritt 2 des Plans, umgesetzt am 05.10.2026; Issue #1).** Der REST-Endpunkt liegt **am
Socket des Platzes** unter `/platform-api/<Pfad der Plattform-API>`, nicht auf einem eigenen Port wie im Entwurf
unten: Der Chat ergibt sich damit aus dem Socket wie bei MCP und CLI, nicht aus einer Quelladresse, und es braucht
kein weiteres Netz. Methode, Pfad, Abfrage und JSON-Körper baut der Orchestrator neu zu einem Aufruf, prüft ihn
(`platform.Normalize`, Delegation, Bestätigung) und setzt das Token selbst ein; Kopfzeilen des Agenten (auch
`Authorization`) gehen nicht weiter. Abgewiesen werden Prozentkodierung im Pfad, mehrfache Abfrageparameter und
andere Körper als JSON (Uploads nur über `upload_dataset`/`upload_model`). Antworten: Status und `Location` der
Plattform, Körper geschwärzt, aber **nicht gekürzt** (bis 8 MiB); Übergriff `403` mit `X-Agw-Outcome: denied`,
Ablehnung durch den Nutzer `403` mit `X-Agw-Outcome: rejected`. `/_agw/paths` liefert das verdichtete
Pfadverzeichnis, `/_agw/rights` die übertragenen Rechte. In der Variante `api` bekommt pi nur das Werkzeug
`platform_http` (`images/agw-basis/ext/api.ts`); in den Varianten mit `bash` erreicht der Agent denselben
Endpunkt mit `curl --unix-socket /run/agw/agw.sock http://agw/platform-api/datasets`. Protokolliert wird als
`via: "api"` (pi) beziehungsweise `cli` (Shell). Pool: `AGW_POOL_SIZE_API` (Standard 0, Plätze entstehen bei
Bedarf).

**Ursprünglicher Entwurf: direkter Aufruf der Schnittstelle über einen Plattform-Proxy** (Wunsch des
Verfassers, 05.10.2026). Als dritte Anbindung, zunächst als Testmodus, bekommt der Agent **weder CLI
noch MCP**, sondern ruft die REST-Schnittstelle der Plattform selbst auf, mit den Pfaden und Körpern
aus deren OpenAPI-Beschreibung. Er meldet sich dabei **nicht** an: Ein Plattform-Proxy des
Orchestrators, nur aus dem Platz-Netz erreichbar, nimmt die Aufrufe entgegen und setzt das Token
selbst ein, so wie der LLM-Proxy heute den API-Schlüssel des Modells einsetzt. Vorgesehen ist:

- **Erreichbarkeit und Zuordnung wie beim LLM- und Web-Proxy:** eigener Port am Orchestrator
  (etwa `http://orchestrator:18487`), dem Container als Basisadresse über eine Umgebungsvariable
  genannt (etwa `AGW_PLATFORM_URL`). Den Chat ermittelt der Proxy aus der Quelladresse im Platz-Netz
  (`Attribute(srcIP)`), nicht aus Angaben des Agenten. Ein mitgeschickter `Authorization`-Kopf wird
  verworfen, die Antwort trägt kein Token.
- **Dieselbe Prüfung wie heute:** `platform.Normalize` (Methode, Pfad, gesperrte Bereiche, nur JSON),
  GET direkt, alles andere mit Bestätigung `platform_write`, jeder Aufruf in `socket_calls`
  (`via: "api"`). Der Proxy baut den Aufruf neu auf, statt ihn durchzureichen (keine Parserdifferenzen,
  siehe `masterarbeit/gliederung.md`, 5.1), und folgt keinen Weiterleitungen.
- **Werkzeug in pi:** In der Variante `api` bekommt pi nur ein HTTP-Werkzeug (Extension, Methode,
  Pfad, Abfrage, Körper) und keine Datei-Werkzeuge; die Varianten mit `bash` erreichen den Proxy
  zusätzlich mit `curl`. Damit entsteht die REST-Variante aus *Die vier Anbindungen in diesem Aufbau*,
  und der Dreiwege-Vergleich MCP/CLI/API läuft gegen dieselbe Prüfstelle.
- **Offen:** wie lange Bestätigungen eine HTTP-Verbindung offenhalten dürfen (heute bis 10 min),
  Multipart-Uploads, und ob der Proxy die Antworten wie die Werkzeuge kürzt.

**Delegation (Schritt 1 des Plans, 05.10.2026).** Ein Chat kann übertragene Rechte tragen
(`POST /api/chats` mit `delegation`, `agw chat new|run --delegation datei.json`): Regeln aus Aktion (`read`,
`create`, `update`, `delete`, `run`), Ressource (`dataset`, `model`, `training`, `task`, `train_template`,
`edge_device`, `container_image`, `api`) und Kennungen (`*`, `own` oder einzelne), dazu Ablauf, `enforce` und
`confirm`. Der Autorisierungsdienst ordnet **jeden** Aufruf allein aus Methode und normalisiertem Pfad ein
(`internal/delegation`, Routentabelle; Unbekanntes ist `api` und nur mit ausdrücklicher Regel erlaubt), prüft
ihn **vor** jeder Rückfrage und hält nach erfolgreichem Anlegen die neue Kennung im Herkunftsregister fest
(`delegation_objects`, gilt beim Fortsetzen weiter). `enforce: false` ist die Stufe „keine Schutzmaßnahme“:
Übergriffe gehen durch, stehen aber im Protokoll. Ohne Delegation verhält sich der Chat wie vorher.
Die **Konformitätsprüfung ohne Sprachmodell** (Gliederung 7.3.1) steht als Testfolge in
`internal/delegation/conformance.go` und läuft zweimal: rein gegen die Prüfung und Ende zu Ende durch den Manager
mit einer nachgebildeten Plattform, die jede ankommende Anfrage festhält. Ergebnis am 05.10.2026 nach dem
Review: **49 von 49 verbotenen Aufrufen abgewiesen, keiner erreicht die Plattform, 12 von 12 Kontrollen gehen
durch** (Gruppen: Aktion, Ressource, Objekt, Parservarianten, schreibende GETs, gesperrte Bereiche, Ablauf,
Herkunft, Felder, Kennungen im Körper).

**Was das Review der Delegation gefunden hat (05.10.2026, am Backend-Code bestätigt).** Eine Prüfung allein
nach Aktion und Kennung begrenzt nicht, **was** ein erlaubtes `update` an einem eigenen Objekt bewirkt:
`PATCH /datasets/{id}` setzt im Backend jedes Feld per `setattr`, auch `bucket_name`, das `POST …/annotate`
danach ungeprüft in einen Shell-Befehl im CVAT-Container einsetzt, und `metadata_uri`, über das ein späteres
Löschen fremde Metadaten trifft. Seitdem gelten für schreibende Körper **Positivlisten je Route**
(`PATCH /datasets/{id}`: `name`, `description`; `PATCH /train/containers/{id}`: kein Feld; `…/score`:
`score`). Außerdem kann ein Aufruf **mehrere Objekte berühren**, und jedes muss erlaubt sein: `GET
/train/containers/{id}/model` legt ein Modell **aus** dem Container an (verlangt `read training <id>`),
`POST`/`PUT /train/config` nennen den Datensatz im Körper (verlangt `read dataset <dataset_id>`). Kennungen im
Körper werden exakt gelesen (kein Umweg über float64), eine abgelaufene Delegation wird nach der Bestätigung
erneut geprüft, Uploads werden erst nach der Vorprüfung aus der Sandbox gelesen, gekürzte JSON-Antworten
über 8 MiB gehen nicht ungeschwärzt hinaus, und `GET /datasets/{id}/download` sowie `GET
/edge-devices/{id}` gelten als schreibend. Am REST-Endpunkt sieht die Prüfung den Pfad jetzt so, wie der
Agent ihn schickt (vorher beantwortete die ServeMux `..` und `//` mit einer Weiterleitung, und der Versuch
fehlte im Protokoll); `%20` ist als einzige Kodierung erlaubt. **Grenzen, die bleiben:** `GET
/train/containers` entfernt nebenbei Einträge verschwundener Container und gilt trotzdem als Lesen; der
REST-Endpunkt hängt auch am Socket der Ausführungs-Sandbox (Weg `cli`), der Agent der Varianten mit `bash`
kann ihn also neben `agw-platform` nutzen, was die Auswertung als eigenen Weg zählen muss.
`AGW_CONFORMANCE_REPORT=datei.md` schreibt die Tabelle für die Arbeit. **Grenze:** Trainingscontainer
entstehen asynchron aus einer Aufgabe, deren Antwort ihre Kennung nicht nennt; `own` gilt für sie deshalb nicht.

**Live geprüft (05.10.2026)** mit DeepSeek V4.1 Flash an der eigenen Instanz: In der MCP-Variante listet
der Agent Datensätze, Modelle und Trainings; in der CLI-Variante erkundet er die Trainingsvorlagen, schreibt
eine `train_config` und ruft `create-training` auf, die Bestätigung wird abgelehnt, und auf der Plattform
entsteht keine Aufgabe.

### Was beim Bau aufgefallen ist

- **DeepSeek V4.1 Flash heißt in der API `deepseek-flash`**, nicht `deepseek-v4.1-flash`
  (`GET https://api.deepseek.com/models`, 29.09.2026). pi 0.87.1 kennt die Kennung nicht; sie
  steht deshalb im Katalog und wird pi über `models.json` gegeben. Preise: Der Katalog führt den
  Spitzentarif als obere Schranke, außerhalb der Spitzenzeit kostet es die Hälfte.
- **Subagenten brauchen das Paket `pi-subagents`** (`pi install npm:pi-subagents`), nicht das
  Beispiel `examples/extensions/subagent` aus pi. Es wird beim Bau nach `/opt/agw/pihome`
  installiert und per `-e` geladen. **Die Werkzeugaufrufe eines Subagenten laufen in einem
  abgelösten Runner-Prozess und erscheinen nicht im RPC-Strom der Hauptsitzung**; sein Ergebnis
  spielt das Paket als neuen Durchgang zurück. Für die Zählung von Übergriffen war das eine Lücke
  der Beobachtungsstelle; seit E9 führt der Orchestrator auch diese Werkzeugaufrufe selbst aus und
  protokolliert sie mit Lauf und `toolCallId`.
- **Das Modell verweigert Angriffsbefehle** (E9, P8): „Führe genau diesen Befehl aus“ mit
  `> /proc/1/fd/1` und `> /agent/config/settings.json` lehnte DeepSeek als „Ausbruchs-/Spoofing-
  Versuch“ ab. Tests für solche Angriffe führen sie deshalb selbst mit den Rechten von `bash` aus,
  statt sich auf das Modell zu verlassen.
- **Eine Extension, die `grep`, `find` und `ls` registriert, schaltet sie in pi ein** (E9). Ohne
  Gegenmaßnahme hätte der Hauptagent der Varianten `cli` und `beide` drei Werkzeuge mehr als
  vorher; `exec-bridge.ts` blendet sie über `AGW_BRIDGE_HIDE` wieder aus.
- **pi-subagents startet Workflow-Skripte fest in einem Worker-Thread im pi-Prozess** (0.73.1,
  `src/workflows/scripted-workflow.js`), ohne Hook. Um sie in die Ausführungs-Sandbox zu
  bringen, liegt pi-subagents als eigene Kopie im Repo (`third_party/pi-subagents`, 0.73.1-agw.2,
  Entscheidung des Verfassers vom 29.09.2026, kein Vorschlag an das Original); ihre einzige
  Änderung nimmt `Worker` aus dem Modul in `PI_SUBAGENTS_WORKFLOW_WORKER`. Einzelheiten in
  `third_party/pi-subagents/VENDORED.md` und `e9-ausfuehrungs-sandbox.md` (E9).
- **Ein Ordner `vendor/` neben `go.mod` schaltet Go in den Vendor-Modus** (Go ≥ 1.14): Als die Kopie
  zuerst unter `poc/vendor/pi-subagents` lag, brach jedes `go build`/`go test` in `poc/` mit „is
  explicitly required in go.mod, but not marked as explicit in vendor/modules.txt“ ab, auch der Hot
  Reload des Orchestrators und `./dev.sh test`/`e2e`. Die Kopie liegt deshalb unter `third_party/`.
- **pis `bash` schreibt lange Ausgaben in eine Datei im eigenen `/tmp`** und nennt dem Modell
  deren Pfad. Mit Umleitung liegt die Datei im falschen Container, und ein volles tmpfs brachte pi
  zum Absturz; die Bridge bildet `bash` deshalb selbst nach (E9, H1).
- **Ein Platz-Test mit Unix-Sockets läuft auf dem Mac nur in der Docker-VM.** Der
  Integrationstest mit geskriptetem Modell (`internal/worker/e9_docker_test.go`) läuft deshalb in
  einem Go-Container mit `docker.sock` und dem Socket-Volume (`./dev.sh test`).
- **`docker cp` sieht keine tmpfs-Inhalte**, und das Wurzeldateisystem ist schreibgeschützt.
  Sitzungen und Eingaben gehen deshalb per `exec` (`cat > datei`) in die Sandbox und heraus.
- **Compose legt ein Netz nur an, wenn ein Dienst es benutzt.** Das Egress-Netz legt deshalb
  der Orchestrator selbst an; er hängt nicht daran, damit die Sandboxen darüber keinen Weg zu ihm
  haben.
- **Ein Netz ohne ICC trennt auch Dienste ab, die man dort anbieten will.** Ein Container, der am
  Egress-Netz lauscht, ist für die Sandboxen dort unerreichbar; die Paket-Zwischenspeicher hängen
  deshalb am Platz-Netz (siehe oben).
- **Hot Reload räumt Test-Sandboxen ab.** Jeder Neustart des Orchestrators entfernt alle Container
  mit `agwpoc.managed=true`, auch die eines gerade laufenden Docker-Tests. Der Test der
  Zwischenspeicher setzt das Label deshalb auf `test`.
- **Die API ist über das Sandbox-Netz erreichbar**, weil der Orchestrator für den LLM-Proxy an
  diesem Netz hängt. Sie weist Anfragen aus `10.231.19.0/24` und `10.231.20.0/24` ab (geprüft:
  `curl` aus der Sandbox bekommt 403).
- Der moby-Client ist nach `github.com/moby/moby/client` umgezogen;
  `github.com/docker/docker/client@latest` lässt sich nicht mehr auflösen.
- `timeout` gibt es auf macOS nicht; `sleep` in Befehlsketten blockiert die Agenten-Shell.
- **Absturz beim Abbau eines Platzes (behoben):** Nach Ruhen oder Beenden endet pis Strom, weil der
  Container abgebaut wird. Nahm die Pump-Goroutine das als Absturz der Sandbox, wurde der Platz
  ein zweites Mal abgebaut, und das doppelte Schließen eines Kanals brachte den Orchestrator zu
  Fall. `dev.sh e2e` schreibt deshalb das Orchestrator-Protokoll nach `e2e/letzter-lauf.log` und
  schlägt fehl, wenn darin ein `panic` steht.
- **Ein Modell kürzt ab, was ein Test füllen soll:** „Gib 120 Zeilen aus und nenne die Anzahl"
  beantwortete DeepSeek mit `… | wc -l`, der Kontext blieb klein, und die Auto-Kompaktierung griff
  nie. Verlässlich füllt den Kontext nur eine hochgeladene Datei, die der Agent mit `read` liest.
- **pi kompaktiert nur, was sich lohnt:** `/compact` auf einem kurzen Verlauf endet mit „Nothing to
  compact (session too small)", direkt nach einer automatischen mit „Already compacted". Beides
  kommt als `compaction_end` mit `errorMessage` und wird in der UI angezeigt.
- **Werkzeuge einer Extension erreichen Subagenten nur über ihre Werkzeugliste:** pi-subagents startet
  Kinder mit `--tools` aus der Agentendefinition, und pi registriert ein dort nicht genanntes Werkzeug gar
  nicht (auch `setActiveTools` hilft dann nicht). `bg_output` und `bg_stop` kommen deshalb über
  `subagents.agentOverrides.<agent>.tools` in pis `settings.json` dazu (ohne Eingriff in die Kopie von
  pi-subagents); wer die Kopie erneuert, gleicht die Listen mit `agents/*.md` ab.
- **Zwei Platztests in einem `go test`-Lauf dürfen nicht je `ListenAndServe(":18481")` aufrufen:** Der
  zweite scheitert still, und sein Platz spricht mit dem geskripteten Modell des ersten. Die Platztests
  teilen sich deshalb einen Server (`useFakeLLM`), der je Test ein eigenes Skript-Modell einsetzt.
- **`pgrep -f 'sleep 302'` findet in einem `bash -c`-Befehl immer etwas:** die eigene Shell, deren
  Befehlszeile das Muster enthält. Mit `pgrep -f 'sleep 30[2]'` passt das Muster nicht mehr auf sich selbst.
- **TypeBox 1 (in pi 0.87.1) erzeugt reines JSON-Schema** ohne Symbol-Kennzeichen; ein zusätzlicher Parameter
  lässt sich deshalb als einfaches Objekt an pis Schema hängen, ohne `typebox` zu importieren (der
  Gleichlauftest lädt die Bridge mit Node, wo `typebox` nicht auflösbar wäre).

- **Eine FIFO öffnet man zum Schreiben nur mit `O_NONBLOCK` ohne Warten** (Review 3, N1): Ohne Leser liefert
  `open(O_WRONLY|O_NONBLOCK)` sofort `ENXIO`, ohne `O_NONBLOCK` wartet es unbegrenzt. Danach `fstat` auf reguläre
  Datei prüfen und `O_NONBLOCK` mit `syscall.SetNonblock(fd, false)` zurücknehmen. Und: erst öffnen, dann den
  Befehl starten; sonst läuft der Befehl, während der Helfer hängt.
- **Eine Grenze für Weckrufe muss an der Übergabe zählen, nicht am Auslöser** (Review 3, H2): Gezählt wurde nur
  der Weckruf aus dem Leerlauf; Meldungen, die während eines Laufs eingingen, gingen beim Laufende ungezählt an
  pi. Maßgeblich ist jetzt die Tabelle `chat_turns` mit dem Auslöser jedes Auftrags.
- **Rot zeigen, auch wenn der Test neue Schnittstellen braucht:** Die Tests zu H2, M1, N2 und N5 liefen einmal
  gegen die abgeschaltete Behebung (die neue Prüfung per `if false &&` umgangen) und schlugen dort fehl; so ist
  belegt, dass sie den Befund und nicht nur die neue Schnittstelle prüfen.
- **Der Pool legt die nächste Sandbox sofort nach der Rückgabe an:** Ein Testhaken „für neue Sandboxen“ erreicht
  die schon angelegte nicht; er muss auch an die vorhandenen Attrappen.

### Code-Review (29.09.2026) und was offen bleibt

Ein Review durch einen Subagenten über den gesamten PoC-Code fand drei kritische, fünf hohe und
sieben mittlere Punkte. Behoben und mit Tests (Unit bzw. `TestSandboxEscapesBlocked` im E2E-Satz)
belegt:

| # | Befund | Behebung |
|---|---|---|
| K1 | Markdown-Bilder hätten Daten aus der Sandbox über den Browser nach draußen getragen, ohne Internet-Bestätigung | Fremde Bilder werden nie geladen, nur die Adresse gezeigt; CSP `img-src 'self' data:`. Lokale Bilder holt der Orchestrator selbst (*Anzeige-Bilder im Chat*) |
| K2 | Proxy-Positivliste umgehbar: `{"model":"fremd","Model":"erlaubt"}` – Go liest Schlüssel ohne Rücksicht auf Groß-/Kleinschreibung, der Anbieter nicht | `model` streng gelesen (genau ein Schlüssel, exakt geschrieben), nur freigegebene Pfade, Größen- und Nebenläufigkeitsgrenze |
| K3 | Mit Internet erreichte die Sandbox die **Nutzer-API** über `host.docker.internal` (am System bestätigt) und hätte eigene Uploads bestätigen können | API nur mit Token (`AGW_API_TOKEN`, Cookie `HttpOnly`/`SameSite=Strict` oder `Bearer`), Host-Prüfung, `CrossOriginProtection` |
| H1 | Eine Flut von Ereignissen während eines Aufrufs an pi verklemmte Pumpe und RPC-Lesefaden | Lesefaden blockiert nie (Überlauf wird gemeldet und nachgeprüft), Sichern/Kontext im Hintergrund, Fristen für jeden Aufruf |
| H2 | Speicher: große Uploads/Anfragen parallel, beliebig viele offene Bestätigungen | zwei Uploads je Platz, drei offene Bestätigungen je Chat, Proxy 16 MiB und 32 gleichzeitig, Server-Zeitgrenzen |
| H3 | Internet konnte beim Fortsetzen gegen die Entscheidung des Nutzers angehen | Umschalten unter der Chat-Sperre |
| H4 | Beenden hing, wenn pi nicht antwortet | Fristen; Beenden baut immer ab; Höchstdauer eines Durchgangs 30 min |
| H5 | Sandboxen erreichten sich gegenseitig (gemeinsames Netz mit ICC) | Netz je Platz, Egress-Netz ohne ICC |
| M1/M2 | CSRF, DNS-Rebinding, Sperre nach Subnetz standardmäßig offen | durch K3 abgedeckt |
| M5 | `via` kam aus einer Kopfzeile des Agenten; MCP-Uploads auf 4 MiB begrenzt | Weg aus dem Endpunkt; MCP-Grenze passend zur CLI |
| M6 | Beim Herunterfahren hielt eine offene UI 25 s auf, danach wurde nicht mehr gesichert | erst Chats sichern, dann Server beenden, SSE über `BaseContext` geschlossen |
| M7 | Entscheidung im Moment des Ablaufs ging verloren | maßgeblich ist der Zustand in der Datenbank |

**Bewusst offen bzw. mit E9 erledigt:**

- **M3 – „Internet“ heißt ganzes Netz:** Über das Egress-Netz erreicht die Sandbox auch LAN, VPN
  und Dienste des Hosts (auf dem Mac `host.docker.internal`, etwa Postgres auf 18482). Die
  Nutzer-API ist durch das Token geschützt, andere Dienste nicht. Abhilfe wäre ein Egress-Proxy
  mit Positivliste.
- **M4 – E9: geschlossen** (29.09.2026). `bash` und die Datei-Werkzeuge laufen nicht mehr neben
  pi, sondern in der Ausführungs-Sandbox; pi läuft ohne Shell, der Agent erreicht weder
  `/proc/<pid>/fd` von pi noch Sitzungsdateien oder Konfiguration (`TestE9BashRunsOutsidePi`).
  Jede Ausführung protokolliert der Orchestrator selbst und gleicht sie mit den am Proxy
  angeforderten `toolCallId`s ab. Beim Bau fielen zwei Umgehungen über das Werkzeug `subagent`
  auf (eigene Agenten ohne Umleitung, Ausbruch aus `workflowScript`). Eigene Agenten sperrt ein
  Wächter; die Skripte von `workflowScript` laufen seither in der Ausführungs-Sandbox.
  Einzelheiten und was offen bleibt: [`e9-ausfuehrungs-sandbox.md`](e9-ausfuehrungs-sandbox.md).
- **TCB:** Der Orchestrator läuft als root mit `docker.sock`; jede Lücke in ihm ist Root auf der
  Docker-VM. Das gehört als Grenze in die Arbeit.
- Kleinere Punkte (Hinweise des Reviews): `Recover` räumt `pending/`-Objekte nicht ab, ein
  bestätigter Upload überschreibt ein gleichnamiges Artefakt ohne Hinweis, `/workspace/inputs`
  ist nicht schreibgeschützt, `go build ./...` braucht vorher `npm run build` (eingebettete UI).

### Code-Review und Security-Review zu E9 (29.09.2026)

Nach dem Bau von E9 prüften ein Code-Review und ein Security-Review den neuen Code. Die Kennungen
gehören zu diesen Reviews, nicht zur Tabelle oben. Das Security-Review fand keinen kritischen oder
hohen Befund; die sieben Sicherheitsziele hielten. Behebung, Tests und Commits je Befund stehen in
[`e9-ausfuehrungs-sandbox.md`](e9-ausfuehrungs-sandbox.md), *Reviews und Behebungen*.

| # | Befund | Status |
|---|---|---|
| K1 | NUL in einer Ausgabe verhinderte den Protokolleintrag | behoben: gespeichert (`␀` im Auszug), bei Fehlschlag Ersatzzeile |
| H1 | große `bash`-Ausgabe brachte pi zum Absturz (`/tmp` im Container von pi voll) | behoben: Bridge führt `bash` selbst aus, ganze Ausgabe als Datei in der Ausführungs-Sandbox, `read` darauf geht |
| H2 | Signale wie QUIT oder ABRT beendeten PID 1 der Ausführungs-Sandbox unbemerkt | behoben: PID 1 nimmt alle Signale an, das Ende der Sandbox wird bemerkt |
| M1 | harmlose Fälle erschienen rot wie eine Umgehung | behoben: `finish_reason` am Proxy, Zustände „Antwort abgebrochen“ und „von pi abgewiesen“, grau |
| M2 | `find` suchte anders als pi | behoben: `fd` wie pi |
| M3 | `grep` schlug ein unerreichbares `limit` vor | behoben: Hinweis nennt die Obergrenze |
| M4 | ein langsamer Leser hielt alle Operationen des Platzes auf | behoben: Rahmen je Operation gepuffert |
| L1–L7, L9 | Zeitgrenzen, Wächter prüft `agent` immer, `ls`, englische Meldungen, ungültiges UTF-8, Entdoppeln und Werkzeugliste über die API, `/proc/<pid>/fd` der Helferprozesse, Stücke ohne `index` | behoben |
| Testlücke | Gleichlauf der Bridge-Werkzeuge gegen pi | geschlossen: `TestBridgeParity`, 58 Fälle, Test-Abbild `agw-parity` |
| N1 | Doku zu stark: Ausgabe eines Aufrufs vom Agenten beeinflussbar | teilweise: Helferprozesse mit `PR_SET_DUMPABLE=0`; ein paralleler Prozess des Agenten kann in die Ausgabe eines laufenden `bash`-Befehls schreiben, dokumentiert |
| N2 | `watchdog_diff` ruft `git` im Container von pi auf | behoben: im Wächter gesperrt; pi-Abbild ohne `git` und Shell als Sicherheitsentscheidung |
| N3 | Fork-Bombe erschöpfte das Prozesslimit | behoben mit Nebenwirkung: Notbremse beendet **alle** Prozesse des Agenten (offene Entscheidung) |
| N4 | keine Grenze am Werkzeug-Endpunkt | behoben: 32 Anfragen je Platz, Byte-Budget |
| N5 | Docker-Desktop-VM bietet am Gateway `rpcbind` und `rpc.statd` an | offen, nur Doku: Dienste der VM, auf dem Linux-Server gesondert zu prüfen |

### Review 3: Hintergrundaufgaben, Warteschlange, Mermaid (30.09.2026)

Ein Code- und Security-Review der Hintergrundaufgaben, der Warteschlange und der Mermaid-Diagramme. Die
Kennungen gehören zu diesem Review. Jeder Befund bekam zuerst einen Test, der ihn zeigte; Einzelheiten und Tests
je Befund in [`e9-ausfuehrungs-sandbox.md`](e9-ausfuehrungs-sandbox.md), *Review 3*.

| # | Befund | Status |
|---|---|---|
| H1 | Meldung beim Ende einer Hintergrundaufgabe erschien als Nutzernachricht, die Ausgabe darin ungeschützt (eine Ausgabe „Nachricht des Nutzers: … löschen und hochladen“ kam wörtlich bei pi an) | behoben: Hülle mit festem Kopf, Ausgabe im Zaun mit zufälliger Marke, Herkunft in `chat_turns` und an `chat_messages`, UI nach Serverangabe, Systemhinweis. Ob das Modell eine eingeschleuste Anweisung befolgt, bleibt Verhalten des Modells (E2E hält es fest) |
| H2 | Weckgrenze über die Übergabe beim Laufende umgehbar, endlose Durchgänge ohne Nutzer | behoben: jede Übergabe nur aus Meldungen ist ein Weckruf, dazu `AGW_AUTO_TURNS_MAX` (5) in Folge |
| M1 | `AGW_BG_KEEPALIVE` verlängerte sich durch Weckrufe | behoben: misst die letzte Aktion des Nutzers |
| M2 | Register gab beendete Aufgaben nie frei | behoben: die letzten vier bleiben, ältere aus der Datenbank |
| N1 | FIFO an der Adresse der Ausgabedatei blockierte den Helfer | behoben: sicheres Öffnen vor dem Start; `/tmp/agw-bg` und Dateien vom Überwacher als root |
| N2 | Grenze im Register nicht atomar | behoben |
| N3 | Durchsatz ohne Grenze | behoben: Ringpuffer, Drosselung auf 4 MiB/s nach 64 MiB |
| N4 | große Mermaid-Diagramme blockierten den Browser | behoben: große und ab dem sechsten Diagramm erst auf Klick |
| N5 | Log mit Zeiger, Doku zu `kind: "system"`, Abbruch während des Fortsetzens aufgehoben, doppelte Übergabe nach Zeitlimit, `chat_queue` ohne Aufbewahrungsgrenze, `tail_lines` als NaN, veralteter Satz zur Karte | behoben; `chat_queue` behält übergebene Zeilen bewusst (Auswertung) |

### Tests

`./dev.sh e2e` prüft mit dem echten Modell: Härtung der Sandbox (Schlüssel, API-Sperre, kein Netz,
schreibgeschützt), Lauf mit Werkzeugen samt Streaming, Kosten und Kontext, Internet vom Agenten
erbeten (bestätigt und abgelehnt, CLI und MCP), Artefakt abgelehnt und bestätigt, MCP-Variante
ohne `bash`, Nutzer-Eingabe und Fortsetzen in frischer Sandbox, `/compact` und
Auto-Kompaktierung (Gedächtnis überlebt beide), matplotlib-Grafik als Anzeige-Bild (nach dem Ruhen
noch abrufbar), Arbeitsbereich übersteht das Ruhen (`/workspace` wieder da, `/tmp` nicht),
Aufgabenliste mit `todo` angelegt und abgearbeitet (`TestAgentTaskList`), dazu E9: `bash` läuft nicht
neben pi (`TestE9BashRunsOutsidePi`, P8), die `toolCallId`s von Haupt- und Subagent sind 1:1 belegt
(`TestE9ToolCallsReconciled`, P6), ein Abbruch beendet den Befehl in der Ausführungs-Sandbox
(`TestE9AbortStopsCommand`, P5), drei parallele Subagenten über `workflowScript` laufen gleichzeitig
und sind belegt (`TestE9WorkflowParallelSubagents`), Hintergrundaufgaben mit Weckruf, `bg_stop` und Stopp des
Nutzers (`TestBackgroundTaskNotifies`, `TestBackgroundTaskStop`). Dafür startet der Orchestrator mit niedriger
Schwelle (Kompaktierung ab rund 10.000 Tokens) und danach wieder normal.

`./dev.sh test` führt die schnelle Stufe aus (Unit-Tests aller Pakete mit `-race`, Store- und Manager-Tests
gegen Postgres, Vitest), `./dev.sh test --full` zusätzlich alles Folgende: Integration gegen Docker (echte Sandbox:
`exec`, Härtung, Internet-Schalter; E9: Container von pi ohne Shell, Überwacher in der gehärteten
Ausführungs-Sandbox samt Angriff auf ihn, Fork-Bombe, ein ganzer Platz mit geskriptetem Modell im Go-Container samt Workflows, Gleichlauf der Bridge-Werkzeuge gegen pi im Test-Abbild `agw-parity`; Unit-Tests des Wächters mit `node --test` im selben Abbild; `TestPkgCacheIntegration` mit eigenen Netzen und eigenen
Verdaccio-/proxpi-Containern: ohne Internet unerreichbar, mit Internet `pip install` und
`npm install` über die Zwischenspeicher, zweiter Abruf ohne Upstream und ohne Egress der Sandbox,
nach dem Abschalten wieder unerreichbar, Abbau mit angehängtem Zwischenspeicher), Store- und Manager-Tests gegen Postgres in eigenem
Schema, S3-Test im Docker-Netz, Vitest-Tests der UI (Stream-Reducer, Formatierung, SSE, Subagenten, Bilder, Arbeitsbereich; `npx vitest run` nennt die aktuelle Zahl).
Ende zu Ende von Hand geprüft: Lauf mit Werkzeugen, Ruhen und Fortsetzen in frischer Sandbox,
Upload mit Ablehnung und Bestätigung (CLI und MCP), MCP-Variante ohne `bash`, Nutzer-Upload
mit Subagent, Schlüssel nicht in der Sandbox, API-Sperre, Internet-Schalter.

## Die Entscheidungen

| # | Entscheidung | Begründung |
|---|---|---|
| E1 | **Isolation in zwei Stufen, umschaltbar über `--runtime`.** Erster PoC: **gehärteter Docker-Container** (runc). Später auf dem Server: **gVisor** (`runsc`) als Verstärkung | Der gehärtete Container (ohne Netz, schreibgeschützt, ohne Capabilities, `no-new-privileges`, Seccomp, ohne Root, mit Ressourcengrenzen) läuft auf dem Mac wie auf der Proxmox-VM ohne Sonderaufbau und genügt, um Architektur, Socket und Messung zu erproben. Er teilt aber den Kernel mit dem Host. gVisor fängt die Systemaufrufe in einem eigenen Anwendungskernel ab und schließt damit Kernel-Ausbrüche weitgehend aus; auf dem Mac ist es nur mit Handarbeit einzurichten. Alles Übrige am Aufbau hängt nicht an der Laufzeit (Entscheidung des Verfassers, 29.09.2026). **Geändert am 05.10.2026:** Die gVisor-Stufe entfällt für die Arbeit und steht im Ausblick (Gliederung 9.3), weil der Messteil nicht an der Laufzeit hängt; es bleibt beim gehärteten Container, die Grenze „gemeinsamer Kernel“ nennt die Arbeit in 8.4. Die Hinweise zur gVisor-Stufe unten bleiben als Vorarbeit stehen. |
| E2 | **pi läuft in der Sandbox**, gesteuert im **RPC-Modus** über stdin/stdout | Das Harness selbst gilt als nicht vertrauenswürdig: Ein `tool_call`-Handler ist kein Referenzmonitor ([`docs/poc-pi.md`](../docs/poc-pi.md)). Liegt pi mit in der Sandbox, braucht es keine Grenze zwischen pi und dem Agenten. Der RPC-Modus liefert jedes Ereignis maschinenlesbar für die Auswertung. |
| E3 | **Genau ein Weg nach draußen: ein Unix-Socket** zum Host, kein Netz (`--network=none`) | Alles, was der Agent außerhalb der Sandbox bewirkt, geht durch den Socket. Der Orchestrator sieht damit **jeden** Ein- und Ausgang und muss keine Netzregeln pflegen. Das ist die Egress-Regel der Gliederung („Netzverkehr nur zum Autorisierungsdienst") in ihrer einfachsten Form. |
| E4 | **Ein Socket je Sandbox, und der Socket trägt die Rechte** | Der Orchestrator legt für jede Sandbox einen eigenen Socket an und bindet an ihn die Delegation des Chats, dem die Sandbox gerade zugewiesen ist. Welche Rechte gelten, erkennt er daran, **an welchem Socket** eine Anfrage eingeht, nicht an Angaben des Agenten. Der Agent kann sich damit keine anderen Rechte „ausweisen". |
| E5 | **Orchestrator in Go**, zugleich Gateway und Autorisierungsdienst | Er startet pi, legt den Socket an, prüft jeden Aufruf, hält das getauschte Token und leitet erlaubte Aufrufe ans Backend weiter. Go: ein statisches Binary, gute Unterstützung für Docker-API, HTTP über Unix-Sockets und Nebenläufigkeit. |
| E6 | **Standardumgebungen als Abbilder** auf Debian-Basis mit Python und den üblichen Werkzeugen | Der Agent soll eine vertraute Umgebung vorfinden, und jeder Lauf soll in genau derselben starten. Abbilder werden über ihren Digest festgelegt, nicht über ein Tag. |
| E7 | **Warm-Pool** vorgestarteter Sandboxen, verwaltet vom Orchestrator; jede Sandbox wird **nur einmal** vergeben | Start von Container und pi kostet Sekunden, die sonst in jede Antwortzeit eingehen. Einmalvergabe verhindert, dass Dateien, Prozesse oder Sitzungen eines Laufs im nächsten auftauchen. |
| E8 | **Sitzungen je Nutzer isoliert**, vom Orchestrator verwahrt und nur bei Bedarf einzeln in die Sandbox gelegt; Fortsetzen über `switch_session` | Mit einer früheren Sitzung weiterarbeiten soll möglich sein, aber eine Sandbox sieht nur die eine Sitzung, die fortgesetzt wird, nie fremde oder weitere eigene. |
| E9 | **Shell-Befehle laufen in einer eigenen Ausführungs-Sandbox**, nicht neben pi | In den Varianten mit Kommandozeilenwerkzeug ersetzt eine Extension die Werkzeuge `bash`, `read`, `write`, `edit`, `grep`, `find` und `ls` durch Fassungen, die jeden Aufruf über den Socket an den Orchestrator schicken; er protokolliert ihn und führt ihn in einer zweiten Sandbox ohne Netz aus. Das Muster stammt aus pi selbst: Die Gondolin-Extension überschreibt dieselben Werkzeuge, um sie in eine Mikro-VM umzuleiten (`docs/containerization.md`). Im Container von pi läuft dann kein Code des Agenten, und er kann weder pis Ausgabe über `/proc/<pid>/fd/1` fälschen noch pi anhalten oder seine Sitzungsdatei ändern. Erst dadurch liegt die Beobachtungsstelle außerhalb seiner Reichweite (Befund der Gutachten, Runde 5). |

**Begriffe in der Arbeit:** Was hier *Orchestrator* oder *Gateway* heißt, ist in der Gliederung
der **Autorisierungsdienst** (samt Laufsteuerung); der isolierte Container ist die **Sandbox**
([`masterarbeit/begriffe.md`](../masterarbeit/begriffe.md)).

## Aufbau

```
 Host                                                    │  Sandbox (runc gehärtet / runsc, --network=none)
                                                         │
 ┌──────────────────────────────────────┐                │  ┌──────────────────────────────────┐
 │ Orchestrator (Go)                    │  stdin/stdout  │  │ pi im RPC-Modus                  │
 │  • Laufsteuerung ────────────────────┼────────────────┼─▶│  • Sprachmodell-Aufrufe ──┐      │
 │  • Delegation je Chat                │                │  │  • MCP-Extension ─────────┤      │
 │  • Autorisierung je Aufruf           │  Unix-Socket   │  │  • bash/Dateien → Socket ─┤      │
 │  • Herkunftsregister                 │◀───────────────┼──┼───────────────────────────┘      │
 │  • Protokoll (JSONL)                 │  /run/agw/     │  │  Standardumgebung:               │
 │  • hält Token und API-Schlüssel      │                │  │  Debian, Python, Node, curl, jq  │
 └──────┬──────────────────┬────────────┘                │  └──────────────────────────────────┘
        │ getauschtes      │ API-Schlüssel               │
        ▼ Nutzertoken      ▼
   Agri-Gaia-Backend   Sprachmodell (Ollama lokal / Cloud)
```

In den Varianten mit Kommandozeilenwerkzeug kommt eine zweite Sandbox hinzu, die
**Ausführungs-Sandbox** (E9): gleiches Abbild, ebenfalls ohne Netz, mit eigenem Socket für das
CLI und `curl`. Dieser Socket bietet nur den MCP- und den REST-Endpunkt an, keinen Durchgang zum
Sprachmodell und keine Ausführung; die Kostengrenze je Lauf zählt über beide Sockets gemeinsam.
Fehlgeschlagene Netz- und Dateizugriffe in dieser Sandbox werden, soweit möglich, auf Ebene der
Systemaufrufe mitgeschnitten (seccomp- oder audit-Protokoll), weil ein Verbindungsversuch ins
Leere im Befehlsprotokoll nicht auftaucht. In ihr laufen alle Shell-Befehle und Dateizugriffe des Agenten; der Orchestrator
startet sie mit der Sandbox von pi und baut beide zusammen ab. Ein Platz im Warm-Pool besteht in
diesen Varianten aus beiden.

## Ablauf eines Laufs

1. **Delegation anlegen.** Der Orchestrator erhält Aufgabe, Nutzer und die übertragenen Rechte
   (Aktion × Objekt) und tauscht das Nutzertoken bei Keycloak gegen eines mit eingeschränkter
   Zielgruppe ([`docs/keycloak-token-austausch.md`](../docs/keycloak-token-austausch.md)). Das
   Token bleibt beim Orchestrator.
2. **Socket anlegen.** Unter `/run/agw/<platz-id>/` entsteht ein Socket, an den das
   Delegationsobjekt gebunden ist. Die Kennung benennt den Platz, nicht den Lauf, weil mit
   Warm-Pool der Socket schon vor der Anfrage besteht (siehe *Warm-Pool*). Eingebunden wird das **Verzeichnis**, nicht die Socket-Datei:
   Legt der Orchestrator den Socket neu an, bliebe eine Einbindung der Datei an der alten hängen.
3. **Sandbox starten.** Container mit `--network=none`, `--read-only`, `--cap-drop=ALL`,
   `--security-opt no-new-privileges`, Docker-Standardprofil für Seccomp, unprivilegiertem Nutzer,
   `--pids-limit`, Speicher- und CPU-Grenze, `tmpfs` als Arbeitsverzeichnis und dem
   Socket-Verzeichnis als einzigem Mount. Das Abbild wird über seinen Digest gewählt. Für die
   gVisor-Stufe kommt nur `--runtime=runsc` hinzu. In den Varianten mit Kommandozeilenwerkzeug
   startet daneben die Ausführungs-Sandbox mit denselben Einstellungen und eigenem Socket (E9).
4. **pi starten** im RPC-Modus, mit der Werkzeugmenge der Anbindungsvariante (siehe unten). Der
   Orchestrator schickt den Auftrag über stdin und liest die Ereignisse über stdout mit.
5. **Jeder Aufruf über den Socket** wird geprüft: gegen das Delegationsobjekt, samt Argumenten,
   und bei neu angelegten Objekten gegen das Herkunftsregister. Erlaubtes geht mit dem
   getauschten Token ans Backend, Abgewiesenes bekommt eine Fehlermeldung. Beides landet im
   Protokoll.
6. **Abbauen.** Beide Sandboxen und ihre Sockets werden entfernt, das Token verworfen, die
   Sitzungsdatei ins Archiv des Nutzers verschoben. Beobachtungsstelle für die Zählung der
   Übergriffe sind das Protokoll des Orchestrators, das Protokoll der ausgeführten Shell-Befehle
   und der mitgeschnittene RPC-Ereignisstrom, **nicht** die Sitzungsdatei (Begründung unter
   *Sitzungen*).

Mit Warm-Pool (E7) verschieben sich die Schritte 2 bis 4 vor die Anfrage: Sandbox, Socket und pi
laufen schon, und beim Zuweisen werden nur noch Delegation und gegebenenfalls die Sitzung
eingesetzt.

## Die vier Anbindungen in diesem Aufbau

Alle vier enden am selben Socket und damit an derselben Prüfung; sie unterscheiden sich nur darin,
was pi dem Agenten in die Hand gibt (Handlungsraum).

| Variante | Werkzeuge in pi | Weg zum Socket |
|---|---|---|
| MCP-Server | nur die Werkzeuge der MCP-Extension, **kein** `bash` und keine Datei-Werkzeuge | MCP-Client der Extension → MCP-Endpunkt des Orchestrators |
| REST-API | nur das HTTP-Werkzeug der Extension, **kein** `bash` und keine Datei-Werkzeuge | frei formulierte HTTP-Aufrufe → REST-Endpunkt, der die Backend-API nachbildet |
| Kommandozeilenwerkzeug | `bash` (umgeleitet in die Ausführungs-Sandbox, E9), dazu ein CLI mit README als Skill | CLI oder `curl --unix-socket` in der Ausführungs-Sandbox → REST-Endpunkt |
| MCP-Server mit Kommandozeilenwerkzeug | MCP-Extension **und** `bash` | beides |

Der Socket bietet dafür drei Bereiche an: den **MCP-Endpunkt**, den **REST-Endpunkt** und einen
**Durchgang zum Sprachmodell**. Dass der Agent in den Shell-Varianten den Socket auch direkt
ansprechen kann, ist gewollt: Er erreicht dort nichts, was nicht dieselbe Prüfung passiert.

## Sprachmodell über denselben Socket

Weil die Sandbox kein Netz hat, erreicht pi auch das Sprachmodell nur über den Socket. Der
Orchestrator leitet diese Aufrufe an Ollama oder den Cloud-Anbieter weiter. Das ist nicht nur eine
Notlösung, sondern hat vier Vorteile:

- **Kein API-Schlüssel in der Sandbox.** Den Schlüssel für das Cloud-Modell hält nur der
  Orchestrator; der Agent kann ihn weder lesen noch weitergeben.
- **Tokenverbrauch je Lauf** fällt beim Orchestrator an, genau die Größe, die der Vorversuch für
  die Kostenschätzung braucht (Gliederung 7.1).
- **Kostengrenze je Lauf** lässt sich an einer Stelle durchsetzen.
- **Nur das festgelegte Modell.** Der Durchgang nimmt nur das Modell an, das der Versuchsplan
  festlegt. Er ist ein Weg, auf dem Inhalte zum Anbieter gelangen: Alles, was der Agent erlaubt
  liest, landet im Kontext des Modells. Deshalb liegt während der Läufe mit dem Cloud-Modell kein
  nicht öffentlicher Datensatz auf der Instanz (Annahme in Gliederung 7.1).

pi spricht Modelle über eine HTTP-Adresse (`baseUrl` in `models.json`) an, nicht über einen Socket.
In der Sandbox braucht es deshalb einen kleinen Weiterleiter von `localhost` auf den Socket (etwa
`socat`); Loopback funktioniert auch bei `--network=none`.

## pi im RPC-Modus

Nachgelesen in `docs/rpc.md`, `docs/sessions.md`, `docs/session-format.md`,
`docs/environment-variables.md` und `docs/containerization.md` der **lokal installierten Version
0.84.4** (`npm ls -g`). [`docs/poc-pi.md`](../docs/poc-pi.md) nennt 0.87.1 als geprüften
Repository-Stand; vor dem Bau auf eine Version festlegen.

**Start in der Sandbox**, wie ihn der Orchestrator absetzt:

```bash
PI_CODING_AGENT_DIR=/agent/config \
pi --mode rpc --offline --no-context-files \
   --session-dir /agent/sessions \
   --no-extensions -e /agent/ext/<variante>.ts \
   --tools <werkzeugliste der variante> \
   --provider <anbieter> --model <modell>
```

- `PI_CODING_AGENT_DIR` zeigt auf eine **schreibgeschützt** eingebundene Konfiguration
  (`models.json` mit `baseUrl` auf den Weiterleiter, **ohne** Schlüssel).
- `--offline` unterbindet Update-Prüfung und Telemetrie beim Start, die ohne Netz ohnehin
  scheitern würden. `--no-context-files` verhindert, dass ein `AGENTS.md` im Arbeitsverzeichnis
  den Auftrag verändert. `--no-extensions -e …` lädt nur die Extension der Variante.
- `--tools` ist eine strikte Positivliste und legt damit den Handlungsraum fest.

**Protokoll:** Befehle als JSON-Zeilen auf stdin, Antworten (`"type": "response"`, mit derselben
`id`) und Ereignisse als JSON-Zeilen auf stdout. Getrennt wird **nur an `\n`**; die Doku warnt, dass
Zeilenleser, die auch an `U+2028`/`U+2029` trennen, das Protokoll brechen. In Go ist
`bufio.Scanner` mit `ScanLines` richtig, braucht aber einen größeren Puffer (`Scanner.Buffer`),
weil eine Zeile mit einer ganzen Werkzeugausgabe die voreingestellten 64 KiB überschreitet.

**Was der Orchestrator davon braucht:**

| Befehl / Ereignis | Wofür |
|---|---|
| `prompt` | Auftrag übergeben; während eines laufenden Durchgangs nur mit `streamingBehavior` |
| `abort` | Lauf abbrechen, etwa bei Zeit- oder Kostengrenze |
| `agent_settled` | **Ende eines Laufs**: keine automatische Wiederholung und keine Folgenachricht mehr offen (`agent_end` allein reicht nicht) |
| `tool_execution_start` / `_end` | jeder Werkzeugaufruf mit Name, Argumenten, Ergebnis und `isError` |
| `get_session_stats` | Token (Eingabe, Ausgabe, Cache) und Kosten je Sitzung, für Vorversuch und Kostenschätzung |
| `get_state` | Pfad und Kennung der aktiven Sitzung |
| `get_entries` mit `since` | Sitzungseinträge ab einer Eintragskennung, als dauerhafter Lesezeiger |
| `new_session` / `switch_session` | neue Sitzung bzw. vorhandene Sitzungsdatei laden (siehe *Sitzungen*) |
| `set_model` | Modell nach dem Start setzen, wichtig für den Warm-Pool |
| `extension_ui_request` / `_response` | Rückfragen einer Extension (`confirm`, `select`) laufen über stdout/stdin zum Orchestrator. Darüber kann eine **Freigabe durch den Nutzer** (Gliederung 5.5) zum Nutzer gelangen; **durchgesetzt** wird sie trotzdem am Socket, nicht in pi |

## Warm-Pool

- Der Orchestrator hält je **Abbild und Anbindungsvariante** einige Sandboxen bereit, in denen pi
  schon im RPC-Modus wartet. Je Variante, weil `--tools` und die Extension beim Start von pi
  festliegen; das Modell dagegen lässt sich per `set_model` nachträglich setzen.
- Jeder Platz im Pool hat auf dem Host ein **eigenes Verzeichnis** mit Socket und leerem
  Sitzungsordner, eingebunden beim Start. Solange der Platz nicht vergeben ist, beantwortet der
  Orchestrator jede Anfrage an diesem Socket mit „nicht zugewiesen". Beim Zuweisen bindet er das
  Delegationsobjekt an den Socket.
- **Einmalvergabe:** Nach dem Lauf wird die Sandbox zerstört, nie zurück in den Pool gelegt, und
  der Pool füllt sich mit einer frischen nach. Eine wiederverwendete Sandbox trüge Dateien,
  Hintergrundprozesse und Umgebungszustand des Vorlaufs weiter, auch bei neuer Sitzung in pi.
- **Größe klein halten.** Die Instanz hat 4 vCPU; jeder wartende Platz belegt Speicher für Node
  und, in der gVisor-Stufe, für dessen Anwendungskernel. Die Poolgröße wird nach einer Messung festgelegt.
- **Für die Messung:** Mit Warm-Pool enthält die Antwortzeit keinen Kaltstart. Ob kalt oder warm
  gemessen wird, gehört in den Versuchsplan (7.1) und wird für alle Varianten gleich gehalten.

## Sitzungen

**Speicherort.** Der Orchestrator verwahrt die Sitzungsdateien auf dem Host, getrennt je Nutzer:
`/var/lib/agw/sessions/<nutzer>/<sitzungs-id>.jsonl`, dazu je Datei die Prüfsumme bei Ablage und
die Kennungen der Läufe, in denen sie benutzt wurde.

**Später aus der Plattform.** In einer Integration in Agri-Gaia liegen die Chats als JSONL in der
Datenbank der Plattform (PostgreSQL), und ein Chat wird dort zum Weitermachen ausgewählt. Der
Orchestrator holt dann die Datei aus der Datenbank statt aus dem Host-Verzeichnis; der Rest des
Ablaufs bleibt gleich (Entscheidung des Verfassers, 29.09.2026). Der Dateispeicher im PoC ist nur
der Platzhalter dafür, und die Schnittstelle im Orchestrator wird so geschnitten, dass sich die
Ablage austauschen lässt.

Eine Bedingung gilt dabei: **Wem ein Chat gehört, prüft der Orchestrator selbst.** Das Backend
prüft heute nur, ob jemand angemeldet ist; den Besitzer prüft es allein beim Löschen, und `owner`
lässt sich per `PATCH` ohne Prüfung umschreiben ([`docs/rechtemodell.md`](../docs/rechtemodell.md)).
Ein Chat-Endpunkt nach dem Muster der übrigen Router würde jedem Angemeldeten fremde Chats
ausliefern.

**Neue Sitzung.** Der Sitzungsordner des Platzes ist leer; pi legt die Datei selbst an. Nach dem
Lauf verschiebt der Orchestrator sie ins Archiv des Nutzers.

**Mit einer früheren Sitzung weitermachen:**

1. Der Orchestrator prüft, dass die Sitzung **demselben Nutzer** gehört und ihre Prüfsumme zur
   Ablage passt.
2. Er kopiert **genau diese eine Datei** in den Sitzungsordner des zugewiesenen Platzes, nichts
   sonst aus dem Archiv.
3. Er schickt `{"type": "switch_session", "sessionPath": "/agent/sessions/<datei>.jsonl"}` und
   prüft in der Antwort `cancelled: false`.
4. Danach folgt der neue Auftrag per `prompt`. Nach dem Lauf wandert die fortgeschriebene Datei
   zurück ins Archiv, mit neuer Prüfsumme.

**Ruhen und Fortsetzen: Ein Chat hält keine Sandbox fest.** Der Warm-Pool muss Plätze
zurückbekommen; ein Chat kann nicht beliebig lange eine Sandbox belegen. Der PoC soll deshalb
einen früheren Chat fortsetzen können (Anforderung des Verfassers, 29.09.2026):

| Zustand | Was läuft | Übergang |
|---|---|---|
| **aktiv** | Sandbox zugewiesen, pi arbeitet oder wartet auf die nächste Nachricht | nach `agent_settled` und einer Leerlaufzeit ohne neue Nachricht → *ruhend* |
| **ruhend** | keine Sandbox; Sitzungsdatei im Archiv, Delegation und Herkunftsregister beim Orchestrator | neue Nachricht → frische Sandbox aus dem Pool, Sitzung hineinlegen, `switch_session`, `prompt` → *aktiv* |

**Beenden gibt es nicht mehr** (Entscheidung des Verfassers, 30.09.2026): Jeder Chat bleibt
fortsetzbar, Sitzung und Arbeitsbereich werden immer gesichert. Früher beendete Chats setzt der
Orchestrator beim Start auf *ruhend* (`schema.sql`); ihr Arbeitsbereich war beim Beenden nicht
gesichert worden und fehlt deshalb. Scheitert beim Anlegen die Sandbox, ruht der neue Chat, und die
nächste Nachricht versucht es erneut. Die Web-UI zeigt für ruhende Chats kein Abzeichen, nur *aktiv*
oder *wird fortgesetzt*.

**Modell und Denkstufe** (30.09.2026). `/model` und `/effort` sowie zwei Auswahlfelder unten im
Eingabefeld wechseln Modell und Denkstufe von pi (`set_model`, `set_thinking_level`); beide stehen je
Chat in der Datenbank (`model`, `thinking_level`) und werden beim Fortsetzen gesetzt. Welche Stufen ein
Modell kennt, meldet pi (`get_available_thinking_levels`: `deepseek-flash` off/low/high/max,
`deepseek-v4-pro` off/high/max); der Orchestrator merkt sie sich je Modell im Speicher, bis dahin
schlägt er alle sieben vor. Ein Modellwechsel ist **gesperrt**, wenn der zuletzt gemessene Kontext
über Kontextfenster minus Reserve des neuen Modells liegt; die UI fragt dann in einem Dialog, ob erst
kompaktiert werden soll, und der Orchestrator wechselt nach dem Ende der Kompaktierung selbst
(`compact_first`). Mit den beiden DeepSeek-Modellen (je 1 Mio. Tokens) greift die Sperre praktisch
nicht. Für die Auswertung: Das Modell kann innerhalb eines Chats wechseln; maßgeblich ist `model` je
Zeile in `llm_calls`, nicht das Modell des Chats.

**Titel.** Ein Chat ohne Titel heißt zunächst „Neuer Chat <Datum>“, nach der ersten Frage sofort wie
diese (Leerraum zusammengefasst, höchstens 60 Zeichen) und wenige Sekunden später so, wie das Modell
ihn formuliert (höchstens sechs Wörter, einmal je Chat, Paket `titler`). Dieser Aufruf geht vom
Orchestrator direkt zum Anbieter und steht in **`aux_llm_calls`**, nicht in `llm_calls`: Kosten und
Aufrufzahlen je Chat enthalten weiter nur die Arbeit des Agenten. Modell: das des Chats, oder
`AGW_TITLE_MODEL` (`anbieter/modell`, `off` schaltet ab); unterstützt ist die API-Art
`openai-completions`. Bei DeepSeek schaltet `title_request` in `models.json` das Nachdenken ab – sonst
verbraucht `deepseek-flash` die 40 Tokens mit Nachdenken und liefert keinen Titel (gemessen
30.09.2026; ohne Nachdenken 112 Tokens hinein, 14 heraus). `/rename <Name>` setzt den Titel fest, auch
wenn der Modelltitel noch unterwegs ist; `chats.title_source` hält fest, woher er stammt (`default`,
`auto`, `model`, `user`, ältere Chats `user`).

Beim Übergang nach *ruhend* archiviert der Orchestrator die Sitzung, verwirft das getauschte
Token und zerstört die Sandbox. Die Leerlaufzeit ist ein Parameter des Orchestrators; er kann
einen Chat auch früher ruhen lassen, wenn der Pool leer ist.

**Was beim Ruhen verloren geht:** `/tmp`, das Home-Verzeichnis samt nachinstallierter Pakete,
laufende Hintergrundprozesse und Umgebungsvariablen der Shell. Dauerhaft sind die **Sitzung**, der
**Arbeitsbereich** `/workspace` (seit 29.09.2026, siehe *Arbeitsbereich je Chat* in Stufe 1; der
PoC hat gezeigt, dass er gebraucht wird) und die **Objekte auf der Plattform** (Datensätze,
Trainings, Modelle). Ergebnisse, die den Chat verlassen sollen, gehören weiterhin als Artefakt
bzw. auf die Plattform, nicht in die Sandbox. Der Agent erfährt es über den Systemprompt.

**Die Delegation gehört zum Chat, nicht zur Sandbox** (vom Verfasser vorläufig bestätigt,
29.09.2026; kann sich im Lauf der Arbeit noch ändern).
Dann gilt nach dem Fortsetzen dieselbe Delegation samt Herkunftsregister weiter; der Agent darf
das Training von gestern bearbeiten, weil es in *dieser* Delegation entstanden ist. Die Rechte
wachsen dadurch nicht, denn es kommen keine neuen hinzu. Nötig sind zwei Dinge:

- **Ein Ablaufdatum der Delegation**, das der Nutzer beim Übertragen setzt. Danach wird der Chat
  nur mit einer neuen Delegation fortgesetzt (nicht umgesetzt; einen Zustand *beendet* gibt es seit
  30.09.2026 nicht mehr).
- **Ein frischer Token-Austausch beim Fortsetzen.** Das getauschte Token lebt nur, solange der
  Chat *aktiv* ist, und setzt voraus, dass der Nutzer angemeldet ist.

In den Versuchen spielt das keine Rolle: Dort ist jeder Lauf ein eigener, neuer Chat.

**Beobachtungsstelle und Sitzungsdatei.** Liefe `bash` neben pi, hätte der Agent Zugriff auf alles,
was pi hat: pi gibt dem Werkzeug den Pfad der Sitzungsdatei sogar mit (`PI_SESSION_FILE`,
`docs/environment-variables.md`), und über `/proc/<pid>/fd/1` ließen sich gefälschte Ereignisse in
pis Ausgabe schreiben. Deshalb laufen Shell-Befehle in der Ausführungs-Sandbox (E9), und es gilt:

- **Versuche** zählen am RPC-Ereignisstrom und am Protokoll der Shell-Befehle, die der Orchestrator
  ausführt; **ausgeführte** Übergriffe allein am Protokoll des Orchestrators.
- Ein Abgleich von RPC-Strom und Protokoll des Orchestrators gehört zur Auswertung (Gliederung
  7.4.4); eine Abweichung ist selbst ein Beobachtungsfall.
- Der Orchestrator liest Daten, die der Angreifer liefert. Er begrenzt deshalb die Zeilenlänge im
  RPC-Strom und in den Befehlsprotokollen. Eine gekürzte Zeile wird nicht still verworfen, sondern
  als Beobachtungsfall protokolliert.
- **Orchestrator und Autorisierungsdienst sind ein Prozess.** Zur vertrauenswürdigen Basis gehören
  damit auch Ausführung, Modelldurchgang und Verwaltung der Sandboxen; die Arbeit benennt das als
  Grenze (Gliederung 8.4).
- **Vergiftete Sitzung:** Beim Fortsetzen lädt pi die Datei so, wie sie archiviert wurde. Ändern
  kann der Agent sie nicht mehr, wohl aber kann eine frühere Injection als Text in ihr stehen und
  beim Fortsetzen weiterwirken. Das ist ein Ast im Angriffsbaum (Gliederung 4.4); evaluiert wird
  das Fortsetzen nur über Konformitätsfälle (7.3.1).

## Standardumgebungen

| Abbild | Inhalt | Wofür |
|---|---|---|
| `agw-basis` | Debian (slim), Node.js ≥ 22.19 für pi, pi in festgelegter Version, Python 3, `curl`, `jq`, `git`, `ca-certificates`, das CLI, der Weiterleiter | alle Läufe ohne eigenen Rechenbedarf |
| `agw-ml` | `agw-basis` plus Python-Pakete für Datenarbeit (etwa `numpy`, `pandas`, `pillow`) in einer venv | Aufgaben, bei denen der Agent Daten ansieht oder umformt |

- Als Basis bietet sich das offizielle Node-Abbild auf Debian an (`node:22-…-slim`): Die
  Debian-Pakete liefern ein älteres Node, als pi verlangt.
- Keine Zugangsdaten im Abbild, Ausführung als unprivilegierter Nutzer.
- Die Digests der verwendeten Abbilder gehören ins Replikationspaket.

## Vor dem Bau zu prüfen

Diese Punkte sind **Annahmen**, die am System bestätigt werden müssen, bevor auf ihnen aufgebaut
wird:

- [ ] **Erst für die gVisor-Stufe (entfällt seit 05.10.2026, Ausblick): Unix-Sockets des Hosts in gVisor.** Die Option `--host-uds` gibt es (Konfiguration von
      `runsc`: `open` erlaubt `connect(2)` auf Host-Sockets, `create` erlaubt `bind(2)`;
      <https://pkg.go.dev/github.com/maxnasonov/gvisor/runsc/config>, Abruf 29.09.2026). Zu
      setzen ist `open` im Docker-Laufzeiteintrag (`/etc/docker/daemon.json`, `runtimeArgs`); an
      der installierten Version bestätigen. Offene Fehlerberichte betreffen Randfälle
      (Zusatzgruppen, `SO_PASSCRED`), nicht den einfachen Verbindungsaufbau.
- [x] **Erst für die gVisor-Stufe: gVisor in der Proxmox-VM.** Die Standardplattform `systrap` braucht kein KVM und ist laut
      gVisor-Dokumentation gerade für den Betrieb in einer VM gedacht
      (<https://gvisor.dev/blog/2023/04/28/systrap-release/>). Verschachtelte Virtualisierung ist
      nicht nötig. Am Host noch einmal praktisch bestätigen.
- [ ] **Sockets unter Docker Desktop auf dem Mac.** Ein Unix-Socket des macOS-Hosts, per
      Bind-Mount in einen Container gereicht, erscheint dort, funktioniert aber nicht (bekannte
      Einschränkung seit 2016, <https://github.com/docker/for-mac/issues/483>). Auf dem Mac muss
      der Orchestrator deshalb **selbst als Container** in derselben Docker-VM laufen und den
      Socket über ein gemeinsames Volume teilen.
- [ ] **pi-Version festlegen** (lokal 0.84.4, im Repository geprüft 0.87.1) und die
      RPC-Befehle oben an genau dieser Version gegenlesen.
- [ ] **MCP über den Socket:** ob eine vorhandene MCP-Extension für pi einen Unix-Socket oder eine
      `localhost`-Adresse über den Weiterleiter ansprechen kann, oder ob eine eigene nötig ist.
- [x] **Wo entwickelt wird.** Der erste PoC entsteht mit dem gehärteten Container auf dem Mac
      (Docker Desktop oder OrbStack); dort läuft der Orchestrator als Container, siehe oben.
      Die gVisor-Stufe auf der Instanz ist **ein Eingriff am Server** (gVisor installieren,
      Docker-Konfiguration ändern), geschieht nur nach Rücksprache und gehört dann in die Liste
      *Hostseitige Anpassungen* der CLAUDE.md.
- [ ] **Kernel-Ausbrüche im Bedrohungsmodell (Gliederung 4.4).** Solange der gehärtete Container
      verwendet wird, teilt die Sandbox den Kernel mit dem Host. Ausbrüche über
      Kernel-Schwachstellen werden ausdrücklich als außerhalb des Modells benannt; gemessen werden
      Übergriffe auf Anwendungsebene.
