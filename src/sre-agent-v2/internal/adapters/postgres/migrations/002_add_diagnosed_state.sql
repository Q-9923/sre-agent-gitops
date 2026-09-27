ALTER TABLE incidents
    DROP CONSTRAINT incidents_state_check,
    DROP CONSTRAINT incidents_check;

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

---- create above / drop below ----

ALTER TABLE incident_transitions
    DROP CONSTRAINT incident_transitions_from_state_check,
    DROP CONSTRAINT incident_transitions_to_state_check;

ALTER TABLE incident_transitions
    ADD CONSTRAINT incident_transitions_from_state_check
        CHECK (
            from_state IN (
                'DETECTED',
                'RESOLVED'
            )
        ),
    ADD CONSTRAINT incident_transitions_to_state_check
        CHECK (
            to_state IN (
                'DETECTED',
                'RESOLVED'
            )
        );

ALTER TABLE incidents
    DROP CONSTRAINT incidents_state_check,
    DROP CONSTRAINT incidents_state_resolution_check;

ALTER TABLE incidents
    ADD CONSTRAINT incidents_state_check
        CHECK (
            state IN (
                'DETECTED',
                'RESOLVED'
            )
        ),
    ADD CONSTRAINT incidents_check
        CHECK (
            (
                state = 'DETECTED'
                AND resolved_at IS NULL
            )
            OR
            (
                state = 'RESOLVED'
                AND resolved_at IS NOT NULL
            )
        );
