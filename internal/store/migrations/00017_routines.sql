-- +goose Up
-- Routines: instructions an agent or a chat gets on a schedule or when a
-- webhook arrives (docs/design/routines.md).
CREATE TABLE routines (
    id            uuid PRIMARY KEY,
    owner_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id    uuid REFERENCES sessions(id) ON DELETE CASCADE,
    chat_id       uuid REFERENCES chats(id) ON DELETE CASCADE,
    name          text NOT NULL,
    instruction   text NOT NULL,
    trigger       text NOT NULL,              -- schedule | webhook
    schedule      jsonb NOT NULL DEFAULT '{}',
    enabled       boolean NOT NULL DEFAULT true,
    -- Webhooks: the signing secret (HMAC needs it as is), the event type a
    -- request must carry ('' = any) and field filters on its JSON body.
    webhook_secret text NOT NULL DEFAULT '',
    event_type    text NOT NULL DEFAULT '',
    filters       jsonb NOT NULL DEFAULT '[]',
    next_run_at   timestamptz,
    last_run_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK ((session_id IS NULL) <> (chat_id IS NULL))
);
CREATE INDEX routines_owner ON routines (owner_id);
CREATE INDEX routines_due ON routines (next_run_at) WHERE enabled AND trigger = 'schedule';

CREATE TABLE routine_runs (
    id            bigserial PRIMARY KEY,
    routine_id    uuid NOT NULL REFERENCES routines(id) ON DELETE CASCADE,
    source        text NOT NULL,              -- schedule | webhook | test
    status        text NOT NULL,              -- queued | running | succeeded | failed | interrupted | sent
    input_id      text NOT NULL DEFAULT '',   -- the conversation input of an agent routine
    turn          text NOT NULL DEFAULT '',
    chat_message  bigint,                     -- the message of a chat routine
    quiet         boolean NOT NULL DEFAULT false,
    error         text NOT NULL DEFAULT '',
    payload       text NOT NULL DEFAULT '',
    scheduled_for timestamptz,
    started_at    timestamptz NOT NULL DEFAULT now(),
    ended_at      timestamptz
);
CREATE INDEX routine_runs_routine ON routine_runs (routine_id, id DESC);
CREATE INDEX routine_runs_input ON routine_runs (input_id) WHERE input_id <> '';
CREATE INDEX routine_runs_turn ON routine_runs (turn) WHERE turn <> '';

-- +goose Down
DROP TABLE routine_runs;
DROP TABLE routines;
