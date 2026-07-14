PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS usage_records (
  usage_id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  message_id TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  observed_at TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT '',
  input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_5m_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_1h_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  is_fast INTEGER NOT NULL DEFAULT 0,
  carried_cost_usd REAL,
  FOREIGN KEY (session_id) REFERENCES threads(session_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_usage_session_time
  ON usage_records(session_id, observed_at);

CREATE INDEX IF NOT EXISTS idx_usage_model_time
  ON usage_records(model, observed_at);
