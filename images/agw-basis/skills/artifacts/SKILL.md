---
name: artifacts
description: Store files as an artifact of the current chat at the orchestrator, list them and fetch them again. Use when the user wants to keep, download or "upload" a result as a file.
---

# Storing artifacts

This sandbox has the command `agw-artifact`. It stores files permanently at the
orchestrator, separately per chat. The working directory of this sandbox, by contrast, is
volatile: it disappears when the chat is idle.

```bash
agw-artifact upload <file> [--name <name>]
agw-artifact list
agw-artifact get <name> [-o <file>]
```

**Every upload must be approved by the user.** `agw-artifact upload` waits until the
user approves or rejects in the UI, which can take several minutes. So call it
with a generous timeout (at least 900 seconds), not in the background,
and wait for the result.

- Exit code 0 and `approved: …` — the artifact is stored.
- Exit code 3 and `rejected: …` — the user rejected it. Do not try again without
  asking first; tell the user about the rejection.
