package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
	"time"
)

func TestAgentVerificationWorkerLogsFailureWithWorkIdentity(
	t *testing.T,
) {
	agent, logs := newVerificationWorkerTestAgent()
	work, completion := verificationWorkerFailureFixture(agent.now())
	lifecycle := &verificationWorkerLifecycleAdapter{}
	configurePendingWorkerFixture(lifecycle, work, completion)
	agent.verificationWorker = lifecycle
	agent.verificationExecutor = verificationExecutorFunc(func(
		context.Context,
		remediationdomain.Verification,
	) (verificationExecutionResult, error) {
		return verificationExecutionResult{},
			errors.New("verification evidence source unavailable")
	})

	ctx, cancel := context.WithCancel(context.Background())
	agent.waitVerificationWorker = func(
		ctx context.Context,
		_ time.Duration,
	) error {
		cancel()
		return ctx.Err()
	}

	agent.runVerificationWorker(ctx)

	logOutput := logs.String()
	for _, expected := range []string{
		`"msg":"agent_verification_worker_failed"`,
		`"incident_id":"inc-worker-failure"`,
		`"verification_id":"ver-worker-failure"`,
		`"action_attempt_id":"att-worker-failure"`,
		`"action":"EXECUTE_VERIFICATION"`,
		`"target":"default/crash-app-rs"`,
		`"error_code":"VERIFICATION_WORKER_EXECUTION_FAILED"`,
	} {
		if !strings.Contains(logOutput, expected) {
			t.Fatalf(
				"Worker failure log missing %q:\n%s",
				expected,
				logOutput,
			)
		}
	}
}

func TestAgentVerificationWorkerLogsEveryClassifiedFailureFromOneIteration(
	t *testing.T,
) {
	t.Parallel()

	agent, logs := newVerificationWorkerTestAgent()
	work, completion := verificationWorkerFailureFixture(agent.now())
	lifecycle := &verificationWorkerLifecycleAdapter{
		claimRecovered: func(
			context.Context,
			incidentdomain.ClaimRecoveredVerificationResolutionCommand,
		) (incidentdomain.RecoveredVerificationResolutionClaim, bool, error) {
			return incidentdomain.RecoveredVerificationResolutionClaim{},
				false,
				errors.New("recovered resolution scheduler unavailable")
		},
	}
	configurePendingWorkerFixture(lifecycle, work, completion)
	agent.verificationWorker = lifecycle
	agent.verificationExecutor = verificationExecutorFunc(func(
		context.Context,
		remediationdomain.Verification,
	) (verificationExecutionResult, error) {
		return verificationExecutionResult{},
			errors.New("verification evidence source unavailable")
	})

	ctx, cancel := context.WithCancel(context.Background())
	agent.waitVerificationWorker = func(
		context.Context,
		time.Duration,
	) error {
		cancel()
		return context.Canceled
	}

	agent.runVerificationWorker(ctx)

	logOutput := logs.String()
	for _, errorCode := range []string{
		"VERIFICATION_WORKER_RECOVERED_RESOLUTION_CLAIM_FAILED",
		"VERIFICATION_WORKER_EXECUTION_FAILED",
	} {
		expected := `"error_code":"` + errorCode + `"`
		if count := strings.Count(logOutput, expected); count != 1 {
			t.Fatalf(
				"Worker failure log count for %q = %d; want 1:\n%s",
				errorCode,
				count,
				logOutput,
			)
		}
	}
}
func TestAgentVerificationWorkerDoesNotLogCancellationAsFailure(
	t *testing.T,
) {
	agent, logs := newVerificationWorkerTestAgent()
	work, completion := verificationWorkerFailureFixture(agent.now())
	lifecycle := &verificationWorkerLifecycleAdapter{}
	configurePendingWorkerFixture(lifecycle, work, completion)
	agent.verificationWorker = lifecycle
	started := make(chan struct{})
	agent.verificationExecutor = verificationExecutorFunc(func(
		ctx context.Context,
		_ remediationdomain.Verification,
	) (verificationExecutionResult, error) {
		close(started)
		<-ctx.Done()
		return verificationExecutionResult{}, ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runVerificationWorker(ctx)
	}()

	<-started
	cancel()
	<-done

	if strings.Contains(
		logs.String(),
		`"msg":"agent_verification_worker_failed"`,
	) {
		t.Fatalf(
			"normal Worker cancellation logged as failure:\n%s",
			logs.String(),
		)
	}
}
