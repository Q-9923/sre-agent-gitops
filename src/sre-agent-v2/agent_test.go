package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestHandlePodCrashLoopingConnectsEvidenceDecisionPolicyAndUIDDelete(t *testing.T) {
	observedAt := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	alert := Alert{
		Labels: map[string]string{
			"alertname": podCrashLoopingAlert,
			"namespace": "default",
			"pod":       "crash-app-a",
		},
		Annotations: map[string]string{"description": "CrashLoopBackOff"},
		State:       "firing",
		ActiveAt:    observedAt,
	}
	podEvidence := PodEvidence{
		Target: DecisionTarget{
			Cluster: "dev", Namespace: "default", Kind: "Pod", Name: "crash-app-a", UID: "pod-uid-a",
		},
		Owner: OwnerEvidence{Kind: "ReplicaSet", Name: "crash-app-rs", UID: "rs-uid"},
		Containers: []ContainerEvidence{
			{Name: "main", State: "waiting", Reason: "CrashLoopBackOff", RestartCount: 4},
		},
	}
	incidentID := incidentIDFor(alert, podEvidence.Target.UID)
	decision := policyTestDecision(ActionRestartPod)
	decision.IncidentID = incidentID
	decision.Target = podEvidence.Target

	kubernetesClient := fake.NewSimpleClientset(&v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: podEvidence.Target.Name, Namespace: podEvidence.Target.Namespace, UID: types.UID(podEvidence.Target.UID),
	}})
	collector := &stubContextCollector{evidence: podEvidence}
	decider := &stubDecisionSource{decision: decision}
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
		collector:  collector,
		ollama:     decider,
		memory:     newRemediationMemory(),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:        func() time.Time { return observedAt },
	}

	agent.handlePodCrashLooping(context.Background(), alert)

	if decider.received.IncidentID != incidentID || decider.received.Pod.Target.UID != "pod-uid-a" {
		t.Fatalf("decider received %#v; want current incident and observed Pod UID", decider.received)
	}
	actions := kubernetesClient.Actions()
	if len(actions) != 1 || actions[0].GetVerb() != "delete" || actions[0].GetResource().Resource != "pods" {
		t.Fatalf("Kubernetes actions = %#v; want exactly one Pod delete", actions)
	}
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
}

func (source *stubDecisionSource) decide(_ context.Context, evidence IncidentEvidence) (Decision, error) {
	source.received = evidence
	return source.decision, source.err
}
