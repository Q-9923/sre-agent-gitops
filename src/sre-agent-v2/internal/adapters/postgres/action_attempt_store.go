package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
    status,
    version,
    started_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (
    incident_id,
    plan_hash,
    target_uid
)
DO NOTHING
`,
		attempt.ID,
		attempt.Key.IncidentID,
		attempt.Key.PlanHash,
		attempt.Key.TargetUID,
		attempt.Key.FencingToken,
		string(attempt.Status),
		attempt.Version,
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
		command.Key.ActionKey(),
	)
	if err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	return existing, false, nil
}

func (store *ActionAttemptStore) Complete(
	ctx context.Context,
	command remediationdomain.CompleteActionAttemptCommand,
) (remediationdomain.ActionAttempt, error) {
	if err := ctx.Err(); err != nil {
		return remediationdomain.ActionAttempt{}, err
	}

	if err := command.Validate(); err != nil {
		return remediationdomain.ActionAttempt{}, err
	}

	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return remediationdomain.ActionAttempt{}, fmt.Errorf(
			"begin PostgreSQL action attempt completion: %w",
			err,
		)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	current, err := lookupActionAttemptForUpdate(
		ctx,
		tx,
		command.Key.ActionKey(),
	)
	if err != nil {
		return remediationdomain.ActionAttempt{}, err
	}

	if current.Key.FencingToken !=
		command.Key.FencingToken {
		return remediationdomain.ActionAttempt{}, fmt.Errorf(
			"%w: current=%d expected=%d",
			remediationdomain.
				ErrActionAttemptFencingConflict,
			current.Key.FencingToken,
			command.Key.FencingToken,
		)
	}

	if postgresCompletionMatches(current, command) {
		if err := tx.Commit(ctx); err != nil {
			return remediationdomain.ActionAttempt{},
				fmt.Errorf(
					"commit idempotent PostgreSQL action attempt completion: %w",
					err,
				)
		}

		return current, nil
	}

	if current.Version != command.ExpectedVersion {
		return remediationdomain.ActionAttempt{}, fmt.Errorf(
			"%w: current=%d expected=%d",
			remediationdomain.
				ErrActionAttemptVersionConflict,
			current.Version,
			command.ExpectedVersion,
		)
	}

	if current.Status !=
		remediationdomain.ActionAttemptStatusStarted {
		return remediationdomain.ActionAttempt{}, fmt.Errorf(
			"%w: current=%q requested=%q",
			remediationdomain.
				ErrInvalidActionAttemptTransition,
			current.Status,
			command.To,
		)
	}

	if command.FinishedAt.Before(current.StartedAt) {
		return remediationdomain.ActionAttempt{}, fmt.Errorf(
			"%w: finish time precedes start time",
			remediationdomain.ErrInvalidActionAttempt,
		)
	}

	var storedErrorCode any
	if command.ErrorCode != "" {
		storedErrorCode = command.ErrorCode
	}

	commandTag, err := tx.Exec(
		ctx,
		`
UPDATE action_attempts
SET
    status = $1,
    version = version + 1,
    finished_at = $2,
    error_code = $3,
    recovered_by_fencing_token = NULL
WHERE id = $4
  AND version = $5
  AND status = 'STARTED'
  AND fencing_token = $6
`,
		string(command.To),
		command.FinishedAt,
		storedErrorCode,
		current.ID,
		command.ExpectedVersion,
		command.Key.FencingToken,
	)
	if err != nil {
		return remediationdomain.ActionAttempt{}, fmt.Errorf(
			"complete PostgreSQL action attempt: %w",
			err,
		)
	}
	if commandTag.RowsAffected() != 1 {
		return remediationdomain.ActionAttempt{}, fmt.Errorf(
			"%w: concurrent completion",
			remediationdomain.
				ErrActionAttemptVersionConflict,
		)
	}

	finishedAt := command.FinishedAt

	current.Status = command.To
	current.Version++
	current.FinishedAt = &finishedAt
	current.ErrorCode = command.ErrorCode
	current.RecoveredByFencingToken = 0

	if err := tx.Commit(ctx); err != nil {
		return remediationdomain.ActionAttempt{}, fmt.Errorf(
			"commit PostgreSQL action attempt completion: %w",
			err,
		)
	}

	return current, nil
}

func (store *ActionAttemptStore) Recover(
	ctx context.Context,
	command remediationdomain.RecoverActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	if err := ctx.Err(); err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	if err := command.Validate(); err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return remediationdomain.ActionAttempt{}, false,
			fmt.Errorf(
				"begin PostgreSQL action attempt recovery: %w",
				err,
			)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	current, err := lookupActionAttemptForUpdate(
		ctx,
		tx,
		command.Key.ActionKey(),
	)
	if err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	if postgresRecoveryMatches(current, command) {
		if err := tx.Commit(ctx); err != nil {
			return remediationdomain.ActionAttempt{}, false,
				fmt.Errorf(
					"commit idempotent PostgreSQL action attempt recovery: %w",
					err,
				)
		}

		return current, false, nil
	}

	if current.Status !=
		remediationdomain.ActionAttemptStatusStarted {
		return remediationdomain.ActionAttempt{}, false,
			fmt.Errorf(
				"%w: current=%q requested=%q",
				remediationdomain.
					ErrInvalidActionAttemptTransition,
				current.Status,
				remediationdomain.
					ActionAttemptStatusUnknown,
			)
	}

	if command.Key.FencingToken <=
		current.Key.FencingToken {
		return remediationdomain.ActionAttempt{}, false,
			fmt.Errorf(
				"%w: recovery token=%d must exceed owner token=%d",
				remediationdomain.
					ErrActionAttemptFencingConflict,
				command.Key.FencingToken,
				current.Key.FencingToken,
			)
	}

	if command.RecoveredAt.Before(current.StartedAt) {
		return remediationdomain.ActionAttempt{}, false,
			fmt.Errorf(
				"%w: recovery time precedes start time",
				remediationdomain.
					ErrInvalidActionAttempt,
			)
	}

	commandTag, err := tx.Exec(
		ctx,
		`
UPDATE action_attempts
SET
    status = 'UNKNOWN',
    version = version + 1,
    finished_at = $1,
    error_code = $2,
    recovered_by_fencing_token = $3
WHERE id = $4
  AND version = $5
  AND status = 'STARTED'
  AND fencing_token < $3
`,
		command.RecoveredAt,
		command.ReasonCode,
		command.Key.FencingToken,
		current.ID,
		current.Version,
	)
	if err != nil {
		return remediationdomain.ActionAttempt{}, false,
			fmt.Errorf(
				"recover PostgreSQL action attempt: %w",
				err,
			)
	}
	if commandTag.RowsAffected() != 1 {
		return remediationdomain.ActionAttempt{}, false,
			fmt.Errorf(
				"%w: concurrent recovery",
				remediationdomain.
					ErrActionAttemptVersionConflict,
			)
	}

	recoveredAt := command.RecoveredAt

	current.Status =
		remediationdomain.ActionAttemptStatusUnknown
	current.Version++
	current.FinishedAt = &recoveredAt
	current.ErrorCode = command.ReasonCode
	current.RecoveredByFencingToken =
		command.Key.FencingToken

	if err := tx.Commit(ctx); err != nil {
		return remediationdomain.ActionAttempt{}, false,
			fmt.Errorf(
				"commit PostgreSQL action attempt recovery: %w",
				err,
			)
	}

	return current, true, nil
}

func (store *ActionAttemptStore) lookup(
	ctx context.Context,
	key remediationdomain.ActionKey,
) (remediationdomain.ActionAttempt, error) {
	attempt, err := scanActionAttempt(
		store.pool.QueryRow(
			ctx,
			`
SELECT
    id,
    incident_id,
    plan_hash,
    target_uid,
    fencing_token,
    status,
    version,
    started_at,
    finished_at,
    error_code,
    recovered_by_fencing_token
FROM action_attempts
WHERE incident_id = $1
  AND plan_hash = $2
  AND target_uid = $3
`,
			key.IncidentID,
			key.PlanHash,
			key.TargetUID,
		),
		key,
	)
	if err != nil {
		return remediationdomain.ActionAttempt{}, fmt.Errorf(
			"query PostgreSQL action attempt: %w",
			err,
		)
	}

	return attempt, nil
}

func lookupActionAttemptForUpdate(
	ctx context.Context,
	tx pgx.Tx,
	key remediationdomain.ActionKey,
) (remediationdomain.ActionAttempt, error) {
	attempt, err := scanActionAttempt(
		tx.QueryRow(
			ctx,
			`
SELECT
    id,
    incident_id,
    plan_hash,
    target_uid,
    fencing_token,
    status,
    version,
    started_at,
    finished_at,
    error_code,
    recovered_by_fencing_token
FROM action_attempts
WHERE incident_id = $1
  AND plan_hash = $2
  AND target_uid = $3
FOR UPDATE
`,
			key.IncidentID,
			key.PlanHash,
			key.TargetUID,
		),
		key,
	)
	if err != nil {
		return remediationdomain.ActionAttempt{}, fmt.Errorf(
			"lock PostgreSQL action attempt: %w",
			err,
		)
	}

	return attempt, nil
}

func scanActionAttempt(
	row pgx.Row,
	key remediationdomain.ActionKey,
) (remediationdomain.ActionAttempt, error) {
	var attempt remediationdomain.ActionAttempt
	var fencingToken int64
	var status string
	var version int64
	var startedAt time.Time
	var finishedAt pgtype.Timestamptz
	var errorCode pgtype.Text
	var recoveredByFencingToken pgtype.Int8

	err := row.Scan(
		&attempt.ID,
		&attempt.Key.IncidentID,
		&attempt.Key.PlanHash,
		&attempt.Key.TargetUID,
		&fencingToken,
		&status,
		&version,
		&startedAt,
		&finishedAt,
		&errorCode,
		&recoveredByFencingToken,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return remediationdomain.ActionAttempt{},
			fmt.Errorf(
				"%w: incident=%q plan=%q target_uid=%q",
				remediationdomain.
					ErrActionAttemptNotFound,
				key.IncidentID,
				key.PlanHash,
				key.TargetUID,
			)
	}
	if err != nil {
		return remediationdomain.ActionAttempt{}, err
	}

	if fencingToken <= 0 {
		return remediationdomain.ActionAttempt{},
			fmt.Errorf(
				"invalid stored action attempt fencing token %d",
				fencingToken,
			)
	}
	if version <= 0 {
		return remediationdomain.ActionAttempt{},
			fmt.Errorf(
				"invalid stored action attempt version %d",
				version,
			)
	}
	if recoveredByFencingToken.Valid &&
		recoveredByFencingToken.Int64 <= 0 {
		return remediationdomain.ActionAttempt{},
			fmt.Errorf(
				"invalid stored recovery fencing token %d",
				recoveredByFencingToken.Int64,
			)
	}

	attempt.Key.FencingToken = uint64(fencingToken)
	attempt.Status =
		remediationdomain.ActionAttemptStatus(status)
	attempt.Version = uint64(version)
	attempt.StartedAt = startedAt

	if finishedAt.Valid {
		storedFinishedAt := finishedAt.Time
		attempt.FinishedAt = &storedFinishedAt
	}
	if errorCode.Valid {
		attempt.ErrorCode = errorCode.String
	}
	if recoveredByFencingToken.Valid {
		attempt.RecoveredByFencingToken =
			uint64(recoveredByFencingToken.Int64)
	}

	return attempt, nil
}

func postgresCompletionMatches(
	current remediationdomain.ActionAttempt,
	command remediationdomain.CompleteActionAttemptCommand,
) bool {
	return current.Status == command.To &&
		current.Key.FencingToken ==
			command.Key.FencingToken &&
		current.FinishedAt != nil &&
		current.FinishedAt.Equal(command.FinishedAt) &&
		current.ErrorCode == command.ErrorCode &&
		current.RecoveredByFencingToken == 0
}

func postgresRecoveryMatches(
	current remediationdomain.ActionAttempt,
	command remediationdomain.RecoverActionAttemptCommand,
) bool {
	return current.Status ==
		remediationdomain.ActionAttemptStatusUnknown &&
		current.FinishedAt != nil &&
		current.FinishedAt.Equal(command.RecoveredAt) &&
		current.ErrorCode == command.ReasonCode &&
		current.RecoveredByFencingToken ==
			command.Key.FencingToken
}
