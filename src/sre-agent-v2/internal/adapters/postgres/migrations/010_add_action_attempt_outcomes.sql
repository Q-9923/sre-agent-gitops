ALTER TABLE action_attempts
    ADD COLUMN status TEXT NOT NULL DEFAULT 'STARTED',
    ADD COLUMN version BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN finished_at TIMESTAMPTZ,
    ADD COLUMN error_code TEXT,
    ADD COLUMN recovered_by_fencing_token BIGINT,
    ADD CONSTRAINT action_attempts_status_check
        CHECK (
            status IN (
                'STARTED',
                'SUCCEEDED',
                'FAILED',
                'UNKNOWN'
            )
        ),
    ADD CONSTRAINT action_attempts_version_check
        CHECK (version > 0),
    ADD CONSTRAINT action_attempts_finished_at_check
        CHECK (
            finished_at IS NULL
            OR finished_at >= started_at
        ),
    ADD CONSTRAINT action_attempts_result_consistency_check
        CHECK (
            (
                status = 'STARTED'
                AND version = 1
                AND finished_at IS NULL
                AND error_code IS NULL
                AND recovered_by_fencing_token IS NULL
            )
            OR
            (
                status = 'SUCCEEDED'
                AND version = 2
                AND finished_at IS NOT NULL
                AND error_code IS NULL
                AND recovered_by_fencing_token IS NULL
            )
            OR
            (
                status = 'FAILED'
                AND version = 2
                AND finished_at IS NOT NULL
                AND error_code IS NOT NULL
                AND btrim(error_code) <> ''
                AND recovered_by_fencing_token IS NULL
            )
            OR
            (
                status = 'UNKNOWN'
                AND version = 2
                AND finished_at IS NOT NULL
                AND error_code IS NOT NULL
                AND btrim(error_code) <> ''
                AND recovered_by_fencing_token IS NOT NULL
                AND recovered_by_fencing_token > fencing_token
            )
        ),
    ADD CONSTRAINT action_attempts_recovery_claim_fk
        FOREIGN KEY (
            incident_id,
            recovered_by_fencing_token
        )
        REFERENCES incident_claims (
            incident_id,
            incident_version
        )
        ON DELETE RESTRICT;

---- create above / drop below ----

ALTER TABLE action_attempts
    DROP CONSTRAINT action_attempts_recovery_claim_fk,
    DROP CONSTRAINT action_attempts_result_consistency_check,
    DROP CONSTRAINT action_attempts_finished_at_check,
    DROP CONSTRAINT action_attempts_version_check,
    DROP CONSTRAINT action_attempts_status_check,
    DROP COLUMN recovered_by_fencing_token,
    DROP COLUMN error_code,
    DROP COLUMN finished_at,
    DROP COLUMN version,
    DROP COLUMN status;
