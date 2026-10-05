---
name: mermaid
description: Abläufe, Architekturen, Zustände, Sequenzen, Zeitpläne (Gantt) und Datenmodelle (ER) als Mermaid-Diagramm direkt in der Antwort zeigen. Verwenden, wenn der Nutzer ein Diagramm, Flussdiagramm, Ablaufdiagramm, Sequenzdiagramm, Zustandsdiagramm, Klassendiagramm, ER-Diagramm, Gantt-Diagramm, Mindmap oder eine Skizze einer Architektur möchte oder ein Zusammenhang als Bild klarer wird als als Text.
---

# Mermaid-Diagramme zeigen

## Mermaid oder matplotlib?

- **Mermaid** für Struktur: Abläufe, Entscheidungen, Architekturen, Zustände, Nachrichtenfolgen,
  Zeitpläne, Datenmodelle, Gliederungen. Kein Werkzeug, keine Datei nötig.
- **matplotlib** (Skill `diagramme`) für **Daten**: Messwerte, Verläufe, Verteilungen, alles mit Achsen und
  Zahlen. Ein Balkendiagramm aus einer CSV ist kein Fall für Mermaid.

## So zeigst du es

Schreib das Diagramm als Codeblock mit der Sprache `mermaid` in die Antwort:

````markdown
```mermaid
flowchart TD
  A["Anfrage"] --> B["Prüfung"]
```
````

Die Web-UI zeichnet den Block, sobald er geschlossen ist; der Nutzer kann zwischen Diagramm und
Quelltext umschalten und es vergrößern. **Nur die Web-UI zeichnet**: In der CLI, in Artefakten und in
Dateien bleibt es Quelltext. Das ist der Normalfall; eine Datei brauchst du nur, wenn der Nutzer eine
möchte.

## Zur Not als Datei: mmdc

Soll das Diagramm eine Datei werden (Artefakt, Typst-Dokument, Bild mit `![…](…)`), zeichnest du es
mit der Mermaid-CLI `mmdc` (Version 12, wie die Web-UI; läuft ohne Internet, etwa 1 s je Diagramm):

```bash
mmdc -i diagramm.mmd -o diagramm.png          # PNG; auch .svg oder .pdf
mmdc -i diagramm.mmd -o diagramm.png -s 2     # doppelte Auflösung, schärfer
mmdc -i bericht.md -o bericht.out.md          # ersetzt alle mermaid-Blöcke durch SVG-Dateien
```

Für die Anzeige im Chat nimm **PNG** (SVG zeigt die Web-UI nicht an), für Typst **SVG** oder **PDF**.
Dateien unter `/workspace` ablegen. Scheitert der Aufruf mit einem Syntaxfehler, steht die Zeile in
der Meldung; die Fallstricke unten gelten genauso.

Ein Diagramm je Codeblock. Halte es klein (etwa bis 20 Knoten); lieber zwei übersichtliche als ein
unlesbares. Ein Satz davor sagt, was es zeigt.

## Fallstricke

- **Beschriftungen in Anführungszeichen**, sobald sie Umlaute, ß, Leerzeichen, Klammern, Doppelpunkte,
  Schrägstriche, `#`, `&` oder Satzzeichen enthalten: `A["Größe prüfen (m²)"]`, Kantentext
  `-->|"ja, bestätigt"|`. Anführungszeichen im Text selbst als `#quot;` schreiben.
- **Kennungen** (`A`, `pruefung`, `db1`) nur aus ASCII-Buchstaben, Ziffern und `_`; kein `end` als
  Kennung (Schlüsselwort), sonst `End` oder `ende`.
- **Kein HTML** in Beschriftungen (`<br>`, `<b>` …): Die UI zeichnet mit `securityLevel: "strict"` und
  ohne HTML-Labels. Für Zeilenumbrüche lieber kürzere Beschriftungen.
- **Keine `click`-Direktiven, Links oder Callbacks**: im strikten Modus wirkungslos.
- **Keine `%%{init: …}%%`-Direktiven** für Theme, Schrift, HTML-Labels oder Sicherheit: Die UI setzt sie
  selbst und ignoriert Änderungen daran.
- Keine Stile mit Adressen (`url(...)`) oder Bildern aus dem Netz; sie werden entfernt.
- Bei einem Syntaxfehler zeigt die UI den Quelltext mit Hinweis. Dann den Fehler suchen (meist
  fehlende Anführungszeichen) und den Block korrigiert neu senden.

## Gängige Typen

**Ablauf** (`flowchart`, Richtung `TD` oben nach unten, `LR` links nach rechts):

```mermaid
flowchart LR
  A["Nachricht"] --> B{"Internet nötig?"}
  B -->|ja| C["Nutzer fragen"]
  B -->|nein| D["Ausführen"]
  C --> D
```

**Architektur** (Ablauf mit Gruppen):

```mermaid
flowchart TB
  subgraph host["Host"]
    O["Orchestrator"]
  end
  subgraph sandbox["Sandbox"]
    P["pi"] --> S[("Socket")]
  end
  S --> O
  O --> M["Sprachmodell"]
```

**Sequenz:**

```mermaid
sequenceDiagram
  participant N as Nutzer
  participant A as Agent
  participant O as Orchestrator
  N->>A: Auftrag
  A->>O: Werkzeugaufruf
  O-->>A: Ergebnis
  A-->>N: Antwort
```

**Zustände:**

```mermaid
stateDiagram-v2
  [*] --> Aktiv
  Aktiv --> Ruhend: ruhen lassen
  Ruhend --> Aktiv: fortsetzen
  Aktiv --> Beendet: beenden
  Beendet --> [*]
```

**Klassen:**

```mermaid
classDiagram
  class Chat {
    +String titel
    +senden(text)
  }
  class Nachricht {
    +String rolle
  }
  Chat "1" --> "*" Nachricht
```

**Datenmodell (ER):**

```mermaid
erDiagram
  CHAT ||--o{ NACHRICHT : enthaelt
  CHAT ||--o{ ARTEFAKT : erzeugt
  CHAT {
    string id
    string titel
  }
```

In ER-Diagrammen stehen Beziehungsnamen ohne Anführungszeichen nur in ASCII; mit Umlauten in
Anführungszeichen: `CHAT ||--o{ NACHRICHT : "enthält"`.

**Zeitplan (Gantt):**

```mermaid
gantt
  title Zeitplan
  dateFormat YYYY-MM-DD
  section Analyse
  Literatur      :a1, 2026-10-01, 14d
  section Umsetzung
  Prototyp       :a2, after a1, 21d
  Auswertung     :after a2, 10d
```

**Mindmap** (Einrückung bestimmt die Ebene):

```mermaid
mindmap
  root(("Agent"))
    Werkzeuge
      bash
      read
    Grenzen
      Internet
      Subagenten
```

**Kreis** (Anteile, nur für wenige Werte; genaue Zahlen lieber mit matplotlib):

```mermaid
pie title Kosten nach Art
  "Antworten" : 70
  "Subagenten" : 20
  "Kompaktierung" : 10
```
