package approval

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestMemoryStoreGrantAndLookupReturnsApproval(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	plan := mustNewPlan(t, validPlanCommand())
	granted := validApproval(plan)

	if err := store.Grant(context.Background(), granted); err != nil {
		t.Fatalf("Grant() error = %v; want nil", err)
	}

	got, err := store.Lookup(
		context.Background(),
		ApprovalKey{
			IncidentID: plan.IncidentID,
			PlanHash:   plan.Hash,
			TargetUID:  plan.Target.UID,
		},
	)
	if err != nil {
		t.Fatalf("Lookup() error = %v; want nil", err)
	}

	if got != granted {
		t.Fatalf("Lookup() = %#v; want %#v", got, granted)
	}
}

func TestMemoryStoreGrantIsIdempotent(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	plan := mustNewPlan(t, validPlanCommand())
	granted := validApproval(plan)

	if err := store.Grant(context.Background(), granted); err != nil {
		t.Fatalf("first Grant() error = %v; want nil", err)
	}

	if err := store.Grant(context.Background(), granted); err != nil {
		t.Fatalf("second Grant() error = %v; want nil", err)
	}
}

func TestMemoryStoreGrantRejectsConflictingApproval(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	plan := mustNewPlan(t, validPlanCommand())

	first := validApproval(plan)
	if err := store.Grant(context.Background(), first); err != nil {
		t.Fatalf("first Grant() error = %v; want nil", err)
	}

	conflicting := first
	conflicting.ApprovedBy = "operator-b"

	err := store.Grant(context.Background(), conflicting)
	if !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf(
			"conflicting Grant() error = %v; want ErrApprovalConflict",
			err,
		)
	}
}

func TestMemoryStoreLookupRequiresExactKey(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	plan := mustNewPlan(t, validPlanCommand())
	granted := validApproval(plan)

	if err := store.Grant(context.Background(), granted); err != nil {
		t.Fatalf("Grant() error = %v; want nil", err)
	}

	_, err := store.Lookup(
		context.Background(),
		ApprovalKey{
			IncidentID: plan.IncidentID,
			PlanHash:   plan.Hash,
			TargetUID:  "pod-uid-replaced",
		},
	)
	if !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf(
			"Lookup() error = %v; want ErrApprovalNotFound",
			err,
		)
	}
}

func TestMemoryStoreRejectsInvalidApproval(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	plan := mustNewPlan(t, validPlanCommand())
	granted := validApproval(plan)
	granted.ApprovedBy = " "

	err := store.Grant(context.Background(), granted)
	if !errors.Is(err, ErrInvalidApproval) {
		t.Fatalf(
			"Grant() error = %v; want ErrInvalidApproval",
			err,
		)
	}
}

func TestMemoryStoreRejectsInvalidLookupKey(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()

	_, err := store.Lookup(
		context.Background(),
		ApprovalKey{
			IncidentID: "inc-canonical-plan",
			PlanHash:   "sha256:plan",
		},
	)
	if !errors.Is(err, ErrInvalidApprovalKey) {
		t.Fatalf(
			"Lookup() error = %v; want ErrInvalidApprovalKey",
			err,
		)
	}
}

func TestMemoryStoreHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	plan := mustNewPlan(t, validPlanCommand())
	granted := validApproval(plan)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.Grant(ctx, granted)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf(
			"Grant() error = %v; want context.Canceled",
			err,
		)
	}
}

func TestMemoryStoreConcurrentGrantAllowsOneImmutableWinner(t *testing.T) {
	t.Parallel()

	const competitors = 32

	store := NewMemoryStore()
	plan := mustNewPlan(t, validPlanCommand())
	start := make(chan struct{})
	results := make(chan error, competitors)

	for index := 0; index < competitors; index++ {
		granted := validApproval(plan)
		granted.ApprovedBy = fmt.Sprintf("operator-%02d", index)

		go func(candidate Approval) {
			<-start
			results <- store.Grant(context.Background(), candidate)
		}(granted)
	}

	close(start)

	successes := 0
	conflicts := 0

	for index := 0; index < competitors; index++ {
		err := <-results

		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrApprovalConflict):
			conflicts++
		default:
			t.Fatalf("concurrent Grant() error = %v", err)
		}
	}

	if successes != 1 {
		t.Fatalf("successful Grant() calls = %d; want 1", successes)
	}

	if conflicts != competitors-1 {
		t.Fatalf(
			"conflicting Grant() calls = %d; want %d",
			conflicts,
			competitors-1,
		)
	}

	stored, err := store.Lookup(
		context.Background(),
		ApprovalKey{
			IncidentID: plan.IncidentID,
			PlanHash:   plan.Hash,
			TargetUID:  plan.Target.UID,
		},
	)
	if err != nil {
		t.Fatalf("Lookup() error = %v; want nil", err)
	}

	if stored.ApprovedBy == "" {
		t.Fatal("Lookup() returned an approval without an approver")
	}
}
