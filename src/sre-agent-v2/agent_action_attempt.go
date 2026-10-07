package main

import (
	"context"
	"errors"
	"fmt"

	approvaldomain "sre-agent/internal/approval"
	"sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

const (
	actionAttemptPreviousExecutorLost = "PREVIOUS_EXECUTOR_LOST"
	actionAttemptResultPersistFailed  = "ACTION_ATTEMPT_RESULT_PERSIST_FAILED"
)

func (agent *sreAgent) beginActionAttempt(
	ctx context.Context,
	currentIncident incident.Incident,
	plan approvaldomain.Plan,
	verificationSubject remediationdomain.VerificationSubject,
	targetLabel string,
) (remediationdomain.ActionAttempt, bool) {
	startedAt := agent.now()

	fail := func(
		err error,
	) (remediationdomain.ActionAttempt, bool) {
		if ctx.Err() != nil {
			return remediationdomain.ActionAttempt{}, false
		}

		agent.logger.Warn(
			"action_attempt_begin_failed",
			"incident_id", currentIncident.ID,
			"action", plan.Action,
			"target", targetLabel,
			"result", "NO_ACTION",
			"duration_ms",
			agent.now().Sub(startedAt).Milliseconds(),
			"error_code", "ACTION_ATTEMPT_BEGIN_FAILED",
			"error", err,
		)

		return remediationdomain.ActionAttempt{}, false
	}

	if agent.actionAttempts == nil {
		return fail(errors.New(
			"action attempt store is not configured",
		))
	}
	if agent.plans == nil {
		return fail(errors.New(
			"canonical plan store is not configured",
		))
	}
	if plan.IncidentID != currentIncident.ID {
		return fail(fmt.Errorf(
			"canonical plan incident %q does not match current incident %q",
			plan.IncidentID,
			currentIncident.ID,
		))
	}

	// Publishing is idempotent. It also guarantees that the PostgreSQL
	// ActionAttempt foreign key can bind to the exact canonical Plan.
	planContext, cancelPlan := context.WithTimeout(
		ctx,
		agent.config.KubernetesRequestTimeout,
	)
	err := agent.publishCanonicalPlan(
		planContext,
		plan,
	)
	cancelPlan()
	if err != nil {
		return fail(err)
	}

	actionFenceStartedAt := agent.now()
	actionClaim, err := agent.incidents.Claim(
		ctx,
		incident.ClaimCommand{
			IncidentID:      currentIncident.ID,
			ExpectedVersion: currentIncident.Version,
			HolderID: agent.config.
				IncidentClaimHolderID,
			Now: agent.now(),
			LeaseDuration: agent.config.
				IncidentClaimLeaseDuration,
		},
	)
	actionFenceDuration := agent.now().Sub(
		actionFenceStartedAt,
	)

	if err != nil {
		if ctx.Err() != nil {
			return remediationdomain.ActionAttempt{}, false
		}

		agent.logger.Warn(
			"incident_action_fence_failed",
			"incident_id", currentIncident.ID,
			"action", plan.Action,
			"target", targetLabel,
			"result", "NO_ACTION",
			"duration_ms",
			actionFenceDuration.Milliseconds(),
			"error_code", "INCIDENT_ACTION_FENCE_FAILED",
			"error", err,
		)

		return remediationdomain.ActionAttempt{}, false
	}

	executionKey := remediationdomain.ExecutionKey{
		IncidentID: currentIncident.ID,
		PlanHash:   plan.Hash,
		TargetUID:  plan.Target.UID,
		FencingToken: actionClaim.
			Incident.Version,
	}

	attemptContext, cancelAttempt := context.WithTimeout(
		ctx,
		agent.config.KubernetesRequestTimeout,
	)
	attempt, created, err := agent.actionAttempts.Begin(
		attemptContext,
		remediationdomain.BeginActionAttemptCommand{
			Key:       executionKey,
			StartedAt: agent.now(),
		},
	)
	cancelAttempt()

	if err != nil {
		return fail(fmt.Errorf(
			"begin persistent action attempt: %w",
			err,
		))
	}

	if !created {
		if attempt.Status ==
			remediationdomain.ActionAttemptStatusStarted &&
			executionKey.FencingToken >
				attempt.Key.FencingToken {
			recoveryStartedAt := agent.now()

			recoveryContext, cancelRecovery :=
				context.WithTimeout(
					ctx,
					agent.config.
						KubernetesRequestTimeout,
				)
			recovered, changed, recoveryErr :=
				agent.actionAttempts.Recover(
					recoveryContext,
					remediationdomain.
						RecoverActionAttemptCommand{
						Key: executionKey,
						RecoveredAt: agent.
							now(),
						ReasonCode: actionAttemptPreviousExecutorLost,
					},
				)
			cancelRecovery()

			if recoveryErr != nil {
				if ctx.Err() != nil {
					return remediationdomain.
							ActionAttempt{},
						false
				}

				agent.logger.Warn(
					"action_attempt_recovery_failed",
					"incident_id",
					currentIncident.ID,
					"attempt_id", attempt.ID,
					"action", plan.Action,
					"target", targetLabel,
					"target_uid",
					plan.Target.UID,
					"plan_hash", plan.Hash,
					"owner_fencing_token",
					attempt.Key.FencingToken,
					"recovery_fencing_token",
					executionKey.FencingToken,
					"result", "NO_ACTION",
					"duration_ms",
					agent.now().
						Sub(recoveryStartedAt).
						Milliseconds(),
					"error_code",
					"ACTION_ATTEMPT_RECOVERY_FAILED",
					"error", recoveryErr,
				)

				return remediationdomain.
						ActionAttempt{},
					false
			}

			agent.logger.Warn(
				"action_attempt_recovered",
				"incident_id", currentIncident.ID,
				"attempt_id", recovered.ID,
				"action", plan.Action,
				"target", targetLabel,
				"target_uid", plan.Target.UID,
				"plan_hash", plan.Hash,
				"owner_fencing_token",
				recovered.Key.FencingToken,
				"recovery_fencing_token",
				recovered.
					RecoveredByFencingToken,
				"attempt_version",
				recovered.Version,
				"changed", changed,
				"result", "UNKNOWN",
				"duration_ms",
				agent.now().
					Sub(recoveryStartedAt).
					Milliseconds(),
				"error_code",
				actionAttemptPreviousExecutorLost,
			)

			_, verifying := agent.beginVerification(
				ctx,
				actionClaim.Incident,
				recovered,
				executionKey.FencingToken,
				verificationSubject,
				plan.Action,
				targetLabel,
			)
			if !verifying {
				return recovered, false
			}

			return recovered, false
		}

		if attempt.Status !=
			remediationdomain.ActionAttemptStatusStarted {
			_, verifying := agent.beginVerification(
				ctx,
				actionClaim.Incident,
				attempt,
				executionKey.FencingToken,
				verificationSubject,
				plan.Action,
				targetLabel,
			)
			if !verifying {
				return attempt, false
			}
		}

		agent.logger.Info(
			"action_attempt_skipped",
			"incident_id", currentIncident.ID,
			"attempt_id", attempt.ID,
			"action", plan.Action,
			"target", targetLabel,
			"target_uid", plan.Target.UID,
			"plan_hash", plan.Hash,
			"fencing_token",
			attempt.Key.FencingToken,
			"attempt_status", attempt.Status,
			"attempt_version", attempt.Version,
			"result", "NO_ACTION",
			"duration_ms",
			agent.now().Sub(startedAt).Milliseconds(),
			"error_code", "DUPLICATE_ACTION_ATTEMPT",
		)

		return attempt, false
	}
	agent.logger.Info(
		"action_attempt_started",
		"incident_id", currentIncident.ID,
		"attempt_id", attempt.ID,
		"action", plan.Action,
		"target", targetLabel,
		"target_uid", plan.Target.UID,
		"plan_hash", plan.Hash,
		"fencing_token", attempt.Key.FencingToken,
		"attempt_version", attempt.Version,
		"result", "STARTED",
		"duration_ms",
		agent.now().Sub(startedAt).Milliseconds(),
		"error_code", "",
	)

	return attempt, true
}

func (agent *sreAgent) completeActionAttempt(
	ctx context.Context,
	attempt remediationdomain.ActionAttempt,
	action string,
	targetLabel string,
	actionErr error,
) (remediationdomain.ActionAttempt, bool) {
	startedAt := agent.now()

	status := remediationdomain.ActionAttemptStatusSucceeded
	result := "SUCCEEDED"
	errorCode := ""

	if actionErr != nil {
		status = remediationdomain.ActionAttemptStatusFailed
		result = "FAILED"
		errorCode = "KUBERNETES_ACTION_FAILED"
	}

	if agent.actionAttempts == nil {
		agent.logger.Error(
			"action_attempt_result_persist_failed",
			"incident_id", attempt.Key.IncidentID,
			"attempt_id", attempt.ID,
			"action", action,
			"target", targetLabel,
			"target_uid", attempt.Key.TargetUID,
			"plan_hash", attempt.Key.PlanHash,
			"fencing_token", attempt.Key.FencingToken,
			"attempt_version", attempt.Version,
			"action_result", result,
			"result", "ERROR",
			"duration_ms",
			agent.now().Sub(startedAt).Milliseconds(),
			"error_code",
			actionAttemptResultPersistFailed,
			"error",
			"action attempt store is not configured",
		)

		return remediationdomain.ActionAttempt{}, false
	}

	resultContext, cancelResult := context.WithTimeout(
		ctx,
		agent.config.KubernetesRequestTimeout,
	)
	completed, err := agent.actionAttempts.Complete(
		resultContext,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             attempt.Key,
			ExpectedVersion: attempt.Version,
			To:              status,
			FinishedAt:      agent.now(),
			ErrorCode:       errorCode,
		},
	)
	cancelResult()

	if err != nil {
		if ctx.Err() != nil {
			return remediationdomain.ActionAttempt{}, false
		}

		agent.logger.Error(
			"action_attempt_result_persist_failed",
			"incident_id", attempt.Key.IncidentID,
			"attempt_id", attempt.ID,
			"action", action,
			"target", targetLabel,
			"target_uid", attempt.Key.TargetUID,
			"plan_hash", attempt.Key.PlanHash,
			"fencing_token", attempt.Key.FencingToken,
			"attempt_version", attempt.Version,
			"action_result", result,
			"action_error_code", errorCode,
			"result", "ERROR",
			"duration_ms",
			agent.now().Sub(startedAt).Milliseconds(),
			"error_code",
			actionAttemptResultPersistFailed,
			"error", err,
		)

		return remediationdomain.ActionAttempt{}, false
	}

	agent.logger.Info(
		"action_attempt_completed",
		"incident_id", completed.Key.IncidentID,
		"attempt_id", completed.ID,
		"action", action,
		"target", targetLabel,
		"target_uid", completed.Key.TargetUID,
		"plan_hash", completed.Key.PlanHash,
		"fencing_token", completed.Key.FencingToken,
		"attempt_status", completed.Status,
		"attempt_version", completed.Version,
		"result", result,
		"duration_ms",
		agent.now().Sub(startedAt).Milliseconds(),
		"error_code", errorCode,
	)

	return completed, true
}
