package main

import (
	"context"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"log/slog"
	remediationdomain "sre-agent/internal/remediation"
	"testing"
)

func TestNewSREAgentWiresRecoveredReplicaSetVerificationExecutor(
	t *testing.T,
) {
	t.Parallel()

	const (
		clusterName           = "dev"
		namespace             = "default"
		replicaSetName        = "crash-app-rs"
		originalPodUID        = "original-pod-uid"
		replacementPodName    = "crash-app-replacement"
		replacementPodUID     = "replacement-pod-uid"
		recoveredEvidenceCode = "REPLICASET_AVAILABLE"
	)

	replicaSetUID := types.UID("replicaset-uid")
	desiredReplicas := int32(1)
	controller := true

	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  namespace,
			Name:       replicaSetName,
			UID:        replicaSetUID,
			Generation: 1,
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &desiredReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "crash-app",
				},
			},
		},
		Status: appsv1.ReplicaSetStatus{
			ObservedGeneration:   1,
			Replicas:             1,
			FullyLabeledReplicas: 1,
			ReadyReplicas:        1,
			AvailableReplicas:    1,
		},
	}

	replacementPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      replacementPodName,
			UID:       types.UID(replacementPodUID),
			Labels: map[string]string{
				"app": "crash-app",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       replicaSetName,
					UID:        replicaSetUID,
					Controller: &controller,
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	kubernetesClient := fake.NewSimpleClientset(
		replicaSet,
		replacementPod,
	)

	agent := newSREAgent(
		agentConfig{
			ClusterName: clusterName,
		},
		kubernetesClient,
		incidentRegistryHandle{},
		slog.Default(),
		func() {},
		func() {},
		func(string) {},
	)

	if agent.verificationExecutor == nil {
		t.Fatal(
			"newSREAgent() Verification Executor = nil; " +
				"want configured Kubernetes executor",
		)
	}

	verification := remediationdomain.Verification{
		ID: "ver-replicaset-recovered",
		ActionKey: remediationdomain.ActionKey{
			TargetUID: originalPodUID,
		},
		Subject: remediationdomain.VerificationSubject{
			Cluster:   clusterName,
			Namespace: namespace,
			Kind:      "ReplicaSet",
			Name:      replicaSetName,
			UID:       string(replicaSetUID),
		},
		Status:  remediationdomain.VerificationStatusPending,
		Version: 1,
	}

	result, err := agent.verificationExecutor.Verify(
		context.Background(),
		verification,
	)
	if err != nil {
		t.Fatalf(
			"Verification Executor Verify() error = %v",
			err,
		)
	}

	if result.Status !=
		remediationdomain.VerificationStatusRecovered {
		t.Fatalf(
			"Verification Executor Status = %q; want %q",
			result.Status,
			remediationdomain.VerificationStatusRecovered,
		)
	}

	if result.EvidenceCode != recoveredEvidenceCode {
		t.Fatalf(
			"Verification Executor EvidenceCode = %q; want %q",
			result.EvidenceCode,
			recoveredEvidenceCode,
		)
	}
}
