# Typst Examples

These are compact, copy-pasteable patterns.

## 1) Minimal document with title block
```typst
#set page(paper: "a4", margin: 2.5cm)
#set text(font: "Libertinus Serif", size: 11pt, lang: "en", region: "US")

#let title = "Assignment 1"
#let author = "Your Name"
#let date = datetime.today()

#align(center)[
  #text(size: 18pt, weight: "bold")[#title]
  #v(4pt)
  #author " - " #date.display("[year]-[month]-[day]")
]

#set document(title: #title, author: #author, date: #date)
#set text(lang: "en", region: "US")

= Tasks
- Solve problem 1.
- Submit as PDF.
```

## 2) Figure with caption + label + reference
```typst
See @fig:demo.

#figure(
  image("plot.png", width: 70%, alt: "Demo plot"),
  caption: [A demo plot.],
) <fig:demo>
```

## 3) Table with header + caption
```typst
#figure(
  table(
    columns: 3,
    table.header([*Param*], [*Meaning*], [*Value*]),
    [m], [Mass], [2.0 kg],
    [c], [Speed], [3.0e8 m/s],
  ),
  caption: [Parameters used in the experiment.],
)
```

## 4) Math patterns
```typst
Inline: $E = m c^2$.

Display:
$ sum_(k=1)^n k = (n(n+1))/2 $

Aligned:
$ a &= b + c \
  &= d $
```

## 5) Small custom block (theorem-ish)
```typst
#let theorem(title: none, body) = block(
  fill: luma(250),
  stroke: luma(180),
  radius: 3pt,
  inset: (x: 10pt, y: 8pt),
  breakable: true,
)[
  *Theorem*#if title != none { [ (] + title + [)] } \
  #body
]

#theorem(title: [Example])[For a right triangle, $a^2 + b^2 = c^2$.]
```

## 6) One-page landscape table that fills the page
```typst
#set page(paper: "a4", flipped: true, margin: 8mm)
#set text(font: "New Computer Modern", size: 8pt, lang: "en", region: "US")
#set par(leading: 1.0em, spacing: 0pt)

#table(
  columns: (1fr, 2fr, 1fr),
  rows: (auto, ..((1fr,) * 8)),
  stroke: 0.6pt + luma(140),
  inset: 2mm,

  table.header([*Goal*], [*Details*], [*Price*]),
  // ... 8 rows of data (3 cells per row)
)
```

## 7) Single-column title + multi-column body
```typst
#set page(paper: "a4", margin: 2.2cm)
#set text(font: "Libertinus Serif", size: 11pt, lang: "en", region: "US")

= Project Overview
This introduction stays single-column.

#columns(2, gutter: 1em)[
  == Method
  Short method summary for column layout.

  == Results
  Highlights of the main outcomes.
]
```

## 8) Counter-based tasks (with context)
```typst
#let task = counter("task")

#show heading.where(level: 2): it => context [
  #counter(heading).display("1.1") #it.body
]

== Tasks
#task.step()
#task.display("1.") [Collect data]
#task.step()
#task.display("1.") [Analyze results]
```

## 9) Accessible display math
```typst
Inline: $E = m c^2$.

#math.equation(
  $ x^2 $,
  alt: "The value of x squared.",
)
```

## 10) PNG export (command line)
```bash
typst compile --format png file.typ
```
