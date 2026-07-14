PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS graph_events (
  event_id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  root_session_id TEXT NOT NULL,
  parent_session_id TEXT,
  parent_event_id TEXT,
  worker_id TEXT,
  type TEXT NOT NULL,
  role TEXT,
  model TEXT,
  effort TEXT,
  status TEXT,
  started_at TEXT,
  ended_at TEXT,
  duration_ms REAL,
  summary TEXT,
  content TEXT,
  tool_name TEXT,
  tool_use_id TEXT,
  raw_json TEXT NOT NULL,
  FOREIGN KEY (session_id) REFERENCES threads(session_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_graph_events_session_time
  ON graph_events(session_id, started_at, event_id);

CREATE INDEX IF NOT EXISTS idx_graph_events_root_time
  ON graph_events(root_session_id, started_at, event_id);

CREATE INDEX IF NOT EXISTS idx_graph_events_parent_session
  ON graph_events(parent_session_id, started_at, event_id);

CREATE INDEX IF NOT EXISTS idx_graph_events_parent_event
  ON graph_events(parent_event_id, started_at, event_id);
