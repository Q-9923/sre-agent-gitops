package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
)

const podCrashLoopingAlert = "PodCrashLooping"

type firingAlertSource interface {
	firingAlerts(context.Context) ([]Alert, error)
}

type podContextSource interface {
	collect(context.Context, string, string, string) (PodEvidence, error)
}

type decisionSource interface {
	decide(context.Context, IncidentEvidence) (Decision, error)
}

type sreAgent struct {
	config     agentConfig
	kubernetes kubernetes.Interface
	collector  podContextSource
	prometheus firingAlertSource
	ollama     decisionSource
	memory     *remediationMemory
	logger     *slog.Logger
	now        func() time.Time
}

func newSREAgent(config agentConfig, kubernetesClient kubernetes.Interface, logger *slog.Logger) *sreAgent {
	return &sreAgent{
		config:     config,
		kubernetes: kubernetesClient,
		collector:  newKubernetesContextCollector(kubernetesClient),
		prometheus: newPrometheusClient(config.PrometheusURL, &http.Client{Timeout: config.PrometheusTimeout}),
		ollama:     newOllamaClient(config.OllamaURL, config.OllamaModel, &http.Client{Timeout: config.OllamaTimeout}),
		memory:     newRemediationMemory(),
		logger:     logger,
		now:        time.Now,
	}
}

func (agent *sreAgent) run(ctx context.Context) {
	agent.runCycle(ctx)

	ticker := time.NewTicker(agent.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			agent.logger.Info("agent_stopped", "result", "SHUTDOWN")
			return
		case <-ticker.C:
			agent.runCycle(ctx)
		}
	}
}

func (agent *sreAgent) runCycle(ctx context.Context) {
	startedAt := agent.now()
	alerts, err := agent.prometheus.firingAlerts(ctx)
	if err != nil {
		agent.logger.Error(
			"cycle_failed",
			"result", "ERROR",
			"error_code", "PROMETHEUS_QUERY_FAILED",
			"duration_ms", agent.now().Sub(startedAt).Milliseconds(),
			"error", err,
		)
		return
	}

	actionableTotal := 0
	unsupportedTotal := 0
	invalidTotal := 0
	for _, alert := range alerts {
		if alert.Labels["alertname"] != podCrashLoopingAlert {
			unsupportedTotal++
			continue
		}
		namespace := alert.Labels["namespace"]
		podName := alert.Labels["pod"]
		if namespace == "" || podName == "" {
			invalidTotal++
			continue
		}
		actionableTotal++
		agent.handlePodCrashLooping(ctx, alert)
	}

	result := "PROCESSED"
	if actionableTotal == 0 {
		result = "NO_ACTION"
	}
	agent.logger.Info(
		"cycle_completed",
		"result", result,
		"firing_total", len(alerts),
		"actionable_total", actionableTotal,
		"unsupported_total", unsupportedTotal,
		"invalid_total", invalidTotal,
		"duration_ms", agent.now().Sub(startedAt).Milliseconds(),
	)
}

func (agent *sreAgent) handlePodCrashLooping(ctx context.Context, alert Alert) {
	namespace := alert.Labels["namespace"]
	podName := alert.Labels["pod"]
	targetLabel := namespace + "/" + podName

	collectContext, cancelCollect := context.WithTimeout(ctx, agent.config.KubernetesRequestTimeout)
	podEvidence, err := agent.collector.collect(collectContext, agent.config.ClusterName, namespace, podName)
	cancelCollect()
	if err != nil {
		if apierrors.IsNotFound(err) {
			agent.logger.Info(
				"incident_stale",
				"target", targetLabel,
				"result", "NO_ACTION",
				"error_code", "TARGET_NOT_FOUND",
			)
			return
		}
		agent.logger.Warn(
			"incident_context_failed",
			"target", targetLabel,
			"result", "NO_ACTION",
			"error_code", "KUBERNETES_CONTEXT_FAILED",
			"error", err,
		)
		return
	}

	incidentID := incidentIDFor(alert, podEvidence.Target.UID)
	targetKey := remediationTargetKey(podEvidence)
	now := agent.now()
	remediationState := agent.memory.snapshot(
		incidentID,
		targetKey,
		now,
		agent.config.RestartCooldown,
		agent.config.AttemptWindow,
	)
	if remediationState.Duplicate {
		agent.logger.Info(
			"incident_skipped",
			"incident_id", incidentID,
			"target", targetLabel,
			"result", "NO_ACTION",
			"error_code", PolicyCodeDuplicate,
		)
		return
	}

	evidence := IncidentEvidence{
		IncidentID: incidentID,
		Alert: AlertEvidence{
			Name:        alert.Labels["alertname"],
			Description: truncateUTF8(alert.Annotations["description"], 4096),
			State:       alert.State,
			ActiveAt:    alert.ActiveAt,
		},
		Pod: podEvidence,
	}

	decisionContext, cancelDecision := context.WithTimeout(ctx, agent.config.OllamaTimeout)
	decision, err := agent.ollama.decide(decisionContext, evidence)
	cancelDecision()
	if err != nil {
		agent.logger.Warn(
			"decision_failed",
			"incident_id", incidentID,
			"target", targetLabel,
			"result", "NO_ACTION",
			"error_code", "OLLAMA_DECISION_FAILED",
			"error", err,
		)
		return
	}

	policyResult := EvaluateDecision(decision, PolicyContext{
		IncidentID:             incidentID,
		CurrentTarget:          podEvidence.Target,
		AllowedNamespaces:      agent.config.AllowedNamespaces,
		ControllerOwnerKind:    podEvidence.Owner.Kind,
		AllowedControllerKinds: agent.config.AllowedControllerKinds,
		ApprovalGranted:        agent.config.RestartPodApproved,
		MinimumConfidence:      agent.config.MinimumConfidence,
		Duplicate:              remediationState.Duplicate,
		InCooldown:             remediationState.InCooldown,
		AttemptsInWindow:       remediationState.AttemptsInWindow,
		MaximumAttempts:        agent.config.MaximumAttempts,
	})
	if !policyResult.Allowed {
		agent.memory.markIncident(incidentID, now)
		agent.logger.Info(
			"decision_denied",
			"incident_id", incidentID,
			"action", decision.Action,
			"target", targetLabel,
			"confidence", decision.Confidence,
			"risk", decision.Risk,
			"result", "NO_ACTION",
			"error_code", policyResult.Code,
		)
		return
	}

	// Count the authorized attempt before calling Kubernetes. A timeout or
	// ambiguous network result must not trigger an immediate second deletion.
	agent.memory.record(incidentID, targetKey, now)
	actionContext, cancelAction := context.WithTimeout(ctx, agent.config.KubernetesRequestTimeout)
	err = executeApprovedAction(actionContext, agent.kubernetes, decision)
	cancelAction()
	if err != nil {
		agent.logger.Error(
			"remediation_failed",
			"incident_id", incidentID,
			"action", decision.Action,
			"target", targetLabel,
			"result", "ERROR",
			"error_code", "KUBERNETES_ACTION_FAILED",
			"error", err,
		)
		return
	}

	agent.logger.Info(
		"remediation_submitted",
		"incident_id", incidentID,
		"action", decision.Action,
		"target", targetLabel,
		"result", "SUBMITTED",
		"error_code", "",
	)
}

func incidentIDFor(alert Alert, podUID string) string {
	input := fmt.Sprintf(
		"%s\x00%s\x00%s\x00%s\x00%s",
		alert.Labels["alertname"],
		alert.Labels["namespace"],
		alert.Labels["pod"],
		podUID,
		alert.ActiveAt.UTC().Format(time.RFC3339Nano),
	)
	digest := sha256.Sum256([]byte(input))
	return fmt.Sprintf("inc-%x", digest[:12])
}
