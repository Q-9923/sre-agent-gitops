package postgres

import (
	"context"
	"fmt"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func (registry *Registry) ClaimRecoveredVerificationResolution(
	ctx context.Context,
	command incidentdomain.
		ClaimRecoveredVerificationResolutionCommand,
) (
	incidentdomain.RecoveredVerificationResolutionClaim,
	bool,
	error,
) {
	if registry == nil || registry.pool == nil {
		return incidentdomain.
				RecoveredVerificationResolutionClaim{},
			false,
			fmt.Errorf(
				"%w: PostgreSQL registry is not configured",
				incidentdomain.
					ErrRecoveredVerificationResolutionSchedulerUnavailable,
			)
	}

	if ctx == nil {
		return incidentdomain.
				RecoveredVerificationResolutionClaim{},
			false,
			fmt.Errorf(
				"%w: context is required",
				incidentdomain.
					ErrInvalidRecoveredVerificationResolutionClaim,
			)
	}

	if err := command.Validate(); err != nil {
		return incidentdomain.
				RecoveredVerificationResolutionClaim{},
			false,
			err
	}

	if err := ctx.Err(); err != nil {
		return incidentdomain.
				RecoveredVerificationResolutionClaim{},
			false,
			err
	}

	expiresAt := command.Now.Add(command.LeaseDuration)

	const query = `
WITH candidate AS MATERIALIZED (
	SELECT
		verification.id AS verification_id,
		verification.action_attempt_id,
		action_attempt.incident_id,
		action_attempt.plan_hash,
		action_attempt.target_uid,
		verification.subject_cluster,
		verification.subject_namespace,
		verification.subject_kind,
		verification.subject_name,
		verification.subject_uid,
		verification.status AS verification_status,
		verification.version AS verification_version,
		verification.started_at,
		verification.finished_at,
		COALESCE(
			verification.evidence_code,
			''
		) AS evidence_code
	FROM verifications AS verification
	INNER JOIN action_attempts AS action_attempt
		ON action_attempt.id =
			verification.action_attempt_id
	INNER JOIN incidents AS incident
		ON incident.id = action_attempt.incident_id
	WHERE verification.status = 'RECOVERED'
	  AND incident.state = 'VERIFYING'
	  AND incident.resolution_verification_id IS NULL
	  AND (
		(
			incident.claim_holder IS NULL
			AND incident.claim_expires_at IS NULL
		)
		OR incident.claim_expires_at <= $2
	  )
	ORDER BY
		verification.finished_at,
		verification.id
	FOR UPDATE OF incident, verification SKIP LOCKED
	LIMIT 1
),
claimed AS (
	UPDATE incidents AS incident
	SET
		version = incident.version + 1,
		claim_holder = $1,
		claim_expires_at = $3
	FROM candidate
	WHERE incident.id = candidate.incident_id
	RETURNING
		incident.id,
		incident.state,
		incident.version,
		COALESCE(
			incident.resolution_verification_id,
			''
		) AS resolution_verification_id,
		incident.claim_holder,
		incident.claim_expires_at
),
audited AS (
	INSERT INTO incident_claims (
		incident_id,
		incident_version,
		holder_id,
		acquired_at,
		expires_at
	)
	SELECT
		claimed.id,
		claimed.version,
		claimed.claim_holder,
		$2,
		claimed.claim_expires_at
	FROM claimed
	RETURNING
		incident_id,
		incident_version
)
SELECT
	candidate.verification_id,
	candidate.action_attempt_id,
	candidate.incident_id,
	candidate.plan_hash,
	candidate.target_uid,
	candidate.subject_cluster,
	candidate.subject_namespace,
	candidate.subject_kind,
	candidate.subject_name,
	candidate.subject_uid,
	candidate.verification_status,
	candidate.verification_version,
	candidate.started_at,
	candidate.finished_at,
	candidate.evidence_code,
	claimed.id,
	claimed.state,
	claimed.version,
	claimed.resolution_verification_id,
	claimed.claim_holder,
	claimed.claim_expires_at
FROM candidate
INNER JOIN claimed
	ON claimed.id = candidate.incident_id
INNER JOIN audited
	ON audited.incident_id = claimed.id
	AND audited.incident_version = claimed.version
`

	var result incidentdomain.RecoveredVerificationResolutionClaim

	found, err := scanVerificationClaimRow(
		registry.pool.QueryRow(
			ctx,
			query,
			command.HolderID,
			command.Now,
			expiresAt,
		),
		&result.Verification,
		&result.Claim,
		remediationdomain.VerificationStatusRecovered,
		true,
		"claim recovered Verification resolution",
	)
	if err != nil {
		return incidentdomain.RecoveredVerificationResolutionClaim{},
			false,
			err
	}
	if !found {
		return incidentdomain.RecoveredVerificationResolutionClaim{},
			false,
			nil
	}
	if result.Claim.Incident.ResolutionVerificationID != "" {
		return incidentdomain.
				RecoveredVerificationResolutionClaim{},
			false,
			fmt.Errorf(
				"claim recovered Verification resolution "+
					"returned resolved Incident %q",
				result.Claim.Incident.ID,
			)
	}

	return result, true, nil
}
