package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

type verificationClaimScanTarget struct {
	verification        *remediationdomain.Verification
	claim               *incidentdomain.Claim
	verificationStatus  string
	verificationVersion int64
	incidentState       string
	incidentVersion     int64
}

func scanVerificationClaimRow(
	row pgx.Row,
	verification *remediationdomain.Verification,
	claim *incidentdomain.Claim,
	expectedStatus remediationdomain.VerificationStatus,
	includeResolutionVerification bool,
	operation string,
) (bool, error) {
	target := verificationClaimScanTarget{
		verification: verification,
		claim:        claim,
	}

	err := row.Scan(
		target.destinations(includeResolutionVerification)...,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s in PostgreSQL: %w", operation, err)
	}

	if err := target.restoreAndValidate(
		expectedStatus,
		operation,
	); err != nil {
		return false, err
	}

	return true, nil
}

func (target *verificationClaimScanTarget) destinations(
	includeResolutionVerification bool,
) []any {
	destinations := []any{
		&target.verification.ID,
		&target.verification.ActionAttemptID,
		&target.verification.ActionKey.IncidentID,
		&target.verification.ActionKey.PlanHash,
		&target.verification.ActionKey.TargetUID,
		&target.verification.Subject.Cluster,
		&target.verification.Subject.Namespace,
		&target.verification.Subject.Kind,
		&target.verification.Subject.Name,
		&target.verification.Subject.UID,
		&target.verificationStatus,
		&target.verificationVersion,
		&target.verification.StartedAt,
		&target.verification.FinishedAt,
		&target.verification.EvidenceCode,
		&target.claim.Incident.ID,
		&target.incidentState,
		&target.incidentVersion,
	}

	if includeResolutionVerification {
		destinations = append(
			destinations,
			&target.claim.Incident.ResolutionVerificationID,
		)
	}

	return append(
		destinations,
		&target.claim.HolderID,
		&target.claim.ExpiresAt,
	)
}

func (target *verificationClaimScanTarget) restoreAndValidate(
	expectedStatus remediationdomain.VerificationStatus,
	operation string,
) error {
	if target.verificationVersion <= 0 {
		return fmt.Errorf(
			"%s returned invalid Verification version %d",
			operation,
			target.verificationVersion,
		)
	}
	if target.incidentVersion <= 0 {
		return fmt.Errorf(
			"%s returned invalid Incident version %d",
			operation,
			target.incidentVersion,
		)
	}

	target.verification.Status = remediationdomain.VerificationStatus(
		target.verificationStatus,
	)
	target.verification.Version = target.verificationVersion
	target.claim.Incident.State = incidentdomain.State(target.incidentState)
	target.claim.Incident.Version = uint64(target.incidentVersion)

	if target.verification.Status != expectedStatus {
		return fmt.Errorf(
			"%s returned Verification status %q",
			operation,
			target.verification.Status,
		)
	}
	if target.claim.Incident.State != incidentdomain.StateVerifying {
		return fmt.Errorf(
			"%s returned Incident state %q",
			operation,
			target.claim.Incident.State,
		)
	}

	return nil
}
