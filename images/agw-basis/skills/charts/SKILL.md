---
name: charts
description: Create charts and graphics (line, bar, scatter, histogram, heatmap charts) and show them to the user in the chat. Use when data is to be visualised, plotted or presented as a graphic/image.
---

# Creating and showing charts

The standard tool is **matplotlib** (preinstalled, plus numpy and pandas; no internet needed).
The backend is `Agg` (no display), `MPLBACKEND=Agg` is set.

## Steps

1. Draw the chart with matplotlib, save it as **PNG** under `/workspace`:
   `fig.savefig("/workspace/<name>.png", dpi=150, bbox_inches="tight")`, then `plt.close(fig)`.
   **Never** `plt.show()`: there is no display, nothing appears.
2. Show it in the answer with `![Short description](/workspace/<name>.png)`. The UI then shows
   the image as a preview with an enlarged view. Allowed are PNG, JPEG, GIF and WebP under
   `/workspace`, `/tmp` or `/home/agent`, at most 10 MB. **Addresses from the internet and SVG
   are not displayed.**
3. If the user should keep the file, also store it as an artifact (skill `artifacts`).
   Displaying alone is not an upload.

## Defaults

- Size `figsize=(8, 4.5)` (16:9), with several subplots `layout="constrained"`.
- Readable font: `plt.rcParams.update({"font.size": 11, "axes.titlesize": 13})`.
- Label axes **with the unit** ("Temperature in °C", "Time in s"), plus a title.
- Subtle grid: `ax.grid(True, alpha=0.3)`; remove superfluous spines:
  `ax.spines[["top", "right"]].set_visible(False)`.
- Labels in German if the user writes German; then use a **decimal comma** on the
  axes (there is no German locale, so use a formatter, see the example).
- Colours that stay distinguishable with colour vision deficiency: categories `tab10` (default),
  continuous values `viridis` or `cividis`; not red against green as the only distinguishing feature.
- Legend only with several series; label values directly if there are few.

## pandas and plotly

- **pandas:** load data with `pd.read_csv(...)` and draw with `df.plot(ax=ax, ...)`; the rules above
  still apply, saving goes through `fig.savefig`.
- **plotly** is installed together with kaleido: `fig.write_image("/workspace/image.png")` writes a PNG
  (via the sandbox's Chromium, a few seconds for the first image). For images in the chat,
  matplotlib remains the first choice; plotly if the user wants it or an interactive HTML file
  is needed (`fig.write_html(...)`, then store it as an artifact).

## Charts in Typst documents

For charts in a Typst document, draw directly in Typst: `@preview/cetz` with `cetz-plot`
or `@preview/lilaq` (both preinstalled, see skill `writing-typst`). Alternatively embed a
matplotlib PNG with `#image("plot.png")`.

## Example

For a user who writes German (German labels, decimal comma):

```python
import matplotlib.pyplot as plt
import numpy as np
from matplotlib.ticker import FuncFormatter

plt.rcParams.update({"font.size": 11, "axes.titlesize": 13})
comma = FuncFormatter(lambda x, _: f"{x:g}".replace(".", ","))

t = np.linspace(0, 10, 200)
fig, ax = plt.subplots(figsize=(8, 4.5))
ax.plot(t, np.sin(t), label="Sinus")
ax.plot(t, 0.5 * np.cos(t), label="0,5 · Kosinus")
ax.set(title="Schwingungen", xlabel="Zeit in s", ylabel="Auslenkung in cm")
ax.xaxis.set_major_formatter(comma)
ax.yaxis.set_major_formatter(comma)
ax.grid(True, alpha=0.3)
ax.spines[["top", "right"]].set_visible(False)
ax.legend()
fig.savefig("/workspace/oscillations.png", dpi=150, bbox_inches="tight")
plt.close(fig)
```

Then in the answer: `![Sinus und Kosinus über 10 s](/workspace/oscillations.png)`
