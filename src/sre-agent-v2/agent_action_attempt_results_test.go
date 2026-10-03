package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	remediationdomain "sre-agent/internal/remediation"
)

func TestHandlePodCrashLoopingPersistsSuccessfulActionAttemptResult(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(
		t,
		"action-attempt-success-result",
	)
	harness.grantApproval(
		t,
		harness.now.Add(-time.Minute),
		harness.now.Add(time.Hour),
	)

	store := &observingAgentActionAttemptStore{
		delegate: remediationdomain.
			NewMemoryActionAttemptStore(),
	}
	store.onComplete = func(
		command remediationdomain.CompleteActionAttemptCommand,
	) {
		actions := harness.kubernetesClient.Actions()
		if len(actions) != 1 ||
			actions[0].GetVerb() != "delete" ||
			actions[0].GetResource().Resource != "pods" {
			t.Fatalf(
				"Kubernetes actions before Complete = %#v; want one Pod delete",
				actions,
			)
		}
	}

	agent := harness.newAgent(
		harness.registry,
		harness.approvalStore,
	)
	agent.actionAttempts = store

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	if len(store.completeCommands) != 1 {
		t.Fatalf(
			"ActionAttempt Complete calls = %d; want 1",
			len(store.completeCommands),
		)
	}

	command := store.completeCommands[0]

	if command.To !=
		remediationdomain.ActionAttemptStatusSucceeded {
		t.Fatalf(
			"Complete To = %q; want %q",
			command.To,
			remediationdomain.
				ActionAttemptStatusSucceeded,
		)
	}
	if command.ExpectedVersion != 1 {
		t.Fatalf(
			"Complete ExpectedVersion = %d; want 1",
			command.ExpectedVersion,
		)
	}
	if command.ErrorCode != "" {
		t.Fatalf(
			"Complete ErrorCode = %q; want empty",
			command.ErrorCode,
		)
	}
	if command.FinishedAt.IsZero() {
		t.Fatal("Complete FinishedAt is zero")
	}
	if len(store.recoverCommands) != 0 {
		t.Fatalf(
			"ActionAttempt Recover calls = %d; want 0",
			len(store.recoverCommands),
		)
	}
}

func TestHandlePodCrashLoopingPersistsFailedActionAttemptResult(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(
		t,
		"action-attempt-failed-result",
	)
	harness.grantApproval(
		t,
		harness.now.Add(-time.Minute),
		harness.now.Add(time.Hour),
	)

	actionErr := errors.New("injected Kubernetes action failure")

	harness.kubernetesClient.PrependReactor(
		"delete",
		"pods",
		func(
			k8stesting.Action,
		) (bool, runtime.Object, error) {
			return true, nil, actionErr
		},
	)

	store := &observingAgentActionAttemptStore{
		delegate: remediationdomain.
			NewMemoryActionAttemptStore(),
	}

	agent := harness.newAgent(
		harness.registry,
		harness.approvalStore,
	)
	agent.actionAttempts = store

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	if len(store.completeCommands) != 1 {
		t.Fatalf(
			"ActionAttempt Complete calls = %d; want 1",
			len(store.completeCommands),
		)
	}

	command := store.completeCommands[0]

	if command.To !=
		remediationdomain.ActionAttemptStatusFailed {
		t.Fatalf(
			"Complete To = %q; want %q",
			command.To,
			remediationdomain.ActionAttemptStatusFailed,
		)
	}
	if command.ExpectedVersion != 1 {
		t.Fatalf(
			"Complete ExpectedVersion = %d; want 1",
			command.ExpectedVersion,
		)
	}
	if command.ErrorCode != "KUBERNETES_ACTION_FAILED" {
		t.Fatalf(
			"Complete ErrorCode = %q; want %q",
			command.ErrorCode,
			"KUBERNETES_ACTION_FAILED",
		)
	}
	if command.FinishedAt.IsZero() {
		t.Fatal("Complete FinishedAt is zero")
	}
	if !strings.Contains(
		harness.logs.String(),
		`"error_code":"KUBERNETES_ACTION_FAILED"`,
	) {
		t.Fatalf(
			"missing KUBERNETES_ACTION_FAILED log: %s",
			harness.logs.String(),
		)
	}
}

func TestHandlePodCrashLoopingRecoversAbandonedStartedAttemptWithoutReplay(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(
		t,
		"action-attempt-abandoned-recovery",
	)
	harness.grantApproval(
		t,
		harness.now.Add(-time.Minute),
		harness.now.Add(time.Hour),
	)

	store := &abandonedAgentActionAttemptStore{}

	agent := harness.newAgent(
		harness.registry,
		harness.approvalStore,
	)
	agent.actionAttempts = store

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	if store.beginCalls != 1 {
		t.Fatalf(
			"ActionAttempt Begin calls = %d; want 1",
			store.beginCalls,
		)
	}
	if len(store.recoverCommands) != 1 {
		t.Fatalf(
			"ActionAttempt Recover calls = %d; want 1",
			len(store.recoverCommands),
		)
	}

	command := store.recoverCommands[0]

	if command.Key.FencingToken <=
		store.started.Key.FencingToken {
		t.Fatalf(
			"recovery FencingToken = %d; want greater than original %d",
			command.Key.FencingToken,
			store.started.Key.FencingToken,
		)
	}
	if command.ReasonCode != "PREVIOUS_EXECUTOR_LOST" {
		t.Fatalf(
			"Recover ReasonCode = %q; want %q",
			command.ReasonCode,
			"PREVIOUS_EXECUTOR_LOST",
		)
	}
	if command.RecoveredAt.IsZero() {
		t.Fatal("Recover RecoveredAt is zero")
	}

	if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none after abandoned attempt recovery",
			actions,
		)
	}
	if len(store.completeCommands) != 0 {
		t.Fatalf(
			"ActionAttempt Complete calls = %d; want 0",
			len(store.completeCommands),
		)
	}
	if !strings.Contains(
		harness.logs.String(),
		`"error_code":"PREVIOUS_EXECUTOR_LOST"`,
	) {
		t.Fatalf(
			"missing PREVIOUS_EXECUTOR_LOST log: %s",
			harness.logs.String(),
		)
	}
}

func TestHandlePodCrashLoopingDoesNotReportSubmissionWhenActionAttemptResultPersistenceFails(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(
		t,
		"action-attempt-result-failure",
	)
	harness.grantApproval(
		t,
		harness.now.Add(-time.Minute),
		harness.now.Add(time.Hour),
	)

	store := &observingAgentActionAttemptStore{
		delegate: remediationdomain.
			NewMemoryActionAttemptStore(),
		completeErr: errors.New(
			"injected action attempt result persistence failure",
		),
	}

	agent := harness.newAgent(
		harness.registry,
		harness.approvalStore,
	)
	agent.actionAttempts = store

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	actions := harness.kubernetesClient.Actions()
	if len(actions) != 1 ||
		actions[0].GetVerb() != "delete" ||
		actions[0].GetResource().Resource != "pods" {
		t.Fatalf(
			"Kubernetes actions = %#v; want one Pod delete",
			actions,
		)
	}
	if len(store.completeCommands) != 1 {
		t.Fatalf(
			"ActionAttempt Complete calls = %d; want 1",
			len(store.completeCommands),
		)
	}

	if !strings.Contains(
		harness.logs.String(),
		`"error_code":"ACTION_ATTEMPT_RESULT_PERSIST_FAILED"`,
	) {
		t.Fatalf(
			"missing ACTION_ATTEMPT_RESULT_PERSIST_FAILED log: %s",
			harness.logs.String(),
		)
	}
	if strings.Contains(
		harness.logs.String(),
		`"msg":"remediation_submitted"`,
	) {
		t.Fatalf(
			"unexpected remediation_submitted log after result persistence failure: %s",
			harness.logs.String(),
		)
	}
}

type observingAgentActionAttemptStore struct {
	delegate remediationdomain.ActionAttemptStore

	beginCommands    []remediationdomain.BeginActionAttemptCommand
	completeCommands []remediationdomain.CompleteActionAttemptCommand
	recoverCommands  []remediationdomain.RecoverActionAttemptCommand

	completeErr error
	recoverErr  error

	onComplete func(
		remediationdomain.CompleteActionAttemptCommand,
	)
}

func (store *observingAgentActionAttemptStore) Begin(
	ctx context.Context,
	command remediationdomain.BeginActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	store.beginCommands = append(
		store.beginCommands,
		command,
	)

	return store.delegate.Begin(ctx, command)
}

func (store *observingAgentActionAttemptStore) Complete(
	ctx context.Context,
	command remediationdomain.CompleteActionAttemptCommand,
) (remediationdomain.ActionAttempt, error) {
	store.completeCommands = append(
		store.completeCommands,
		command,
	)

	if store.onComplete != nil {
		store.onComplete(command)
	}
	if store.completeErr != nil {
		return remediationdomain.ActionAttempt{},
			store.completeErr
	}

	return store.delegate.Complete(ctx, command)
}

func (store *observingAgentActionAttemptStore) Recover(
	ctx context.Context,
	command remediationdomain.RecoverActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	store.recoverCommands = append(
		store.recoverCommands,
		command,
	)

	if store.recoverErr != nil {
		return remediationdomain.ActionAttempt{},
			false,
			store.recoverErr
	}

	return store.delegate.Recover(ctx, command)
}

type abandonedAgentActionAttemptStore struct {
	started          remediationdomain.ActionAttempt
	beginCalls       int
	completeCommands []remediationdomain.CompleteActionAttemptCommand
	recoverCommands  []remediationdomain.RecoverActionAttemptCommand
}

func (store *abandonedAgentActionAttemptStore) Begin(
	ctx context.Context,
	command remediationdomain.BeginActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	if err := ctx.Err(); err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	store.beginCalls++

	if store.started.ID == "" {
		if command.Key.FencingToken <= 1 {
			return remediationdomain.ActionAttempt{},
				false,
				errors.New(
					"current fencing token is too small to construct abandoned attempt",
				)
		}

		abandonedCommand := command
		abandonedCommand.Key.FencingToken--
		abandonedCommand.StartedAt =
			command.StartedAt.Add(-time.Minute)

		started, err := remediationdomain.NewActionAttempt(
			abandonedCommand,
		)
		if err != nil {
			return remediationdomain.ActionAttempt{},
				false,
				err
		}

		store.started = started
	}

	return store.started, false, nil
}

func (store *abandonedAgentActionAttemptStore) Complete(
	ctx context.Context,
	command remediationdomain.CompleteActionAttemptCommand,
) (remediationdomain.ActionAttempt, error) {
	if err := ctx.Err(); err != nil {
		return remediationdomain.ActionAttempt{}, err
	}

	store.completeCommands = append(
		store.completeCommands,
		command,
	)

	return remediationdomain.ActionAttempt{},
		errors.New(
			"unexpected Complete call for abandoned attempt",
		)
}

func (store *abandonedAgentActionAttemptStore) Recover(
	ctx context.Context,
	command remediationdomain.RecoverActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	if err := ctx.Err(); err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}
	if err := command.Validate(); err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	store.recoverCommands = append(
		store.recoverCommands,
		command,
	)

	if store.started.Status ==
		remediationdomain.ActionAttemptStatusUnknown {
		return store.started, false, nil
	}
	if command.Key.FencingToken <=
		store.started.Key.FencingToken {
		return remediationdomain.ActionAttempt{},
			false,
			remediationdomain.
				ErrActionAttemptFencingConflict
	}

	recoveredAt := command.RecoveredAt

	store.started.Status =
		remediationdomain.ActionAttemptStatusUnknown
	store.started.Version++
	store.started.FinishedAt = &recoveredAt
	store.started.ErrorCode = command.ReasonCode
	store.started.RecoveredByFencingToken =
		command.Key.FencingToken

	return store.started, true, nil
}

func (store *recordingAgentActionAttemptStore) Complete(
	ctx context.Context,
	command remediationdomain.CompleteActionAttemptCommand,
) (remediationdomain.ActionAttempt, error) {
	return store.delegate.Complete(ctx, command)
}

func (store *recordingAgentActionAttemptStore) Recover(
	ctx context.Context,
	command remediationdomain.RecoverActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	return store.delegate.Recover(ctx, command)
}

func (store failingAgentActionAttemptStore) Complete(
	context.Context,
	remediationdomain.CompleteActionAttemptCommand,
) (remediationdomain.ActionAttempt, error) {
	return remediationdomain.ActionAttempt{}, store.err
}

func (store failingAgentActionAttemptStore) Recover(
	context.Context,
	remediationdomain.RecoverActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	return remediationdomain.ActionAttempt{},
		false,
		store.err
}

func (store *duplicateAgentActionAttemptStore) Complete(
	context.Context,
	remediationdomain.CompleteActionAttemptCommand,
) (remediationdomain.ActionAttempt, error) {
	return remediationdomain.ActionAttempt{},
		errors.New(
			"unexpected Complete call for duplicate attempt",
		)
}

func (store *duplicateAgentActionAttemptStore) Recover(
	context.Context,
	remediationdomain.RecoverActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	return remediationdomain.ActionAttempt{},
		false,
		errors.New(
			"unexpected Recover call for same-token duplicate attempt",
		)
}

func TestHandlePodCrashLoopingPersistsSuccessfulActionAttemptAfterWaitingApprovalResume(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(
		t,
		"waiting-approval-action-result",
	)

	decisionSource :=
		&countingWaitingApprovalDecisionSource{
			decision: harness.decision,
		}

	store := &observingAgentActionAttemptStore{
		delegate: remediationdomain.
			NewMemoryActionAttemptStore(),
	}
	store.onComplete = func(
		command remediationdomain.CompleteActionAttemptCommand,
	) {
		actions := harness.kubernetesClient.Actions()
		if len(actions) != 1 ||
			actions[0].GetVerb() != "delete" ||
			actions[0].GetResource().Resource != "pods" {
			t.Fatalf(
				"Kubernetes actions before Complete = %#v; want one Pod delete",
				actions,
			)
		}
	}

	agent := harness.newAgent(
		harness.registry,
		harness.approvalStore,
	)
	agent.ollama = decisionSource
	agent.actionAttempts = store

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	if decisionSource.calls != 1 {
		t.Fatalf(
			"decision calls after first handling = %d; want 1",
			decisionSource.calls,
		)
	}
	if len(store.beginCommands) != 0 {
		t.Fatalf(
			"ActionAttempt Begin calls before approval = %d; want 0",
			len(store.beginCommands),
		)
	}
	if len(store.completeCommands) != 0 {
		t.Fatalf(
			"ActionAttempt Complete calls before approval = %d; want 0",
			len(store.completeCommands),
		)
	}
	if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions before approval = %#v; want none",
			actions,
		)
	}

	harness.grantApproval(
		t,
		harness.now.Add(-time.Minute),
		harness.now.Add(time.Hour),
	)

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	if decisionSource.calls != 1 {
		t.Fatalf(
			"decision calls after approval resume = %d; want 1 total",
			decisionSource.calls,
		)
	}
	if len(store.beginCommands) != 1 {
		t.Fatalf(
			"ActionAttempt Begin calls = %d; want 1",
			len(store.beginCommands),
		)
	}
	if len(store.completeCommands) != 1 {
		t.Fatalf(
			"ActionAttempt Complete calls = %d; want 1",
			len(store.completeCommands),
		)
	}

	command := store.completeCommands[0]

	if command.To !=
		remediationdomain.ActionAttemptStatusSucceeded {
		t.Fatalf(
			"Complete To = %q; want %q",
			command.To,
			remediationdomain.
				ActionAttemptStatusSucceeded,
		)
	}
	if command.ExpectedVersion != 1 {
		t.Fatalf(
			"Complete ExpectedVersion = %d; want 1",
			command.ExpectedVersion,
		)
	}
	if command.ErrorCode != "" {
		t.Fatalf(
			"Complete ErrorCode = %q; want empty",
			command.ErrorCode,
		)
	}
	if command.FinishedAt.IsZero() {
		t.Fatal("Complete FinishedAt is zero")
	}
	if len(store.recoverCommands) != 0 {
		t.Fatalf(
			"ActionAttempt Recover calls = %d; want 0",
			len(store.recoverCommands),
		)
	}

	actions := harness.kubernetesClient.Actions()
	if len(actions) != 1 ||
		actions[0].GetVerb() != "delete" ||
		actions[0].GetResource().Resource != "pods" {
		t.Fatalf(
			"Kubernetes actions = %#v; want one Pod delete",
			actions,
		)
	}

	logOutput := harness.logs.String()

	if !strings.Contains(
		logOutput,
		`"msg":"incident_approval_resumed"`,
	) {
		t.Fatalf(
			"missing incident_approval_resumed log: %s",
			logOutput,
		)
	}
	if !strings.Contains(
		logOutput,
		`"msg":"action_attempt_completed"`,
	) {
		t.Fatalf(
			"missing action_attempt_completed log: %s",
			logOutput,
		)
	}
	if !strings.Contains(
		logOutput,
		`"msg":"remediation_submitted"`,
	) {
		t.Fatalf(
			"missing remediation_submitted log: %s",
			logOutput,
		)
	}
}
