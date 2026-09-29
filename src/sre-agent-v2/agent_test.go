package main

import (
	"bytes"
	"context"
	"errors"
	"io"
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
)

func TestHandlePodCrashLoopingConnectsEvidenceDecisionPolicyAndUIDDelete(
	t *testing.T,
) {
	observedAt := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-a",
		},
		Annotations: map[string]string{
			"description": "CrashLoopBackOff",
		},
		State:    "firing",
		ActiveAt: observedAt,
	}
	podEvidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-a",
			UID:       "pod-uid-a",
		},
		Owner: OwnerEvidence{
			Kind: "ReplicaSet",
			Name: "crash-app-rs",
			UID:  "rs-uid",
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

	legacyIncidentID := incidentIDFor(
		alert,
		podEvidence.Target.UID,
	)
	decision := policyTestDecision(ActionRestartPod)
	decision.IncidentID = legacyIncidentID
	decision.Target = podEvidence.Target

	ctx := context.Background()
	registry := incident.NewMemoryRegistry()
	lifecycleIncident, _, err := registry.Observe(
		ctx,
		incident.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: alert.Labels["alertname"],
			Target: incident.Target{
				Kind:      podEvidence.Target.Kind,
				Namespace: podEvidence.Target.Namespace,
				Name:      podEvidence.Target.Name,
				UID:       podEvidence.Target.UID,
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}

	approvalStore := approval.NewMemoryStore()
	mustGrantApproval(
		t,
		approvalStore,
		lifecycleIncident.ID,
		string(decision.Action),
		podEvidence.Target,
		observedAt,
	)

	kubernetesClient := fake.NewSimpleClientset(
		&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podEvidence.Target.Name,
				Namespace: podEvidence.Target.Namespace,
				UID:       types.UID(podEvidence.Target.UID),
			},
		},
	)
	decider := &stubDecisionSource{decision: decision}

	agent := &sreAgent{
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
		kubernetes: kubernetesClient,
		collector:  &stubContextCollector{evidence: podEvidence},
		ollama:     decider,
		incidents:  registry,
		approvals:  approvalStore,
		memory:     newRemediationMemory(),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:        func() time.Time { return observedAt },
	}

	agent.handlePodCrashLooping(ctx, alert)

	if decider.received.IncidentID != legacyIncidentID ||
		decider.received.Pod.Target.UID != "pod-uid-a" {
		t.Fatalf(
			"decider received %#v; want current incident and observed Pod UID",
			decider.received,
		)
	}

	actions := kubernetesClient.Actions()
	if len(actions) != 1 ||
		actions[0].GetVerb() != "delete" ||
		actions[0].GetResource().Resource != "pods" {
		t.Fatalf(
			"Kubernetes actions = %#v; want exactly one Pod delete",
			actions,
		)
	}
}

func TestHandlePodCrashLoopingDoesNotActWithoutStoredApproval(t *testing.T) {
	observedAt := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-unapproved",
		},
		Annotations: map[string]string{
			"description": "CrashLoopBackOff",
		},
		State:    "firing",
		ActiveAt: observedAt,
	}
	podEvidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-unapproved",
			UID:       "pod-uid-unapproved",
		},
		Owner: OwnerEvidence{
			Kind: "ReplicaSet",
			Name: "crash-app-rs",
			UID:  "rs-uid-unapproved",
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

	incidentID := incidentIDFor(
		alert,
		podEvidence.Target.UID,
	)
	decision := policyTestDecision(ActionRestartPod)
	decision.IncidentID = incidentID
	decision.Target = podEvidence.Target

	kubernetesClient := fake.NewSimpleClientset(
		&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podEvidence.Target.Name,
				Namespace: podEvidence.Target.Namespace,
				UID:       types.UID(podEvidence.Target.UID),
			},
		},
	)
	decider := &stubDecisionSource{
		decision: decision,
	}

	agent := &sreAgent{
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
		kubernetes: kubernetesClient,
		collector:  &stubContextCollector{evidence: podEvidence},
		ollama:     decider,
		incidents:  incident.NewMemoryRegistry(),
		approvals:  approval.NewMemoryStore(),
		memory:     newRemediationMemory(),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:        func() time.Time { return observedAt },
	}

	agent.handlePodCrashLooping(
		context.Background(),
		alert,
	)

	if decider.calls != 1 {
		t.Fatalf(
			"decision source calls = %d; want 1 before approval lookup",
			decider.calls,
		)
	}
	if actions := kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none without stored approval",
			actions,
		)
	}
}

func TestHandlePodCrashLoopingFailsClosedForInvalidOrUnavailableApproval(
	t *testing.T,
) {
	observedAt := time.Date(2026, 9, 29, 10, 30, 0, 0, time.UTC)

	tests := []struct {
		name      string
		mutate    func(*approval.Approval)
		lookupErr error
	}{
		{
			name: "expired approval",
			mutate: func(granted *approval.Approval) {
				granted.ExpiresAt = observedAt
			},
		},
		{
			name: "tampered plan hash",
			mutate: func(granted *approval.Approval) {
				granted.PlanHash = "sha256:tampered"
			},
		},
		{
			name: "tampered target UID",
			mutate: func(granted *approval.Approval) {
				granted.TargetUID = "different-pod-uid"
			},
		},
		{
			name:      "approval store unavailable",
			lookupErr: errors.New("approval store unavailable"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alert := Alert{
				Labels: map[string]string{
					"alertname": podCrashLoopingAlert,
					"namespace": "default",
					"pod":       "crash-app-approval-failure",
				},
				Annotations: map[string]string{
					"description": "CrashLoopBackOff",
				},
				State:    "firing",
				ActiveAt: observedAt,
			}
			podEvidence := PodEvidence{
				Target: DecisionTarget{
					Cluster:   "dev",
					Namespace: "default",
					Kind:      "Pod",
					Name:      "crash-app-approval-failure",
					UID:       "pod-uid-approval-failure",
				},
				Owner: OwnerEvidence{
					Kind: "ReplicaSet",
					Name: "crash-app-rs",
					UID:  "rs-uid-approval-failure",
				},
			}

			decision := policyTestDecision(ActionRestartPod)
			decision.IncidentID = incidentIDFor(
				alert,
				podEvidence.Target.UID,
			)
			decision.Target = podEvidence.Target

			ctx := context.Background()
			registry := incident.NewMemoryRegistry()
			lifecycleIncident, _, err := registry.Observe(
				ctx,
				incident.Observation{
					Source:    "prometheus",
					Cluster:   "dev",
					AlertName: alert.Labels["alertname"],
					Target: incident.Target{
						Kind:      podEvidence.Target.Kind,
						Namespace: podEvidence.Target.Namespace,
						Name:      podEvidence.Target.Name,
						UID:       podEvidence.Target.UID,
					},
				},
			)
			if err != nil {
				t.Fatalf("Observe() error = %v", err)
			}

			plan, err := approval.NewPlan(
				approval.PlanCommand{
					IncidentID: lifecycleIncident.ID,
					Action:     string(decision.Action),
					Target: approval.Target{
						Cluster:   podEvidence.Target.Cluster,
						Namespace: podEvidence.Target.Namespace,
						Kind:      podEvidence.Target.Kind,
						Name:      podEvidence.Target.Name,
						UID:       podEvidence.Target.UID,
					},
				},
			)
			if err != nil {
				t.Fatalf("NewPlan() error = %v", err)
			}

			granted := approval.Approval{
				IncidentID: plan.IncidentID,
				PlanHash:   plan.Hash,
				TargetUID:  plan.Target.UID,
				ApprovedBy: "operator-a",
				ApprovedAt: observedAt.Add(-time.Minute),
				ExpiresAt:  observedAt.Add(time.Hour),
			}
			if test.mutate != nil {
				test.mutate(&granted)
			}

			approvalStore := &staticApprovalStore{
				granted:   granted,
				lookupErr: test.lookupErr,
			}
			kubernetesClient := fake.NewSimpleClientset(
				&v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podEvidence.Target.Name,
						Namespace: podEvidence.Target.Namespace,
						UID:       types.UID(podEvidence.Target.UID),
					},
				},
			)
			var logOutput bytes.Buffer

			agent := &sreAgent{
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
				kubernetes: kubernetesClient,
				collector:  &stubContextCollector{evidence: podEvidence},
				ollama: &stubDecisionSource{
					decision: decision,
				},
				incidents: registry,
				approvals: approvalStore,
				memory:    newRemediationMemory(),
				logger: slog.New(
					slog.NewJSONHandler(&logOutput, nil),
				),
				now: func() time.Time { return observedAt },
			}

			agent.handlePodCrashLooping(ctx, alert)

			if approvalStore.lookupCalls != 1 {
				t.Fatalf(
					"approval Lookup calls = %d; want 1",
					approvalStore.lookupCalls,
				)
			}
			if actions := kubernetesClient.Actions(); len(actions) != 0 {
				t.Fatalf(
					"Kubernetes actions = %#v; want none",
					actions,
				)
			}
			if !strings.Contains(
				logOutput.String(),
				`"error_code":"APPROVAL_CHECK_FAILED"`,
			) {
				t.Fatalf(
					"missing approval failure log: %s",
					logOutput.String(),
				)
			}
		})
	}
}

func TestHandlePodCrashLoopingDoesNotActAfterClaimTakeover(
	t *testing.T,
) {
	observedAt := time.Date(2026, 9, 29, 11, 0, 0, 0, time.UTC)
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-takeover",
		},
		Annotations: map[string]string{
			"description": "CrashLoopBackOff",
		},
		State:    "firing",
		ActiveAt: observedAt,
	}
	podEvidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-takeover",
			UID:       "pod-uid-takeover",
		},
		Owner: OwnerEvidence{
			Kind: "ReplicaSet",
			Name: "crash-app-rs",
			UID:  "rs-uid-takeover",
		},
	}

	legacyIncidentID := incidentIDFor(
		alert,
		podEvidence.Target.UID,
	)
	decision := policyTestDecision(ActionRestartPod)
	decision.IncidentID = legacyIncidentID
	decision.Target = podEvidence.Target

	ctx := context.Background()
	registry := incident.NewMemoryRegistry()
	lifecycleIncident, _, err := registry.Observe(
		ctx,
		incident.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: alert.Labels["alertname"],
			Target: incident.Target{
				Kind:      podEvidence.Target.Kind,
				Namespace: podEvidence.Target.Namespace,
				Name:      podEvidence.Target.Name,
				UID:       podEvidence.Target.UID,
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}

	approvalStore := approval.NewMemoryStore()
	mustGrantApproval(
		t,
		approvalStore,
		lifecycleIncident.ID,
		string(decision.Action),
		podEvidence.Target,
		observedAt,
	)

	stateStore := &takeoverRemediationStateStore{
		delegate: newRemediationMemory(),
		registry: registry,
		command: incident.ClaimCommand{
			IncidentID:      lifecycleIncident.ID,
			ExpectedVersion: 3,
			HolderID:        "agent-b",
			Now:             observedAt.Add(31 * time.Second),
			LeaseDuration:   30 * time.Second,
		},
	}

	kubernetesClient := fake.NewSimpleClientset(
		&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podEvidence.Target.Name,
				Namespace: podEvidence.Target.Namespace,
				UID:       types.UID(podEvidence.Target.UID),
			},
		},
	)
	var logOutput bytes.Buffer

	agent := &sreAgent{
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
		kubernetes: kubernetesClient,
		collector:  &stubContextCollector{evidence: podEvidence},
		ollama: &stubDecisionSource{
			decision: decision,
		},
		incidents: registry,
		approvals: approvalStore,
		memory:    stateStore,
		logger: slog.New(
			slog.NewJSONHandler(&logOutput, nil),
		),
		now: func() time.Time { return observedAt },
	}

	agent.handlePodCrashLooping(ctx, alert)

	if stateStore.takeoverErr != nil {
		t.Fatalf(
			"takeover Claim() error = %v; want nil",
			stateStore.takeoverErr,
		)
	}
	if actions := kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none after late claim takeover",
			actions,
		)
	}
	if !strings.Contains(
		logOutput.String(),
		`"error_code":"INCIDENT_ACTION_FENCE_FAILED"`,
	) {
		t.Fatalf(
			"missing action fence failure log: %s",
			logOutput.String(),
		)
	}
}

func TestHandlePodCrashLoopingTreatsReactivatedAlertForSamePodAsDuplicate(
	t *testing.T,
) {
	observedAt := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-reactivated",
		},
		Annotations: map[string]string{
			"description": "CrashLoopBackOff",
		},
		State:    "firing",
		ActiveAt: observedAt,
	}
	podEvidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-reactivated",
			UID:       "pod-uid-reactivated",
		},
		Owner: OwnerEvidence{
			Kind: "ReplicaSet",
			Name: "crash-app-rs",
			UID:  "rs-uid-reactivated",
		},
	}

	decision := policyTestDecision(ActionRestartPod)
	decision.IncidentID = incidentIDFor(alert, podEvidence.Target.UID)
	decision.Target = podEvidence.Target

	kubernetesClient := fake.NewSimpleClientset()
	decider := &stubDecisionSource{decision: decision}
	var logOutput bytes.Buffer

	agent := &sreAgent{
		config: agentConfig{
			ClusterName:              "dev",
			AllowedNamespaces:        []string{"default"},
			AllowedControllerKinds:   []string{"ReplicaSet"},
			RestartPodApproved:       false,
			MinimumConfidence:        0.90,
			RestartCooldown:          10 * time.Minute,
			AttemptWindow:            time.Hour,
			MaximumAttempts:          1,
			OllamaTimeout:            time.Second,
			KubernetesRequestTimeout: time.Second,
		},
		kubernetes: kubernetesClient,
		collector:  &stubContextCollector{evidence: podEvidence},
		ollama:     decider,
		incidents:  incident.NewMemoryRegistry(),
		memory:     newRemediationMemory(),
		logger:     slog.New(slog.NewJSONHandler(&logOutput, nil)),
		now:        func() time.Time { return observedAt },
	}

	agent.handlePodCrashLooping(context.Background(), alert)

	reactivatedAlert := alert
	reactivatedAlert.ActiveAt = observedAt.Add(2 * time.Minute)
	agent.handlePodCrashLooping(context.Background(), reactivatedAlert)

	if decider.calls != 1 {
		t.Fatalf(
			"decision source calls = %d; want 1 for the same Pod UID after alert reactivation",
			decider.calls,
		)
	}
	if actions := kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf("Kubernetes actions = %#v; want none in Shadow mode", actions)
	}
	if !strings.Contains(
		logOutput.String(),
		`"error_code":"DUPLICATE_INCIDENT"`,
	) {
		t.Fatalf("missing duplicate incident log: %s", logOutput.String())
	}
}

func TestHandlePodCrashLoopingDoesNotActWhenRemediationStateUnavailable(t *testing.T) {
	observedAt := time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-state-unavailable",
		},
		Annotations: map[string]string{"description": "CrashLoopBackOff"},
		State:       "firing",
		ActiveAt:    observedAt,
	}
	podEvidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-state-unavailable",
			UID:       "pod-uid-state-unavailable",
		},
		Owner: OwnerEvidence{
			Kind: "ReplicaSet",
			Name: "crash-app-rs",
			UID:  "rs-uid-state-unavailable",
		},
	}

	kubernetesClient := fake.NewSimpleClientset()
	decider := &stubDecisionSource{}
	var logOutput bytes.Buffer
	agent := &sreAgent{
		config: agentConfig{
			ClusterName:              "dev",
			AllowedNamespaces:        []string{"default"},
			AllowedControllerKinds:   []string{"ReplicaSet"},
			RestartPodApproved:       true,
			MinimumConfidence:        0.90,
			RestartCooldown:          10 * time.Minute,
			AttemptWindow:            time.Hour,
			MaximumAttempts:          1,
			OllamaTimeout:            time.Second,
			KubernetesRequestTimeout: time.Second,
		},
		kubernetes: kubernetesClient,
		collector:  &stubContextCollector{evidence: podEvidence},
		ollama:     decider,
		incidents:  incident.NewMemoryRegistry(),
		memory: &failingRemediationStateStore{
			err: errors.New("state unavailable"),
		},
		logger: slog.New(slog.NewJSONHandler(&logOutput, nil)),
		now:    func() time.Time { return observedAt },
	}

	agent.handlePodCrashLooping(context.Background(), alert)

	if decider.received.IncidentID != "" {
		t.Fatalf(
			"decision source received incident %q; want no decision call",
			decider.received.IncidentID,
		)
	}
	if actions := kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf("Kubernetes actions = %#v; want none", actions)
	}
	if !strings.Contains(
		logOutput.String(),
		`"error_code":"REMEDIATION_STATE_UNAVAILABLE"`,
	) {
		t.Fatalf(
			"missing REMEDIATION_STATE_UNAVAILABLE log: %s",
			logOutput.String(),
		)
	}
}

type failingRemediationStateStore struct {
	err error
}

func (store *failingRemediationStateStore) Snapshot(
	context.Context,
	remediationStateQuery,
) (remediationSnapshot, error) {
	return remediationSnapshot{}, store.err
}

func (store *failingRemediationStateStore) Record(
	context.Context,
	remediationStateRecord,
) error {
	return store.err
}

func TestHandlePodCrashLoopingDoesNotActWhenRemediationStateRecordFails(
	t *testing.T,
) {
	observedAt := time.Date(2026, 9, 19, 20, 30, 0, 0, time.UTC)
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-record-failure",
		},
		Annotations: map[string]string{
			"description": "CrashLoopBackOff",
		},
		State:    "firing",
		ActiveAt: observedAt,
	}
	podEvidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-record-failure",
			UID:       "pod-uid-record-failure",
		},
		Owner: OwnerEvidence{
			Kind: "ReplicaSet",
			Name: "crash-app-rs",
			UID:  "rs-uid-record-failure",
		},
	}
	incidentID := incidentIDFor(alert, podEvidence.Target.UID)
	decision := policyTestDecision(ActionRestartPod)
	decision.IncidentID = incidentID
	decision.Target = podEvidence.Target
	incidentRegistry := incident.NewMemoryRegistry()

	persistentIncident, _, err := incidentRegistry.Observe(
		context.Background(),
		incident.Observation{
			Source:    "prometheus",
			Cluster:   podEvidence.Target.Cluster,
			AlertName: alert.Labels["alertname"],
			Target: incident.Target{
				Kind:      podEvidence.Target.Kind,
				Namespace: podEvidence.Target.Namespace,
				Name:      podEvidence.Target.Name,
				UID:       podEvidence.Target.UID,
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}

	plan, err := approval.NewPlan(
		approval.PlanCommand{
			IncidentID: persistentIncident.ID,
			Action:     string(decision.Action),
			Target: approval.Target{
				Cluster:   podEvidence.Target.Cluster,
				Namespace: podEvidence.Target.Namespace,
				Kind:      podEvidence.Target.Kind,
				Name:      podEvidence.Target.Name,
				UID:       podEvidence.Target.UID,
			},
		},
	)
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	approvalStore := approval.NewMemoryStore()
	err = approvalStore.Grant(
		context.Background(),
		approval.Approval{
			IncidentID: persistentIncident.ID,
			PlanHash:   plan.Hash,
			TargetUID:  podEvidence.Target.UID,
			ApprovedBy: "test-approver",
			ApprovedAt: observedAt.Add(-time.Minute),
			ExpiresAt:  observedAt.Add(time.Hour),
		},
	)
	if err != nil {
		t.Fatalf("Grant() error = %v", err)
	}
	kubernetesClient := fake.NewSimpleClientset(
		&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podEvidence.Target.Name,
				Namespace: podEvidence.Target.Namespace,
				UID:       types.UID(podEvidence.Target.UID),
			},
		},
	)
	decider := &stubDecisionSource{decision: decision}
	var logOutput bytes.Buffer
	agent := &sreAgent{
		config: agentConfig{
			ClusterName:                "dev",
			AllowedNamespaces:          []string{"default"},
			AllowedControllerKinds:     []string{"ReplicaSet"},
			RestartPodApproved:         true,
			MinimumConfidence:          0.90,
			RestartCooldown:            10 * time.Minute,
			AttemptWindow:              time.Hour,
			MaximumAttempts:            1,
			IncidentClaimHolderID:      "sre-agent-v2-record-failure-test",
			IncidentClaimLeaseDuration: 30 * time.Second,
			OllamaTimeout:              time.Second,
			KubernetesRequestTimeout:   time.Second,
		},
		kubernetes: kubernetesClient,
		incidents:  incidentRegistry,
		approvals:  approvalStore,
		collector: &stubContextCollector{
			evidence: podEvidence,
		},
		ollama: decider,
		memory: &recordFailingRemediationStateStore{
			err: errors.New("persist state failed"),
		},
		logger: slog.New(slog.NewJSONHandler(&logOutput, nil)),
		now:    func() time.Time { return observedAt },
	}

	agent.handlePodCrashLooping(context.Background(), alert)

	if decider.received.IncidentID != incidentID {
		t.Fatalf(
			"decision source received incident %q; want %q",
			decider.received.IncidentID,
			incidentID,
		)
	}
	if actions := kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none after state record failure",
			actions,
		)
	}
	if !strings.Contains(
		logOutput.String(),
		`"error_code":"REMEDIATION_STATE_RECORD_FAILED"`,
	) {
		t.Fatalf(
			"missing REMEDIATION_STATE_RECORD_FAILED log: %s",
			logOutput.String(),
		)
	}
}

type recordFailingRemediationStateStore struct {
	err         error
	lastRecord  remediationStateRecord
	sawDeadline bool
}

func (*recordFailingRemediationStateStore) Snapshot(
	context.Context,
	remediationStateQuery,
) (remediationSnapshot, error) {
	return remediationSnapshot{}, nil
}

func (store *recordFailingRemediationStateStore) Record(
	ctx context.Context,
	record remediationStateRecord,
) error {
	store.lastRecord = record
	_, store.sawDeadline = ctx.Deadline()
	return store.err
}

type stubContextCollector struct {
	evidence PodEvidence
	err      error
}

func (collector *stubContextCollector) collect(context.Context, string, string, string) (PodEvidence, error) {
	return collector.evidence, collector.err
}

type stubDecisionSource struct {
	decision Decision
	err      error
	received IncidentEvidence
	calls    int
}

func (source *stubDecisionSource) decide(_ context.Context, evidence IncidentEvidence) (Decision, error) {
	source.calls++
	source.received = evidence
	return source.decision, source.err
}

func TestRunCycleMarksReadinessAfterSuccessfulPrometheusQuery(t *testing.T) {
	observedAt := time.Date(
		2026,
		time.September,
		9,
		12,
		0,
		0,
		0,
		time.UTC,
	)

	readiness := newReadinessState(
		time.Minute,
		func() time.Time {
			return observedAt
		},
	)

	agent := &sreAgent{
		prometheus: &stubFiringAlertSource{},
		incidents:  incident.NewMemoryRegistry(),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		now: func() time.Time {
			return observedAt
		},
		markSuccessfulCycle: readiness.markSuccessfulCycle,
	}

	agent.runCycle(context.Background())

	if !readiness.isReady() {
		t.Fatal("readiness after successful Agent cycle = false; want true")
	}
}

type stubFiringAlertSource struct {
	alerts []Alert
	err    error
}

func (source *stubFiringAlertSource) firingAlerts(
	context.Context,
) ([]Alert, error) {
	return source.alerts, source.err
}

func TestNewSREAgentConnectsSuccessfulCycleToReadiness(t *testing.T) {
	observedAt := time.Date(
		2026,
		time.September,
		9,
		12,
		0,
		0,
		0,
		time.UTC,
	)

	readiness := newReadinessState(
		time.Minute,
		func() time.Time {
			return observedAt
		},
	)

	config := agentConfig{
		PrometheusURL:     "http://prometheus.test",
		OllamaURL:         "http://ollama.test",
		OllamaModel:       "test-model",
		PrometheusTimeout: time.Second,
		OllamaTimeout:     time.Second,
	}

	agent := newSREAgent(
		config,
		fake.NewSimpleClientset(),
		incident.NewMemoryRegistry(),
		approval.NewMemoryStore(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() {},
		readiness.markSuccessfulCycle,
		func(string) {},
	)

	agent.prometheus = &stubFiringAlertSource{}
	agent.now = func() time.Time {
		return observedAt
	}

	agent.runCycle(context.Background())

	if !readiness.isReady() {
		t.Fatal("readiness after constructed Agent cycle = false; want true")
	}
}

func TestRunCycleMarksLivenessProgressWhenPrometheusQueryFails(t *testing.T) {
	currentTime := time.Date(
		2026,
		time.September,
		13,
		19,
		0,
		0,
		0,
		time.UTC,
	)

	liveness := newLivenessState(
		time.Minute,
		func() time.Time {
			return currentTime
		},
	)

	currentTime = currentTime.Add(time.Minute + time.Nanosecond)

	if liveness.isLive() {
		t.Fatal("test setup liveness = true; want stale false")
	}

	readiness := newReadinessState(
		time.Minute,
		func() time.Time {
			return currentTime
		},
	)

	agent := &sreAgent{
		prometheus: &stubFiringAlertSource{
			err: errors.New("prometheus unavailable"),
		},
		incidents: incident.NewMemoryRegistry(),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),

		now: func() time.Time {
			return currentTime
		},
		markCycleProgress:   liveness.markProgress,
		markSuccessfulCycle: readiness.markSuccessfulCycle,
	}

	agent.runCycle(context.Background())

	if !liveness.isLive() {
		t.Fatal("liveness after failed cycle = false; want true")
	}

	if readiness.isReady() {
		t.Fatal("readiness after failed Prometheus query = true; want false")
	}
}

func TestNewSREAgentConnectsCycleProgressToLiveness(t *testing.T) {
	currentTime := time.Date(
		2026,
		time.September,
		13,
		20,
		0,
		0,
		0,
		time.UTC,
	)

	liveness := newLivenessState(
		time.Minute,
		func() time.Time {
			return currentTime
		},
	)

	currentTime = currentTime.Add(time.Minute + time.Nanosecond)

	if liveness.isLive() {
		t.Fatal("test setup liveness = true; want stale false")
	}

	config := agentConfig{
		PrometheusURL:     "http://prometheus.test",
		OllamaURL:         "http://ollama.test",
		OllamaModel:       "test-model",
		PrometheusTimeout: time.Second,
		OllamaTimeout:     time.Second,
	}

	agent := newSREAgent(
		config,
		fake.NewSimpleClientset(),
		incident.NewMemoryRegistry(),
		approval.NewMemoryStore(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		liveness.markProgress,
		func() {},
		func(string) {},
	)

	agent.prometheus = &stubFiringAlertSource{
		err: errors.New("prometheus unavailable"),
	}
	agent.now = func() time.Time {
		return currentTime
	}

	agent.runCycle(context.Background())

	if !liveness.isLive() {
		t.Fatal("constructed Agent did not refresh liveness")
	}
}

type progressObservingCollector struct {
	calls            int
	advance          func()
	isLive           func() bool
	liveAtSecondCall bool
}

func (collector *progressObservingCollector) collect(
	_ context.Context,
	_, _, _ string,
) (PodEvidence, error) {
	collector.calls++

	if collector.calls == 1 {
		collector.advance()
	}

	if collector.calls == 2 {
		collector.liveAtSecondCall = collector.isLive()
	}

	return PodEvidence{}, errors.New(
		"stop context collection for liveness test",
	)
}

func TestRunCycleRefreshesLivenessBetweenActionableAlerts(t *testing.T) {
	currentTime := time.Date(
		2026,
		time.September,
		13,
		23,
		0,
		0,
		0,
		time.UTC,
	)

	liveness := newLivenessState(
		time.Minute,
		func() time.Time {
			return currentTime
		},
	)

	collector := &progressObservingCollector{
		advance: func() {
			currentTime = currentTime.Add(
				time.Minute + time.Nanosecond,
			)
		},
		isLive: liveness.isLive,
	}

	agent := &sreAgent{
		config: agentConfig{
			ClusterName:              "dev",
			KubernetesRequestTimeout: time.Second,
		},
		collector: collector,
		incidents: incident.NewMemoryRegistry(),
		prometheus: &stubFiringAlertSource{
			alerts: []Alert{
				{
					Labels: map[string]string{
						"alertname": podCrashLoopingAlert,
						"namespace": "default",
						"pod":       "crash-app-a",
					},
				},
				{
					Labels: map[string]string{
						"alertname": podCrashLoopingAlert,
						"namespace": "default",
						"pod":       "crash-app-b",
					},
				},
			},
		},
		logger: slog.New(
			slog.NewTextHandler(io.Discard, nil),
		),
		now: func() time.Time {
			return currentTime
		},
		markCycleProgress: liveness.markProgress,
	}

	agent.runCycle(context.Background())

	if collector.calls != 2 {
		t.Fatalf(
			"collector calls = %d; want 2",
			collector.calls,
		)
	}

	if !collector.liveAtSecondCall {
		t.Fatal(
			"liveness before second alert = false; want true",
		)
	}
}

type countingAlertSourceForCanceledRun struct {
	calls int
}

func (source *countingAlertSourceForCanceledRun) firingAlerts(
	context.Context,
) ([]Alert, error) {
	source.calls++
	return nil, nil
}

func TestSREAgentRunDoesNotStartCycleWhenContextAlreadyCanceled(
	t *testing.T,
) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	alertSource := &countingAlertSourceForCanceledRun{}

	agent := &sreAgent{
		config: agentConfig{
			PollInterval: time.Hour,
		},
		prometheus: alertSource,
		incidents:  incident.NewMemoryRegistry(),
		logger: slog.New(
			slog.NewTextHandler(io.Discard, nil),
		),
		now: time.Now,
	}

	agent.run(ctx)

	if alertSource.calls != 0 {
		t.Fatalf(
			"Prometheus calls after cancellation = %d; want 0",
			alertSource.calls,
		)
	}
}

func TestRunCycleDoesNotQueryPrometheusWhenContextCanceled(
	t *testing.T,
) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	alertSource := &countingAlertSourceForCanceledRun{}

	agent := &sreAgent{
		prometheus: alertSource,
		incidents:  incident.NewMemoryRegistry(),
		logger: slog.New(
			slog.NewTextHandler(io.Discard, nil),
		),
		now: time.Now,
	}

	agent.runCycle(ctx)

	if alertSource.calls != 0 {
		t.Fatalf(
			"Prometheus calls after cancellation = %d; want 0",
			alertSource.calls,
		)
	}
}

type cancelingAlertSourceForShutdown struct {
	cancel context.CancelFunc
}

func (source *cancelingAlertSourceForShutdown) firingAlerts(
	ctx context.Context,
) ([]Alert, error) {
	source.cancel()
	return nil, ctx.Err()
}

func TestRunCycleDoesNotLogFailureOrRecordErrorWhenPrometheusCallIsCanceledByShutdown(
	t *testing.T,
) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var logOutput bytes.Buffer
	var recordedResults []string

	agent := &sreAgent{
		prometheus: &cancelingAlertSourceForShutdown{
			cancel: cancel,
		},
		incidents: incident.NewMemoryRegistry(),
		logger: slog.New(
			slog.NewJSONHandler(&logOutput, nil),
		),
		now: time.Now,
		recordCycleResult: func(result string) {
			recordedResults = append(
				recordedResults,
				result,
			)
		},
	}

	agent.runCycle(ctx)

	if strings.Contains(
		logOutput.String(),
		`"msg":"cycle_failed"`,
	) {
		t.Fatalf(
			"runCycle logged cycle_failed for normal shutdown: %s",
			logOutput.String(),
		)
	}

	if len(recordedResults) != 0 {
		t.Fatalf(
			"recorded cycle results during shutdown = %v; want none",
			recordedResults,
		)
	}
}

type failingAlertSourceForLogging struct{}

func (*failingAlertSourceForLogging) firingAlerts(
	context.Context,
) ([]Alert, error) {
	return nil, context.DeadlineExceeded
}

func TestRunCycleLogsPrometheusFailureWhenApplicationContextIsActive(
	t *testing.T,
) {
	var logOutput bytes.Buffer

	agent := &sreAgent{
		prometheus: &failingAlertSourceForLogging{},
		incidents:  incident.NewMemoryRegistry(),
		logger: slog.New(
			slog.NewJSONHandler(&logOutput, nil),
		),
		now: time.Now,
	}

	agent.runCycle(context.Background())

	output := logOutput.String()

	for _, expected := range []string{
		`"msg":"cycle_failed"`,
		`"error_code":"PROMETHEUS_QUERY_FAILED"`,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf(
				"runCycle log = %s; want %s",
				output,
				expected,
			)
		}
	}
}

type notifyingAlertSourceForRunLoop struct {
	calls chan struct{}
}

func (source *notifyingAlertSourceForRunLoop) firingAlerts(
	context.Context,
) ([]Alert, error) {
	source.calls <- struct{}{}
	return nil, nil
}

func TestSREAgentRunContinuesProcessingUntilContextCanceled(
	t *testing.T,
) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	alertSource := &notifyingAlertSourceForRunLoop{
		calls: make(chan struct{}, 2),
	}

	agent := &sreAgent{
		config: agentConfig{
			PollInterval: time.Millisecond,
		},
		prometheus: alertSource,
		incidents:  incident.NewMemoryRegistry(),
		logger: slog.New(
			slog.NewTextHandler(io.Discard, nil),
		),
		now: time.Now,
	}

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		agent.run(ctx)
	}()

	select {
	case <-alertSource.calls:
	case <-time.After(time.Second):
		t.Fatal("first cycle did not run")
	}

	select {
	case <-alertSource.calls:
	case <-runDone:
		t.Fatal("agent.run returned before the second cycle")
	case <-time.After(time.Second):
		t.Fatal("second cycle did not run")
	}

	cancel()

	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("agent.run did not stop after cancellation")
	}
}

func TestRunCycleRecordsNoActionResult(t *testing.T) {
	observedAt := time.Date(
		2026,
		time.September,
		15,
		21,
		0,
		0,
		0,
		time.UTC,
	)

	var recordedResults []string

	agent := &sreAgent{
		prometheus: &stubFiringAlertSource{},
		incidents:  incident.NewMemoryRegistry(),
		logger: slog.New(
			slog.NewTextHandler(io.Discard, nil),
		),
		now: func() time.Time {
			return observedAt
		},
		recordCycleResult: func(result string) {
			recordedResults = append(
				recordedResults,
				result,
			)
		},
	}

	agent.runCycle(context.Background())

	if len(recordedResults) != 1 {
		t.Fatalf(
			"recorded cycle results = %v; want exactly one result",
			recordedResults,
		)
	}

	if recordedResults[0] != "NO_ACTION" {
		t.Fatalf(
			"recorded cycle result = %q; want NO_ACTION",
			recordedResults[0],
		)
	}
}

func TestRunCycleRecordsErrorWhenPrometheusQueryFails(t *testing.T) {
	observedAt := time.Date(
		2026,
		time.September,
		15,
		22,
		0,
		0,
		0,
		time.UTC,
	)

	var recordedResults []string

	agent := &sreAgent{
		prometheus: &stubFiringAlertSource{
			err: errors.New("prometheus unavailable"),
		},
		logger: slog.New(
			slog.NewTextHandler(io.Discard, nil),
		),
		now: func() time.Time {
			return observedAt
		},
		recordCycleResult: func(result string) {
			recordedResults = append(
				recordedResults,
				result,
			)
		},
	}

	agent.runCycle(context.Background())

	if len(recordedResults) != 1 {
		t.Fatalf(
			"recorded cycle results = %v; want exactly one result",
			recordedResults,
		)
	}

	if recordedResults[0] != "ERROR" {
		t.Fatalf(
			"recorded cycle result = %q; want ERROR",
			recordedResults[0],
		)
	}
}

func TestNewSREAgentConnectsCycleResultRecorder(t *testing.T) {
	observedAt := time.Date(
		2026,
		time.September,
		15,
		23,
		0,
		0,
		0,
		time.UTC,
	)

	config := agentConfig{
		PrometheusURL:     "http://prometheus.test",
		OllamaURL:         "http://ollama.test",
		OllamaModel:       "test-model",
		PrometheusTimeout: time.Second,
		OllamaTimeout:     time.Second,
	}

	var recordedResults []string

	agent := newSREAgent(
		config,
		fake.NewSimpleClientset(),
		incident.NewMemoryRegistry(),
		approval.NewMemoryStore(),
		slog.New(
			slog.NewTextHandler(io.Discard, nil),
		),
		func() {},
		func() {},
		func(result string) {
			recordedResults = append(
				recordedResults,
				result,
			)
		},
	)

	agent.prometheus = &stubFiringAlertSource{}
	agent.now = func() time.Time {
		return observedAt
	}

	agent.runCycle(context.Background())

	if len(recordedResults) != 1 {
		t.Fatalf(
			"recorded cycle results = %v; want exactly one result",
			recordedResults,
		)
	}

	if recordedResults[0] != "NO_ACTION" {
		t.Fatalf(
			"recorded cycle result = %q; want NO_ACTION",
			recordedResults[0],
		)
	}
}
func TestNewSREAgentUsesConfiguredConfigMapRemediationState(t *testing.T) {
	config := agentConfig{
		RemediationStateNamespace: "custom-agent-system",
		RemediationStateConfigMap: "custom-remediation-state",
	}
	kubernetesClient := fake.NewSimpleClientset()

	agent := newSREAgent(
		config,
		kubernetesClient,
		incident.NewMemoryRegistry(),
		approval.NewMemoryStore(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
		nil,
		nil,
	)

	stateStore, ok := agent.memory.(*configMapRemediationState)
	if !ok {
		t.Fatalf(
			"newSREAgent() memory type = %T; want *configMapRemediationState",
			agent.memory,
		)
	}
	if stateStore.namespace != config.RemediationStateNamespace {
		t.Fatalf(
			"state namespace = %q; want %q",
			stateStore.namespace,
			config.RemediationStateNamespace,
		)
	}
	if stateStore.configName != config.RemediationStateConfigMap {
		t.Fatalf(
			"state ConfigMap = %q; want %q",
			stateStore.configName,
			config.RemediationStateConfigMap,
		)
	}
}

func TestHandlePodCrashLoopingReportsDeniedIncidentStateRecordFailure(
	t *testing.T,
) {
	observedAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-denied-record-failure",
		},
		Annotations: map[string]string{
			"description": "CrashLoopBackOff",
		},
		State:    "firing",
		ActiveAt: observedAt,
	}
	podEvidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-denied-record-failure",
			UID:       "pod-uid-denied-record-failure",
		},
		Owner: OwnerEvidence{
			Kind: "ReplicaSet",
			Name: "crash-app-rs",
			UID:  "rs-uid-denied-record-failure",
		},
	}
	incidentID := incidentIDFor(alert, podEvidence.Target.UID)
	decision := policyTestDecision(ActionRestartPod)
	decision.IncidentID = incidentID
	decision.Target = podEvidence.Target

	kubernetesClient := fake.NewSimpleClientset()
	decider := &stubDecisionSource{decision: decision}
	stateStore := &recordFailingRemediationStateStore{
		err: errors.New("persist denied incident failed"),
	}
	var logOutput bytes.Buffer

	agent := &sreAgent{
		config: agentConfig{
			ClusterName:              "dev",
			AllowedNamespaces:        []string{"default"},
			AllowedControllerKinds:   []string{"ReplicaSet"},
			RestartPodApproved:       false,
			MinimumConfidence:        0.90,
			RestartCooldown:          10 * time.Minute,
			AttemptWindow:            time.Hour,
			MaximumAttempts:          1,
			OllamaTimeout:            time.Second,
			KubernetesRequestTimeout: time.Second,
		},
		kubernetes: kubernetesClient,
		incidents:  incident.NewMemoryRegistry(),
		collector: &stubContextCollector{
			evidence: podEvidence,
		},
		ollama: decider,
		memory: stateStore,
		logger: slog.New(
			slog.NewJSONHandler(&logOutput, nil),
		),
		now: func() time.Time { return observedAt },
	}

	agent.handlePodCrashLooping(context.Background(), alert)

	if decider.received.IncidentID != incidentID {
		t.Fatalf(
			"decision source received incident %q; want %q",
			decider.received.IncidentID,
			incidentID,
		)
	}
	if stateStore.lastRecord.Kind != remediationStateRecordIncidentHandled {
		t.Fatalf(
			"record kind = %q; want %q",
			stateStore.lastRecord.Kind,
			remediationStateRecordIncidentHandled,
		)
	}
	if !stateStore.sawDeadline {
		t.Fatal(
			"denied incident state record received no deadline",
		)
	}
	if actions := kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none",
			actions,
		)
	}
	if !strings.Contains(
		logOutput.String(),
		`"error_code":"REMEDIATION_STATE_RECORD_FAILED"`,
	) {
		t.Fatalf(
			"missing REMEDIATION_STATE_RECORD_FAILED log: %s",
			logOutput.String(),
		)
	}
}
func TestHandlePodCrashLoopingObservesLifecycleIncidentBeforeRemediationState(
	t *testing.T,
) {
	observedAt := time.Date(
		2026,
		time.September,
		26,
		16,
		30,
		0,
		0,
		time.UTC,
	)
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-lifecycle",
		},
		Annotations: map[string]string{
			"description": "CrashLoopBackOff",
		},
		State:    "firing",
		ActiveAt: observedAt,
	}
	podEvidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-lifecycle",
			UID:       "pod-uid-lifecycle",
		},
		Owner: OwnerEvidence{
			Kind: "ReplicaSet",
			Name: "crash-app-rs",
			UID:  "rs-uid-lifecycle",
		},
	}

	lifecycleRegistry := incident.NewMemoryRegistry()
	agent := &sreAgent{
		config: agentConfig{
			ClusterName:              "dev",
			KubernetesRequestTimeout: time.Second,
		},
		collector: &stubContextCollector{
			evidence: podEvidence,
		},
		incidents: lifecycleRegistry,
		memory: &failingRemediationStateStore{
			err: errors.New("remediation state unavailable"),
		},
		logger: slog.New(
			slog.NewTextHandler(io.Discard, nil),
		),
		now: func() time.Time {
			return observedAt
		},
	}

	agent.handlePodCrashLooping(
		context.Background(),
		alert,
	)

	observed, created, err := lifecycleRegistry.Observe(
		context.Background(),
		incident.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: podCrashLoopingAlert,
			Target: incident.Target{
				Kind:      "Pod",
				Namespace: "default",
				Name:      "crash-app-lifecycle",
				UID:       "pod-uid-lifecycle",
			},
		},
	)
	if err != nil {
		t.Fatalf(
			"lifecycle Registry.Observe() error = %v; want nil",
			err,
		)
	}
	if created {
		t.Fatal(
			"lifecycle Registry.Observe() created = true; " +
				"want false because the Agent should have observed it",
		)
	}
	if observed.State != incident.StateDetected {
		t.Fatalf(
			"lifecycle incident state = %q; want %q",
			observed.State,
			incident.StateDetected,
		)
	}
}
func TestNewSREAgentConnectsProvidedIncidentRegistry(t *testing.T) {
	observedAt := time.Date(
		2026,
		time.September,
		26,
		17,
		0,
		0,
		0,
		time.UTC,
	)
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-constructor",
		},
		Annotations: map[string]string{
			"description": "CrashLoopBackOff",
		},
		State:    "firing",
		ActiveAt: observedAt,
	}
	podEvidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-constructor",
			UID:       "pod-uid-constructor",
		},
	}

	lifecycleRegistry := incident.NewMemoryRegistry()
	agent := newSREAgent(
		agentConfig{
			ClusterName:              "dev",
			PrometheusURL:            "http://prometheus.test",
			OllamaURL:                "http://ollama.test",
			OllamaModel:              "test-model",
			PrometheusTimeout:        time.Second,
			OllamaTimeout:            time.Second,
			KubernetesRequestTimeout: time.Second,
		},
		fake.NewSimpleClientset(),
		lifecycleRegistry,
		approval.NewMemoryStore(),
		slog.New(
			slog.NewTextHandler(io.Discard, nil),
		),
		nil,
		nil,
		nil,
	)

	agent.collector = &stubContextCollector{
		evidence: podEvidence,
	}
	agent.memory = &failingRemediationStateStore{
		err: errors.New("remediation state unavailable"),
	}
	agent.now = func() time.Time {
		return observedAt
	}

	agent.handlePodCrashLooping(
		context.Background(),
		alert,
	)

	_, created, err := lifecycleRegistry.Observe(
		context.Background(),
		incident.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: podCrashLoopingAlert,
			Target: incident.Target{
				Kind:      "Pod",
				Namespace: "default",
				Name:      "crash-app-constructor",
				UID:       "pod-uid-constructor",
			},
		},
	)
	if err != nil {
		t.Fatalf(
			"lifecycle Registry.Observe() error = %v; want nil",
			err,
		)
	}
	if created {
		t.Fatal(
			"lifecycle Registry.Observe() created = true; " +
				"want false because newSREAgent should connect " +
				"the provided Registry",
		)
	}
}
func TestHandlePodCrashLoopingDoesNotActWhenIncidentStoreUnavailable(
	t *testing.T,
) {
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-registry-unavailable",
		},
		Annotations: map[string]string{
			"description": "CrashLoopBackOff",
		},
		State: "firing",
	}

	podEvidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-registry-unavailable",
			UID:       "pod-uid-registry-unavailable",
		},
	}
	expectedIncidentID := incidentIDFor(
		alert,
		podEvidence.Target.UID,
	)
	kubernetesClient := fake.NewSimpleClientset()
	decider := &stubDecisionSource{}
	stateStore := &trackingRemediationStateStore{}
	var logOutput bytes.Buffer

	agent := &sreAgent{
		config: agentConfig{
			ClusterName:              "dev",
			KubernetesRequestTimeout: time.Second,
		},
		kubernetes: kubernetesClient,
		incidents: &failingIncidentRegistry{
			err: errors.New("incident registry unavailable"),
		},
		collector: &stubContextCollector{
			evidence: podEvidence,
		},
		ollama: decider,
		memory: stateStore,
		logger: slog.New(
			slog.NewJSONHandler(&logOutput, nil),
		),
		now: func() time.Time {
			return time.Date(
				2026,
				time.September,
				26,
				17,
				30,
				0,
				0,
				time.UTC,
			)
		},
	}

	agent.handlePodCrashLooping(
		context.Background(),
		alert,
	)

	if stateStore.snapshotCalls != 0 {
		t.Fatalf(
			"remediation state Snapshot calls = %d; want 0",
			stateStore.snapshotCalls,
		)
	}
	if decider.calls != 0 {
		t.Fatalf(
			"decision source calls = %d; want 0",
			decider.calls,
		)
	}
	if actions := kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none",
			actions,
		)
	}
	for _, expected := range []string{
		`"msg":"incident_store_unavailable"`,
		`"incident_id":"` + expectedIncidentID + `"`,
		`"action":"OBSERVE_INCIDENT"`,
		`"result":"NO_ACTION"`,
		`"duration_ms":`,
		`"error_code":"INCIDENT_STORE_UNAVAILABLE"`,
	} {
		if !strings.Contains(logOutput.String(), expected) {
			t.Fatalf(
				"incident store failure log %q does not contain %q",
				logOutput.String(),
				expected,
			)
		}
	}
}

func TestHandlePodCrashLoopingUsesClaimVersionToFenceDiagnosisTransition(
	t *testing.T,
) {
	observedAt := time.Date(
		2026,
		time.September,
		28,
		17,
		30,
		0,
		0,
		time.UTC,
	)
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-diagnosis-fence",
		},
		Annotations: map[string]string{
			"description": "CrashLoopBackOff",
		},
		State:    "firing",
		ActiveAt: observedAt,
	}
	podEvidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-diagnosis-fence",
			UID:       "pod-uid-diagnosis-fence",
		},
		Owner: OwnerEvidence{
			Kind: "ReplicaSet",
			Name: "crash-app-rs",
			UID:  "rs-uid-diagnosis-fence",
		},
	}

	const holderID = "sre-agent-v2-pod-a"
	leaseDuration := 30 * time.Second

	observedIncident := incident.Incident{
		ID:      "inc-agent-diagnosis-fence",
		State:   incident.StateDetected,
		Version: 1,
	}
	claimedIncident := incident.Incident{
		ID:      observedIncident.ID,
		State:   incident.StateDetected,
		Version: 2,
	}
	registry := &claimRejectingIncidentRegistry{
		observed: observedIncident,
		claim: incident.Claim{
			Incident:  claimedIncident,
			HolderID:  holderID,
			ExpiresAt: observedAt.Add(leaseDuration),
		},
		transitionErr: incident.ErrVersionConflict,
	}

	decision := policyTestDecision(ActionRestartPod)
	decision.IncidentID = incidentIDFor(
		alert,
		podEvidence.Target.UID,
	)
	decision.Target = podEvidence.Target

	decider := &stubDecisionSource{
		decision: decision,
	}
	stateStore := &trackingRemediationStateStore{}
	kubernetesClient := fake.NewSimpleClientset()

	agent := &sreAgent{
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
			IncidentClaimHolderID:      holderID,
			IncidentClaimLeaseDuration: leaseDuration,
		},
		kubernetes: kubernetesClient,
		incidents:  registry,
		collector: &stubContextCollector{
			evidence: podEvidence,
		},
		ollama: decider,
		memory: stateStore,
		logger: slog.New(
			slog.NewTextHandler(io.Discard, nil),
		),
		now: func() time.Time {
			return observedAt
		},
	}

	agent.handlePodCrashLooping(
		context.Background(),
		alert,
	)

	if decider.calls != 1 {
		t.Fatalf(
			"decision source calls = %d; want 1",
			decider.calls,
		)
	}
	if registry.transitionCalls != 1 {
		t.Fatalf(
			"incident Transition calls = %d; want 1",
			registry.transitionCalls,
		)
	}

	command := registry.transitionCommand
	if command.IncidentID != claimedIncident.ID {
		t.Fatalf(
			"Transition IncidentID = %q; want %q",
			command.IncidentID,
			claimedIncident.ID,
		)
	}
	if command.ExpectedVersion != claimedIncident.Version {
		t.Fatalf(
			"Transition ExpectedVersion = %d; want %d",
			command.ExpectedVersion,
			claimedIncident.Version,
		)
	}
	if command.To != incident.StateDiagnosed {
		t.Fatalf(
			"Transition To = %q; want %q",
			command.To,
			incident.StateDiagnosed,
		)
	}
	if command.Actor != holderID {
		t.Fatalf(
			"Transition Actor = %q; want %q",
			command.Actor,
			holderID,
		)
	}
	if command.ReasonCode != "DIAGNOSIS_COMPLETED" {
		t.Fatalf(
			"Transition ReasonCode = %q; want %q",
			command.ReasonCode,
			"DIAGNOSIS_COMPLETED",
		)
	}
	if actions := kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none after fencing conflict",
			actions,
		)
	}
}

func TestHandlePodCrashLoopingDoesNotContinueWhenIncidentClaimIsHeld(
	t *testing.T,
) {
	observedAt := time.Date(
		2026,
		time.September,
		28,
		17,
		0,
		0,
		0,
		time.UTC,
	)
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-claim-held",
		},
		Annotations: map[string]string{
			"description": "CrashLoopBackOff",
		},
		State:    "firing",
		ActiveAt: observedAt,
	}
	podEvidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-claim-held",
			UID:       "pod-uid-claim-held",
		},
	}

	const holderID = "sre-agent-v2-pod-a"
	leaseDuration := 30 * time.Second

	registry := &claimRejectingIncidentRegistry{
		observed: incident.Incident{
			ID:      "inc-agent-claim-held",
			State:   incident.StateDetected,
			Version: 1,
		},
		err: incident.ErrLeaseHeld,
	}
	stateStore := &trackingRemediationStateStore{}
	decider := &stubDecisionSource{}
	kubernetesClient := fake.NewSimpleClientset()

	agent := &sreAgent{
		config: agentConfig{
			ClusterName:                "dev",
			IncidentClaimHolderID:      holderID,
			IncidentClaimLeaseDuration: leaseDuration,
			KubernetesRequestTimeout:   time.Second,
		},
		kubernetes: kubernetesClient,
		incidents:  registry,
		collector: &stubContextCollector{
			evidence: podEvidence,
		},
		ollama: decider,
		memory: stateStore,
		logger: slog.New(
			slog.NewTextHandler(io.Discard, nil),
		),
		now: func() time.Time {
			return observedAt
		},
	}

	agent.handlePodCrashLooping(
		context.Background(),
		alert,
	)

	if registry.claimCalls != 1 {
		t.Fatalf(
			"incident Claim calls = %d; want 1",
			registry.claimCalls,
		)
	}
	if registry.claimCommand.IncidentID != registry.observed.ID {
		t.Fatalf(
			"Claim IncidentID = %q; want %q",
			registry.claimCommand.IncidentID,
			registry.observed.ID,
		)
	}
	if registry.claimCommand.ExpectedVersion != registry.observed.Version {
		t.Fatalf(
			"Claim ExpectedVersion = %d; want %d",
			registry.claimCommand.ExpectedVersion,
			registry.observed.Version,
		)
	}
	if registry.claimCommand.HolderID != holderID {
		t.Fatalf(
			"Claim HolderID = %q; want %q",
			registry.claimCommand.HolderID,
			holderID,
		)
	}
	if !registry.claimCommand.Now.Equal(observedAt) {
		t.Fatalf(
			"Claim Now = %s; want %s",
			registry.claimCommand.Now,
			observedAt,
		)
	}
	if registry.claimCommand.LeaseDuration != leaseDuration {
		t.Fatalf(
			"Claim LeaseDuration = %s; want %s",
			registry.claimCommand.LeaseDuration,
			leaseDuration,
		)
	}
	if stateStore.snapshotCalls != 0 {
		t.Fatalf(
			"remediation state Snapshot calls = %d; want 0",
			stateStore.snapshotCalls,
		)
	}
	if decider.calls != 0 {
		t.Fatalf(
			"decision source calls = %d; want 0",
			decider.calls,
		)
	}
	if actions := kubernetesClient.Actions(); len(actions) != 0 {
		t.Fatalf(
			"Kubernetes actions = %#v; want none",
			actions,
		)
	}
}

type claimRejectingIncidentRegistry struct {
	observed          incident.Incident
	claim             incident.Claim
	err               error
	transitionErr     error
	claimCalls        int
	claimCommand      incident.ClaimCommand
	transitionCalls   int
	transitionCommand incident.TransitionCommand
}

func (registry *claimRejectingIncidentRegistry) Observe(
	context.Context,
	incident.Observation,
) (incident.Incident, bool, error) {
	return registry.observed, true, nil
}

func (registry *claimRejectingIncidentRegistry) Claim(
	_ context.Context,
	command incident.ClaimCommand,
) (incident.Claim, error) {
	registry.claimCalls++
	registry.claimCommand = command

	return registry.claim, registry.err
}

func (registry *claimRejectingIncidentRegistry) Transition(
	_ context.Context,
	command incident.TransitionCommand,
) (incident.Incident, error) {
	registry.transitionCalls++
	registry.transitionCommand = command

	return incident.Incident{}, registry.transitionErr
}

type failingIncidentRegistry struct {
	err error
}

func (registry *failingIncidentRegistry) Claim(
	context.Context,
	incident.ClaimCommand,
) (incident.Claim, error) {
	return incident.Claim{}, registry.err
}

func (registry *failingIncidentRegistry) Observe(
	context.Context,
	incident.Observation,
) (incident.Incident, bool, error) {
	return incident.Incident{}, false, registry.err
}

func (registry *failingIncidentRegistry) Transition(
	context.Context,
	incident.TransitionCommand,
) (incident.Incident, error) {
	return incident.Incident{}, registry.err
}

type trackingRemediationStateStore struct {
	snapshotCalls int
}

func (store *trackingRemediationStateStore) Snapshot(
	context.Context,
	remediationStateQuery,
) (remediationSnapshot, error) {
	store.snapshotCalls++
	return remediationSnapshot{}, nil
}

func (*trackingRemediationStateStore) Record(
	context.Context,
	remediationStateRecord,
) error {
	return nil
}
func mustGrantApproval(
	t *testing.T,
	store approval.Store,
	incidentID string,
	action string,
	target DecisionTarget,
	now time.Time,
) {
	t.Helper()

	plan, err := approval.NewPlan(
		approval.PlanCommand{
			IncidentID: incidentID,
			Action:     action,
			Target: approval.Target{
				Cluster:   target.Cluster,
				Namespace: target.Namespace,
				Kind:      target.Kind,
				Name:      target.Name,
				UID:       target.UID,
			},
		},
	)
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	err = store.Grant(
		context.Background(),
		approval.Approval{
			IncidentID: plan.IncidentID,
			PlanHash:   plan.Hash,
			TargetUID:  plan.Target.UID,
			ApprovedBy: "operator-a",
			ApprovedAt: now.Add(-time.Minute),
			ExpiresAt:  now.Add(time.Hour),
		},
	)
	if err != nil {
		t.Fatalf("Grant() error = %v", err)
	}
}

type takeoverRemediationStateStore struct {
	delegate    remediationStateStore
	registry    *incident.Registry
	command     incident.ClaimCommand
	takeoverErr error
}

func (store *takeoverRemediationStateStore) Snapshot(
	ctx context.Context,
	query remediationStateQuery,
) (remediationSnapshot, error) {
	return store.delegate.Snapshot(ctx, query)
}

func (store *takeoverRemediationStateStore) Record(
	ctx context.Context,
	record remediationStateRecord,
) error {
	if err := store.delegate.Record(ctx, record); err != nil {
		return err
	}
	if record.Kind != remediationStateRecordActionAttempted {
		return nil
	}

	_, store.takeoverErr = store.registry.Claim(
		ctx,
		store.command,
	)
	return store.takeoverErr
}

type staticApprovalStore struct {
	granted     approval.Approval
	lookupErr   error
	lookupCalls int
}

func (*staticApprovalStore) Grant(
	context.Context,
	approval.Approval,
) error {
	return errors.New("Grant is not supported by this test store")
}

func (store *staticApprovalStore) Lookup(
	ctx context.Context,
	approvalKey approval.ApprovalKey,
) (approval.Approval, error) {
	if err := ctx.Err(); err != nil {
		return approval.Approval{}, err
	}

	store.lookupCalls++

	if store.lookupErr != nil {
		return approval.Approval{}, store.lookupErr
	}

	return store.granted, nil
}
