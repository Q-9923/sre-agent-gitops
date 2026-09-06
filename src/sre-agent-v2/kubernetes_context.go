package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	defaultLogTailLines = int64(50)
	maxLogBytes         = int64(32 * 1024)
	maxLoggedContainers = 5
	maxRecentEvents     = 20
)

type kubernetesContextCollector struct {
	client kubernetes.Interface
}

func newKubernetesContextCollector(client kubernetes.Interface) *kubernetesContextCollector {
	return &kubernetesContextCollector{client: client}
}

func (collector *kubernetesContextCollector) collect(
	ctx context.Context,
	cluster, namespace, podName string,
) (PodEvidence, error) {
	pod, err := collector.client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return PodEvidence{}, fmt.Errorf("get pod %s/%s: %w", namespace, podName, err)
	}

	logs := collector.collectLogs(ctx, pod)
	eventList, err := collector.client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.uid=" + string(pod.UID),
		Limit:         50,
	})
	if err != nil {
		return PodEvidence{}, fmt.Errorf("list events for pod %s/%s: %w", namespace, podName, err)
	}
	events := newestEvents(eventList.Items, maxRecentEvents)

	return buildPodEvidence(cluster, pod, logs, events)
}

func (collector *kubernetesContextCollector) collectLogs(ctx context.Context, pod *v1.Pod) []ContainerLog {
	statuses := append([]v1.ContainerStatus{}, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	if len(statuses) > maxLoggedContainers {
		statuses = statuses[:maxLoggedContainers]
	}

	logs := make([]ContainerLog, 0, len(statuses))
	for _, status := range statuses {
		previous := status.RestartCount > 0
		content, err := collector.readContainerLog(ctx, pod.Namespace, pod.Name, status.Name, previous)
		if err != nil && previous {
			previous = false
			content, err = collector.readContainerLog(ctx, pod.Namespace, pod.Name, status.Name, false)
		}

		entry := ContainerLog{Container: status.Name, Previous: previous, Content: content}
		if err != nil {
			entry.Error = err.Error()
		}
		logs = append(logs, entry)
	}
	return logs
}

func (collector *kubernetesContextCollector) readContainerLog(
	ctx context.Context,
	namespace, podName, containerName string,
	previous bool,
) (string, error) {
	tailLines := defaultLogTailLines
	limitBytes := maxLogBytes
	raw, err := collector.client.CoreV1().Pods(namespace).GetLogs(podName, &v1.PodLogOptions{
		Container:  containerName,
		Previous:   previous,
		TailLines:  &tailLines,
		LimitBytes: &limitBytes,
	}).DoRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("read logs for container %s: %w", containerName, err)
	}
	return truncateUTF8(string(raw), int(maxLogBytes)), nil
}

func buildPodEvidence(cluster string, pod *v1.Pod, logs []ContainerLog, events []v1.Event) (PodEvidence, error) {
	if strings.TrimSpace(cluster) == "" {
		return PodEvidence{}, fmt.Errorf("cluster is required")
	}
	if pod == nil {
		return PodEvidence{}, fmt.Errorf("pod is required")
	}
	if pod.Name == "" || pod.Namespace == "" || pod.UID == "" {
		return PodEvidence{}, fmt.Errorf("pod name, namespace, and UID are required")
	}

	evidence := PodEvidence{
		Target: DecisionTarget{
			Cluster:   cluster,
			Namespace: pod.Namespace,
			Kind:      "Pod",
			Name:      pod.Name,
			UID:       string(pod.UID),
		},
		Phase:      string(pod.Status.Phase),
		NodeName:   pod.Spec.NodeName,
		Owner:      controllerOwner(pod.OwnerReferences),
		RecentLogs: append([]ContainerLog(nil), logs...),
	}

	statuses := append([]v1.ContainerStatus{}, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	for _, status := range statuses {
		evidence.Containers = append(evidence.Containers, containerEvidence(status))
	}
	for _, event := range events {
		evidence.RecentEvents = append(evidence.RecentEvents, EventEvidence{
			Type:    event.Type,
			Reason:  event.Reason,
			Message: truncateUTF8(event.Message, 4096),
			Count:   event.Count,
		})
	}

	return evidence, nil
}

func controllerOwner(references []metav1.OwnerReference) OwnerEvidence {
	for _, reference := range references {
		if reference.Controller != nil && *reference.Controller {
			return OwnerEvidence{
				Kind: reference.Kind,
				Name: reference.Name,
				UID:  string(reference.UID),
			}
		}
	}
	return OwnerEvidence{}
}

func containerEvidence(status v1.ContainerStatus) ContainerEvidence {
	evidence := ContainerEvidence{
		Name:         status.Name,
		Ready:        status.Ready,
		RestartCount: status.RestartCount,
	}
	switch {
	case status.State.Waiting != nil:
		evidence.State = "waiting"
		evidence.Reason = status.State.Waiting.Reason
	case status.State.Running != nil:
		evidence.State = "running"
	case status.State.Terminated != nil:
		evidence.State = "terminated"
		evidence.Reason = status.State.Terminated.Reason
	}
	if status.LastTerminationState.Terminated != nil {
		evidence.LastTerminationReason = status.LastTerminationState.Terminated.Reason
		evidence.LastTerminationExitCode = status.LastTerminationState.Terminated.ExitCode
	}
	return evidence
}

func newestEvents(events []v1.Event, maximum int) []v1.Event {
	sorted := append([]v1.Event(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return eventTime(sorted[i]).After(eventTime(sorted[j]))
	})
	if len(sorted) > maximum {
		sorted = sorted[:maximum]
	}
	return sorted
}

func eventTime(event v1.Event) time.Time {
	if !event.EventTime.IsZero() {
		return event.EventTime.Time
	}
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time
	}
	return event.CreationTimestamp.Time
}
