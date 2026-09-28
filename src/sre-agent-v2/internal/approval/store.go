package approval

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrInvalidApprovalKey = errors.New("invalid approval key")
	ErrApprovalNotFound   = errors.New("approval not found")
	ErrApprovalConflict   = errors.New("approval conflict")
)

type ApprovalKey struct {
	IncidentID string
	PlanHash   string
	TargetUID  string
}

func (key ApprovalKey) Validate() error {
	if invalidIdentity(key.IncidentID) ||
		invalidIdentity(key.PlanHash) ||
		invalidIdentity(key.TargetUID) {
		return ErrInvalidApprovalKey
	}

	return nil
}

type Store interface {
	Grant(context.Context, Approval) error
	Lookup(context.Context, ApprovalKey) (Approval, error)
}

type MemoryStore struct {
	mu        sync.RWMutex
	approvals map[ApprovalKey]Approval
}

var _ Store = (*MemoryStore)(nil)

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		approvals: make(map[ApprovalKey]Approval),
	}
}

func (store *MemoryStore) Grant(
	ctx context.Context,
	granted Approval,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := ValidateRecord(granted); err != nil {
		return err
	}

	key := ApprovalKey{
		IncidentID: granted.IncidentID,
		PlanHash:   granted.PlanHash,
		TargetUID:  granted.TargetUID,
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	existing, exists := store.approvals[key]
	if exists {
		if approvalsEqual(existing, granted) {
			return nil
		}

		return ErrApprovalConflict
	}

	store.approvals[key] = granted

	return nil
}

func (store *MemoryStore) Lookup(
	ctx context.Context,
	key ApprovalKey,
) (Approval, error) {
	if err := ctx.Err(); err != nil {
		return Approval{}, err
	}

	if err := key.Validate(); err != nil {
		return Approval{}, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	if err := ctx.Err(); err != nil {
		return Approval{}, err
	}

	granted, exists := store.approvals[key]
	if !exists {
		return Approval{}, ErrApprovalNotFound
	}

	return granted, nil
}

func approvalsEqual(left Approval, right Approval) bool {
	return left.IncidentID == right.IncidentID &&
		left.PlanHash == right.PlanHash &&
		left.TargetUID == right.TargetUID &&
		left.ApprovedBy == right.ApprovedBy &&
		left.ApprovedAt.Equal(right.ApprovedAt) &&
		left.ExpiresAt.Equal(right.ExpiresAt)
}
