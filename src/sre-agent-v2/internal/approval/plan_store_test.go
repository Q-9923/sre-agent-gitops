package approval

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMemoryPlanStorePublishesAndLooksUpCanonicalPlan(t *testing.T) {
	t.Parallel()

	store := NewMemoryPlanStore()
	plan := newPlanStoreTestPlan(t)
	key := PlanKey{
		IncidentID: plan.IncidentID,
		Hash:       plan.Hash,
		TargetUID:  plan.Target.UID,
	}

	if err := store.Publish(context.Background(), plan); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	stored, err := store.Lookup(context.Background(), key)
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if stored != plan {
		t.Fatalf("Lookup() plan = %#v; want %#v", stored, plan)
	}
}

func TestMemoryPlanStoreRepeatedPublishIsIdempotent(t *testing.T) {
	t.Parallel()

	store := NewMemoryPlanStore()
	plan := newPlanStoreTestPlan(t)

	for attempt := 0; attempt < 2; attempt++ {
		if err := store.Publish(context.Background(), plan); err != nil {
			t.Fatalf(
				"Publish() attempt %d error = %v",
				attempt+1,
				err,
			)
		}
	}
}

func TestMemoryPlanStoreConcurrentRepeatedPublishIsIdempotent(
	t *testing.T,
) {
	t.Parallel()

	store := NewMemoryPlanStore()
	plan := newPlanStoreTestPlan(t)

	const workers = 32

	start := make(chan struct{})
	results := make(chan error, workers)

	var waitGroup sync.WaitGroup
	waitGroup.Add(workers)

	for range workers {
		go func() {
			defer waitGroup.Done()
			<-start

			results <- store.Publish(
				context.Background(),
				plan,
			)
		}()
	}

	close(start)
	waitGroup.Wait()
	close(results)

	for err := range results {
		if err != nil {
			t.Fatalf("concurrent Publish() error = %v", err)
		}
	}

	stored, err := store.Lookup(
		context.Background(),
		PlanKey{
			IncidentID: plan.IncidentID,
			Hash:       plan.Hash,
			TargetUID:  plan.Target.UID,
		},
	)
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if stored != plan {
		t.Fatalf("Lookup() plan = %#v; want %#v", stored, plan)
	}
}

func TestMemoryPlanStoreRejectsTamperedPlan(t *testing.T) {
	t.Parallel()

	store := NewMemoryPlanStore()
	plan := newPlanStoreTestPlan(t)
	plan.Action = "SCALE_WORKLOAD"

	err := store.Publish(context.Background(), plan)
	if !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("Publish() error = %v; want ErrInvalidPlan", err)
	}
}

func TestMemoryPlanStoreRejectsInvalidKey(t *testing.T) {
	t.Parallel()

	store := NewMemoryPlanStore()

	_, err := store.Lookup(
		context.Background(),
		PlanKey{
			IncidentID: "inc-plan-store",
			Hash:       "",
			TargetUID:  "pod-uid-plan-store",
		},
	)
	if !errors.Is(err, ErrInvalidPlanKey) {
		t.Fatalf(
			"Lookup() error = %v; want ErrInvalidPlanKey",
			err,
		)
	}
}

func TestMemoryPlanStoreReturnsNotFound(t *testing.T) {
	t.Parallel()

	store := NewMemoryPlanStore()
	plan := newPlanStoreTestPlan(t)

	_, err := store.Lookup(
		context.Background(),
		PlanKey{
			IncidentID: plan.IncidentID,
			Hash:       plan.Hash,
			TargetUID:  plan.Target.UID,
		},
	)
	if !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("Lookup() error = %v; want ErrPlanNotFound", err)
	}
}

func TestMemoryPlanStoreHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	store := NewMemoryPlanStore()
	plan := newPlanStoreTestPlan(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := store.Publish(ctx, plan); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Fatalf("Publish() error = %v; want context.Canceled", err)
	}

	_, err := store.Lookup(
		ctx,
		PlanKey{
			IncidentID: plan.IncidentID,
			Hash:       plan.Hash,
			TargetUID:  plan.Target.UID,
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Lookup() error = %v; want context.Canceled", err)
	}
}

func newPlanStoreTestPlan(t *testing.T) Plan {
	t.Helper()

	plan, err := NewPlan(
		PlanCommand{
			IncidentID: "inc-plan-store",
			Action:     "RESTART_POD",
			Target: Target{
				Cluster:   "dev",
				Namespace: "sre-agent-lab",
				Kind:      "Pod",
				Name:      "crash-app",
				UID:       "pod-uid-plan-store",
			},
		},
	)
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	return plan
}
