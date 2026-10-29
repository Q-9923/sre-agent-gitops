package remediation

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryVerificationStoreListsOnlyRequestedLifecycleStatus(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryVerificationStore()
	pending := beginVerificationForListTest(t, store, "pending", 1)
	recoveredPending := beginVerificationForListTest(
		t,
		store,
		"recovered",
		2,
	)
	recoveredAt := recoveredPending.StartedAt.Add(time.Minute)
	recovered, err := store.Complete(
		ctx,
		CompleteVerificationCommand{
			ActionKey:       recoveredPending.ActionKey,
			ExpectedVersion: recoveredPending.Version,
			To:              VerificationStatusRecovered,
			FinishedAt:      recoveredAt,
			EvidenceCode:    "WORKLOAD_RECOVERED",
		},
	)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	pendingList, err := store.ListPending(ctx)
	if err != nil {
		t.Fatalf("ListPending() error = %v", err)
	}
	assertSingleVerificationForListTest(t, pendingList, pending)

	recoveredList, err := store.ListRecovered(ctx)
	if err != nil {
		t.Fatalf("ListRecovered() error = %v", err)
	}
	assertSingleVerificationForListTest(t, recoveredList, recovered)

	pendingList[0].ID = "mutated-copy"
	again, err := store.ListPending(ctx)
	if err != nil {
		t.Fatalf("second ListPending() error = %v", err)
	}
	assertSingleVerificationForListTest(t, again, pending)
}

func TestMemoryVerificationStoreListByStatusHonorsCanceledContext(
	t *testing.T,
) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := NewMemoryVerificationStore()

	if _, err := store.ListPending(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListPending() error = %v; want context canceled", err)
	}
	if _, err := store.ListRecovered(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListRecovered() error = %v; want context canceled", err)
	}
}

func beginVerificationForListTest(
	t *testing.T,
	store *MemoryVerificationStore,
	suffix string,
	fencingToken uint64,
) Verification {
	t.Helper()

	startedAt := time.Date(
		2026,
		time.October,
		13,
		15,
		0,
		0,
		0,
		time.UTC,
	).Add(time.Duration(fencingToken) * time.Minute)
	finishedAt := startedAt.Add(time.Second)
	attempt := ActionAttempt{
		ID: "att-list-" + suffix,
		Key: ExecutionKey{
			IncidentID:   "inc-list-" + suffix,
			PlanHash:     "plan-list-" + suffix,
			TargetUID:    "target-list-" + suffix,
			FencingToken: fencingToken,
		},
		Status:     ActionAttemptStatusSucceeded,
		Version:    2,
		StartedAt:  startedAt,
		FinishedAt: &finishedAt,
	}

	verification, created, err := store.Begin(
		context.Background(),
		BeginVerificationCommand{
			ActionAttempt: attempt,
			Subject: VerificationSubject{
				Cluster:   "dev",
				Namespace: "default",
				Kind:      "ReplicaSet",
				Name:      "workload-" + suffix,
				UID:       "workload-uid-" + suffix,
			},
			StartedAt: finishedAt,
		},
	)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if !created {
		t.Fatal("Begin() created = false; want true")
	}

	return verification
}

func assertSingleVerificationForListTest(
	t *testing.T,
	got []Verification,
	want Verification,
) {
	t.Helper()

	if len(got) != 1 {
		t.Fatalf("listed Verifications = %d; want 1", len(got))
	}
	if got[0].ID != want.ID || got[0].Status != want.Status {
		t.Fatalf(
			"listed Verification = (%q, %q); want (%q, %q)",
			got[0].ID,
			got[0].Status,
			want.ID,
			want.Status,
		)
	}
}
