-- Schema des Orchestrators. Idempotent; wird bei jedem Start ausgeführt.

CREATE TABLE IF NOT EXISTS chats (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title        text NOT NULL,
    model        text NOT NULL,
    variant      text NOT NULL,
    state        text NOT NULL DEFAULT 'active',   -- active | dormant
    internet     boolean NOT NULL DEFAULT false,
    session      bytea,                            -- Sitzungsdatei von pi (JSONL)
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

-- Kosten je Antwort, vom Orchestrator nach Tarif (Spitzen-/Nebenzeit) berechnet.
-- NULL bei älteren Einträgen; dann gilt pis eigener Wert aus usage.cost.total.
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS cost double precision;
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS peak boolean;

-- Kompaktierung und Kontext (Stufe 1, Nachtrag)
ALTER TABLE chats ADD COLUMN IF NOT EXISTS auto_compact boolean NOT NULL DEFAULT true;
ALTER TABLE chats ADD COLUMN IF NOT EXISTS context jsonb;   -- zuletzt bekannte Kontextauslastung
ALTER TABLE chats ADD COLUMN IF NOT EXISTS commands jsonb;  -- zuletzt bekannte Befehle von pi

-- Modellaufrufe, am LLM-Proxy erfasst: maßgeblich für Kosten und Tokens, weil
-- hier auch Subagenten, Kompaktierungen und direkte Aufrufe erscheinen.
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

-- Subagenten: Einträge aus deren Sitzungsdateien in der Sandbox (nicht
-- fälschungssicher; "confirmed" = die Antwort ist am Proxy belegt).
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

-- Name und Zustand je Subagenten-Lauf aus den Statusdateien von pi-subagents (Sandbox, also nicht
-- fälschungssicher): Agent, Name im Workflow (label), Zustand, Kennungen bei pi-subagents.
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

-- Anzeige-Bilder: Bilder, die der Agent in einer Antwort per Markdown zeigt
-- (![..](/workspace/plot.png)). Der Orchestrator liest sie aus der Sandbox und legt
-- sie in S3 ab, damit sie sichtbar bleiben, wenn der Chat ruht. Keine Artefakte:
-- Sie gehen nur an die angemeldete UI, nicht als Ergebnis nach draußen.
-- msg ist die Kennung der Antwort (responseId, sonst ts-<timestamp>).
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

-- Arbeitsbereich je Chat: /workspace (ohne inputs/ und ohne Paket- und Cache-Ordner) als
-- tar.gz in S3 unter <chat>/workspace.tar.gz, damit Dateien das Ruhen überstehen. Beim
-- Fortsetzen spielt der Orchestrator das Archiv in die frische Sandbox ein. object_key NULL:
-- noch nie gesichert (dann steht hier nur, warum eine Sicherung ausgelassen wurde).
-- size: Summe der Dateigrößen (unkomprimiert), archive_size: Größe des Archivs.
-- fingerprint: Prüfsumme über Namen, Größen, Zeiten und Rechte; gleich = nichts zu tun.
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

-- Werkzeugausführungen (E9): Jede Operation, die der Orchestrator für ein Werkzeug von pi in der
-- Ausführungs-Sandbox ausführt (bash, Dateien lesen/schreiben, Suchen). Der Orchestrator trägt sie
-- selbst ein, sie sind deshalb belegt. Ein Werkzeugaufruf (tool_call_id) kann mehrere Operationen
-- haben (edit: access, read, write). session: "main" oder die Kennung des Subagenten-Laufs.
-- args ist gekürzt (bei write Pfad, Größe und SHA-256 statt Inhalt); output_excerpt sind höchstens
-- 4 KiB, output_sha256 gilt für die ganze Ausgabe (bash) bzw. den gelesenen Inhalt.
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

-- M1 (Code-Review E9): Kam die Antwort vollständig an? finish_reason des Anbieters; complete ist
-- falsch, wenn der Strom vor dem Ende abbrach. Werkzeugaufrufe aus einer abgebrochenen Antwort
-- führt pi nicht aus; der Abgleich zeigt sie deshalb nicht als Umgehung. Ältere Zeilen gelten als
-- vollständig.
ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS finish_reason text NOT NULL DEFAULT '';
ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS complete boolean NOT NULL DEFAULT true;

-- Warteschlange je Chat: Nachrichten, die der Nutzer schickt, während pi arbeitet oder der Chat
-- fortgesetzt wird. Der Orchestrator übergibt alle offenen Einträge gemeinsam als nächsten Auftrag,
-- sobald der Lauf endet (agent_settled). delivered_at gesetzt: übergeben (nicht mehr entfernbar).
-- attachments: Namen hochgeladener Eingaben (/workspace/inputs/).
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

-- Warteschlange: Systemeinträge (etwa die Meldung, dass eine Hintergrundaufgabe endete) stehen
-- neben den Nachrichten des Nutzers und gehen mit ihnen an pi.
ALTER TABLE chat_queue ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'user';

-- Hintergrundaufgaben (bash mit run_in_background): Der Orchestrator startet den Befehl in der
-- Ausführungs-Sandbox und verfolgt ihn bis zum Ende. seq ist die Nummer im Chat (bg-<seq>).
-- Zustände: running, exited, failed, timeout, stopped (bg_stop oder UI), lost (Sandbox oder
-- Verbindung weg), suspended (beim Ruhen des Chats beendet), closed (Chat beendet; nur alte Zeilen).
-- notified_at: Meldung an den Agenten erzeugt; woke: sie hat einen neuen Durchgang gestartet
-- (Grenze je Stunde); notice_pending: beim Ruhen beendet, dem Agenten noch nicht gesagt.
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

-- Review 3 (H1, H2): Herkunft und Auslöser. Ein Durchgang (chat_turns) ist ein Auftrag an pi mit
-- trigger user (der Nutzer hat gesendet), queue (beim Laufende übergeben, mindestens eine Nachricht
-- des Nutzers darunter) oder wake (nur Meldungen des Orchestrators, ohne Zutun des Nutzers) und origin
-- user, system oder mixed; sources nennt die Teile in Reihenfolge (Art, Meldung, Aufgaben, Eintrag der
-- Warteschlange, Marke des Zauns). Die Nachrichten des Durchgangs tragen turn_id und trigger, die
-- Nutzernachricht dazu origin und sources. chat_runs bleibt die Lebenszeit einer Sandbox.
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
-- Systemeinträge: Art der Meldung (background, sandbox) und betroffene Aufgaben (bg-3).
ALTER TABLE chat_queue ADD COLUMN IF NOT EXISTS note text NOT NULL DEFAULT '';
ALTER TABLE chat_queue ADD COLUMN IF NOT EXISTS refs jsonb NOT NULL DEFAULT '[]';

-- Herkunft des Titels (store.TitleDefault/Auto/User). Ältere Chats gelten als vom Nutzer benannt.
ALTER TABLE chats ADD COLUMN IF NOT EXISTS title_source text NOT NULL DEFAULT 'user';

-- Modellaufrufe des Orchestrators neben der Arbeit des Agenten (etwa der Chattitel). Getrennt von
-- llm_calls, damit Kosten und Aufrufzahlen je Chat nur die Arbeit des Agenten zeigen.
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

-- Denkstufe von pi je Chat (/effort); leer: pis Vorgabe.
ALTER TABLE chats ADD COLUMN IF NOT EXISTS thinking_level text NOT NULL DEFAULT '';

-- Anfragen der Werkzeuge web_search und web_extract über den Web-Proxy (webproxy), auch
-- abgewiesene (denied). Bei HTTPS (CONNECT) nur Ziel und Bytes, der Inhalt ist verschlüsselt.
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

-- Werkzeugaufruf, der ein Artefakt hochgeladen hat (Anzeige im Verlauf an der richtigen Stelle).
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS tool_call_id text NOT NULL DEFAULT '';

-- Wer über den Socket fragte: Hauptagent („main“) oder Subagenten-Lauf, samt Werkzeugaufruf.
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS session text NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS tool_call_id text NOT NULL DEFAULT '';
ALTER TABLE socket_calls ADD COLUMN IF NOT EXISTS session text NOT NULL DEFAULT '';
ALTER TABLE socket_calls ADD COLUMN IF NOT EXISTS tool_call_id text NOT NULL DEFAULT '';

-- Chats werden nicht mehr beendet (30.09.2026): früher beendete ruhen und lassen sich fortsetzen.
UPDATE chats SET state='dormant' WHERE state='closed';

-- Delegation (plan-delegation-rest-plattform.md, Schritt 1): übertragene Rechte je Chat und das
-- Herkunftsregister der Objekte, die in dieser Delegation entstanden sind. Das Register gilt beim
-- Fortsetzen weiter; Einträge entstehen nur aus Antworten auf Anlage-Aufrufe.
ALTER TABLE chats ADD COLUMN IF NOT EXISTS delegation jsonb;
CREATE TABLE IF NOT EXISTS delegation_objects (
    chat_id    uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    resource   text NOT NULL,
    object_id  text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chat_id, resource, object_id)
);
