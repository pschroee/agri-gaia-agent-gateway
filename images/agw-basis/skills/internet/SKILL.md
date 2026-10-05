---
name: internet
description: Internetzugang beim Nutzer erbitten. Verwenden, bevor etwas aus dem Netz geholt wird (pip install, curl, git clone, Webseiten), wenn Netzaufrufe fehlschlagen oder absehbar gebraucht werden.
---

# Internetzugang erbitten

Diese Sandbox hat **standardmäßig kein Internet**. Netzaufrufe schlagen dann mit Namens- oder
Verbindungsfehlern fehl. Der Weg zum Sprachmodell und zum Orchestrator besteht unabhängig davon.

Braucht die Aufgabe Internet, bitte den Nutzer darum — mit einer kurzen, konkreten Begründung:

```bash
agw-internet "pip install scikit-learn, um ein Klassifikationsmodell zu trainieren"
```

Der Befehl **wartet**, bis der Nutzer entscheidet (das kann Minuten dauern); rufe ihn mit großzügiger
Zeitgrenze auf (mindestens 900 Sekunden) und nicht im Hintergrund.

- Exit-Code 0, `bestätigt: …` — Internet ist jetzt da, fahre fort.
- Exit-Code 3, `abgelehnt: …` — nicht erneut fragen, ohne dass der Nutzer es wünscht. Arbeite ohne
  Netz weiter oder erkläre, was ohne Internet nicht geht.

Frage nicht vorsorglich, sondern erst, wenn die Aufgabe es wirklich braucht. Vorinstalliert sind
unter anderem numpy, pandas, matplotlib, plotly, jinja2 und openpyxl; dafür braucht es kein Internet.

## Pakete installieren

Mit Internetzugang laufen `pip install` und `npm install` automatisch über Paket-Zwischenspeicher
(`pip-cache`, `npm-cache`); einmal geladene Pakete kommen beim nächsten Mal schneller. Ohne
Internetzugang scheitern sie nach wenigen Sekunden. Dann nicht wiederholen, sondern Internet erbitten
oder mit den vorinstallierten Paketen arbeiten. Nachinstallierte Pakete gehen verloren, wenn der Chat
ruht und in einer frischen Sandbox fortgesetzt wird; installiere sie dann erneut.
