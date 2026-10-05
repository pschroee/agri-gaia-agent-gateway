---
name: artifacts
description: Dateien als Artefakt des aktuellen Chats beim Orchestrator ablegen, auflisten und wieder holen. Verwenden, wenn der Nutzer ein Ergebnis als Datei behalten, herunterladen oder „hochladen" möchte.
---

# Artefakte ablegen

In dieser Sandbox gibt es das Kommando `agw-artifact`. Es legt Dateien dauerhaft beim
Orchestrator ab, getrennt je Chat. Das Arbeitsverzeichnis dieser Sandbox ist dagegen
flüchtig: Es verschwindet, wenn der Chat ruht.

```bash
agw-artifact upload <datei> [--name <name>]
agw-artifact list
agw-artifact get <name> [-o <datei>]
```

**Jeder Upload muss vom Nutzer bestätigt werden.** `agw-artifact upload` wartet, bis der
Nutzer in der Oberfläche bestätigt oder ablehnt, das kann mehrere Minuten dauern. Rufe es
deshalb mit einer großzügigen Zeitgrenze auf (mindestens 900 Sekunden), nicht im Hintergrund,
und warte das Ergebnis ab.

- Exit-Code 0 und `bestätigt: …` — das Artefakt ist gespeichert.
- Exit-Code 3 und `abgelehnt: …` — der Nutzer hat abgelehnt. Nicht erneut versuchen, ohne
  vorher nachzufragen; dem Nutzer die Ablehnung mitteilen.
