---
name: diagramme
description: Diagramme und Grafiken erstellen (Linien-, Balken-, Streu-, Histogramm-, Heatmap-Diagramme) und dem Nutzer im Chat zeigen. Verwenden, wenn Daten visualisiert, geplottet oder als Grafik/Bild dargestellt werden sollen.
---

# Diagramme erstellen und zeigen

Standardwerkzeug ist **matplotlib** (vorinstalliert, dazu numpy und pandas; kein Internet nötig).
Das Backend ist `Agg` (ohne Bildschirm), `MPLBACKEND=Agg` ist gesetzt.

## Ablauf

1. Diagramm mit matplotlib zeichnen, als **PNG** unter `/workspace` speichern:
   `fig.savefig("/workspace/<name>.png", dpi=150, bbox_inches="tight")`, danach `plt.close(fig)`.
   **Nie** `plt.show()`: Es gibt keinen Bildschirm, es erscheint nichts.
2. In der Antwort zeigen mit `![Kurze Beschreibung](/workspace/<name>.png)`. Die Oberfläche zeigt
   das Bild dann als Vorschau mit Großansicht. Erlaubt sind PNG, JPEG, GIF und WebP unter
   `/workspace`, `/tmp` oder `/home/agent`, höchstens 10 MB. **Adressen aus dem Internet und SVG
   werden nicht angezeigt.**
3. Soll der Nutzer die Datei behalten, zusätzlich als Artefakt ablegen (Skill `artifacts`).
   Das Anzeigen allein ist kein Upload.

## Voreinstellungen

- Größe `figsize=(8, 4.5)` (16:9), bei mehreren Teilbildern `layout="constrained"`.
- Lesbare Schrift: `plt.rcParams.update({"font.size": 11, "axes.titlesize": 13})`.
- Achsen **mit Einheit** beschriften („Temperatur in °C“, „Zeit in s“), dazu ein Titel.
- Gitter dezent: `ax.grid(True, alpha=0.3)`; überflüssige Rahmen weg:
  `ax.spines[["top", "right"]].set_visible(False)`.
- Beschriftungen auf Deutsch, wenn der Nutzer Deutsch schreibt; dann **Dezimalkomma** an den
  Achsen (ein deutsches Locale gibt es nicht, deshalb über einen Formatter, siehe Beispiel).
- Farben, die auch bei Farbfehlsichtigkeit unterscheidbar sind: Kategorien `tab10` (Standard),
  stetige Werte `viridis` oder `cividis`; nicht Rot gegen Grün als einziges Unterscheidungsmerkmal.
- Legende nur bei mehreren Reihen; Werte direkt beschriften, wenn es wenige sind.

## pandas und plotly

- **pandas:** Daten mit `pd.read_csv(...)` laden und `df.plot(ax=ax, ...)` zeichnen; die Regeln oben
  gelten weiter, gespeichert wird über `fig.savefig`.
- **plotly** ist samt kaleido installiert: `fig.write_image("/workspace/bild.png")` schreibt ein PNG
  (über das Chromium der Sandbox, einige Sekunden beim ersten Bild). Für Bilder im Chat bleibt
  matplotlib die erste Wahl; plotly, wenn der Nutzer es möchte oder eine interaktive HTML-Datei
  gebraucht wird (`fig.write_html(...)`, dann als Artefakt ablegen).

## Diagramme in Typst-Dokumenten

Für Diagramme in einem Typst-Dokument direkt in Typst zeichnen: `@preview/cetz` mit `cetz-plot`
oder `@preview/lilaq` (beide vorinstalliert, siehe Skill `writing-typst`). Alternativ ein
matplotlib-PNG mit `#image("plot.png")` einbinden.

## Beispiel

```python
import matplotlib.pyplot as plt
import numpy as np
from matplotlib.ticker import FuncFormatter

plt.rcParams.update({"font.size": 11, "axes.titlesize": 13})
komma = FuncFormatter(lambda x, _: f"{x:g}".replace(".", ","))

t = np.linspace(0, 10, 200)
fig, ax = plt.subplots(figsize=(8, 4.5))
ax.plot(t, np.sin(t), label="Sinus")
ax.plot(t, 0.5 * np.cos(t), label="0,5 · Kosinus")
ax.set(title="Schwingungen", xlabel="Zeit in s", ylabel="Auslenkung in cm")
ax.xaxis.set_major_formatter(komma)
ax.yaxis.set_major_formatter(komma)
ax.grid(True, alpha=0.3)
ax.spines[["top", "right"]].set_visible(False)
ax.legend()
fig.savefig("/workspace/schwingungen.png", dpi=150, bbox_inches="tight")
plt.close(fig)
```

In der Antwort dann: `![Sinus und Kosinus über 10 s](/workspace/schwingungen.png)`
