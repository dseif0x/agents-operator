# Routines

Status: phase 3 of the OpenBot-style rework (see `conversations.md`).

A routine is an instruction an agent or a chat gets on a schedule or when
a webhook arrives, like OpenBot's routines.

## Model

- `routines`: owner, target (`session_id` of a conversation session, or
  `chat_id`), name, instruction, trigger (`schedule` or `webhook`),
  schedule (JSON, `internal/routine.Schedule`), enabled, the webhook's
  secret, event type and filters, `next_run_at`, `last_run_at`.
- `routine_runs`: source (`schedule`, `webhook`, `test`), status
  (`queued` → `running` → `succeeded` / `failed` / `interrupted`; `sent`
  for a chat), the conversation input or chat message it made, the turn
  that worked on it, `quiet`, error, payload, when it was due.

Schedules (`internal/routine`): once, hourly, every n hours, daily,
weekdays, weekly (days), monthly (a day; a shorter month runs on its last
day), yearly, and five-field cron, each in an IANA time zone (the hub
embeds tzdata). The UI defaults to the browser's zone.

## Runs

- The leader fires due routines from its poll loop (`RunDueRoutines`). The
  next run is computed from now, so a hub that was down runs a missed
  routine once; one more than six hours late is recorded as a failed
  "missed" run instead.
- An **agent routine** queues a conversation input with source `routine`
  (its item carries `extra.routine` for the UI's run marker). The text
  starts with `[agents-operator routine] "<name>" (<scheduled run | webhook
  | test run>)`, then the instruction and, for a webhook, the event and
  the body. A stopped agent is started. The run follows the turn that
  takes the input: running at `turn_started`, ended at `turn_ended`.
- A **chat routine** posts the text to the chat as the owner, which
  reaches the members as any message does; stopped members are started.
- Two tools of the agent's MCP server report on a run: `routine_no_update`
  (nothing new: the run is *quiet*, and the UI folds its answer into the
  run marker) and `routine_report_failure` (the run fails with the
  reason). They only work while the agent works on a routine input.
- **Run now** makes a test run, never quiet.

## Webhooks

`POST /api/v1/hooks/{routine}` needs no login. The request is signed with
the routine's secret: `X-Hub-Signature-256` or `X-Signature-256` set to
`sha256=<hex HMAC-SHA256 of the body>` (what GitHub sends), or the secret
as `Authorization: Bearer` or `?token=`. The event type comes from
`X-Event-Type`, `X-GitHub-Event`, `X-Gitlab-Event` or `X-Event-Key`, else
the body's `type`, `event`, `event_type` or `action`. A routine with an
event type, or with filters (`path` = `value` on the JSON body, dotted
paths), ignores requests that do not match (202 with the reason). Unknown
routines and bad signatures get the same 401, and failures count against
the login rate limiter. The body is limited to 1 MiB; the agent gets up to
8 KiB of it. The secret is shown once, when it is made or rotated.

The hub must be reachable from the sender: with a LAN-only hub, expose
only `/api/v1/hooks/` publicly (a tunnel or an Ingress path).

## UI

- An agent's details panel lists its routines (toggle, last run, next
  run) and opens the editor: name, instruction, Schedule or Webhook, the
  schedule picker (kind, days, time, zone, a sentence of what it means),
  the webhook's URL, secret, event type and filters, Run now, Delete and
  the run history.
- The Routines page (rail) shows the week: each day's past runs (✓ / ✗)
  and upcoming ones, the next runs, and every routine by agent or chat,
  with New routine for any agent or chat.
- In the conversation a run is a marker (name, source, time); a quiet run
  keeps its answer folded inside.
