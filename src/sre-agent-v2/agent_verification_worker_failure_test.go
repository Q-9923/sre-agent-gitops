package main

import (
	"context"
	"errors"
	"testing"
	"time"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

type verificationExecutorFunc func(
	context.Context,
	remediationdomain.Verification,
) (verificationExecutionResult, error)

func (verify verificationExecutorFunc) Verify(
	ctx context.Context,
	verification remediationdomain.Verification,
) (verificationExecutionResult, error) {
	return verify(ctx, verification)
}

func TestAgentVerificationWorkerClassifiesEveryFailurePoint(
	t *testing.T,
) {
	workerAt := time.Date(
		2026,
		time.October,
		13,
		13,
		0,
		0,
		0,
		time.UTC,
	)
	work, completion := verificationWorkerFailureFixture(workerAt)

	tests := []struct {
		name                  string
		configure             func(*verificationWorkerLifecycleAdapter) verificationExecutor
		wantCause             error
		wantCode              string
		wantAction            string
		wantWorkIdentity      bool
		wantRecoveredIdentity bool
		requestTimeout        time.Duration
	}{
		{
			name: "recovered_resolution_claim",
			configure: func(
				lifecycle *verificationWorkerLifecycleAdapter,
			) verificationExecutor {
				lifecycle.claimRecovered = func(
					context.Context,
					incidentdomain.ClaimRecoveredVerificationResolutionCommand,
				) (incidentdomain.RecoveredVerificationResolutionClaim, bool, error) {
					return incidentdomain.RecoveredVerificationResolutionClaim{},
						false,
						errRecoveredClaimForWorkerTest
				}

				return successfulVerificationExecutorForWorkerTest()
			},
			wantCause:  errRecoveredClaimForWorkerTest,
			wantCode:   "VERIFICATION_WORKER_RECOVERED_RESOLUTION_CLAIM_FAILED",
			wantAction: "CLAIM_RECOVERED_VERIFICATION_RESOLUTION",
		},
		{
			name: "recovered_resolution_resolve",
			configure: func(
				lifecycle *verificationWorkerLifecycleAdapter,
			) verificationExecutor {
				lifecycle.claimRecovered = func(
					context.Context,
					incidentdomain.ClaimRecoveredVerificationResolutionCommand,
				) (incidentdomain.RecoveredVerificationResolutionClaim, bool, error) {
					return incidentdomain.RecoveredVerificationResolutionClaim{
						Claim:        work.Claim,
						Verification: completion.Verification,
					}, true, nil
				}
				lifecycle.resolve = func(
					context.Context,
					incidentdomain.ResolveCommand,
				) (incidentdomain.Incident, error) {
					return incidentdomain.Incident{}, errResolveForWorkerTest
				}

				return successfulVerificationExecutorForWorkerTest()
			},
			wantCause:             errResolveForWorkerTest,
			wantCode:              "VERIFICATION_WORKER_RECOVERED_RESOLUTION_FAILED",
			wantAction:            "RESOLVE_RECOVERED_VERIFICATION",
			wantRecoveredIdentity: true,
		},
		{
			name: "pending_verification_claim",
			configure: func(
				lifecycle *verificationWorkerLifecycleAdapter,
			) verificationExecutor {
				lifecycle.claimPending = func(
					context.Context,
					incidentdomain.ClaimPendingVerificationCommand,
				) (incidentdomain.PendingVerificationClaim, bool, error) {
					return incidentdomain.PendingVerificationClaim{},
						false,
						errPendingClaimForWorkerTest
				}

				return successfulVerificationExecutorForWorkerTest()
			},
			wantCause:  errPendingClaimForWorkerTest,
			wantCode:   "VERIFICATION_WORKER_PENDING_VERIFICATION_CLAIM_FAILED",
			wantAction: "CLAIM_PENDING_VERIFICATION",
		},
		{
			name: "verification_executor",
			configure: func(
				lifecycle *verificationWorkerLifecycleAdapter,
			) verificationExecutor {
				configurePendingWorkerFixture(lifecycle, work, completion)

				return verificationExecutorFunc(func(
					context.Context,
					remediationdomain.Verification,
				) (verificationExecutionResult, error) {
					return verificationExecutionResult{}, errExecutorForWorkerTest
				})
			},
			wantCause:        errExecutorForWorkerTest,
			wantCode:         "VERIFICATION_WORKER_EXECUTION_FAILED",
			wantAction:       "EXECUTE_VERIFICATION",
			wantWorkIdentity: true,
		},
		{
			name: "execution_deadline",
			configure: func(
				lifecycle *verificationWorkerLifecycleAdapter,
			) verificationExecutor {
				configurePendingWorkerFixture(lifecycle, work, completion)

				return verificationExecutorFunc(func(
					ctx context.Context,
					_ remediationdomain.Verification,
				) (verificationExecutionResult, error) {
					<-ctx.Done()
					return verificationExecutionResult{}, ctx.Err()
				})
			},
			wantCause:        context.DeadlineExceeded,
			wantCode:         "VERIFICATION_WORKER_EXECUTION_DEADLINE_EXCEEDED",
			wantAction:       "EXECUTE_VERIFICATION",
			wantWorkIdentity: true,
			requestTimeout:   time.Millisecond,
		},
		{
			name: "invalid_execution_result",
			configure: func(
				lifecycle *verificationWorkerLifecycleAdapter,
			) verificationExecutor {
				configurePendingWorkerFixture(lifecycle, work, completion)

				return verificationExecutorFunc(func(
					context.Context,
					remediationdomain.Verification,
				) (verificationExecutionResult, error) {
					return verificationExecutionResult{
						Status:       remediationdomain.VerificationStatusPending,
						EvidenceCode: "INVALID_RESULT",
					}, nil
				})
			},
			wantCode:         "VERIFICATION_WORKER_EXECUTION_RESULT_INVALID",
			wantAction:       "VALIDATE_VERIFICATION_RESULT",
			wantWorkIdentity: true,
		},
		{
			name: "fenced_completion",
			configure: func(
				lifecycle *verificationWorkerLifecycleAdapter,
			) verificationExecutor {
				configurePendingWorkerFixture(lifecycle, work, completion)
				lifecycle.complete = func(
					context.Context,
					incidentdomain.CompleteFencedVerificationCommand,
				) (incidentdomain.FencedVerificationCompletionResult, error) {
					return incidentdomain.FencedVerificationCompletionResult{},
						errCompletionForWorkerTest
				}

				return successfulVerificationExecutorForWorkerTest()
			},
			wantCause:        errCompletionForWorkerTest,
			wantCode:         "VERIFICATION_WORKER_FENCED_COMPLETION_FAILED",
			wantAction:       "COMPLETE_FENCED_VERIFICATION",
			wantWorkIdentity: true,
		},
		{
			name: "resolve_after_recovered_completion",
			configure: func(
				lifecycle *verificationWorkerLifecycleAdapter,
			) verificationExecutor {
				configurePendingWorkerFixture(lifecycle, work, completion)
				lifecycle.resolve = func(
					context.Context,
					incidentdomain.ResolveCommand,
				) (incidentdomain.Incident, error) {
					return incidentdomain.Incident{}, errResolveForWorkerTest
				}

				return successfulVerificationExecutorForWorkerTest()
			},
			wantCause:        errResolveForWorkerTest,
			wantCode:         "VERIFICATION_WORKER_RECOVERED_RESOLUTION_FAILED",
			wantAction:       "RESOLVE_RECOVERED_VERIFICATION",
			wantWorkIdentity: true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			agent, _ := newVerificationWorkerTestAgent()
			lifecycle := &verificationWorkerLifecycleAdapter{}
			agent.verificationWorker = lifecycle
			agent.verificationExecutor = test.configure(lifecycle)
			if test.requestTimeout > 0 {
				agent.config.KubernetesRequestTimeout = test.requestTimeout
			}

			_, advanced, err := agent.runVerificationWorkerOnce(
				context.Background(),
			)
			if err == nil {
				t.Fatal("runVerificationWorkerOnce() error = nil; want failure")
			}
			if test.wantCause != nil && !errors.Is(err, test.wantCause) {
				t.Fatalf(
					"runVerificationWorkerOnce() error = %v; want cause %v",
					err,
					test.wantCause,
				)
			}
			if advanced {
				t.Fatal("runVerificationWorkerOnce() advanced = true; want false")
			}

			var failure *verificationWorkerFailure
			if !errors.As(err, &failure) {
				t.Fatalf(
					"runVerificationWorkerOnce() error type = %T; "+
						"want *verificationWorkerFailure",
					err,
				)
			}
			if failure.errorCode != test.wantCode ||
				failure.action != test.wantAction {
				t.Fatalf(
					"failure classification = (%q, %q); want (%q, %q)",
					failure.errorCode,
					failure.action,
					test.wantCode,
					test.wantAction,
				)
			}

			wantIdentity := test.wantWorkIdentity ||
				test.wantRecoveredIdentity
			if wantIdentity {
				if failure.incidentID != work.Claim.Incident.ID ||
					failure.verificationID != work.Verification.ID ||
					failure.actionAttemptID != work.Verification.ActionAttemptID {
					t.Fatalf(
						"failure identity = (%q, %q, %q); want (%q, %q, %q)",
						failure.incidentID,
						failure.verificationID,
						failure.actionAttemptID,
						work.Claim.Incident.ID,
						work.Verification.ID,
						work.Verification.ActionAttemptID,
					)
				}
			}
		})
	}
}

var (
	errRecoveredClaimForWorkerTest = errors.New("recovered claim failed")
	errPendingClaimForWorkerTest   = errors.New("pending claim failed")
	errExecutorForWorkerTest       = errors.New("verification execution failed")
	errCompletionForWorkerTest     = errors.New("verification completion failed")
	errResolveForWorkerTest        = errors.New("verification resolution failed")
)

func verificationWorkerFailureFixture(
	now time.Time,
) (
	incidentdomain.PendingVerificationClaim,
	incidentdomain.FencedVerificationCompletionResult,
) {
	incident := incidentdomain.Incident{
		ID:      "inc-worker-failure",
		State:   incidentdomain.StateVerifying,
		Version: 7,
	}
	verification := remediationdomain.Verification{
		ID:              "ver-worker-failure",
		ActionAttemptID: "att-worker-failure",
		ActionKey: remediationdomain.ActionKey{
			IncidentID: incident.ID,
			TargetUID:  "original-pod-uid",
		},
		Subject: remediationdomain.VerificationSubject{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "ReplicaSet",
			Name:      "crash-app-rs",
			UID:       "replicaset-uid",
		},
		Status:    remediationdomain.VerificationStatusPending,
		Version:   1,
		StartedAt: now.Add(-time.Minute),
	}
	recovered := verification
	recovered.Status = remediationdomain.VerificationStatusRecovered
	recovered.Version = 2
	recovered.EvidenceCode = "WORKLOAD_RECOVERED"
	recovered.FinishedAt = &now

	return incidentdomain.PendingVerificationClaim{
			Claim: incidentdomain.Claim{
				Incident:  incident,
				HolderID:  "verification-worker",
				ExpiresAt: now.Add(30 * time.Second),
			},
			Verification: verification,
		}, incidentdomain.FencedVerificationCompletionResult{
			Incident:     incident,
			Verification: recovered,
		}
}

func configurePendingWorkerFixture(
	lifecycle *verificationWorkerLifecycleAdapter,
	work incidentdomain.PendingVerificationClaim,
	completion incidentdomain.FencedVerificationCompletionResult,
) {
	lifecycle.claimPending = func(
		context.Context,
		incidentdomain.ClaimPendingVerificationCommand,
	) (incidentdomain.PendingVerificationClaim, bool, error) {
		return work, true, nil
	}
	lifecycle.complete = func(
		context.Context,
		incidentdomain.CompleteFencedVerificationCommand,
	) (incidentdomain.FencedVerificationCompletionResult, error) {
		return completion, nil
	}
	lifecycle.resolve = func(
		context.Context,
		incidentdomain.ResolveCommand,
	) (incidentdomain.Incident, error) {
		return completion.Incident, nil
	}
}

func successfulVerificationExecutorForWorkerTest() verificationExecutor {
	return verificationExecutorFunc(func(
		context.Context,
		remediationdomain.Verification,
	) (verificationExecutionResult, error) {
		return verificationExecutionResult{
			Status:       remediationdomain.VerificationStatusRecovered,
			EvidenceCode: "WORKLOAD_RECOVERED",
		}, nil
	})
}
