package incident

import (
	"context"
	"errors"
	"testing"
	"time"

	remediationdomain "sre-agent/internal/remediation"
)

func TestRegistryCompleteFencedVerificationRejectsClaimWithoutMatchingAudit(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(
		2026,
		time.October,
		8,
		17,
		0,
		0,
		0,
		time.UTC,
	)

	actionFinishedAt := now.Add(-time.Second)

	store := remediationdomain.NewMemoryVerificationStore()

	pending, created, err := store.Begin(
		ctx,
		remediationdomain.BeginVerificationCommand{
			ActionAttempt: remediationdomain.ActionAttempt{
				ID: "att-missing-claim-audit",
				Key: remediationdomain.ExecutionKey{
					IncidentID:   "inc-missing-claim-audit",
					PlanHash:     "sha256:missing-claim-audit",
					TargetUID:    "pod-uid-missing-claim-audit",
					FencingToken: 7,
				},
				Status: remediationdomain.
					ActionAttemptStatusSucceeded,
				Version:    2,
				StartedAt:  now.Add(-2 * time.Second),
				FinishedAt: &actionFinishedAt,
			},
			Subject: remediationdomain.VerificationSubject{
				Cluster:   "dev",
				Namespace: "default",
				Kind:      "Deployment",
				Name:      "missing-claim-audit",
				UID:       "deployment-uid-missing-claim-audit",
			},
			StartedAt: now,
		},
	)
	if err != nil {
		t.Fatalf("Verification Begin() error = %v", err)
	}
	if !created {
		t.Fatal("Verification Begin() created = false; want true")
	}

	registry := NewRegistryWithVerificationLifecycleStore(store)

	current := Incident{
		ID:      pending.ActionKey.IncidentID,
		State:   StateVerifying,
		Version: 7,
	}
	claim := Claim{
		Incident:  current,
		HolderID:  "verification-scheduler-a",
		ExpiresAt: now.Add(time.Minute),
	}

	registry.incidentsByID[current.ID] = current
	registry.claimsByIncidentID[current.ID] = claim

	// Deliberately omit claimHistoryByIncidentID[current.ID].
	// An unaudited in-memory Claim must not authorize completion.

	_, err = registry.CompleteFencedVerification(
		ctx,
		CompleteFencedVerificationCommand{
			IncidentID:      current.ID,
			ExpectedVersion: current.Version,
			HolderID:        claim.HolderID,
			Now:             now.Add(time.Second),
			Verification: remediationdomain.
				CompleteVerificationCommand{
				ActionKey:       pending.ActionKey,
				ExpectedVersion: pending.Version,
				To: remediationdomain.
					VerificationStatusRecovered,
				FinishedAt:   now.Add(time.Second),
				EvidenceCode: "WORKLOAD_RECOVERED",
			},
		},
	)
	if !errors.Is(err, ErrVerificationFenceConflict) {
		t.Fatalf(
			"CompleteFencedVerification() error = %v; "+
				"want ErrVerificationFenceConflict",
			err,
		)
	}

	remaining, err := store.ListPending(ctx)
	if err != nil {
		t.Fatalf("Verification ListPending() error = %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf(
			"pending Verifications = %d; want 1",
			len(remaining),
		)
	}
	if remaining[0].Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"Verification Status = %q; want %q",
			remaining[0].Status,
			remediationdomain.VerificationStatusPending,
		)
	}
	if remaining[0].Version != pending.Version {
		t.Fatalf(
			"Verification Version = %d; want unchanged %d",
			remaining[0].Version,
			pending.Version,
		)
	}
}
