package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	remediationdomain "sre-agent/internal/remediation"
)

func TestKubernetesVerificationExecutorFailsClosedAtReplicaSetAndPodBoundaries(
	t *testing.T,
) {
	t.Parallel()

	tests := []struct {
		name       string
		prepare    func(*appsv1.ReplicaSet, *corev1.Pod) kubernetes.Interface
		mutateRS   func(*appsv1.ReplicaSet)
		mutatePod  func(*corev1.Pod)
		wantError  string
		wantStatus remediationdomain.VerificationStatus
	}{
		{
			name: "client_not_configured",
			prepare: func(
				*appsv1.ReplicaSet,
				*corev1.Pod,
			) kubernetes.Interface {
				return nil
			},
			wantError: "Kubernetes client is not configured",
		},
		{
			name: "replicaset_get_failure",
			prepare: func(
				replicaSet *appsv1.ReplicaSet,
				pod *corev1.Pod,
			) kubernetes.Interface {
				client := fake.NewSimpleClientset(replicaSet, pod)
				client.Fake.PrependReactor(
					"get",
					"replicasets",
					func(
						k8stesting.Action,
					) (bool, runtime.Object, error) {
						return true,
							nil,
							errors.New("ReplicaSet API unavailable")
					},
				)

				return client
			},
			wantError: "ReplicaSet API unavailable",
		},
		{
			name: "replicaset_not_found",
			prepare: func(
				_ *appsv1.ReplicaSet,
				pod *corev1.Pod,
			) kubernetes.Interface {
				return fake.NewSimpleClientset(pod)
			},
			wantError: "not found",
		},
		{
			name: "observed_generation_is_stale",
			mutateRS: func(replicaSet *appsv1.ReplicaSet) {
				replicaSet.Status.ObservedGeneration =
					replicaSet.Generation - 1
			},
			wantError: "status generation",
		},
		{
			name: "desired_replicas_is_zero",
			mutateRS: func(replicaSet *appsv1.ReplicaSet) {
				zero := int32(0)
				replicaSet.Spec.Replicas = &zero
				replicaSet.Status.Replicas = 0
				replicaSet.Status.ReadyReplicas = 0
				replicaSet.Status.AvailableReplicas = 0
			},
			wantError: "non-positive desired replicas",
		},
		{
			name: "replacement_pod_is_terminating",
			mutatePod: func(pod *corev1.Pod) {
				deletedAt := metav1.NewTime(
					time.Date(
						2026,
						time.October,
						13,
						14,
						0,
						0,
						0,
						time.UTC,
					),
				)
				pod.DeletionTimestamp = &deletedAt
			},
			wantStatus: remediationdomain.VerificationStatusNotRecovered,
		},
		{
			name: "replacement_pod_running_but_not_ready",
			mutatePod: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodRunning
				pod.Status.Conditions = []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionFalse,
					},
				}
			},
			wantStatus: remediationdomain.VerificationStatusNotRecovered,
		},
		{
			name: "replicaset_selector_is_invalid",
			mutateRS: func(replicaSet *appsv1.ReplicaSet) {
				replicaSet.Spec.Selector = &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "app",
							Operator: metav1.LabelSelectorOperator("INVALID"),
						},
					},
				}
			},
			wantError: "selector",
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			replicaSet := verificationBoundaryReplicaSet()
			pod := verificationBoundaryReplacementPod(replicaSet)
			if test.mutateRS != nil {
				test.mutateRS(replicaSet)
			}
			if test.mutatePod != nil {
				test.mutatePod(pod)
			}

			var client kubernetes.Interface
			if test.prepare != nil {
				client = test.prepare(replicaSet, pod)
			} else {
				client = fake.NewSimpleClientset(replicaSet, pod)
			}

			executor := newKubernetesVerificationExecutor(client, "dev")
			result, err := executor.Verify(
				context.Background(),
				verificationBoundaryVerification(replicaSet),
			)

			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf(
						"Verify() error = %v; want error containing %q",
						err,
						test.wantError,
					)
				}
				return
			}

			if err != nil {
				t.Fatalf("Verify() error = %v; want nil", err)
			}
			if result.Status != test.wantStatus {
				t.Fatalf(
					"Verify() status = %q; want %q",
					result.Status,
					test.wantStatus,
				)
			}
			if result.EvidenceCode !=
				replicaSetReplacementNotReadyEvidenceCode {
				t.Fatalf(
					"Verify() evidence code = %q; want %q",
					result.EvidenceCode,
					replicaSetReplacementNotReadyEvidenceCode,
				)
			}
		})
	}
}

func verificationBoundaryReplicaSet() *appsv1.ReplicaSet {
	desired := int32(1)

	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "crash-app-rs",
			Namespace:  "default",
			UID:        types.UID("replicaset-uid"),
			Generation: 2,
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &desired,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "crash-app"},
			},
		},
		Status: appsv1.ReplicaSetStatus{
			ObservedGeneration: 2,
			Replicas:           1,
			ReadyReplicas:      1,
			AvailableReplicas:  1,
		},
	}
}

func verificationBoundaryReplacementPod(
	replicaSet *appsv1.ReplicaSet,
) *corev1.Pod {
	controller := true

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crash-app-replacement",
			Namespace: "default",
			UID:       types.UID("replacement-pod-uid"),
			Labels:    map[string]string{"app": "crash-app"},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       replicaSet.Name,
					UID:        replicaSet.UID,
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
}

func verificationBoundaryVerification(
	replicaSet *appsv1.ReplicaSet,
) remediationdomain.Verification {
	return remediationdomain.Verification{
		ActionKey: remediationdomain.ActionKey{
			IncidentID: "inc-executor-boundary",
			PlanHash:   "plan-executor-boundary",
			TargetUID:  "original-pod-uid",
		},
		Subject: remediationdomain.VerificationSubject{
			Cluster:   "dev",
			Namespace: replicaSet.Namespace,
			Kind:      "ReplicaSet",
			Name:      replicaSet.Name,
			UID:       string(replicaSet.UID),
		},
	}
}
