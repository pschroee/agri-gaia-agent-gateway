-- Schema of the orchestrator. Idempotent; executed at every start.

CREATE TABLE IF NOT EXISTS chats (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title        text NOT NULL,
    model        text NOT NULL,
    variant      text NOT NULL,
    state        text NOT NULL DEFAULT 'active',   -- active | dormant
    internet     boolean NOT NULL DEFAULT false,
    session      bytea,                            -- pi's session file (JSONL)
    session_sha  text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS chat_messages (
    chat_id    uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    seq        integer NOT NULL,
    role       text NOT NULL,
    message    jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chat_id, seq)
);

CREATE TABLE IF NOT EXISTS chat_runs (
    id           bigserial PRIMARY KEY,
    chat_id      uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    slot_id      text NOT NULL,
    container_id text NOT NULL,
    started_at   timestamptz NOT NULL DEFAULT now(),
    ended_at     timestamptz
);

CREATE TABLE IF NOT EXISTS artifacts (
    chat_id      uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    kind         text NOT NULL,                    -- input | output
    name         text NOT NULL,
    size         bigint NOT NULL,
    sha256       text NOT NULL,
    content_type text NOT NULL DEFAULT 'application/octet-stream',
    via          text NOT NULL,                    -- cli | mcp | ui
    object_key   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chat_id, kind, name)
);

CREATE TABLE IF NOT EXISTS approvals (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id      uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    kind         text NOT NULL,
    via          text NOT NULL,
    name         text NOT NULL,
    size         bigint NOT NULL,
    sha256       text NOT NULL,
    content_type text NOT NULL,
    pending_key  text NOT NULL,
    preview      text,
    state        text NOT NULL DEFAULT 'pending',  -- pending | approved | rejected | expired
    created_at   timestamptz NOT NULL DEFAULT now(),
    decided_at   timestamptz
);
CREATE INDEX IF NOT EXISTS approvals_pending ON approvals (state) WHERE state = 'pending';

CREATE TABLE IF NOT EXISTS socket_calls (
    id         bigserial PRIMARY KEY,
    chat_id    uuid REFERENCES chats(id) ON DELETE CASCADE,
    slot_id    text NOT NULL,
    via        text NOT NULL,
    op         text NOT NULL,
    detail     text NOT NULL DEFAULT '',
    result     text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS socket_calls_chat ON socket_calls (chat_id, id);

-- Cost per reply, computed by the orchestrator by tariff (peak/off-peak).
-- NULL for older entries; then pi's own value from usage.cost.total applies.
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS cost double precision;
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS peak boolean;

-- Compaction and context (stage 1, addendum)
ALTER TABLE chats ADD COLUMN IF NOT EXISTS auto_compact boolean NOT NULL DEFAULT true;
ALTER TABLE chats ADD COLUMN IF NOT EXISTS context jsonb;   -- last known context usage
ALTER TABLE chats ADD COLUMN IF NOT EXISTS commands jsonb;  -- last known commands of pi

-- Model calls, captured at the LLM proxy: authoritative for cost and tokens, because
-- subagents, compactions and direct calls also show up here.
CREATE TABLE IF NOT EXISTS llm_calls (
    id          bigserial PRIMARY KEY,
    chat_id     uuid REFERENCES chats(id) ON DELETE CASCADE,
    slot_id     text NOT NULL,
    source_ip   text NOT NULL,
    model       text NOT NULL,
    response_id text NOT NULL DEFAULT '',
    status      integer NOT NULL,
    input       bigint NOT NULL DEFAULT 0,
    output      bigint NOT NULL DEFAULT 0,
    cache_read  bigint NOT NULL DEFAULT 0,
    cache_write bigint NOT NULL DEFAULT 0,
    cost        double precision NOT NULL DEFAULT 0,
    peak        boolean NOT NULL DEFAULT false,
    tool_calls  jsonb NOT NULL DEFAULT '[]',
    started_at  timestamptz NOT NULL,
    duration_ms bigint NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS llm_calls_chat ON llm_calls (chat_id, id);
CREATE INDEX IF NOT EXISTS llm_calls_resp ON llm_calls (response_id);

-- Subagents: entries from their session files in the sandbox (not
-- tamper-proof; "confirmed" = the reply is recorded at the proxy).
CREATE TABLE IF NOT EXISTS subagent_entries (
    chat_id    uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    run_id     text NOT NULL,
    entry_id   text NOT NULL,
    agent      text NOT NULL DEFAULT '',
    kind       text NOT NULL,          -- task | tool_call | tool_result | text
    payload    jsonb NOT NULL,
    response_id text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chat_id, run_id, entry_id)
);

ALTER TABLE chats ADD COLUMN IF NOT EXISTS max_subagents integer NOT NULL DEFAULT 3;

-- Name and state per subagent run from the status files of pi-subagents (sandbox, hence not
-- tamper-proof): agent, name in the workflow (label), state, IDs at pi-subagents.
CREATE TABLE IF NOT EXISTS subagent_runs (
    chat_id       uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    run_id        text NOT NULL,
    agent         text NOT NULL DEFAULT '',
    label         text NOT NULL DEFAULT '',
    state         text NOT NULL DEFAULT '',
    pi_run_id     text NOT NULL DEFAULT '',
    parent_run_id text NOT NULL DEFAULT '',
    started_at    timestamptz,
    ended_at      timestamptz,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chat_id, run_id)
);

-- Display images: images the agent shows in a reply via Markdown
-- (![..](/workspace/plot.png)). The orchestrator reads them from the sandbox and stores
-- them in S3 so that they stay visible while the chat is idle. Not artifacts:
-- they only go to the logged-in UI, not to the outside as a result.
-- msg is the ID of the reply (responseId, otherwise ts-<timestamp>).
CREATE TABLE IF NOT EXISTS chat_images (
    chat_id      uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    msg          text NOT NULL,
    path         text NOT NULL,
    object_key   text NOT NULL,
    content_type text NOT NULL,
    size         bigint NOT NULL,
    sha256       text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chat_id, msg, path)
);

-- Workspace per chat: /workspace (without inputs/ and without package and cache folders) as
-- tar.gz in S3 under <chat>/workspace.tar.gz, so that files survive idling. On
-- resume the orchestrator restores the archive into the fresh sandbox. object_key NULL:
-- never backed up (then this row only says why a backup was skipped).
-- size: sum of the file sizes (uncompressed), archive_size: size of the archive.
-- fingerprint: checksum over names, sizes, times and permissions; equal = nothing to do.
CREATE TABLE IF NOT EXISTS chat_workspaces (
    chat_id        uuid PRIMARY KEY REFERENCES chats(id) ON DELETE CASCADE,
    object_key     text,
    size           bigint NOT NULL DEFAULT 0,
    archive_size   bigint NOT NULL DEFAULT 0,
    files          integer NOT NULL DEFAULT 0,
    sha256         text NOT NULL DEFAULT '',
    fingerprint    text NOT NULL DEFAULT '',
    saved_at       timestamptz,
    skipped_reason text,
    skipped_size   bigint,
    skipped_at     timestamptz
);

-- Tool executions (E9): every operation the orchestrator executes for a pi tool in the
-- execution sandbox (bash, reading/writing files, searching). The orchestrator records them
-- itself, so they are proven. A tool call (tool_call_id) can have several operations
-- (edit: access, read, write). session: "main" or the ID of the subagent run.
-- args is shortened (for write: path, size and SHA-256 instead of the content); output_excerpt is at most
-- 4 KiB, output_sha256 covers the full output (bash) or the content read.
CREATE TABLE IF NOT EXISTS tool_executions (
    id             bigserial PRIMARY KEY,
    chat_id        uuid REFERENCES chats(id) ON DELETE CASCADE,
    slot_id        text NOT NULL,
    session        text NOT NULL DEFAULT 'main',
    tool_call_id   text NOT NULL,
    tool           text NOT NULL,
    op             text NOT NULL,
    args           jsonb NOT NULL DEFAULT '{}',
    exit_code      integer,
    error          text NOT NULL DEFAULT '',
    output_excerpt text NOT NULL DEFAULT '',
    output_sha256  text NOT NULL DEFAULT '',
    output_bytes   bigint NOT NULL DEFAULT 0,
    started_at     timestamptz NOT NULL,
    duration_ms    bigint NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS tool_executions_chat ON tool_executions (chat_id, id);
CREATE INDEX IF NOT EXISTS tool_executions_call ON tool_executions (chat_id, tool_call_id);

-- M1 (code review E9): did the reply arrive completely? finish_reason of the provider; complete is
-- false if the stream broke off before the end. pi does not execute tool calls from an aborted reply;
-- the reconciliation therefore does not show them as a bypass. Older rows count as
-- complete.
ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS finish_reason text NOT NULL DEFAULT '';
ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS complete boolean NOT NULL DEFAULT true;

-- Queue per chat: messages the user sends while pi is working or the chat is being
-- resumed. The orchestrator delivers all open entries together as the next request
-- as soon as the run ends (agent_settled). delivered_at set: delivered (no longer removable).
-- attachments: names of uploaded inputs (/workspace/inputs/).
CREATE TABLE IF NOT EXISTS chat_queue (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq          bigserial,
    chat_id      uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    text         text NOT NULL,
    attachments  jsonb NOT NULL DEFAULT '[]',
    created_at   timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz
);
CREATE INDEX IF NOT EXISTS chat_queue_chat ON chat_queue (chat_id, seq);

-- Queue: system entries (such as the note that a background task ended) sit
-- next to the user's messages and go to pi together with them.
ALTER TABLE chat_queue ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'user';

-- Background tasks (bash with run_in_background): the orchestrator starts the command in the
-- execution sandbox and follows it until it ends. seq is the number within the chat (bg-<seq>).
-- States: running, exited, failed, timeout, stopped (bg_stop or UI), lost (sandbox or
-- connection gone), suspended (ended when the chat went idle), closed (chat closed; old rows only).
-- notified_at: note to the agent created; woke: it started a new turn
-- (limit per hour); notice_pending: ended while idling, not yet told to the agent.
CREATE TABLE IF NOT EXISTS background_tasks (
    id             bigserial PRIMARY KEY,
    chat_id        uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    seq            integer NOT NULL,
    slot_id        text NOT NULL,
    session        text NOT NULL DEFAULT 'main',
    tool_call_id   text NOT NULL,
    command        text NOT NULL,
    cwd            text NOT NULL DEFAULT '',
    log_path       text NOT NULL DEFAULT '',
    state          text NOT NULL DEFAULT 'running',
    exit_code      integer,
    error          text NOT NULL DEFAULT '',
    stopped_by     text NOT NULL DEFAULT '',
    started_at     timestamptz NOT NULL DEFAULT now(),
    ended_at       timestamptz,
    output_bytes   bigint NOT NULL DEFAULT 0,
    output_lines   bigint NOT NULL DEFAULT 0,
    output_excerpt text NOT NULL DEFAULT '',
    output_sha256  text NOT NULL DEFAULT '',
    tail           text NOT NULL DEFAULT '',
    notified_at    timestamptz,
    woke           boolean NOT NULL DEFAULT false,
    notice_pending boolean NOT NULL DEFAULT false,
    UNIQUE (chat_id, seq)
);
CREATE INDEX IF NOT EXISTS background_tasks_running ON background_tasks (chat_id) WHERE state = 'running';

-- Review 3 (H1, H2): origin and trigger. A turn (chat_turns) is a request to pi with
-- trigger user (the user sent something), queue (delivered at the end of a run, with at least one
-- user message among it) or wake (only orchestrator notes, without the user's involvement) and origin
-- user, system or mixed; sources lists the parts in order (kind, note, tasks, queue
-- entry, fence marker). The messages of the turn carry turn_id and trigger, the
-- user message additionally origin and sources. chat_runs remains the lifetime of a sandbox.
CREATE TABLE IF NOT EXISTS chat_turns (
    id         bigserial PRIMARY KEY,
    chat_id    uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    trigger    text NOT NULL,
    origin     text NOT NULL,
    sources    jsonb NOT NULL DEFAULT '[]',
    queue_ids  jsonb NOT NULL DEFAULT '[]',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS chat_turns_chat ON chat_turns (chat_id, id);
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS turn_id bigint;
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS trigger text;
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS origin text;
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS sources jsonb;
-- System entries: kind of note (background, sandbox) and affected tasks (bg-3).
ALTER TABLE chat_queue ADD COLUMN IF NOT EXISTS note text NOT NULL DEFAULT '';
ALTER TABLE chat_queue ADD COLUMN IF NOT EXISTS refs jsonb NOT NULL DEFAULT '[]';

-- Origin of the title (store.TitleDefault/Auto/User). Older chats count as named by the user.
ALTER TABLE chats ADD COLUMN IF NOT EXISTS title_source text NOT NULL DEFAULT 'user';

-- Model calls of the orchestrator besides the agent's work (such as the chat title). Separate from
-- llm_calls so that cost and call counts per chat show only the agent's work.
CREATE TABLE IF NOT EXISTS aux_llm_calls (
    id          bigserial PRIMARY KEY,
    chat_id     uuid REFERENCES chats(id) ON DELETE CASCADE,
    purpose     text NOT NULL,
    model       text NOT NULL,
    status      integer NOT NULL DEFAULT 0,
    input       bigint NOT NULL DEFAULT 0,
    output      bigint NOT NULL DEFAULT 0,
    cache_read  bigint NOT NULL DEFAULT 0,
    cost        double precision NOT NULL DEFAULT 0,
    peak        boolean NOT NULL DEFAULT false,
    error       text NOT NULL DEFAULT '',
    started_at  timestamptz NOT NULL,
    duration_ms bigint NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS aux_llm_calls_chat ON aux_llm_calls (chat_id, id);

-- pi's thinking level per chat (/effort); empty: pi's default.
ALTER TABLE chats ADD COLUMN IF NOT EXISTS thinking_level text NOT NULL DEFAULT '';

-- Requests of the tools web_search and web_extract through the web proxy (webproxy), including
-- refused ones (denied). For HTTPS (CONNECT) only target and bytes, the content is encrypted.
CREATE TABLE IF NOT EXISTS web_requests (
    id          bigserial PRIMARY KEY,
    chat_id     uuid REFERENCES chats(id) ON DELETE CASCADE,
    slot_id     text NOT NULL DEFAULT '',
    source_ip   text NOT NULL DEFAULT '',
    method      text NOT NULL,
    host        text NOT NULL,
    port        integer NOT NULL DEFAULT 0,
    path        text NOT NULL DEFAULT '',
    status      integer NOT NULL DEFAULT 0,
    bytes_up    bigint NOT NULL DEFAULT 0,
    bytes_down  bigint NOT NULL DEFAULT 0,
    denied      text NOT NULL DEFAULT '',
    started_at  timestamptz NOT NULL,
    duration_ms bigint NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS web_requests_chat ON web_requests (chat_id, id);

-- Tool call that uploaded an artifact (shown at the right place in the history).
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS tool_call_id text NOT NULL DEFAULT '';

-- Who asked via the socket: main agent ("main") or subagent run, along with the tool call.
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS session text NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS tool_call_id text NOT NULL DEFAULT '';
ALTER TABLE socket_calls ADD COLUMN IF NOT EXISTS session text NOT NULL DEFAULT '';
ALTER TABLE socket_calls ADD COLUMN IF NOT EXISTS tool_call_id text NOT NULL DEFAULT '';

-- Chats are no longer closed (2026-09-30): previously closed ones are idle and can be resumed.
UPDATE chats SET state='dormant' WHERE state='closed';

-- Delegation (docs/plan-delegation-rest-platform.md, step 1): delegated rights per chat and the
-- provenance register of the objects created within this delegation. The register stays valid on
-- resume; entries only arise from responses to create calls.
ALTER TABLE chats ADD COLUMN IF NOT EXISTS delegation jsonb;
CREATE TABLE IF NOT EXISTS delegation_objects (
    chat_id    uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    resource   text NOT NULL,
    object_id  text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chat_id, resource, object_id)
);

-- Owner of the chat (the user's sub when logged in through the platform, AGW_AUTH_MODE=oidc).
-- NULL: created with the API token (token mode), without an owner.
ALTER TABLE chats ADD COLUMN IF NOT EXISTS owner text;
CREATE INDEX IF NOT EXISTS chats_owner ON chats (owner);

-- The user's preferred language according to the browser (BCP 47, e.g. en-US); NULL: unknown. Goes to the
-- agent with the chat's first request as an orchestrator note (internal/chat, languageNote).
ALTER TABLE chats ADD COLUMN IF NOT EXISTS language text;

-- The variant id "beide" was renamed to "both" (2026-10-05); chats created before keep resuming.
UPDATE chats SET variant = 'both' WHERE variant = 'beide';
