---
name: documents
description: Read documents the user attached or that lie in /workspace - Word (.docx), Excel (.xlsx, .xls), PowerPoint (.pptx), PDF, Outlook (.msg), EPUB, HTML, CSV - by converting them to text or Markdown, in parts for large files. Use when the user refers to an attachment or a document, asks to summarise, compare, translate or extract from a file, or when a file in /workspace/inputs/ is not plain text.
---

# Reading documents

Attachments are in `/workspace/inputs/` (read only). Office files, PDF and EPUB are binary: the
`read` tool shows garbage or nothing for them. Convert them first; everything works without
internet (the tools are preinstalled).

## Which tool

| File | First choice | If the output is empty or garbled |
|---|---|---|
| `.pdf` | `pdftotext <file> -` (add `-layout` for tables; not for two-column text, it mixes the columns) | `markitdown <file>` |
| `.docx`, `.pptx`, `.xlsx`, `.xls`, `.msg`, `.epub`, `.ipynb` | `markitdown <file>` | for Excel: pandas (below) |
| `.zip` | `unzip -l <file>` first; `markitdown <file>` converts every file inside | `unzip <file> -d /workspace/<dir>` |
| `.html`, `.htm` | `markitdown <file>` (Markdown without markup) | `read` |
| `.csv`, `.tsv`, `.json`, `.md`, `.txt`, `.xml` | `read` (plain text) or pandas | `markitdown <file>` |
| `.doc`, `.ppt`, `.odt`, `.ods`, `.odp`, `.rtf` | not supported; say so and ask for .docx, .pptx, .xlsx or PDF | |

- Check the type first if the extension is missing or doubtful: `file <file>`; for PDF
  `pdfinfo <file>` gives the page count.
- markitdown sometimes returns nothing for a PDF without an error, and pdftotext returns nothing
  for a scanned PDF (no text layer). An empty result is **not** an empty document: try the other
  tool, and if both are empty, tell the user the PDF seems to be a scan without text (there is no
  OCR in the sandbox; `pdftoppm -png -r 100 -f 1 -l 1 <file> /tmp/page` and showing the image is
  the fallback for a single page).
- `.docx` comes out with headings, lists and tables; `.pptx` with one `<!-- Slide number: N -->`
  per slide (speaker notes under `### Notes:`); `.xlsx` with one `## <sheet name>` and one table
  per sheet; `.msg` with sender, recipients, subject and body (attachments of the mail are not
  extracted).
- Images inside documents are not described; only their alt text, if any, appears.

## Large files: never dump everything

Output longer than about 50 KB or 2000 lines is cut off, and every token of it fills your
context. Convert into a file under `/workspace`, look at its size, then read only what you need:

```bash
markitdown /workspace/inputs/report.docx > /workspace/report.md
wc -c -l /workspace/report.md          # size first
grep -n '^#' /workspace/report.md      # outline: headings with line numbers
```

Then read sections with `read` (offset and limit in lines) or `sed -n '120,220p'`, search with
`rg -n 'keyword' /workspace/report.md`, and use `head -c 20000` for a first look.

- **PDF page by page:** `pdfinfo` for the page count, then `pdftotext -f 3 -l 5 <file> -` for pages
  3 to 5. Summarise long PDFs in chunks of a few pages and keep notes in a file.
- **PowerPoint slide by slide:** split the Markdown at the `<!-- Slide number:` lines.
- **Excel sheet by sheet:** list the sheets and their sizes first instead of converting the whole
  workbook (a sheet with thousands of rows becomes a huge Markdown table):

  ```python
  import pandas as pd
  sheets = pd.read_excel("/workspace/inputs/data.xlsx", sheet_name=None)  # dict: name -> DataFrame
  for name, df in sheets.items():
      print(name, df.shape)
  df = sheets["Week31"]
  print(df.head(20).to_string())         # a look at the first rows
  print(df.describe())                   # figures instead of all rows
  ```

## Real table work

For computing, filtering, joining or charting, use **pandas** (with openpyxl for .xlsx, xlrd for
.xls; all preinstalled), not the Markdown table: `pd.read_excel(path, sheet_name=...,
header=...)`, `pd.read_csv(path, sep=None, engine="python")` for an unknown separator. Formulas
come as their last saved values; `openpyxl.load_workbook(path, data_only=False)` shows the
formulas themselves. Merged cells and several tables on one sheet need `header=None` and a look
at the raw rows first.

## Answering

- Say which file you read and, for long files, which part (pages, sheets, slides), so the user
  can check it. Quote figures exactly as they are in the file.
- If a conversion lost something (scanned pages, images, an unsupported format), say so instead
  of guessing the content.
