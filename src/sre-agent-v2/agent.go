package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"log/slog"
	"net/http"
	approvaldomain "sre-agent/internal/approval"
	"sre-agent/internal/incident"
	"time"
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
	config              agentConfig
	kubernetes          kubernetes.Interface
	collector           podContextSource
	prometheus          firingAlertSource
	ollama              decisionSource
	incidents           incidentRegistry
	memory              remediationStateStore
	approvals           approvaldomain.Store
	logger              *slog.Logger
	now                 func() time.Time
	markCycleProgress   func()
	markSuccessfulCycle func()
	recordCycleResult   func(string)
}

func newSREAgent(
	config agentConfig,
	kubernetesClient kubernetes.Interface,
	incidents incidentRegistry,
	approvals approvaldomain.Store,
	logger *slog.Logger,
	markCycleProgress func(),
	markSuccessfulCycle func(),
	recordCycleResult func(string),
) *sreAgent {
	return &sreAgent{
		config:     config,
		kubernetes: kubernetesClient,
		incidents:  incidents,
		approvals:  approvals,
		collector:  newKubernetesContextCollector(kubernetesClient),
		prometheus: newPrometheusClient(
			config.PrometheusURL,
			&http.Client{Timeout: config.PrometheusTimeout},
		),
		ollama: newOllamaClient(
			config.OllamaURL,
			config.OllamaModel,
			&http.Client{Timeout: config.OllamaTimeout},
		),
		memory: newConfigMapRemediationState(
			kubernetesClient,
			config.RemediationStateNamespace,
			config.RemediationStateConfigMap,
		),
		logger:              logger,
		now:                 time.Now,
		markCycleProgress:   markCycleProgress,
		markSuccessfulCycle: markSuccessfulCycle,
		recordCycleResult:   recordCycleResult,
	}
}
func (agent *sreAgent) run(ctx context.Context) {
	if ctx.Err() != nil {
		agent.logger.Info(
			"agent_stopped",
			"result", "SHUTDOWN",
		)
		return
	}

	agent.runCycle(ctx)

	ticker := time.NewTicker(agent.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			agent.logger.Info(
				"agent_stopped",
				"result", "SHUTDOWN",
			)
			return

		case <-ticker.C:
			if ctx.Err() != nil {
				agent.logger.Info(
					"agent_stopped",
					"result", "SHUTDOWN",
				)
				return
			}

			agent.runCycle(ctx)
		}
	}
}

func (agent *sreAgent) runCycle(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	if agent.markCycleProgress != nil {
		agent.markCycleProgress()
	}

	defer func() {
		if agent.markCycleProgress != nil {
			agent.markCycleProgress()
		}
	}()

	startedAt := agent.now()
	alerts, err := agent.prometheus.firingAlerts(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}

		agent.logger.Error(
			"cycle_failed",
			"result", "ERROR",
			"error_code", "PROMETHEUS_QUERY_FAILED",
			"duration_ms", agent.now().Sub(startedAt).Milliseconds(),
			"error", err,
		)
		if agent.recordCycleResult != nil {
			agent.recordCycleResult("ERROR")
		}
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
		if agent.markCycleProgress != nil {
			agent.markCycleProgress()
		}
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
	if agent.recordCycleResult != nil {
		agent.recordCycleResult(result)
	}
	if agent.markSuccessfulCycle != nil {
		agent.markSuccessfulCycle()
	}
}

func (agent *sreAgent) storedApprovalGranted(
	ctx context.Context,
	incidentID string,
	action string,
	target DecisionTarget,
	now time.Time,
) (bool, error) {
	if !agent.config.RestartPodApproved {
		return false, nil
	}
	if agent.approvals == nil {
		return false, errors.New("approval store is not configured")
	}

	plan, err := approvaldomain.NewPlan(
		approvaldomain.PlanCommand{
			IncidentID: incidentID,
			Action:     action,
			Target: approvaldomain.Target{
				Cluster:   target.Cluster,
				Namespace: target.Namespace,
				Kind:      target.Kind,
				Name:      target.Name,
				UID:       target.UID,
			},
		},
	)
	if err != nil {
		return false, fmt.Errorf(
			"create canonical remediation plan: %w",
			err,
		)
	}

	granted, err := agent.approvals.Lookup(
		ctx,
		approvaldomain.ApprovalKey{
			IncidentID: plan.IncidentID,
			PlanHash:   plan.Hash,
			TargetUID:  plan.Target.UID,
		},
	)
	if errors.Is(err, approvaldomain.ErrApprovalNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf(
			"lookup remediation approval: %w",
			err,
		)
	}

	if err := approvaldomain.Validate(
		granted,
		plan,
		now,
	); err != nil {
		return false, fmt.Errorf(
			"validate remediation approval: %w",
			err,
		)
	}

	return true, nil
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
	incidentID := incidentIDFor(
		alert,
		podEvidence.Target.UID,
	)

	incidentStoreStartedAt := agent.now()
	observedIncident, _, err := agent.incidents.Observe(
		ctx,
		incident.Observation{
			Source:    "prometheus",
			Cluster:   agent.config.ClusterName,
			AlertName: alert.Labels["alertname"],
			Target: incident.Target{
				Kind:      podEvidence.Target.Kind,
				Namespace: podEvidence.Target.Namespace,
				Name:      podEvidence.Target.Name,
				UID:       podEvidence.Target.UID,
			},
		},
	)
	incidentStoreDuration := agent.now().Sub(
		incidentStoreStartedAt,
	)

	if err != nil {
		if ctx.Err() != nil {
			return
		}

		agent.logger.Warn(
			"incident_store_unavailable",
			"incident_id", incidentID,
			"action", "OBSERVE_INCIDENT",
			"target", targetLabel,
			"result", "NO_ACTION",
			"duration_ms", incidentStoreDuration.Milliseconds(),
			"error_code", "INCIDENT_STORE_UNAVAILABLE",
			"error", err,
		)
		return
	}

	claimEnabled :=
		agent.config.IncidentClaimHolderID != "" &&
			agent.config.IncidentClaimLeaseDuration > 0

	currentIncident := observedIncident

	if claimEnabled {
		claimStartedAt := agent.now()
		claim, claimErr := agent.incidents.Claim(
			ctx,
			incident.ClaimCommand{
				IncidentID:      observedIncident.ID,
				ExpectedVersion: observedIncident.Version,
				HolderID:        agent.config.IncidentClaimHolderID,
				Now:             agent.now(),
				LeaseDuration:   agent.config.IncidentClaimLeaseDuration,
			},
		)
		claimDuration := agent.now().Sub(claimStartedAt)

		if claimErr != nil {
			if ctx.Err() != nil {
				return
			}

			agent.logger.Warn(
				"incident_claim_failed",
				"incident_id", observedIncident.ID,
				"action", "CLAIM_INCIDENT",
				"target", targetLabel,
				"result", "NO_ACTION",
				"duration_ms", claimDuration.Milliseconds(),
				"error_code", "INCIDENT_CLAIM_FAILED",
				"error", claimErr,
			)
			return
		}

		currentIncident = claim.Incident
	}
	targetKey := remediationTargetKey(podEvidence)
	now := agent.now()

	stateContext, cancelState := context.WithTimeout(
		ctx,
		agent.config.KubernetesRequestTimeout,
	)
	remediationState, err := agent.memory.Snapshot(
		stateContext,
		remediationStateQuery{
			IncidentID:    incidentID,
			TargetKey:     targetKey,
			Now:           now,
			Cooldown:      agent.config.RestartCooldown,
			AttemptWindow: agent.config.AttemptWindow,
		},
	)
	cancelState()
	if err != nil {
		agent.logger.Warn(
			"remediation_state_unavailable",
			"incident_id", incidentID,
			"target", targetLabel,
			"result", "NO_ACTION",
			"error_code", "REMEDIATION_STATE_UNAVAILABLE",
			"error", err,
		)
		return
	}

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

	if claimEnabled {
		transitionStartedAt := agent.now()
		transitionedIncident, transitionErr := agent.incidents.Transition(
			ctx,
			incident.TransitionCommand{
				IncidentID:      currentIncident.ID,
				ExpectedVersion: currentIncident.Version,
				To:              incident.StateDiagnosed,
				Actor:           agent.config.IncidentClaimHolderID,
				ReasonCode:      "DIAGNOSIS_COMPLETED",
			},
		)
		transitionDuration := agent.now().Sub(
			transitionStartedAt,
		)

		if transitionErr != nil {
			if ctx.Err() != nil {
				return
			}

			agent.logger.Warn(
				"incident_transition_failed",
				"incident_id", currentIncident.ID,
				"action", "DIAGNOSE_INCIDENT",
				"target", targetLabel,
				"result", "NO_ACTION",
				"duration_ms", transitionDuration.Milliseconds(),
				"error_code", "INCIDENT_TRANSITION_FAILED",
				"error", transitionErr,
			)
			return
		}
		currentIncident = transitionedIncident
	}
	approvalCheckStartedAt := agent.now()

	approvalGranted, approvalErr := agent.storedApprovalGranted(
		ctx,
		observedIncident.ID,
		string(decision.Action),
		podEvidence.Target,
		agent.now(),
	)
	approvalCheckDuration := agent.now().Sub(approvalCheckStartedAt)
	if approvalErr != nil {
		if ctx.Err() != nil {
			return
		}

		agent.logger.Warn(
			"approval_check_failed",
			"incident_id", observedIncident.ID,
			"action", decision.Action,
			"target", targetLabel,
			"result", "NO_ACTION",
			"error_code", "APPROVAL_CHECK_FAILED",
			"duration_ms", approvalCheckDuration.Milliseconds(),
			"error", approvalErr,
		)
		return
	}

	policyResult := EvaluateDecision(decision, PolicyContext{
		IncidentID:             incidentID,
		CurrentTarget:          podEvidence.Target,
		AllowedNamespaces:      agent.config.AllowedNamespaces,
		ControllerOwnerKind:    podEvidence.Owner.Kind,
		AllowedControllerKinds: agent.config.AllowedControllerKinds,
		ApprovalGranted:        approvalGranted,
		MinimumConfidence:      agent.config.MinimumConfidence,
		Duplicate:              remediationState.Duplicate,
		InCooldown:             remediationState.InCooldown,
		AttemptsInWindow:       remediationState.AttemptsInWindow,
		MaximumAttempts:        agent.config.MaximumAttempts,
	})

	if !policyResult.Allowed {
		recordContext, cancelRecord := context.WithTimeout(
			ctx,
			agent.config.KubernetesRequestTimeout,
		)
		err = agent.memory.Record(
			recordContext,
			remediationStateRecord{
				Kind:       remediationStateRecordIncidentHandled,
				IncidentID: incidentID,
				OccurredAt: now,
			},
		)
		cancelRecord()
		if err != nil {
			agent.logger.Error(
				"remediation_state_record_failed",
				"incident_id", incidentID,
				"action", decision.Action,
				"target", targetLabel,
				"result", "ERROR",
				"error_code", "REMEDIATION_STATE_RECORD_FAILED",
				"policy_error_code", policyResult.Code,
				"error", err,
			)
			return
		}

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

	if !claimEnabled {
		agent.logger.Warn(
			"incident_action_fence_failed",
			"incident_id", currentIncident.ID,
			"action", decision.Action,
			"target", targetLabel,
			"result", "NO_ACTION",
			"duration_ms", int64(0),
			"error_code", "INCIDENT_ACTION_FENCE_UNAVAILABLE",
		)
		return
	}

	// Count the authorized attempt before calling Kubernetes. A timeout or
	// ambiguous network result must not trigger an immediate second deletion.

	recordContext, cancelRecord := context.WithTimeout(
		ctx,
		agent.config.KubernetesRequestTimeout,
	)
	err = agent.memory.Record(
		recordContext,
		remediationStateRecord{
			Kind:       remediationStateRecordActionAttempted,
			IncidentID: incidentID,
			TargetKey:  targetKey,
			OccurredAt: now,
		},
	)
	cancelRecord()
	if err != nil {
		agent.logger.Error(
			"remediation_state_record_failed",
			"incident_id", incidentID,
			"action", decision.Action,
			"target", targetLabel,
			"result", "ERROR",
			"error_code", "REMEDIATION_STATE_RECORD_FAILED",
			"error", err,
		)
		return
	}

	actionFenceStartedAt := agent.now()
	_, actionFenceErr := agent.incidents.Claim(
		ctx,
		incident.ClaimCommand{
			IncidentID:      currentIncident.ID,
			ExpectedVersion: currentIncident.Version,
			HolderID:        agent.config.IncidentClaimHolderID,
			Now:             agent.now(),
			LeaseDuration:   agent.config.IncidentClaimLeaseDuration,
		},
	)
	actionFenceDuration := agent.now().Sub(actionFenceStartedAt)

	if actionFenceErr != nil {
		if ctx.Err() != nil {
			return
		}

		agent.logger.Warn(
			"incident_action_fence_failed",
			"incident_id", currentIncident.ID,
			"action", decision.Action,
			"target", targetLabel,
			"result", "NO_ACTION",
			"duration_ms", actionFenceDuration.Milliseconds(),
			"error_code", "INCIDENT_ACTION_FENCE_FAILED",
			"error", actionFenceErr,
		)
		return
	}

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
		"%s\x00%s\x00%s\x00%s",
		alert.Labels["alertname"],
		alert.Labels["namespace"],
		alert.Labels["pod"],
		podUID,
	)
	digest := sha256.Sum256([]byte(input))
	return fmt.Sprintf("inc-%x", digest[:12])
}
