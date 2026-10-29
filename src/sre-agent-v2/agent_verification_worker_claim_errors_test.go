package main

import (
	"context"
	"errors"
	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
	"testing"
	"time"
)

func TestAgentVerificationWorkerReturnsRecoveredResolutionClaimError(
	t *testing.T,
) {
	t.Parallel()

	claimErr := errors.New(
		"recovered verification scheduler unavailable",
	)
	workerAt := time.Date(
		2026,
		time.October,
		12,
		18,
		0,
		0,
		0,
		time.UTC,
	)

	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		recoveredClaimErr: claimErr,
	}

	agent := &sreAgent{
		config: agentConfig{
			IncidentClaimHolderID:      "verification-worker",
			IncidentClaimLeaseDuration: 30 * time.Second,
			KubernetesRequestTimeout:   time.Second,
		},
		verificationWorker: lifecycle,
		now: func() time.Time {
			return workerAt
		},
	}

	result, advanced, err := agent.runVerificationWorkerOnce(
		context.Background(),
	)

	if !errors.Is(err, claimErr) {
		t.Fatalf(
			"runVerificationWorkerOnce() error = %v; want %v",
			err,
			claimErr,
		)
	}

	if advanced {
		t.Fatal(
			"runVerificationWorkerOnce() advanced = true; want false",
		)
	}

	if result.ID != "" {
		t.Fatalf(
			"runVerificationWorkerOnce() Incident ID = %q; want empty",
			result.ID,
		)
	}

	if lifecycle.recoveredClaimCalls != 1 {
		t.Fatalf(
			"ClaimRecoveredVerificationResolution calls = %d; want 1",
			lifecycle.recoveredClaimCalls,
		)
	}

	if lifecycle.claimCalls != 0 {
		t.Fatalf(
			"ClaimPendingVerification calls = %d; want 0",
			lifecycle.claimCalls,
		)
	}

	if lifecycle.completeCalls != 0 {
		t.Fatalf(
			"CompleteFencedVerification calls = %d; want 0",
			lifecycle.completeCalls,
		)
	}

	if lifecycle.resolveCalls != 0 {
		t.Fatalf(
			"Resolve calls = %d; want 0",
			lifecycle.resolveCalls,
		)
	}
}
func TestAgentVerificationWorkerReturnsRecoveredResolutionResolveError(
	t *testing.T,
) {
	t.Parallel()

	resolveErr := errors.New(
		"recovered verification resolution failed",
	)
	workerAt := time.Date(
		2026,
		time.October,
		12,
		18,
		30,
		0,
		0,
		time.UTC,
	)

	verifyingIncident := incidentdomain.Incident{
		ID:      "inc-recovered-resolution-error",
		State:   incidentdomain.StateVerifying,
		Version: 7,
	}
	recoveredVerification := remediationdomain.Verification{
		ID: "ver-recovered-resolution-error",
		ActionKey: remediationdomain.ActionKey{
			IncidentID: verifyingIncident.ID,
		},
		Status:  remediationdomain.VerificationStatusRecovered,
		Version: 2,
	}

	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		recoveredClaimed: true,
		recoveredWork: incidentdomain.
			RecoveredVerificationResolutionClaim{
			Claim: incidentdomain.Claim{
				Incident:  verifyingIncident,
				HolderID:  "verification-worker",
				ExpiresAt: workerAt.Add(30 * time.Second),
			},
			Verification: recoveredVerification,
		},
		resolveErr: resolveErr,
	}

	agent := &sreAgent{
		config: agentConfig{
			IncidentClaimHolderID:      "verification-worker",
			IncidentClaimLeaseDuration: 30 * time.Second,
			KubernetesRequestTimeout:   time.Second,
		},
		verificationWorker: lifecycle,
		now: func() time.Time {
			return workerAt
		},
	}

	result, advanced, err := agent.runVerificationWorkerOnce(
		context.Background(),
	)

	if !errors.Is(err, resolveErr) {
		t.Fatalf(
			"runVerificationWorkerOnce() error = %v; want %v",
			err,
			resolveErr,
		)
	}

	if advanced {
		t.Fatal(
			"runVerificationWorkerOnce() advanced = true; want false",
		)
	}

	if result.ID != "" {
		t.Fatalf(
			"runVerificationWorkerOnce() Incident ID = %q; want empty",
			result.ID,
		)
	}

	if lifecycle.recoveredClaimCalls != 1 {
		t.Fatalf(
			"ClaimRecoveredVerificationResolution calls = %d; want 1",
			lifecycle.recoveredClaimCalls,
		)
	}

	if lifecycle.claimCalls != 0 {
		t.Fatalf(
			"ClaimPendingVerification calls = %d; want 0",
			lifecycle.claimCalls,
		)
	}

	if lifecycle.resolveCalls != 1 {
		t.Fatalf(
			"Resolve calls = %d; want 1",
			lifecycle.resolveCalls,
		)
	}

	if len(lifecycle.resolveCommands) != 1 {
		t.Fatalf(
			"Resolve commands = %d; want 1",
			len(lifecycle.resolveCommands),
		)
	}

	resolveCommand := lifecycle.resolveCommands[0]

	if resolveCommand.IncidentID != verifyingIncident.ID {
		t.Fatalf(
			"Resolve IncidentID = %q; want %q",
			resolveCommand.IncidentID,
			verifyingIncident.ID,
		)
	}

	if resolveCommand.ExpectedVersion != verifyingIncident.Version {
		t.Fatalf(
			"Resolve ExpectedVersion = %d; want %d",
			resolveCommand.ExpectedVersion,
			verifyingIncident.Version,
		)
	}

	if resolveCommand.VerificationID != recoveredVerification.ID {
		t.Fatalf(
			"Resolve VerificationID = %q; want %q",
			resolveCommand.VerificationID,
			recoveredVerification.ID,
		)
	}
}
func TestAgentVerificationWorkerReturnsPendingVerificationClaimError(
	t *testing.T,
) {
	t.Parallel()

	claimErr := errors.New(
		"pending verification scheduler unavailable",
	)
	workerAt := time.Date(
		2026,
		time.October,
		12,
		19,
		0,
		0,
		0,
		time.UTC,
	)

	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		claimErr: claimErr,
	}
	executor := &agentVerificationExecutorProbe{}

	agent := &sreAgent{
		config: agentConfig{
			IncidentClaimHolderID:      "verification-worker",
			IncidentClaimLeaseDuration: 30 * time.Second,
			KubernetesRequestTimeout:   time.Second,
		},
		verificationWorker:   lifecycle,
		verificationExecutor: executor,
		now: func() time.Time {
			return workerAt
		},
	}

	result, advanced, err := agent.runVerificationWorkerOnce(
		context.Background(),
	)

	if !errors.Is(err, claimErr) {
		t.Fatalf(
			"runVerificationWorkerOnce() error = %v; want %v",
			err,
			claimErr,
		)
	}

	if advanced {
		t.Fatal(
			"runVerificationWorkerOnce() advanced = true; want false",
		)
	}

	if result.ID != "" {
		t.Fatalf(
			"runVerificationWorkerOnce() Incident ID = %q; want empty",
			result.ID,
		)
	}

	if lifecycle.recoveredClaimCalls != 1 {
		t.Fatalf(
			"ClaimRecoveredVerificationResolution calls = %d; want 1",
			lifecycle.recoveredClaimCalls,
		)
	}

	if lifecycle.claimCalls != 1 {
		t.Fatalf(
			"ClaimPendingVerification calls = %d; want 1",
			lifecycle.claimCalls,
		)
	}

	if lifecycle.completeCalls != 0 {
		t.Fatalf(
			"CompleteFencedVerification calls = %d; want 0",
			lifecycle.completeCalls,
		)
	}

	if lifecycle.resolveCalls != 0 {
		t.Fatalf(
			"Resolve calls = %d; want 0",
			lifecycle.resolveCalls,
		)
	}

	if executor.calls != 0 {
		t.Fatalf(
			"Verification executor calls = %d; want 0",
			executor.calls,
		)
	}
}
