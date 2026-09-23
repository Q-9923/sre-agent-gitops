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
    id = $1
`

	var (
		result  incident.Incident
		state   string
		version int64
		created bool
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
UPDATE incidents
SET
    state = $1,
    version = version + 1,
    resolved_at = statement_timestamp()
WHERE id = $2
  AND version = $3
  AND state = 'DETECTED'
  AND $1 = 'RESOLVED'
RETURNING id, state, version
`

	var (
		result  incident.Incident
		state   string
		version int64
	)

	err = transaction.QueryRow(
		ctx,
		updateQuery,
		string(command.To),
		command.IncidentID,
		command.ExpectedVersion,
	).Scan(
		&result.ID,
		&state,
		&version,
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
		string(incident.StateDetected),
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
