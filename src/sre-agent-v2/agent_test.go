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
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
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
