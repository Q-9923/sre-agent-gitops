ALTER TABLE incidents
    DROP CONSTRAINT incidents_state_check,
    DROP CONSTRAINT incidents_state_resolution_check,
    ADD COLUMN approval_plan_hash TEXT,
    ADD COLUMN approval_target_uid TEXT;

ALTER TABLE incidents
    ADD CONSTRAINT incidents_state_check
        CHECK (
            state IN (
                'DETECTED',
                'DIAGNOSED',
                'WAITING_APPROVAL',
                'RESOLVED'
            )
        ),
    ADD CONSTRAINT incidents_state_resolution_check
        CHECK (
            (
                state IN (
                    'DETECTED',
                    'DIAGNOSED',
                    'WAITING_APPROVAL'
                )
                AND resolved_at IS NULL
            )
            OR
            (
                state = 'RESOLVED'
                AND resolved_at IS NOT NULL
            )
        ),
    ADD CONSTRAINT incidents_approval_binding_check
        CHECK (
            (
                state = 'WAITING_APPROVAL'
                AND approval_plan_hash IS NOT NULL
                AND btrim(approval_plan_hash) <> ''
                AND approval_target_uid IS NOT NULL
                AND btrim(approval_target_uid) <> ''
            )
            OR
            (
                state <> 'WAITING_APPROVAL'
                AND approval_plan_hash IS NULL
                AND approval_target_uid IS NULL
            )
        );

ALTER TABLE incident_transitions
    DROP CONSTRAINT incident_transitions_from_state_check,
    DROP CONSTRAINT incident_transitions_to_state_check;

ALTER TABLE incident_transitions
    ADD CONSTRAINT incident_transitions_from_state_check
        CHECK (
            from_state IN (
                'DETECTED',
                'DIAGNOSED',
                'WAITING_APPROVAL',
                'RESOLVED'
            )
        ),
    ADD CONSTRAINT incident_transitions_to_state_check
        CHECK (
            to_state IN (
                'DETECTED',
                'DIAGNOSED',
                'WAITING_APPROVAL',
                'RESOLVED'
            )
        );

---- create above / drop below ----

ALTER TABLE incident_transitions
    DROP CONSTRAINT incident_transitions_from_state_check,
    DROP CONSTRAINT incident_transitions_to_state_check;

ALTER TABLE incident_transitions
    ADD CONSTRAINT incident_transitions_from_state_check
        CHECK (
            from_state IN (
                'DETECTED',
                'DIAGNOSED',
                'RESOLVED'
            )
        ),
    ADD CONSTRAINT incident_transitions_to_state_check
        CHECK (
            to_state IN (
                'DETECTED',
                'DIAGNOSED',
                'RESOLVED'
            )
        );

ALTER TABLE incidents
    DROP CONSTRAINT incidents_approval_binding_check,
    DROP CONSTRAINT incidents_state_check,
    DROP CONSTRAINT incidents_state_resolution_check,
    DROP COLUMN approval_target_uid,
    DROP COLUMN approval_plan_hash;

ALTER TABLE incidents
    ADD CONSTRAINT incidents_state_check
        CHECK (
            state IN (
                'DETECTED',
                'DIAGNOSED',
                'RESOLVED'
            )
        ),
    ADD CONSTRAINT incidents_state_resolution_check
        CHECK (
            (
                state IN ('DETECTED', 'DIAGNOSED')
                AND resolved_at IS NULL
            )
            OR
            (
                state = 'RESOLVED'
                AND resolved_at IS NOT NULL
            )
        );
