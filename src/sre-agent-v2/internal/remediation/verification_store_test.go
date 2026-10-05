package remediation

import (
	"context"
	"errors"
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
		Subject:       validTestVerificationSubject(),
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
			Subject:       validTestVerificationSubject(),
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
					Subject:       validTestVerificationSubject(),
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
			Subject:       validTestVerificationSubject(),
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

func TestVerificationStoreBeginPersistsStableVerificationSubject(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryVerificationStore()

	actionFinishedAt := time.Date(
		2026,
		time.October,
		5,
		8,
		0,
		5,
		0,
		time.UTC,
	)
	actionStartedAt := actionFinishedAt.Add(-5 * time.Second)
	verificationStartedAt := actionFinishedAt.Add(time.Second)

	actionAttempt := ActionAttempt{
		ID: "att-verification-subject",
		Key: ExecutionKey{
			IncidentID:   "inc-verification-subject",
			PlanHash:     "plan-verification-subject",
			TargetUID:    "pod-uid-verification-subject",
			FencingToken: 11,
		},
		Status:     ActionAttemptStatusSucceeded,
		Version:    2,
		StartedAt:  actionStartedAt,
		FinishedAt: &actionFinishedAt,
	}

	subject := VerificationSubject{
		Cluster:   "dev",
		Namespace: "default",
		Kind:      "Deployment",
		Name:      "crash-app",
		UID:       "deployment-uid-verification-subject",
	}

	command := BeginVerificationCommand{
		ActionAttempt: actionAttempt,
		Subject:       subject,
		StartedAt:     verificationStartedAt,
	}

	first, firstCreated, err := store.Begin(
		ctx,
		command,
	)
	if err != nil {
		t.Fatalf(
			"first Verification Begin() error = %v",
			err,
		)
	}
	if !firstCreated {
		t.Fatal(
			"first Verification Begin() created = false; want true",
		)
	}
	if first.Subject != subject {
		t.Fatalf(
			"first Verification Subject = %#v; want %#v",
			first.Subject,
			subject,
		)
	}
	if first.ActionKey.TargetUID == first.Subject.UID {
		t.Fatalf(
			"Verification Action target UID = %q; must differ from stable Subject UID %q",
			first.ActionKey.TargetUID,
			first.Subject.UID,
		)
	}

	second, secondCreated, err := store.Begin(
		ctx,
		command,
	)
	if err != nil {
		t.Fatalf(
			"second Verification Begin() error = %v",
			err,
		)
	}
	if secondCreated {
		t.Fatal(
			"second Verification Begin() created = true; want false",
		)
	}
	if second.Subject != subject {
		t.Fatalf(
			"second Verification Subject = %#v; want %#v",
			second.Subject,
			subject,
		)
	}
	if !reflect.DeepEqual(second, first) {
		t.Fatalf(
			"second Verification = %#v; want %#v",
			second,
			first,
		)
	}
}
func TestVerificationStoreBeginRejectsInvalidVerificationSubject(
	t *testing.T,
) {
	t.Parallel()

	actionFinishedAt := time.Date(
		2026,
		time.October,
		5,
		9,
		0,
		5,
		0,
		time.UTC,
	)
	actionStartedAt := actionFinishedAt.Add(-5 * time.Second)

	actionAttempt := ActionAttempt{
		ID: "att-invalid-verification-subject",
		Key: ExecutionKey{
			IncidentID:   "inc-invalid-verification-subject",
			PlanHash:     "plan-invalid-verification-subject",
			TargetUID:    "pod-uid-invalid-verification-subject",
			FencingToken: 13,
		},
		Status:     ActionAttemptStatusSucceeded,
		Version:    2,
		StartedAt:  actionStartedAt,
		FinishedAt: &actionFinishedAt,
	}

	validSubject := VerificationSubject{
		Cluster:   "dev",
		Namespace: "default",
		Kind:      "Deployment",
		Name:      "crash-app",
		UID:       "deployment-uid-invalid-subject",
	}

	tests := []struct {
		name    string
		subject VerificationSubject
	}{
		{
			name: "missing cluster",
			subject: VerificationSubject{
				Namespace: validSubject.Namespace,
				Kind:      validSubject.Kind,
				Name:      validSubject.Name,
				UID:       validSubject.UID,
			},
		},
		{
			name: "blank namespace",
			subject: VerificationSubject{
				Cluster:   validSubject.Cluster,
				Namespace: "   ",
				Kind:      validSubject.Kind,
				Name:      validSubject.Name,
				UID:       validSubject.UID,
			},
		},
		{
			name: "missing kind",
			subject: VerificationSubject{
				Cluster:   validSubject.Cluster,
				Namespace: validSubject.Namespace,
				Name:      validSubject.Name,
				UID:       validSubject.UID,
			},
		},
		{
			name: "missing name",
			subject: VerificationSubject{
				Cluster:   validSubject.Cluster,
				Namespace: validSubject.Namespace,
				Kind:      validSubject.Kind,
				UID:       validSubject.UID,
			},
		},
		{
			name: "missing UID",
			subject: VerificationSubject{
				Cluster:   validSubject.Cluster,
				Namespace: validSubject.Namespace,
				Kind:      validSubject.Kind,
				Name:      validSubject.Name,
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(
			test.name,
			func(t *testing.T) {
				t.Parallel()

				store := NewMemoryVerificationStore()

				_, _, err := store.Begin(
					context.Background(),
					BeginVerificationCommand{
						ActionAttempt: actionAttempt,
						Subject:       test.subject,
						StartedAt: actionFinishedAt.Add(
							time.Second,
						),
					},
				)
				if !errors.Is(
					err,
					ErrInvalidVerification,
				) {
					t.Fatalf(
						"Verification Begin() error = %v; want ErrInvalidVerification",
						err,
					)
				}
			},
		)
	}
}

func TestVerificationStoreBeginRejectsSubjectRebindingForExistingActionAttempt(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryVerificationStore()

	actionStartedAt := time.Date(
		2026,
		time.October,
		4,
		10,
		0,
		0,
		0,
		time.UTC,
	)
	actionFinishedAt := actionStartedAt.Add(5 * time.Second)

	actionAttempt := ActionAttempt{
		ID: "att-verification-subject-rebinding",
		Key: ExecutionKey{
			IncidentID: "inc-verification-subject-rebinding",
			PlanHash: "sha256:" +
				"0123456789abcdef0123456789abcdef" +
				"0123456789abcdef0123456789abcdef",
			TargetUID:    "pod-uid-before-action",
			FencingToken: 41,
		},
		Status:     ActionAttemptStatusSucceeded,
		Version:    2,
		StartedAt:  actionStartedAt,
		FinishedAt: &actionFinishedAt,
	}

	firstSubject := validTestVerificationSubject()
	verificationStartedAt := actionFinishedAt.Add(time.Second)

	first, created, err := store.Begin(
		ctx,
		BeginVerificationCommand{
			ActionAttempt: actionAttempt,
			Subject:       firstSubject,
			StartedAt:     verificationStartedAt,
		},
	)
	if err != nil {
		t.Fatalf("first Verification Begin() error = %v", err)
	}
	if !created {
		t.Fatal("first Verification Begin() created = false; want true")
	}

	differentSubject := firstSubject
	differentSubject.UID = "deployment-uid-replaced"

	_, created, err = store.Begin(
		ctx,
		BeginVerificationCommand{
			ActionAttempt: actionAttempt,
			Subject:       differentSubject,
			StartedAt:     verificationStartedAt,
		},
	)
	if !errors.Is(err, ErrVerificationSubjectConflict) {
		t.Fatalf(
			"subject rebinding Verification Begin() error = %v; "+
				"want ErrVerificationSubjectConflict",
			err,
		)
	}
	if created {
		t.Fatal(
			"subject rebinding Verification Begin() created = true; want false",
		)
	}

	reloaded, created, err := store.Begin(
		ctx,
		BeginVerificationCommand{
			ActionAttempt: actionAttempt,
			Subject:       firstSubject,
			StartedAt:     verificationStartedAt,
		},
	)
	if err != nil {
		t.Fatalf("exact retry Verification Begin() error = %v", err)
	}
	if created {
		t.Fatal("exact retry Verification Begin() created = true; want false")
	}
	if reloaded.ID != first.ID || reloaded.Subject != firstSubject {
		t.Fatalf(
			"exact retry Verification = %#v; want original %#v",
			reloaded,
			first,
		)
	}
}

func validTestVerificationSubject() VerificationSubject {
	return VerificationSubject{
		Cluster:   "dev",
		Namespace: "default",
		Kind:      "Deployment",
		Name:      "crash-app",
		UID:       "deployment-uid-crash-app",
	}
}
