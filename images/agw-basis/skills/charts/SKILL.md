---
name: charts
description: Create charts and graphics (line, bar, scatter, histogram, heatmap charts) and show them to the user in the chat. Use when data is to be visualised, plotted or presented as a graphic/image, and after a training or a training search to show the learning curves unasked (section Evaluation charts). In replies name files by their file name, never by sandbox path.
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
3. If the user should keep the file, also send it with `agw-artifact upload <file>` (skill `artifacts`;
   no approval, it appears in the chat at once). Displaying alone does not send it.

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
  is needed (`fig.write_html(...)`, then send it with `agw-artifact upload`).

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

## Evaluation charts

After a training or a training search, draw the learning curves **unasked** and show them in the answer. The data
comes from `agw-platform training-curves` (`--train-container-id <id>` for one training, `--search-id <id>` for the trials
of a search): per run the epochs with `train_loss`, `val_loss`, `train_acc`, `val_acc` as the run wrote them. It
holds figures of the training dataset and of the template's validation split **inside** it, never the held-out set.

| Situation | Chart | Note |
|---|---|---|
| After a training | two panels: loss and accuracy per epoch, training against validation, x = epoch | draw only the series the run wrote; a run without per-epoch figures gets a sentence, no chart |
| After a search | the learning curves of all trials in one panel per metric, the winner emphasised; or the target value per trial | from 3 trials a chart, with fewer a table |
| Flow of a plan, split of a dataset | a Mermaid diagram (skill `mermaid`) | Mermaid is for flows, not for figures |
| Regression, other numeric results of your own code | prediction against actual value (with diagonal), residuals | every point shown, also with few points |

Rules:

- **No chart about the check.** The check card shows the confusion matrix and the figures per class; never draw or
  describe the checked set (skill `result-check`). Do not draw ROC, PR or calibration curves either: the gateway
  hands out no per-image scores.
- **Never show images of the held-out or validation set to a model**, and do not use them in a chart.
- With a small number of images or epochs name the number next to the value ("92 % of 40 images"), and do not
  smooth curves.
- Title, axis labels and legend in the language of the user, with decimal comma in German (see the example above).

