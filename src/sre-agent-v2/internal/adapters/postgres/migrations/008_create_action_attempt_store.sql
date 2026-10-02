CREATE TABLE action_attempts (
    id TEXT PRIMARY KEY
        CHECK (id ~ '^att-[0-9a-f]{24}$'),
    incident_id TEXT NOT NULL,
    plan_hash TEXT NOT NULL
        CHECK (btrim(plan_hash) <> ''),
    target_uid TEXT NOT NULL
        CHECK (btrim(target_uid) <> ''),
    fencing_token BIGINT NOT NULL
        CHECK (fencing_token > 0),
    started_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT action_attempts_execution_key_unique
        UNIQUE (
            incident_id,
            plan_hash,
            target_uid,
            fencing_token
        ),
    CONSTRAINT action_attempts_plan_fk
        FOREIGN KEY (
            incident_id,
            plan_hash,
            target_uid
        )
        REFERENCES incident_plans (
            incident_id,
            plan_hash,
            target_uid
        )
        ON DELETE RESTRICT,
    CONSTRAINT action_attempts_claim_fk
        FOREIGN KEY (
            incident_id,
            fencing_token
        )
        REFERENCES incident_claims (
            incident_id,
            incident_version
        )
        ON DELETE RESTRICT
);

---- create above / drop below ----

DROP TABLE action_attempts;
