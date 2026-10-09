package incident_test

import (
	"context"
	"errors"
	"testing"
	"time"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func TestRegistryCompleteFencedVerificationRejectsOldHolderAfterLeaseTakeover(
	t *testing.T,
) {
	t.Parallel()

	fixture := newFencedVerificationCompletionTakeoverFixture(t)

	_, err := fixture.registry.CompleteFencedVerification(
		fixture.ctx,
		incidentdomain.CompleteFencedVerificationCommand{
			IncidentID: fixture.verification.ActionKey.IncidentID,
			ExpectedVersion: fixture.staleClaim.Incident.
				Version,
			HolderID: fixture.staleClaim.HolderID,
			Now:      fixture.takeoverAt.Add(time.Second),
			Verification: remediationdomain.
				CompleteVerificationCommand{
				ActionKey: fixture.verification.ActionKey,
				ExpectedVersion: fixture.verification.
					Version,
				To: remediationdomain.
					VerificationStatusRecovered,
				FinishedAt: fixture.takeoverAt.Add(
					time.Second,
				),
				EvidenceCode: "WORKLOAD_RECOVERED",
			},
		},
	)
	if !errors.Is(err, incidentdomain.ErrVersionConflict) {
		t.Fatalf(
			"CompleteFencedVerification() error = %v; "+
				"want ErrVersionConflict",
			err,
		)
	}

	pending, err := fixture.store.ListPending(fixture.ctx)
	if err != nil {
		t.Fatalf("ListPending() error = %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf(
			"pending Verifications = %d; want 1",
			len(pending),
		)
	}

	persisted := pending[0]

	if persisted.ID != fixture.verification.ID {
		t.Fatalf(
			"persisted Verification ID = %q; want %q",
			persisted.ID,
			fixture.verification.ID,
		)
	}
	if persisted.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"persisted Verification Status = %q; want %q",
			persisted.Status,
			remediationdomain.VerificationStatusPending,
		)
	}
	if persisted.Version != fixture.verification.Version {
		t.Fatalf(
			"persisted Verification Version = %d; want %d",
			persisted.Version,
			fixture.verification.Version,
		)
	}
	if persisted.FinishedAt != nil {
		t.Fatalf(
			"persisted Verification FinishedAt = %s; want nil",
			persisted.FinishedAt,
		)
	}
	if persisted.EvidenceCode != "" {
		t.Fatalf(
			"persisted Verification EvidenceCode = %q; want empty",
			persisted.EvidenceCode,
		)
	}
}

type fencedVerificationCompletionTakeoverFixture struct {
	ctx          context.Context
	registry     *incidentdomain.Registry
	store        *remediationdomain.MemoryVerificationStore
	verification remediationdomain.Verification
	staleClaim   incidentdomain.Claim
	activeClaim  incidentdomain.Claim
	takeoverAt   time.Time
}

func newFencedVerificationCompletionTakeoverFixture(
	t *testing.T,
) fencedVerificationCompletionTakeoverFixture {
	t.Helper()

	ctx := context.Background()
	now := time.Date(
		2026,
		time.October,
		8,
		18,
		0,
		0,
		0,
		time.UTC,
	)

	store := remediationdomain.NewMemoryVerificationStore()
	registry :=
		incidentdomain.NewRegistryWithVerificationLifecycleStore(
			store,
		)

	observed, created, err := registry.Observe(
		ctx,
		incidentdomain.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "PodCrashLooping",
			Target: incidentdomain.Target{
				Kind:      "Pod",
				Namespace: "default",
				Name:      "fenced-completion-app",
				UID:       "pod-uid-fenced-completion",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: observed.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "agent-action",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v",
			err,
		)
	}

	actionClaim, err := registry.Claim(
		ctx,
		incidentdomain.ClaimCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			HolderID:        "agent-action",
			Now:             now,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("Claim(agent-action) error = %v", err)
	}

	actionFinishedAt := now.Add(-time.Second)
	actionAttempt := remediationdomain.ActionAttempt{
		ID: "att-fenced-verification-completion",
		Key: remediationdomain.ExecutionKey{
			IncidentID: actionClaim.Incident.ID,
			PlanHash: "sha256:" +
				"fenced-verification-completion-plan",
			TargetUID: "pod-uid-fenced-completion",
			FencingToken: actionClaim.Incident.
				Version,
		},
		Status:  remediationdomain.ActionAttemptStatusSucceeded,
		Version: 2,
		StartedAt: now.Add(
			-2 * time.Second,
		),
		FinishedAt: &actionFinishedAt,
	}

	fenced, err := registry.BeginFencedVerification(
		ctx,
		incidentdomain.BeginFencedVerificationCommand{
			IncidentID: actionClaim.Incident.ID,
			ExpectedVersion: actionClaim.Incident.
				Version,
			HolderID:   actionClaim.HolderID,
			Now:        now.Add(time.Second),
			ReasonCode: "ACTION_ATTEMPT_TERMINAL",
			Verification: remediationdomain.
				BeginVerificationCommand{
				ActionAttempt: actionAttempt,
				Subject: remediationdomain.
					VerificationSubject{
					Cluster:   "dev",
					Namespace: "default",
					Kind:      "ReplicaSet",
					Name:      "fenced-completion-rs",
					UID:       "rs-uid-fenced-completion",
				},
				StartedAt: now,
			},
		},
	)
	if err != nil {
		t.Fatalf(
			"BeginFencedVerification() error = %v",
			err,
		)
	}

	staleClaimAt := actionClaim.ExpiresAt
	staleScheduled, found, err :=
		registry.ClaimPendingVerification(
			ctx,
			incidentdomain.ClaimPendingVerificationCommand{
				HolderID:      "verification-agent-a",
				Now:           staleClaimAt,
				LeaseDuration: 30 * time.Second,
			},
		)
	if err != nil {
		t.Fatalf(
			"ClaimPendingVerification(agent-a) error = %v",
			err,
		)
	}
	if !found {
		t.Fatal(
			"ClaimPendingVerification(agent-a) found = false; want true",
		)
	}

	takeoverAt := staleScheduled.Claim.ExpiresAt
	takeover, found, err := registry.ClaimPendingVerification(
		ctx,
		incidentdomain.ClaimPendingVerificationCommand{
			HolderID:      "verification-agent-b",
			Now:           takeoverAt,
			LeaseDuration: 30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf(
			"ClaimPendingVerification(agent-b) error = %v",
			err,
		)
	}
	if !found {
		t.Fatal(
			"ClaimPendingVerification(agent-b) found = false; want true",
		)
	}
	if takeover.Claim.HolderID != "verification-agent-b" {
		t.Fatalf(
			"takeover Claim HolderID = %q; want %q",
			takeover.Claim.HolderID,
			"verification-agent-b",
		)
	}
	if takeover.Claim.Incident.Version !=
		staleScheduled.Claim.Incident.Version+1 {
		t.Fatalf(
			"takeover Incident Version = %d; want %d",
			takeover.Claim.Incident.Version,
			staleScheduled.Claim.Incident.Version+1,
		)
	}

	return fencedVerificationCompletionTakeoverFixture{
		ctx:          ctx,
		registry:     registry,
		store:        store,
		verification: fenced.Verification,
		staleClaim:   staleScheduled.Claim,
		activeClaim:  takeover.Claim,
		takeoverAt:   takeoverAt,
	}
}

func TestRegistryCompleteFencedVerificationPersistsTerminalOutcomeUnderActiveFence(
	t *testing.T,
) {
	t.Parallel()

	fixture := newFencedVerificationCompletionTakeoverFixture(t)
	completedAt := fixture.takeoverAt.Add(time.Second)
	command := fixture.completionCommand(completedAt)

	result, err := fixture.registry.CompleteFencedVerification(
		fixture.ctx,
		command,
	)
	if err != nil {
		t.Fatalf(
			"CompleteFencedVerification() error = %v",
			err,
		)
	}

	if result.Incident.ID !=
		fixture.activeClaim.Incident.ID {
		t.Fatalf(
			"Incident ID = %q; want %q",
			result.Incident.ID,
			fixture.activeClaim.Incident.ID,
		)
	}
	if result.Incident.State != incidentdomain.StateVerifying {
		t.Fatalf(
			"Incident State = %q; want %q",
			result.Incident.State,
			incidentdomain.StateVerifying,
		)
	}
	if result.Incident.Version !=
		fixture.activeClaim.Incident.Version {
		t.Fatalf(
			"Incident Version = %d; want unchanged %d",
			result.Incident.Version,
			fixture.activeClaim.Incident.Version,
		)
	}
	if result.Incident.ResolutionVerificationID != "" {
		t.Fatalf(
			"Incident ResolutionVerificationID = %q; want empty",
			result.Incident.ResolutionVerificationID,
		)
	}

	if result.Verification.ID != fixture.verification.ID {
		t.Fatalf(
			"Verification ID = %q; want %q",
			result.Verification.ID,
			fixture.verification.ID,
		)
	}
	if result.Verification.Status !=
		remediationdomain.VerificationStatusRecovered {
		t.Fatalf(
			"Verification Status = %q; want %q",
			result.Verification.Status,
			remediationdomain.VerificationStatusRecovered,
		)
	}
	if result.Verification.Version !=
		fixture.verification.Version+1 {
		t.Fatalf(
			"Verification Version = %d; want %d",
			result.Verification.Version,
			fixture.verification.Version+1,
		)
	}
	if result.Verification.FinishedAt == nil {
		t.Fatal("Verification FinishedAt = nil; want non-nil")
	}
	if !result.Verification.FinishedAt.Equal(completedAt) {
		t.Fatalf(
			"Verification FinishedAt = %s; want %s",
			result.Verification.FinishedAt,
			completedAt,
		)
	}
	if result.Verification.EvidenceCode !=
		command.Verification.EvidenceCode {
		t.Fatalf(
			"Verification EvidenceCode = %q; want %q",
			result.Verification.EvidenceCode,
			command.Verification.EvidenceCode,
		)
	}

	err = fixture.store.RequireRecovered(
		fixture.ctx,
		result.Verification.ID,
		result.Incident.ID,
	)
	if err != nil {
		t.Fatalf(
			"RequireRecovered() error = %v",
			err,
		)
	}

	pending, err := fixture.store.ListPending(fixture.ctx)
	if err != nil {
		t.Fatalf("ListPending() error = %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf(
			"pending Verifications = %d; want 0",
			len(pending),
		)
	}
}

func TestRegistryCompleteFencedVerificationRejectsInvalidFence(
	t *testing.T,
) {
	t.Parallel()

	testCases := []struct {
		name   string
		mutate func(
			*fencedVerificationCompletionTakeoverFixture,
			*incidentdomain.CompleteFencedVerificationCommand,
		)
		wantError error
	}{
		{
			name: "wrong holder",
			mutate: func(
				_ *fencedVerificationCompletionTakeoverFixture,
				command *incidentdomain.
					CompleteFencedVerificationCommand,
			) {
				command.HolderID = "verification-agent-stale"
			},
			wantError: incidentdomain.
				ErrVerificationFenceConflict,
		},
		{
			name: "expired lease",
			mutate: func(
				fixture *fencedVerificationCompletionTakeoverFixture,
				command *incidentdomain.
					CompleteFencedVerificationCommand,
			) {
				command.Now = fixture.activeClaim.ExpiresAt
				command.Verification.FinishedAt =
					fixture.activeClaim.ExpiresAt
			},
			wantError: incidentdomain.
				ErrVerificationFenceConflict,
		},
		{
			name: "verification belongs to another incident",
			mutate: func(
				_ *fencedVerificationCompletionTakeoverFixture,
				command *incidentdomain.
					CompleteFencedVerificationCommand,
			) {
				command.Verification.ActionKey.IncidentID =
					"inc-foreign-verification"
			},
			wantError: incidentdomain.
				ErrInvalidFencedVerification,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			fixture :=
				newFencedVerificationCompletionTakeoverFixture(
					t,
				)

			command := fixture.completionCommand(
				fixture.takeoverAt.Add(time.Second),
			)
			testCase.mutate(&fixture, &command)

			_, err :=
				fixture.registry.CompleteFencedVerification(
					fixture.ctx,
					command,
				)
			if !errors.Is(err, testCase.wantError) {
				t.Fatalf(
					"CompleteFencedVerification() error = %v; want %v",
					err,
					testCase.wantError,
				)
			}

			pending, listErr :=
				fixture.store.ListPending(fixture.ctx)
			if listErr != nil {
				t.Fatalf(
					"ListPending() error = %v",
					listErr,
				)
			}
			if len(pending) != 1 {
				t.Fatalf(
					"pending Verifications = %d; want 1",
					len(pending),
				)
			}

			persisted := pending[0]
			if persisted.ID != fixture.verification.ID {
				t.Fatalf(
					"persisted Verification ID = %q; want %q",
					persisted.ID,
					fixture.verification.ID,
				)
			}
			if persisted.Status !=
				remediationdomain.VerificationStatusPending {
				t.Fatalf(
					"persisted Verification Status = %q; want %q",
					persisted.Status,
					remediationdomain.
						VerificationStatusPending,
				)
			}
			if persisted.Version !=
				fixture.verification.Version {
				t.Fatalf(
					"persisted Verification Version = %d; want %d",
					persisted.Version,
					fixture.verification.Version,
				)
			}
			if persisted.FinishedAt != nil {
				t.Fatalf(
					"persisted Verification FinishedAt = %s; want nil",
					persisted.FinishedAt,
				)
			}
			if persisted.EvidenceCode != "" {
				t.Fatalf(
					"persisted Verification EvidenceCode = %q; want empty",
					persisted.EvidenceCode,
				)
			}
		})
	}
}

func (
	fixture fencedVerificationCompletionTakeoverFixture,
) completionCommand(
	completedAt time.Time,
) incidentdomain.CompleteFencedVerificationCommand {
	return incidentdomain.CompleteFencedVerificationCommand{
		IncidentID: fixture.activeClaim.Incident.ID,
		ExpectedVersion: fixture.activeClaim.Incident.
			Version,
		HolderID: fixture.activeClaim.HolderID,
		Now:      completedAt,
		Verification: remediationdomain.
			CompleteVerificationCommand{
			ActionKey: fixture.verification.ActionKey,
			ExpectedVersion: fixture.verification.
				Version,
			To: remediationdomain.
				VerificationStatusRecovered,
			FinishedAt:   completedAt,
			EvidenceCode: "WORKLOAD_RECOVERED",
		},
	}
}

func TestRegistryCompleteFencedVerificationExactRetryIsIdempotent(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(
		2026,
		time.October,
		8,
		21,
		0,
		0,
		0,
		time.UTC,
	)

	store := remediationdomain.NewMemoryVerificationStore()
	registry :=
		incidentdomain.NewRegistryWithVerificationLifecycleStore(store)

	observed, created, err := registry.Observe(
		ctx,
		incidentdomain.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "PodCrashLooping",
			Target: incidentdomain.Target{
				Kind:      "Pod",
				Namespace: "default",
				Name:      "fenced-completion-exact-retry",
				UID:       "pod-uid-fenced-completion-exact-retry",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: observed.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "agent-a",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf("Transition(DIAGNOSED) error = %v", err)
	}

	actionClaim, err := registry.Claim(
		ctx,
		incidentdomain.ClaimCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			HolderID:        "agent-a",
			Now:             now,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("Claim(agent-a) error = %v", err)
	}

	actionFinishedAt := now.Add(-time.Second)

	fenced, err := registry.BeginFencedVerification(
		ctx,
		incidentdomain.BeginFencedVerificationCommand{
			IncidentID: actionClaim.Incident.ID,
			ExpectedVersion: actionClaim.Incident.
				Version,
			HolderID:   actionClaim.HolderID,
			Now:        now.Add(time.Second),
			ReasonCode: "ACTION_ATTEMPT_TERMINAL",
			Verification: remediationdomain.
				BeginVerificationCommand{
				ActionAttempt: remediationdomain.ActionAttempt{
					ID: "att-fenced-completion-exact-retry",
					Key: remediationdomain.ExecutionKey{
						IncidentID: actionClaim.Incident.ID,
						PlanHash: "sha256:" +
							"fenced-completion-exact-retry",
						TargetUID: actionClaim.Incident.ID,
						FencingToken: actionClaim.Incident.
							Version,
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
					Name:      "fenced-completion-exact-retry",
					UID: "deployment-uid-" +
						"fenced-completion-exact-retry",
				},
				StartedAt: now,
			},
		},
	)
	if err != nil {
		t.Fatalf("BeginFencedVerification() error = %v", err)
	}
	if !fenced.Created {
		t.Fatal("BeginFencedVerification() Created = false; want true")
	}

	schedulerNow := actionClaim.ExpiresAt.Add(time.Second)

	scheduled, found, err := registry.ClaimPendingVerification(
		ctx,
		incidentdomain.ClaimPendingVerificationCommand{
			HolderID:      "verification-scheduler-a",
			Now:           schedulerNow,
			LeaseDuration: 30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("ClaimPendingVerification() error = %v", err)
	}
	if !found {
		t.Fatal("ClaimPendingVerification() found = false; want true")
	}

	completedAt := schedulerNow.Add(time.Second)
	command := incidentdomain.CompleteFencedVerificationCommand{
		IncidentID: scheduled.Claim.Incident.ID,
		ExpectedVersion: scheduled.Claim.Incident.
			Version,
		HolderID: scheduled.Claim.HolderID,
		Now:      completedAt,
		Verification: remediationdomain.
			CompleteVerificationCommand{
			ActionKey: scheduled.Verification.ActionKey,
			ExpectedVersion: scheduled.Verification.
				Version,
			To: remediationdomain.
				VerificationStatusRecovered,
			FinishedAt:   completedAt,
			EvidenceCode: "WORKLOAD_RECOVERED",
		},
	}

	first, err := registry.CompleteFencedVerification(ctx, command)
	if err != nil {
		t.Fatalf(
			"first CompleteFencedVerification() error = %v",
			err,
		)
	}

	second, err := registry.CompleteFencedVerification(ctx, command)
	if err != nil {
		t.Fatalf(
			"retry CompleteFencedVerification() error = %v",
			err,
		)
	}

	if first.Incident.Version !=
		scheduled.Claim.Incident.Version {
		t.Fatalf(
			"first Incident Version = %d; want unchanged %d",
			first.Incident.Version,
			scheduled.Claim.Incident.Version,
		)
	}
	if second.Incident.Version != first.Incident.Version {
		t.Fatalf(
			"retry Incident Version = %d; want unchanged %d",
			second.Incident.Version,
			first.Incident.Version,
		)
	}
	if first.Verification.Version !=
		scheduled.Verification.Version+1 {
		t.Fatalf(
			"first Verification Version = %d; want %d",
			first.Verification.Version,
			scheduled.Verification.Version+1,
		)
	}
	if second.Verification.Version !=
		first.Verification.Version {
		t.Fatalf(
			"retry Verification Version = %d; want unchanged %d",
			second.Verification.Version,
			first.Verification.Version,
		)
	}
	if second.Verification.Status !=
		remediationdomain.VerificationStatusRecovered {
		t.Fatalf(
			"retry Verification Status = %q; want %q",
			second.Verification.Status,
			remediationdomain.VerificationStatusRecovered,
		)
	}
	if second.Verification.EvidenceCode !=
		first.Verification.EvidenceCode {
		t.Fatalf(
			"retry EvidenceCode = %q; want %q",
			second.Verification.EvidenceCode,
			first.Verification.EvidenceCode,
		)
	}
}
