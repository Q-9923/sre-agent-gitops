package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func (registry *Registry) Resolve(
	ctx context.Context,
	command incidentdomain.ResolveCommand,
) (incidentdomain.Incident, error) {
	if err := ctx.Err(); err != nil {
		return incidentdomain.Incident{}, err
	}

	if err := command.Validate(); err != nil {
		return incidentdomain.Incident{}, err
	}

	transaction, err := registry.pool.BeginTx(
		ctx,
		pgx.TxOptions{},
	)
	if err != nil {
		return incidentdomain.Incident{}, fmt.Errorf(
			"begin incident resolution: %w",
			err,
		)
	}

	defer func() {
		rollbackContext, cancelRollback := context.WithTimeout(
			context.Background(),
			5*time.Second,
		)
		defer cancelRollback()

		_ = transaction.Rollback(rollbackContext)
	}()

	var (
		currentState   string
		currentVersion int64
	)

	err = transaction.QueryRow(
		ctx,
		`
SELECT
    state,
    version
FROM incidents
WHERE id = $1
FOR UPDATE
`,
		command.IncidentID,
	).Scan(
		&currentState,
		&currentVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return incidentdomain.Incident{}, fmt.Errorf(
			"%w: incident %q",
			incidentdomain.ErrIncidentNotFound,
			command.IncidentID,
		)
	}
	if err != nil {
		return incidentdomain.Incident{}, fmt.Errorf(
			"lock incident for resolution: %w",
			err,
		)
	}

	if currentVersion <= 0 ||
		uint64(currentVersion) != command.ExpectedVersion {
		return incidentdomain.Incident{}, fmt.Errorf(
			"%w: incident %q current=%d expected=%d",
			incidentdomain.ErrVersionConflict,
			command.IncidentID,
			currentVersion,
			command.ExpectedVersion,
		)
	}

	if incidentdomain.State(currentState) !=
		incidentdomain.StateVerifying {
		return incidentdomain.Incident{}, fmt.Errorf(
			"%w: incident %q from %q to %q",
			incidentdomain.ErrInvalidTransition,
			command.IncidentID,
			currentState,
			incidentdomain.StateResolved,
		)
	}

	var (
		verificationStatus     string
		verificationIncidentID string
	)

	err = transaction.QueryRow(
		ctx,
		`
SELECT
    verification.status,
    attempt.incident_id
FROM verifications AS verification
JOIN action_attempts AS attempt
    ON attempt.id = verification.action_attempt_id
WHERE verification.id = $1
FOR UPDATE OF verification, attempt
`,
		command.VerificationID,
	).Scan(
		&verificationStatus,
		&verificationIncidentID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return incidentdomain.Incident{}, fmt.Errorf(
			"%w: verification %q",
			remediationdomain.ErrVerificationNotFound,
			command.VerificationID,
		)
	}
	if err != nil {
		return incidentdomain.Incident{}, fmt.Errorf(
			"lock recovery Verification: %w",
			err,
		)
	}

	if verificationIncidentID != command.IncidentID {
		return incidentdomain.Incident{}, fmt.Errorf(
			"%w: verification %q does not belong to incident %q",
			remediationdomain.ErrVerificationNotFound,
			command.VerificationID,
			command.IncidentID,
		)
	}

	if remediationdomain.VerificationStatus(verificationStatus) !=
		remediationdomain.VerificationStatusRecovered {
		return incidentdomain.Incident{}, fmt.Errorf(
			"%w: verification %q status=%q",
			remediationdomain.ErrVerificationNotRecovered,
			command.VerificationID,
			verificationStatus,
		)
	}

	var resolvedVersion int64

	err = transaction.QueryRow(
		ctx,
		`
UPDATE incidents
SET
    state = 'RESOLVED',
    version = version + 1,
    resolved_at = statement_timestamp(),
    resolution_verification_id = $2,
    approval_plan_hash = NULL,
    approval_target_uid = NULL
WHERE id = $1
  AND state = 'VERIFYING'
  AND version = $3
RETURNING version
`,
		command.IncidentID,
		command.VerificationID,
		command.ExpectedVersion,
	).Scan(&resolvedVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return incidentdomain.Incident{}, fmt.Errorf(
			"%w: incident %q changed during resolution",
			incidentdomain.ErrVersionConflict,
			command.IncidentID,
		)
	}
	if err != nil {
		return incidentdomain.Incident{}, fmt.Errorf(
			"update resolved Incident: %w",
			err,
		)
	}

	_, err = transaction.Exec(
		ctx,
		`
INSERT INTO incident_transitions (
    incident_id,
    version,
    from_state,
    to_state,
    actor,
    reason_code
)
VALUES ($1, $2, $3, $4, $5, $6)
`,
		command.IncidentID,
		resolvedVersion,
		string(incidentdomain.StateVerifying),
		string(incidentdomain.StateResolved),
		command.Actor,
		command.ReasonCode,
	)
	if err != nil {
		return incidentdomain.Incident{}, fmt.Errorf(
			"insert incident resolution audit: %w",
			err,
		)
	}

	if err := transaction.Commit(ctx); err != nil {
		return incidentdomain.Incident{}, fmt.Errorf(
			"commit incident resolution: %w",
			err,
		)
	}

	if resolvedVersion <= 0 {
		return incidentdomain.Incident{}, fmt.Errorf(
			"incident resolution returned invalid version %d",
			resolvedVersion,
		)
	}

	return incidentdomain.Incident{
		ID:                       command.IncidentID,
		State:                    incidentdomain.StateResolved,
		Version:                  uint64(resolvedVersion),
		ResolutionVerificationID: command.VerificationID,
	}, nil
}
