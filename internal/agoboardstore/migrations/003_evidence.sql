-- Schema 4 records structured evidence and the artifacts it references.
--
-- As with schema 3, boards.board_json stays canonical. The artifacts table is a
-- projection of the artifact references inside evidence, maintained by
-- syncProjection. It exists so the artifact store can be reconciled after a
-- crash by asking the database which artifact identifiers are still referenced,
-- without decoding every board.

CREATE TABLE artifacts (
    board_id TEXT NOT NULL REFERENCES boards(board_id) ON DELETE CASCADE,
    artifact_id TEXT NOT NULL,
    evidence_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    media_type TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL DEFAULT '',
    byte_size INTEGER NOT NULL DEFAULT 0 CHECK(byte_size >= 0),
    sha256 TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(board_id, artifact_id)
);

CREATE INDEX artifacts_by_evidence ON artifacts(board_id, evidence_id);

-- Deterministic acceptance is queryable: a reviewer can find evidence whose
-- required checks did not pass without decoding the aggregate.
ALTER TABLE evidence ADD COLUMN required_tests_passed INTEGER NOT NULL DEFAULT 1 CHECK(required_tests_passed IN (0,1));
ALTER TABLE evidence ADD COLUMN verdict TEXT NOT NULL DEFAULT '';
