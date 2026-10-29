package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

var errVerificationWorkerConfiguration = errors.New(
	"verification worker configuration is invalid",
)

const (
	verificationRecoveredReasonCode = "VERIFICATION_RECOVERED"

	verificationWorkerMaxBackoffMultiplier = 8
	verificationWorkerMaxDrainItems        = 8
)

type verificationWorkerFailure struct {
	errorCode       string
	action          string
	incidentID      string
	verificationID  string
	actionAttemptID string
	target          string
	cause           error
}

func (failure *verificationWorkerFailure) Error() string {
	if failure == nil || failure.cause == nil {
		return "verification worker failed"
	}

	return failure.cause.Error()
}

func (failure *verificationWorkerFailure) Unwrap() error {
	if failure == nil {
		return nil
	}

	return failure.cause
}

func newVerificationWorkerFailureForWork(
	errorCode string,
	action string,
	incident incidentdomain.Incident,
	verification remediationdomain.Verification,
	cause error,
) *verificationWorkerFailure {
	return &verificationWorkerFailure{
		errorCode:       errorCode,
		action:          action,
		incidentID:      incident.ID,
		verificationID:  verification.ID,
		actionAttemptID: verification.ActionAttemptID,
		target: fmt.Sprintf(
			"%s/%s",
			verification.Subject.Namespace,
			verification.Subject.Name,
		),
		cause: cause,
	}
}

type verificationWorkerLifecycle interface {
	ClaimRecoveredVerificationResolution(
		context.Context,
		incidentdomain.
			ClaimRecoveredVerificationResolutionCommand,
	) (
		incidentdomain.RecoveredVerificationResolutionClaim,
		bool,
		error,
	)

	ClaimPendingVerification(
		context.Context,
		incidentdomain.ClaimPendingVerificationCommand,
	) (incidentdomain.PendingVerificationClaim, bool, error)

	CompleteFencedVerification(
		context.Context,
		incidentdomain.CompleteFencedVerificationCommand,
	) (
		incidentdomain.FencedVerificationCompletionResult,
		error,
	)

	Resolve(
		context.Context,
		incidentdomain.ResolveCommand,
	) (incidentdomain.Incident, error)
}

func (agent *sreAgent) runVerificationWorkerOnce(
	ctx context.Context,
) (incidentdomain.Incident, bool, error) {
	if agent.verificationWorker == nil {
		return incidentdomain.Incident{},
			false,
			fmt.Errorf(
				"%w: verification worker lifecycle is not configured",
				errVerificationWorkerConfiguration,
			)
	}
	if agent.now == nil {
		return incidentdomain.Incident{},
			false,
			fmt.Errorf(
				"%w: verification worker clock is not configured",
				errVerificationWorkerConfiguration,
			)
	}
	if strings.TrimSpace(agent.config.IncidentClaimHolderID) == "" {
		return incidentdomain.Incident{},
			false,
			fmt.Errorf(
				"%w: verification worker claim holder ID is required",
				errVerificationWorkerConfiguration,
			)
	}
	if agent.config.IncidentClaimLeaseDuration <= 0 {
		return incidentdomain.Incident{},
			false,
			fmt.Errorf(
				"%w: verification worker claim lease duration "+
					"must be greater than zero",
				errVerificationWorkerConfiguration,
			)
	}

	var recoveredResolutionErr error

	resolutionClaimedAt := agent.now()

	resolutionWork, resolutionClaimed, err :=
		agent.verificationWorker.
			ClaimRecoveredVerificationResolution(
				ctx,
				incidentdomain.
					ClaimRecoveredVerificationResolutionCommand{
					HolderID: agent.config.
						IncidentClaimHolderID,
					Now: resolutionClaimedAt,
					LeaseDuration: agent.config.
						IncidentClaimLeaseDuration,
				},
			)
	if err != nil {
		cause := fmt.Errorf(
			"claim recovered verification resolution: %w",
			err,
		)

		recoveredResolutionErr = &verificationWorkerFailure{
			errorCode: "VERIFICATION_WORKER_" +
				"RECOVERED_RESOLUTION_CLAIM_FAILED",
			action: "CLAIM_RECOVERED_" +
				"VERIFICATION_RESOLUTION",
			cause: cause,
		}
	}

	if err == nil && resolutionClaimed {
		resolved, err := agent.verificationWorker.Resolve(
			ctx,
			incidentdomain.ResolveCommand{
				IncidentID: resolutionWork.Claim.Incident.ID,
				ExpectedVersion: resolutionWork.Claim.
					Incident.Version,
				VerificationID: resolutionWork.Verification.ID,
				HolderID: agent.config.
					IncidentClaimHolderID,
				Now: agent.now(),
				Actor: agent.config.
					IncidentClaimHolderID,
				ReasonCode: verificationRecoveredReasonCode,
			},
		)
		if err != nil {
			cause := fmt.Errorf(
				"resolve recovered verification: %w",
				err,
			)

			recoveredResolutionErr = newVerificationWorkerFailureForWork(
				"VERIFICATION_WORKER_RECOVERED_RESOLUTION_FAILED",
				"RESOLVE_RECOVERED_VERIFICATION",
				resolutionWork.Claim.Incident,
				resolutionWork.Verification,
				cause,
			)
		} else {
			return resolved, true, nil
		}
	}

	if agent.verificationExecutor == nil {
		configurationErr := fmt.Errorf(
			"%w: verification executor is not configured",
			errVerificationWorkerConfiguration,
		)

		if recoveredResolutionErr != nil {
			return incidentdomain.Incident{}, false, recoveredResolutionErr
		}

		return incidentdomain.Incident{}, false, configurationErr
	}
	if agent.config.KubernetesRequestTimeout <= 0 {
		configurationErr := fmt.Errorf(
			"%w: verification execution timeout must be greater than zero",
			errVerificationWorkerConfiguration,
		)

		if recoveredResolutionErr != nil {
			return incidentdomain.Incident{}, false, recoveredResolutionErr
		}

		return incidentdomain.Incident{}, false, configurationErr
	}
	if agent.config.IncidentClaimLeaseDuration <=
		agent.config.KubernetesRequestTimeout {
		configurationErr := fmt.Errorf(
			"%w: verification worker claim lease duration must be greater than verification execution timeout",
			errVerificationWorkerConfiguration,
		)

		if recoveredResolutionErr != nil {
			return incidentdomain.Incident{}, false, recoveredResolutionErr
		}

		return incidentdomain.Incident{}, false, configurationErr
	}

	pendingClaimedAt := agent.now()

	work, claimed, err := agent.verificationWorker.
		ClaimPendingVerification(
			ctx,
			incidentdomain.ClaimPendingVerificationCommand{
				HolderID: agent.config.IncidentClaimHolderID,
				Now:      pendingClaimedAt,
				LeaseDuration: agent.config.
					IncidentClaimLeaseDuration,
			},
		)
	if err != nil {
		cause := fmt.Errorf(
			"claim pending verification: %w",
			err,
		)

		pendingClaimErr := &verificationWorkerFailure{
			errorCode: "VERIFICATION_WORKER_" +
				"PENDING_VERIFICATION_CLAIM_FAILED",
			action: "CLAIM_PENDING_" +
				"VERIFICATION",
			cause: cause,
		}

		return incidentdomain.Incident{},
			false,
			combineVerificationWorkerErrors(
				recoveredResolutionErr,
				pendingClaimErr,
			)
	}
	if !claimed {
		return incidentdomain.Incident{}, false, recoveredResolutionErr
	}

	executionContext, cancel := context.WithTimeout(
		ctx,
		agent.config.KubernetesRequestTimeout,
	)

	executionResult, executionErr :=
		agent.verificationExecutor.Verify(
			executionContext,
			work.Verification,
		)

	executionContextErr := executionContext.Err()
	cancel()

	if executionContextErr != nil {
		cause := fmt.Errorf(
			"verification execution deadline: %w",
			executionContextErr,
		)

		return incidentdomain.Incident{},
			false,
			combineVerificationWorkerErrors(
				recoveredResolutionErr,
				newVerificationWorkerFailureForWork(
					"VERIFICATION_WORKER_EXECUTION_DEADLINE_EXCEEDED",
					"EXECUTE_VERIFICATION",
					work.Claim.Incident,
					work.Verification,
					cause,
				),
			)
	}
	if executionErr != nil {
		cause := fmt.Errorf(
			"execute verification: %w",
			executionErr,
		)

		return incidentdomain.Incident{},
			false,
			combineVerificationWorkerErrors(
				recoveredResolutionErr,
				newVerificationWorkerFailureForWork(
					"VERIFICATION_WORKER_EXECUTION_FAILED",
					"EXECUTE_VERIFICATION",
					work.Claim.Incident,
					work.Verification,
					cause,
				),
			)
	}
	if validationErr := executionResult.validate(); validationErr != nil {
		cause := fmt.Errorf(
			"validate verification execution result: %w",
			validationErr,
		)

		return incidentdomain.Incident{},
			false,
			combineVerificationWorkerErrors(
				recoveredResolutionErr,
				newVerificationWorkerFailureForWork(
					"VERIFICATION_WORKER_EXECUTION_RESULT_INVALID",
					"VALIDATE_VERIFICATION_RESULT",
					work.Claim.Incident,
					work.Verification,
					cause,
				),
			)
	}

	finishedAt := agent.now()
	completion, err := agent.verificationWorker.
		CompleteFencedVerification(
			ctx,
			incidentdomain.CompleteFencedVerificationCommand{
				IncidentID:      work.Claim.Incident.ID,
				ExpectedVersion: work.Claim.Incident.Version,
				HolderID:        agent.config.IncidentClaimHolderID,
				Now:             finishedAt,
				Verification: remediationdomain.
					CompleteVerificationCommand{
					ActionKey: work.Verification.ActionKey,
					ExpectedVersion: work.Verification.
						Version,
					To:         executionResult.Status,
					FinishedAt: finishedAt,
					EvidenceCode: executionResult.
						EvidenceCode,
				},
			},
		)
	if err != nil {
		cause := fmt.Errorf(
			"complete fenced verification: %w",
			err,
		)

		return incidentdomain.Incident{},
			false,
			combineVerificationWorkerErrors(
				recoveredResolutionErr,
				newVerificationWorkerFailureForWork(
					"VERIFICATION_WORKER_FENCED_COMPLETION_FAILED",
					"COMPLETE_FENCED_VERIFICATION",
					work.Claim.Incident,
					work.Verification,
					cause,
				),
			)
	}

	if completion.Verification.Status !=
		remediationdomain.VerificationStatusRecovered {
		return completion.Incident, true, recoveredResolutionErr
	}

	resolved, err := agent.verificationWorker.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      completion.Incident.ID,
			ExpectedVersion: completion.Incident.Version,
			VerificationID:  completion.Verification.ID,
			HolderID:        agent.config.IncidentClaimHolderID,
			Now:             agent.now(),
			Actor:           agent.config.IncidentClaimHolderID,
			ReasonCode:      verificationRecoveredReasonCode,
		},
	)
	if err != nil {
		cause := fmt.Errorf(
			"resolve completed recovered verification: %w",
			err,
		)

		return incidentdomain.Incident{},
			false,
			combineVerificationWorkerErrors(
				recoveredResolutionErr,
				newVerificationWorkerFailureForWork(
					"VERIFICATION_WORKER_RECOVERED_RESOLUTION_FAILED",
					"RESOLVE_RECOVERED_VERIFICATION",
					work.Claim.Incident,
					work.Verification,
					cause,
				),
			)
	}

	return resolved, true, recoveredResolutionErr
}

func combineVerificationWorkerErrors(
	first error,
	second error,
) error {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	default:
		return errors.Join(first, second)
	}
}
func visitVerificationWorkerFailures(
	err error,
	visit func(error),
) {
	if err == nil || visit == nil {
		return
	}

	joined, ok := err.(interface {
		Unwrap() []error
	})
	if !ok {
		visit(err)
		return
	}

	for _, child := range joined.Unwrap() {
		visitVerificationWorkerFailures(child, visit)
	}
}

func verificationWorkerBackoffDelay(
	baseDelay time.Duration,
	consecutiveFailures int,
) time.Duration {
	if consecutiveFailures <= 1 {
		return baseDelay
	}

	multiplier := 1
	for failure := 1; failure < consecutiveFailures &&
		multiplier < verificationWorkerMaxBackoffMultiplier; failure++ {
		multiplier *= 2
	}

	maxDuration := time.Duration(1<<63 - 1)
	if baseDelay > maxDuration/time.Duration(multiplier) {
		return maxDuration
	}

	return baseDelay * time.Duration(multiplier)
}

func (agent *sreAgent) waitForVerificationWorker(
	ctx context.Context,
	delay time.Duration,
) error {
	if agent.waitVerificationWorker != nil {
		return agent.waitVerificationWorker(ctx, delay)
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()

	case <-timer.C:
		return nil
	}
}

func (agent *sreAgent) runVerificationWorker(
	ctx context.Context,
) {
	if ctx.Err() != nil {
		return
	}

	if agent.config.PollInterval <= 0 {
		startedAt := time.Now()
		err := fmt.Errorf(
			"%w: verification worker poll interval "+
				"must be greater than zero",
			errVerificationWorkerConfiguration,
		)

		if agent.logger != nil {
			agent.logger.Error(
				"agent_verification_worker_failed",
				"component", "verification_worker",
				"request_id", "",
				"incident_id", "",
				"verification_id", "",
				"action_attempt_id", "",
				"action", "RUN_VERIFICATION_WORKER",
				"target", "",
				"result", "FAILED",
				"duration_ms",
				time.Since(startedAt).Milliseconds(),
				"error_code",
				"VERIFICATION_WORKER_CONFIGURATION_INVALID",
				"error", err,
			)
		}

		return
	}

	logFailure := func(startedAt time.Time, err error) {
		if agent.logger == nil {
			return
		}

		failure := &verificationWorkerFailure{
			errorCode: "VERIFICATION_WORKER_ITERATION_FAILED",
			action:    "RUN_VERIFICATION_WORKER",
			cause:     err,
		}
		if errors.Is(err, errVerificationWorkerConfiguration) {
			failure.errorCode =
				"VERIFICATION_WORKER_CONFIGURATION_INVALID"
		}

		var classifiedFailure *verificationWorkerFailure
		if errors.As(err, &classifiedFailure) {
			failure = classifiedFailure
		}

		agent.logger.Error(
			"agent_verification_worker_failed",
			"component", "verification_worker",
			"request_id", "",
			"incident_id", failure.incidentID,
			"verification_id", failure.verificationID,
			"action_attempt_id", failure.actionAttemptID,
			"action", failure.action,
			"target", failure.target,
			"result", "FAILED",
			"duration_ms", time.Since(startedAt).Milliseconds(),
			"error_code", failure.errorCode,
			"error", err,
		)
	}

	consecutiveFailures := 0

	for {
		startedAt := time.Now()
		var iterationErr error

		for item := 0; item < verificationWorkerMaxDrainItems; item++ {
			_, didWork, err := agent.runVerificationWorkerOnce(ctx)

			if ctxErr := ctx.Err(); ctxErr != nil {
				if err == nil || errors.Is(err, ctxErr) {
					return
				}
			}

			iterationErr = combineVerificationWorkerErrors(
				iterationErr,
				err,
			)
			if !didWork {
				break
			}
		}

		if ctx.Err() != nil {
			return
		}

		if iterationErr != nil {
			visitVerificationWorkerFailures(
				iterationErr,
				func(err error) {
					logFailure(startedAt, err)
				},
			)
			consecutiveFailures++
		} else {
			consecutiveFailures = 0
		}

		delay := verificationWorkerBackoffDelay(
			agent.config.PollInterval,
			consecutiveFailures,
		)

		if err := agent.waitForVerificationWorker(ctx, delay); err != nil {
			return
		}
	}
}
