ALTER TABLE incidents
    DROP CONSTRAINT incidents_state_check,
    DROP CONSTRAINT incidents_state_resolution_check,
    DROP CONSTRAINT incidents_approval_binding_check;

ALTER TABLE incidents
    ADD CONSTRAINT incidents_state_check
        CHECK (
            state IN (
                'DETECTED',
                'DIAGNOSED',
                'WAITING_APPROVAL',
                'VERIFYING',
                'RESOLVED'
            )
        ),
    ADD CONSTRAINT incidents_state_resolution_check
        CHECK (
            (
                state IN (
                    'DETECTED',
                    'DIAGNOSED',
                    'WAITING_APPROVAL',
                    'VERIFYING'
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
                state = 'VERIFYING'
                AND
                (
                    (
                        approval_plan_hash IS NULL
                        AND approval_target_uid IS NULL
                    )
                    OR
                    (
                        approval_plan_hash IS NOT NULL
                        AND btrim(approval_plan_hash) <> ''
                        AND approval_target_uid IS NOT NULL
                        AND btrim(approval_target_uid) <> ''
                    )
                )
            )
            OR
            (
                state NOT IN (
                    'WAITING_APPROVAL',
                    'VERIFYING'
                )
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
                'VERIFYING',
                'RESOLVED'
            )
        ),
    ADD CONSTRAINT incident_transitions_to_state_check
        CHECK (
            to_state IN (
                'DETECTED',
                'DIAGNOSED',
                'WAITING_APPROVAL',
                'VERIFYING',
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

ALTER TABLE incidents
    DROP CONSTRAINT incidents_state_check,
    DROP CONSTRAINT incidents_state_resolution_check,
    DROP CONSTRAINT incidents_approval_binding_check;

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
