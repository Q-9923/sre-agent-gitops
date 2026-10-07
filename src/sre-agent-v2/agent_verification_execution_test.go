package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	remediationdomain "sre-agent/internal/remediation"
)

func TestAgentCompletesPendingVerificationFromIndependentRecoveredEvidence(
	t *testing.T,
) {
	t.Parallel()

	harness := newAgentVerificationExecutionHarness(t, 0)
	executor := &agentVerificationExecutorProbe{
		result: verificationExecutionResult{
			Status: remediationdomain.
				VerificationStatusRecovered,
			EvidenceCode: "WORKLOAD_AVAILABLE_REPLICAS_RESTORED",
		},
	}
	harness.agent.verificationExecutor = executor

	completed, completedOK := harness.execute(
		harness.pending,
	)
	if !completedOK {
		t.Fatal("executeVerification() completed = false; want true")
	}

	if executor.calls != 1 {
		t.Fatalf(
			"Verification executor calls = %d; want 1",
			executor.calls,
		)
	}
	if len(executor.subjects) != 1 ||
		executor.subjects[0] != harness.subject {
		t.Fatalf(
			"Verification executor Subjects = %#v; want %#v",
			executor.subjects,
			[]remediationdomain.VerificationSubject{
				harness.subject,
			},
		)
	}

	want := expectedCompletedVerification(
		harness.pending,
		remediationdomain.VerificationStatusRecovered,
		"WORKLOAD_AVAILABLE_REPLICAS_RESTORED",
		harness.now,
	)
	assertVerificationEqual(t, completed, want)
	assertVerificationEqual(t, harness.reload(t), want)
}

func TestAgentCompletesPendingVerificationWithIndependentTerminalOutcomes(
	t *testing.T,
) {
	t.Parallel()

	testCases := []struct {
		name         string
		status       remediationdomain.VerificationStatus
		evidenceCode string
	}{
		{
			name: "target_not_recovered",
			status: remediationdomain.
				VerificationStatusNotRecovered,
			evidenceCode: "WORKLOAD_REMAINS_UNAVAILABLE",
		},
		{
			name: "evidence_inconclusive",
			status: remediationdomain.
				VerificationStatusInconclusive,
			evidenceCode: "WORKLOAD_RECOVERY_EVIDENCE_INCONCLUSIVE",
		},
	}

	for index, testCase := range testCases {
		index := index
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			harness := newAgentVerificationExecutionHarness(
				t,
				10+index,
			)
			executor := &agentVerificationExecutorProbe{
				result: verificationExecutionResult{
					Status:       testCase.status,
					EvidenceCode: testCase.evidenceCode,
				},
			}
			harness.agent.verificationExecutor = executor

			completed, completedOK := harness.execute(
				harness.pending,
			)
			if !completedOK {
				t.Fatal(
					"executeVerification() completed = false; " +
						"want true",
				)
			}

			if executor.calls != 1 {
				t.Fatalf(
					"Verification executor calls = %d; want 1",
					executor.calls,
				)
			}

			want := expectedCompletedVerification(
				harness.pending,
				testCase.status,
				testCase.evidenceCode,
				harness.now,
			)
			assertVerificationEqual(t, completed, want)
			assertVerificationEqual(
				t,
				harness.reload(t),
				want,
			)
		})
	}
}

func TestAgentFailsClosedWhenIndependentVerificationCannotExecute(
	t *testing.T,
) {
	t.Parallel()

	testCases := []struct {
		name          string
		executor      *agentVerificationExecutorProbe
		wantCalls     int
		wantErrorCode string
	}{
		{
			name:          "executor_unavailable",
			wantCalls:     0,
			wantErrorCode: verificationExecutorUnavailable,
		},
		{
			name: "executor_failed",
			executor: &agentVerificationExecutorProbe{
				err: errors.New(
					"verification evidence source unavailable",
				),
			},
			wantCalls:     1,
			wantErrorCode: verificationExecutionFailed,
		},
	}

	for index, testCase := range testCases {
		index := index
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			harness := newAgentVerificationExecutionHarness(
				t,
				20+index,
			)
			if testCase.executor != nil {
				harness.agent.verificationExecutor =
					testCase.executor
			}

			result, completed := harness.execute(
				harness.pending,
			)
			if completed {
				t.Fatal(
					"executeVerification() completed = true; " +
						"want false",
				)
			}

			if testCase.executor != nil &&
				testCase.executor.calls != testCase.wantCalls {
				t.Fatalf(
					"Verification executor calls = %d; want %d",
					testCase.executor.calls,
					testCase.wantCalls,
				)
			}

			assertVerificationEqual(
				t,
				result,
				harness.pending,
			)
			assertVerificationEqual(
				t,
				harness.reload(t),
				harness.pending,
			)
			assertVerificationErrorCode(
				t,
				&harness.logs,
				testCase.wantErrorCode,
			)
		})
	}
}

func TestAgentRejectsInvalidIndependentVerificationResults(
	t *testing.T,
) {
	t.Parallel()

	testCases := []struct {
		name   string
		result verificationExecutionResult
	}{
		{
			name: "non_terminal_status",
			result: verificationExecutionResult{
				Status: remediationdomain.
					VerificationStatusPending,
				EvidenceCode: "CHECK_STILL_RUNNING",
			},
		},
		{
			name: "missing_evidence_code",
			result: verificationExecutionResult{
				Status: remediationdomain.
					VerificationStatusRecovered,
				EvidenceCode: "   ",
			},
		},
	}

	for index, testCase := range testCases {
		index := index
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			harness := newAgentVerificationExecutionHarness(
				t,
				30+index,
			)
			executor := &agentVerificationExecutorProbe{
				result: testCase.result,
			}
			harness.agent.verificationExecutor = executor

			result, completed := harness.execute(
				harness.pending,
			)
			if completed {
				t.Fatal(
					"executeVerification() completed = true; " +
						"want false",
				)
			}
			if executor.calls != 1 {
				t.Fatalf(
					"Verification executor calls = %d; want 1",
					executor.calls,
				)
			}

			assertVerificationEqual(
				t,
				result,
				harness.pending,
			)
			assertVerificationEqual(
				t,
				harness.reload(t),
				harness.pending,
			)
			assertVerificationErrorCode(
				t,
				&harness.logs,
				verificationResultInvalid,
			)
		})
	}
}

func TestAgentDoesNotReportCompletionWhenVerificationPersistenceFails(
	t *testing.T,
) {
	t.Parallel()

	harness := newAgentVerificationExecutionHarness(t, 40)
	store := &agentVerificationCompletionFailStore{
		inner: harness.backingStore,
		err: errors.New(
			"verification result store unavailable",
		),
	}
	executor := &agentVerificationExecutorProbe{
		result: verificationExecutionResult{
			Status: remediationdomain.
				VerificationStatusRecovered,
			EvidenceCode: "WORKLOAD_AVAILABLE_REPLICAS_RESTORED",
		},
	}

	harness.agent.verifications = store
	harness.agent.verificationExecutor = executor

	result, completed := harness.execute(harness.pending)
	if completed {
		t.Fatal(
			"executeVerification() completed = true; want false",
		)
	}
	if executor.calls != 1 {
		t.Fatalf(
			"Verification executor calls = %d; want 1",
			executor.calls,
		)
	}
	if store.completeCalls != 1 {
		t.Fatalf(
			"Verification Complete calls = %d; want 1",
			store.completeCalls,
		)
	}

	assertVerificationEqual(t, result, harness.pending)
	assertVerificationEqual(
		t,
		harness.reload(t),
		harness.pending,
	)
	assertVerificationErrorCode(
		t,
		&harness.logs,
		verificationCompleteFailed,
	)
}

func TestAgentDoesNotReexecuteTerminalVerification(
	t *testing.T,
) {
	t.Parallel()

	testCases := []struct {
		name         string
		status       remediationdomain.VerificationStatus
		evidenceCode string
	}{
		{
			name: "recovered",
			status: remediationdomain.
				VerificationStatusRecovered,
			evidenceCode: "WORKLOAD_AVAILABLE_REPLICAS_RESTORED",
		},
		{
			name: "not_recovered",
			status: remediationdomain.
				VerificationStatusNotRecovered,
			evidenceCode: "WORKLOAD_REMAINS_UNAVAILABLE",
		},
		{
			name: "inconclusive",
			status: remediationdomain.
				VerificationStatusInconclusive,
			evidenceCode: "WORKLOAD_RECOVERY_EVIDENCE_INCONCLUSIVE",
		},
	}

	for index, testCase := range testCases {
		index := index
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			harness := newAgentVerificationExecutionHarness(
				t,
				45+index,
			)
			terminal, err := harness.backingStore.Complete(
				harness.ctx,
				remediationdomain.
					CompleteVerificationCommand{
					ActionKey: harness.pending.ActionKey,
					ExpectedVersion: harness.pending.
						Version,
					To:           testCase.status,
					FinishedAt:   harness.now,
					EvidenceCode: testCase.evidenceCode,
				},
			)
			if err != nil {
				t.Fatalf(
					"Verification Complete() error = %v",
					err,
				)
			}

			executor := &agentVerificationExecutorProbe{
				result: verificationExecutionResult{
					Status: remediationdomain.
						VerificationStatusRecovered,
					EvidenceCode: "SHOULD_NOT_BE_USED",
				},
			}
			harness.agent.verificationExecutor = executor

			result, completed := harness.execute(terminal)
			if completed {
				t.Fatal(
					"executeVerification() completed = true; " +
						"want false for terminal Verification",
				)
			}
			if executor.calls != 0 {
				t.Fatalf(
					"Verification executor calls = %d; "+
						"want 0 for terminal Verification",
					executor.calls,
				)
			}

			assertVerificationEqual(t, result, terminal)
			assertVerificationEqual(
				t,
				harness.reload(t),
				terminal,
			)
			assertVerificationErrorCode(
				t,
				&harness.logs,
				verificationNotPending,
			)
		})
	}
}

func TestAgentRejectsLateVerificationResultAfterExecutionDeadline(
	t *testing.T,
) {
	t.Parallel()

	harness := newAgentVerificationExecutionHarness(t, 50)
	executor := &agentVerificationLateSuccessExecutor{}

	harness.agent.config.KubernetesRequestTimeout =
		10 * time.Millisecond
	harness.agent.verificationExecutor = executor

	result, completed := harness.execute(harness.pending)
	if completed {
		t.Fatal(
			"executeVerification() completed = true; " +
				"want false after execution deadline",
		)
	}
	if executor.calls != 1 {
		t.Fatalf(
			"Verification executor calls = %d; want 1",
			executor.calls,
		)
	}
	if !errors.Is(
		executor.contextErr,
		context.DeadlineExceeded,
	) {
		t.Fatalf(
			"Verification executor context error = %v; want %v",
			executor.contextErr,
			context.DeadlineExceeded,
		)
	}

	assertVerificationEqual(t, result, harness.pending)
	assertVerificationEqual(
		t,
		harness.reload(t),
		harness.pending,
	)
	assertVerificationErrorCode(
		t,
		&harness.logs,
		verificationExecutionFailed,
	)
}

func TestAgentFailsClosedWithoutVerificationResultStore(
	t *testing.T,
) {
	t.Parallel()

	harness := newAgentVerificationExecutionHarness(t, 51)
	executor := &agentVerificationExecutorProbe{
		result: verificationExecutionResult{
			Status: remediationdomain.
				VerificationStatusRecovered,
			EvidenceCode: "SHOULD_NOT_BE_USED",
		},
	}

	harness.agent.verificationExecutor = executor
	harness.agent.verifications = nil

	result, completed := harness.execute(harness.pending)
	if completed {
		t.Fatal(
			"executeVerification() completed = true; want false",
		)
	}
	if executor.calls != 0 {
		t.Fatalf(
			"Verification executor calls = %d; "+
				"want 0 without result store",
			executor.calls,
		)
	}

	assertVerificationEqual(t, result, harness.pending)
	assertVerificationEqual(
		t,
		harness.reload(t),
		harness.pending,
	)
	assertVerificationErrorCode(
		t,
		&harness.logs,
		verificationStoreUnavailable,
	)
}

type agentVerificationExecutionHarness struct {
	now          time.Time
	ctx          context.Context
	subject      remediationdomain.VerificationSubject
	beginCommand remediationdomain.BeginVerificationCommand
	backingStore *remediationdomain.MemoryVerificationStore
	pending      remediationdomain.Verification
	logs         bytes.Buffer
	agent        *sreAgent
}

func newAgentVerificationExecutionHarness(
	t *testing.T,
	minute int,
) *agentVerificationExecutionHarness {
	t.Helper()

	now := time.Date(
		2026,
		time.October,
		7,
		21,
		minute,
		0,
		0,
		time.UTC,
	)
	ctx := context.Background()
	attempt := agentVerificationTerminalAttempt(now)
	subject := remediationdomain.VerificationSubject{
		Cluster:   "dev",
		Namespace: "default",
		Kind:      "Deployment",
		Name:      "crash-app",
		UID:       "deployment-uid-agent-verification-execution",
	}
	beginCommand :=
		remediationdomain.BeginVerificationCommand{
			ActionAttempt: attempt,
			Subject:       subject,
			StartedAt:     *attempt.FinishedAt,
		}

	backingStore :=
		remediationdomain.NewMemoryVerificationStore()
	pending, created, err := backingStore.Begin(
		ctx,
		beginCommand,
	)
	if err != nil {
		t.Fatalf("Verification Begin() error = %v", err)
	}
	if !created {
		t.Fatal(
			"Verification Begin() created = false; want true",
		)
	}

	harness := &agentVerificationExecutionHarness{
		now:          now,
		ctx:          ctx,
		subject:      subject,
		beginCommand: beginCommand,
		backingStore: backingStore,
		pending:      pending,
	}
	harness.agent = newAgentVerificationTestAgent(
		now,
		&agentVerificationIncidentRegistry{},
		backingStore,
	)
	harness.agent.logger = slog.New(
		slog.NewJSONHandler(&harness.logs, nil),
	)

	return harness
}

func (harness *agentVerificationExecutionHarness) execute(
	verification remediationdomain.Verification,
) (remediationdomain.Verification, bool) {
	return harness.agent.executeVerification(
		harness.ctx,
		verification,
		"RESTART_POD",
		"default/crash-app",
	)
}

func (harness *agentVerificationExecutionHarness) reload(
	t *testing.T,
) remediationdomain.Verification {
	t.Helper()

	reloaded, created, err := harness.backingStore.Begin(
		harness.ctx,
		harness.beginCommand,
	)
	if err != nil {
		t.Fatalf(
			"reload Verification through Begin() error = %v",
			err,
		)
	}
	if created {
		t.Fatal(
			"reload Verification through Begin() created = true; " +
				"want existing Verification",
		)
	}

	return reloaded
}

func expectedCompletedVerification(
	pending remediationdomain.Verification,
	status remediationdomain.VerificationStatus,
	evidenceCode string,
	finishedAt time.Time,
) remediationdomain.Verification {
	completed := pending
	completed.Status = status
	completed.Version++
	completed.FinishedAt = &finishedAt
	completed.EvidenceCode = evidenceCode

	return completed
}

func assertVerificationEqual(
	t *testing.T,
	got remediationdomain.Verification,
	want remediationdomain.Verification,
) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf(
			"Verification = %#v; want %#v",
			got,
			want,
		)
	}
}

func assertVerificationErrorCode(
	t *testing.T,
	logs *bytes.Buffer,
	errorCode string,
) {
	t.Helper()

	if !strings.Contains(
		logs.String(),
		`"error_code":"`+errorCode+`"`,
	) {
		t.Fatalf(
			"missing %s log: %s",
			errorCode,
			logs.String(),
		)
	}
}

type agentVerificationExecutorProbe struct {
	calls    int
	subjects []remediationdomain.VerificationSubject
	result   verificationExecutionResult
	err      error
}

func (executor *agentVerificationExecutorProbe) Verify(
	_ context.Context,
	subject remediationdomain.VerificationSubject,
) (verificationExecutionResult, error) {
	executor.calls++
	executor.subjects = append(
		executor.subjects,
		subject,
	)

	return executor.result, executor.err
}

type agentVerificationLateSuccessExecutor struct {
	calls      int
	contextErr error
}

func (executor *agentVerificationLateSuccessExecutor) Verify(
	ctx context.Context,
	_ remediationdomain.VerificationSubject,
) (verificationExecutionResult, error) {
	executor.calls++

	<-ctx.Done()
	executor.contextErr = ctx.Err()

	return verificationExecutionResult{
		Status: remediationdomain.
			VerificationStatusRecovered,
		EvidenceCode: "LATE_RECOVERY_RESULT",
	}, nil
}

type agentVerificationCompletionFailStore struct {
	inner         remediationdomain.VerificationStore
	completeCalls int
	err           error
}

func (store *agentVerificationCompletionFailStore) Begin(
	ctx context.Context,
	command remediationdomain.BeginVerificationCommand,
) (remediationdomain.Verification, bool, error) {
	return store.inner.Begin(ctx, command)
}

func (store *agentVerificationCompletionFailStore) Complete(
	_ context.Context,
	_ remediationdomain.CompleteVerificationCommand,
) (remediationdomain.Verification, error) {
	store.completeCalls++

	return remediationdomain.Verification{}, store.err
}
