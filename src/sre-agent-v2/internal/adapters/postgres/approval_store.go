package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	approvaldomain "sre-agent/internal/approval"
)

type ApprovalStore struct {
	pool *pgxpool.Pool
}

var _ approvaldomain.Store = (*ApprovalStore)(nil)

func NewApprovalStore(pool *pgxpool.Pool) *ApprovalStore {
	return &ApprovalStore{
		pool: pool,
	}
}

func (store *ApprovalStore) Grant(
	ctx context.Context,
	granted approvaldomain.Approval,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := approvaldomain.ValidateRecord(granted); err != nil {
		return err
	}

	commandTag, err := store.pool.Exec(
		ctx,
		`
			INSERT INTO approvals (
				incident_id,
				plan_hash,
				target_uid,
				approved_by,
				approved_at,
				expires_at
			)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (
				incident_id,
				plan_hash,
				target_uid
			)
			DO NOTHING
		`,
		granted.IncidentID,
		granted.PlanHash,
		granted.TargetUID,
		granted.ApprovedBy,
		granted.ApprovedAt,
		granted.ExpiresAt,
	)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		return fmt.Errorf("insert approval: %w", err)
	}

	if commandTag.RowsAffected() == 1 {
		return nil
	}

	existing, err := store.Lookup(
		ctx,
		approvaldomain.ApprovalKey{
			IncidentID: granted.IncidentID,
			PlanHash:   granted.PlanHash,
			TargetUID:  granted.TargetUID,
		},
	)
	if err != nil {
		return fmt.Errorf("lookup existing approval: %w", err)
	}

	if approvalRecordsEqual(existing, granted) {
		return nil
	}

	return approvaldomain.ErrApprovalConflict
}

func (store *ApprovalStore) Lookup(
	ctx context.Context,
	key approvaldomain.ApprovalKey,
) (approvaldomain.Approval, error) {
	if err := ctx.Err(); err != nil {
		return approvaldomain.Approval{}, err
	}

	if err := key.Validate(); err != nil {
		return approvaldomain.Approval{}, err
	}

	var granted approvaldomain.Approval

	err := store.pool.QueryRow(
		ctx,
		`
			SELECT
				incident_id,
				plan_hash,
				target_uid,
				approved_by,
				approved_at,
				expires_at
			FROM approvals
			WHERE incident_id = $1
			  AND plan_hash = $2
			  AND target_uid = $3
		`,
		key.IncidentID,
		key.PlanHash,
		key.TargetUID,
	).Scan(
		&granted.IncidentID,
		&granted.PlanHash,
		&granted.TargetUID,
		&granted.ApprovedBy,
		&granted.ApprovedAt,
		&granted.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return approvaldomain.Approval{},
			approvaldomain.ErrApprovalNotFound
	}
	if err != nil {
		if ctx.Err() != nil {
			return approvaldomain.Approval{}, ctx.Err()
		}

		return approvaldomain.Approval{},
			fmt.Errorf("lookup approval: %w", err)
	}

	return granted, nil
}

func approvalRecordsEqual(
	left approvaldomain.Approval,
	right approvaldomain.Approval,
) bool {
	return left.IncidentID == right.IncidentID &&
		left.PlanHash == right.PlanHash &&
		left.TargetUID == right.TargetUID &&
		left.ApprovedBy == right.ApprovedBy &&
		left.ApprovedAt.Equal(right.ApprovedAt) &&
		left.ExpiresAt.Equal(right.ExpiresAt)
}
