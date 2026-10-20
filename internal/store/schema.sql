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
  -- Account-WIDE request rate. The provider's ceiling applies to the
  -- credential, not to any one model: a 5-requests-per-minute tier is spent by
  -- five calls to five DIFFERENT models. So this lives on the account, and
  -- every hop routed through it shares one budget. 0 = unlimited.
  rate_limit  INTEGER NOT NULL DEFAULT 0,       -- requests per rate_window
  rate_window TEXT NOT NULL DEFAULT '',         -- Go duration, e.g. '60s'; '' = off
  max_context_tokens INTEGER NOT NULL DEFAULT 0,  -- 0 = unknown/unlimited; filter by this BEFORE dispatch
  -- How the upstream Messages wire counts input_tokens: 'auto' = unknown
  -- (cross-wire Messages destinations refuse), 'anthropic' = input EXCLUDES
  -- cache tokens, 'total' = input INCLUDES cache tokens. An OPERATOR fact:
  -- never inferred from kind/base_url/wiring.
  usage_convention TEXT NOT NULL DEFAULT 'auto',
  notes       TEXT,
  management_key_plain TEXT,                    -- optional second credential (e.g. TokenRouter management key), encrypted at rest
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
  proto_source    TEXT,                         -- 'probe' | 'curated' | NULL legacy: who wrote the verdicts; curated sticks until released
  proto_note      TEXT,                         -- operator's reason, curated rows only
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
  fresh_tool_output   TEXT NOT NULL DEFAULT 'keep', -- v24: keep | compress a trailing tool result on first send
  notes         TEXT,
  created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- ── combos: an ordered hop list a client addresses by bare `name`. NO
--    namespace — names must not contain '/', which keeps the combo space
--    disjoint from account namespaces by shape.
--    `strategy` is validated in Go (store.ValidStrategies), deliberately NOT by
--    a schema CHECK: a CHECK means every new strategy needs a full table
--    rebuild, because SQLite cannot alter a constraint. block_round_robin is
--    what forced that rebuild once; the duplication is gone so it cannot
--    happen again.
CREATE TABLE IF NOT EXISTS combos (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  enabled     INTEGER NOT NULL DEFAULT 1,
  strategy    TEXT NOT NULL DEFAULT 'failover',
  sticky_idle_s INTEGER NOT NULL DEFAULT 1800,  -- sticky_last_good: unstick after idle
  compression_profile_id INTEGER REFERENCES compression_profiles(id),
  context_size INTEGER NOT NULL DEFAULT 200000 CHECK (context_size BETWEEN 1 AND 10000000),
  -- block_round_robin: consecutive dispatches served by one hop before the
  -- rotation advances. 1 = plain per-request round robin.
  block_size  INTEGER NOT NULL DEFAULT 1 CHECK (block_size BETWEEN 1 AND 1000),
  rr_cursor   INTEGER NOT NULL DEFAULT 0,       -- UNUSED (rotation cursor is in-process)
  rr_epoch    INTEGER NOT NULL DEFAULT 0,       -- UNUSED (see rr_cursor)
  notes       TEXT,
  -- auto_free: hops are derived from the catalogs' free models (see
  -- store/autofree.go). auto_free_accounts is a comma list of account ids the
  -- pool draws from; '' = every enabled account.
  auto_free   INTEGER NOT NULL DEFAULT 0,
  auto_free_accounts TEXT NOT NULL DEFAULT '',
  auto_free_include TEXT NOT NULL DEFAULT '',  -- JSON [{account_id,model_id}]: always in the pool
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
  -- block_round_robin: when 1, this hop does not rotate on its N dispatches —
  -- it HOLDS the block while it keeps succeeding, and releases it on a
  -- hop-attributable failure (429/5xx/transport/failed stream; a 4xx is the
  -- request's fault and keeps the hold).
  hold_until_failure INTEGER NOT NULL DEFAULT 0,
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
  allowed_models TEXT NOT NULL DEFAULT '',      -- CSV globs; '' = every model
  expires_at  TEXT NOT NULL DEFAULT '',          -- RFC3339 UTC; '' = never
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
  ip             TEXT NOT NULL DEFAULT '',       -- v14: caller IP (X-Forwarded-For/X-Real-IP/remote)
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
  compression_version TEXT,
  compression_stages  TEXT,
  shadow_tokens_saved INTEGER NOT NULL DEFAULT 0,
  -- caveman attribution: how many prose rules actually fired. 0 for every
  -- other engine, so a surprising saving is traceable to condensation.
  compression_rules_fired INTEGER NOT NULL DEFAULT 0,
  context_tokens_pre INTEGER NOT NULL DEFAULT 0,
  context_tokens_saved INTEGER NOT NULL DEFAULT 0,
  -- v8 (M9 research): what the ledger could NOT answer before.
  -- "Would compression have saved?" is kept separate from what actually
  -- shipped, so the dashboard headline (SUM(tokens_saved)) is never inflated
  -- by requests the ratio floor rejected.
  compression_saved_notional INTEGER NOT NULL DEFAULT 0,
  -- Cacheability is unmeasurable without the request SHAPE, which raw_usage
  -- (a response) cannot give. prefix_sha is the stable conversation prefix
  -- (system + tools + all but the newest turn); two calls sharing it are the
  -- append-only pattern prompt caching rewards. msg/tool counts size the
  -- action space; session_id is the operator's own grouping key (client sends
  -- X-Ezllm-Session); req_bytes is the exact payload posted upstream.
  prefix_sha     TEXT NOT NULL DEFAULT '',
  msg_count      INTEGER NOT NULL DEFAULT 0,
  tool_count     INTEGER NOT NULL DEFAULT 0,
  session_id     TEXT NOT NULL DEFAULT '',
  req_bytes      INTEGER NOT NULL DEFAULT 0,
  -- block_round_robin: this dispatch's 1-based position inside its block.
  -- 0 = not counted (direct routes, non-block strategies, len(routes)==0).
  -- The "N requests finished" tag the dashboard renders as ▮ pos/size.
  block_pos      INTEGER NOT NULL DEFAULT 0,
  -- v12: the served hop's WIRE differed from the client's, so the bodies were
  -- TRANSLATED — the anthropic->openai-chat leg (Claude Code onto an
  -- openai-only hop) or a hop curated onto the Responses wire. Attribution,
  -- so translated traffic is a row you can count instead of infer from
  -- surface+model. 0 = the client surface and the hop wire matched.
  -- Usage was long the exception here: the synthesized envelope passed the
  -- vendor's INCLUSIVE input total through and carried no cache split, so
  -- tokens_cached_read stayed 0. Fixed by projecting the split on every client
  -- wire (Messages subtracts cache from input_tokens; OpenAI/Responses keep the
  -- inclusive total and fill the details object), so tokens_in now follows the
  -- same never-includes-cached invariant as a native row. Historical rows keep
  -- their inclusive totals.
  translated     INTEGER NOT NULL DEFAULT 0,
  err            TEXT
);
CREATE INDEX IF NOT EXISTS idx_calls_ts        ON calls(ts DESC);
CREATE INDEX IF NOT EXISTS idx_calls_acct_ts   ON calls(account, ts DESC);
CREATE INDEX IF NOT EXISTS idx_calls_client_ts ON calls(client, ts DESC);
CREATE INDEX IF NOT EXISTS idx_calls_key_ts    ON calls(provider_key_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_calls_alias_ts  ON calls(alias, ts DESC);
CREATE INDEX IF NOT EXISTS idx_calls_acct_model ON calls(account_id, model, ts DESC);
-- v8: cacheability research groups by stable prefix within a model+window, and
-- session rollups filter by session_id. Both are new hot paths for /admin only.
CREATE INDEX IF NOT EXISTS idx_calls_prefix ON calls(model, prefix_sha, ts DESC);
CREATE INDEX IF NOT EXISTS idx_calls_session ON calls(session_id, ts DESC);

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

-- v20: per-client-token budget meter. One row per (token name, window bucket),
-- advanced in the same transaction as the ledger insert. Counted tokens are the
-- same quantity as model caps: tokens_in + tokens_out.
CREATE TABLE IF NOT EXISTS client_usage_counters (
  client     TEXT    NOT NULL,
  bucket     TEXT    NOT NULL,
  tokens     INTEGER NOT NULL DEFAULT 0,
  calls      INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT    NOT NULL,
  PRIMARY KEY (client, bucket)
) WITHOUT ROWID;

-- v21: in-flight budget reservations. A request reserves its max_tokens
-- before dispatch; the unused remainder is released when the handler returns.
-- Rows carry an expiry so a crashed request cannot pin a budget forever.
CREATE TABLE IF NOT EXISTS client_budget_reservations (
  id         INTEGER PRIMARY KEY,
  client     TEXT    NOT NULL,
  bucket     TEXT    NOT NULL,
  tokens     INTEGER NOT NULL,
  created_at TEXT    NOT NULL,
  expires_at TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_client_reservations ON client_budget_reservations(client, bucket);
-- Per-million-USD rates. Keyed by (account, model), NOT model alone: the same
-- model id costs different money through different resellers, and that is
-- exactly the arbitrage the routing research cares about. Versioned by
-- effective_from (a price change is a NEW row, never an UPDATE) so historical
-- cost re-derivation stays correct when a provider reprices — every rate we
-- seed today came from a dated source (arXiv 2601.06007 App. A, Jan 2026) and
-- will be stale eventually.
--
-- Cost of a call (see NormalizeUsage — tokens_in is ALREADY the non-cached,
-- full-price input portion, so the four columns partition input with no
-- overlap and the bill is a plain weighted sum / 1e6):
--   usd = (tokens_in*price_in + tokens_out*price_out
--          + tokens_cached_read*price_cache_read + tokens_cached_write*price_cache_write)/1e6
-- NULL/absent price row => cost unknown (0), never guessed.
CREATE TABLE IF NOT EXISTS model_prices (
  account_id         INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  model_id           TEXT    NOT NULL,
  effective_from     TEXT    NOT NULL,          -- 'YYYY-MM-DD' the rate started
  currency           TEXT    NOT NULL DEFAULT 'USD',
  price_in           REAL    NOT NULL DEFAULT 0, -- per 1M full-price input tokens
  price_out          REAL    NOT NULL DEFAULT 0,
  price_cache_read   REAL    NOT NULL DEFAULT 0,
  price_cache_write  REAL    NOT NULL DEFAULT 0,
  note               TEXT    NOT NULL DEFAULT '',
  source             TEXT    NOT NULL DEFAULT 'manual', -- manual|import|openrouter|litellm
  fetched_at         TEXT    NOT NULL DEFAULT '',       -- when the source was read
  updated_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  PRIMARY KEY (account_id, model_id, effective_from)
);

-- ── v15: service_bindings — the DEFAULT model per service endpoint ──────────
-- The Services page: which model serves /v1/embeddings, /v1/images/generations,
-- /v1/audio/speech, /v1/audio/transcriptions when the client's request omits
-- `model`. An explicit model in the request always wins — this is a default,
-- never a routing override. `model_target` feeds ResolveCandidates verbatim, so
-- it may be '<namespace>/<model_id>' or a bare combo name.
--   mode 'manual' — model_target serves the endpoint
--   mode 'auto'   — the most recently tested-ok model for the surface serves it
-- No CHECK on mode: SQLite cannot alter a constraint (same rule as
-- combos.strategy); the values are validated in Go.
CREATE TABLE IF NOT EXISTS service_bindings (
  surface      TEXT PRIMARY KEY,               -- 'embeddings'|'images_gen'|'audio_tts'|'audio_stt'
  mode         TEXT NOT NULL DEFAULT 'manual', -- 'manual'|'auto'
  model_target TEXT NOT NULL DEFAULT '',       -- '' only with 'auto'
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- ── v15: service_probes — real-test verdicts per (surface, account, model) ──
-- Written only by an explicit per-card Test click (image probes cost money — no
-- sweep may ever run one unattended). `ok` is "the wire answered this service
-- shape", not output quality. A green probe replaces the row; detail keeps the
-- upstream's own words on failure.
CREATE TABLE IF NOT EXISTS service_probes (
  surface    TEXT    NOT NULL,
  account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  model_id   TEXT    NOT NULL,
  ok         INTEGER NOT NULL,
  detail     TEXT    NOT NULL DEFAULT '',
  tested_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  PRIMARY KEY (surface, account_id, model_id)
);

-- ── v23: catalog_gen — routing-catalog generation counter ──────────────────
--    The router serves accounts, keys, models and combos from an in-memory
--    snapshot (store.CatalogSnapshot) and re-reads only this one row per request.
--    Every write to a table the snapshot holds bumps the counter, from any code
--    path or process, so a stale snapshot can never be served after a write.
CREATE TABLE IF NOT EXISTS catalog_gen (
  id  INTEGER PRIMARY KEY CHECK (id = 1),
  gen INTEGER NOT NULL
);
INSERT OR IGNORE INTO catalog_gen (id, gen) VALUES (1, 0);
CREATE TRIGGER IF NOT EXISTS catalog_gen_accounts_ins AFTER INSERT ON accounts BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_accounts_upd AFTER UPDATE ON accounts BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_accounts_del AFTER DELETE ON accounts BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_provider_keys_ins AFTER INSERT ON provider_keys BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_provider_keys_upd AFTER UPDATE ON provider_keys BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_provider_keys_del AFTER DELETE ON provider_keys BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_key_cooldowns_ins AFTER INSERT ON key_cooldowns BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_key_cooldowns_upd AFTER UPDATE ON key_cooldowns BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_key_cooldowns_del AFTER DELETE ON key_cooldowns BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_models_ins AFTER INSERT ON models BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_models_upd AFTER UPDATE ON models BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_models_del AFTER DELETE ON models BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_combos_ins AFTER INSERT ON combos BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_combos_upd AFTER UPDATE ON combos BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_combos_del AFTER DELETE ON combos BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_combo_hops_ins AFTER INSERT ON combo_hops BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_combo_hops_upd AFTER UPDATE ON combo_hops BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
CREATE TRIGGER IF NOT EXISTS catalog_gen_combo_hops_del AFTER DELETE ON combo_hops BEGIN UPDATE catalog_gen SET gen = gen + 1 WHERE id = 1; END;
