# Plan: Delegation, REST-Variante, Chat in der Plattform

Stand 05.10.2026, Reihenfolge vom Verfasser bestätigt. Schritt 1 und 2 baut der Agent ohne
Unterbrechung, Schritt 3 braucht Eingriffe am Server und wird erst nach Rücksprache umgesetzt.

| Schritt | Was | Wofür in der Arbeit | Stand |
|---|---|---|---|
| 1 | **Delegation**: übertragene Rechte je Chat, Herkunftsregel, Ablauf, Konformitätsprüfung ohne Sprachmodell | FF1, 5.2, 5.3, 7.3.1 | **umgesetzt** (05.10.2026) |
| 2 | **REST-Variante**: REST-Endpunkt des Autorisierungsdienstes am Socket, Variante `api` in pi | FF2 (dritte Anbindung), 6.2.2, Issue #1 | **umgesetzt** (05.10.2026) |
| 3 | **Chat in der Plattform**: schmales Panel rechts im Agri-Gaia-Frontend, Token des angemeldeten Nutzers | Titel, Kapitel 6, Vorführung | geplant, braucht Rücksprache |

Warum diese Reihenfolge: Ohne Delegation gibt es nichts, wogegen FF1 messen könnte; ohne REST-Variante
keinen Dreiwege-Vergleich. Die Einbettung in die Plattform trägt keine Forschungsfrage und passt in die
Wochen, in denen die Hauptläufe rechnen ([`masterarbeit/zeitplan.md`](../masterarbeit/zeitplan.md)).

---

## Schritt 1: Delegation

### Was eine Delegation ist

Eine Delegation gehört zu einem Chat und sagt, **welche Aktionen auf welchen Objekten** der Agent für
diese Aufgabe ausführen darf. Alles andere ist ein Übergriff. Grundlage: `masterarbeit/gliederung.md`,
*Wie die Prüfung gebaut ist*, und 5.2.

```json
{
  "rules": [
    {"action": "read",   "resource": "dataset", "ids": ["*"]},
    {"action": "create", "resource": "dataset"},
    {"action": "update", "resource": "dataset", "ids": ["own"]},
    {"action": "run",    "resource": "training", "ids": ["7"]}
  ],
  "expires_at": "2026-10-06T18:00:00Z",
  "enforce": true,
  "confirm": "writes"
}
```

- **Aktionen:** `read` (Liste und Einzelobjekt), `create`, `update`, `delete`, `run` (Training starten).
- **Ressourcen:** `dataset`, `model`, `training`, `task`, `train_template` (Vorlagen und Konfigurationen),
  `edge_device`, `container_image`, `api` (alle übrigen Pfade der REST-API).
- **Objekte:** `ids` ist eine Liste von Kennungen, `"*"` für alle, `"own"` für Objekte, die **in dieser
  Delegation** entstanden sind (Herkunftsregel). Ohne `ids` gilt die Regel nur für Aktionen ohne Objekt
  (`create`, Listen).
- **Ablauf:** Nach `expires_at` gilt nichts mehr (Anforderung: nach Ablauf keine weiteren Rechte).
- **`enforce`:** `true` weist Übergriffe ab; `false` protokolliert sie nur und lässt durch. Das ist die
  Stufe „keine Schutzmaßnahme" des Versuchsplans: dieselbe Beobachtungsstelle, aber keine Grenze.
- **`confirm`:** `writes` (Standard) fragt bei schreibenden Aufrufen zusätzlich den Nutzer; `none`
  nicht. Die Bestätigung bleibt eine ergänzende Maßnahme, keine Autorisierungsgrenze (5.5).
- **Ohne Delegation** verhält sich der Chat wie bisher (lesen frei, schreiben mit Bestätigung), damit
  die vorhandenen Abläufe und Tests nicht brechen; das Protokoll vermerkt „ohne Delegation".

### Einordnung jedes Aufrufs

Der Autorisierungsdienst ordnet **jeden** Plattform-Aufruf selbst einer Aktion, einer Ressource und
gegebenenfalls einer Kennung zu, aus Methode und normalisiertem Pfad, nie aus Angaben des Agenten.
Eine Routentabelle deckt die Pfade der kuratierten Werkzeuge und die häufigen Pfade der API ab; was sie
nicht kennt, ist `api` mit der Aktion aus der Methode (GET `read`, sonst `update`) und ist nur mit einer
ausdrücklichen `api`-Regel erlaubt. Bekannte schreibende GETs (`/train/containers/{id}/model`,
`/licenses` mit Abfrage) gelten als `create` beziehungsweise `update`.

### Herkunftsregel

- Antwortet die Plattform auf ein `create` mit 2xx, hält der Dienst die neue Kennung fest: `id` im
  Körper bei Datensätzen und Modellen, die Aufgabe aus `Location: /tasks/<id>` beim Training.
- Das Register liegt in Postgres (`delegation_objects`) und gilt beim Fortsetzen des Chats weiter.
- Herkunft entsteht **nur** aus Antworten auf Anlage-Aufrufe, nie aus `owner` oder Angaben des Agenten.
- **Grenze:** Trainingscontainer entstehen asynchron aus einer Aufgabe; die Antwort nennt ihre
  Kennung nicht. `own` gilt deshalb für Container nicht; sie sind nur über ausdrückliche Kennungen oder
  `*` delegierbar. Das steht als Grenze in der Arbeit.

### Ablauf einer Prüfung (Manager.PlatformCall)

1. Aufruf normalisieren (`platform.Normalize`), einordnen (`delegation.Classify`).
2. Gegen die Delegation prüfen: abgelaufen → abweisen; keine passende Regel → Übergriff.
3. Übergriff: mit `enforce` abweisen, ohne `enforce` durchlassen; in beiden Fällen protokollieren
   (`socket_calls`, Ergebnis `uebergriff: …`, und Ereignis für die UI).
4. Erlaubt und schreibend und `confirm: writes` → Bestätigung durch den Nutzer wie bisher.
5. Ausführen; bei erfolgreichem `create` die Herkunft festhalten.

### Konformitätsprüfung ohne Sprachmodell (7.3.1)

Eigene Testfolge `internal/conformance`: jeder verbotene Aufruf mit feindlichen Argumenten direkt an
den Autorisierungsdienst, auch in Varianten, die ein Parser anders verstehen könnte (Groß- und
Kleinschreibung, angehängter Schrägstrich, Kennung als `01` oder ` 1`, doppelte JSON-Schlüssel,
Rohzugriff auf denselben Pfad, schreibendes GET); dazu eine abgelaufene Delegation und ein
fortgesetzter Chat mit einem fremden Objekt. Soll: 100 % abgewiesen, und **kein** verbotener Aufruf
erreicht die (nachgebildete) Plattform. Die Folge gibt eine Tabelle aus, die in 7.3.1 eingeht.

### Oberfläche und Werkzeuge

- API: `POST /api/chats` mit Feld `delegation`; `GET /api/chats/{id}` liefert sie mit.
- CLI: `agw chat new --delegation datei.json`, `agw run --delegation datei.json`.
- Web-UI: Delegation des Chats als Karte (Regeln, Ablauf, Modus), Übergriffe im Socket-Protokoll
  hervorgehoben. Ein Editor für Delegationen ist nicht Teil dieses Schritts.
- Der Agent erfährt seine Rechte über das Werkzeug `rights` (MCP `platform_rights`, CLI
  `agw-platform rights`), damit er nicht blind gegen die Grenze läuft; die Grenze selbst hängt davon nicht ab.
  (Abweichung vom ersten Entwurf: Der Systemhinweis entsteht beim Start eines Platzes im Warm-Pool, also
  bevor feststeht, welchem Chat der Platz gehört; ein Werkzeug ist deshalb der verlässliche Weg.)

---

## Schritt 2: REST-Variante

Umsetzung von Issue #1 mit einer Änderung gegenüber dem dortigen Entwurf: Der REST-Endpunkt liegt
**am Socket des Platzes**, nicht auf einem eigenen TCP-Port. Damit ergibt sich der Chat aus dem Socket
(wie bei MCP und CLI) statt aus der Quelladresse, und es braucht kein weiteres Netz.

- Endpunkt `/platform-api/<pfad der Plattform-API>` an beiden Sockets: Methode, Pfad, Abfrage und
  JSON-Körper gehen unverändert in `platform.Request`; der Dienst prüft, setzt das Token ein und gibt
  Status, `Location` und Körper (geschwärzt) zurück. Ein `Authorization`-Kopf des Agenten wird
  verworfen. Schreibendes wartet auf die Bestätigung wie bei den anderen Wegen.
- `GET /platform-api/_agw/paths?prefix=` liefert das verdichtete Pfadverzeichnis (wie `api_paths`).
- Variante `api` in pi: nur ein HTTP-Werkzeug `platform_http` (Extension `api.ts`, spricht den Socket
  an), dazu `todo`, `web_search`, `web_extract`; **keine** Datei-Werkzeuge, kein `bash`.
- In den Varianten mit `bash` erreicht der Agent denselben Endpunkt mit
  `curl --unix-socket /run/agw/agw.sock http://agw/platform-api/datasets`.
- Protokoll: `via: "api"` für den Weg über `platform_http`, `cli` für `curl` aus der Shell.

---

## Schritt 3: Chat in der Plattform (nach Rücksprache)

- **Panel rechts im Agri-Gaia-Frontend** (`~/dev/agri-gaia/platform/services/frontend`): ausklappbar,
  zeigt den Chat des PoC; erste Fassung als eingebettete Seite des Orchestrators, später als eigene
  React-Komponente nach dem Entwurf in `prototyp/` (Reiter Chat, Werkbank, Aktivität).
- **Anmeldung:** Das Frontend reicht das Keycloak-Token des angemeldeten Nutzers an den Orchestrator;
  der tauscht es je Chat (statt des fest eingestellten Testnutzers). Das ist die Delegation, wie sie
  die Arbeit beschreibt.
- **Orchestrator auf der Instanz** statt auf dem Mac, hinter Traefik (Subdomain `agent.`).
- **Eingriffe am Server, die Rücksprache brauchen:** Orchestrator und Abbilder auf dem Server der Instanz
  betreiben; angepasstes Frontend bauen und ausrollen; Traefik-Route; gegebenenfalls Redirect-URI am
  Keycloak-Client. Eigener Code im Frontend kommt als getrennter Commit in eine eigene Kopie, nicht in
  dieses Repo (Konvention: Upstream-Code wird nicht hierher kopiert).
