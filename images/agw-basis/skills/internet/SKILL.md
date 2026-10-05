---
name: internet
description: Ask the user for internet access. Use before fetching anything from the network (pip install, curl, git clone, web pages), when network calls fail or will foreseeably be needed.
---

# Asking for internet access

This sandbox has **no internet by default**. Network calls then fail with name resolution or
connection errors. The route to the language model and to the orchestrator exists regardless.

If the task needs internet, ask the user for it — with a short, concrete reason:

```bash
agw-internet "pip install scikit-learn to train a classification model"
```

The command **waits** until the user decides (this can take minutes); call it with a generous
timeout (at least 900 seconds) and not in the background.

- Exit code 0, `approved: …` — internet is now available, carry on.
- Exit code 3, `rejected: …` — do not ask again unless the user wants it. Continue without
  network or explain what cannot be done without internet.

Do not ask as a precaution, only when the task really needs it. Preinstalled are, among
others, numpy, pandas, matplotlib, plotly, jinja2 and openpyxl; these need no internet.

## Installing packages

With internet access, `pip install` and `npm install` automatically go through package caches
(`pip-cache`, `npm-cache`); packages loaded once come faster the next time. Without
internet access they fail after a few seconds. Then do not retry, but ask for internet
or work with the preinstalled packages. Packages installed later are lost when the chat
is idle and resumed in a fresh sandbox; install them again then.
