ALTER TABLE incidents
    ADD COLUMN resolution_verification_id TEXT,
    ADD CONSTRAINT incidents_resolution_verification_fk
        FOREIGN KEY (resolution_verification_id)
        REFERENCES verifications (id)
        ON DELETE RESTRICT,
    ADD CONSTRAINT incidents_resolution_verification_state_check
        CHECK (
            resolution_verification_id IS NULL
            OR
            (
                state = 'RESOLVED'
                AND resolved_at IS NOT NULL
            )
        );

CREATE UNIQUE INDEX incidents_resolution_verification_unique
    ON incidents (resolution_verification_id)
    WHERE resolution_verification_id IS NOT NULL;

---- create above / drop below ----

DROP INDEX incidents_resolution_verification_unique;

ALTER TABLE incidents
    DROP CONSTRAINT incidents_resolution_verification_state_check,
    DROP CONSTRAINT incidents_resolution_verification_fk,
    DROP COLUMN resolution_verification_id;
