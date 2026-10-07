---
name: mermaid
description: Show processes, architectures, states, sequences, schedules (Gantt) and data models (ER) as a Mermaid diagram directly in the answer. Use when the user wants a diagram, flowchart, process diagram, sequence diagram, state diagram, class diagram, ER diagram, Gantt chart, mind map or a sketch of an architecture, or when a relationship is clearer as a picture than as text. Rules, because the web UI renders strictly and without HTML - no HTML tags in labels (no <br>, no <b>), for a line break use a shorter label or a real line break inside a quoted label; put labels with spaces, umlauts, brackets or punctuation in double quotes, A["Größe (m²)"]; no %%{init}%% and no click directives. Check every diagram with mermaid-check before answering.
---

# Showing Mermaid diagrams

## Mermaid or matplotlib?

- **Mermaid** for structure: processes, decisions, architectures, states, message sequences,
  schedules, data models, outlines. No tool, no file needed.
- **matplotlib** (skill `charts`) for **data**: measurements, trends, distributions, anything with axes and
  numbers. A bar chart from a CSV is not a case for Mermaid.

## How to show it

Write the diagram as a code block with the language `mermaid` in the answer:

````markdown
```mermaid
flowchart TD
  A["Request"] --> B["Check"]
```
````

The web UI renders the block as soon as it is closed; the user can switch between diagram and
source and enlarge it. If it cannot render the block, the user only sees a note and has to ask you again. **Only the web UI renders**: in the CLI, in artifacts and in
files it stays source text. That is the normal case; you only need a file if the user wants
one.

## Check before answering: mermaid-check

Before you send a diagram, check it in the sandbox with `mermaid-check`. It renders with `mmdc` and the
same settings as the web UI (`securityLevel: "strict"`, no HTML labels, `/opt/agw/mmdc/ui-config.json`)
and also refuses HTML tags, so it finds the errors the UI would show:

```bash
mermaid-check - <<'EOF'
flowchart LR
  A["Größe prüfen"] --> B["fertig"]
EOF
mermaid-check diagram.mmd        # one diagram from a file
mermaid-check answer.md          # every mermaid block of a Markdown file
```

`OK` means the UI renders it. Otherwise fix the line the message names (usually missing quotes or an
HTML tag) and check again; send only a diagram that passed. Without bash (MCP, REST) there is no check,
so follow the pitfalls below all the more strictly.

## If need be as a file: mmdc

If the diagram is to become a file (artifact, Typst document, image with `![…](…)`), render it
with the Mermaid CLI `mmdc` (version 12, like the web UI; runs without internet, about 1 s per diagram):

```bash
mmdc -i diagram.mmd -o diagram.png          # PNG; also .svg or .pdf
mmdc -i diagram.mmd -o diagram.png -s 2     # double resolution, sharper
mmdc -i report.md -o report.out.md          # replaces all mermaid blocks with SVG files
```

For display in the chat use **PNG** (the web UI does not display SVG), for Typst **SVG** or **PDF**.
Add `-c /opt/agw/mmdc/ui-config.json` for the same look as in the web UI (no HTML labels).
Store files under `/workspace`. If the call fails with a syntax error, the line is in
the message; the pitfalls below apply just the same.

One diagram per code block. Keep it small (up to about 20 nodes); better two clear ones than one
unreadable one. A sentence before it says what it shows.

## Pitfalls

- **Labels in quotes** as soon as they contain umlauts, ß, spaces, brackets, colons,
  slashes, `#`, `&` or punctuation: `A["Größe prüfen (m²)"]`, edge text
  `-->|"ja, bestätigt"|`. Avoid double quotes within the text (use `'` instead): `#quot;` shows
  up literally as `&quot;` in the UI.
- **Identifiers** (`A`, `check`, `db1`) only from ASCII letters, digits and `_`; no `end` as
  an identifier (keyword), use `End` or `finish` instead.
- **No HTML** in labels (`<br>`, `<b>` …): the UI renders with `securityLevel: "strict"` and
  without HTML labels; `<b>` shows up as text. For a line break, prefer a shorter label, else a real
  line break inside a quoted label:

  ```
  A["Vorlage wählen
  Provider + Architektur"]
  ```
- **No `click` directives, links or callbacks**: they have no effect in strict mode.
- **No `%%{init: …}%%` directives** for theme, font, HTML labels or security: the UI sets them
  itself and ignores changes to them.
- No styles with addresses (`url(...)`) or images from the network; they are removed.
- On a syntax error the UI shows a note and the source collapsed. Then look for the error (usually
  missing quotes), check it with `mermaid-check` and send the corrected block again.

## Common types

**Process** (`flowchart`, direction `TD` top to bottom, `LR` left to right):

```mermaid
flowchart LR
  A["Message"] --> B{"Internet needed?"}
  B -->|yes| C["Ask the user"]
  B -->|no| D["Run"]
  C --> D
```

**Architecture** (process with groups):

```mermaid
flowchart TB
  subgraph host["Host"]
    O["Orchestrator"]
  end
  subgraph sandbox["Sandbox"]
    P["pi"] --> S[("Socket")]
  end
  S --> O
  O --> M["Language model"]
```

**Sequence:**

```mermaid
sequenceDiagram
  participant U as User
  participant A as Agent
  participant O as Orchestrator
  U->>A: Request
  A->>O: Tool call
  O-->>A: Result
  A-->>U: Answer
```

**States:**

```mermaid
stateDiagram-v2
  [*] --> Active
  Active --> Idle: let idle
  Idle --> Active: resume
  Active --> Closed: close
  Closed --> [*]
```

**Classes:**

```mermaid
classDiagram
  class Chat {
    +String title
    +send(text)
  }
  class Message {
    +String role
  }
  Chat "1" --> "*" Message
```

**Data model (ER):**

```mermaid
erDiagram
  CHAT ||--o{ MESSAGE : contains
  CHAT ||--o{ ARTIFACT : creates
  CHAT {
    string id
    string title
  }
```

In ER diagrams, relationship names without quotes may only be ASCII; with umlauts, put them in
quotes: `CHAT ||--o{ NACHRICHT : "enthält"`.

**Schedule (Gantt):**

```mermaid
gantt
  title Schedule
  dateFormat YYYY-MM-DD
  section Analysis
  Literature     :a1, 2026-10-01, 14d
  section Implementation
  Prototype      :a2, after a1, 21d
  Evaluation     :after a2, 10d
```

**Mind map** (indentation determines the level):

```mermaid
mindmap
  root(("Agent"))
    Tools
      bash
      read
    Limits
      Internet
      Subagents
```

**Pie** (shares, only for a few values; exact numbers better with matplotlib):

```mermaid
pie title Costs by type
  "Answers" : 70
  "Subagents" : 20
  "Compaction" : 10
```
