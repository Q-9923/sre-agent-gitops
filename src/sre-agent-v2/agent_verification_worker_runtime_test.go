package main

import (
	"bytes"
	"context"
	"errors"
	"k8s.io/client-go/kubernetes/fake"
	"log/slog"
	approvaldomain "sre-agent/internal/approval"
	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
	"strings"
	"testing"
	"time"
)

func TestAgentVerificationWorkerRunProcessesPendingWorkAndStopsOnCancellation(
	t *testing.T,
) {
	t.Parallel()

	now := time.Date(
		2026,
		time.October,
		9,
		11,
		0,
		0,
		0,
		time.UTC,
	)

	subject := remediationdomain.VerificationSubject{
		Cluster:   "dev",
		Namespace: "default",
		Kind:      "ReplicaSet",
		Name:      "worker-loop-app-rs",
		UID:       "rs-uid-verification-worker-loop",
	}
	actionKey := remediationdomain.ActionKey{
		IncidentID: "inc-verification-worker-loop",
		PlanHash:   "sha256:verification-worker-loop-plan",
		TargetUID:  "pod-uid-verification-worker-loop",
	}
	pending := remediationdomain.Verification{
		ID:              "ver-verification-worker-loop",
		ActionKey:       actionKey,
		ActionAttemptID: "att-verification-worker-loop",
		Subject:         subject,
		Status: remediationdomain.
			VerificationStatusPending,
		Version:   1,
		StartedAt: now.Add(-time.Minute),
	}
	verifyingIncident := incidentdomain.Incident{
		ID:      actionKey.IncidentID,
		State:   incidentdomain.StateVerifying,
		Version: 5,
	}
	claim := incidentdomain.Claim{
		Incident:  verifyingIncident,
		HolderID:  "verification-worker",
		ExpiresAt: now.Add(30 * time.Second),
	}

	finishedAt := now
	recovered := pending
	recovered.Status =
		remediationdomain.VerificationStatusRecovered
	recovered.Version = 2
	recovered.FinishedAt = &finishedAt
	recovered.EvidenceCode = "WORKLOAD_RECOVERED"

	resolvedIncident := verifyingIncident
	resolvedIncident.State = incidentdomain.StateResolved
	resolvedIncident.Version++
	resolvedIncident.ResolutionVerificationID = recovered.ID

	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		work: incidentdomain.PendingVerificationClaim{
			Verification: pending,
			Claim:        claim,
		},
		completion: incidentdomain.
			FencedVerificationCompletionResult{
			Incident:     verifyingIncident,
			Verification: recovered,
		},
		resolvedIncident: resolvedIncident,
		resolvedEvents: make(
			chan incidentdomain.Incident,
			1,
		),
	}
	executor := &agentVerificationExecutorProbe{
		result: verificationExecutionResult{
			Status: remediationdomain.
				VerificationStatusRecovered,
			EvidenceCode: "WORKLOAD_RECOVERED",
		},
	}

	var logs bytes.Buffer
	agent := &sreAgent{
		config: agentConfig{
			PollInterval:               time.Hour,
			IncidentClaimHolderID:      claim.HolderID,
			IncidentClaimLeaseDuration: 30 * time.Second,
			KubernetesRequestTimeout:   time.Second,
		},
		verificationWorker:   lifecycle,
		verificationExecutor: executor,
		logger: slog.New(
			slog.NewJSONHandler(&logs, nil),
		),
		now: func() time.Time {
			return now
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		agent.runVerificationWorker(ctx)
	}()

	var resolved incidentdomain.Incident

	select {
	case resolved = <-lifecycle.resolvedEvents:
	case <-time.After(time.Second):
		t.Fatalf(
			"runVerificationWorker() did not resolve pending work\nlogs:\n%s",
			logs.String(),
		)
	}

	cancel()

	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal(
			"runVerificationWorker() did not stop after context cancellation",
		)
	}

	if resolved.ID != resolvedIncident.ID {
		t.Fatalf(
			"resolved Incident ID = %q; want %q",
			resolved.ID,
			resolvedIncident.ID,
		)
	}
	if resolved.State != incidentdomain.StateResolved {
		t.Fatalf(
			"resolved Incident State = %q; want %q",
			resolved.State,
			incidentdomain.StateResolved,
		)
	}
	if lifecycle.claimCalls != 2 {
		t.Fatalf(
			"ClaimPendingVerification calls = %d; want 2",
			lifecycle.claimCalls,
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
	if executor.calls != 1 {
		t.Fatalf(
			"Verification executor calls = %d; want 1",
			executor.calls,
		)
	}
}

func TestAgentRunStartsVerificationWorkerAndStopsItOnCancellation(
	t *testing.T,
) {
	t.Parallel()

	now := time.Date(
		2026,
		time.October,
		9,
		12,
		0,
		0,
		0,
		time.UTC,
	)

	claimedEvents := make(chan struct{}, 1)
	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		claimedEvents: claimedEvents,
	}
	executor := &agentVerificationExecutorProbe{
		result: verificationExecutionResult{
			Status: remediationdomain.
				VerificationStatusRecovered,
			EvidenceCode: "WORKLOAD_RECOVERED",
		},
	}

	var logs bytes.Buffer
	agent := newSREAgent(
		agentConfig{
			PollInterval:               time.Hour,
			IncidentClaimHolderID:      "verification-worker",
			IncidentClaimLeaseDuration: 30 * time.Second,
			KubernetesRequestTimeout:   time.Second,
		},
		fake.NewSimpleClientset(),
		incidentRegistryHandle{
			Registry: incidentdomain.NewMemoryRegistry(),
			Approvals: approvaldomain.
				NewMemoryStore(),
			VerificationWorker: lifecycle,
		},
		slog.New(
			slog.NewJSONHandler(&logs, nil),
		),
		func() {},
		func() {},
		func(string) {},
	)
	agent.prometheus = &stubFiringAlertSource{}
	agent.verificationExecutor = executor
	agent.now = func() time.Time {
		return now
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		agent.run(ctx)
	}()

	select {
	case <-claimedEvents:
	case <-time.After(time.Second):
		cancel()

		select {
		case <-agentDone:
		case <-time.After(time.Second):
		}

		t.Fatalf(
			"agent.run() did not start Verification Worker\nlogs:\n%s",
			logs.String(),
		)
	}

	cancel()

	select {
	case <-agentDone:
	case <-time.After(time.Second):
		t.Fatal(
			"agent.run() did not stop after context cancellation",
		)
	}

	if lifecycle.claimCalls < 1 {
		t.Fatalf(
			"ClaimPendingVerification calls = %d; want at least 1",
			lifecycle.claimCalls,
		)
	}
}

func TestAgentVerificationWorkerRunLogsIterationError(
	t *testing.T,
) {
	t.Parallel()

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
	claimErr := errors.New(
		"recovered verification scheduler unavailable",
	)
	claimEvents := make(chan struct{}, 1)

	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		recoveredClaimErr:    claimErr,
		recoveredClaimEvents: claimEvents,
	}

	var logs bytes.Buffer
	agent := &sreAgent{
		config: agentConfig{
			PollInterval:          time.Hour,
			IncidentClaimHolderID: "verification-worker",
			IncidentClaimLeaseDuration: 30 *
				time.Second,
			KubernetesRequestTimeout: time.Second,
		},
		verificationWorker: lifecycle,
		logger: slog.New(
			slog.NewJSONHandler(&logs, nil),
		),
		now: func() time.Time {
			return workerAt
		},
	}

	ctx, cancel := context.WithCancel(
		context.Background(),
	)
	done := make(chan struct{})

	go func() {
		defer close(done)
		agent.runVerificationWorker(ctx)
	}()

	select {
	case <-claimEvents:
	case <-time.After(time.Second):
		cancel()
		t.Fatal(
			"runVerificationWorker() did not attempt " +
				"recovered Verification resolution Claim",
		)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal(
			"runVerificationWorker() did not stop " +
				"after context cancellation",
		)
	}

	logOutput := logs.String()

	if !strings.Contains(
		logOutput,
		`"msg":"agent_verification_worker_failed"`,
	) {
		t.Fatalf(
			"missing agent_verification_worker_failed log:\n%s",
			logOutput,
		)
	}
	if !strings.Contains(
		logOutput,
		`"result":"FAILED"`,
	) {
		t.Fatalf(
			"missing FAILED result:\n%s",
			logOutput,
		)
	}
	if !strings.Contains(
		logOutput,
		`"error_code":"VERIFICATION_WORKER_RECOVERED_RESOLUTION_CLAIM_FAILED"`,
	) {
		t.Fatalf(
			"missing VERIFICATION_WORKER_RECOVERED_RESOLUTION_CLAIM_FAILED "+
				"error code:\n%s",
			logOutput,
		)
	}
	if !strings.Contains(
		logOutput,
		claimErr.Error(),
	) {
		t.Fatalf(
			"missing Worker iteration error:\n%s",
			logOutput,
		)
	}
}

type agentVerificationWorkerCancellationExecutor struct {
	started chan struct{}
}

func (
	executor *agentVerificationWorkerCancellationExecutor,
) Verify(
	ctx context.Context,
	_ remediationdomain.Verification,
) (verificationExecutionResult, error) {
	select {
	case executor.started <- struct{}{}:
	default:
	}

	<-ctx.Done()

	return verificationExecutionResult{}, ctx.Err()
}

func TestAgentVerificationWorkerRunDoesNotLogCancellationAsFailure(
	t *testing.T,
) {
	t.Parallel()

	workerAt := time.Date(
		2026,
		time.October,
		12,
		16,
		0,
		0,
		0,
		time.FixedZone("CST", 8*60*60),
	)

	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		work: incidentdomain.PendingVerificationClaim{
			Verification: remediationdomain.Verification{
				ID:      "ver-worker-cancellation",
				Status:  remediationdomain.VerificationStatusPending,
				Version: 1,
			},
			Claim: incidentdomain.Claim{
				Incident: incidentdomain.Incident{
					ID:      "inc-worker-cancellation",
					State:   incidentdomain.StateVerifying,
					Version: 12,
				},
				HolderID: "verification-worker",
				ExpiresAt: workerAt.Add(
					30 * time.Second,
				),
			},
		},
	}

	executor := &agentVerificationWorkerCancellationExecutor{
		started: make(chan struct{}, 1),
	}

	var logs bytes.Buffer

	agent := &sreAgent{
		config: agentConfig{
			PollInterval:               time.Hour,
			IncidentClaimHolderID:      "verification-worker",
			IncidentClaimLeaseDuration: 2 * time.Hour,
			KubernetesRequestTimeout:   time.Hour,
		},
		verificationWorker:   lifecycle,
		verificationExecutor: executor,
		logger: slog.New(
			slog.NewJSONHandler(
				&logs,
				nil,
			),
		),
		now: func() time.Time {
			return workerAt
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		agent.runVerificationWorker(ctx)
	}()

	select {
	case <-executor.started:
	case <-time.After(time.Second):
		cancel()

		t.Fatal(
			"Verification Worker did not start Verification execution",
		)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal(
			"Verification Worker did not stop after cancellation",
		)
	}

	if strings.Contains(
		logs.String(),
		`"msg":"agent_verification_worker_failed"`,
	) {
		t.Fatalf(
			"normal Worker cancellation logged as iteration failure:\n%s",
			logs.String(),
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

func TestAgentVerificationWorkerRunRetriesAfterIterationError(
	t *testing.T,
) {
	t.Parallel()

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
	iterationErr := errors.New(
		"recovered verification scheduler unavailable",
	)
	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		recoveredClaimErr: iterationErr,
	}

	logs := newVerificationWorkerLogCapture()

	agent := &sreAgent{
		config: agentConfig{
			PollInterval:               5 * time.Millisecond,
			IncidentClaimHolderID:      "verification-worker",
			IncidentClaimLeaseDuration: 30 * time.Second,
			KubernetesRequestTimeout:   time.Second,
		},
		verificationWorker: lifecycle,
		logger: slog.New(
			slog.NewJSONHandler(
				logs,
				nil,
			),
		),
		now: func() time.Time {
			return workerAt
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		agent.runVerificationWorker(ctx)
	}()

	for attempt := 1; attempt <= 2; attempt++ {
		select {
		case <-logs.records:
		case <-time.After(time.Second):
			cancel()

			t.Fatalf(
				"Verification Worker failure log %d was not written",
				attempt,
			)
		}
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal(
			"Verification Worker did not stop after cancellation",
		)
	}

	if lifecycle.recoveredClaimCalls < 2 {
		t.Fatalf(
			"ClaimRecoveredVerificationResolution calls = %d; "+
				"want at least 2",
			lifecycle.recoveredClaimCalls,
		)
	}

	logOutput := logs.String()
	if failureLogs := strings.Count(
		logOutput,
		`"msg":"agent_verification_worker_failed"`,
	); failureLogs < 2 {
		t.Fatalf(
			"iteration failure logs = %d; want at least 2\nlogs:\n%s",
			failureLogs,
			logOutput,
		)
	}

	if errorLogs := strings.Count(
		logOutput,
		`"error":"claim recovered verification resolution: `+
			`recovered verification scheduler unavailable"`,
	); errorLogs < 2 {
		t.Fatalf(
			"scheduler error logs = %d; want at least 2\nlogs:\n%s",
			errorLogs,
			logOutput,
		)
	}
}
