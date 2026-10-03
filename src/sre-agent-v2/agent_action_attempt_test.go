package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	remediationdomain "sre-agent/internal/remediation"
)

func TestHandlePodCrashLoopingBeginsActionAttemptBeforeKubernetesAction(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(
		t,
		"action-attempt-before-kubernetes",
	)
	harness.grantApproval(
		t,
		harness.now.Add(-time.Minute),
		harness.now.Add(time.Hour),
	)

	store := &recordingAgentActionAttemptStore{
		delegate: remediationdomain.NewMemoryActionAttemptStore(),
	}
	store.onBegin = func(
		command remediationdomain.BeginActionAttemptCommand,
	) {
		if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
			t.Fatalf(
				"Kubernetes actions before ActionAttempt Begin = %#v; want none",
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

	if len(store.commands) != 1 {
		t.Fatalf(
			"ActionAttempt Begin calls = %d; want 1",
			len(store.commands),
		)
	}

	command := store.commands[0]

	if command.Key.IncidentID != harness.observedIncident.ID {
		t.Fatalf(
			"ExecutionKey IncidentID = %q; want %q",
			command.Key.IncidentID,
			harness.observedIncident.ID,
		)
	}
	if command.Key.PlanHash != harness.plan.Hash {
		t.Fatalf(
			"ExecutionKey PlanHash = %q; want %q",
			command.Key.PlanHash,
			harness.plan.Hash,
		)
	}
	if command.Key.TargetUID != harness.evidence.Target.UID {
		t.Fatalf(
			"ExecutionKey TargetUID = %q; want %q",
			command.Key.TargetUID,
			harness.evidence.Target.UID,
		)
	}
	if !command.StartedAt.Equal(harness.now) {
		t.Fatalf(
			"ActionAttempt StartedAt = %s; want %s",
			command.StartedAt,
			harness.now,
		)
	}

	claimHistory, err := harness.registry.ClaimHistory(
		context.Background(),
		harness.observedIncident.ID,
	)
	if err != nil {
		t.Fatalf("ClaimHistory() error = %v", err)
	}
	if len(claimHistory) == 0 {
		t.Fatal("ClaimHistory() is empty")
	}

	expectedFencingToken :=
		claimHistory[len(claimHistory)-1].IncidentVersion

	if command.Key.FencingToken != expectedFencingToken {
		t.Fatalf(
			"ExecutionKey FencingToken = %d; want latest Claim version %d",
			command.Key.FencingToken,
			expectedFencingToken,
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
}

func TestHandlePodCrashLoopingFailsClosedWhenActionAttemptCannotBegin(
	t *testing.T,
) {
	testCases := []struct {
		name  string
		store remediationdomain.ActionAttemptStore
	}{
		{
			name:  "unconfigured",
			store: nil,
		},
		{
			name: "store-unavailable",
			store: failingAgentActionAttemptStore{
				err: errAgentActionAttemptStoreUnavailable,
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newWaitingApprovalHarness(
				t,
				"action-attempt-"+testCase.name,
			)
			harness.grantApproval(
				t,
				harness.now.Add(-time.Minute),
				harness.now.Add(time.Hour),
			)

			agent := harness.newAgent(
				harness.registry,
				harness.approvalStore,
			)
			agent.actionAttempts = testCase.store

			agent.handlePodCrashLooping(
				context.Background(),
				harness.alert,
			)

			if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
				t.Fatalf(
					"Kubernetes actions = %#v; want none",
					actions,
				)
			}

			if !strings.Contains(
				harness.logs.String(),
				`"error_code":"ACTION_ATTEMPT_BEGIN_FAILED"`,
			) {
				t.Fatalf(
					"missing ACTION_ATTEMPT_BEGIN_FAILED log: %s",
					harness.logs.String(),
				)
			}
		})
	}
}

func TestHandlePodCrashLoopingDoesNotRepeatExistingActionAttempt(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(
		t,
		"duplicate-action-attempt",
	)
	harness.grantApproval(
		t,
		harness.now.Add(-time.Minute),
		harness.now.Add(time.Hour),
	)

	store := &duplicateAgentActionAttemptStore{}

	agent := harness.newAgent(
		harness.registry,
		harness.approvalStore,
	)
	agent.actionAttempts = store

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	if store.calls != 1 {
		t.Fatalf(
			"ActionAttempt Begin calls = %d; want 1",
			store.calls,
		)
	}
	if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none for existing ExecutionKey",
			actions,
		)
	}

	if !strings.Contains(
		harness.logs.String(),
		`"error_code":"DUPLICATE_ACTION_ATTEMPT"`,
	) {
		t.Fatalf(
			"missing DUPLICATE_ACTION_ATTEMPT log: %s",
			harness.logs.String(),
		)
	}
}

type recordingAgentActionAttemptStore struct {
	delegate remediationdomain.ActionAttemptStore
	commands []remediationdomain.BeginActionAttemptCommand
	onBegin  func(remediationdomain.BeginActionAttemptCommand)
}

func (store *recordingAgentActionAttemptStore) Begin(
	ctx context.Context,
	command remediationdomain.BeginActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	store.commands = append(store.commands, command)

	if store.onBegin != nil {
		store.onBegin(command)
	}

	return store.delegate.Begin(ctx, command)
}

var errAgentActionAttemptStoreUnavailable = errors.New(
	"action attempt store unavailable",
)

type failingAgentActionAttemptStore struct {
	err error
}

func (store failingAgentActionAttemptStore) Begin(
	context.Context,
	remediationdomain.BeginActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	return remediationdomain.ActionAttempt{}, false, store.err
}

type duplicateAgentActionAttemptStore struct {
	calls int
}

func (store *duplicateAgentActionAttemptStore) Begin(
	ctx context.Context,
	command remediationdomain.BeginActionAttemptCommand,
) (remediationdomain.ActionAttempt, bool, error) {
	if err := ctx.Err(); err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	store.calls++

	attempt, err := remediationdomain.NewActionAttempt(command)
	if err != nil {
		return remediationdomain.ActionAttempt{}, false, err
	}

	return attempt, false, nil
}
