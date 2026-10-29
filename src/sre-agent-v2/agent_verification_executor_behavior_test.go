package main

import (
	"context"
	"errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"log/slog"
	remediationdomain "sre-agent/internal/remediation"
	"testing"
)

func TestKubernetesVerificationExecutorDoesNotTrustStaleReplicaSetStatusBeforeReplacement(
	t *testing.T,
) {
	t.Parallel()

	desiredReplicas := int32(1)
	controller := true

	subject := remediationdomain.VerificationSubject{
		Cluster:   "dev",
		Namespace: "default",
		Kind:      "ReplicaSet",
		Name:      "crash-app-rs",
		UID:       "rs-uid-crash-app",
	}
	originalTargetUID := "pod-uid-before-remediation"

	kubernetesClient := fake.NewSimpleClientset(
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:  subject.Namespace,
				Name:       subject.Name,
				UID:        types.UID(subject.UID),
				Generation: 9,
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
				ObservedGeneration: 9,
				Replicas:           desiredReplicas,
				ReadyReplicas:      desiredReplicas,
				AvailableReplicas:  desiredReplicas,
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: subject.Namespace,
				Name:      "crash-app-before-remediation",
				UID:       types.UID(originalTargetUID),
				Labels: map[string]string{
					"app": "crash-app",
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "apps/v1",
						Kind:       subject.Kind,
						Name:       subject.Name,
						UID:        types.UID(subject.UID),
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
		},
	)

	executor := newKubernetesVerificationExecutor(
		kubernetesClient,
		subject.Cluster,
	)

	result, err := executor.Verify(
		context.Background(),
		remediationdomain.Verification{
			ActionKey: remediationdomain.ActionKey{
				IncidentID: "inc-replicaset-stale-status",
				PlanHash:   "sha256:replicaset-stale-status",
				TargetUID:  originalTargetUID,
			},
			Subject: subject,
		},
	)
	if err != nil {
		t.Fatalf(
			"Verification Executor Verify() error = %v",
			err,
		)
	}

	if result.Status !=
		remediationdomain.VerificationStatusNotRecovered {
		t.Fatalf(
			"Verification Status = %q; want %q",
			result.Status,
			remediationdomain.VerificationStatusNotRecovered,
		)
	}
	if result.EvidenceCode !=
		"REPLICASET_REPLACEMENT_NOT_READY" {
		t.Fatalf(
			"Verification EvidenceCode = %q; want %q",
			result.EvidenceCode,
			"REPLICASET_REPLACEMENT_NOT_READY",
		)
	}
}

func TestKubernetesVerificationExecutorFailsClosedWhenReplacementPodsCannotBeListed(
	t *testing.T,
) {
	t.Parallel()

	const (
		clusterName    = "dev"
		namespace      = "default"
		replicaSetName = "crash-app-rs"
		originalPodUID = "original-pod-uid"
	)

	replicaSetUID := types.UID("replicaset-uid")
	desiredReplicas := int32(1)

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

	kubernetesClient := fake.NewSimpleClientset(replicaSet)

	listErr := errors.New("pod API unavailable")
	kubernetesClient.Fake.PrependReactor(
		"list",
		"pods",
		func(
			k8stesting.Action,
		) (bool, runtime.Object, error) {
			return true, nil, listErr
		},
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

	result, err := agent.verificationExecutor.Verify(
		context.Background(),
		remediationdomain.Verification{
			ID: "ver-pod-list-failure",
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
		},
	)
	if !errors.Is(err, listErr) {
		t.Fatalf(
			"Verification Executor Verify() error = %v; want %v",
			err,
			listErr,
		)
	}

	if result.Status != "" {
		t.Fatalf(
			"Verification Executor Status = %q; want empty status",
			result.Status,
		)
	}

	if result.EvidenceCode != "" {
		t.Fatalf(
			"Verification Executor EvidenceCode = %q; want empty evidence",
			result.EvidenceCode,
		)
	}
}
func TestKubernetesVerificationExecutorRejectsReplacedReplicaSetSubject(
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
		originalReplicaSetUID = "original-replicaset-uid"
	)

	currentReplicaSetUID := types.UID("current-replicaset-uid")
	desiredReplicas := int32(1)
	controller := true

	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  namespace,
			Name:       replicaSetName,
			UID:        currentReplicaSetUID,
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
					UID:        currentReplicaSetUID,
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

	result, err := agent.verificationExecutor.Verify(
		context.Background(),
		remediationdomain.Verification{
			ID: "ver-replaced-replicaset",
			ActionKey: remediationdomain.ActionKey{
				TargetUID: originalPodUID,
			},
			Subject: remediationdomain.VerificationSubject{
				Cluster:   clusterName,
				Namespace: namespace,
				Kind:      "ReplicaSet",
				Name:      replicaSetName,
				UID:       originalReplicaSetUID,
			},
			Status:  remediationdomain.VerificationStatusPending,
			Version: 1,
		},
	)
	if err == nil {
		t.Fatal(
			"Verification Executor Verify() error = nil; " +
				"want replaced ReplicaSet subject rejection",
		)
	}

	if result.Status != "" {
		t.Fatalf(
			"Verification Executor Status = %q; want empty status",
			result.Status,
		)
	}

	if result.EvidenceCode != "" {
		t.Fatalf(
			"Verification Executor EvidenceCode = %q; want empty evidence",
			result.EvidenceCode,
		)
	}
}
func TestKubernetesVerificationExecutorIgnoresReadyPodOwnedByAnotherReplicaSet(
	t *testing.T,
) {
	t.Parallel()

	const (
		clusterName              = "dev"
		namespace                = "default"
		replicaSetName           = "crash-app-rs"
		otherReplicaSetName      = "other-app-rs"
		originalPodUID           = "original-pod-uid"
		foreignReadyPodName      = "foreign-ready-pod"
		foreignReadyPodUID       = "foreign-ready-pod-uid"
		notRecoveredEvidenceCode = "REPLICASET_REPLACEMENT_NOT_READY"
	)

	replicaSetUID := types.UID("replicaset-uid")
	otherReplicaSetUID := types.UID("other-replicaset-uid")
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

	foreignReadyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      foreignReadyPodName,
			UID:       types.UID(foreignReadyPodUID),
			Labels: map[string]string{
				"app": "crash-app",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       otherReplicaSetName,
					UID:        otherReplicaSetUID,
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
		foreignReadyPod,
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

	result, err := agent.verificationExecutor.Verify(
		context.Background(),
		remediationdomain.Verification{
			ID: "ver-foreign-ready-pod",
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
		},
	)
	if err != nil {
		t.Fatalf(
			"Verification Executor Verify() error = %v",
			err,
		)
	}

	if result.Status !=
		remediationdomain.VerificationStatusNotRecovered {
		t.Fatalf(
			"Verification Executor Status = %q; want %q",
			result.Status,
			remediationdomain.VerificationStatusNotRecovered,
		)
	}

	if result.EvidenceCode != notRecoveredEvidenceCode {
		t.Fatalf(
			"Verification Executor EvidenceCode = %q; want %q",
			result.EvidenceCode,
			notRecoveredEvidenceCode,
		)
	}
}
func TestKubernetesVerificationExecutorDoesNotCountOriginalActionTargetAsReplacement(
	t *testing.T,
) {
	t.Parallel()

	const (
		clusterName              = "dev"
		namespace                = "default"
		replicaSetName           = "crash-app-rs"
		originalPodName          = "crash-app-original"
		originalPodUID           = "original-pod-uid"
		notRecoveredEvidenceCode = "REPLICASET_REPLACEMENT_NOT_READY"
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

	originalPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      originalPodName,
			UID:       types.UID(originalPodUID),
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
		originalPod,
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

	result, err := agent.verificationExecutor.Verify(
		context.Background(),
		remediationdomain.Verification{
			ID: "ver-original-pod-not-replacement",
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
		},
	)
	if err != nil {
		t.Fatalf(
			"Verification Executor Verify() error = %v",
			err,
		)
	}

	if result.Status !=
		remediationdomain.VerificationStatusNotRecovered {
		t.Fatalf(
			"Verification Executor Status = %q; want %q",
			result.Status,
			remediationdomain.VerificationStatusNotRecovered,
		)
	}

	if result.EvidenceCode != notRecoveredEvidenceCode {
		t.Fatalf(
			"Verification Executor EvidenceCode = %q; want %q",
			result.EvidenceCode,
			notRecoveredEvidenceCode,
		)
	}
}
func TestKubernetesVerificationExecutorFailsClosedForInvalidSubjectOrCanceledContext(
	t *testing.T,
) {
	t.Parallel()

	const (
		clusterName    = "dev"
		namespace      = "default"
		replicaSetName = "crash-app-rs"
		replicaSetUID  = "replicaset-uid"
		originalPodUID = "original-pod-uid"
	)

	kubernetesClient := fake.NewSimpleClientset()

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

	baseVerification := remediationdomain.Verification{
		ID: "ver-invalid-subject",
		ActionKey: remediationdomain.ActionKey{
			TargetUID: originalPodUID,
		},
		Subject: remediationdomain.VerificationSubject{
			Cluster:   clusterName,
			Namespace: namespace,
			Kind:      "ReplicaSet",
			Name:      replicaSetName,
			UID:       replicaSetUID,
		},
		Status:  remediationdomain.VerificationStatusPending,
		Version: 1,
	}

	clusterMismatch := baseVerification
	clusterMismatch.Subject.Cluster = "other-cluster"

	unsupportedKind := baseVerification
	unsupportedKind.Subject.Kind = "Deployment"

	canceledContext, cancel := context.WithCancel(
		context.Background(),
	)
	cancel()

	testCases := []struct {
		name         string
		ctx          context.Context
		verification remediationdomain.Verification
		wantError    error
	}{
		{
			name:         "cluster_mismatch",
			ctx:          context.Background(),
			verification: clusterMismatch,
		},
		{
			name:         "unsupported_kind",
			ctx:          context.Background(),
			verification: unsupportedKind,
		},
		{
			name:         "context_canceled",
			ctx:          canceledContext,
			verification: baseVerification,
			wantError:    context.Canceled,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			result, err := agent.verificationExecutor.Verify(
				testCase.ctx,
				testCase.verification,
			)
			if err == nil {
				t.Fatal(
					"Verification Executor Verify() error = nil; " +
						"want fail-closed error",
				)
			}

			if testCase.wantError != nil &&
				!errors.Is(err, testCase.wantError) {
				t.Fatalf(
					"Verification Executor Verify() error = %v; want %v",
					err,
					testCase.wantError,
				)
			}

			if result.Status != "" {
				t.Fatalf(
					"Verification Executor Status = %q; want empty status",
					result.Status,
				)
			}

			if result.EvidenceCode != "" {
				t.Fatalf(
					"Verification Executor EvidenceCode = %q; "+
						"want empty evidence",
					result.EvidenceCode,
				)
			}
		})
	}
}
