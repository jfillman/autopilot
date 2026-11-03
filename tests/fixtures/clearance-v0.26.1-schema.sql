-- clearance v0.26.1's pg.py SCHEMA, verbatim: the tables the Python gateway wrote on clearance-pg before the
-- cutover. Vendored so the read API's and the migration's tests need no clearance package (R-3d).
CREATE TABLE IF NOT EXISTS clearance_evidence (
  seq        bigint PRIMARY KEY,
  hash       text   NOT NULL UNIQUE,
  prev_hash  text   NOT NULL,
  record     jsonb  NOT NULL,
  assurance  text   NOT NULL DEFAULT 'recorded-not-checkpointed'
             CHECK (assurance IN ('checkpointed', 'recorded-not-checkpointed', 'not-recorded')),
  written_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS clearance_sessions (
  id         text PRIMARY KEY,
  record     jsonb NOT NULL,
  expires_at timestamptz NOT NULL,
  closed     boolean NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS clearance_sessions_live ON clearance_sessions (closed, expires_at);
CREATE INDEX IF NOT EXISTS clearance_evidence_receipt ON clearance_evidence
  ((record->'effect'->>'repo'), (record->'effect'->>'pr')) WHERE record ? 'effect';
CREATE TABLE IF NOT EXISTS clearance_artifacts (
  task_id    text        NOT NULL,
  name       text        NOT NULL,
  content    text        NOT NULL,
  bytes      integer     NOT NULL,
  expires_at timestamptz NOT NULL,
  written_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, name)
);
CREATE INDEX IF NOT EXISTS clearance_artifacts_expiry ON clearance_artifacts (expires_at);
CREATE TABLE IF NOT EXISTS clearance_human_requests (
  id         text        PRIMARY KEY,
  session    text        NOT NULL,
  status     text        NOT NULL,
  expires_at timestamptz NOT NULL,
  record     jsonb       NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS clearance_human_requests_pending ON clearance_human_requests (status, expires_at);
CREATE TABLE IF NOT EXISTS clearance_events (
  id          text        PRIMARY KEY,
  received_at timestamptz NOT NULL,
  source      text        NOT NULL,
  app         text,
  routing_key text,
  record      jsonb       NOT NULL,
  written_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS clearance_events_received ON clearance_events (received_at DESC);
CREATE INDEX IF NOT EXISTS clearance_events_app ON clearance_events (app, received_at DESC);
CREATE TABLE IF NOT EXISTS clearance_holds (
  agent      text        PRIMARY KEY,
  until      timestamptz NOT NULL,
  record     jsonb       NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS clearance_changes (
  id         text        PRIMARY KEY,
  app        text        NOT NULL,
  env        text        NOT NULL,
  state      text        NOT NULL,
  repo       text        NOT NULL,
  pr         integer     NOT NULL,
  head       text        NOT NULL,
  expires_at timestamptz NOT NULL,
  record     jsonb       NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS clearance_changes_pr ON clearance_changes (repo, pr);
CREATE TABLE IF NOT EXISTS clearance_results (
  session    text        PRIMARY KEY,
  task_id    text        NOT NULL,
  record     jsonb       NOT NULL,
  expires_at timestamptz NOT NULL,
  written_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS clearance_results_expiry ON clearance_results (expires_at);
CREATE INDEX IF NOT EXISTS clearance_changes_state ON clearance_changes (state, expires_at);
CREATE INDEX IF NOT EXISTS clearance_changes_app ON clearance_changes (app, env, updated_at DESC);
CREATE TABLE IF NOT EXISTS clearance_definitions (
  name       text        NOT NULL,
  state      text        NOT NULL,
  record     jsonb       NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (name, state)
);
