
CREATE TABLE IF NOT EXISTS gateway_sessions (
  id         text PRIMARY KEY,
  record     jsonb NOT NULL,
  expires_at timestamptz NOT NULL,
  closed     boolean NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS gateway_sessions_live ON gateway_sessions (closed, expires_at);
CREATE TABLE IF NOT EXISTS gateway_artifacts (
  task_id    text        NOT NULL,
  name       text        NOT NULL,
  content    text        NOT NULL,
  bytes      integer     NOT NULL,
  expires_at timestamptz NOT NULL,
  written_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, name)
);
CREATE INDEX IF NOT EXISTS gateway_artifacts_expiry ON gateway_artifacts (expires_at);
CREATE TABLE IF NOT EXISTS gateway_human_requests (
  id         text        PRIMARY KEY,
  session    text        NOT NULL,
  status     text        NOT NULL,
  expires_at timestamptz NOT NULL,
  record     jsonb       NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS gateway_human_requests_pending ON gateway_human_requests (status, expires_at);
CREATE TABLE IF NOT EXISTS gateway_events (
  id          text        PRIMARY KEY,
  received_at timestamptz NOT NULL,
  source      text        NOT NULL,
  app         text,
  routing_key text,
  record      jsonb       NOT NULL,
  written_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS gateway_events_received ON gateway_events (received_at DESC);
CREATE INDEX IF NOT EXISTS gateway_events_app ON gateway_events (app, received_at DESC);
CREATE TABLE IF NOT EXISTS gateway_holds (
  agent      text        PRIMARY KEY,
  until      timestamptz NOT NULL,
  record     jsonb       NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS gateway_results (
  session    text        PRIMARY KEY,
  task_id    text        NOT NULL,
  record     jsonb       NOT NULL,
  expires_at timestamptz NOT NULL,
  written_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS gateway_results_expiry ON gateway_results (expires_at);
CREATE TABLE IF NOT EXISTS gateway_definitions (
  name       text        NOT NULL,
  state      text        NOT NULL,
  record     jsonb       NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (name, state)
);
CREATE TABLE IF NOT EXISTS gateway_outbox (
  id          bigserial   PRIMARY KEY,
  op          text        NOT NULL CHECK (op IN ('record', 'outcome')),
  kind        text,
  decision_id text,
  body        jsonb       NOT NULL,
  reason      text        NOT NULL CHECK (reason IN ('outage', 'core-unavailable', 'held', 'refused')),
  state       text        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'done', 'refused')),
  error       text,
  seq         bigint,
  created_at  timestamptz NOT NULL DEFAULT now(),
  posted_at   timestamptz
);
CREATE INDEX IF NOT EXISTS gateway_outbox_pending ON gateway_outbox (id) WHERE state = 'pending';
-- clearance v0.28.0: the core writes definition.* records itself when the gateway publishes its set (PUT
-- /v1/definitions), and POST /v1/records refuses them. A definition.* row an older gateway held can never be posted,
-- and the set it describes is published whole, so it goes.
DELETE FROM gateway_outbox WHERE state = 'pending' AND reason = 'held' AND kind LIKE 'definition.%';
-- A-5 trigger durability (engine/trigger_ledger.py): one row per (agent, trigger key), claimed before a triggered
-- session is opened, so a trigger launches once across restarts, replays and the reconciliation sweep; and the event
-- bridge's cursor per source (the newest event id handled, and the floor the sweep never reaches back past).
CREATE TABLE IF NOT EXISTS gateway_trigger_launches (
  agent      text        NOT NULL,
  key        text        NOT NULL,
  via        text        NOT NULL CHECK (via IN ('stream', 'recent', 'reconcile', 'schedule', 'rabbitmq')),
  event_id   text,
  state      text        NOT NULL CHECK (state IN ('claimed', 'launched', 'deduped', 'braked', 'held', 'refused',
                                                    'launch-failed', 'missed')),
  session    text,
  run        text,
  claimed_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (agent, key)
);
CREATE INDEX IF NOT EXISTS gateway_trigger_launches_claimed ON gateway_trigger_launches (claimed_at);
CREATE TABLE IF NOT EXISTS gateway_event_cursors (
  source     text        PRIMARY KEY,
  last_id    text,
  floor_at   timestamptz NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
