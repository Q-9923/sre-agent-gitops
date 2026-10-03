package remediation

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestVerificationStoreBeginCreatesOnePendingVerificationForSucceededActionAttempt(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	actionAttempts := NewMemoryActionAttemptStore()

	actionStartedAt := time.Date(
		2026,
		time.October,
		3,
		8,
		0,
		0,
		0,
		time.UTC,
	)
	actionFinishedAt := actionStartedAt.Add(5 * time.Second)
	verificationStartedAt := actionFinishedAt.Add(time.Second)

	key := ExecutionKey{
		IncidentID:   "inc-verification-success",
		PlanHash:     "plan-verification-success",
		TargetUID:    "pod-uid-verification-success",
		FencingToken: 7,
	}

	started, created, err := actionAttempts.Begin(
		ctx,
		BeginActionAttemptCommand{
			Key:       key,
			StartedAt: actionStartedAt,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !created {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	succeeded, err := actionAttempts.Complete(
		ctx,
		CompleteActionAttemptCommand{
			Key:             key,
			ExpectedVersion: started.Version,
			To:              ActionAttemptStatusSucceeded,
			FinishedAt:      actionFinishedAt,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	store := NewMemoryVerificationStore()

	command := BeginVerificationCommand{
		ActionAttempt: succeeded,
		StartedAt:     verificationStartedAt,
	}

	first, firstCreated, err := store.Begin(ctx, command)
	if err != nil {
		t.Fatalf("first Verification Begin() error = %v", err)
	}
	if !firstCreated {
		t.Fatal("first Verification Begin() created = false; want true")
	}
	if first.ID == "" {
		t.Fatal("Verification ID is empty")
	}
	if first.ActionAttemptID != succeeded.ID {
		t.Fatalf(
			"Verification ActionAttemptID = %q; want %q",
			first.ActionAttemptID,
			succeeded.ID,
		)
	}
	if first.ActionKey != succeeded.Key.ActionKey() {
		t.Fatalf(
			"Verification ActionKey = %#v; want %#v",
			first.ActionKey,
			succeeded.Key.ActionKey(),
		)
	}
	if first.Status != VerificationStatusPending {
		t.Fatalf(
			"Verification Status = %q; want %q",
			first.Status,
			VerificationStatusPending,
		)
	}
	if first.Version != 1 {
		t.Fatalf(
			"Verification Version = %d; want 1",
			first.Version,
		)
	}
	if !first.StartedAt.Equal(verificationStartedAt) {
		t.Fatalf(
			"Verification StartedAt = %s; want %s",
			first.StartedAt,
			verificationStartedAt,
		)
	}

	second, secondCreated, err := store.Begin(ctx, command)
	if err != nil {
		t.Fatalf("second Verification Begin() error = %v", err)
	}
	if secondCreated {
		t.Fatal("second Verification Begin() created = true; want false")
	}
	if !reflect.DeepEqual(second, first) {
		t.Fatalf(
			"second Verification = %#v; want %#v",
			second,
			first,
		)
	}
}

func TestVerificationStoreCompletesPendingVerificationAsRecovered(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	actionAttempts := NewMemoryActionAttemptStore()

	actionStartedAt := time.Date(
		2026,
		time.October,
		3,
		9,
		0,
		0,
		0,
		time.UTC,
	)
	actionFinishedAt := actionStartedAt.Add(5 * time.Second)
	verificationStartedAt := actionFinishedAt.Add(time.Second)
	verificationFinishedAt := verificationStartedAt.Add(30 * time.Second)

	key := ExecutionKey{
		IncidentID:   "inc-verification-recovered",
		PlanHash:     "plan-verification-recovered",
		TargetUID:    "pod-uid-verification-recovered",
		FencingToken: 11,
	}

	startedAttempt, created, err := actionAttempts.Begin(
		ctx,
		BeginActionAttemptCommand{
			Key:       key,
			StartedAt: actionStartedAt,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !created {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		CompleteActionAttemptCommand{
			Key:             key,
			ExpectedVersion: startedAttempt.Version,
			To:              ActionAttemptStatusSucceeded,
			FinishedAt:      actionFinishedAt,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	store := NewMemoryVerificationStore()

	pending, created, err := store.Begin(
		ctx,
		BeginVerificationCommand{
			ActionAttempt: succeededAttempt,
			StartedAt:     verificationStartedAt,
		},
	)
	if err != nil {
		t.Fatalf("Verification Begin() error = %v", err)
	}
	if !created {
		t.Fatal("Verification Begin() created = false; want true")
	}

	completed, err := store.Complete(
		ctx,
		CompleteVerificationCommand{
			ActionKey:       pending.ActionKey,
			ExpectedVersion: pending.Version,
			To:              VerificationStatusRecovered,
			FinishedAt:      verificationFinishedAt,
			EvidenceCode:    "TARGET_RECOVERED",
		},
	)
	if err != nil {
		t.Fatalf("Verification Complete() error = %v", err)
	}

	if completed.ID != pending.ID {
		t.Fatalf(
			"completed ID = %q; want %q",
			completed.ID,
			pending.ID,
		)
	}
	if completed.ActionAttemptID != pending.ActionAttemptID {
		t.Fatalf(
			"completed ActionAttemptID = %q; want %q",
			completed.ActionAttemptID,
			pending.ActionAttemptID,
		)
	}
	if completed.ActionKey != pending.ActionKey {
		t.Fatalf(
			"completed ActionKey = %#v; want %#v",
			completed.ActionKey,
			pending.ActionKey,
		)
	}
	if completed.Status != VerificationStatusRecovered {
		t.Fatalf(
			"completed Status = %q; want %q",
			completed.Status,
			VerificationStatusRecovered,
		)
	}
	if completed.Version != pending.Version+1 {
		t.Fatalf(
			"completed Version = %d; want %d",
			completed.Version,
			pending.Version+1,
		)
	}
	if completed.FinishedAt == nil {
		t.Fatal("completed FinishedAt = nil; want timestamp")
	}
	if !completed.FinishedAt.Equal(verificationFinishedAt) {
		t.Fatalf(
			"completed FinishedAt = %s; want %s",
			completed.FinishedAt,
			verificationFinishedAt,
		)
	}
	if completed.EvidenceCode != "TARGET_RECOVERED" {
		t.Fatalf(
			"completed EvidenceCode = %q; want %q",
			completed.EvidenceCode,
			"TARGET_RECOVERED",
		)
	}
}

func TestVerificationStoreCompletesPendingVerificationWithIndependentTerminalOutcomes(
	t *testing.T,
) {
	t.Parallel()

	tests := []struct {
		name         string
		status       VerificationStatus
		evidenceCode string
	}{
		{
			name:         "target not recovered",
			status:       VerificationStatusNotRecovered,
			evidenceCode: "CRASHLOOP_ALERT_STILL_FIRING",
		},
		{
			name:         "evidence inconclusive",
			status:       VerificationStatusInconclusive,
			evidenceCode: "OBSERVATION_UNAVAILABLE",
		},
	}

	for index, test := range tests {
		index := index
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			actionAttempts := NewMemoryActionAttemptStore()

			actionStartedAt := time.Date(
				2026,
				time.October,
				3,
				10,
				index,
				0,
				0,
				time.UTC,
			)
			actionFinishedAt := actionStartedAt.Add(5 * time.Second)
			verificationStartedAt := actionFinishedAt.Add(time.Second)
			verificationFinishedAt := verificationStartedAt.Add(
				30 * time.Second,
			)

			key := ExecutionKey{
				IncidentID: fmt.Sprintf(
					"inc-verification-outcome-%d",
					index,
				),
				PlanHash: fmt.Sprintf(
					"plan-verification-outcome-%d",
					index,
				),
				TargetUID: fmt.Sprintf(
					"pod-uid-verification-outcome-%d",
					index,
				),
				FencingToken: uint64(index + 21),
			}

			startedAttempt, created, err := actionAttempts.Begin(
				ctx,
				BeginActionAttemptCommand{
					Key:       key,
					StartedAt: actionStartedAt,
				},
			)
			if err != nil {
				t.Fatalf("ActionAttempt Begin() error = %v", err)
			}
			if !created {
				t.Fatal(
					"ActionAttempt Begin() created = false; want true",
				)
			}

			succeededAttempt, err := actionAttempts.Complete(
				ctx,
				CompleteActionAttemptCommand{
					Key:             key,
					ExpectedVersion: startedAttempt.Version,
					To:              ActionAttemptStatusSucceeded,
					FinishedAt:      actionFinishedAt,
				},
			)
			if err != nil {
				t.Fatalf(
					"ActionAttempt Complete() error = %v",
					err,
				)
			}

			store := NewMemoryVerificationStore()

			pending, created, err := store.Begin(
				ctx,
				BeginVerificationCommand{
					ActionAttempt: succeededAttempt,
					StartedAt:     verificationStartedAt,
				},
			)
			if err != nil {
				t.Fatalf("Verification Begin() error = %v", err)
			}
			if !created {
				t.Fatal(
					"Verification Begin() created = false; want true",
				)
			}

			completed, err := store.Complete(
				ctx,
				CompleteVerificationCommand{
					ActionKey:       pending.ActionKey,
					ExpectedVersion: pending.Version,
					To:              test.status,
					FinishedAt:      verificationFinishedAt,
					EvidenceCode:    test.evidenceCode,
				},
			)
			if err != nil {
				t.Fatalf("Verification Complete() error = %v", err)
			}

			if completed.Status != test.status {
				t.Fatalf(
					"completed Status = %q; want %q",
					completed.Status,
					test.status,
				)
			}
			if completed.Version != pending.Version+1 {
				t.Fatalf(
					"completed Version = %d; want %d",
					completed.Version,
					pending.Version+1,
				)
			}
			if completed.FinishedAt == nil {
				t.Fatal(
					"completed FinishedAt = nil; want timestamp",
				)
			}
			if !completed.FinishedAt.Equal(
				verificationFinishedAt,
			) {
				t.Fatalf(
					"completed FinishedAt = %s; want %s",
					completed.FinishedAt,
					verificationFinishedAt,
				)
			}
			if completed.EvidenceCode != test.evidenceCode {
				t.Fatalf(
					"completed EvidenceCode = %q; want %q",
					completed.EvidenceCode,
					test.evidenceCode,
				)
			}
		})
	}
}

func TestVerificationStoreCompleteIsIdempotentForExactRetry(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	actionAttempts := NewMemoryActionAttemptStore()

	actionStartedAt := time.Date(
		2026,
		time.October,
		3,
		11,
		0,
		0,
		0,
		time.UTC,
	)
	actionFinishedAt := actionStartedAt.Add(5 * time.Second)
	verificationStartedAt := actionFinishedAt.Add(time.Second)
	verificationFinishedAt := verificationStartedAt.Add(30 * time.Second)

	key := ExecutionKey{
		IncidentID:   "inc-verification-idempotent",
		PlanHash:     "plan-verification-idempotent",
		TargetUID:    "pod-uid-verification-idempotent",
		FencingToken: 31,
	}

	startedAttempt, created, err := actionAttempts.Begin(
		ctx,
		BeginActionAttemptCommand{
			Key:       key,
			StartedAt: actionStartedAt,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !created {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		CompleteActionAttemptCommand{
			Key:             key,
			ExpectedVersion: startedAttempt.Version,
			To:              ActionAttemptStatusSucceeded,
			FinishedAt:      actionFinishedAt,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	store := NewMemoryVerificationStore()

	pending, created, err := store.Begin(
		ctx,
		BeginVerificationCommand{
			ActionAttempt: succeededAttempt,
			StartedAt:     verificationStartedAt,
		},
	)
	if err != nil {
		t.Fatalf("Verification Begin() error = %v", err)
	}
	if !created {
		t.Fatal("Verification Begin() created = false; want true")
	}

	command := CompleteVerificationCommand{
		ActionKey:       pending.ActionKey,
		ExpectedVersion: pending.Version,
		To:              VerificationStatusRecovered,
		FinishedAt:      verificationFinishedAt,
		EvidenceCode:    "TARGET_RECOVERED",
	}

	first, err := store.Complete(ctx, command)
	if err != nil {
		t.Fatalf("first Verification Complete() error = %v", err)
	}

	second, err := store.Complete(ctx, command)
	if err != nil {
		t.Fatalf(
			"exact retry Verification Complete() error = %v; want nil",
			err,
		)
	}

	if !reflect.DeepEqual(second, first) {
		t.Fatalf(
			"exact retry Verification = %#v; want %#v",
			second,
			first,
		)
	}
}
