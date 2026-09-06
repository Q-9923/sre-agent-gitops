package main

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestBuildPodEvidenceIncludesIdentityOwnerFailureAndEvents(t *testing.T) {
	isController := true
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crash-app-abc",
			Namespace: "default",
			UID:       types.UID("pod-uid-123"),
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "crash-app-rs", UID: types.UID("rs-uid-1"), Controller: &isController},
			},
		},
		Spec: v1.PodSpec{NodeName: "k8s-node2"},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			ContainerStatuses: []v1.ContainerStatus{
				{
					Name:         "main",
					Ready:        false,
					RestartCount: 4,
					State: v1.ContainerState{
						Waiting: &v1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
					LastTerminationState: v1.ContainerState{
						Terminated: &v1.ContainerStateTerminated{Reason: "Error", ExitCode: 1},
					},
				},
			},
		},
	}
	logs := []ContainerLog{{Container: "main", Previous: true, Content: "I am designed to fail!"}}
	events := []v1.Event{{
		Type:    v1.EventTypeWarning,
		Reason:  "BackOff",
		Message: "Back-off restarting failed container",
		Count:   4,
	}}

	evidence, err := buildPodEvidence("dev", pod, logs, events)
	if err != nil {
		t.Fatalf("buildPodEvidence() error = %v; want nil", err)
	}
	if evidence.Target.UID != "pod-uid-123" || evidence.Target.Name != "crash-app-abc" {
		t.Fatalf("Target = %#v; want the observed pod identity", evidence.Target)
	}
	if evidence.Owner.Kind != "ReplicaSet" || evidence.Owner.Name != "crash-app-rs" {
		t.Fatalf("Owner = %#v; want controller ReplicaSet/crash-app-rs", evidence.Owner)
	}
	if len(evidence.Containers) != 1 {
		t.Fatalf("len(Containers) = %d; want 1", len(evidence.Containers))
	}
	container := evidence.Containers[0]
	if container.State != "waiting" || container.Reason != "CrashLoopBackOff" {
		t.Fatalf("container state = %q reason = %q; want waiting/CrashLoopBackOff", container.State, container.Reason)
	}
	if container.LastTerminationReason != "Error" || container.LastTerminationExitCode != 1 {
		t.Fatalf("last termination = %q/%d; want Error/1", container.LastTerminationReason, container.LastTerminationExitCode)
	}
	if len(evidence.RecentLogs) != 1 || evidence.RecentLogs[0].Content != "I am designed to fail!" {
		t.Fatalf("RecentLogs = %#v; want previous container log", evidence.RecentLogs)
	}
	if len(evidence.RecentEvents) != 1 || evidence.RecentEvents[0].Reason != "BackOff" {
		t.Fatalf("RecentEvents = %#v; want BackOff event", evidence.RecentEvents)
	}
}
