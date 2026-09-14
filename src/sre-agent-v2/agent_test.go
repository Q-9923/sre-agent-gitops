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

func TestRunCycleDoesNotLogFailureWhenPrometheusCallIsCanceledByShutdown(
	t *testing.T,
) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var logOutput bytes.Buffer

	agent := &sreAgent{
		prometheus: &cancelingAlertSourceForShutdown{
			cancel: cancel,
		},
		logger: slog.New(
			slog.NewJSONHandler(&logOutput, nil),
		),
		now: time.Now,
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
