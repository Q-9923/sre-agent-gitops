//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	remediationdomain "sre-agent/internal/remediation"
)

func TestVerificationStoreBeginSurvivesAdapterRestartPostgreSQL(
	t *testing.T,
) {
	pool, beginAttemptCommand := newActionAttemptPostgresFixture(
		t,
		"verification-adapter-restart",
	)
	ctx := context.Background()

	actionAttempts := NewActionAttemptStore(pool)

	startedAttempt, created, err := actionAttempts.Begin(
		ctx,
		beginAttemptCommand,
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !created {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	actionFinishedAt := beginAttemptCommand.StartedAt.Add(
		5 * time.Second,
	)

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             beginAttemptCommand.Key,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: actionFinishedAt,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	subject := postgresTestVerificationSubject()

	command := remediationdomain.BeginVerificationCommand{
		ActionAttempt: succeededAttempt,
		Subject:       subject,
		StartedAt:     actionFinishedAt.Add(time.Second),
	}

	firstStore := NewVerificationStore(pool)

	first, created, err := firstStore.Begin(ctx, command)
	if err != nil {
		t.Fatalf("first Verification Begin() error = %v", err)
	}
	if !created {
		t.Fatal("first Verification Begin() created = false; want true")
	}
	if first.Subject != subject {
		t.Fatalf(
			"first Verification Subject = %#v; want %#v",
			first.Subject,
			subject,
		)
	}
	if first.Status != remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"first Verification Status = %q; want %q",
			first.Status,
			remediationdomain.VerificationStatusPending,
		)
	}
	if first.Version != 1 {
		t.Fatalf(
			"first Verification Version = %d; want 1",
			first.Version,
		)
	}

	restartedStore := NewVerificationStore(pool)

	second, created, err := restartedStore.Begin(ctx, command)
	if err != nil {
		t.Fatalf("second Verification Begin() error = %v", err)
	}
	if created {
		t.Fatal("second Verification Begin() created = true; want false")
	}

	if second.ID != first.ID {
		t.Fatalf(
			"second Verification ID = %q; want %q",
			second.ID,
			first.ID,
		)
	}
	if second.ActionAttemptID != first.ActionAttemptID {
		t.Fatalf(
			"second ActionAttemptID = %q; want %q",
			second.ActionAttemptID,
			first.ActionAttemptID,
		)
	}
	if second.ActionKey != first.ActionKey {
		t.Fatalf(
			"second ActionKey = %#v; want %#v",
			second.ActionKey,
			first.ActionKey,
		)
	}
	if second.Subject != subject {
		t.Fatalf(
			"second Verification Subject = %#v; want %#v",
			second.Subject,
			subject,
		)
	}
	if second.Status != first.Status {
		t.Fatalf(
			"second Status = %q; want %q",
			second.Status,
			first.Status,
		)
	}
	if second.Version != first.Version {
		t.Fatalf(
			"second Version = %d; want %d",
			second.Version,
			first.Version,
		)
	}
	if !second.StartedAt.Equal(first.StartedAt) {
		t.Fatalf(
			"second StartedAt = %s; want same instant as %s",
			second.StartedAt,
			first.StartedAt,
		)
	}
	if second.FinishedAt != nil {
		t.Fatalf(
			"second FinishedAt = %s; want nil",
			second.FinishedAt,
		)
	}
	if second.EvidenceCode != first.EvidenceCode {
		t.Fatalf(
			"second EvidenceCode = %q; want %q",
			second.EvidenceCode,
			first.EvidenceCode,
		)
	}

	conflictCommand := command
	conflictCommand.Subject.UID = "deployment-uid-rebound"

	_, created, err = restartedStore.Begin(
		ctx,
		conflictCommand,
	)
	if !errors.Is(
		err,
		remediationdomain.ErrVerificationSubjectConflict,
	) {
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
}

func TestVerificationStoreCompletionSurvivesAdapterRestartPostgreSQL(

	t *testing.T,
) {
	pool, beginAttemptCommand := newActionAttemptPostgresFixture(
		t,
		"verification-completion-restart",
	)
	ctx := context.Background()

	actionAttempts := NewActionAttemptStore(pool)

	startedAttempt, created, err := actionAttempts.Begin(
		ctx,
		beginAttemptCommand,
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !created {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	actionFinishedAt := beginAttemptCommand.StartedAt.Add(
		5 * time.Second,
	)

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             beginAttemptCommand.Key,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: actionFinishedAt,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	beginVerificationCommand :=
		remediationdomain.BeginVerificationCommand{
			ActionAttempt: succeededAttempt,
			Subject:       postgresTestVerificationSubject(),
			StartedAt:     actionFinishedAt.Add(time.Second),
		}

	firstStore := NewVerificationStore(pool)

	pending, created, err := firstStore.Begin(
		ctx,
		beginVerificationCommand,
	)
	if err != nil {
		t.Fatalf("Verification Begin() error = %v", err)
	}
	if !created {
		t.Fatal("Verification Begin() created = false; want true")
	}

	verificationFinishedAt := beginVerificationCommand.
		StartedAt.
		Add(30 * time.Second)

	completeCommand :=
		remediationdomain.CompleteVerificationCommand{
			ActionKey:       pending.ActionKey,
			ExpectedVersion: pending.Version,
			To: remediationdomain.
				VerificationStatusRecovered,
			FinishedAt:   verificationFinishedAt,
			EvidenceCode: "TARGET_RECOVERED",
		}

	completed, err := firstStore.Complete(
		ctx,
		completeCommand,
	)
	if err != nil {
		t.Fatalf("Verification Complete() error = %v", err)
	}

	if completed.Status !=
		remediationdomain.VerificationStatusRecovered {
		t.Fatalf(
			"completed Status = %q; want %q",
			completed.Status,
			remediationdomain.VerificationStatusRecovered,
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

	restartedStore := NewVerificationStore(pool)

	reloaded, created, err := restartedStore.Begin(
		ctx,
		beginVerificationCommand,
	)
	if err != nil {
		t.Fatalf(
			"restarted Verification Begin() error = %v",
			err,
		)
	}
	if created {
		t.Fatal(
			"restarted Verification Begin() created = true; want false",
		)
	}

	if reloaded.ID != completed.ID {
		t.Fatalf(
			"reloaded ID = %q; want %q",
			reloaded.ID,
			completed.ID,
		)
	}
	if reloaded.ActionAttemptID != completed.ActionAttemptID {
		t.Fatalf(
			"reloaded ActionAttemptID = %q; want %q",
			reloaded.ActionAttemptID,
			completed.ActionAttemptID,
		)
	}
	if reloaded.ActionKey != completed.ActionKey {
		t.Fatalf(
			"reloaded ActionKey = %#v; want %#v",
			reloaded.ActionKey,
			completed.ActionKey,
		)
	}
	if reloaded.Status != completed.Status {
		t.Fatalf(
			"reloaded Status = %q; want %q",
			reloaded.Status,
			completed.Status,
		)
	}
	if reloaded.Version != completed.Version {
		t.Fatalf(
			"reloaded Version = %d; want %d",
			reloaded.Version,
			completed.Version,
		)
	}
	if reloaded.FinishedAt == nil {
		t.Fatal("reloaded FinishedAt = nil; want timestamp")
	}
	if !reloaded.FinishedAt.Equal(*completed.FinishedAt) {
		t.Fatalf(
			"reloaded FinishedAt = %s; want same instant as %s",
			reloaded.FinishedAt,
			completed.FinishedAt,
		)
	}
	if reloaded.EvidenceCode != completed.EvidenceCode {
		t.Fatalf(
			"reloaded EvidenceCode = %q; want %q",
			reloaded.EvidenceCode,
			completed.EvidenceCode,
		)
	}

	retried, err := restartedStore.Complete(
		ctx,
		completeCommand,
	)
	if err != nil {
		t.Fatalf(
			"exact retry Verification Complete() error = %v; want nil",
			err,
		)
	}
	if retried.Status != completed.Status ||
		retried.Version != completed.Version ||
		retried.FinishedAt == nil ||
		!retried.FinishedAt.Equal(*completed.FinishedAt) ||
		retried.EvidenceCode != completed.EvidenceCode {
		t.Fatalf(
			"exact retry Verification = %#v; want %#v",
			retried,
			completed,
		)
	}
}
func TestVerificationStoreConcurrentCompletionAllowsOneOutcomePostgreSQL(
	t *testing.T,
) {
	pool, beginAttemptCommand := newActionAttemptPostgresFixture(
		t,
		"verification-concurrent-completion",
	)
	ctx := context.Background()

	actionAttempts := NewActionAttemptStore(pool)

	startedAttempt, created, err := actionAttempts.Begin(
		ctx,
		beginAttemptCommand,
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !created {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	actionFinishedAt := beginAttemptCommand.StartedAt.Add(
		5 * time.Second,
	)

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             beginAttemptCommand.Key,
			ExpectedVersion: startedAttempt.Version,
			To:              remediationdomain.ActionAttemptStatusSucceeded,
			FinishedAt:      actionFinishedAt,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	store := NewVerificationStore(pool)
	verificationStartedAt := actionFinishedAt.Add(time.Second)

	pending, created, err := store.Begin(
		ctx,
		remediationdomain.BeginVerificationCommand{
			ActionAttempt: succeededAttempt,
			Subject:       postgresTestVerificationSubject(),
			StartedAt:     verificationStartedAt,
		},
	)
	if err != nil {
		t.Fatalf("Verification Begin() error = %v", err)
	}
	if !created {
		t.Fatal("Verification Begin() created = false; want true")
	}

	finishedAt := verificationStartedAt.Add(10 * time.Second)

	commands := []remediationdomain.CompleteVerificationCommand{
		{
			ActionKey:       pending.ActionKey,
			ExpectedVersion: pending.Version,
			To:              remediationdomain.VerificationStatusRecovered,
			FinishedAt:      finishedAt,
			EvidenceCode:    "TARGET_HEALTHY",
		},
		{
			ActionKey:       pending.ActionKey,
			ExpectedVersion: pending.Version,
			To:              remediationdomain.VerificationStatusNotRecovered,
			FinishedAt:      finishedAt,
			EvidenceCode:    "TARGET_STILL_UNHEALTHY",
		},
	}

	type completionResult struct {
		verification remediationdomain.Verification
		err          error
	}

	start := make(chan struct{})
	results := make(chan completionResult, len(commands))

	for _, command := range commands {
		command := command

		go func() {
			<-start

			completed, completeErr := store.Complete(
				ctx,
				command,
			)
			results <- completionResult{
				verification: completed,
				err:          completeErr,
			}
		}()
	}

	close(start)

	successCount := 0
	conflictCount := 0
	var winner remediationdomain.Verification

	for range commands {
		result := <-results

		switch {
		case result.err == nil:
			successCount++
			winner = result.verification
		case errors.Is(
			result.err,
			remediationdomain.ErrVerificationVersionConflict,
		):
			conflictCount++
		default:
			t.Fatalf(
				"concurrent Verification Complete() error = %v; want nil or ErrVerificationVersionConflict",
				result.err,
			)
		}
	}

	if successCount != 1 {
		t.Fatalf(
			"successful Verification Complete calls = %d; want 1",
			successCount,
		)
	}
	if conflictCount != 1 {
		t.Fatalf(
			"conflicting Verification Complete calls = %d; want 1",
			conflictCount,
		)
	}
	if winner.Version != pending.Version+1 {
		t.Fatalf(
			"winner Version = %d; want %d",
			winner.Version,
			pending.Version+1,
		)
	}
	if winner.Status != remediationdomain.VerificationStatusRecovered &&
		winner.Status != remediationdomain.VerificationStatusNotRecovered {
		t.Fatalf(
			"winner Status = %q; want RECOVERED or NOT_RECOVERED",
			winner.Status,
		)
	}

	restartedStore := NewVerificationStore(pool)

	reloaded, reloadedCreated, err := restartedStore.Begin(
		ctx,
		remediationdomain.BeginVerificationCommand{
			ActionAttempt: succeededAttempt,
			Subject:       postgresTestVerificationSubject(),
			StartedAt:     verificationStartedAt,
		},
	)
	if err != nil {
		t.Fatalf(
			"restarted Verification Begin() error = %v",
			err,
		)
	}
	if reloadedCreated {
		t.Fatal(
			"restarted Verification Begin() created = true; want existing result",
		)
	}
	if reloaded.Status != winner.Status {
		t.Fatalf(
			"reloaded Status = %q; want %q",
			reloaded.Status,
			winner.Status,
		)
	}
	if reloaded.Version != winner.Version {
		t.Fatalf(
			"reloaded Version = %d; want %d",
			reloaded.Version,
			winner.Version,
		)
	}
	if reloaded.EvidenceCode != winner.EvidenceCode {
		t.Fatalf(
			"reloaded EvidenceCode = %q; want %q",
			reloaded.EvidenceCode,
			winner.EvidenceCode,
		)
	}
}

func postgresTestVerificationSubject() remediationdomain.VerificationSubject {
	return remediationdomain.VerificationSubject{
		Cluster:   "dev",
		Namespace: "default",
		Kind:      "Deployment",
		Name:      "crash-app",
		UID:       "deployment-uid-crash-app",
	}
}
