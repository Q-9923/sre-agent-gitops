package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func TestHandlePodCrashLoopingCreatesOrReusesVerificationForExistingTerminalActionAttempt(
	t *testing.T,
) {
	tests := []struct {
		name                string
		harnessSuffix       string
		status              remediationdomain.ActionAttemptStatus
		preseedVerification bool
	}{
		{
			name:          "succeeded attempt",
			harnessSuffix: "existing-succeeded-attempt",
			status: remediationdomain.
				ActionAttemptStatusSucceeded,
		},
		{
			name:          "failed attempt",
			harnessSuffix: "existing-failed-attempt",
			status: remediationdomain.
				ActionAttemptStatusFailed,
		},
		{
			name:                "existing pending verification",
			harnessSuffix:       "existing-pending-verification",
			status:              remediationdomain.ActionAttemptStatusSucceeded,
			preseedVerification: true,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			harness := newWaitingApprovalHarness(
				t,
				testCase.harnessSuffix,
			)
			harness.grantApproval(
				t,
				harness.now.Add(-time.Minute),
				harness.now.Add(time.Hour),
			)

			actionAttemptStore :=
				&handlerExistingTerminalActionAttemptStore{
					status: testCase.status,
				}

			delegateVerificationStore :=
				remediationdomain.NewMemoryVerificationStore()

			verificationStore :=
				&handlerRecordingVerificationStore{
					delegate: delegateVerificationStore,
				}

			subject := verificationSubjectForPod(
				harness.evidence,
			)

			if testCase.preseedVerification {
				preseededAttempt :=
					newHandlerTerminalActionAttempt(
						t,
						remediationdomain.BeginActionAttemptCommand{
							Key: remediationdomain.ExecutionKey{
								IncidentID: harness.
									observedIncident.ID,
								PlanHash: harness.plan.Hash,
								TargetUID: harness.plan.
									Target.UID,
								FencingToken: 1,
							},
							StartedAt: harness.now.Add(
								-time.Minute,
							),
						},
						testCase.status,
					)

				preseededVerification, created, err :=
					delegateVerificationStore.Begin(
						context.Background(),
						remediationdomain.
							BeginVerificationCommand{
							ActionAttempt: preseededAttempt,
							Subject:       subject,
							StartedAt: harness.now.Add(
								-30 * time.Second,
							),
						},
					)
				if err != nil {
					t.Fatalf(
						"preseed Verification Begin() error = %v",
						err,
					)
				}
				if !created {
					t.Fatal(
						"preseed Verification Begin() created = false; want true",
					)
				}
				if preseededVerification.Status !=
					remediationdomain.
						VerificationStatusPending {
					t.Fatalf(
						"preseed Verification Status = %q; want %q",
						preseededVerification.Status,
						remediationdomain.
							VerificationStatusPending,
					)
				}
			}

			agent := harness.newAgent(
				harness.registry,
				harness.approvalStore,
			)
			agent.actionAttempts = actionAttemptStore
			agent.verifications = verificationStore

			agent.verificationLifecycle =
				&agentVerificationLifecycleAdapter{
					registry: harness.registry,
					store:    verificationStore,
				}
			agent.handlePodCrashLooping(
				context.Background(),
				harness.alert,
			)

			if actionAttemptStore.beginCalls != 1 {
				t.Fatalf(
					"ActionAttempt Begin calls = %d; want 1",
					actionAttemptStore.beginCalls,
				)
			}
			if actionAttemptStore.completeCalls != 0 {
				t.Fatalf(
					"ActionAttempt Complete calls = %d; want 0",
					actionAttemptStore.completeCalls,
				)
			}
			if actionAttemptStore.recoverCalls != 0 {
				t.Fatalf(
					"ActionAttempt Recover calls = %d; want 0",
					actionAttemptStore.recoverCalls,
				)
			}

			if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
				t.Fatalf(
					"Kubernetes actions = %#v; want none for existing terminal Action Attempt",
					actions,
				)
			}

			if verificationStore.beginCalls != 1 {
				t.Fatalf(
					"Verification Begin calls = %d; want 1\nlogs:\n%s",
					verificationStore.beginCalls,
					harness.logs.String(),
				)
			}

			if len(verificationStore.beginCommands) != 1 {
				t.Fatalf(
					"Verification Begin commands = %d; want 1",
					len(verificationStore.beginCommands),
				)
			}
			if len(verificationStore.beginResults) != 1 {
				t.Fatalf(
					"Verification Begin results = %d; want 1",
					len(verificationStore.beginResults),
				)
			}

			command :=
				verificationStore.beginCommands[0]
			verification :=
				verificationStore.beginResults[0]

			if command.ActionAttempt.Status !=
				testCase.status {
				t.Fatalf(
					"Verification ActionAttempt Status = %q; want %q",
					command.ActionAttempt.Status,
					testCase.status,
				)
			}
			if command.Subject != subject {
				t.Fatalf(
					"Verification Subject = %#v; want %#v",
					command.Subject,
					subject,
				)
			}
			if verification.Status !=
				remediationdomain.
					VerificationStatusPending {
				t.Fatalf(
					"Verification Status = %q; want %q",
					verification.Status,
					remediationdomain.
						VerificationStatusPending,
				)
			}
			if verification.ActionKey !=
				command.ActionAttempt.Key.ActionKey() {
				t.Fatalf(
					"Verification ActionKey = %#v; want %#v",
					verification.ActionKey,
					command.ActionAttempt.Key.ActionKey(),
				)
			}

			wantCreated :=
				!testCase.preseedVerification
			if verificationStore.beginCreated[0] !=
				wantCreated {
				t.Fatalf(
					"Verification created = %t; want %t",
					verificationStore.beginCreated[0],
					wantCreated,
				)
			}

			current := harness.currentIncident(t)
			if current.State != incident.StateVerifying {
				t.Fatalf(
					"Incident State = %q; want %q",
					current.State,
					incident.StateVerifying,
				)
			}

			logOutput := harness.logs.String()
			if !strings.Contains(
				logOutput,
				`"msg":"incident_verification_pending"`,
			) {
				t.Fatalf(
					"missing incident_verification_pending log: %s",
					logOutput,
				)
			}
			if strings.Contains(
				logOutput,
				`"msg":"remediation_submitted"`,
			) {
				t.Fatalf(
					"unexpected remediation_submitted log for existing terminal Action Attempt: %s",
					logOutput,
				)
			}
		})
	}
}

func TestHandlePodCrashLoopingCreatesVerificationForRecoveredUnknownActionAttemptWithoutReplay(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(
		t,
		"recovered-unknown-verification",
	)
	harness.grantApproval(
		t,
		harness.now.Add(-time.Minute),
		harness.now.Add(time.Hour),
	)

	actionAttemptStore :=
		&handlerAbandonedActionAttemptStore{}

	verificationStore :=
		&handlerRecordingVerificationStore{
			delegate: remediationdomain.
				NewMemoryVerificationStore(),
		}

	agent := harness.newAgent(
		harness.registry,
		harness.approvalStore,
	)
	agent.actionAttempts = actionAttemptStore
	agent.verifications = verificationStore
	agent.verificationLifecycle =
		&agentVerificationLifecycleAdapter{
			registry: harness.registry,
			store:    verificationStore,
		}
	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	if actionAttemptStore.beginCalls != 1 {
		t.Fatalf(
			"ActionAttempt Begin calls = %d; want 1",
			actionAttemptStore.beginCalls,
		)
	}
	if actionAttemptStore.recoverCalls != 1 {
		t.Fatalf(
			"ActionAttempt Recover calls = %d; want 1",
			actionAttemptStore.recoverCalls,
		)
	}
	if actionAttemptStore.completeCalls != 0 {
		t.Fatalf(
			"ActionAttempt Complete calls = %d; want 0",
			actionAttemptStore.completeCalls,
		)
	}

	if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none after recovering abandoned Action Attempt",
			actions,
		)
	}

	if actionAttemptStore.recovered.Status !=
		remediationdomain.ActionAttemptStatusUnknown {
		t.Fatalf(
			"recovered ActionAttempt Status = %q; want %q",
			actionAttemptStore.recovered.Status,
			remediationdomain.
				ActionAttemptStatusUnknown,
		)
	}
	if actionAttemptStore.recovered.
		RecoveredByFencingToken == 0 {
		t.Fatal(
			"recovered ActionAttempt RecoveredByFencingToken = 0; want positive token",
		)
	}

	if verificationStore.beginCalls != 1 {
		t.Fatalf(
			"Verification Begin calls = %d; want 1",
			verificationStore.beginCalls,
		)
	}
	if len(verificationStore.beginCommands) != 1 {
		t.Fatalf(
			"Verification Begin commands = %d; want 1",
			len(verificationStore.beginCommands),
		)
	}
	if len(verificationStore.beginResults) != 1 {
		t.Fatalf(
			"Verification Begin results = %d; want 1",
			len(verificationStore.beginResults),
		)
	}
	if !verificationStore.beginCreated[0] {
		t.Fatal(
			"Verification Begin created = false; want true",
		)
	}

	command := verificationStore.beginCommands[0]
	verification := verificationStore.beginResults[0]

	if command.ActionAttempt.Status !=
		remediationdomain.ActionAttemptStatusUnknown {
		t.Fatalf(
			"Verification ActionAttempt Status = %q; want %q",
			command.ActionAttempt.Status,
			remediationdomain.
				ActionAttemptStatusUnknown,
		)
	}
	if command.ActionAttempt.ID !=
		actionAttemptStore.recovered.ID {
		t.Fatalf(
			"Verification ActionAttempt ID = %q; want %q",
			command.ActionAttempt.ID,
			actionAttemptStore.recovered.ID,
		)
	}
	if verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"Verification Status = %q; want %q",
			verification.Status,
			remediationdomain.
				VerificationStatusPending,
		)
	}
	if verification.ActionKey !=
		actionAttemptStore.recovered.Key.ActionKey() {
		t.Fatalf(
			"Verification ActionKey = %#v; want %#v",
			verification.ActionKey,
			actionAttemptStore.recovered.Key.ActionKey(),
		)
	}

	wantSubject := verificationSubjectForPod(
		harness.evidence,
	)
	if command.Subject != wantSubject {
		t.Fatalf(
			"Verification Subject = %#v; want %#v",
			command.Subject,
			wantSubject,
		)
	}

	current := harness.currentIncident(t)
	if current.State != incident.StateVerifying {
		t.Fatalf(
			"Incident State = %q; want %q",
			current.State,
			incident.StateVerifying,
		)
	}

	logOutput := harness.logs.String()
	if !strings.Contains(
		logOutput,
		`"msg":"action_attempt_recovered"`,
	) {
		t.Fatalf(
			"missing action_attempt_recovered log: %s",
			logOutput,
		)
	}
	if !strings.Contains(
		logOutput,
		`"msg":"incident_verification_pending"`,
	) {
		t.Fatalf(
			"missing incident_verification_pending log: %s",
			logOutput,
		)
	}
	if strings.Contains(
		logOutput,
		`"msg":"remediation_submitted"`,
	) {
		t.Fatalf(
			"unexpected remediation_submitted log after UNKNOWN recovery: %s",
			logOutput,
		)
	}
}

type handlerRecordingVerificationStore struct {
	delegate      remediationdomain.VerificationStore
	beginCalls    int
	beginCommands []remediationdomain.BeginVerificationCommand
	beginResults  []remediationdomain.Verification
	beginCreated  []bool
}

func (store *handlerRecordingVerificationStore) Begin(
	ctx context.Context,
	command remediationdomain.BeginVerificationCommand,
) (remediationdomain.Verification, bool, error) {
	store.beginCalls++
	store.beginCommands = append(
		store.beginCommands,
		command,
	)

	verification, created, err :=
		store.delegate.Begin(
			ctx,
			command,
		)
	if err != nil {
		return remediationdomain.Verification{}, false, err
	}

	store.beginResults = append(
		store.beginResults,
		verification,
	)
	store.beginCreated = append(
		store.beginCreated,
		created,
	)

	return verification, created, nil
}

func (store *handlerRecordingVerificationStore) Complete(
	ctx context.Context,
	command remediationdomain.CompleteVerificationCommand,
) (remediationdomain.Verification, error) {
	return store.delegate.Complete(
		ctx,
		command,
	)
}

type handlerExistingTerminalActionAttemptStore struct {
	status        remediationdomain.ActionAttemptStatus
	delegate      *remediationdomain.MemoryActionAttemptStore
	beginCalls    int
	completeCalls int
	recoverCalls  int
	attempt       remediationdomain.ActionAttempt
}

func (store *handlerExistingTerminalActionAttemptStore) Begin(
	ctx context.Context,
	command remediationdomain.BeginActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	if err := ctx.Err(); err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	store.beginCalls++

	if store.delegate == nil {
		store.delegate =
			remediationdomain.NewMemoryActionAttemptStore()

		if command.Key.FencingToken <= 1 {
			return remediationdomain.ActionAttempt{}, false,
				remediationdomain.ErrInvalidActionAttempt
		}

		ownerCommand := command
		ownerCommand.Key.FencingToken--
		ownerCommand.StartedAt = command.StartedAt.Add(
			-2 * time.Minute,
		)

		started, created, err := store.delegate.Begin(
			ctx,
			ownerCommand,
		)
		if err != nil {
			return remediationdomain.ActionAttempt{}, false, err
		}
		if !created {
			return remediationdomain.ActionAttempt{}, false,
				remediationdomain.ErrInvalidActionAttempt
		}

		errorCode := ""
		if store.status ==
			remediationdomain.ActionAttemptStatusFailed {
			errorCode = "KUBERNETES_ACTION_FAILED"
		}

		completed, err := store.delegate.Complete(
			ctx,
			remediationdomain.CompleteActionAttemptCommand{
				Key:             started.Key,
				ExpectedVersion: started.Version,
				To:              store.status,
				FinishedAt: command.StartedAt.Add(
					-time.Minute,
				),
				ErrorCode: errorCode,
			},
		)
		if err != nil {
			return remediationdomain.ActionAttempt{}, false, err
		}

		store.attempt = completed
	}

	existing, created, err := store.delegate.Begin(
		ctx,
		command,
	)
	if err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	store.attempt = existing

	return existing, created, nil
}

func (store *handlerExistingTerminalActionAttemptStore) Complete(
	ctx context.Context,
	command remediationdomain.CompleteActionAttemptCommand,
) (remediationdomain.ActionAttempt, error) {
	store.completeCalls++

	return store.delegate.Complete(
		ctx,
		command,
	)
}

func (store *handlerExistingTerminalActionAttemptStore) Recover(
	ctx context.Context,
	command remediationdomain.RecoverActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	store.recoverCalls++

	return store.delegate.Recover(
		ctx,
		command,
	)
}

type handlerAbandonedActionAttemptStore struct {
	beginCalls    int
	completeCalls int
	recoverCalls  int
	started       remediationdomain.ActionAttempt
	recovered     remediationdomain.ActionAttempt
}

func (store *handlerAbandonedActionAttemptStore) Begin(
	ctx context.Context,
	command remediationdomain.BeginActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	if err := ctx.Err(); err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	store.beginCalls++

	if command.Key.FencingToken <= 1 {
		return remediationdomain.ActionAttempt{}, false,
			remediationdomain.ErrInvalidActionAttempt
	}

	ownerCommand := command
	ownerCommand.Key.FencingToken--

	attempt, err := remediationdomain.NewActionAttempt(
		ownerCommand,
	)
	if err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	store.started = attempt

	return attempt, false, nil
}

func (store *handlerAbandonedActionAttemptStore) Complete(
	context.Context,
	remediationdomain.CompleteActionAttemptCommand,
) (remediationdomain.ActionAttempt, error) {
	store.completeCalls++

	return store.started, nil
}

func (store *handlerAbandonedActionAttemptStore) Recover(
	ctx context.Context,
	command remediationdomain.RecoverActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	if err := ctx.Err(); err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	store.recoverCalls++

	finishedAt := command.RecoveredAt
	recovered := store.started
	recovered.Status =
		remediationdomain.ActionAttemptStatusUnknown
	recovered.Version++
	recovered.FinishedAt = &finishedAt
	recovered.ErrorCode = command.ReasonCode
	recovered.RecoveredByFencingToken =
		command.Key.FencingToken

	store.recovered = recovered

	return recovered, true, nil
}

func newHandlerTerminalActionAttempt(
	t *testing.T,
	command remediationdomain.BeginActionAttemptCommand,
	status remediationdomain.ActionAttemptStatus,
) remediationdomain.ActionAttempt {
	t.Helper()

	attempt, err := remediationdomain.NewActionAttempt(
		command,
	)
	if err != nil {
		t.Fatalf(
			"NewActionAttempt() error = %v",
			err,
		)
	}

	finishedAt := command.StartedAt.Add(time.Second)

	attempt.Status = status
	attempt.Version = 2
	attempt.FinishedAt = &finishedAt

	if status ==
		remediationdomain.ActionAttemptStatusFailed {
		attempt.ErrorCode = "KUBERNETES_ACTION_FAILED"
	}

	return attempt
}
