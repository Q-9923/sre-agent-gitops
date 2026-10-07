package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

const (
	verificationStoreUnavailable = "VERIFICATION_STORE_UNAVAILABLE"

	verificationSubjectInvalid = "VERIFICATION_SUBJECT_INVALID"

	verificationBeginFailed = "VERIFICATION_BEGIN_FAILED"

	verificationNotPending = "VERIFICATION_NOT_PENDING"

	incidentVerifyingTransitionFailed = "INCIDENT_VERIFYING_TRANSITION_FAILED"

	incidentVerifyingFenceConflict = "INCIDENT_VERIFYING_FENCE_CONFLICT"

	verificationTransitionReason = "ACTION_ATTEMPT_TERMINAL"

	verificationExecutorUnavailable = "VERIFICATION_EXECUTOR_UNAVAILABLE"

	verificationExecutionFailed = "VERIFICATION_EXECUTION_FAILED"

	verificationResultInvalid = "VERIFICATION_RESULT_INVALID"

	verificationCompleteFailed = "VERIFICATION_COMPLETE_FAILED"
)

type incidentVerificationLifecycle interface {
	BeginFencedVerification(
		context.Context,
		incident.BeginFencedVerificationCommand,
	) (incident.FencedVerificationResult, error)
}

type verificationExecutionResult struct {
	Status       remediationdomain.VerificationStatus
	EvidenceCode string
}

func (result verificationExecutionResult) validate() error {
	if strings.TrimSpace(result.EvidenceCode) == "" {
		return errors.New(
			"verification evidence code is required",
		)
	}

	switch result.Status {
	case remediationdomain.VerificationStatusRecovered,
		remediationdomain.VerificationStatusNotRecovered,
		remediationdomain.VerificationStatusInconclusive:
		return nil

	default:
		return fmt.Errorf(
			"verification status %q is not terminal",
			result.Status,
		)
	}
}

type verificationExecutor interface {
	Verify(
		context.Context,
		remediationdomain.VerificationSubject,
	) (verificationExecutionResult, error)
}

func verificationSubjectForPod(
	evidence PodEvidence,
) remediationdomain.VerificationSubject {
	return remediationdomain.VerificationSubject{
		Cluster:   evidence.Target.Cluster,
		Namespace: evidence.Target.Namespace,
		Kind:      evidence.Owner.Kind,
		Name:      evidence.Owner.Name,
		UID:       evidence.Owner.UID,
	}
}

func verificationLifecycleErrorCode(err error) string {
	switch {
	case errors.Is(
		err,
		incident.ErrVerificationFenceConflict,
	):
		return incidentVerifyingFenceConflict

	case errors.Is(
		err,
		incident.ErrVersionConflict,
	):
		return incidentVerifyingFenceConflict

	case errors.Is(
		err,
		incident.ErrVerificationLifecycleUnavailable,
	):
		return verificationStoreUnavailable

	case errors.Is(
		err,
		incident.ErrInvalidVerificationIncidentState,
	):
		return incidentVerifyingTransitionFailed

	case errors.Is(
		err,
		incident.ErrInvalidFencedVerification,
	):
		return verificationBeginFailed

	default:
		return verificationBeginFailed
	}
}

func (agent *sreAgent) beginVerification(
	ctx context.Context,
	currentIncident incident.Incident,
	attempt remediationdomain.ActionAttempt,
	expectedIncidentVersion uint64,
	subject remediationdomain.VerificationSubject,
	action string,
	targetLabel string,
) (remediationdomain.Verification, bool) {
	startedAt := agent.now()

	fail := func(
		verification remediationdomain.Verification,
		errorCode string,
		err error,
	) (remediationdomain.Verification, bool) {
		if ctx.Err() != nil {
			return verification, false
		}

		agent.logger.Error(
			"agent_verification_orchestration_failed",
			"incident_id", attempt.Key.IncidentID,
			"attempt_id", attempt.ID,
			"action", action,
			"target", targetLabel,
			"target_uid", attempt.Key.TargetUID,
			"plan_hash", attempt.Key.PlanHash,
			"fencing_token", expectedIncidentVersion,
			"attempt_status", attempt.Status,
			"verification_id", verification.ID,
			"result", "NO_RESOLUTION",
			"duration_ms",
			agent.now().Sub(startedAt).Milliseconds(),
			"error_code", errorCode,
			"error", err,
		)

		return verification, false
	}

	if agent.verificationLifecycle == nil {
		return fail(
			remediationdomain.Verification{},
			verificationStoreUnavailable,
			errors.New(
				"verification lifecycle is not configured",
			),
		)
	}

	if strings.TrimSpace(
		agent.config.IncidentClaimHolderID,
	) == "" {
		return fail(
			remediationdomain.Verification{},
			incidentVerifyingTransitionFailed,
			errors.New(
				"incident claim holder ID is not configured",
			),
		)
	}

	if expectedIncidentVersion == 0 {
		return fail(
			remediationdomain.Verification{},
			incidentVerifyingFenceConflict,
			errors.New(
				"expected incident version must be positive",
			),
		)
	}

	if attempt.Key.IncidentID != currentIncident.ID {
		return fail(
			remediationdomain.Verification{},
			verificationBeginFailed,
			fmt.Errorf(
				"action attempt incident %q does not match current incident %q",
				attempt.Key.IncidentID,
				currentIncident.ID,
			),
		)
	}

	if err := subject.Validate(); err != nil {
		return fail(
			remediationdomain.Verification{},
			verificationSubjectInvalid,
			err,
		)
	}

	beginCommand := remediationdomain.BeginVerificationCommand{
		ActionAttempt: attempt,
		Subject:       subject,
		StartedAt:     startedAt,
	}
	if err := beginCommand.Validate(); err != nil {
		return fail(
			remediationdomain.Verification{},
			verificationBeginFailed,
			err,
		)
	}

	lifecycleContext, cancelLifecycle := context.WithTimeout(
		ctx,
		agent.config.KubernetesRequestTimeout,
	)
	result, err := agent.verificationLifecycle.
		BeginFencedVerification(
			lifecycleContext,
			incident.BeginFencedVerificationCommand{
				IncidentID:      currentIncident.ID,
				ExpectedVersion: expectedIncidentVersion,
				HolderID: agent.config.
					IncidentClaimHolderID,
				Now:          startedAt,
				ReasonCode:   verificationTransitionReason,
				Verification: beginCommand,
			},
		)
	cancelLifecycle()

	if err != nil {
		return fail(
			remediationdomain.Verification{},
			verificationLifecycleErrorCode(err),
			err,
		)
	}

	verification := result.Verification
	verifyingIncident := result.Incident

	if verifyingIncident.ID != currentIncident.ID {
		return fail(
			verification,
			incidentVerifyingTransitionFailed,
			fmt.Errorf(
				"fenced verification returned incident %q; want %q",
				verifyingIncident.ID,
				currentIncident.ID,
			),
		)
	}

	if verifyingIncident.State != incident.StateVerifying {
		return fail(
			verification,
			incidentVerifyingTransitionFailed,
			fmt.Errorf(
				"incident %q has state %q; want %q",
				verifyingIncident.ID,
				verifyingIncident.State,
				incident.StateVerifying,
			),
		)
	}

	if verification.Status !=
		remediationdomain.VerificationStatusPending {
		return fail(
			verification,
			verificationNotPending,
			fmt.Errorf(
				"verification %q has status %q; want %q",
				verification.ID,
				verification.Status,
				remediationdomain.VerificationStatusPending,
			),
		)
	}

	agent.logger.Info(
		"incident_verification_pending",
		"incident_id", verifyingIncident.ID,
		"incident_version", verifyingIncident.Version,
		"attempt_id", attempt.ID,
		"attempt_status", attempt.Status,
		"verification_id", verification.ID,
		"verification_created", result.Created,
		"verification_status", verification.Status,
		"action", action,
		"target", targetLabel,
		"target_uid", attempt.Key.TargetUID,
		"plan_hash", attempt.Key.PlanHash,
		"subject_cluster", subject.Cluster,
		"subject_namespace", subject.Namespace,
		"subject_kind", subject.Kind,
		"subject_name", subject.Name,
		"subject_uid", subject.UID,
		"result", "VERIFYING",
		"duration_ms",
		agent.now().Sub(startedAt).Milliseconds(),
		"error_code", "",
	)

	return verification, true
}

func (agent *sreAgent) executeVerification(
	ctx context.Context,
	verification remediationdomain.Verification,
	action string,
	targetLabel string,
) (remediationdomain.Verification, bool) {
	startedAt := agent.now()

	fail := func(
		errorCode string,
		err error,
	) (remediationdomain.Verification, bool) {
		if ctx.Err() != nil {
			return verification, false
		}

		agent.logger.Error(
			"agent_verification_execution_failed",
			"incident_id", verification.ActionKey.IncidentID,
			"attempt_id", verification.ActionAttemptID,
			"verification_id", verification.ID,
			"verification_status", verification.Status,
			"verification_version", verification.Version,
			"action", action,
			"target", targetLabel,
			"target_uid", verification.ActionKey.TargetUID,
			"plan_hash", verification.ActionKey.PlanHash,
			"subject_cluster", verification.Subject.Cluster,
			"subject_namespace", verification.Subject.Namespace,
			"subject_kind", verification.Subject.Kind,
			"subject_name", verification.Subject.Name,
			"subject_uid", verification.Subject.UID,
			"result", "NO_RESOLUTION",
			"duration_ms",
			agent.now().Sub(startedAt).Milliseconds(),
			"error_code", errorCode,
			"error", err,
		)

		return verification, false
	}

	if verification.Status !=
		remediationdomain.VerificationStatusPending {
		return fail(
			verificationNotPending,
			fmt.Errorf(
				"verification %q has status %q; want %q",
				verification.ID,
				verification.Status,
				remediationdomain.VerificationStatusPending,
			),
		)
	}

	if agent.verificationExecutor == nil {
		return fail(
			verificationExecutorUnavailable,
			errors.New(
				"verification executor is not configured",
			),
		)
	}

	if agent.verifications == nil {
		return fail(
			verificationStoreUnavailable,
			errors.New(
				"verification store is not configured",
			),
		)
	}

	executionContext, cancelExecution := context.WithTimeout(
		ctx,
		agent.config.KubernetesRequestTimeout,
	)
	result, err := agent.verificationExecutor.Verify(
		executionContext,
		verification.Subject,
	)
	executionContextErr := executionContext.Err()
	cancelExecution()

	if err != nil {
		return fail(
			verificationExecutionFailed,
			err,
		)
	}

	if executionContextErr != nil {
		return fail(
			verificationExecutionFailed,
			executionContextErr,
		)
	}

	if err := result.validate(); err != nil {
		return fail(
			verificationResultInvalid,
			err,
		)
	}

	finishedAt := agent.now()
	completeCommand :=
		remediationdomain.CompleteVerificationCommand{
			ActionKey:       verification.ActionKey,
			ExpectedVersion: verification.Version,
			To:              result.Status,
			FinishedAt:      finishedAt,
			EvidenceCode:    result.EvidenceCode,
		}

	if err := completeCommand.Validate(); err != nil {
		return fail(
			verificationResultInvalid,
			err,
		)
	}

	completionContext, cancelCompletion := context.WithTimeout(
		ctx,
		agent.config.KubernetesRequestTimeout,
	)
	completed, err := agent.verifications.Complete(
		completionContext,
		completeCommand,
	)
	cancelCompletion()

	if err != nil {
		return fail(
			verificationCompleteFailed,
			err,
		)
	}

	agent.logger.Info(
		"agent_verification_completed",
		"incident_id", completed.ActionKey.IncidentID,
		"attempt_id", completed.ActionAttemptID,
		"verification_id", completed.ID,
		"verification_status", completed.Status,
		"verification_version", completed.Version,
		"action", action,
		"target", targetLabel,
		"target_uid", completed.ActionKey.TargetUID,
		"plan_hash", completed.ActionKey.PlanHash,
		"subject_cluster", completed.Subject.Cluster,
		"subject_namespace", completed.Subject.Namespace,
		"subject_kind", completed.Subject.Kind,
		"subject_name", completed.Subject.Name,
		"subject_uid", completed.Subject.UID,
		"evidence_code", completed.EvidenceCode,
		"result", "COMPLETED",
		"duration_ms",
		agent.now().Sub(startedAt).Milliseconds(),
		"error_code", "",
	)

	return completed, true
}
