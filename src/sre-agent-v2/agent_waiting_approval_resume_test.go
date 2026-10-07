package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	k8stesting "k8s.io/client-go/testing"

	"sre-agent/internal/approval"
	"sre-agent/internal/incident"
)

func TestHandlePodCrashLoopingResumesWaitingApprovalWithoutRediagnosis(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(t, "resume")
	agent := harness.newAgent(
		harness.registry,
		harness.approvalStore,
	)

	decisionSource := &countingWaitingApprovalDecisionSource{
		decision: harness.decision,
	}
	agent.ollama = decisionSource

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	waiting := harness.currentIncident(t)
	if waiting.State != incident.StateWaitingApproval {
		t.Fatalf(
			"first handling Incident State = %q; want %q",
			waiting.State,
			incident.StateWaitingApproval,
		)
	}
	if decisionSource.calls != 1 {
		t.Fatalf(
			"decision calls after first handling = %d; want 1",
			decisionSource.calls,
		)
	}
	if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"first handling Kubernetes actions = %#v; want none",
			actions,
		)
	}

	harness.grantApproval(
		t,
		harness.now.Add(-1),
		harness.now.Add(3600),
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

	actions := harness.kubernetesClient.Actions()
	if len(actions) != 1 {
		t.Fatalf(
			"Kubernetes actions = %#v; want one Pod delete",
			actions,
		)
	}

	deleteAction, ok := actions[0].(k8stesting.DeleteAction)
	if !ok {
		t.Fatalf(
			"Kubernetes action type = %T; want DeleteAction",
			actions[0],
		)
	}

	deleteOptions := deleteAction.GetDeleteOptions()
	if deleteOptions.Preconditions == nil ||
		deleteOptions.Preconditions.UID == nil {
		t.Fatalf(
			"delete options = %#v; want UID precondition",
			deleteOptions,
		)
	}
	if got := string(*deleteOptions.Preconditions.UID); got !=
		harness.evidence.Target.UID {
		t.Fatalf(
			"delete UID precondition = %q; want %q",
			got,
			harness.evidence.Target.UID,
		)
	}

	if len(harness.stateStore.records) != 1 ||
		harness.stateStore.records[0].Kind !=
			remediationStateRecordActionAttempted {
		t.Fatalf(
			"remediation records = %#v; want one action-attempt record",
			harness.stateStore.records,
		)
	}

	current := harness.currentIncident(t)

	if current.State != incident.StateVerifying {
		t.Fatalf(
			"Incident State after submission = %q; "+
				"want %q after terminal Action Attempt "+
				"and pending Verification",
			current.State,
			incident.StateVerifying,
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
		`"msg":"remediation_submitted"`,
	) {
		t.Fatalf(
			"missing remediation_submitted log: %s",
			logOutput,
		)
	}
}

func TestHandlePodCrashLoopingKeepsWaitingWithoutApprovalAndDoesNotRediagnose(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(t, "still-waiting")
	agent := harness.newAgent(
		harness.registry,
		harness.approvalStore,
	)

	decisionSource := &countingWaitingApprovalDecisionSource{
		decision: harness.decision,
	}
	agent.ollama = decisionSource

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)
	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	if decisionSource.calls != 1 {
		t.Fatalf(
			"decision calls = %d; want 1 while waiting",
			decisionSource.calls,
		)
	}
	if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none without approval",
			actions,
		)
	}
	if harness.stateStore.recordCalls != 0 {
		t.Fatalf(
			"remediation Record calls = %d; want 0 without approval",
			harness.stateStore.recordCalls,
		)
	}

	current := harness.currentIncident(t)
	if current.State != incident.StateWaitingApproval {
		t.Fatalf(
			"Incident State = %q; want %q",
			current.State,
			incident.StateWaitingApproval,
		)
	}
}

func TestHandlePodCrashLoopingFailsClosedWhenWaitingPlanCannotBeRecovered(
	t *testing.T,
) {
	testCases := []struct {
		name        string
		replacePlan func(
			t *testing.T,
			agent *sreAgent,
			harness *waitingApprovalHarness,
		)
	}{
		{
			name: "missing",
			replacePlan: func(
				_ *testing.T,
				agent *sreAgent,
				_ *waitingApprovalHarness,
			) {
				agent.plans = approval.NewMemoryPlanStore()
			},
		},
		{
			name: "store-unavailable",
			replacePlan: func(
				_ *testing.T,
				agent *sreAgent,
				_ *waitingApprovalHarness,
			) {
				agent.plans = unavailableWaitingApprovalPlanStore{}
			},
		},
		{
			name: "tampered-target",
			replacePlan: func(
				t *testing.T,
				agent *sreAgent,
				harness *waitingApprovalHarness,
			) {
				t.Helper()

				tamperedPlan, err := approval.NewPlan(
					approval.PlanCommand{
						IncidentID: harness.plan.IncidentID,
						Action:     harness.plan.Action,
						Target: approval.Target{
							Cluster: harness.plan.Target.Cluster,
							Namespace: harness.plan.
								Target.Namespace,
							Kind: harness.plan.Target.Kind,
							Name: harness.plan.Target.Name +
								"-replacement",
							UID: harness.plan.Target.UID,
						},
					},
				)
				if err != nil {
					t.Fatalf(
						"NewPlan(tampered) error = %v",
						err,
					)
				}

				agent.plans = staticWaitingApprovalPlanStore{
					plan: tamperedPlan,
				}
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newWaitingApprovalHarness(
				t,
				"recovery-"+testCase.name,
			)
			agent := harness.newAgent(
				harness.registry,
				harness.approvalStore,
			)

			decisionSource :=
				&countingWaitingApprovalDecisionSource{
					decision: harness.decision,
				}
			agent.ollama = decisionSource

			agent.handlePodCrashLooping(
				context.Background(),
				harness.alert,
			)

			harness.grantApproval(
				t,
				harness.now.Add(-1),
				harness.now.Add(3600),
			)

			testCase.replacePlan(t, agent, harness)

			agent.handlePodCrashLooping(
				context.Background(),
				harness.alert,
			)

			if decisionSource.calls != 1 {
				t.Fatalf(
					"decision calls = %d; want 1",
					decisionSource.calls,
				)
			}
			if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
				t.Fatalf(
					"Kubernetes actions = %#v; want none",
					actions,
				)
			}
			if harness.stateStore.recordCalls != 0 {
				t.Fatalf(
					"remediation Record calls = %d; want 0",
					harness.stateStore.recordCalls,
				)
			}
			if !strings.Contains(
				harness.logs.String(),
				`"error_code":"INCIDENT_PLAN_RECOVERY_FAILED"`,
			) {
				t.Fatalf(
					"missing plan recovery failure log: %s",
					harness.logs.String(),
				)
			}
		})
	}
}

func TestHandlePodCrashLoopingDoesNotActWhenWaitingApprovalFenceConflicts(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(t, "resume-fence")
	registry := &waitingApprovalNthClaimFailingRegistry{
		delegate: harness.registry,
		failAt:   3,
		err:      incident.ErrVersionConflict,
	}
	agent := harness.newAgent(
		registry,
		harness.approvalStore,
	)

	decisionSource := &countingWaitingApprovalDecisionSource{
		decision: harness.decision,
	}
	agent.ollama = decisionSource

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	harness.grantApproval(
		t,
		harness.now.Add(-1),
		harness.now.Add(3600),
	)

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	if registry.claimCalls != 3 {
		t.Fatalf(
			"Claim calls = %d; want 3",
			registry.claimCalls,
		)
	}
	if harness.stateStore.recordCalls != 1 {
		t.Fatalf(
			"remediation Record calls before final fence = %d; want 1",
			harness.stateStore.recordCalls,
		)
	}
	if decisionSource.calls != 1 {
		t.Fatalf(
			"decision calls = %d; want 1",
			decisionSource.calls,
		)
	}
	if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none after fencing conflict",
			actions,
		)
	}
	if !strings.Contains(
		harness.logs.String(),
		`"error_code":"INCIDENT_ACTION_FENCE_FAILED"`,
	) {
		t.Fatalf(
			"missing action fence failure log: %s",
			harness.logs.String(),
		)
	}
}

func TestHandlePodCrashLoopingDoesNotEnterWaitingWhenPlanPublishFails(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(t, "publish-failure")
	agent := harness.newAgent(
		harness.registry,
		harness.approvalStore,
	)
	agent.plans = unavailableWaitingApprovalPlanStore{}

	decisionSource := &countingWaitingApprovalDecisionSource{
		decision: harness.decision,
	}
	agent.ollama = decisionSource

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	current := harness.currentIncident(t)
	if current.State != incident.StateDiagnosed {
		t.Fatalf(
			"Incident State = %q; want %q after Plan publish failure",
			current.State,
			incident.StateDiagnosed,
		)
	}
	if current.ApprovalBinding != (incident.ApprovalBinding{}) {
		t.Fatalf(
			"ApprovalBinding = %#v; want empty binding",
			current.ApprovalBinding,
		)
	}
	if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none",
			actions,
		)
	}
	if !strings.Contains(
		harness.logs.String(),
		`"error_code":"INCIDENT_PLAN_PUBLISH_FAILED"`,
	) {
		t.Fatalf(
			"missing Plan publish failure log: %s",
			harness.logs.String(),
		)
	}
}

type countingWaitingApprovalDecisionSource struct {
	decision Decision
	calls    int
}

func (source *countingWaitingApprovalDecisionSource) decide(
	ctx context.Context,
	_ IncidentEvidence,
) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}

	source.calls++

	return source.decision, nil
}

var errWaitingApprovalPlanStoreUnavailable = errors.New(
	"canonical plan store unavailable",
)

type unavailableWaitingApprovalPlanStore struct{}

func (unavailableWaitingApprovalPlanStore) Publish(
	context.Context,
	approval.Plan,
) error {
	return errWaitingApprovalPlanStoreUnavailable
}

func (unavailableWaitingApprovalPlanStore) Lookup(
	context.Context,
	approval.PlanKey,
) (approval.Plan, error) {
	return approval.Plan{},
		errWaitingApprovalPlanStoreUnavailable
}

type staticWaitingApprovalPlanStore struct {
	plan approval.Plan
}

func (store staticWaitingApprovalPlanStore) Publish(
	context.Context,
	approval.Plan,
) error {
	return nil
}

func (store staticWaitingApprovalPlanStore) Lookup(
	context.Context,
	approval.PlanKey,
) (approval.Plan, error) {
	return store.plan, nil
}

type waitingApprovalNthClaimFailingRegistry struct {
	delegate   incidentRegistry
	failAt     int
	err        error
	claimCalls int
}

func (registry *waitingApprovalNthClaimFailingRegistry) Observe(
	ctx context.Context,
	observation incident.Observation,
) (incident.Incident, bool, error) {
	return registry.delegate.Observe(ctx, observation)
}

func (registry *waitingApprovalNthClaimFailingRegistry) Claim(
	ctx context.Context,
	command incident.ClaimCommand,
) (incident.Claim, error) {
	registry.claimCalls++

	if registry.claimCalls == registry.failAt {
		return incident.Claim{}, registry.err
	}

	return registry.delegate.Claim(ctx, command)
}

func (registry *waitingApprovalNthClaimFailingRegistry) Transition(
	ctx context.Context,
	command incident.TransitionCommand,
) (incident.Incident, error) {
	return registry.delegate.Transition(ctx, command)
}
