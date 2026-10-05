package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"sre-agent/internal/incident"
	"time"
)

type Registry struct {
	pool *pgxpool.Pool
}

func NewRegistry(pool *pgxpool.Pool) *Registry {
	return &Registry{
		pool: pool,
	}
}

func (registry *Registry) Observe(
	ctx context.Context,
	observation incident.Observation,
) (incident.Incident, bool, error) {
	if err := ctx.Err(); err != nil {
		return incident.Incident{}, false, err
	}

	idempotencyKey, err := observation.IdempotencyKey()
	if err != nil {
		return incident.Incident{}, false, err
	}

	candidateID, err := newIncidentID()
	if err != nil {
		return incident.Incident{}, false, err
	}

	const query = `
INSERT INTO incidents (
    id,
    idempotency_key,
    source,
    cluster,
    alert_name,
    target_kind,
    target_namespace,
    target_name,
    target_uid,
    state,
    version
)
VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, 'DETECTED', 1
)
ON CONFLICT (idempotency_key)
WHERE resolved_at IS NULL
DO UPDATE SET
    idempotency_key = incidents.idempotency_key
RETURNING
    id,
    state,
    version,
    COALESCE(approval_plan_hash, ''),
    COALESCE(approval_target_uid, ''),
    id = $1
`

	var (
		result            incident.Incident
		state             string
		version           int64
		approvalPlanHash  string
		approvalTargetUID string
		created           bool
	)

	err = registry.pool.QueryRow(
		ctx,
		query,
		candidateID,
		idempotencyKey,
		observation.Source,
		observation.Cluster,
		observation.AlertName,
		observation.Target.Kind,
		observation.Target.Namespace,
		observation.Target.Name,
		observation.Target.UID,
	).Scan(
		&result.ID,
		&state,
		&version,
		&approvalPlanHash,
		&approvalTargetUID,
		&created,
	)
	if err != nil {
		return incident.Incident{}, false, fmt.Errorf(
			"observe incident in PostgreSQL: %w",
			err,
		)
	}

	if version <= 0 {
		return incident.Incident{}, false, fmt.Errorf(
			"observe incident returned invalid version %d",
			version,
		)
	}

	result.State = incident.State(state)
	result.Version = uint64(version)
	result.ApprovalBinding = incident.ApprovalBinding{
		PlanHash:  approvalPlanHash,
		TargetUID: approvalTargetUID,
	}

	return result, created, nil
}

func newIncidentID() (string, error) {
	var randomBytes [12]byte

	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", fmt.Errorf(
			"generate incident ID: %w",
			err,
		)
	}

	return fmt.Sprintf("inc-%x", randomBytes), nil
}

func (registry *Registry) Claim(
	ctx context.Context,
	command incident.ClaimCommand,
) (incident.Claim, error) {
	if err := ctx.Err(); err != nil {
		return incident.Claim{}, err
	}
	if err := command.Validate(); err != nil {
		return incident.Claim{}, err
	}

	expiresAt := command.Now.Add(command.LeaseDuration)

	const query = `
WITH claimed AS (
    UPDATE incidents
    SET
        version = version + 1,
        claim_holder = $1,
        claim_expires_at = $2
    WHERE id = $3
      AND version = $4
      AND (
          claim_holder IS NULL
          OR claim_holder = $1
          OR claim_expires_at <= $5
      )
    RETURNING
        id,
        state,
        version,
        claim_holder,
        claim_expires_at
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
        id,
        version,
        claim_holder,
        $5,
        claim_expires_at
    FROM claimed
    RETURNING
        incident_id,
        incident_version
)
SELECT
    claimed.id,
    claimed.state,
    claimed.version,
    claimed.claim_holder,
    claimed.claim_expires_at
FROM claimed
JOIN audited
  ON audited.incident_id = claimed.id
 AND audited.incident_version = claimed.version
`
	var (
		result             incident.Incident
		state              string
		version            int64
		persistedHolder    string
		persistedExpiresAt time.Time
	)

	err := registry.pool.QueryRow(
		ctx,
		query,
		command.HolderID,
		expiresAt,
		command.IncidentID,
		command.ExpectedVersion,
		command.Now,
	).Scan(
		&result.ID,
		&state,
		&version,
		&persistedHolder,
		&persistedExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return incident.Claim{}, registry.explainClaimFailure(
			ctx,
			command,
		)
	}
	if err != nil {
		return incident.Claim{}, fmt.Errorf(
			"claim incident in PostgreSQL: %w",
			err,
		)
	}

	if version <= 0 {
		return incident.Claim{}, fmt.Errorf(
			"claim incident returned invalid version %d",
			version,
		)
	}

	result.State = incident.State(state)
	result.Version = uint64(version)

	return incident.Claim{
		Incident:  result,
		HolderID:  persistedHolder,
		ExpiresAt: persistedExpiresAt,
	}, nil
}

func (registry *Registry) ClaimHistory(
	ctx context.Context,
	incidentID string,
) ([]incident.ClaimAuditEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	const query = `
SELECT
    incident_id,
    incident_version,
    holder_id,
    acquired_at,
    expires_at
FROM incident_claims
WHERE incident_id = $1
ORDER BY incident_version
`

	rows, err := registry.pool.Query(
		ctx,
		query,
		incidentID,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"query incident Claim history: %w",
			err,
		)
	}
	defer rows.Close()

	history := make([]incident.ClaimAuditEvent, 0)

	for rows.Next() {
		var (
			event   incident.ClaimAuditEvent
			version int64
		)

		if err := rows.Scan(
			&event.IncidentID,
			&version,
			&event.HolderID,
			&event.AcquiredAt,
			&event.ExpiresAt,
		); err != nil {
			return nil, fmt.Errorf(
				"scan incident Claim history: %w",
				err,
			)
		}

		if version <= 0 {
			return nil, fmt.Errorf(
				"incident Claim history returned invalid version %d",
				version,
			)
		}

		event.IncidentVersion = uint64(version)
		history = append(history, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterate incident Claim history: %w",
			err,
		)
	}

	return history, nil
}

func (registry *Registry) explainClaimFailure(
	ctx context.Context,
	command incident.ClaimCommand,
) error {
	var (
		currentVersion   int64
		currentHolder    string
		currentExpiresAt time.Time
	)

	err := registry.pool.QueryRow(
		ctx,
		`
SELECT
    version,
    COALESCE(claim_holder, ''),
    COALESCE(claim_expires_at, 'epoch'::timestamptz)
FROM incidents
WHERE id = $1
`,
		command.IncidentID,
	).Scan(
		&currentVersion,
		&currentHolder,
		&currentExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf(
			"incident %q was not found",
			command.IncidentID,
		)
	}
	if err != nil {
		return fmt.Errorf(
			"inspect failed incident claim: %w",
			err,
		)
	}

	if currentVersion <= 0 ||
		uint64(currentVersion) != command.ExpectedVersion {
		return fmt.Errorf(
			"%w: incident %q current=%d expected=%d",
			incident.ErrVersionConflict,
			command.IncidentID,
			currentVersion,
			command.ExpectedVersion,
		)
	}

	if currentHolder != "" &&
		currentHolder != command.HolderID &&
		command.Now.Before(currentExpiresAt) {
		return fmt.Errorf(
			"%w: incident %q is held by %q until %s",
			incident.ErrLeaseHeld,
			command.IncidentID,
			currentHolder,
			currentExpiresAt.UTC().Format(time.RFC3339Nano),
		)
	}

	return fmt.Errorf(
		"incident %q claim was not updated",
		command.IncidentID,
	)
}

func (registry *Registry) Transition(
	ctx context.Context,
	command incident.TransitionCommand,
) (incident.Incident, error) {
	if err := ctx.Err(); err != nil {
		return incident.Incident{}, err
	}

	if err := command.Validate(); err != nil {
		return incident.Incident{}, err
	}

	transaction, err := registry.pool.BeginTx(
		ctx,
		pgx.TxOptions{},
	)
	if err != nil {
		return incident.Incident{}, fmt.Errorf(
			"begin incident transition: %w",
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

	const updateQuery = `
WITH current AS MATERIALIZED (
    SELECT
        id,
        state
    FROM incidents
    WHERE id = $2
      AND version = $3
    FOR UPDATE
)
UPDATE incidents AS updated
SET
    state = $1,
    version = updated.version + 1,
    resolved_at = CASE
        WHEN $1 = 'RESOLVED'
            THEN statement_timestamp()
        ELSE NULL
    END,
approval_plan_hash = CASE
    WHEN $1 = 'WAITING_APPROVAL'
        THEN $4
    WHEN $1 = 'VERIFYING'
        THEN updated.approval_plan_hash
    ELSE NULL
END,
approval_target_uid = CASE
    WHEN $1 = 'WAITING_APPROVAL'
        THEN $5
    WHEN $1 = 'VERIFYING'
        THEN updated.approval_target_uid
    ELSE NULL
END
FROM current
WHERE updated.id = current.id
AND (
    (
        current.state = 'DETECTED'
        AND $1 IN (
            'DIAGNOSED',
            'RESOLVED'
        )
    )
    OR
    (
        current.state = 'DIAGNOSED'
        AND $1 IN (
            'WAITING_APPROVAL',
            'VERIFYING',
            'RESOLVED'
        )
    )
    OR
    (
        current.state = 'WAITING_APPROVAL'
        AND $1 = 'VERIFYING'
    )
)
RETURNING
    updated.id,
    updated.state,
    updated.version,
    COALESCE(updated.approval_plan_hash, ''),
    COALESCE(updated.approval_target_uid, ''),
    current.state
`

	var (
		result            incident.Incident
		state             string
		fromState         string
		version           int64
		approvalPlanHash  string
		approvalTargetUID string
	)

	err = transaction.QueryRow(
		ctx,
		updateQuery,
		string(command.To),
		command.IncidentID,
		command.ExpectedVersion,
		command.ApprovalBinding.PlanHash,
		command.ApprovalBinding.TargetUID,
	).Scan(
		&result.ID,
		&state,
		&version,
		&approvalPlanHash,
		&approvalTargetUID,
		&fromState,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return incident.Incident{}, explainTransitionFailure(
			ctx,
			transaction,
			command,
		)
	}
	if err != nil {
		return incident.Incident{}, fmt.Errorf(
			"update incident transition: %w",
			err,
		)
	}

	const auditQuery = `
INSERT INTO incident_transitions (
    incident_id,
    version,
    from_state,
    to_state,
    actor,
    reason_code
)
VALUES ($1, $2, $3, $4, $5, $6)
`

	_, err = transaction.Exec(
		ctx,
		auditQuery,
		result.ID,
		version,
		fromState,
		state,
		command.Actor,
		command.ReasonCode,
	)
	if err != nil {
		return incident.Incident{}, fmt.Errorf(
			"insert incident transition audit: %w",
			err,
		)
	}

	if err := transaction.Commit(ctx); err != nil {
		return incident.Incident{}, fmt.Errorf(
			"commit incident transition: %w",
			err,
		)
	}

	if version <= 0 {
		return incident.Incident{}, fmt.Errorf(
			"transition returned invalid version %d",
			version,
		)
	}

	result.State = incident.State(state)
	result.Version = uint64(version)
	result.ApprovalBinding = incident.ApprovalBinding{
		PlanHash:  approvalPlanHash,
		TargetUID: approvalTargetUID,
	}

	return result, nil
}

func explainTransitionFailure(
	ctx context.Context,
	transaction pgx.Tx,
	command incident.TransitionCommand,
) error {
	var (
		currentState   string
		currentVersion int64
	)

	err := transaction.QueryRow(
		ctx,
		`
SELECT state, version
FROM incidents
WHERE id = $1
`,
		command.IncidentID,
	).Scan(
		&currentState,
		&currentVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf(
			"incident %q was not found",
			command.IncidentID,
		)
	}
	if err != nil {
		return fmt.Errorf(
			"inspect failed incident transition: %w",
			err,
		)
	}

	if currentVersion < 0 ||
		uint64(currentVersion) != command.ExpectedVersion {
		return fmt.Errorf(
			"%w: incident %q current=%d expected=%d",
			incident.ErrVersionConflict,
			command.IncidentID,
			currentVersion,
			command.ExpectedVersion,
		)
	}

	return fmt.Errorf(
		"%w: incident %q from %q to %q",
		incident.ErrInvalidTransition,
		command.IncidentID,
		currentState,
		command.To,
	)
}
