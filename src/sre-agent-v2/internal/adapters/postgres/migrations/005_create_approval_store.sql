CREATE TABLE approvals (
    incident_id TEXT NOT NULL
        REFERENCES incidents (id)
        ON DELETE RESTRICT,
    plan_hash TEXT NOT NULL
        CHECK (btrim(plan_hash) <> ''),
    target_uid TEXT NOT NULL
        CHECK (btrim(target_uid) <> ''),
    approved_by TEXT NOT NULL
        CHECK (btrim(approved_by) <> ''),
    approved_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT approvals_validity_window_check
        CHECK (expires_at > approved_at),
    PRIMARY KEY (incident_id, plan_hash, target_uid)
);

---- create above / drop below ----

DROP TABLE approvals;
