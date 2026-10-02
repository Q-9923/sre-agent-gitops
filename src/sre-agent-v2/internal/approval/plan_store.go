package approval

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrInvalidPlanKey = errors.New("invalid plan key")
	ErrPlanNotFound   = errors.New("plan not found")
	ErrPlanConflict   = errors.New("plan conflict")
)

type PlanKey struct {
	IncidentID string
	Hash       string
	TargetUID  string
}

func (key PlanKey) Validate() error {
	if invalidIdentity(key.IncidentID) ||
		invalidIdentity(key.Hash) ||
		invalidIdentity(key.TargetUID) {
		return fmt.Errorf(
			"%w: incident ID, plan hash, and target UID are required",
			ErrInvalidPlanKey,
		)
	}

	return nil
}

type PlanStore interface {
	Publish(context.Context, Plan) error
	Lookup(context.Context, PlanKey) (Plan, error)
}

type MemoryPlanStore struct {
	mu    sync.RWMutex
	plans map[PlanKey]Plan
}

var _ PlanStore = (*MemoryPlanStore)(nil)

func NewMemoryPlanStore() *MemoryPlanStore {
	return &MemoryPlanStore{
		plans: make(map[PlanKey]Plan),
	}
}

func ValidatePlan(plan Plan) error {
	canonical, err := NewPlan(
		PlanCommand{
			IncidentID: plan.IncidentID,
			Action:     plan.Action,
			Target:     plan.Target,
		},
	)
	if err != nil {
		return err
	}

	if canonical != plan {
		return fmt.Errorf(
			"%w: canonical hash does not match plan content",
			ErrInvalidPlan,
		)
	}

	return nil
}

func (store *MemoryPlanStore) Publish(
	ctx context.Context,
	plan Plan,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := ValidatePlan(plan); err != nil {
		return err
	}

	key := PlanKey{
		IncidentID: plan.IncidentID,
		Hash:       plan.Hash,
		TargetUID:  plan.Target.UID,
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	if store.plans == nil {
		store.plans = make(map[PlanKey]Plan)
	}

	existing, exists := store.plans[key]
	if exists {
		if existing == plan {
			return nil
		}

		return fmt.Errorf(
			"%w: incident %q plan %q target UID %q",
			ErrPlanConflict,
			key.IncidentID,
			key.Hash,
			key.TargetUID,
		)
	}

	store.plans[key] = plan

	return nil
}

func (store *MemoryPlanStore) Lookup(
	ctx context.Context,
	key PlanKey,
) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}

	if err := key.Validate(); err != nil {
		return Plan{}, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}

	plan, exists := store.plans[key]
	if !exists {
		return Plan{}, fmt.Errorf(
			"%w: incident %q plan %q target UID %q",
			ErrPlanNotFound,
			key.IncidentID,
			key.Hash,
			key.TargetUID,
		)
	}

	return plan, nil
}
