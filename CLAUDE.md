<!--
SPDX-FileCopyrightText: 2026 Philipp Schröer

SPDX-License-Identifier: MIT
-->

# Agri-Gaia Agent Gateway — working notes for agents

This repository is the agent gateway of the master's thesis (see `README.md`). It is used as submodule
`services/agent-gateway` of [pschroee/agri-gaia-platform](https://github.com/pschroee/agri-gaia-platform)
(branch `ki-agents`); branch model and platform conventions are in that repository's `CLAUDE.md`.

## Origin

Imported on 2026-10-05 from `poc/` of the thesis repository (private, HS GitLab) at commit `a2962bc`. The
history before that point lives there. The design notes came along under `docs/` and
have since been translated to English (`docs/design.md` was `poc/README.md`); paths like `poc/…` in those notes
refer to this repository.

## Conventions

- **Branches:** `main` is the integration branch; feature branches (`feat/…`, `fix/…`) come back by pull
  request (`gh pr create --base main`). The platform's submodule pointer on `ki-agents` is moved after a merge.
- **Commit messages in English**, short, one line, like the platform (`added …`, `fixed …`).
- **Everything in English:** code, comments, log and error messages, UI texts, skills, prompts and docs. Only
  test inputs that deliberately check German user input stay German. The agent still replies in the user's
  language.
- **Licensing:** MIT. `REUSE.toml` annotates all files; third-party copies keep their own `LICENSE`.
- **Own and foreign code apart:** anything taken from elsewhere lands unchanged in its own commit under
  `third_party/`, own changes in a separate commit and listed in that directory's `VENDORED.md`. Never put
  copies under `vendor/` (switches Go to vendor mode).
- **No secrets, no instance data:** keys only in `.env` (ignored) or, in the platform, in
  `secrets/agent-gateway.env`. Before every commit: `git diff --cached | grep -c 'sk-'` must print 0. No host
  names of private instances and no test credentials in code, docs or tests.

## Login

- The `return` target of `/oidc/login` must be an absolute path on the same host (`internal/oidc`, `safeReturn`):
  the UI under the base and, under a path prefix, any page of the platform frontend on that host (`/ai-agent`).
  Schemes, hosts, `//`, backslashes, dot segments and percent-encoded slashes, backslashes, dots or control
  characters fall back to the UI's start page. Rules and examples in `API.md`, *Return after login*; new cases go
  into `internal/oidc/return_test.go`.

## Testing

- `./dev.sh test` (about 25 s) before every commit, `./dev.sh test --full` (5–6 min) before every push.
- Do not save Go files while `./dev.sh test --full` or `./dev.sh e2e` runs: each orchestrator restart removes all
  containers labelled `agwpoc.managed=true`.
- Do not run `npx prettier --write` on `web/`: there is no Prettier config; the code is hand-formatted at
  120 columns without semicolons. Check with `tsc -b` and `npm run lint`.
- Networks of the stack and of tests are fixed in `10.231.0.0/16`; create test networks only through
  `sandbox.CreateTestNetwork`.

Further pitfalls (Docker Desktop on macOS, pi, pi-subagents, token exchange, Agri-Gaia backend quirks) are
collected in `docs/design.md`, section *Pitfalls found while building*.
