-- Schema 3 adds the durable data the background scheduler and its fencing,
-- retry, and concurrency rules need.
--
-- Canonical truth stays in boards.board_json. Every column added here is a
-- projection of that aggregate, maintained by syncProjection, and exists only
-- so the scheduler can count slots and find due leases with an index instead of
-- decoding every board. The one exception being retired by this migration is
-- leases.expires_at, which until now lived only in SQL; it is copied into the
-- aggregate by the accompanying backfill and becomes derived like the rest.

ALTER TABLE boards ADD COLUMN repository_id TEXT NOT NULL DEFAULT '';
ALTER TABLE boards ADD COLUMN paused INTEGER NOT NULL DEFAULT 0 CHECK(paused IN (0,1));
ALTER TABLE boards ADD COLUMN next_generation INTEGER NOT NULL DEFAULT 1 CHECK(next_generation >= 1);

ALTER TABLE tasks ADD COLUMN access_mode TEXT NOT NULL DEFAULT 'write' CHECK(access_mode IN ('read','write'));
ALTER TABLE tasks ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0);
ALTER TABLE tasks ADD COLUMN next_eligible_at INTEGER NOT NULL DEFAULT 0 CHECK(next_eligible_at >= 0);
ALTER TABLE tasks ADD COLUMN failure_class TEXT NOT NULL DEFAULT '';

ALTER TABLE attempts ADD COLUMN attempt_number INTEGER NOT NULL DEFAULT 0 CHECK(attempt_number >= 0);
ALTER TABLE attempts ADD COLUMN generation INTEGER NOT NULL DEFAULT 0 CHECK(generation >= 0);
ALTER TABLE attempts ADD COLUMN fencing_token TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN failure_class TEXT NOT NULL DEFAULT '';

ALTER TABLE leases ADD COLUMN generation INTEGER NOT NULL DEFAULT 0 CHECK(generation >= 0);
ALTER TABLE leases ADD COLUMN fencing_token TEXT NOT NULL DEFAULT '';
ALTER TABLE leases ADD COLUMN acquired_at INTEGER NOT NULL DEFAULT 0 CHECK(acquired_at >= 0);
ALTER TABLE leases ADD COLUMN access_mode TEXT NOT NULL DEFAULT 'write' CHECK(access_mode IN ('read','write'));
ALTER TABLE leases ADD COLUMN repository_id TEXT NOT NULL DEFAULT '';

-- A fencing token must be unique across the whole store for as long as it
-- exists, so a token can never authorize two attempts. Empty tokens are the
-- migrated "no authority" marker and are deliberately excluded.
CREATE UNIQUE INDEX attempts_fencing_token
    ON attempts(fencing_token) WHERE fencing_token <> '';

-- Slot accounting indexes. The scheduler counts active leases per repository
-- and access mode inside its claim transaction.
CREATE INDEX leases_repository_slots
    ON leases(repository_id, access_mode) WHERE state = 'active';
CREATE INDEX leases_board_slots
    ON leases(board_id) WHERE state = 'active';

-- Retry eligibility lookup for the scheduler's readiness pass.
CREATE INDEX tasks_eligibility ON tasks(board_id, state, next_eligible_at);
