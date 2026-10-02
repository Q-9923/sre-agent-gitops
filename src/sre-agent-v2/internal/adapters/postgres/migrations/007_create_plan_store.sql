CREATE TABLE incident_plans (
    incident_id TEXT NOT NULL
        REFERENCES incidents (id)
        ON DELETE RESTRICT,
    plan_hash TEXT NOT NULL,
    action TEXT NOT NULL,
    target_cluster TEXT NOT NULL,
    target_namespace TEXT NOT NULL,
    target_kind TEXT NOT NULL,
    target_name TEXT NOT NULL,
    target_uid TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
        DEFAULT statement_timestamp(),
    CONSTRAINT incident_plans_identity_check
        CHECK (
            btrim(plan_hash) <> ''
            AND btrim(action) <> ''
            AND btrim(target_cluster) <> ''
            AND btrim(target_namespace) <> ''
            AND btrim(target_kind) <> ''
            AND btrim(target_name) <> ''
            AND btrim(target_uid) <> ''
        ),
    PRIMARY KEY (
        incident_id,
        plan_hash,
        target_uid
    )
);

---- create above / drop below ----

DROP TABLE incident_plans;
