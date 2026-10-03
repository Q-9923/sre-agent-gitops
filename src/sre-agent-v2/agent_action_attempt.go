package main

import (
	"context"
	"errors"
	"fmt"

	approvaldomain "sre-agent/internal/approval"
	"sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func (agent *sreAgent) beginActionAttempt(
	ctx context.Context,
	currentIncident incident.Incident,
	plan approvaldomain.Plan,
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
			HolderID:        agent.config.IncidentClaimHolderID,
			Now:             agent.now(),
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

	attemptContext, cancelAttempt := context.WithTimeout(
		ctx,
		agent.config.KubernetesRequestTimeout,
	)
	attempt, created, err := agent.actionAttempts.Begin(
		attemptContext,
		remediationdomain.BeginActionAttemptCommand{
			Key: remediationdomain.ExecutionKey{
				IncidentID: currentIncident.ID,
				PlanHash:   plan.Hash,
				TargetUID:  plan.Target.UID,
				FencingToken: actionClaim.
					Incident.Version,
			},
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
		"result", "STARTED",
		"duration_ms",
		agent.now().Sub(startedAt).Milliseconds(),
		"error_code", "",
	)

	return attempt, true
}
