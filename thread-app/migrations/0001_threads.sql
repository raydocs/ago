PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS threads (
  session_id TEXT PRIMARY KEY,
  parent_session_id TEXT,
  root_session_id TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT 'Untitled thread',
  cwd TEXT NOT NULL DEFAULT '',
  project_name TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  effort TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'supervisor',
  state TEXT NOT NULL DEFAULT 'active',
  started_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  ended_at TEXT,
  FOREIGN KEY (parent_session_id) REFERENCES threads(session_id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_threads_parent ON threads(parent_session_id);
CREATE INDEX IF NOT EXISTS idx_threads_updated ON threads(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_threads_project ON threads(project_name, updated_at DESC);

CREATE TABLE IF NOT EXISTS events (
  event_id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  event_type TEXT NOT NULL,
  tool_name TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY (session_id) REFERENCES threads(session_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_events_session_time ON events(session_id, created_at ASC);
CREATE INDEX IF NOT EXISTS idx_events_type_time ON events(event_type, created_at DESC);
