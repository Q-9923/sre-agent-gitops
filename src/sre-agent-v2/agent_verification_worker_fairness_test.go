package main

import (
	"context"
	"errors"
	"testing"
	"time"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func TestAgentVerificationWorkerDoesNotStarvePendingWorkWhenRecoveredResolutionFails(
	t *testing.T,
) {
	t.Parallel()

	now := time.Date(2026, time.October, 14, 13, 0, 0, 0, time.UTC)
	pending, completion := verificationWorkerFailureFixture(now)
	completion.Verification.Status =
		remediationdomain.VerificationStatusNotRecovered
	completion.Verification.EvidenceCode = "WORKLOAD_NOT_RECOVERED"

	resolutionErr := errors.New("resolution backend unavailable")
	pendingClaims := 0
	completions := 0
	lifecycle := &verificationWorkerLifecycleAdapter{
		claimRecovered: func(
			context.Context,
			incidentdomain.ClaimRecoveredVerificationResolutionCommand,
		) (incidentdomain.RecoveredVerificationResolutionClaim, bool, error) {
			return incidentdomain.RecoveredVerificationResolutionClaim{
				Claim:        pending.Claim,
				Verification: completion.Verification,
			}, true, nil
		},
		claimPending: func(
			context.Context,
			incidentdomain.ClaimPendingVerificationCommand,
		) (incidentdomain.PendingVerificationClaim, bool, error) {
			pendingClaims++
			return pending, true, nil
		},
		complete: func(
			context.Context,
			incidentdomain.CompleteFencedVerificationCommand,
		) (incidentdomain.FencedVerificationCompletionResult, error) {
			completions++
			return completion, nil
		},
		resolve: func(
			context.Context,
			incidentdomain.ResolveCommand,
		) (incidentdomain.Incident, error) {
			return incidentdomain.Incident{}, resolutionErr
		},
	}

	agent, _ := newVerificationWorkerTestAgent()
	agent.now = func() time.Time { return now }
	agent.verificationWorker = lifecycle
	agent.verificationExecutor = verificationExecutorFunc(func(
		context.Context,
		remediationdomain.Verification,
	) (verificationExecutionResult, error) {
		return verificationExecutionResult{
			Status:       remediationdomain.VerificationStatusNotRecovered,
			EvidenceCode: "WORKLOAD_NOT_RECOVERED",
		}, nil
	})

	incident, advanced, err := agent.runVerificationWorkerOnce(
		context.Background(),
	)
	if !errors.Is(err, resolutionErr) {
		t.Fatalf(
			"runVerificationWorkerOnce() error = %v; want resolution error",
			err,
		)
	}
	if !advanced {
		t.Fatal("runVerificationWorkerOnce() advanced = false; want true")
	}
	if incident.ID != completion.Incident.ID {
		t.Fatalf(
			"runVerificationWorkerOnce() Incident ID = %q; want %q",
			incident.ID,
			completion.Incident.ID,
		)
	}
	if pendingClaims != 1 {
		t.Fatalf("pending Claim calls = %d; want 1", pendingClaims)
	}
	if completions != 1 {
		t.Fatalf("completion calls = %d; want 1", completions)
	}
}

func TestAgentVerificationWorkerRunDrainsBoundedPendingBatchBeforePolling(
	t *testing.T,
) {
	t.Parallel()

	now := time.Date(2026, time.October, 14, 13, 30, 0, 0, time.UTC)
	pending, completion := verificationWorkerFailureFixture(now)
	completion.Verification.Status =
		remediationdomain.VerificationStatusNotRecovered
	completion.Verification.EvidenceCode = "WORKLOAD_NOT_RECOVERED"

	pendingClaims := 0
	completions := 0
	lifecycle := &verificationWorkerLifecycleAdapter{
		claimPending: func(
			context.Context,
			incidentdomain.ClaimPendingVerificationCommand,
		) (incidentdomain.PendingVerificationClaim, bool, error) {
			pendingClaims++

			work := pending
			work.Verification.ID += string(rune('0' + pendingClaims))
			return work, true, nil
		},
		complete: func(
			context.Context,
			incidentdomain.CompleteFencedVerificationCommand,
		) (incidentdomain.FencedVerificationCompletionResult, error) {
			completions++
			return completion, nil
		},
	}

	agent, _ := newVerificationWorkerTestAgent()
	agent.now = func() time.Time { return now }
	agent.verificationWorker = lifecycle
	agent.verificationExecutor = verificationExecutorFunc(func(
		context.Context,
		remediationdomain.Verification,
	) (verificationExecutionResult, error) {
		return verificationExecutionResult{
			Status:       remediationdomain.VerificationStatusNotRecovered,
			EvidenceCode: "WORKLOAD_NOT_RECOVERED",
		}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	agent.waitVerificationWorker = func(
		context.Context,
		time.Duration,
	) error {
		waits++
		cancel()
		return context.Canceled
	}

	agent.runVerificationWorker(ctx)

	if completions != verificationWorkerMaxDrainItems {
		t.Fatalf(
			"completion calls = %d; want bounded maximum %d",
			completions,
			verificationWorkerMaxDrainItems,
		)
	}
	if pendingClaims != verificationWorkerMaxDrainItems {
		t.Fatalf(
			"pending Claim calls = %d; want bounded maximum %d",
			pendingClaims,
			verificationWorkerMaxDrainItems,
		)
	}
	if waits != 1 {
		t.Fatalf("Worker waits = %d; want 1", waits)
	}
}
