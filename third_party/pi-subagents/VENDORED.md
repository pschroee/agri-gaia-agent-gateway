# pi-subagents: eigene Kopie im PoC

Dieses Verzeichnis ist eine **angepasste Kopie** des npm-Pakets `pi-subagents`. Sie liegt im
Repo, wird committet und beim Bau des Abbilds `agw-basis` an Stelle des npm-Pakets genutzt.
Anpassungen passieren nur hier. Es gibt **keinen Vorschlag an das Original und keinen Kontakt
zum Upstream** (Entscheidung des Verfassers, 29.09.2026).

## Herkunft

| | |
|---|---|
| Paket | `pi-subagents` auf npm (<https://www.npmjs.com/package/pi-subagents>) |
| Quelle | <https://github.com/nicobailon/pi-subagents> |
| Version | **0.73.1**, hier gekennzeichnet als `0.73.1-agw.2` (`agw.1` am 29.09.2026, `agw.2` am 30.09.2026) |
| Integrity (`npm view pi-subagents@0.73.1 dist.integrity`) | `sha512-IklOqw67DtvIWQFKxFJpDFXsOGcq8RGYZEbokykD5Kvfs9aa/du1cW2NEJ5xx4GUgN2LVt3vAq5IdzEMo7YwJw==` |
| shasum (`dist.shasum`) | `587cd7d2694b74d926f32afc724bd310c2e33c8b` |
| Übernommen am | 29.09.2026 |
| Inhalt | der Inhalt des Tarballs aus `npm pack pi-subagents@0.73.1` (Ordner `package/`), also genau das, was `pi install npm:pi-subagents@0.73.1` anlegt, ohne `node_modules` (1 223 Dateien) |

Vor der ersten Änderung war die Kopie per `diff -r` byteweise gleich dem Tarball-Inhalt.

**Was fremd und was eigen ist, zeigt die Git-Historie:** Commit `d21c5c0` enthält das Paket
unverändert (byteweise gleich dem Tarball), Commit `8dfbd4a` nur die eigenen Änderungen. Genau
diese zeigt

```bash
git diff d21c5c0 8dfbd4a -- poc/third_party/pi-subagents
```

Jede künftige Änderung an der Kopie kommt in einen eigenen Commit und in die Liste unten.

## Lizenz

MIT, Copyright bei den Autoren von pi-subagents; der Lizenztext liegt unverändert in
[`LICENSE`](LICENSE). Die lokalen Änderungen stehen unter derselben Lizenz.

## Zweck

`workflowScript` führt ein vom Agenten geschriebenes Skript in einem Worker aus. pi-subagents
startet diesen Worker fest über `node:worker_threads`, also im pi-Prozess. Im PoC soll das Skript
in der Ausführungs-Sandbox laufen (E9, siehe `poc/e9-ausfuehrungs-sandbox.md`, Abschnitt
`workflowScript`). Bisher hat das Dockerfile den Import beim Bau per `sed` umgelegt. Mit dieser
Kopie ist die Umleitung eine versionierte, geprüfte Änderung am Quelltext statt eines
Textersatzes im Abbild.

## Lokale Änderungen

Alles andere ist unverändert gegenüber dem Tarball. Zusätzlich angelegt, nicht Teil des Pakets:
`VENDORED.md` (diese Datei) und `test-agw/`.

### 1. `package.json`: Version `0.73.1-agw.1`

Nur das Feld `version`, von `0.73.1` auf `0.73.1-agw.1`. Grund: Die Kopie soll an der Version
als Eigenstand erkennbar sein, damit niemand sie für das veröffentlichte Paket hält. Der
Nachsatz `-agw.N` zählt die Stände der eigenen Änderungen.

### 2. `src/workflows/scripted-workflow.js`: Laufzeit des Workers einstellbar

Der statische Import

```js
import { Worker } from "node:worker_threads";
```

ist ersetzt durch

```js
// agw: Laufzeit des Workflow-Workers einstellbar (siehe VENDORED.md, Änderung 2).
const workerModule = process.env.PI_SUBAGENTS_WORKFLOW_WORKER || undefined;
const { Worker } = workerModule ? await import(workerModule) : await import("node:worker_threads");
/** agw: Modul, aus dem `Worker` für workflowScript stammt; die Bridge prüft daran die Umleitung. */
export const workflowWorkerModule = workerModule ?? "node:worker_threads";
```

- **Variable `PI_SUBAGENTS_WORKFLOW_WORKER`:** Modulbezeichner, aus dem `Worker` geladen wird,
  im PoC `/opt/agw/ext/remote-worker.mjs`. Das Modul muss eine Klasse `Worker` exportieren, die
  sich wie die aus `node:worker_threads` verhält (`new Worker(source, { eval: true, workerData })`,
  Ereignisse, `postMessage`, `terminate`). Nicht gesetzt oder leer: `node:worker_threads` wie im
  Original.
- **Gelesen wird beim Laden des Moduls**, nicht je Aufruf. Die Variable muss also im Prozess
  gesetzt sein, bevor pi die Erweiterung lädt.
- **Kein stilles Zurückfallen:** Zeigt die Variable auf ein fehlendes Modul, schlägt schon das
  Laden von `scripted-workflow.js` fehl (`ERR_MODULE_NOT_FOUND`). workflowScript läuft dann
  gar nicht, statt unbemerkt im pi-Prozess.
- **Export `workflowWorkerModule`:** zeigt zur Laufzeit, welches Modul aktiv ist. Die Bridge
  prüft daran, ob die Umleitung greift.
- Das Paket ist ESM (`"type": "module"`), Top-Level-`await` ist damit zulässig; `index.js` des
  Pakets nutzt ihn selbst schon. `Worker` wird nur an einer Stelle instanziiert, in
  `runWorkflowScript` (`new Worker(WORKER_SOURCE, { eval: true, workerData: { acornPath } })`).
- Die Quellzuordnung `scripted-workflow.js.map` ist nicht nachgezogen; sie liegt ab Zeile 4 um
  vier Zeilen daneben. Das betrifft nur Stapelangaben in Fehlermeldungen.

### 3. `src/workflows/scripted-workflow.d.ts`: Deklaration des neuen Exports

Am Ende ergänzt:

```ts
export declare const workflowWorkerModule: string;
```

Grund: Die Bridge ist TypeScript und importiert den Export; ohne Deklaration kennt die
Typprüfung ihn nicht.

### 4. `src/runs/shared/single-output.js`: Ausgabe in der Antwort statt als Datei (`agw.2`, 30.09.2026)

`formatOutputPathInstruction` wählt bei gesetzter Umgebungsvariable `PI_SUBAGENTS_OUTPUT_INLINE=1`
immer die Anweisung, die pi-subagents sonst nur Agenten ohne schreibende Werkzeuge gibt: „Return
the complete artifact in your final response. The runtime will persist it to exactly this path“.
Die Version in `package.json` steht dafür auf `0.73.1-agw.2`.

Grund: pi-subagents gibt jedem Kind eines Workflows (und jedem Lauf mit `output`) einen Pfad unter
`/agent/sessions/subagent-artifacts/outputs/…` vor, mit der Anweisung, genau dorthin zu schreiben.
Der Pfad liegt im Container von pi; die Werkzeuge des Kindes laufen aber in der Ausführungs-Sandbox
(E9), wo `/agent` nicht existiert. Im Chat vom 30.09.2026 („Recherche Schwanzbeißen“) scheiterten
daran alle drei Kinder mit `mkdir: cannot create directory '/agent': Read-only file system` und
wichen auf eigene Pfade aus. Gespeichert hat pi-subagents die Datei trotzdem, weil
`resolveSingleOutput` bei unveränderter Datei die Endantwort schreibt; nur die Anweisung passte
nicht. Ohne die Variable verhält sich die Kopie wie das Original. Das pi-Abbild setzt sie fest.
Test: `test-agw/output-inline.test.mjs` (`node --test`, ohne Abhängigkeiten): ohne Variable die
ursprüngliche Anweisung, mit Variable die Anweisung zur Rückgabe in der Antwort.

## Weitere Stellen mit `node:worker_threads` oder `node:vm`

Stand 0.73.1, für die Einordnung in E9; **nicht geändert**:

- `src/workflows/scripted-workflow.js`, Konstante `WORKER_SOURCE`: der Quelltext, der **im**
  Worker läuft, nutzt `require("node:worker_threads")` (`parentPort`, `workerData`) und
  `require("node:vm")` für das Skript des Agenten. Er wird über den umgeleiteten `Worker`
  gestartet und läuft damit dort, wo das Worker-Modul ihn ausführt.
- `src/runs/background/async-retention.js` importiert `Worker` aus `node:worker_threads` und
  startet `async-retention-discovery-worker.mjs` (dort `parentPort` aus `node:worker_threads`).
  Das ist Aufräumarbeit für abgeschlossene Hintergrundläufe (Suche nach alten Laufdateien), kein
  vom Agenten geschriebener Code.
- `node:vm` kommt außerhalb von `WORKER_SOURCE` nicht vor.

## Test

`test-agw/workflow-worker.test.mjs`, ohne Netz, mit `node --test`. Geprüft wird: ohne Variable
(und mit leerer) stammt `Worker` aus `node:worker_threads`; mit Variable aus dem angegebenen
Modul, belegt durch eine Attrappe (`test-agw/fixtures/mock-worker.mjs`), die jede Instanziierung
protokolliert; die eingebaute Klasse wird dann nicht aufgerufen; `workflowWorkerModule` nennt
jeweils das richtige Modul; ein fehlendes Modul führt zu `ERR_MODULE_NOT_FOUND`. Gegen den
unveränderten Tarball schlagen alle fünf Fälle fehl.

Die Abhängigkeiten (`acorn`, `jiti`, `undici`, `yaml`) liegen nicht im Repo. Zum Testen eine
Kopie mit Abhängigkeiten außerhalb des Repos anlegen:

```bash
S=<Scratchpad>/pi-subagents
cp -Rp poc/third_party/pi-subagents "$S"
(cd "$S" && npm install --omit=dev --ignore-scripts --no-audit --no-fund)
cd poc/third_party/pi-subagents && PI_SUBAGENTS_DIR="$S" node --test test-agw/*.test.mjs
```

`node --test test-agw/` (Verzeichnis statt Muster) versucht unter Node 24 das Verzeichnis als
Modul zu laden und schlägt fehl; das Muster `*.test.mjs` angeben.

## Auf eine neue Version heben

1. Tarball holen und auspacken, außerhalb des Repos:
   `npm pack pi-subagents@<neu> && tar xzf pi-subagents-<neu>.tgz` (legt `package/` an);
   Integrity notieren: `npm view pi-subagents@<neu> dist.integrity dist.shasum`.
2. Upstream-Änderungen ansehen: den alten Tarball ebenso auspacken und
   `diff -r <alt>/package <neu>/package` lesen, besonders `src/workflows/scripted-workflow.js`
   und alle Stellen mit `node:worker_threads`, `node:vm` und `new Worker(`.
3. Kopie ersetzen: Inhalt von `poc/third_party/pi-subagents/` außer `VENDORED.md` und `test-agw/`
   löschen, `package/` hineinkopieren, dann `diff -r <neu>/package poc/third_party/pi-subagents`
   — es dürfen nur `VENDORED.md` und `test-agw` als zusätzlich gemeldet werden.
4. Änderungen 1 bis 3 von oben neu anwenden (Version `<neu>-agw.1`). Passt der Import nicht mehr
   wörtlich, die Stelle suchen, an der `Worker` für workflowScript entsteht, und dort gleichwertig
   umlegen.
5. Test laufen lassen (siehe oben), außerdem `diff -r` gegen den neuen Tarball: Die Ausgabe muss
   genau die hier gelisteten Änderungen zeigen.
6. Diese Datei nachziehen: Tabelle *Herkunft*, Liste *Weitere Stellen*, ggf. neue Änderungen.

## Hinweis zum eigenen Repo (05.10.2026)

Das Gateway liegt seit dem 05.10.2026 in einem eigenen Repo. Die oben genannten Commits stehen im
Masterarbeits-Repo (`poc/third_party/…`); hier kam die Kopie mit dem ersten Commit **bereits angepasst**
herein. Die eigenen Änderungen lassen sich trotzdem jederzeit nachprüfen: `npm pack` mit der oben
genannten Version holen, entpacken und mit `diff -r package/ <dieses Verzeichnis>` vergleichen.
