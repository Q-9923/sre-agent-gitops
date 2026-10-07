package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	approvaldomain "sre-agent/internal/approval"
	"sre-agent/internal/incident"
)

func (agent *sreAgent) publishCanonicalPlan(
	ctx context.Context,
	plan approvaldomain.Plan,
) error {
	if agent.plans == nil {
		return errors.New("canonical plan store is not configured")
	}

	if err := agent.plans.Publish(ctx, plan); err != nil {
		return fmt.Errorf("publish canonical remediation plan: %w", err)
	}

	return nil
}

func (agent *sreAgent) resumeWaitingApproval(
	ctx context.Context,
	remediationIncidentID string,
	currentIncident incident.Incident,
	podEvidence PodEvidence,
	remediationState remediationSnapshot,
	targetKey string,
	targetLabel string,
	now time.Time,
) {
	planRecoveryStartedAt := agent.now()

	plan, err := agent.recoverWaitingApprovalPlan(
		ctx,
		currentIncident,
		podEvidence,
	)
	planRecoveryDuration := agent.now().Sub(planRecoveryStartedAt)

	if err != nil {
		if ctx.Err() != nil {
			return
		}

		agent.logger.Warn(
			"incident_plan_recovery_failed",
			"incident_id", currentIncident.ID,
			"action", "RECOVER_PLAN",
			"target", targetLabel,
			"result", "NO_ACTION",
			"duration_ms", planRecoveryDuration.Milliseconds(),
			"error_code", "INCIDENT_PLAN_RECOVERY_FAILED",
			"error", err,
		)
		return
	}

	approvalCheckStartedAt := agent.now()
	approvalGranted, err := agent.waitingApprovalGranted(
		ctx,
		plan,
		now,
	)
	approvalCheckDuration := agent.now().Sub(approvalCheckStartedAt)

	if err != nil {
		if ctx.Err() != nil {
			return
		}

		agent.logger.Warn(
			"approval_check_failed",
			"incident_id", currentIncident.ID,
			"action", plan.Action,
			"target", targetLabel,
			"result", "NO_ACTION",
			"duration_ms", approvalCheckDuration.Milliseconds(),
			"error_code", "APPROVAL_CHECK_FAILED",
			"error", err,
		)
		return
	}

	if !approvalGranted {
		agent.logger.Info(
			"incident_waiting_approval",
			"incident_id", currentIncident.ID,
			"incident_version", currentIncident.Version,
			"action", plan.Action,
			"target", targetLabel,
			"target_uid", plan.Target.UID,
			"plan_hash", plan.Hash,
			"result", "WAITING_APPROVAL",
			"duration_ms", approvalCheckDuration.Milliseconds(),
			"error_code", PolicyCodeApprovalRequired,
		)
		return
	}

	if policyCode := agent.waitingApprovalPolicyCode(
		plan,
		podEvidence,
		remediationState,
	); policyCode != "" {
		agent.logger.Info(
			"decision_denied",
			"incident_id", currentIncident.ID,
			"action", plan.Action,
			"target", targetLabel,
			"result", "NO_ACTION",
			"error_code", policyCode,
		)
		return
	}
	//Claim 配置检查
	if agent.config.IncidentClaimHolderID == "" ||
		agent.config.IncidentClaimLeaseDuration <= 0 {
		agent.logger.Warn(
			"incident_action_fence_failed",
			"incident_id", currentIncident.ID,
			"action", plan.Action,
			"target", targetLabel,
			"result", "NO_ACTION",
			"duration_ms", int64(0),
			"error_code", "INCIDENT_ACTION_FENCE_UNAVAILABLE",
		)
		return
	}

	// Count the authorized attempt before calling Kubernetes. A timeout or an
	// ambiguous Kubernetes response must not allow an immediate second delete.
	recordContext, cancelRecord := context.WithTimeout(
		ctx,
		agent.config.KubernetesRequestTimeout,
	)
	err = agent.memory.Record(
		recordContext,
		remediationStateRecord{
			Kind:       remediationStateRecordActionAttempted,
			IncidentID: remediationIncidentID,
			TargetKey:  targetKey,
			OccurredAt: now,
		},
	)
	cancelRecord()

	if err != nil {
		agent.logger.Error(
			"remediation_state_record_failed",
			"incident_id", remediationIncidentID,
			"action", plan.Action,
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
		plan,
		verificationSubject,
		targetLabel,
	)
	if !shouldExecute {
		return
	}

	agent.logger.Info(
		"incident_approval_resumed",
		"incident_id", currentIncident.ID,
		"incident_version", actionAttempt.Key.FencingToken,
		"attempt_id", actionAttempt.ID,
		"action", plan.Action,
		"target", targetLabel,
		"target_uid", plan.Target.UID,
		"plan_hash", plan.Hash,
		"result", "APPROVED",
		"error_code", "",
	)

	actionContext, cancelAction := context.WithTimeout(
		ctx,
		agent.config.KubernetesRequestTimeout,
	)
	err = agent.executeApprovedPlan(
		actionContext,
		plan,
	)
	cancelAction()

	completedAttempt, completed :=
		agent.completeActionAttempt(
			ctx,
			actionAttempt,
			plan.Action,
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
		plan.Action,
		targetLabel,
	)
	if !verifying {
		return
	}
	if err != nil {
		agent.logger.Error(
			"remediation_failed",
			"incident_id", remediationIncidentID,
			"action", plan.Action,
			"target", targetLabel,
			"result", "ERROR",
			"error_code", "KUBERNETES_ACTION_FAILED",
			"error", err,
		)
		return
	}

	agent.logger.Info(
		"remediation_submitted",
		"incident_id", remediationIncidentID,
		"action", plan.Action,
		"target", targetLabel,
		"attempt_id", completedAttempt.ID,
		"fencing_token", completedAttempt.Key.FencingToken,
		"verification_state", "PENDING",
		"result", "SUBMITTED",
		"error_code", "",
	)

}

func (agent *sreAgent) recoverWaitingApprovalPlan(
	ctx context.Context,
	currentIncident incident.Incident,
	podEvidence PodEvidence,
) (approvaldomain.Plan, error) {
	if agent.plans == nil {
		return approvaldomain.Plan{},
			errors.New("canonical plan store is not configured")
	}

	if currentIncident.State != incident.StateWaitingApproval {
		return approvaldomain.Plan{}, fmt.Errorf(
			"incident %q is in state %q, not WAITING_APPROVAL",
			currentIncident.ID,
			currentIncident.State,
		)
	}

	binding := currentIncident.ApprovalBinding
	key := approvaldomain.PlanKey{
		IncidentID: currentIncident.ID,
		Hash:       binding.PlanHash,
		TargetUID:  binding.TargetUID,
	}

	plan, err := agent.plans.Lookup(ctx, key)
	if err != nil {
		return approvaldomain.Plan{}, fmt.Errorf(
			"lookup canonical remediation plan: %w",
			err,
		)
	}

	if err := approvaldomain.ValidatePlan(plan); err != nil {
		return approvaldomain.Plan{}, fmt.Errorf(
			"validate recovered canonical plan: %w",
			err,
		)
	}

	expectedTarget := approvaldomain.Target{
		Cluster:   podEvidence.Target.Cluster,
		Namespace: podEvidence.Target.Namespace,
		Kind:      podEvidence.Target.Kind,
		Name:      podEvidence.Target.Name,
		UID:       podEvidence.Target.UID,
	}

	if plan.IncidentID != currentIncident.ID ||
		plan.Hash != binding.PlanHash ||
		plan.Target.UID != binding.TargetUID ||
		plan.Target != expectedTarget ||
		plan.Action != string(ActionRestartPod) {
		return approvaldomain.Plan{}, errors.New(
			"recovered canonical plan does not match the incident binding and current target",
		)
	}

	return plan, nil
}

func (agent *sreAgent) waitingApprovalGranted(
	ctx context.Context,
	plan approvaldomain.Plan,
	now time.Time,
) (bool, error) {
	if agent.approvals == nil {
		return false, errors.New("approval store is not configured")
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

	if err := approvaldomain.Validate(granted, plan, now); err != nil {
		if errors.Is(err, approvaldomain.ErrApprovalExpired) ||
			errors.Is(err, approvaldomain.ErrApprovalNotYetValid) {
			return false, nil
		}

		return false, fmt.Errorf(
			"validate remediation approval: %w",
			err,
		)
	}

	return true, nil
}

func (agent *sreAgent) waitingApprovalPolicyCode(
	plan approvaldomain.Plan,
	podEvidence PodEvidence,
	remediationState remediationSnapshot,
) string {
	if !agent.config.RestartPodApproved {
		return PolicyCodeApprovalRequired
	}

	if !containsString(
		agent.config.AllowedNamespaces,
		plan.Target.Namespace,
	) {
		return PolicyCodeNamespaceDenied
	}

	if !containsString(
		agent.config.AllowedControllerKinds,
		podEvidence.Owner.Kind,
	) {
		return PolicyCodeOwnerDenied
	}

	if remediationState.Duplicate {
		return PolicyCodeDuplicate
	}

	if remediationState.InCooldown {
		return PolicyCodeCooldown
	}

	if agent.config.MaximumAttempts <= 0 ||
		remediationState.AttemptsInWindow >=
			agent.config.MaximumAttempts {
		return PolicyCodeAttemptLimit
	}

	return ""
}

func (agent *sreAgent) executeApprovedPlan(
	ctx context.Context,
	plan approvaldomain.Plan,
) error {
	if agent.kubernetes == nil {
		return errors.New("Kubernetes client is not configured")
	}

	if err := approvaldomain.ValidatePlan(plan); err != nil {
		return fmt.Errorf("refuse invalid canonical plan: %w", err)
	}

	if plan.Action != string(ActionRestartPod) {
		return fmt.Errorf(
			"refuse unsupported executable action %q",
			plan.Action,
		)
	}

	deleteOptions := metav1.NewPreconditionDeleteOptions(
		plan.Target.UID,
	)
	if err := agent.kubernetes.
		CoreV1().
		Pods(plan.Target.Namespace).
		Delete(
			ctx,
			plan.Target.Name,
			*deleteOptions,
		); err != nil {
		return fmt.Errorf(
			"delete approved plan target %s/%s: %w",
			plan.Target.Namespace,
			plan.Target.Name,
			err,
		)
	}

	return nil
}
