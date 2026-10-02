//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	approvaldomain "sre-agent/internal/approval"
	"sre-agent/internal/incident"
)

func TestPlanStorePersistsCanonicalPlanAcrossAdapterRestart(
	t *testing.T,
) {
	ctx, pool, firstStore, plan := newPostgresPlanStoreFixture(t)

	if err := firstStore.Publish(ctx, plan); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	secondStore := NewPlanStore(pool)

	stored, err := secondStore.Lookup(
		ctx,
		approvaldomain.PlanKey{
			IncidentID: plan.IncidentID,
			Hash:       plan.Hash,
			TargetUID:  plan.Target.UID,
		},
	)
	if err != nil {
		t.Fatalf("Lookup() after adapter restart error = %v", err)
	}
	if stored != plan {
		t.Fatalf("Lookup() plan = %#v; want %#v", stored, plan)
	}
}

func TestPlanStoreConcurrentRepeatedPublishIsIdempotent(
	t *testing.T,
) {
	ctx, pool, store, plan := newPostgresPlanStoreFixture(t)

	const workers = 32

	start := make(chan struct{})
	results := make(chan error, workers)

	var waitGroup sync.WaitGroup
	waitGroup.Add(workers)

	for range workers {
		go func() {
			defer waitGroup.Done()
			<-start

			results <- store.Publish(ctx, plan)
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

	var rowCount int
	err := pool.QueryRow(
		ctx,
		`
SELECT count(*)
FROM incident_plans
WHERE incident_id = $1
  AND plan_hash = $2
  AND target_uid = $3
`,
		plan.IncidentID,
		plan.Hash,
		plan.Target.UID,
	).Scan(&rowCount)
	if err != nil {
		t.Fatalf("count persisted plans: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("persisted plan rows = %d; want 1", rowCount)
	}
}

func TestPlanStoreRejectsTamperedPlan(t *testing.T) {
	ctx, _, store, plan := newPostgresPlanStoreFixture(t)

	plan.Target.Name = "different-pod"

	err := store.Publish(ctx, plan)
	if !errors.Is(err, approvaldomain.ErrInvalidPlan) {
		t.Fatalf("Publish() error = %v; want ErrInvalidPlan", err)
	}
}

func TestPlanStoreReturnsNotFound(t *testing.T) {
	ctx, _, store, plan := newPostgresPlanStoreFixture(t)

	_, err := store.Lookup(
		ctx,
		approvaldomain.PlanKey{
			IncidentID: plan.IncidentID,
			Hash:       plan.Hash + "-missing",
			TargetUID:  plan.Target.UID,
		},
	)
	if !errors.Is(err, approvaldomain.ErrPlanNotFound) {
		t.Fatalf("Lookup() error = %v; want ErrPlanNotFound", err)
	}
}

func newPostgresPlanStoreFixture(
	t *testing.T,
) (
	context.Context,
	*pgxpool.Pool,
	*PlanStore,
	approvaldomain.Plan,
) {
	t.Helper()

	ctx, pool, registry := newPostgresTestRegistry(t)

	observed, created, err := registry.Observe(
		ctx,
		incident.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "KubePodCrashLooping",
			Target: incident.Target{
				Kind:      "Pod",
				Namespace: "sre-agent-lab",
				Name:      "crash-app-plan-store",
				UID:       "pod-uid-postgres-plan-store",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	plan, err := approvaldomain.NewPlan(
		approvaldomain.PlanCommand{
			IncidentID: observed.ID,
			Action:     "RESTART_POD",
			Target: approvaldomain.Target{
				Cluster:   "dev",
				Namespace: "sre-agent-lab",
				Kind:      "Pod",
				Name:      "crash-app-plan-store",
				UID:       "pod-uid-postgres-plan-store",
			},
		},
	)
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	return ctx, pool, NewPlanStore(pool), plan
}
