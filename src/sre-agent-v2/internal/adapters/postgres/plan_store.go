package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	approvaldomain "sre-agent/internal/approval"
)

type PlanStore struct {
	pool *pgxpool.Pool
}

var _ approvaldomain.PlanStore = (*PlanStore)(nil)

func NewPlanStore(pool *pgxpool.Pool) *PlanStore {
	return &PlanStore{
		pool: pool,
	}
}

func (store *PlanStore) Publish(
	ctx context.Context,
	plan approvaldomain.Plan,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := approvaldomain.ValidatePlan(plan); err != nil {
		return err
	}

	commandTag, err := store.pool.Exec(
		ctx,
		`
INSERT INTO incident_plans (
    incident_id,
    plan_hash,
    action,
    target_cluster,
    target_namespace,
    target_kind,
    target_name,
    target_uid
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (incident_id, plan_hash, target_uid)
DO NOTHING
`,
		plan.IncidentID,
		plan.Hash,
		plan.Action,
		plan.Target.Cluster,
		plan.Target.Namespace,
		plan.Target.Kind,
		plan.Target.Name,
		plan.Target.UID,
	)
	if err != nil {
		return fmt.Errorf("insert canonical plan: %w", err)
	}

	if commandTag.RowsAffected() == 1 {
		return nil
	}

	existing, err := store.Lookup(
		ctx,
		approvaldomain.PlanKey{
			IncidentID: plan.IncidentID,
			Hash:       plan.Hash,
			TargetUID:  plan.Target.UID,
		},
	)
	if err != nil {
		return fmt.Errorf(
			"read existing canonical plan after conflict: %w",
			err,
		)
	}

	if existing == plan {
		return nil
	}

	return fmt.Errorf(
		"%w: incident %q plan %q target UID %q",
		approvaldomain.ErrPlanConflict,
		plan.IncidentID,
		plan.Hash,
		plan.Target.UID,
	)
}

func (store *PlanStore) Lookup(
	ctx context.Context,
	key approvaldomain.PlanKey,
) (approvaldomain.Plan, error) {
	if err := ctx.Err(); err != nil {
		return approvaldomain.Plan{}, err
	}

	if err := key.Validate(); err != nil {
		return approvaldomain.Plan{}, err
	}

	var plan approvaldomain.Plan

	err := store.pool.QueryRow(
		ctx,
		`
SELECT
    incident_id,
    plan_hash,
    action,
    target_cluster,
    target_namespace,
    target_kind,
    target_name,
    target_uid
FROM incident_plans
WHERE incident_id = $1
  AND plan_hash = $2
  AND target_uid = $3
`,
		key.IncidentID,
		key.Hash,
		key.TargetUID,
	).Scan(
		&plan.IncidentID,
		&plan.Hash,
		&plan.Action,
		&plan.Target.Cluster,
		&plan.Target.Namespace,
		&plan.Target.Kind,
		&plan.Target.Name,
		&plan.Target.UID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return approvaldomain.Plan{}, fmt.Errorf(
			"%w: incident %q plan %q target UID %q",
			approvaldomain.ErrPlanNotFound,
			key.IncidentID,
			key.Hash,
			key.TargetUID,
		)
	}
	if err != nil {
		return approvaldomain.Plan{}, fmt.Errorf(
			"query canonical plan: %w",
			err,
		)
	}

	if err := approvaldomain.ValidatePlan(plan); err != nil {
		return approvaldomain.Plan{}, fmt.Errorf(
			"validate stored canonical plan: %w",
			err,
		)
	}

	return plan, nil
}
