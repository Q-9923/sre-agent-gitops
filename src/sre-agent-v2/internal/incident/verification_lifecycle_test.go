package incident_test

import (
	"context"
	"errors"
	"testing"
	"time"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func TestRegistryBeginFencedVerificationAtomicallyRejectsConcurrentTakeover(
	t *testing.T,
) {
	now := time.Date(
		2026,
		time.October,
		6,
		16,
		0,
		0,
		0,
		time.UTC,
	)

	verificationStore :=
		newBlockingVerificationLifecycleStore()

	registry :=
		incidentdomain.NewRegistryWithVerificationLifecycleStore(
			verificationStore,
		)

	observed, created, err := registry.Observe(
		context.Background(),
		incidentdomain.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "PodCrashLooping",
			Target: incidentdomain.Target{
				Kind:      "Pod",
				Namespace: "default",
				Name:      "atomic-verification-app",
				UID:       "pod-uid-atomic-verification",
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
		context.Background(),
		incidentdomain.TransitionCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: observed.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "agent-a",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v",
			err,
		)
	}

	claim, err := registry.Claim(
		context.Background(),
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

	finishedAt := now.Add(-time.Second)
	actionAttempt := remediationdomain.ActionAttempt{
		ID: "att-atomic-verification",
		Key: remediationdomain.ExecutionKey{
			IncidentID: claim.Incident.ID,
			PlanHash: "sha256:" +
				"atomic-verification-plan",
			TargetUID: "pod-uid-atomic-verification",
			FencingToken: claim.Incident.
				Version,
		},
		Status: remediationdomain.
			ActionAttemptStatusSucceeded,
		Version:    2,
		StartedAt:  now.Add(-2 * time.Second),
		FinishedAt: &finishedAt,
	}

	beginResult := make(
		chan fencedVerificationBeginOutcome,
		1,
	)
	go func() {
		result, beginErr :=
			registry.BeginFencedVerification(
				context.Background(),
				incidentdomain.
					BeginFencedVerificationCommand{
					IncidentID: claim.Incident.ID,
					ExpectedVersion: claim.
						Incident.Version,
					HolderID:   "agent-a",
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
							Name:      "atomic-verification-rs",
							UID: "rs-uid-" +
								"atomic-verification",
						},
						StartedAt: now,
					},
				},
			)

		beginResult <- fencedVerificationBeginOutcome{
			result: result,
			err:    beginErr,
		}
	}()

	select {
	case <-verificationStore.beginEntered:
	case <-time.After(time.Second):
		t.Fatal(
			"Verification Store Begin was not entered",
		)
	}

	takeoverResult := make(
		chan incidentClaimOutcome,
		1,
	)
	go func() {
		takeover, takeoverErr := registry.Claim(
			context.Background(),
			incidentdomain.ClaimCommand{
				IncidentID: claim.Incident.ID,
				ExpectedVersion: claim.
					Incident.Version,
				HolderID: "agent-b",
				Now: now.Add(
					31 * time.Second,
				),
				LeaseDuration: 30 * time.Second,
			},
		)

		takeoverResult <- incidentClaimOutcome{
			claim: takeover,
			err:   takeoverErr,
		}
	}()

	select {
	case takeover := <-takeoverResult:
		t.Fatalf(
			"concurrent takeover completed before atomic Verification commit: "+
				"claim=%#v error=%v",
			takeover.claim,
			takeover.err,
		)
	case <-time.After(100 * time.Millisecond):
	}

	close(verificationStore.releaseBegin)

	var begin fencedVerificationBeginOutcome
	select {
	case begin = <-beginResult:
	case <-time.After(time.Second):
		t.Fatal(
			"BeginFencedVerification did not complete",
		)
	}

	if begin.err != nil {
		t.Fatalf(
			"BeginFencedVerification() error = %v",
			begin.err,
		)
	}
	if begin.result.Incident.State !=
		incidentdomain.StateVerifying {
		t.Fatalf(
			"Incident State = %q; want %q",
			begin.result.Incident.State,
			incidentdomain.StateVerifying,
		)
	}
	if begin.result.Incident.Version !=
		claim.Incident.Version+1 {
		t.Fatalf(
			"Incident Version = %d; want %d",
			begin.result.Incident.Version,
			claim.Incident.Version+1,
		)
	}
	if begin.result.Verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"Verification Status = %q; want %q",
			begin.result.Verification.Status,
			remediationdomain.
				VerificationStatusPending,
		)
	}
	if !begin.result.Created {
		t.Fatal(
			"BeginFencedVerification Created = false; want true",
		)
	}

	select {
	case takeover := <-takeoverResult:
		if !errors.Is(
			takeover.err,
			incidentdomain.ErrVersionConflict,
		) {
			t.Fatalf(
				"concurrent takeover error = %v; want ErrVersionConflict",
				takeover.err,
			)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent takeover did not finish")
	}
}

type fencedVerificationBeginOutcome struct {
	result incidentdomain.FencedVerificationResult
	err    error
}

type incidentClaimOutcome struct {
	claim incidentdomain.Claim
	err   error
}

type blockingVerificationLifecycleStore struct {
	delegate     *remediationdomain.MemoryVerificationStore
	beginEntered chan struct{}
	releaseBegin chan struct{}
}

func newBlockingVerificationLifecycleStore() *blockingVerificationLifecycleStore {
	return &blockingVerificationLifecycleStore{
		delegate: remediationdomain.
			NewMemoryVerificationStore(),
		beginEntered: make(chan struct{}),
		releaseBegin: make(chan struct{}),
	}
}

func (store *blockingVerificationLifecycleStore) Begin(
	ctx context.Context,
	command remediationdomain.BeginVerificationCommand,
) (remediationdomain.Verification, bool, error) {
	close(store.beginEntered)

	select {
	case <-ctx.Done():
		return remediationdomain.Verification{},
			false,
			ctx.Err()
	case <-store.releaseBegin:
	}

	return store.delegate.Begin(
		ctx,
		command,
	)
}

func (store *blockingVerificationLifecycleStore) Complete(
	ctx context.Context,
	command remediationdomain.CompleteVerificationCommand,
) (remediationdomain.Verification, error) {
	return store.delegate.Complete(
		ctx,
		command,
	)
}

func (store *blockingVerificationLifecycleStore) RequireRecovered(
	ctx context.Context,
	incidentID string,
	verificationID string,
) error {
	return store.delegate.RequireRecovered(
		ctx,
		incidentID,
		verificationID,
	)
}
