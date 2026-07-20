CREATE TABLE boards (
    board_id TEXT PRIMARY KEY,
    version INTEGER NOT NULL CHECK(version >= 1),
    title TEXT NOT NULL,
    board_json BLOB NOT NULL
);

CREATE TABLE board_definitions (
    board_id TEXT PRIMARY KEY REFERENCES boards(board_id) ON DELETE CASCADE,
    definition_json BLOB NOT NULL
);

CREATE TABLE events (
    board_id TEXT NOT NULL REFERENCES boards(board_id) ON DELETE CASCADE,
    version INTEGER NOT NULL CHECK(version >= 1),
    event_id TEXT NOT NULL,
    event_json BLOB NOT NULL,
    PRIMARY KEY(board_id, version)
);

CREATE TABLE commands (
    actor_id TEXT NOT NULL,
    command_id TEXT NOT NULL,
    request_hash BLOB NOT NULL,
    board_id TEXT NOT NULL,
    result_json BLOB NOT NULL,
    PRIMARY KEY(actor_id, command_id)
);

CREATE TABLE tasks (
    board_id TEXT NOT NULL REFERENCES boards(board_id) ON DELETE CASCADE,
    task_id TEXT NOT NULL,
    state TEXT NOT NULL,
    task_json BLOB NOT NULL,
    PRIMARY KEY(board_id, task_id)
);

CREATE TABLE dependencies (
    board_id TEXT NOT NULL REFERENCES boards(board_id) ON DELETE CASCADE,
    dependency_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    depends_on TEXT NOT NULL,
    dependency_json BLOB NOT NULL,
    PRIMARY KEY(board_id, dependency_id),
    UNIQUE(board_id, task_id, depends_on),
    FOREIGN KEY(board_id, task_id) REFERENCES tasks(board_id, task_id),
    FOREIGN KEY(board_id, depends_on) REFERENCES tasks(board_id, task_id)
);

CREATE TABLE attempts (
    board_id TEXT NOT NULL REFERENCES boards(board_id) ON DELETE CASCADE,
    attempt_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    state TEXT NOT NULL,
    attempt_json BLOB NOT NULL,
    PRIMARY KEY(board_id, attempt_id),
    FOREIGN KEY(board_id, task_id) REFERENCES tasks(board_id, task_id)
);

CREATE TABLE bindings (
    board_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    executor_id TEXT NOT NULL DEFAULT '',
    binding_json BLOB NOT NULL,
    PRIMARY KEY(board_id, attempt_id),
    UNIQUE(board_id, thread_id),
    FOREIGN KEY(board_id, attempt_id) REFERENCES attempts(board_id, attempt_id) ON DELETE CASCADE
);

CREATE TABLE leases (
    board_id TEXT NOT NULL REFERENCES boards(board_id) ON DELETE CASCADE,
    lease_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    state TEXT NOT NULL,
    expires_at INTEGER NOT NULL DEFAULT 0 CHECK(expires_at >= 0),
    lease_json BLOB NOT NULL,
    PRIMARY KEY(board_id, lease_id),
    FOREIGN KEY(board_id, task_id) REFERENCES tasks(board_id, task_id),
    FOREIGN KEY(board_id, attempt_id) REFERENCES attempts(board_id, attempt_id)
);
CREATE UNIQUE INDEX one_active_lease_per_task
    ON leases(board_id, task_id) WHERE state = 'active';
CREATE INDEX leases_expiry ON leases(state, expires_at);

CREATE TABLE evidence (
    board_id TEXT NOT NULL REFERENCES boards(board_id) ON DELETE CASCADE,
    evidence_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    state TEXT NOT NULL,
    evidence_json BLOB NOT NULL,
    PRIMARY KEY(board_id, evidence_id),
    FOREIGN KEY(board_id, task_id) REFERENCES tasks(board_id, task_id),
    FOREIGN KEY(board_id, attempt_id) REFERENCES attempts(board_id, attempt_id)
);
