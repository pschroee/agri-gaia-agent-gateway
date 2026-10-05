---
name: writing-typst
description: Use whenever the user mentions Typst / .typ (e.g. "Typst document", "write Typst", "onepager", "Typst table", "typst compile"), or when writing/editing Typst documents, converting from LaTeX, or providing Typst syntax for headings, lists, math, tables, figures, citations, templates, or styling.
compatibility: opencode, claude
---

# Writing Typst (Not LaTeX)

## Overview
Typst is not LaTeX. It has its own markup, math syntax, and a small scripting language.

The main failure mode to avoid: emitting LaTeX commands/environments (e.g. `\\section`, `\\begin{...}`, `\\usepackage`, `\\textbf{}`) instead of valid Typst markup and functions.

## When to Use
Use this skill when the request involves any of:
- creating or editing `.typ` files
- document structure (title/author/date, headings, outline)
- styling (set rules, show rules)
- math and equations
- figures, captions, labels, and references
- tables
- citations and bibliographies
- templates and packages (Typst Universe)

## Default Compile Behavior
- Default: compile into the current working directory (same folder as the `.typ`), i.e. run `typst compile <file>.typ` and expect `<file>.pdf` next to it.
- PNG export (only if requested): `typst compile --format png <file>.typ` or `typst compile <file>.typ <file>.png` (writes next to source by default).
- Only write to `/tmp` (or other output paths) if the user explicitly asks for it.

Do NOT use this skill for:
- TeX/LaTeX documents (`.tex`)
- Markdown-only formatting tasks

## First Principles (Typst Mental Model)
- **Three modes:** markup (default), code (prefix with `#`), math (wrap with `$...$`).
- **Markup syntax covers common things:** headings `=`, lists `-` / `+`, emphasis `*` / `_`, labels `<name>`, refs `@name`, code/raw using backticks.
- **Everything else is functions:** `#image(...)`, `#figure(...)`, `#table(...)`, `#bibliography(...)`.
- **Styling uses rules:**
  - `#set ...` sets defaults from that point in scope.
  - `#show ...: set ...` or `#show ...: it => ...` transforms elements.

## Red Flags (Stop and Fix)
If you see yourself writing any of these, you are drifting into LaTeX:
- `\\documentclass`, `\\usepackage`, `\\begin{}`, `\\end{}`
- `\\section{}`, `\\subsection{}`
- `\\textbf{}`, `\\emph{}`
- `\\label{}`, `\\ref{}`, `\\cite{}`

**Fix:** Delete LaTeX commands/environments and rewrite using Typst markup/functions from the Quick Reference.

## Common Rationalizations
| Excuse | Reality |
| --- | --- |
| "I can just use LaTeX inside Typst" | Typst is not a TeX engine. Use Typst syntax. |
| "Backslashes are fine" | Backslashes in Typst are escapes/line breaks, not LaTeX macros. |
| "I'll keep the LaTeX as reference" | You will copy it. Rewrite directly in Typst. |
| "I only need one command" | One LaTeX command tends to cascade into environments/packages. Stay in Typst. |

## Red Flags (Stop Immediately)
- You type a leading `\\` for a command
- You write `\\begin{` / `\\end{`
- You reach for `\\usepackage`
- You write `\\label{...}` / `\\ref{...}` / `\\cite{...}`

If any trigger hits: remove the LaTeX and restate the same intent in Typst.

## Quick Reference

### Structure
- **Title metadata (simple):** `#set document(title: "...", author: "...", date: datetime.today())`
- **Document language:** `#set text(lang: "en")` (optionally add `region: "US"`; set early for hyphenation + accessibility)
- **Headings:** `= H1`, `== H2`, `=== H3`
- **Paragraphs:** blank line separates paragraphs
- **Columns:** `#columns(3, gutter: 0.9em)[...]` for multi-column layouts
- **Single-column title + columns:** `= Title` then `#columns(2)[...body...]` to switch layouts cleanly
- **See:** `references/reference/language/syntax.md`

### Inline formatting
- **Bold:** `*bold*`
- **Italic:** `_italic_`
- **Monospace text:** `#text(font: "DejaVu Sans Mono")[...]` for formatted mono; use raw backticks only for verbatim.
- **See:** `references/reference/library/text.md`

### Lists
- **Bulleted list:** `- item`
- **Numbered list:** `+ item`
- **Nested lists:** indent the sub-list under the item
- **Custom enum style:** `#set enum(numbering: "(a)", indent: 1.1em, body-indent: 0.25em)`
- **See:** `references/reference/language/syntax.md`

### Figures + labels + refs
- **Image:** `#image("path.png", width: 70%, alt: "Short description")`
- **Figure with caption:**
  - `#figure(image("a.png", alt: "Short description"), caption: [Caption]) <fig:label>`
- **Reference in text:** `See @fig:label`
- **Alignment note:** `align(center + middle)` is not valid; use `align(center)` or a proper layout container
- **See:** `references/reference/library/model.md`

### Tables
- **Table:** `#table(columns: 3, [A], [B], [C], ...)`
- **Header row:** `table.header(...)` for semantics + accessibility (use even when styling is minimal)
- **Caption + numbering:** wrap in `#figure(table(...), caption: [...])`
- **See:** `references/guides/tables.md`

### Math
- **Inline:** `$x^2$`
- **Display:** `$ x^2 $` (note spaces)
- **Accessible display:** `#math.equation($ x^2 $, alt: "x squared")` (prefer full-sentence alt text)
- **Sub/superscripts:** `$x_1$`, `$x^2$`, multi-part with parentheses: `$x_(a -> b)$`
- **Fractions:** `/` becomes a fraction with precedence, e.g. `$ (a+b)/2 $`
- **Multi-letter text:** quote it: `$ "time offset" $`
- **Greek and symbols:** `alpha`, `beta`, `RR`, `->`, `<=`, variants via `symbol.mod`
- **Alignment example:** see `references/reference/library/math.md#alignment`
- **See:** `references/reference/library/math.md`


### Styling
- **Page setup:** `#set page(paper: "a4", margin: (top: 20mm, bottom: 20mm, left: 22mm, right: 22mm))` (see `references/guides/page-setup.md`)
- **Global text style:** `#set text(font: "Libertinus Serif", size: 11pt)`
- **Paragraph spacing:** `#set par(leading: 1.2em)`
- **Set heading numbering:** `#set heading(numbering: "1.")`
- **Show rule (simple):** `#show heading: set text(navy)` (applies globally to all headings; see `references/reference/language/styling.md`)

### Charts / Plots (Cetz)
- **Import:** `#import "@preview/cetz:0.4.0"`
- **Plot module:** `#import "@preview/cetz-plot:0.1.2": plot` (see `references/reference/library/visualize.md`)
- **Canvas + plot:**
  - `#cetz.canvas({ plot.plot(size: (3.2, 5.76), x-min: 0, x-max: 2.5, y-min: 0, y-max: 4.5, plot.add(domain: (0, 2), x => 3*x/2 + 1) ) })`

### Counters + custom headings
- **Counter:** `#let task = counter("task")` and `#task.update(1)`
- **Context rule:** `counter(heading)` and `task.display(...)` often need `context` inside `#show` rules
- **Numbered heading from counter:** `#show heading.where(level: 2): it => context block[#text(weight: "bold")[#task.display("1.") Task] #task.step()]`
- **See:** `references/reference/language/context.md`

### Bibliography / citations
- **Add bibliography:** `#bibliography("works.bib", style: "ieee")`
- **Cite:** `@key` (same syntax as label references)
- **See:** `references/reference/library/model.md` and `references/guides/guide-for-latex-users.md`

## Practical Patterns
- **Conditional content in outline vs body:** use `state` + `context` to switch behavior when the outline is rendered (e.g. hide or shorten captions in body, show full text in lists).
- **Caption helpers:** wrap caption construction in a function so you can add an optional second line (e.g. sources) without duplicating layout.
- **List-of-* sections:** generate lists with `#outline(target: figure.where(kind: ...))` instead of manual lists (see `references/reference/library/model.md`).
- **Figure supplements:** use `#show figure.where(kind: raw): set figure(supplement: [Code])` to name figure groups in lists.
- **Custom numbering regions:** reset counters and numbering when switching front matter, main body, or appendix.
- **Advanced header logic:** see `references/reference/language/context.md#location-context` and `references/reference/library/introspection.md`
- **Page-aware headers/footers:** use `context` + `here()` to branch by page location (see `references/reference/language/context.md`)

## One Solid Example (Typical Academic Short Report)
```typst
#set document(
  title: "Short Report",
  author: "Your Name",
  date: datetime.today(),
)

#set page(
  paper: "a4",
  margin: (top: 20mm, bottom: 22mm, left: 22mm, right: 22mm),
)
#set text(font: "Libertinus Serif", size: 11pt)
#set text(lang: "en", region: "US")
#set par(leading: 1.25em)
#set heading(numbering: "1.")

= Introduction
Typst is a markup-based typesetting system. This document demonstrates
headings, lists, math, figures with references, tables, and citations.

- Bulleted lists use a leading hyphen.
+ Numbered lists use a leading plus.

A key equation (inline): $E = m c^2$.

A displayed equation:
$ sum_(k=1)^n k = (n(n+1))/2 $

= Figure
See @fig:placeholder.

#figure(
  block(
    width: 100%,
    height: 40mm,
    fill: luma(245),
    stroke: luma(190),
    radius: 3pt,
    align(center)[*Figure Placeholder*],
  ),
  caption: [A placeholder figure with a caption.],
) <fig:placeholder>

= Table
#figure(
  table(
    columns: 3,
    table.header([*Item*], [*Meaning*], [*Value*]),
    [m], [Mass], [2.0 kg],
    [c], [Speed of light], [3.00e8 m/s],
    [E], [Energy], [1.80e17 J],
  ),
  caption: [Key parameters used in the report.],
)

= References
This cites a placeholder entry: @doe2020.
#bibliography("works.bib", style: "ieee")
```

## Common Mistakes (Typst vs. LaTeX)
- **Writing LaTeX commands:** Typst has no `\\section` / `\\begin{}`; use `=` headings and Typst functions.
- **Forgetting mode rules:** `#` only needed to enter code from markup; inside function arguments you're already in code.
- **Using raw backticks for styled mono:** raw is verbatim; use `text(font: ...)` when you need formatting inside.
- **Math confusion:** Typst math is not LaTeX; use Typst symbols (`RR`, `pi`, `->`) and quote multi-letter words.

## Common Typst Pitfalls
- **Landscape pages:** Use `#set page(flipped: true)` (there is no `landscape: true`).
- **Line spacing:** `leading` is a `par` property, not a `text` property. Use `#set par(leading: 1.1em)`.
- **Colors:** `rgb(...)` expects 0–255 integer values, not 0–1 floats.
- **Unicode text:** Use proper Unicode characters (e.g. accented letters, arrows) when writing non-ASCII text; do not transliterate unless the user asks.
- **Fonts on CLI:** Many fonts are not available by default. If you see `warning: unknown font family`, run `typst fonts` and pick from the available families.
- **Table row/column sizing:** `table(rows: ...)` expects track sizes (`auto`, `1fr`, lengths, arrays of those). Don’t pass nested arrays.
  - Repeating rows: `rows: (1fr,) * 8`
  - Header + repeated rows: `rows: (auto, ..((1fr,) * 8))` (note the spread `..`)
- **Tables across pages:** `figure` blocks are not breakable by default. Use `#show figure: set block(breakable: true)` if a table in a figure must break across pages.
- **Accessibility:** For PDF/UA, add `alt` text to images and `alt` to `math.equation`; set `#set document(title: ...)` and `#set text(lang: "en")` early.
- **HTML export:** Requires `--features html` or `TYPST_FEATURES=html`; still preview.
- **Avoid hidden control chars:** Don’t generate `\u0000` / NUL bytes or other invisible separators in `.typ` files; they can make files unreadable or cause strange parsing errors.

## Quick Troubleshooting
- `unexpected argument: leading` (in `#set text(...)`): move `leading` to `#set par(...)`.
- `expected content, found integer` around `repeat(...)`: use track repetition like `(1fr,) * n`.

## Preflight Checklist
- Hard rule: If `typst compile` shows an error or warning, fix it first (or explicitly tell the user what’s missing/why) before presenting the final snippet.
- Validate syntax: run `typst compile <file>.typ` once (writes `<file>.pdf` next to the source; avoid `/tmp` unless explicitly requested).
- Font sanity (CLI):
  - If you see `unknown font family`: run `typst fonts` and choose an installed font.
  - Avoid assuming fonts like `Libertinus Sans` exist; default-safe: `Libertinus Serif` / `New Computer Modern` / `DejaVu Sans Mono`.
- Export target awareness:
  - PDF is default; HTML requires `--features html` or `TYPST_FEATURES=html`.
  - If PDF/UA or PDF/A requested, ensure all required metadata/alt text is present.
- Accessibility:
  - Images: add `alt` on `image(...)`.
  - Math: use `math.equation(alt: ...)` when accessibility matters.
  - Document: set `#set document(title: ...)` and `#set text(lang: "en")` early.
- Quick PNG preview (debug only): use `qlmanage -t -s 1800 -o . <file>.pdf` to generate `./<file>.pdf.png` only when you need visual debugging; do not create PNGs by default.
- PNG export (requested only): `typst compile --format png <file>.typ` or `typst compile <file>.typ <file>.png` for direct PNG output.
- Page orientation & sizing:
  - Landscape: use `#set page(flipped: true)`.
  - Avoid mid-document `#set page(...)` changes unless you want a page break.
- Spacing rules:
  - Line spacing: use `#set par(leading: ...)` (not `#set text(leading: ...)`).
  - If you need more compact text: lower `leading`, and also consider lowering `table.inset`.
- One-page “full bleed” / single-container layouts:
  - Make a deliberate choice between page `margin` and container `inset` (don’t stack them blindly).
  - Borders: prefer exactly one outer border source.
    - Use table `stroke` if you want a full grid including outer border.
    - Use `rect(stroke: ...)` only if table `stroke` is `none` or you’re drawing a custom border.
- Tables (structure checks):
  - Confirm the requested structure matches: number of columns, presence/absence of header, grid style (full grid vs only horizontal lines).
  - If unclear, ask before writing the table.
  - `rows:` must be track sizes; common patterns:
    - Repeat rows: `rows: (1fr,) * n`
    - Header + rows: `rows: (auto, ..((1fr,) * n))`
  - Use `table.header(...)` whenever there is a header row.
- Encoding sanity:
  - Don’t introduce NUL bytes / hidden control characters when pasting text into `.typ`.

## Additional Resources
Local docs (preferred for deep dives):
- Overview + entry points: references/overview.md
- Tutorial (quick refresher): references/tutorial/welcome.md
- Syntax cheat sheet: references/reference/language/syntax.md
- Styling rules (set/show): references/reference/language/styling.md
- Scripting: references/reference/language/scripting.md
- Context + introspection: references/reference/language/context.md
- Page setup: references/guides/page-setup.md
- Tables guide: references/guides/tables.md
- Accessibility: references/guides/accessibility.md
- LaTeX user guide: references/guides/guide-for-latex-users.md
- Export targets: references/reference/export/pdf.md, references/reference/export/png.md, references/reference/export/svg.md, references/reference/export/html.md
- Function categories: references/reference/library/model.md, references/reference/library/text.md, references/reference/library/math.md, references/reference/library/layout.md, references/reference/library/visualize.md, references/reference/library/introspection.md, references/reference/library/data-loading.md, references/reference/library/foundations.md, references/reference/library/symbols.md

When to consult which doc (quick map):
- Tables layout, strokes, captions, pagination: references/guides/tables.md
- Page size, margins, headers/footers, columns: references/guides/page-setup.md
- Show/set rules, selectors, styling patterns: references/reference/language/styling.md
- Functions, variables, data, control flow: references/reference/language/scripting.md
- Context, counters, queries, location logic: references/reference/language/context.md + references/reference/library/introspection.md
- Math syntax, symbols, alignment, accessibility: references/reference/library/math.md
- Accessibility + PDF/UA rules, alt text: references/guides/accessibility.md + references/reference/export/pdf.md
- Export formats + CLI flags: references/reference/export/pdf.md, references/reference/export/png.md, references/reference/export/svg.md, references/reference/export/html.md
- LaTeX migration concerns: references/guides/guide-for-latex-users.md

Quick jump (topic -> section):
- Table strokes: references/guides/tables.md#strokes + references/guides/tables.md#stroke-functions
- Table cells/overrides: references/guides/tables.md#fill-override + references/guides/tables.md#stroke-override
- Table lines: references/guides/tables.md#individual-lines
- Table pagination in figures: references/guides/tables.md#table-across-pages
- Table sizing: references/guides/tables.md#column-sizes
- Table captions/refs: references/guides/tables.md#captions-and-references
- Context basics: references/reference/language/context.md#context
- Counters + here/locate: references/reference/language/context.md#location-context
- Headers/footers conditionals: references/guides/page-setup.md#specific-pages
- Columns + single-column title: references/guides/page-setup.md#columns
- Rotate tables/pages: references/guides/tables.md#rotate-table
- Math alignment: references/reference/library/math.md#alignment
- Math function calls: references/reference/library/math.md#function-calls
- Math accessibility: references/reference/library/math.md#accessibility
- Export PDF/UA: references/reference/export/pdf.md#pdf-ua
- HTML export flags: references/reference/export/html.md#exporting-as-html

Upstream docs (GitHub, for when local files are missing/outdated):
- Doc tree (current list, filter `.md` client-side): https://api.github.com/repos/typst/typst/git/trees/main:docs?recursive=1
- Raw doc pattern: https://raw.githubusercontent.com/typst/typst/main/references/<path>.md
- Raw doc example: https://raw.githubusercontent.com/typst/typst/main/references/overview.md


## In this sandbox (agw-basis)

- `typst` **0.14.2** is installed: `typst compile file.typ` produces `file.pdf`,
  `typst compile file.typ output.png` an image.
- **Only these packages are available** (no internet, baked into the image; others cannot be fetched):
  `@preview/cetz` 0.4.0 and 0.5.2, `@preview/cetz-plot` 0.1.2 and 0.1.4, `@preview/fletcher:0.5.8`,
  `@preview/touying:0.7.4`, `@preview/codly:1.3.0`, `@preview/glossarium` 0.5.9 and 0.5.10,
  `@preview/zebraw` 0.6.1 and 0.6.3, `@preview/pintorita:0.1.4`, `@preview/cmarker:0.1.8`,
  `@preview/cheq` 0.3.0 and 0.3.1, `@preview/showybox:2.0.4`, `@preview/lilaq:0.6.0`, `@preview/tablem:0.3.0`.
  The list is also in `/opt/typst/packages.txt`.
- If an `#import "@preview/…"` fails, the package is not in the image: fall back to one of the
  packages above, do not try to download it.
- Store the finished PDF as an artifact if needed (`agw-artifact upload file.pdf`).
