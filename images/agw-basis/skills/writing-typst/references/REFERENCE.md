# Typst Reference Notes (Quick Navigation)

This file exists to keep `SKILL.md` short (progressive disclosure). It is NOT a copy of the official docs.

For authoritative details, use local docs in this skill, or upstream docs:
- Local docs root: references/overview.md
- Upstream doc tree: https://api.github.com/repos/typst/typst/git/trees/main:docs?recursive=1
- Upstream raw pattern: https://raw.githubusercontent.com/typst/typst/main/references/<path>.md

## Core Concepts
- Modes: markup (default), code (`#` prefix), math (`$...$`).
- Content blocks: `[...]` inside code (function args) to pass markup content.
- Labels: `<name>` attach to elements; reference with `@name`.
- See `references/reference/language/syntax.md`

## Typical Building Blocks
- See `references/reference/library/model.md`

## Counter + Context
- Counter values in `#show` rules often need `context` to render correctly.
- Example: `#show heading.where(level: 2): it => context [#counter(heading).display("1.1") #it.body]`
- See `references/reference/language/context.md`

### Document metadata
- `#set document(title: "...", author: "...", date: datetime.today())`
- See `references/overview.md`

### Page setup
- `#set page(paper: "a4", margin: (top: 20mm, bottom: 20mm, left: 20mm, right: 20mm))`
- Landscape: `#set page(flipped: true)`
- Note: changing `page(...)` mid-document forces a page break.
- See `references/guides/page-setup.md`

### Styling
- Set rules: `#set text(...)`, `#set heading(...)`, `#set par(...)`
- Show-set rules: `#show heading: set text(navy)`
- Transformational show rules: `#show heading: it => ...`
- Line spacing: use `#set par(leading: 1.1em)` (not `#set text(leading: ...)`)
- Color note: `rgb(...)` expects 0–255 integers, not 0–1 floats
- See `references/reference/language/styling.md`

### Query + heading lookup
- Use `query(...)` to find the active H1 for custom headers.
- See `references/reference/language/context.md`
```typst
#let current_h1() = {
  let current = here().page()
  let h = query(heading.where(level: 1).after(here()))
    .filter(x => x.location().page() == current)
    .at(0, default: none)

  if h == none {
    h = query(heading.where(level: 1).before(here()))
      .at(-1, default: none)
  }

  h
}
```

### Images & figures
- `#image("file.png", width: 70%, alt: "Short description")`
- `#figure(image("file.png", alt: "Short description"), caption: [Caption]) <fig:label>`
- Alignment note: `align(center + middle)` is not valid; use `align(center)` or a layout container
- `#show figure.caption: emph`
- See `references/reference/library/model.md`

### Tables
- `#table(columns: 3, ...)`
- Prefer `table.header(...)` for accessibility (even when styling is minimal).
- To caption/number a table: wrap in `#figure(table(...), caption: [...])`
- Track sizing reminders:
  - Repeat rows: `rows: (1fr,) * 8`
  - Header + rows: `rows: (auto, ..((1fr,) * 8))` (spread `..` flattens the array)
- See `references/guides/tables.md`

### Math
- Inline: `$x^2$`
- Display: `$ x^2 $`
- Accessible display: `#math.equation($ x^2 $, alt: "x squared")` (prefer full-sentence alt text)
- Alignment: use `&` alignment points and `\` line breaks.
- Multi-letter words: quote them: `$ "time offset" $`
- See `references/reference/library/math.md`

### Bibliography
- `#bibliography("works.bib", style: "ieee")`
- Citation: `@key` or `#cite(<key>)`
- See `references/reference/library/model.md` and `references/guides/guide-for-latex-users.md`

### Columns
- `#columns(2, gutter: 0.9em)[...body...]`
- Single-column title + columns: title/intro in markup, then wrap body in `#columns(...)`
- See `references/guides/page-setup.md`

### Export (PNG)
- PNG export (only when requested): `typst compile --format png file.typ` or `typst compile file.typ file.png`
- PNG preview: `qlmanage -t -s 1800 -o . file.pdf`
- See `references/reference/export/png.md`

## Docs Entry Points
- Local docs root: references/overview.md
- Local tutorial: references/tutorial/welcome.md
- Local syntax: references/reference/language/syntax.md
- Local styling: references/reference/language/styling.md
- Local scripting: references/reference/language/scripting.md
- Local model: references/reference/library/model.md
- Local math: references/reference/library/math.md
- Local accessibility: references/guides/accessibility.md
- Local LaTeX guide: references/guides/guide-for-latex-users.md
- Upstream doc tree: https://api.github.com/repos/typst/typst/git/trees/main:docs?recursive=1
- Upstream raw pattern: https://raw.githubusercontent.com/typst/typst/main/references/<path>.md
- Upstream raw example: https://raw.githubusercontent.com/typst/typst/main/references/overview.md
