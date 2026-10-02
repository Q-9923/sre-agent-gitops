package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	remediationdomain "sre-agent/internal/remediation"
)

type ActionAttemptStore struct {
	pool *pgxpool.Pool
}

var _ remediationdomain.ActionAttemptStore = (*ActionAttemptStore)(nil)

func NewActionAttemptStore(
	pool *pgxpool.Pool,
) *ActionAttemptStore {
	return &ActionAttemptStore{
		pool: pool,
	}
}

func (store *ActionAttemptStore) Begin(
	ctx context.Context,
	command remediationdomain.BeginActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	if err := ctx.Err(); err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	attempt, err := remediationdomain.NewActionAttempt(command)
	if err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	commandTag, err := store.pool.Exec(
		ctx,
		`
INSERT INTO action_attempts (
    id,
    incident_id,
    plan_hash,
    target_uid,
    fencing_token,
    started_at
)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (
    incident_id,
    plan_hash,
    target_uid,
    fencing_token
)
DO NOTHING
`,
		attempt.ID,
		attempt.Key.IncidentID,
		attempt.Key.PlanHash,
		attempt.Key.TargetUID,
		attempt.Key.FencingToken,
		attempt.StartedAt,
	)
	if err != nil {
		return remediationdomain.ActionAttempt{}, false,
			fmt.Errorf(
				"begin PostgreSQL action attempt: %w",
				err,
			)
	}

	if commandTag.RowsAffected() == 1 {
		return attempt, true, nil
	}

	existing, err := store.lookup(
		ctx,
		command.Key,
	)
	if err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	return existing, false, nil
}

func (store *ActionAttemptStore) lookup(
	ctx context.Context,
	key remediationdomain.ExecutionKey,
) (remediationdomain.ActionAttempt, error) {
	var attempt remediationdomain.ActionAttempt
	var fencingToken uint64
	var startedAt time.Time

	err := store.pool.QueryRow(
		ctx,
		`
SELECT
    id,
    incident_id,
    plan_hash,
    target_uid,
    fencing_token,
    started_at
FROM action_attempts
WHERE incident_id = $1
  AND plan_hash = $2
  AND target_uid = $3
  AND fencing_token = $4
`,
		key.IncidentID,
		key.PlanHash,
		key.TargetUID,
		key.FencingToken,
	).Scan(
		&attempt.ID,
		&attempt.Key.IncidentID,
		&attempt.Key.PlanHash,
		&attempt.Key.TargetUID,
		&fencingToken,
		&startedAt,
	)
	if err != nil {
		return remediationdomain.ActionAttempt{},
			fmt.Errorf(
				"query PostgreSQL action attempt: %w",
				err,
			)
	}

	attempt.Key.FencingToken = fencingToken
	attempt.StartedAt = startedAt

	return attempt, nil
}
