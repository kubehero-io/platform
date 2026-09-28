-- Alerting (internal/alerts): rules, per-series alert state, silences,
-- the notification log, and a one-row-per-key meta table (the default
-- rule set is seeded once, on the first boot that finds no rules).

CREATE TABLE IF NOT EXISTS alert_rules (
  id               UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  name             TEXT        NOT NULL,
  description      TEXT        NOT NULL DEFAULT '',
  kind             TEXT        NOT NULL CHECK (kind IN ('logs','cost','budget','anomaly','network','event')),
  query            TEXT        NOT NULL,
  op               TEXT        NOT NULL CHECK (op IN ('>','>=','<','<=','==','!=')),
  threshold        DOUBLE PRECISION NOT NULL DEFAULT 0,
  pending_for_ms   BIGINT      NOT NULL DEFAULT 0 CHECK (pending_for_ms >= 0),
  severity         TEXT        NOT NULL DEFAULT 'warn' CHECK (severity IN ('info','warn','critical')),
  -- Channel URLs are credentials (webhook paths, routing keys): the API
  -- returns them redacted to non-admins and never logs them.
  channels         JSONB       NOT NULL DEFAULT '[]'::jsonb,
  labels           JSONB       NOT NULL DEFAULT '{}'::jsonb,
  annotations      JSONB       NOT NULL DEFAULT '{}'::jsonb,
  enabled          BOOLEAN     NOT NULL DEFAULT TRUE,
  eval_interval_ms BIGINT      NOT NULL DEFAULT 60000 CHECK (eval_interval_ms > 0),
  created_by       TEXT        NOT NULL DEFAULT '',
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- alertname (= rule name) is what silences match on: keep it unique.
CREATE UNIQUE INDEX IF NOT EXISTS alert_rules_name_idx ON alert_rules (name);
DROP TRIGGER IF EXISTS alert_rules_touch ON alert_rules;
CREATE TRIGGER alert_rules_touch BEFORE UPDATE ON alert_rules
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- One row per (rule, series): pending, firing, or resolved (kept 24h).
CREATE TABLE IF NOT EXISTS alert_states (
  id               TEXT        PRIMARY KEY,           -- stable per (rule, label set)
  rule_id          UUID        NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
  fingerprint      TEXT        NOT NULL,
  labels           JSONB       NOT NULL DEFAULT '{}'::jsonb,
  state            TEXT        NOT NULL CHECK (state IN ('pending','firing','resolved')),
  value            DOUBLE PRECISION NOT NULL DEFAULT 0,
  summary          TEXT        NOT NULL DEFAULT '',
  description      TEXT        NOT NULL DEFAULT '',
  link_path        TEXT        NOT NULL DEFAULT '',
  started_at       TIMESTAMPTZ NOT NULL,
  fired_at         TIMESTAMPTZ,
  resolved_at      TIMESTAMPTZ,
  last_eval_at     TIMESTAMPTZ NOT NULL,
  last_notified_at TIMESTAMPTZ,
  UNIQUE (rule_id, fingerprint)
);
CREATE INDEX IF NOT EXISTS alert_states_state_idx ON alert_states (state, last_eval_at DESC);

CREATE TABLE IF NOT EXISTS alert_silences (
  id         UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  matchers   JSONB       NOT NULL,
  starts_at  TIMESTAMPTZ NOT NULL,
  ends_at    TIMESTAMPTZ NOT NULL,
  created_by TEXT        NOT NULL DEFAULT '',
  comment    TEXT        NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CHECK (ends_at > starts_at)
);
CREATE INDEX IF NOT EXISTS alert_silences_ends_idx ON alert_silences (ends_at DESC);

-- Every delivery attempt, per channel (stored redacted). No FK to the
-- rule: the log outlives rule deletion. Pruned after 30 days.
CREATE TABLE IF NOT EXISTS alert_notifications (
  id       BIGSERIAL   PRIMARY KEY,
  at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  alert_id TEXT        NOT NULL,
  rule_id  UUID,
  event    TEXT        NOT NULL CHECK (event IN ('firing','resolved','renotify','heartbeat')),
  channel  TEXT        NOT NULL,
  ok       BOOLEAN     NOT NULL,
  error    TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS alert_notifications_at_idx ON alert_notifications (at DESC);
CREATE INDEX IF NOT EXISTS alert_notifications_alert_idx ON alert_notifications (alert_id, at DESC);

CREATE TABLE IF NOT EXISTS alert_meta (
  key        TEXT        PRIMARY KEY,
  value      TEXT        NOT NULL DEFAULT '',
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
