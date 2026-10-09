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

type verificationFence struct {
	IncidentID      string
	ExpectedVersion uint64
	HolderID        string
	Now             time.Time
}

func verificationFenceFromBegin(
	command incidentdomain.BeginFencedVerificationCommand,
) verificationFence {
	return verificationFence{
		IncidentID:      command.IncidentID,
		ExpectedVersion: command.ExpectedVersion,
		HolderID:        command.HolderID,
		Now:             command.Now,
	}
}

func verificationFenceFromComplete(
	command incidentdomain.CompleteFencedVerificationCommand,
) verificationFence {
	return verificationFence{
		IncidentID:      command.IncidentID,
		ExpectedVersion: command.ExpectedVersion,
		HolderID:        command.HolderID,
		Now:             command.Now,
	}
}

type verificationBeginQuerier interface {
	QueryRow(
		context.Context,
		string,
		...any,
	) pgx.Row
}

func (registry *Registry) BeginFencedVerification(
	ctx context.Context,
	command incidentdomain.BeginFencedVerificationCommand,
) (incidentdomain.FencedVerificationResult, error) {
	if registry == nil || registry.pool == nil {
		return incidentdomain.FencedVerificationResult{}, fmt.Errorf(
			"%w: PostgreSQL registry is not configured",
			incidentdomain.ErrVerificationLifecycleUnavailable,
		)
	}

	if ctx == nil {
		return incidentdomain.FencedVerificationResult{}, fmt.Errorf(
			"%w: context is required",
			incidentdomain.ErrInvalidFencedVerification,
		)
	}

	if err := command.Validate(); err != nil {
		return incidentdomain.FencedVerificationResult{}, err
	}

	if err := ctx.Err(); err != nil {
		return incidentdomain.FencedVerificationResult{}, err
	}

	transaction, err := registry.pool.BeginTx(
		ctx,
		pgx.TxOptions{},
	)
	if err != nil {
		return incidentdomain.FencedVerificationResult{}, fmt.Errorf(
			"begin PostgreSQL fenced Verification transaction: %w",
			err,
		)
	}
	defer func() {
		_ = transaction.Rollback(context.Background())
	}()

	current, err := lockIncidentForFencedVerification(
		ctx,
		transaction,
		verificationFenceFromBegin(command),
	)
	if err != nil {
		return incidentdomain.FencedVerificationResult{}, err
	}

	if err := requirePersistedTerminalActionAttempt(
		ctx,
		transaction,
		command,
	); err != nil {
		return incidentdomain.FencedVerificationResult{}, err
	}

	next, stateChanged, err := transitionIncidentToVerifyingInTransaction(
		ctx,
		transaction,
		current,
		command,
	)
	if err != nil {
		return incidentdomain.FencedVerificationResult{}, err
	}

	if stateChanged {
		if err := insertFencedVerificationTransitionAudit(
			ctx,
			transaction,
			current,
			next,
			command,
		); err != nil {
			return incidentdomain.FencedVerificationResult{}, err
		}
	}

	verification, created, err := beginVerificationWithQuerier(
		ctx,
		transaction,
		command.Verification,
	)
	if err != nil {
		return incidentdomain.FencedVerificationResult{}, fmt.Errorf(
			"begin PostgreSQL fenced Verification: %w",
			err,
		)
	}

	if verification.Status !=
		remediationdomain.VerificationStatusPending {
		return incidentdomain.FencedVerificationResult{}, fmt.Errorf(
			"%w: verification %q has status %q",
			incidentdomain.ErrInvalidFencedVerification,
			verification.ID,
			verification.Status,
		)
	}

	if err := transaction.Commit(ctx); err != nil {
		return incidentdomain.FencedVerificationResult{}, fmt.Errorf(
			"commit PostgreSQL fenced Verification transaction: %w",
			err,
		)
	}

	return incidentdomain.FencedVerificationResult{
		Incident:     next,
		Verification: verification,
		Created:      created,
	}, nil
}

func lockIncidentForFencedVerification(
	ctx context.Context,
	transaction pgx.Tx,
	fence verificationFence,
) (incidentdomain.Incident, error) {
	var (
		current             incidentdomain.Incident
		state               string
		version             int64
		claimHolder         string
		claimExpiresAt      time.Time
		currentClaimAudited bool
	)

	err := transaction.QueryRow(
		ctx,
		`
SELECT
	incident.id,
	incident.state,
	incident.version,
	COALESCE(incident.approval_plan_hash, ''),
	COALESCE(incident.approval_target_uid, ''),
	COALESCE(incident.claim_holder, ''),
	COALESCE(
		incident.claim_expires_at,
		'epoch'::timestamptz
	),
	EXISTS (
		SELECT 1
		FROM incident_claims AS incident_claim
		WHERE
			incident_claim.incident_id = incident.id
			AND incident_claim.incident_version =
				incident.version
			AND incident_claim.holder_id =
				incident.claim_holder
			AND incident_claim.expires_at =
				incident.claim_expires_at
	)
FROM incidents AS incident
WHERE incident.id = $1
FOR UPDATE OF incident
`,
		fence.IncidentID,
	).Scan(
		&current.ID,
		&state,
		&version,
		&current.ApprovalBinding.PlanHash,
		&current.ApprovalBinding.TargetUID,
		&claimHolder,
		&claimExpiresAt,
		&currentClaimAudited,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return incidentdomain.Incident{}, fmt.Errorf(
			"%w: incident %q",
			incidentdomain.ErrVerificationIncidentNotFound,
			fence.IncidentID,
		)
	}
	if err != nil {
		return incidentdomain.Incident{}, fmt.Errorf(
			"lock PostgreSQL Incident for fenced Verification: %w",
			err,
		)
	}

	current.State = incidentdomain.State(state)
	current.Version = uint64(version)

	if current.Version != fence.ExpectedVersion {
		return incidentdomain.Incident{}, fmt.Errorf(
			"%w: incident %q current=%d expected=%d",
			incidentdomain.ErrVersionConflict,
			fence.IncidentID,
			current.Version,
			fence.ExpectedVersion,
		)
	}

	if claimHolder != fence.HolderID {
		return incidentdomain.Incident{}, fmt.Errorf(
			"%w: incident %q is held by %q, not %q",
			incidentdomain.ErrVerificationFenceConflict,
			fence.IncidentID,
			claimHolder,
			fence.HolderID,
		)
	}

	if !currentClaimAudited {
		return incidentdomain.Incident{}, fmt.Errorf(
			"%w: incident %q version %d has no matching claim audit",
			incidentdomain.ErrVerificationFenceConflict,
			fence.IncidentID,
			fence.ExpectedVersion,
		)
	}

	if !fence.Now.Before(claimExpiresAt) {
		return incidentdomain.Incident{}, fmt.Errorf(
			"%w: claim for incident %q expired at %s",
			incidentdomain.ErrVerificationFenceConflict,
			fence.IncidentID,
			claimExpiresAt,
		)
	}

	switch current.State {
	case incidentdomain.StateDiagnosed,
		incidentdomain.StateWaitingApproval,
		incidentdomain.StateVerifying:
		return current, nil

	default:
		return incidentdomain.Incident{}, fmt.Errorf(
			"%w: incident %q is in state %q",
			incidentdomain.ErrInvalidVerificationIncidentState,
			fence.IncidentID,
			current.State,
		)
	}
}
func requirePersistedTerminalActionAttempt(
	ctx context.Context,
	transaction pgx.Tx,
	command incidentdomain.BeginFencedVerificationCommand,
) error {
	attempt := command.Verification.ActionAttempt

	var persistedStatus string

	err := transaction.QueryRow(
		ctx,
		`
SELECT status
FROM action_attempts
WHERE
	id = $1
	AND incident_id = $2
	AND plan_hash = $3
	AND target_uid = $4
FOR SHARE
`,
		attempt.ID,
		attempt.Key.IncidentID,
		attempt.Key.PlanHash,
		attempt.Key.TargetUID,
	).Scan(&persistedStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf(
			"%w: persisted action attempt %q does not match "+
				"the requested Action Key",
			incidentdomain.ErrInvalidFencedVerification,
			attempt.ID,
		)
	}
	if err != nil {
		return fmt.Errorf(
			"lock PostgreSQL Action Attempt for Verification: %w",
			err,
		)
	}

	if persistedStatus != string(attempt.Status) {
		return fmt.Errorf(
			"%w: action attempt %q persisted status=%q requested=%q",
			incidentdomain.ErrInvalidFencedVerification,
			attempt.ID,
			persistedStatus,
			attempt.Status,
		)
	}

	switch remediationdomain.ActionAttemptStatus(persistedStatus) {
	case remediationdomain.ActionAttemptStatusSucceeded,
		remediationdomain.ActionAttemptStatusFailed,
		remediationdomain.ActionAttemptStatusUnknown:
		return nil

	default:
		return fmt.Errorf(
			"%w: action attempt %q has non-terminal status %q",
			incidentdomain.ErrInvalidFencedVerification,
			attempt.ID,
			persistedStatus,
		)
	}
}

func transitionIncidentToVerifyingInTransaction(
	ctx context.Context,
	transaction pgx.Tx,
	current incidentdomain.Incident,
	command incidentdomain.BeginFencedVerificationCommand,
) (incidentdomain.Incident, bool, error) {
	if current.State == incidentdomain.StateVerifying {
		return current, false, nil
	}

	var nextVersion int64

	err := transaction.QueryRow(
		ctx,
		`
UPDATE incidents
SET
	state = $1,
	version = version + 1
WHERE
	id = $2
	AND version = $3
	AND state = $4
RETURNING version
`,
		incidentdomain.StateVerifying,
		current.ID,
		current.Version,
		current.State,
	).Scan(&nextVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return incidentdomain.Incident{}, false, fmt.Errorf(
			"%w: incident %q changed while entering VERIFYING",
			incidentdomain.ErrVersionConflict,
			current.ID,
		)
	}
	if err != nil {
		return incidentdomain.Incident{}, false, fmt.Errorf(
			"transition PostgreSQL Incident to VERIFYING: %w",
			err,
		)
	}

	next := current
	next.State = incidentdomain.StateVerifying
	next.Version = uint64(nextVersion)

	return next, true, nil
}

func insertFencedVerificationTransitionAudit(
	ctx context.Context,
	transaction pgx.Tx,
	current incidentdomain.Incident,
	next incidentdomain.Incident,
	command incidentdomain.BeginFencedVerificationCommand,
) error {
	_, err := transaction.Exec(
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
		next.ID,
		next.Version,
		current.State,
		next.State,
		command.HolderID,
		command.ReasonCode,
	)
	if err != nil {
		return fmt.Errorf(
			"insert PostgreSQL fenced Verification transition audit: %w",
			err,
		)
	}

	return nil
}

func beginVerificationWithQuerier(
	ctx context.Context,
	querier verificationBeginQuerier,
	command remediationdomain.BeginVerificationCommand,
) (remediationdomain.Verification, bool, error) {
	if ctx == nil {
		return remediationdomain.Verification{}, false,
			remediationdomain.ErrInvalidVerification
	}

	if err := ctx.Err(); err != nil {
		return remediationdomain.Verification{}, false, err
	}

	candidate, err := remediationdomain.NewVerification(command)
	if err != nil {
		return remediationdomain.Verification{}, false, err
	}

	var insertedID string

	err = querier.QueryRow(
		ctx,
		`
INSERT INTO verifications (
	id,
	action_attempt_id,
	subject_cluster,
	subject_namespace,
	subject_kind,
	subject_name,
	subject_uid,
	status,
	version,
	started_at,
	finished_at,
	evidence_code
)
VALUES (
	$1,
	$2,
	$3,
	$4,
	$5,
	$6,
	$7,
	$8,
	$9,
	$10,
	NULL,
	NULL
)
ON CONFLICT (action_attempt_id) DO NOTHING
RETURNING id
`,
		candidate.ID,
		candidate.ActionAttemptID,
		candidate.Subject.Cluster,
		candidate.Subject.Namespace,
		candidate.Subject.Kind,
		candidate.Subject.Name,
		candidate.Subject.UID,
		candidate.Status,
		candidate.Version,
		candidate.StartedAt,
	).Scan(&insertedID)
	if err == nil {
		return candidate, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return remediationdomain.Verification{}, false, fmt.Errorf(
			"begin PostgreSQL verification: %w",
			err,
		)
	}

	existing, err := lookupVerificationByActionAttemptIDWithQuerier(
		ctx,
		querier,
		command.ActionAttempt.ID,
	)
	if err != nil {
		return remediationdomain.Verification{}, false, err
	}

	if existing.Subject != command.Subject {
		return remediationdomain.Verification{}, false, fmt.Errorf(
			"%w: action attempt %q is already bound to another subject",
			remediationdomain.ErrVerificationSubjectConflict,
			command.ActionAttempt.ID,
		)
	}

	return existing, false, nil
}

func lookupVerificationByActionAttemptIDWithQuerier(
	ctx context.Context,
	querier verificationBeginQuerier,
	actionAttemptID string,
) (remediationdomain.Verification, error) {
	row := querier.QueryRow(
		ctx,
		`
SELECT
	verification.id,
	verification.action_attempt_id,
	action_attempt.incident_id,
	action_attempt.plan_hash,
	action_attempt.target_uid,
	verification.subject_cluster,
	verification.subject_namespace,
	verification.subject_kind,
	verification.subject_name,
	verification.subject_uid,
	verification.status,
	verification.version,
	verification.started_at,
	verification.finished_at,
	COALESCE(verification.evidence_code, '')
FROM verifications AS verification
INNER JOIN action_attempts AS action_attempt
	ON action_attempt.id = verification.action_attempt_id
WHERE verification.action_attempt_id = $1
`,
		actionAttemptID,
	)

	verification, err := scanVerification(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return remediationdomain.Verification{}, fmt.Errorf(
			"%w: action attempt %q",
			remediationdomain.ErrVerificationNotFound,
			actionAttemptID,
		)
	}
	if err != nil {
		return remediationdomain.Verification{}, fmt.Errorf(
			"lookup PostgreSQL verification by action attempt: %w",
			err,
		)
	}

	return verification, nil
}
