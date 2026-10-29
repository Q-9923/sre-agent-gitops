package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	remediationdomain "sre-agent/internal/remediation"
)

const (
	replicaSetAvailableEvidenceCode = "REPLICASET_AVAILABLE"

	replicaSetReplacementNotReadyEvidenceCode = "REPLICASET_REPLACEMENT_NOT_READY"
)

type kubernetesVerificationExecutor struct {
	client      kubernetes.Interface
	clusterName string
}

func newKubernetesVerificationExecutor(
	client kubernetes.Interface,
	clusterName string,
) verificationExecutor {
	return &kubernetesVerificationExecutor{
		client:      client,
		clusterName: strings.TrimSpace(clusterName),
	}
}

func (
	executor *kubernetesVerificationExecutor,
) Verify(
	ctx context.Context,
	verification remediationdomain.Verification,
) (verificationExecutionResult, error) {
	if ctx == nil {
		return verificationExecutionResult{},
			errors.New("verification context is required")
	}
	if err := ctx.Err(); err != nil {
		return verificationExecutionResult{}, err
	}
	if executor == nil || executor.client == nil {
		return verificationExecutionResult{},
			errors.New("Kubernetes client is not configured")
	}
	if executor.clusterName == "" {
		return verificationExecutionResult{},
			errors.New("verification cluster is not configured")
	}

	subject := verification.Subject
	if err := subject.Validate(); err != nil {
		return verificationExecutionResult{},
			fmt.Errorf("validate verification subject: %w", err)
	}
	if strings.TrimSpace(
		verification.ActionKey.TargetUID,
	) == "" {
		return verificationExecutionResult{},
			errors.New(
				"verification action target UID is required",
			)
	}
	if subject.Cluster != executor.clusterName {
		return verificationExecutionResult{},
			fmt.Errorf(
				"verification subject cluster %q does not match configured cluster %q",
				subject.Cluster,
				executor.clusterName,
			)
	}
	if subject.Kind != "ReplicaSet" {
		return verificationExecutionResult{},
			fmt.Errorf(
				"verification subject kind %q is not supported",
				subject.Kind,
			)
	}

	replicaSet, err := executor.client.
		AppsV1().
		ReplicaSets(subject.Namespace).
		Get(
			ctx,
			subject.Name,
			metav1.GetOptions{},
		)
	if err != nil {
		return verificationExecutionResult{},
			fmt.Errorf(
				"get verification ReplicaSet %s/%s: %w",
				subject.Namespace,
				subject.Name,
				err,
			)
	}

	if string(replicaSet.UID) != subject.UID {
		return verificationExecutionResult{},
			fmt.Errorf(
				"verification ReplicaSet UID %q does not match subject UID %q",
				replicaSet.UID,
				subject.UID,
			)
	}
	if replicaSet.DeletionTimestamp != nil {
		return verificationExecutionResult{},
			fmt.Errorf(
				"verification ReplicaSet %s/%s is terminating",
				subject.Namespace,
				subject.Name,
			)
	}
	if replicaSet.Spec.Replicas == nil {
		return verificationExecutionResult{},
			fmt.Errorf(
				"verification ReplicaSet %s/%s has no desired replica count",
				subject.Namespace,
				subject.Name,
			)
	}

	desiredReplicas := *replicaSet.Spec.Replicas
	if desiredReplicas <= 0 {
		return verificationExecutionResult{},
			fmt.Errorf(
				"verification ReplicaSet %s/%s has non-positive desired replicas %d",
				subject.Namespace,
				subject.Name,
				desiredReplicas,
			)
	}
	if replicaSet.Status.ObservedGeneration <
		replicaSet.Generation {
		return verificationExecutionResult{},
			fmt.Errorf(
				"verification ReplicaSet %s/%s status generation %d is behind generation %d",
				subject.Namespace,
				subject.Name,
				replicaSet.Status.ObservedGeneration,
				replicaSet.Generation,
			)
	}
	if replicaSet.Status.Replicas < desiredReplicas ||
		replicaSet.Status.ReadyReplicas < desiredReplicas ||
		replicaSet.Status.AvailableReplicas <
			desiredReplicas {
		return verificationExecutionResult{
			Status: remediationdomain.
				VerificationStatusNotRecovered,
			EvidenceCode: replicaSetReplacementNotReadyEvidenceCode,
		}, nil
	}
	if replicaSet.Spec.Selector == nil {
		return verificationExecutionResult{},
			fmt.Errorf(
				"verification ReplicaSet %s/%s has no selector",
				subject.Namespace,
				subject.Name,
			)
	}

	selector, err := metav1.LabelSelectorAsSelector(
		replicaSet.Spec.Selector,
	)
	if err != nil {
		return verificationExecutionResult{},
			fmt.Errorf(
				"convert verification ReplicaSet selector: %w",
				err,
			)
	}

	pods, err := executor.client.
		CoreV1().
		Pods(subject.Namespace).
		List(
			ctx,
			metav1.ListOptions{
				LabelSelector: selector.String(),
			},
		)
	if err != nil {
		return verificationExecutionResult{},
			fmt.Errorf(
				"list verification Pods for ReplicaSet %s/%s: %w",
				subject.Namespace,
				subject.Name,
				err,
			)
	}

	readyReplacementCount := int32(0)
	for index := range pods.Items {
		if readyControlledReplacementPod(
			&pods.Items[index],
			replicaSet,
			verification.ActionKey.TargetUID,
		) {
			readyReplacementCount++
		}
	}

	if readyReplacementCount < desiredReplicas {
		return verificationExecutionResult{
			Status: remediationdomain.
				VerificationStatusNotRecovered,
			EvidenceCode: replicaSetReplacementNotReadyEvidenceCode,
		}, nil
	}

	return verificationExecutionResult{
		Status: remediationdomain.
			VerificationStatusRecovered,
		EvidenceCode: replicaSetAvailableEvidenceCode,
	}, nil
}

func readyControlledReplacementPod(
	pod *corev1.Pod,
	replicaSet *appsv1.ReplicaSet,
	actionTargetUID string,
) bool {
	if pod == nil ||
		replicaSet == nil ||
		pod.DeletionTimestamp != nil ||
		string(pod.UID) == actionTargetUID {
		return false
	}

	controller := metav1.GetControllerOf(pod)
	if controller == nil ||
		controller.Kind != "ReplicaSet" ||
		controller.UID != replicaSet.UID {
		return false
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady &&
			condition.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}
