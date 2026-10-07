---
name: artifacts
description: Send files to the user with agw-artifact upload - the file appears in the chat at once as a file message from you, without approval - and list or fetch the files of this chat. Use for result files the user wants to keep or download (a CSV, a chart as PNG, a report as PDF), when the user asks you to send, give, export or upload a file; not for intermediate files. Only files inside /workspace.
---

# Sending files to the user

This sandbox has the command `agw-artifact`. It sends a file to the user: the orchestrator
stores it permanently with this chat, and the chat shows it **at once** as a file message from
you (a card with name, size, type and download; images with a preview). **There is no approval
and no waiting**; the command returns right away.

```bash
agw-artifact upload <file> [--name <name>]   # send a file to the user
agw-artifact list                            # files of this chat (yours and the user's)
agw-artifact get <name> [-o <file>]          # fetch a file you sent earlier (user inputs: --kind input)
```

## When to send a file

- Send **finished results** the user wants to keep or download: a table as CSV or Excel, a
  chart as PNG, a report as PDF or Markdown, a script or notebook they asked for.
- Send it when the user asks you to send, give, export, save or upload a file, or when the
  result of the task is a file.
- Do **not** send intermediate files, scratch data, logs or every version of a file. One file
  per result; sending the same name again replaces the earlier file in the chat.
- Showing an image in your reply with `![…](/workspace/x.png)` does not send it, and the backup of
  `/workspace` is not a file for the user either. If the user should keep the image, send it too.
- After sending, mention the file briefly in your reply (its name and what it contains); do not
  paste its whole content again.

## Rules

- Only regular files **inside `/workspace`**; no symbolic links, directories or FIFOs. Copy a
  file from `/tmp` into `/workspace` first. To send several files, send each one (or a ZIP).
- At most the configured size per file (default 50 MB).
- Exit code 0 and `sent: …` — the user has the file. Exit code 1 and `not sent: …` or `error: …`
  — nothing was sent; fix the cause (path, size) before trying again.
