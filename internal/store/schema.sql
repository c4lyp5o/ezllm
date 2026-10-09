-- ezllm schema (embedded via go:embed, applied by internal/store/migrate.go)
-- Conventions: WAL, foreign_keys ON, single writer + read pool.
-- Ledger rows (`calls`) and quota snapshots are IMMUTABLE / append-only: history
-- must survive key or account deletion, so attribution fields are denormalized.

-- Browser-only dashboard login; admin sessions are stored by token digest only.
CREATE TABLE IF NOT EXISTS dashboard_auth (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    password_hash TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS dashboard_sessions (
    token_hash TEXT PRIMARY KEY,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_dashboard_sessions_expiry ON dashboard_sessions(expires_at);

CREATE TABLE IF NOT EXISTS schema_version (
  version    INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- ── accounts: a BILLING identity. `namespace` is how clients address it ────────
--    kind selects the adapter: 'opencode-go' | 'openai-compatible' | 'anthropic-compatible'
CREATE TABLE IF NOT EXISTS accounts (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,             -- display: 'SSN GPT', 'opencode-go #1'
  namespace   TEXT NOT NULL UNIQUE,             -- routing slug: 'super-ssn', 'opengo'
  kind        TEXT NOT NULL,
  base_url    TEXT NOT NULL,
  enabled     INTEGER NOT NULL DEFAULT 1,
  requires_session_header INTEGER NOT NULL DEFAULT 0,
  quota_mode  TEXT NOT NULL DEFAULT 'probe',    -- probe|percent|money|tokens|none
  custom_headers TEXT,                          -- JSON object; e.g. CF-friendly UA
  probe_delay_ms INTEGER NOT NULL DEFAULT 400,  -- anti-Cloudflare-1010 pacing
  cap_window  TEXT NOT NULL DEFAULT 'monthly',  -- 5h|daily|weekly|monthly
  cap_tokens  INTEGER NOT NULL DEFAULT 0,       -- 0 = unlimited; at cap => hard 429
  notes       TEXT,
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- ── provider_keys: plaintext credential per explicit operator choice ────────
--    Rows are inserted ONLY after a passing key test, so enabled=0 is a
--    disabled-by-user state, never an untested key.
CREATE TABLE IF NOT EXISTS provider_keys (
  id           INTEGER PRIMARY KEY,
  account_id   INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  label        TEXT NOT NULL,
  key_hint     TEXT NOT NULL,
  key_plain    TEXT NOT NULL,
  enabled      INTEGER NOT NULL DEFAULT 1,
  last_test_at TEXT,
  last_test_ok INTEGER,
  last_test_detail TEXT,
  added_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_pkeys_account ON provider_keys(account_id, enabled);

-- ── quota_snapshots: APPEND-ONLY. Shape varies per provider (opencode=percent,
--    ssn-gpt=money), so `kind` discriminates and `raw` keeps the verbatim JSON.
CREATE TABLE IF NOT EXISTS quota_snapshots (
  id              INTEGER PRIMARY KEY,
  provider_key_id INTEGER NOT NULL REFERENCES provider_keys(id) ON DELETE CASCADE,
  kind            TEXT NOT NULL,                -- percent|money|tokens|none|error
  percent_rolling REAL, percent_weekly REAL, percent_monthly REAL,
  balance         REAL, balance_unit    TEXT,
  cost_total      REAL, cost_today      REAL,
  tokens_today    INTEGER, requests_today INTEGER,
  resets_at       TEXT,
  raw             TEXT NOT NULL,
  ok              INTEGER NOT NULL DEFAULT 1,
  err             TEXT,
  read_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_quota_key_ts ON quota_snapshots(provider_key_id, read_at DESC);

-- ── models: catalog snapshot + PER-PROTOCOL support (probed; upstream catalogs
--    do not advertise protocol support) ────────────────────────────────────────
CREATE TABLE IF NOT EXISTS models (
  id              INTEGER PRIMARY KEY,
  account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  model_id        TEXT NOT NULL,
  display_name    TEXT,
  owned_by        TEXT,
  proto_openai    INTEGER,                      -- 1 ok / 0 unsupported / NULL untested
  proto_anthropic INTEGER,
  proto_responses INTEGER,
  proto_tested_at TEXT,
  proto_pin       TEXT,                         -- NULL auto / 'openai' | 'anthropic' | 'responses' (enforced, never rerouted)
  UNIQUE(account_id, model_id)
);
CREATE INDEX IF NOT EXISTS idx_models_proto ON models(account_id, proto_anthropic, proto_responses);

-- ── compression_profiles: ordered stage pipelines ("combos for compression").
--    Every safety field defaults to SAFE; see the compression contract.
CREATE TABLE IF NOT EXISTS compression_profiles (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  enabled       INTEGER NOT NULL DEFAULT 1,
  stages        TEXT NOT NULL DEFAULT '[]',      -- JSON: [{"engine":"session_dedup"},{"engine":"rtk","scope":"tool_only"}]
  exempt_last_turn    INTEGER NOT NULL DEFAULT 1,
  preserve_tool_calls INTEGER NOT NULL DEFAULT 1,
  min_compress_ratio  REAL NOT NULL DEFAULT 0.05,
  fail_open           INTEGER NOT NULL DEFAULT 1,
  auto_trigger_tokens INTEGER NOT NULL DEFAULT 0, -- 0 = always; else only above N prompt tokens
  notes         TEXT,
  created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- ── combos: NO namespace. Addressed by bare `name` (must not contain '/'),
--    which keeps the combo space disjoint from account namespaces by shape.
CREATE TABLE IF NOT EXISTS combos (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  enabled     INTEGER NOT NULL DEFAULT 1,
  strategy    TEXT NOT NULL DEFAULT 'failover'
              CHECK (strategy IN ('failover','true_round_robin','strict_round_robin','sticky_last_good','least_used')),
  sticky_idle_s INTEGER NOT NULL DEFAULT 1800,  -- sticky_last_good: unstick after idle
  compression_profile_id INTEGER REFERENCES compression_profiles(id),
  context_size INTEGER NOT NULL DEFAULT 200000 CHECK (context_size BETWEEN 1 AND 10000000),
  rr_cursor   INTEGER NOT NULL DEFAULT 0,       -- strict_round_robin position
  rr_epoch    INTEGER NOT NULL DEFAULT 0,       -- fingerprint of eligible hop set
  notes       TEXT,
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS combo_hops (
  id          INTEGER PRIMARY KEY,
  combo_id    INTEGER NOT NULL REFERENCES combos(id) ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  account_id  INTEGER NOT NULL REFERENCES accounts(id),
  model_id    TEXT NOT NULL,
  weight      INTEGER NOT NULL DEFAULT 1,
  enabled     INTEGER NOT NULL DEFAULT 1,
  UNIQUE(combo_id, position)
);
CREATE INDEX IF NOT EXISTS idx_hops_combo ON combo_hops(combo_id, enabled, position);

-- ── client_tokens: who may call us (per-agent attribution + caps) ─────────────
CREATE TABLE IF NOT EXISTS client_tokens (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,              -- 'hermes', 'claude-code'
  token_hash  TEXT NOT NULL UNIQUE,              -- sha256 hex; plaintext never stored
  roles       TEXT NOT NULL DEFAULT 'infer',     -- csv: infer,admin
  enabled     INTEGER NOT NULL DEFAULT 1,
  cap_window  TEXT NOT NULL DEFAULT 'monthly',
  cap_tokens  INTEGER NOT NULL DEFAULT 0,        -- 0 = unlimited
  compression_profile_id INTEGER REFERENCES compression_profiles(id),
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- ── calls: the LEDGER. Surface-independent normalized counters + attribution.
--    IMMUTABLE: never UPDATE a row; account/key_hint are denormalized text so
--    history stays readable after deletion.
CREATE TABLE IF NOT EXISTS calls (
  id             INTEGER PRIMARY KEY,
  ts             TEXT NOT NULL,
  account_id     INTEGER,                       -- M5: denormalized NAME survives key deletion, this is the id for metering
  client         TEXT NOT NULL DEFAULT '',
  surface        TEXT NOT NULL,                  -- openai|anthropic|responses
  alias          TEXT NOT NULL DEFAULT '',       -- combo name or namespace/model requested
  account        TEXT NOT NULL,                  -- denormalized
  provider_key_id INTEGER,
  key_hint       TEXT NOT NULL DEFAULT '',       -- denormalized
  model          TEXT NOT NULL,                  -- model actually sent upstream
  status         INTEGER NOT NULL,
  stream         INTEGER NOT NULL DEFAULT 0,
  ttft_ms        INTEGER,
  total_ms       INTEGER,
  tokens_in           INTEGER NOT NULL DEFAULT 0,
  tokens_out          INTEGER NOT NULL DEFAULT 0,
  tokens_cached_read  INTEGER NOT NULL DEFAULT 0,
  tokens_cached_write INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
  raw_usage      TEXT,
  endpoint_id    TEXT,                           -- x-opencode-endpoint-id
  upstream_model TEXT,                           -- x-opencode-upstream-model-id
  compression_profile TEXT NOT NULL DEFAULT '',
  compression_applied INTEGER NOT NULL DEFAULT 0,
  prompt_tokens_pre   INTEGER NOT NULL DEFAULT 0,
  tokens_saved        INTEGER NOT NULL DEFAULT 0,
  compression_ms      INTEGER,
  -- caveman attribution: how many prose rules actually fired. 0 for every
  -- other engine, so a surprising saving is traceable to condensation.
  compression_rules_fired INTEGER NOT NULL DEFAULT 0,
  context_tokens_pre INTEGER NOT NULL DEFAULT 0,
  context_tokens_saved INTEGER NOT NULL DEFAULT 0,
  err            TEXT
);
CREATE INDEX IF NOT EXISTS idx_calls_ts        ON calls(ts DESC);
CREATE INDEX IF NOT EXISTS idx_calls_acct_ts   ON calls(account, ts DESC);
CREATE INDEX IF NOT EXISTS idx_calls_client_ts ON calls(client, ts DESC);
CREATE INDEX IF NOT EXISTS idx_calls_key_ts    ON calls(provider_key_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_calls_alias_ts  ON calls(alias, ts DESC);
CREATE INDEX IF NOT EXISTS idx_calls_acct_model ON calls(account_id, model, ts DESC);

-- ── key_cooldowns: reactive backoff; persisted so it survives restart ────────
CREATE TABLE IF NOT EXISTS key_cooldowns (
  provider_key_id INTEGER PRIMARY KEY REFERENCES provider_keys(id) ON DELETE CASCADE,
  until_ts        TEXT NOT NULL,
  reason          TEXT NOT NULL                  -- 429|5xx|timeout|auth|protocol
);

-- ── M5: rules engine ────────────────────────────────────────────────────────
-- One row per (account, model) — mirrors models' UNIQUE(account_id, model_id),
-- so a rule is looked up with exactly the key the router's Eligible seam passes.
-- cap_tokens 0 = unlimited. win_* NULL/empty = the model is always allowed.
CREATE TABLE IF NOT EXISTS model_rules (
  id          INTEGER PRIMARY KEY,
  account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  model_id    TEXT    NOT NULL,
  cap_tokens  INTEGER NOT NULL DEFAULT 0,               -- 0 = unlimited
  cap_window  TEXT    NOT NULL DEFAULT 'monthly',       -- 5h|daily|weekly|monthly
  win_start   TEXT,                                     -- 'HH:MM' local to win_tz
  win_end     TEXT,                                     -- end<start wraps midnight
  win_days    TEXT,                                     -- csv 0-6 (Sun=0); NULL/e'' = every day
  win_tz      TEXT    NOT NULL DEFAULT 'Asia/Kuala_Lumpur',
  enabled     INTEGER NOT NULL DEFAULT 1,
  note        TEXT    NOT NULL DEFAULT '',
  updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  UNIQUE(account_id, model_id)
);

-- O(1) usage meter. WITHOUT ROWID makes the PK the physical index, so an
-- increment is one row touch and the read is a single PK lookup — no SUM()
-- over calls anywhere near the hot path.
-- Rotation is implicit: a new window writes a new bucket key, so there is no
-- reset job and no "forgot to reset the counter" class of bug.
CREATE TABLE IF NOT EXISTS usage_counters (
  account_id INTEGER NOT NULL,
  model_id   TEXT    NOT NULL,
  bucket     TEXT    NOT NULL,   -- 2026-10-06 | 2026-W41 | 2026-10 | 2026-10-06T2
  tokens     INTEGER NOT NULL DEFAULT 0,
  calls      INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT    NOT NULL,
  PRIMARY KEY (account_id, model_id, bucket)
) WITHOUT ROWID;
