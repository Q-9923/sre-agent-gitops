CREATE TABLE incident_claims (
    incident_id TEXT NOT NULL
        REFERENCES incidents (id)
        ON DELETE RESTRICT,
    incident_version BIGINT NOT NULL
        CHECK (incident_version > 0),
    holder_id TEXT NOT NULL
        CHECK (btrim(holder_id) <> ''),
    acquired_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT incident_claims_lease_window_check
        CHECK (expires_at > acquired_at),
    PRIMARY KEY (incident_id, incident_version)
);

---- create above / drop below ----

DROP TABLE incident_claims;
