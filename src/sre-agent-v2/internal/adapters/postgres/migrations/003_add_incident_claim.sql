ALTER TABLE incidents
    ADD COLUMN claim_holder TEXT,
    ADD COLUMN claim_expires_at TIMESTAMPTZ,
    ADD CONSTRAINT incidents_claim_lease_check
        CHECK (
            (
                claim_holder IS NULL
                AND claim_expires_at IS NULL
            )
            OR
            (
                claim_holder IS NOT NULL
                AND btrim(claim_holder) <> ''
                AND claim_expires_at IS NOT NULL
            )
        );

---- create above / drop below ----

ALTER TABLE incidents
    DROP CONSTRAINT incidents_claim_lease_check,
    DROP COLUMN claim_expires_at,
    DROP COLUMN claim_holder;
