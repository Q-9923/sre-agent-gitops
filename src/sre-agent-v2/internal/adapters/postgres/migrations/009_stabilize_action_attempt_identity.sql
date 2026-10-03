ALTER TABLE action_attempts
    DROP CONSTRAINT action_attempts_execution_key_unique,
    ADD CONSTRAINT action_attempts_action_key_unique
        UNIQUE (
            incident_id,
            plan_hash,
            target_uid
        );

---- create above / drop below ----

ALTER TABLE action_attempts
    DROP CONSTRAINT action_attempts_action_key_unique,
    ADD CONSTRAINT action_attempts_execution_key_unique
        UNIQUE (
            incident_id,
            plan_hash,
            target_uid,
            fencing_token
        );
