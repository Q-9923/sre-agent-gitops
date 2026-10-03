CREATE TABLE verifications (
    id TEXT PRIMARY KEY
        CHECK (id ~ '^ver-[0-9a-f]{24}$'),
    action_attempt_id TEXT NOT NULL UNIQUE
        REFERENCES action_attempts (id)
        ON DELETE RESTRICT,
    status TEXT NOT NULL
        CHECK (
            status IN (
                'PENDING',
                'RECOVERED',
                'NOT_RECOVERED',
                'INCONCLUSIVE'
            )
        ),
    version BIGINT NOT NULL
        CHECK (version > 0),
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    evidence_code TEXT,
    CONSTRAINT verifications_finished_at_check
        CHECK (
            finished_at IS NULL
            OR finished_at >= started_at
        ),
    CONSTRAINT verifications_result_consistency_check
        CHECK (
            (
                status = 'PENDING'
                AND version = 1
                AND finished_at IS NULL
                AND evidence_code IS NULL
            )
            OR
            (
                status IN (
                    'RECOVERED',
                    'NOT_RECOVERED',
                    'INCONCLUSIVE'
                )
                AND version = 2
                AND finished_at IS NOT NULL
                AND evidence_code IS NOT NULL
                AND btrim(evidence_code) <> ''
            )
        )
);

---- create above / drop below ----

DROP TABLE verifications;
