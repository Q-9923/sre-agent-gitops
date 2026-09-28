//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	approvaldomain "sre-agent/internal/approval"
	"sre-agent/internal/incident"
)

func TestApprovalStoreGrantSurvivesAdapterRestart(t *testing.T) {
	fixture := newApprovalStoreFixture(t)

	store := NewApprovalStore(fixture.pool)
	if err := store.Grant(fixture.ctx, fixture.granted); err != nil {
		t.Fatalf("Grant() error = %v; want nil", err)
	}

	restarted := NewApprovalStore(fixture.pool)
	got, err := restarted.Lookup(fixture.ctx, fixture.key)
	if err != nil {
		t.Fatalf("Lookup() after restart error = %v; want nil", err)
	}

	assertApprovalEqual(t, got, fixture.granted)
}

func TestApprovalStoreGrantIsIdempotentAndRejectsConflict(t *testing.T) {
	fixture := newApprovalStoreFixture(t)
	store := NewApprovalStore(fixture.pool)

	if err := store.Grant(fixture.ctx, fixture.granted); err != nil {
		t.Fatalf("first Grant() error = %v; want nil", err)
	}

	if err := store.Grant(fixture.ctx, fixture.granted); err != nil {
		t.Fatalf("second Grant() error = %v; want nil", err)
	}

	conflicting := fixture.granted
	conflicting.ApprovedBy = "operator-b"

	err := store.Grant(fixture.ctx, conflicting)
	if !errors.Is(err, approvaldomain.ErrApprovalConflict) {
		t.Fatalf(
			"conflicting Grant() error = %v; want ErrApprovalConflict",
			err,
		)
	}
}

func TestApprovalStoreLookupRequiresExactKey(t *testing.T) {
	fixture := newApprovalStoreFixture(t)
	store := NewApprovalStore(fixture.pool)

	if err := store.Grant(fixture.ctx, fixture.granted); err != nil {
		t.Fatalf("Grant() error = %v; want nil", err)
	}

	changed := fixture.key
	changed.TargetUID = "pod-uid-replaced"

	_, err := store.Lookup(fixture.ctx, changed)
	if !errors.Is(err, approvaldomain.ErrApprovalNotFound) {
		t.Fatalf(
			"Lookup() error = %v; want ErrApprovalNotFound",
			err,
		)
	}
}

func TestApprovalStoreConcurrentGrantAllowsOneImmutableWinner(
	t *testing.T,
) {
	fixture := newApprovalStoreFixture(t)
	store := NewApprovalStore(fixture.pool)

	const competitors = 32

	start := make(chan struct{})
	results := make(chan error, competitors)

	var waitGroup sync.WaitGroup
	waitGroup.Add(competitors)

	for index := 0; index < competitors; index++ {
		candidate := fixture.granted
		candidate.ApprovedBy = fmt.Sprintf("operator-%02d", index)

		go func(granted approvaldomain.Approval) {
			defer waitGroup.Done()
			<-start

			results <- store.Grant(fixture.ctx, granted)
		}(candidate)
	}

	close(start)
	waitGroup.Wait()
	close(results)

	successes := 0
	conflicts := 0

	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, approvaldomain.ErrApprovalConflict):
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

	stored, err := store.Lookup(fixture.ctx, fixture.key)
	if err != nil {
		t.Fatalf("Lookup() error = %v; want nil", err)
	}

	if stored.ApprovedBy == "" {
		t.Fatal("Lookup() returned an approval without an approver")
	}
}

type approvalStoreFixture struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	granted approvaldomain.Approval
	key     approvaldomain.ApprovalKey
}

func newApprovalStoreFixture(t *testing.T) approvalStoreFixture {
	t.Helper()

	ctx, pool, registry := newPostgresTestRegistry(t)

	observation := incident.Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "KubePodCrashLooping",
		Target: incident.Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app",
			UID:       "pod-uid-approval-store",
		},
	}

	observed, _, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}

	plan, err := approvaldomain.NewPlan(
		approvaldomain.PlanCommand{
			IncidentID: observed.ID,
			Action:     "RESTART_POD",
			Target: approvaldomain.Target{
				Cluster:   observation.Cluster,
				Namespace: observation.Target.Namespace,
				Kind:      observation.Target.Kind,
				Name:      observation.Target.Name,
				UID:       observation.Target.UID,
			},
		},
	)
	if err != nil {
		t.Fatalf("NewPlan() error = %v; want nil", err)
	}

	approvedAt := time.Date(
		2026,
		time.September,
		28,
		18,
		0,
		0,
		0,
		time.UTC,
	)

	granted := approvaldomain.Approval{
		IncidentID: plan.IncidentID,
		PlanHash:   plan.Hash,
		TargetUID:  plan.Target.UID,
		ApprovedBy: "operator-a",
		ApprovedAt: approvedAt,
		ExpiresAt:  approvedAt.Add(10 * time.Minute),
	}

	return approvalStoreFixture{
		ctx:     ctx,
		pool:    pool,
		granted: granted,
		key: approvaldomain.ApprovalKey{
			IncidentID: granted.IncidentID,
			PlanHash:   granted.PlanHash,
			TargetUID:  granted.TargetUID,
		},
	}
}

func assertApprovalEqual(
	t *testing.T,
	got approvaldomain.Approval,
	want approvaldomain.Approval,
) {
	t.Helper()

	if got.IncidentID != want.IncidentID ||
		got.PlanHash != want.PlanHash ||
		got.TargetUID != want.TargetUID ||
		got.ApprovedBy != want.ApprovedBy ||
		!got.ApprovedAt.Equal(want.ApprovedAt) ||
		!got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("Approval = %#v; want %#v", got, want)
	}
}
