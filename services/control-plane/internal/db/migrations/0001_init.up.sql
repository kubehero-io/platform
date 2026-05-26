-- KubeHero control-plane metadata store.
-- Applied by golang-migrate on control-plane startup.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ─── Tenancy ──────────────────────────────────────────────────────────────
CREATE TABLE orgs (
  id          UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  slug        TEXT        UNIQUE NOT NULL,
  name        TEXT        NOT NULL,
  plan        TEXT        NOT NULL DEFAULT 'cloud-free',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE users (
  id          UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  email       TEXT        UNIQUE NOT NULL,
  oidc_sub    TEXT        UNIQUE,
  name        TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_seen   TIMESTAMPTZ
);

CREATE TABLE org_members (
  org_id   UUID NOT NULL REFERENCES orgs(id)  ON DELETE CASCADE,
  user_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role     TEXT NOT NULL DEFAULT 'viewer', -- admin · operator · viewer
  added_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (org_id, user_id)
);

-- ─── Clusters ─────────────────────────────────────────────────────────────
CREATE TABLE clusters (
  id             UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  org_id         UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  slug           TEXT        NOT NULL, -- unique per org
  name           TEXT        NOT NULL,
  cloud          TEXT        NOT NULL CHECK (cloud IN ('aws','gcp','azure','onprem')),
  region         TEXT        NOT NULL,
  cert_fingerprint TEXT,
  nodes_count    INT         DEFAULT 0,
  last_seen      TIMESTAMPTZ,
  state          TEXT        NOT NULL DEFAULT 'healthy' CHECK (state IN ('healthy','warn','critical','offline')),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (org_id, slug)
);
CREATE INDEX clusters_org_idx ON clusters(org_id);

-- ─── Policies ─────────────────────────────────────────────────────────────
-- Mirror of the CRD state observed from each cluster's operator.
CREATE TABLE policies (
  id           UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  cluster_id   UUID        NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
  kind         TEXT        NOT NULL CHECK (kind IN ('BudgetPolicy','CeilingPolicy','RightsizingPolicy')),
  namespace    TEXT        NOT NULL,
  name         TEXT        NOT NULL,
  spec         JSONB       NOT NULL,
  armed        BOOLEAN     NOT NULL DEFAULT FALSE,
  armed_by     UUID        REFERENCES users(id),
  armed_at     TIMESTAMPTZ,
  generation   BIGINT      NOT NULL DEFAULT 0,
  last_eval    TIMESTAMPTZ,
  last_eval_result TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (cluster_id, kind, namespace, name)
);
CREATE INDEX policies_cluster_idx ON policies(cluster_id);
CREATE INDEX policies_kind_idx    ON policies(kind);

-- ─── Audit log — append-only ──────────────────────────────────────────────
CREATE TABLE audit_log (
  id           BIGSERIAL   PRIMARY KEY,
  at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  org_id       UUID        REFERENCES orgs(id)     ON DELETE SET NULL,
  cluster_id   UUID        REFERENCES clusters(id) ON DELETE SET NULL,
  actor_sub    TEXT,      -- OIDC subject or 'system' / 'operator' / 'agent'
  actor_email  TEXT,
  action       TEXT        NOT NULL, -- e.g. 'policy.arm', 'rightsize.apply'
  target_kind  TEXT,
  target_name  TEXT,
  payload      JSONB,
  previous_spec JSONB,    -- for 'undo' replay
  outcome      TEXT        NOT NULL, -- success · error · skipped
  signature    TEXT,       -- HMAC for downstream SIEM verification
  request_id   TEXT
);
CREATE INDEX audit_org_at_idx     ON audit_log(org_id, at DESC);
CREATE INDEX audit_cluster_at_idx ON audit_log(cluster_id, at DESC);
CREATE INDEX audit_action_idx     ON audit_log(action);

-- ─── Sessions (Dashboard / CLI tokens) ────────────────────────────────────
CREATE TABLE sessions (
  token_hash   TEXT        PRIMARY KEY, -- sha256 of the raw token
  user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  org_id       UUID        NOT NULL REFERENCES orgs(id)  ON DELETE CASCADE,
  label        TEXT,
  scope        TEXT        NOT NULL DEFAULT 'read',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  expires_at   TIMESTAMPTZ NOT NULL,
  last_used_at TIMESTAMPTZ,
  revoked_at   TIMESTAMPTZ
);

-- ─── Commitments (for Savings Plan replay) ────────────────────────────────
CREATE TABLE commitments (
  id           UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  org_id       UUID        NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  cloud        TEXT        NOT NULL,
  kind         TEXT        NOT NULL, -- savings_plan · reserved · committed_use
  effective_at TIMESTAMPTZ NOT NULL,
  expires_at   TIMESTAMPTZ NOT NULL,
  commit_usd   NUMERIC(18,4) NOT NULL,
  terms        JSONB       NOT NULL,
  replay_state TEXT        NOT NULL DEFAULT 'pending' CHECK (replay_state IN ('pending','in_progress','done','failed')),
  replay_at    TIMESTAMPTZ
);

-- ─── Updated-at auto-touch trigger ────────────────────────────────────────
CREATE OR REPLACE FUNCTION touch_updated_at() RETURNS TRIGGER AS $$
BEGIN NEW.updated_at = NOW(); RETURN NEW; END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER orgs_touch     BEFORE UPDATE ON orgs     FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE TRIGGER policies_touch BEFORE UPDATE ON policies FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
