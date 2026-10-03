package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"sre-agent/internal/approval"
	"sre-agent/internal/incident"

	remediationdomain "sre-agent/internal/remediation"
)

func TestHandlePodCrashLoopingMovesIncidentToWaitingApproval(
	t *testing.T,
) {
	testCases := []struct {
		name             string
		grantApproval    bool
		approvedAtOffset time.Duration
		expiresAtOffset  time.Duration
	}{
		{
			name: "missing",
		},
		{
			name:             "expired",
			grantApproval:    true,
			approvedAtOffset: -2 * time.Hour,
			expiresAtOffset:  -time.Hour,
		},
		{
			name:             "not-yet-valid",
			grantApproval:    true,
			approvedAtOffset: time.Hour,
			expiresAtOffset:  2 * time.Hour,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newWaitingApprovalHarness(
				t,
				testCase.name,
			)

			if testCase.grantApproval {
				harness.grantApproval(
					t,
					harness.now.Add(
						testCase.approvedAtOffset,
					),
					harness.now.Add(
						testCase.expiresAtOffset,
					),
				)
			}

			agent := harness.newAgent(
				harness.registry,
				harness.approvalStore,
			)

			agent.handlePodCrashLooping(
				context.Background(),
				harness.alert,
			)

			current := harness.currentIncident(t)

			if current.State != incident.StateWaitingApproval {
				t.Fatalf(
					"Incident State = %q; want %q",
					current.State,
					incident.StateWaitingApproval,
				)
			}
			if current.ApprovalBinding.PlanHash !=
				harness.plan.Hash {
				t.Fatalf(
					"ApprovalBinding.PlanHash = %q; want %q",
					current.ApprovalBinding.PlanHash,
					harness.plan.Hash,
				)
			}
			if current.ApprovalBinding.TargetUID !=
				harness.evidence.Target.UID {
				t.Fatalf(
					"ApprovalBinding.TargetUID = %q; want %q",
					current.ApprovalBinding.TargetUID,
					harness.evidence.Target.UID,
				)
			}

			harness.assertNoLegacySideEffects(t)

			logOutput := harness.logs.String()
			if !strings.Contains(
				logOutput,
				`"msg":"incident_waiting_approval"`,
			) {
				t.Fatalf(
					"missing incident_waiting_approval log: %s",
					logOutput,
				)
			}
			if !strings.Contains(
				logOutput,
				`"result":"WAITING_APPROVAL"`,
			) {
				t.Fatalf(
					"missing WAITING_APPROVAL result: %s",
					logOutput,
				)
			}
			if !strings.Contains(
				logOutput,
				`"error_code":"APPROVAL_REQUIRED"`,
			) {
				t.Fatalf(
					"missing APPROVAL_REQUIRED code: %s",
					logOutput,
				)
			}
		})
	}
}

func TestHandlePodCrashLoopingDoesNotPretendWaitingWhenApprovalStoreFails(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(
		t,
		"store-unavailable",
	)

	agent := harness.newAgent(
		harness.registry,
		unavailableWaitingApprovalStore{},
	)

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	current := harness.currentIncident(t)
	if current.State != incident.StateDiagnosed {
		t.Fatalf(
			"Incident State = %q; want %q after approval store failure",
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

	harness.assertNoLegacySideEffects(t)

	logOutput := harness.logs.String()
	if !strings.Contains(
		logOutput,
		`"error_code":"APPROVAL_CHECK_FAILED"`,
	) {
		t.Fatalf(
			"missing approval failure log: %s",
			logOutput,
		)
	}
	if strings.Contains(
		logOutput,
		`"msg":"incident_waiting_approval"`,
	) {
		t.Fatalf(
			"unexpected waiting log after store failure: %s",
			logOutput,
		)
	}
}

func TestHandlePodCrashLoopingDoesNotRecordWaitingWhenTransitionConflicts(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(
		t,
		"transition-conflict",
	)
	registry := &waitingApprovalTransitionFailingRegistry{
		delegate:   harness.registry,
		waitingErr: incident.ErrVersionConflict,
	}

	agent := harness.newAgent(
		registry,
		harness.approvalStore,
	)

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	if registry.waitingCalls != 1 {
		t.Fatalf(
			"WAITING_APPROVAL Transition calls = %d; want 1",
			registry.waitingCalls,
		)
	}

	current := harness.currentIncident(t)
	if current.State != incident.StateDiagnosed {
		t.Fatalf(
			"Incident State = %q; want %q after transition conflict",
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

	harness.assertNoLegacySideEffects(t)

	logOutput := harness.logs.String()
	if !strings.Contains(
		logOutput,
		`"error_code":"INCIDENT_WAITING_APPROVAL_TRANSITION_FAILED"`,
	) {
		t.Fatalf(
			"missing waiting transition failure log: %s",
			logOutput,
		)
	}
	if strings.Contains(
		logOutput,
		`"msg":"incident_waiting_approval"`,
	) {
		t.Fatalf(
			"unexpected waiting success log: %s",
			logOutput,
		)
	}
}

func TestHandlePodCrashLoopingDoesNotWaitWhenPolicyRejectsCandidate(
	t *testing.T,
) {
	harness := newWaitingApprovalHarness(
		t,
		"low-confidence",
	)
	harness.decision.Confidence = 0.10

	agent := harness.newAgent(
		harness.registry,
		harness.approvalStore,
	)

	agent.handlePodCrashLooping(
		context.Background(),
		harness.alert,
	)

	current := harness.currentIncident(t)
	if current.State != incident.StateDiagnosed {
		t.Fatalf(
			"Incident State = %q; want %q after policy rejection",
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

	if harness.stateStore.recordCalls != 1 {
		t.Fatalf(
			"remediation state Record calls = %d; want 1",
			harness.stateStore.recordCalls,
		)
	}
	if len(harness.stateStore.records) != 1 ||
		harness.stateStore.records[0].Kind !=
			remediationStateRecordIncidentHandled {
		t.Fatalf(
			"remediation state records = %#v; want one handled record",
			harness.stateStore.records,
		)
	}
	if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none",
			actions,
		)
	}

	logOutput := harness.logs.String()
	if !strings.Contains(
		logOutput,
		`"error_code":"LOW_CONFIDENCE"`,
	) {
		t.Fatalf(
			"missing LOW_CONFIDENCE denial log: %s",
			logOutput,
		)
	}
	if strings.Contains(
		logOutput,
		`"msg":"incident_waiting_approval"`,
	) {
		t.Fatalf(
			"policy-rejected candidate entered WAITING_APPROVAL: %s",
			logOutput,
		)
	}
}

type waitingApprovalHarness struct {
	now              time.Time
	alert            Alert
	evidence         PodEvidence
	decision         Decision
	observation      incident.Observation
	registry         *incident.Registry
	observedIncident incident.Incident
	plan             approval.Plan
	approvalStore    *approval.MemoryStore
	stateStore       *waitingApprovalStateStore
	kubernetesClient *fake.Clientset
	logs             bytes.Buffer
}

func newWaitingApprovalHarness(
	t *testing.T,
	suffix string,
) *waitingApprovalHarness {
	t.Helper()

	now := time.Date(
		2026,
		9,
		29,
		12,
		0,
		0,
		0,
		time.UTC,
	)
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-" + suffix,
		},
		Annotations: map[string]string{
			"description": "CrashLoopBackOff",
		},
		State:    "firing",
		ActiveAt: now,
	}
	evidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      alert.Labels["pod"],
			UID:       "pod-uid-" + suffix,
		},
		Owner: OwnerEvidence{
			Kind: "ReplicaSet",
			Name: "crash-app-rs-" + suffix,
			UID:  "rs-uid-" + suffix,
		},
		Containers: []ContainerEvidence{
			{
				Name:         "main",
				State:        "waiting",
				Reason:       "CrashLoopBackOff",
				RestartCount: 4,
			},
		},
	}
	observation := incident.Observation{
		Source:    "prometheus",
		Cluster:   evidence.Target.Cluster,
		AlertName: alert.Labels["alertname"],
		Target: incident.Target{
			Kind:      evidence.Target.Kind,
			Namespace: evidence.Target.Namespace,
			Name:      evidence.Target.Name,
			UID:       evidence.Target.UID,
		},
	}

	registry := incident.NewMemoryRegistry()
	observedIncident, created, err := registry.Observe(
		context.Background(),
		observation,
	)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	decision := policyTestDecision(ActionRestartPod)
	decision.IncidentID = incidentIDFor(
		alert,
		evidence.Target.UID,
	)
	decision.Target = evidence.Target

	plan, err := approval.NewPlan(
		approval.PlanCommand{
			IncidentID: observedIncident.ID,
			Action:     string(decision.Action),
			Target: approval.Target{
				Cluster:   evidence.Target.Cluster,
				Namespace: evidence.Target.Namespace,
				Kind:      evidence.Target.Kind,
				Name:      evidence.Target.Name,
				UID:       evidence.Target.UID,
			},
		},
	)
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	kubernetesClient := fake.NewSimpleClientset(
		&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      evidence.Target.Name,
				Namespace: evidence.Target.Namespace,
				UID:       types.UID(evidence.Target.UID),
			},
		},
	)

	return &waitingApprovalHarness{
		now:              now,
		alert:            alert,
		evidence:         evidence,
		decision:         decision,
		observation:      observation,
		registry:         registry,
		observedIncident: observedIncident,
		plan:             plan,
		approvalStore:    approval.NewMemoryStore(),
		stateStore:       &waitingApprovalStateStore{},
		kubernetesClient: kubernetesClient,
	}
}

func (harness *waitingApprovalHarness) grantApproval(
	t *testing.T,
	approvedAt time.Time,
	expiresAt time.Time,
) {
	t.Helper()

	err := harness.approvalStore.Grant(
		context.Background(),
		approval.Approval{
			IncidentID: harness.plan.IncidentID,
			PlanHash:   harness.plan.Hash,
			TargetUID:  harness.plan.Target.UID,
			ApprovedBy: "operator-a",
			ApprovedAt: approvedAt,
			ExpiresAt:  expiresAt,
		},
	)
	if err != nil {
		t.Fatalf("Grant() error = %v", err)
	}
}

func (harness *waitingApprovalHarness) newAgent(
	registry incidentRegistry,
	approvalStore approval.Store,
) *sreAgent {
	return &sreAgent{
		config: agentConfig{
			ClusterName:                "dev",
			AllowedNamespaces:          []string{"default"},
			AllowedControllerKinds:     []string{"ReplicaSet"},
			RestartPodApproved:         true,
			MinimumConfidence:          0.90,
			RestartCooldown:            10 * time.Minute,
			AttemptWindow:              time.Hour,
			MaximumAttempts:            1,
			OllamaTimeout:              time.Second,
			KubernetesRequestTimeout:   time.Second,
			IncidentClaimHolderID:      "agent-a",
			IncidentClaimLeaseDuration: 30 * time.Second,
		},
		kubernetes: harness.kubernetesClient,
		collector: &stubContextCollector{
			evidence: harness.evidence,
		},
		ollama: &stubDecisionSource{
			decision: harness.decision,
		},
		incidents:      registry,
		approvals:      approvalStore,
		plans:          approval.NewMemoryPlanStore(),
		actionAttempts: remediationdomain.NewMemoryActionAttemptStore(),
		memory:         harness.stateStore,
		logger: slog.New(
			slog.NewJSONHandler(&harness.logs, nil),
		),
		now: func() time.Time {
			return harness.now
		},
	}
}

func (harness *waitingApprovalHarness) currentIncident(
	t *testing.T,
) incident.Incident {
	t.Helper()

	current, created, err := harness.registry.Observe(
		context.Background(),
		harness.observation,
	)
	if err != nil {
		t.Fatalf("Observe(current) error = %v", err)
	}
	if created {
		t.Fatal("Observe(current) created a new active Incident")
	}

	return current
}

func (harness *waitingApprovalHarness) assertNoLegacySideEffects(
	t *testing.T,
) {
	t.Helper()

	if harness.stateStore.recordCalls != 0 {
		t.Fatalf(
			"remediation state Record calls = %d; want 0",
			harness.stateStore.recordCalls,
		)
	}
	if actions := harness.kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none",
			actions,
		)
	}
}

type waitingApprovalStateStore struct {
	snapshotCalls int
	recordCalls   int
	records       []remediationStateRecord
}

func (store *waitingApprovalStateStore) Snapshot(
	ctx context.Context,
	_ remediationStateQuery,
) (remediationSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return remediationSnapshot{}, err
	}

	store.snapshotCalls++

	return remediationSnapshot{}, nil
}

func (store *waitingApprovalStateStore) Record(
	ctx context.Context,
	record remediationStateRecord,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store.recordCalls++
	store.records = append(store.records, record)

	return nil
}

var errWaitingApprovalStoreUnavailable = errors.New(
	"approval store unavailable",
)

type unavailableWaitingApprovalStore struct{}

func (unavailableWaitingApprovalStore) Grant(
	context.Context,
	approval.Approval,
) error {
	return errWaitingApprovalStoreUnavailable
}

func (unavailableWaitingApprovalStore) Lookup(
	context.Context,
	approval.ApprovalKey,
) (approval.Approval, error) {
	return approval.Approval{},
		errWaitingApprovalStoreUnavailable
}

type waitingApprovalTransitionFailingRegistry struct {
	delegate     incidentRegistry
	waitingErr   error
	waitingCalls int
}

func (registry *waitingApprovalTransitionFailingRegistry) Observe(
	ctx context.Context,
	observation incident.Observation,
) (incident.Incident, bool, error) {
	return registry.delegate.Observe(ctx, observation)
}

func (registry *waitingApprovalTransitionFailingRegistry) Claim(
	ctx context.Context,
	command incident.ClaimCommand,
) (incident.Claim, error) {
	return registry.delegate.Claim(ctx, command)
}

func (registry *waitingApprovalTransitionFailingRegistry) Transition(
	ctx context.Context,
	command incident.TransitionCommand,
) (incident.Incident, error) {
	if command.To == incident.StateWaitingApproval {
		registry.waitingCalls++

		return incident.Incident{}, registry.waitingErr
	}

	return registry.delegate.Transition(ctx, command)
}
