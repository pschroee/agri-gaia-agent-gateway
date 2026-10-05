# Plan: delegation, REST variant, chat in the platform

As of 2026-10-05, order confirmed by the author. The agent builds steps 1 and 2 without interruption; step 3 needs
changes on the server and is only implemented after consultation.

| Step | What | What it is for in the thesis | Status |
|---|---|---|---|
| 1 | **Delegation**: delegated rights per chat, provenance rule, expiry, conformance check without a language model | RQ1, 5.2, 5.3, 7.3.1 | **implemented** (2026-10-05) |
| 2 | **REST variant**: REST endpoint of the authorization service at the socket, variant `api` in pi | RQ2 (third binding), 6.2.2, issue #1 | **implemented** (2026-10-05) |
| 3 | **Chat in the platform**: narrow panel on the right in the Agri-Gaia frontend, token of the logged-in user | title, chapter 6, demonstration | planned, needs consultation |

Why this order: without delegation there is nothing RQ1 could measure against; without the REST variant there is no
three-way comparison. The embedding in the platform carries no research question and fits into the weeks in which
the main runs compute ([`masterarbeit/zeitplan.md`](../masterarbeit/zeitplan.md)).

---

## Step 1: Delegation

### What a delegation is

A delegation belongs to a chat and states **which actions on which objects** the agent may perform for this task.
Everything else is a violation. Basis: `masterarbeit/gliederung.md`, *Wie die Prüfung gebaut ist* (how the check is
built), and 5.2.

```json
{
  "rules": [
    {"action": "read",   "resource": "dataset", "ids": ["*"]},
    {"action": "create", "resource": "dataset"},
    {"action": "update", "resource": "dataset", "ids": ["own"]},
    {"action": "run",    "resource": "training", "ids": ["7"]}
  ],
  "expires_at": "2026-10-06T18:00:00Z",
  "enforce": true,
  "confirm": "writes"
}
```

- **Actions:** `read` (list and single object), `create`, `update`, `delete`, `run` (start training).
- **Resources:** `dataset`, `model`, `training`, `task`, `train_template` (templates and configurations),
  `edge_device`, `container_image`, `api` (all other paths of the REST API).
- **Objects:** `ids` is a list of IDs, `"*"` for all, `"own"` for objects created **in this delegation**
  (provenance rule). Without `ids` the rule only applies to actions without an object (`create`, lists).
- **Expiry:** after `expires_at` nothing applies any more (requirement: no further rights after expiry).
- **`enforce`:** `true` blocks violations; `false` only logs them and lets them through. This is the level "no
  protective measure" of the experiment plan: the same observation point, but no boundary.
- **`confirm`:** `writes` (default) additionally asks the user for writing calls; `none` does not. Confirmation
  remains a complementary measure, not an authorization boundary (5.5).
- **Without a delegation** the chat behaves as before (reading free, writing with confirmation), so the existing
  flows and tests do not break; the log notes "without delegation".

### Classifying every call

The authorization service itself maps **every** platform call to an action, a resource and, where applicable, an
ID, from the method and the normalized path, never from information supplied by the agent. A route table covers the
paths of the curated tools and the frequent paths of the API; whatever it does not know is `api` with the action
from the method (GET `read`, otherwise `update`) and is only allowed with an explicit `api` rule. Known writing GETs
(`/train/containers/{id}/model`, `/licenses` with a query) count as `create` and `update` respectively.

### Provenance rule

- If the platform answers a `create` with 2xx, the service records the new ID: `id` in the body for datasets and
  models, the task from `Location: /tasks/<id>` for training.
- The register lives in Postgres (`delegation_objects`) and remains valid when the chat is resumed.
- Provenance arises **only** from responses to create calls, never from `owner` or information supplied by the agent.
- **Limit:** training containers are created asynchronously from a task; the response does not name their ID.
  `own` therefore does not apply to containers; they can only be delegated by explicit IDs or `*`. This is stated as
  a limitation in the thesis.

### Course of a check (Manager.PlatformCall)

1. Normalize the call (`platform.Normalize`), classify it (`delegation.Classify`).
2. Check against the delegation: expired → refuse; no matching rule → violation.
3. Violation: with `enforce` refuse, without `enforce` let through; log in both cases (`socket_calls`, result
   `violation blocked: …` or `… · violation, logged only: …`, and an event for the UI).
4. Allowed and writing and `confirm: writes` → confirmation by the user as before.
5. Execute; on a successful `create` record the provenance.

### Conformance check without a language model (7.3.1)

Own test suite `internal/conformance`: every forbidden call with hostile arguments directly to the authorization
service, also in variants a parser might understand differently (upper and lower case, trailing slash, ID as `01`
or ` 1`, duplicate JSON keys, raw access to the same path, writing GET); plus an expired delegation and a resumed
chat with a foreign object. Target: 100 % refused, and **no** forbidden call reaches the (mocked) platform. The
suite prints a table that goes into 7.3.1.

### UI and tools

- API: `POST /api/chats` with field `delegation`; `GET /api/chats/{id}` returns it as well.
- CLI: `agw chat new --delegation file.json`, `agw run --delegation file.json`.
- Web UI: the chat's delegation as a card (rules, expiry, mode), violations highlighted in the socket log. An editor
  for delegations is not part of this step.
- The agent learns its rights through the tool `rights` (MCP `platform_rights`, CLI `agw-platform rights`), so it
  does not run blindly into the boundary; the boundary itself does not depend on it. (Deviation from the first
  draft: the system note is created when a slot starts in the warm pool, i.e. before it is known which chat the
  slot belongs to; a tool is therefore the reliable way.)

---

## Step 2: REST variant

Implementation of issue #1 with one change compared with the draft there: the REST endpoint lives **at the slot's
socket**, not on its own TCP port. That way the chat follows from the socket (as with MCP and CLI) instead of from
the source address, and no further network is needed.

- Endpoint `/platform-api/<path of the platform API>` at both sockets: method, path, query and JSON body go
  unchanged into `platform.Request`; the service checks, inserts the token and returns status, `Location` and body
  (redacted). An `Authorization` header from the agent is discarded. Writing calls wait for confirmation as on the
  other paths.
- `GET /platform-api/_agw/paths?prefix=` returns the condensed path directory (like `api_paths`).
- Variant `api` in pi: only one HTTP tool `platform_http` (extension `api.ts`, talks to the socket), plus `todo`,
  `web_search`, `web_extract`; **no** file tools, no `bash`.
- In the variants with `bash` the agent reaches the same endpoint with
  `curl --unix-socket /run/agw/agw.sock http://agw/platform-api/datasets`.
- Log: `via: "api"` for the path through `platform_http`, `cli` for `curl` from the shell.

---

## Step 3: Chat in the platform (after consultation)

- **Panel on the right in the Agri-Gaia frontend** (`~/dev/agri-gaia/platform/services/frontend`): collapsible,
  shows the PoC's chat; first version as an embedded page of the orchestrator, later as its own React component
  following the design in `prototyp/` (tabs Chat, Workbench, Activity).
- **Login:** the frontend passes the Keycloak token of the logged-in user to the orchestrator, which exchanges it
  per chat (instead of the fixed test user). This is delegation as the thesis describes it.
- **Orchestrator on the instance** instead of on the Mac, behind Traefik (subdomain `agent.`).
- **Changes on the server that need consultation:** run the orchestrator and images on the instance's server; build
  and roll out the adapted frontend; Traefik route; possibly a redirect URI at the Keycloak client. Own code in the
  frontend goes as a separate commit into its own copy, not into this repo (convention: upstream code is not copied
  here).
