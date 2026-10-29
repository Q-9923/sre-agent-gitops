package main

import (
	"context"
	"errors"
	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
	"testing"
	"time"
)

func TestAgentVerificationWorkerReturnsVerificationExecutorError(
	t *testing.T,
) {
	t.Parallel()

	executionErr := errors.New(
		"verification evidence source unavailable",
	)
	workerAt := time.Date(
		2026,
		time.October,
		12,
		19,
		30,
		0,
		0,
		time.UTC,
	)

	verifyingIncident := incidentdomain.Incident{
		ID:      "inc-verification-executor-error",
		State:   incidentdomain.StateVerifying,
		Version: 8,
	}
	pendingVerification := remediationdomain.Verification{
		ID: "ver-verification-executor-error",
		ActionKey: remediationdomain.ActionKey{
			IncidentID: verifyingIncident.ID,
			TargetUID:  "original-pod-uid",
		},
		Subject: remediationdomain.VerificationSubject{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "ReplicaSet",
			Name:      "crash-app-rs",
			UID:       "replicaset-uid",
		},
		Status:  remediationdomain.VerificationStatusPending,
		Version: 1,
	}

	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		work: incidentdomain.PendingVerificationClaim{
			Claim: incidentdomain.Claim{
				Incident:  verifyingIncident,
				HolderID:  "verification-worker",
				ExpiresAt: workerAt.Add(30 * time.Second),
			},
			Verification: pendingVerification,
		},
	}
	executor := &agentVerificationExecutorProbe{
		err: executionErr,
	}

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

	if !errors.Is(err, executionErr) {
		t.Fatalf(
			"runVerificationWorkerOnce() error = %v; want %v",
			err,
			executionErr,
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

	if executor.calls != 1 {
		t.Fatalf(
			"Verification executor calls = %d; want 1",
			executor.calls,
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

type agentVerificationWorkerLateSuccessExecutor struct {
	calls int
}

func (
	executor *agentVerificationWorkerLateSuccessExecutor,
) Verify(
	ctx context.Context,
	_ remediationdomain.Verification,
) (verificationExecutionResult, error) {
	executor.calls++

	<-ctx.Done()

	return verificationExecutionResult{
		Status:       remediationdomain.VerificationStatusRecovered,
		EvidenceCode: "LATE_RECOVERY_RESULT",
	}, nil
}

func TestAgentVerificationWorkerReturnsExecutionDeadlineError(
	t *testing.T,
) {
	t.Parallel()

	workerAt := time.Date(
		2026,
		time.October,
		12,
		17,
		0,
		0,
		0,
		time.UTC,
	)

	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		work: incidentdomain.PendingVerificationClaim{
			Verification: remediationdomain.Verification{
				ID:      "ver-worker-late-result",
				Status:  remediationdomain.VerificationStatusPending,
				Version: 1,
			},
			Claim: incidentdomain.Claim{
				Incident: incidentdomain.Incident{
					ID:      "inc-worker-late-result",
					State:   incidentdomain.StateVerifying,
					Version: 8,
				},
				HolderID: "verification-worker",
				ExpiresAt: workerAt.Add(
					30 * time.Second,
				),
			},
		},
	}
	executor := &agentVerificationWorkerLateSuccessExecutor{}

	agent := &sreAgent{
		config: agentConfig{
			IncidentClaimHolderID: "verification-worker",
			IncidentClaimLeaseDuration: 30 *
				time.Second,
			KubernetesRequestTimeout: 10 *
				time.Millisecond,
		},
		verificationWorker:   lifecycle,
		verificationExecutor: executor,
		now: func() time.Time {
			return workerAt
		},
	}

	result, advanced, err :=
		agent.runVerificationWorkerOnce(
			context.Background(),
		)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf(
			"runVerificationWorkerOnce() error = %v; "+
				"want context deadline exceeded",
			err,
		)
	}
	if advanced {
		t.Fatal(
			"runVerificationWorkerOnce() advanced = true; " +
				"want false",
		)
	}
	if result.ID != "" {
		t.Fatalf(
			"runVerificationWorkerOnce() Incident ID = %q; "+
				"want empty",
			result.ID,
		)
	}
	if lifecycle.recoveredClaimCalls != 1 {
		t.Fatalf(
			"recovered resolution Claim calls = %d; want 1",
			lifecycle.recoveredClaimCalls,
		)
	}
	if lifecycle.claimCalls != 1 {
		t.Fatalf(
			"pending Verification Claim calls = %d; want 1",
			lifecycle.claimCalls,
		)
	}
	if executor.calls != 1 {
		t.Fatalf(
			"Verification executor calls = %d; want 1",
			executor.calls,
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
func TestAgentVerificationWorkerReturnsInvalidExecutionResultError(
	t *testing.T,
) {
	t.Parallel()

	workerAt := time.Date(
		2026,
		time.October,
		12,
		17,
		30,
		0,
		0,
		time.UTC,
	)

	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		work: incidentdomain.PendingVerificationClaim{
			Verification: remediationdomain.Verification{
				ID:      "ver-worker-invalid-result",
				Status:  remediationdomain.VerificationStatusPending,
				Version: 1,
			},
			Claim: incidentdomain.Claim{
				Incident: incidentdomain.Incident{
					ID:      "inc-worker-invalid-result",
					State:   incidentdomain.StateVerifying,
					Version: 9,
				},
				HolderID: "verification-worker",
				ExpiresAt: workerAt.Add(
					30 * time.Second,
				),
			},
		},
	}
	executor := &agentVerificationExecutorProbe{
		result: verificationExecutionResult{
			Status: remediationdomain.
				VerificationStatusPending,
			EvidenceCode: "NON_TERMINAL_RESULT",
		},
	}

	agent := &sreAgent{
		config: agentConfig{
			IncidentClaimHolderID: "verification-worker",
			IncidentClaimLeaseDuration: 30 *
				time.Second,
			KubernetesRequestTimeout: time.Second,
		},
		verificationWorker:   lifecycle,
		verificationExecutor: executor,
		now: func() time.Time {
			return workerAt
		},
	}

	result, advanced, err :=
		agent.runVerificationWorkerOnce(
			context.Background(),
		)

	if err == nil {
		t.Fatal(
			"runVerificationWorkerOnce() error = nil; " +
				"want invalid Verification execution result error",
		)
	}
	if advanced {
		t.Fatal(
			"runVerificationWorkerOnce() advanced = true; " +
				"want false",
		)
	}
	if result.ID != "" {
		t.Fatalf(
			"runVerificationWorkerOnce() Incident ID = %q; "+
				"want empty",
			result.ID,
		)
	}
	if lifecycle.recoveredClaimCalls != 1 {
		t.Fatalf(
			"recovered resolution Claim calls = %d; want 1",
			lifecycle.recoveredClaimCalls,
		)
	}
	if lifecycle.claimCalls != 1 {
		t.Fatalf(
			"pending Verification Claim calls = %d; want 1",
			lifecycle.claimCalls,
		)
	}
	if executor.calls != 1 {
		t.Fatalf(
			"Verification executor calls = %d; want 1",
			executor.calls,
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
func TestAgentVerificationWorkerReturnsFencedCompletionError(
	t *testing.T,
) {
	t.Parallel()

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
	completionErr := errors.New(
		"fenced verification completion failed",
	)

	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		work: incidentdomain.PendingVerificationClaim{
			Verification: remediationdomain.Verification{
				ID:      "ver-worker-completion-error",
				Status:  remediationdomain.VerificationStatusPending,
				Version: 1,
			},
			Claim: incidentdomain.Claim{
				Incident: incidentdomain.Incident{
					ID:      "inc-worker-completion-error",
					State:   incidentdomain.StateVerifying,
					Version: 10,
				},
				HolderID: "verification-worker",
				ExpiresAt: workerAt.Add(
					30 * time.Second,
				),
			},
		},
		completionErr: completionErr,
	}
	executor := &agentVerificationExecutorProbe{
		result: verificationExecutionResult{
			Status: remediationdomain.
				VerificationStatusRecovered,
			EvidenceCode: "WORKLOAD_RECOVERED",
		},
	}

	agent := &sreAgent{
		config: agentConfig{
			IncidentClaimHolderID: "verification-worker",
			IncidentClaimLeaseDuration: 30 *
				time.Second,
			KubernetesRequestTimeout: time.Second,
		},
		verificationWorker:   lifecycle,
		verificationExecutor: executor,
		now: func() time.Time {
			return workerAt
		},
	}

	result, advanced, err :=
		agent.runVerificationWorkerOnce(
			context.Background(),
		)

	if !errors.Is(err, completionErr) {
		t.Fatalf(
			"runVerificationWorkerOnce() error = %v; want %v",
			err,
			completionErr,
		)
	}
	if advanced {
		t.Fatal(
			"runVerificationWorkerOnce() advanced = true; " +
				"want false",
		)
	}
	if result.ID != "" {
		t.Fatalf(
			"runVerificationWorkerOnce() Incident ID = %q; "+
				"want empty",
			result.ID,
		)
	}
	if lifecycle.recoveredClaimCalls != 1 {
		t.Fatalf(
			"recovered resolution Claim calls = %d; want 1",
			lifecycle.recoveredClaimCalls,
		)
	}
	if lifecycle.claimCalls != 1 {
		t.Fatalf(
			"pending Verification Claim calls = %d; want 1",
			lifecycle.claimCalls,
		)
	}
	if executor.calls != 1 {
		t.Fatalf(
			"Verification executor calls = %d; want 1",
			executor.calls,
		)
	}
	if lifecycle.completeCalls != 1 {
		t.Fatalf(
			"CompleteFencedVerification calls = %d; want 1",
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
func TestAgentVerificationWorkerReturnsResolveErrorAfterRecoveredCompletion(
	t *testing.T,
) {
	t.Parallel()

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
	resolveErr := errors.New(
		"recovered verification resolution failed",
	)

	verifyingIncident := incidentdomain.Incident{
		ID:      "inc-worker-post-completion-resolve-error",
		State:   incidentdomain.StateVerifying,
		Version: 11,
	}
	pendingVerification := remediationdomain.Verification{
		ID:      "ver-worker-post-completion-resolve-error",
		Status:  remediationdomain.VerificationStatusPending,
		Version: 1,
	}
	recoveredVerification := pendingVerification
	recoveredVerification.Status =
		remediationdomain.VerificationStatusRecovered
	recoveredVerification.Version = 2
	recoveredVerification.EvidenceCode = "WORKLOAD_RECOVERED"

	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		work: incidentdomain.PendingVerificationClaim{
			Verification: pendingVerification,
			Claim: incidentdomain.Claim{
				Incident: verifyingIncident,
				HolderID: "verification-worker",
				ExpiresAt: workerAt.Add(
					30 * time.Second,
				),
			},
		},
		completion: incidentdomain.
			FencedVerificationCompletionResult{
			Incident:     verifyingIncident,
			Verification: recoveredVerification,
		},
		resolveErr: resolveErr,
	}
	executor := &agentVerificationExecutorProbe{
		result: verificationExecutionResult{
			Status: remediationdomain.
				VerificationStatusRecovered,
			EvidenceCode: "WORKLOAD_RECOVERED",
		},
	}

	agent := &sreAgent{
		config: agentConfig{
			IncidentClaimHolderID: "verification-worker",
			IncidentClaimLeaseDuration: 30 *
				time.Second,
			KubernetesRequestTimeout: time.Second,
		},
		verificationWorker:   lifecycle,
		verificationExecutor: executor,
		now: func() time.Time {
			return workerAt
		},
	}

	result, advanced, err :=
		agent.runVerificationWorkerOnce(
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
			"runVerificationWorkerOnce() advanced = true; " +
				"want false",
		)
	}
	if result.ID != "" {
		t.Fatalf(
			"runVerificationWorkerOnce() Incident ID = %q; "+
				"want empty",
			result.ID,
		)
	}
	if lifecycle.recoveredClaimCalls != 1 {
		t.Fatalf(
			"recovered resolution Claim calls = %d; want 1",
			lifecycle.recoveredClaimCalls,
		)
	}
	if lifecycle.claimCalls != 1 {
		t.Fatalf(
			"pending Verification Claim calls = %d; want 1",
			lifecycle.claimCalls,
		)
	}
	if executor.calls != 1 {
		t.Fatalf(
			"Verification executor calls = %d; want 1",
			executor.calls,
		)
	}
	if lifecycle.completeCalls != 1 {
		t.Fatalf(
			"CompleteFencedVerification calls = %d; want 1",
			lifecycle.completeCalls,
		)
	}
	if lifecycle.resolveCalls != 1 {
		t.Fatalf(
			"Resolve calls = %d; want 1",
			lifecycle.resolveCalls,
		)
	}
}
