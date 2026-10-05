---
name: platform
description: Mit der Agri-Gaia-Plattform arbeiten (Datensätze, Modelle, Training, Hintergrundaufgaben, Edge-Geräte, Container-Abbilder) über agw-platform. Verwenden, wenn der Nutzer nach Daten oder Modellen auf der Plattform fragt oder dort ein Training anlegen, starten oder verfolgen will.
---

# Agri-Gaia-Plattform

Agri-Gaia ist eine KI-Plattform für die Agrar- und Ernährungswirtschaft: Datensätze (meist Bilder mit
Annotationen), Training aus installierten Vorlagen, Modellverwaltung, Inferenz-Container und
Edge-Geräte. Du erreichst sie **nur** über `agw-platform`; der Orchestrator meldet sich dort an, ein
Token gibt es in der Sandbox nicht, und `curl` auf die Plattform führt nicht weiter.

```bash
agw-platform --help                 # alle Befehle
agw-platform datasets               # Datensätze (alle Nutzer)
agw-platform dataset 3              # ein Datensatz
agw-platform models                 # Modelle
agw-platform trainings              # Trainingscontainer mit status und score
agw-platform tasks --limit 20       # Hintergrundaufgaben der Plattform
```

Die Ausgabe ist `HTTP <code>` und danach JSON. Exit-Code 0 ok, 3 vom Nutzer abgelehnt, 1 Fehler
(auch eine Fehlerantwort der Plattform). Lange Antworten sind gekürzt; dann mit `--skip`/`--limit`
nachfragen oder mit `jq` filtern (`agw-platform datasets | tail -n +2 | jq '.[].name'`).

## Training anlegen und verfolgen

1. Vorlagen erkunden: `agw-platform train-options` (Anbieter), `agw-platform train-options Torchvision`
   (Architekturen samt `category`), `agw-platform train-options Torchvision EfficientNet` (Schema und
   Standardwerte der `train_config`).
2. `train_config` als Datei schreiben, von den Standardwerten ausgehend, nur ändern, was die Aufgabe
   verlangt (auf dieser Instanz gibt es **keine GPU**: wenige Epochen, kleine Bilder).
3. Anlegen: `agw-platform create-training Torchvision EfficientNet Classification <dataset_id> @train_config.json`
   — die Plattform baut das Trainingsabbild im Hintergrund. Die Antwort nennt `Location: /tasks/<id>`.
4. Bau verfolgen: `agw-platform task <id>` bis `status` `completed` oder `failed` ist (nicht in enger
   Schleife; zwischen den Abfragen mindestens 20 Sekunden warten). Danach `agw-platform trainings`:
   der neue Eintrag hat die `id` des Trainingscontainers.
5. Starten: `agw-platform start-training <train_container_id>`, verfolgen mit
   `agw-platform training-status <id>` und `agw-platform training-logs <id> --tail 50`.

## Dateien hochladen

Dateien aus der Sandbox lädst du mit zwei Befehlen hoch; der Orchestrator liest sie selbst aus der
Sandbox, und der Nutzer sieht vor der Bestätigung Name, Größe und SHA-256 jeder Datei:

```bash
# Datensatz: Name, Beschreibung, dann die Dateien (Glob geht); Klassen als wiederholte Option,
# eine CVAT-Annotation (annotations.xml) als --annotation-file
agw-platform upload-dataset ferkel-bilder "Ferkel, Stall 3, Oktober" bilder/*.png \
  --annotation-labels 0 --annotation-labels 1 --annotation-file annotations.xml
# Modell: Name, Beschreibung, Format (onnx, pytorch, tensorflow, tensorrt), Datei
agw-platform upload-model mnist-klein "MNIST, zwei Klassen" onnx model.onnx
```

Höchstens 2 000 Dateien und 512 MB je Aufruf, jede Datei höchstens so groß wie ein Artefakt.
Schlagwörter (`--keywords`) sind bei beiden **AGROVOC-URIs**, keine freien Wörter; suchen mit
`agw-platform request GET /agrovoc/keywords --query keyword=pig`. „Classification Dataset“ setzt der Orchestrator nie (ein Fehler der Plattform verwirft sonst die
Klassen); die Klassen gehen über `--annotation-labels`.

## Übertragene Rechte

Der Nutzer kann dir für einen Chat nur bestimmte Rechte übertragen (welche Aktionen auf welchen
Objekten). `agw-platform rights` zeigt sie, samt den Objekten, die du in diesem Chat angelegt hast.
Sieh vor schreibenden Aufrufen nach. Was außerhalb liegt, weist der Autorisierungsdienst ab
(Exit-Code 4, „verweigert vom Autorisierungsdienst"); versuche es dann nicht auf anderem Weg,
sondern sag dem Nutzer, welches Recht fehlt.

## Was eine Bestätigung braucht

Lesen geht direkt. **Jeder schreibende Aufruf** (`create-training`, `start-training`, `upload-dataset`,
`upload-model`, `request` mit
POST, PUT, PATCH oder DELETE, dazu einige GETs, die auf der Plattform etwas anlegen, etwa
`/train/containers/<id>/model`) wartet, bis der Nutzer in der Oberfläche zustimmt. Rufe solche Befehle
mit großzügiger Zeitgrenze auf (mindestens 900 Sekunden) und nicht im Hintergrund. Bei Exit-Code 3
nicht erneut fragen, ohne dass der Nutzer es wünscht.

## Alles andere

`agw-platform request <METHODE> <pfad> [--query k=v …] [--body JSON|@datei|-]` ruft jeden Pfad der
REST-API auf, nur mit JSON-Körper (keine Datei-Uploads). Welche Pfade es gibt, zeigt
`agw-platform api-paths` (eine Zeile je Operation; `agw-platform api-paths /train` nur für Training).
Pfade unter `/users`, `/urls`, `/service` und `/network` sind gesperrt. Werte von Passwörtern, Schlüsseln
und Tokens erscheinen als `[geschwärzt vom Orchestrator]`, Downloads nur als Größe und Typ.

Die Plattform unterscheidet bisher nur angemeldet oder nicht: Listen zeigen die Objekte **aller**
Nutzer. Lösche oder ändere nichts, was der Nutzer nicht ausdrücklich verlangt hat.
