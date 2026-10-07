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
	remediationdomain "sre-agent/internal/remediation"
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
	config                agentConfig
	kubernetes            kubernetes.Interface
	collector             podContextSource
	prometheus            firingAlertSource
	ollama                decisionSource
	incidents             incidentRegistry
	memory                remediationStateStore
	approvals             approvaldomain.Store
	plans                 approvaldomain.PlanStore
	actionAttempts        remediationdomain.ActionAttemptStore
	verifications         remediationdomain.VerificationStore
	verificationLifecycle incidentVerificationLifecycle
	verificationExecutor  verificationExecutor
	logger                *slog.Logger
	now                   func() time.Time
	markCycleProgress     func()
	markSuccessfulCycle   func()
	recordCycleResult     func(string)
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

type storedApprovalStatus uint8

const (
	storedApprovalDisabled storedApprovalStatus = iota
	storedApprovalGranted
	storedApprovalWaiting
)

type storedApprovalCheck struct {
	Status storedApprovalStatus
	Plan   approvaldomain.Plan
}

func (agent *sreAgent) checkStoredApproval(
	ctx context.Context,
	incidentID string,
	action string,
	target DecisionTarget,
	now time.Time,
) (storedApprovalCheck, error) {
	if !agent.config.RestartPodApproved ||
		action != string(ActionRestartPod) {
		return storedApprovalCheck{
			Status: storedApprovalDisabled,
		}, nil
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
		return storedApprovalCheck{}, fmt.Errorf(
			"create canonical remediation plan: %w",
			err,
		)
	}

	if agent.approvals == nil {
		return storedApprovalCheck{}, errors.New(
			"approval store is not configured",
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
		return storedApprovalCheck{
			Status: storedApprovalWaiting,
			Plan:   plan,
		}, nil
	}
	if err != nil {
		return storedApprovalCheck{}, fmt.Errorf(
			"lookup remediation approval: %w",
			err,
		)
	}

	if err := approvaldomain.Validate(
		granted,
		plan,
		now,
	); err != nil {
		if errors.Is(err, approvaldomain.ErrApprovalExpired) ||
			errors.Is(err, approvaldomain.ErrApprovalNotYetValid) {
			return storedApprovalCheck{
				Status: storedApprovalWaiting,
				Plan:   plan,
			}, nil
		}

		return storedApprovalCheck{}, fmt.Errorf(
			"validate remediation approval: %w",
			err,
		)
	}

	return storedApprovalCheck{
		Status: storedApprovalGranted,
		Plan:   plan,
	}, nil
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

	if currentIncident.State == incident.StateWaitingApproval {
		agent.resumeWaitingApproval(
			ctx,
			incidentID,
			currentIncident,
			podEvidence,
			remediationState,
			targetKey,
			targetLabel,
			now,
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
	approvalCheck, approvalErr := agent.checkStoredApproval(
		ctx,
		currentIncident.ID,
		string(decision.Action),
		podEvidence.Target,
		agent.now(),
	)
	approvalCheckDuration := agent.now().Sub(
		approvalCheckStartedAt,
	)

	if approvalErr != nil {
		if ctx.Err() != nil {
			return
		}

		agent.logger.Warn(
			"approval_check_failed",
			"incident_id", currentIncident.ID,
			"action", decision.Action,
			"target", targetLabel,
			"result", "NO_ACTION",
			"duration_ms",
			approvalCheckDuration.Milliseconds(),
			"error_code", "APPROVAL_CHECK_FAILED",
			"error", approvalErr,
		)
		return
	}

	approvalGranted :=
		approvalCheck.Status == storedApprovalGranted

	policyContext := PolicyContext{
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
	}
	policyResult := EvaluateDecision(
		decision,
		policyContext,
	)

	if approvalCheck.Status == storedApprovalWaiting {
		eligibilityContext := policyContext
		eligibilityContext.ApprovalGranted = true

		eligibilityResult := EvaluateDecision(
			decision,
			eligibilityContext,
		)
		if !eligibilityResult.Allowed {
			policyResult = eligibilityResult
		} else {
			if !claimEnabled {
				agent.logger.Warn(
					"incident_transition_failed",
					"incident_id", currentIncident.ID,
					"action", decision.Action,
					"target", targetLabel,
					"result", "NO_ACTION",
					"duration_ms",
					approvalCheckDuration.Milliseconds(),
					"error_code",
					"INCIDENT_WAITING_APPROVAL_FENCE_UNAVAILABLE",
				)
				return
			}

			planPublishStartedAt := agent.now()
			planContext, cancelPlan := context.WithTimeout(
				ctx,
				agent.config.KubernetesRequestTimeout,
			)
			planPublishErr := agent.publishCanonicalPlan(
				planContext,
				approvalCheck.Plan,
			)
			cancelPlan()
			planPublishDuration := agent.now().Sub(
				planPublishStartedAt,
			)

			if planPublishErr != nil {
				if ctx.Err() != nil {
					return
				}

				agent.logger.Warn(
					"incident_plan_publish_failed",
					"incident_id", currentIncident.ID,
					"action", decision.Action,
					"target", targetLabel,
					"result", "NO_ACTION",
					"duration_ms",
					planPublishDuration.Milliseconds(),
					"error_code",
					"INCIDENT_PLAN_PUBLISH_FAILED",
					"error", planPublishErr,
				)
				return
			}
			transitionStartedAt := agent.now()
			waitingIncident, transitionErr :=
				agent.incidents.Transition(
					ctx,
					incident.TransitionCommand{
						IncidentID: currentIncident.ID,
						ExpectedVersion: currentIncident.
							Version,
						To: incident.
							StateWaitingApproval,
						Actor: agent.config.
							IncidentClaimHolderID,
						ReasonCode: PolicyCodeApprovalRequired,
						ApprovalBinding: incident.
							ApprovalBinding{
							PlanHash: approvalCheck.
								Plan.Hash,
							TargetUID: approvalCheck.
								Plan.Target.UID,
						},
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
					"action", decision.Action,
					"target", targetLabel,
					"result", "NO_ACTION",
					"duration_ms",
					transitionDuration.Milliseconds(),
					"error_code",
					"INCIDENT_WAITING_APPROVAL_TRANSITION_FAILED",
					"error", transitionErr,
				)
				return
			}

			agent.logger.Info(
				"incident_waiting_approval",
				"incident_id", waitingIncident.ID,
				"incident_version",
				waitingIncident.Version,
				"action", decision.Action,
				"target", targetLabel,
				"target_uid",
				approvalCheck.Plan.Target.UID,
				"plan_hash", approvalCheck.Plan.Hash,
				"result", "WAITING_APPROVAL",
				"duration_ms",
				transitionDuration.Milliseconds(),
				"error_code",
				PolicyCodeApprovalRequired,
			)
			return
		}
	}

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
	verificationSubject := verificationSubjectForPod(
		podEvidence,
	)

	actionAttempt, shouldExecute := agent.beginActionAttempt(
		ctx,
		currentIncident,
		approvalCheck.Plan,
		verificationSubject,
		targetLabel,
	)
	if !shouldExecute {
		return
	}

	actionContext, cancelAction := context.WithTimeout(
		ctx,
		agent.config.KubernetesRequestTimeout,
	)
	err = executeApprovedAction(
		actionContext,
		agent.kubernetes,
		decision,
	)
	cancelAction()

	completedAttempt, completed :=
		agent.completeActionAttempt(
			ctx,
			actionAttempt,
			decision.Action,
			targetLabel,
			err,
		)
	if !completed {
		return
	}

	_, verifying := agent.beginVerification(
		ctx,
		currentIncident,
		completedAttempt,
		completedAttempt.Key.FencingToken,
		verificationSubject,
		decision.Action,
		targetLabel,
	)
	if !verifying {
		return
	}

	if err != nil {
		agent.logger.Error(
			"remediation_failed",
			"incident_id", incidentID,
			"action", decision.Action,
			"target", targetLabel,
			"attempt_id", completedAttempt.ID,
			"verification_state", "PENDING",
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
		"attempt_id", completedAttempt.ID,
		"fencing_token",
		completedAttempt.Key.FencingToken,
		"verification_state", "PENDING",
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
