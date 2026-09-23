CREATE TABLE incidents (
    id TEXT PRIMARY KEY
        CHECK (id ~ '^inc-[0-9a-f]{24}$'),
    idempotency_key TEXT NOT NULL
        CHECK (length(idempotency_key) = 64),
    source TEXT NOT NULL
        CHECK (btrim(source) <> ''),
    cluster TEXT NOT NULL
        CHECK (btrim(cluster) <> ''),
    alert_name TEXT NOT NULL
        CHECK (btrim(alert_name) <> ''),
    target_kind TEXT NOT NULL
        CHECK (btrim(target_kind) <> ''),
    target_namespace TEXT NOT NULL,
    target_name TEXT NOT NULL
        CHECK (btrim(target_name) <> ''),
    target_uid TEXT NOT NULL
        CHECK (btrim(target_uid) <> ''),
    state TEXT NOT NULL
        CHECK (state IN ('DETECTED', 'RESOLVED')),
    version BIGINT NOT NULL DEFAULT 1
        CHECK (version > 0),
    detected_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    resolved_at TIMESTAMPTZ,
    CHECK (
        (state = 'DETECTED' AND resolved_at IS NULL)
        OR
        (state = 'RESOLVED' AND resolved_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX incidents_one_active_per_idempotency_key
    ON incidents (idempotency_key)
    WHERE resolved_at IS NULL;

CREATE TABLE incident_transitions (
    incident_id TEXT NOT NULL
        REFERENCES incidents (id)
        ON DELETE RESTRICT,
    version BIGINT NOT NULL
        CHECK (version > 1),
    from_state TEXT NOT NULL
        CHECK (from_state IN ('DETECTED', 'RESOLVED')),
    to_state TEXT NOT NULL
        CHECK (to_state IN ('DETECTED', 'RESOLVED')),
    actor TEXT NOT NULL
        CHECK (btrim(actor) <> ''),
    reason_code TEXT NOT NULL
        CHECK (btrim(reason_code) <> ''),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (incident_id, version)
);

---- create above / drop below ----

DROP TABLE incident_transitions;
DROP TABLE incidents;
