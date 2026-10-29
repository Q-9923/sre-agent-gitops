package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	incidentdomain "sre-agent/internal/incident"
)

func TestAgentVerificationWorkerOnceRejectsInvalidConfiguration(
	t *testing.T,
) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*sreAgent)
	}{
		{
			name: "missing_lifecycle",
			mutate: func(agent *sreAgent) {
				agent.verificationWorker = nil
			},
		},
		{
			name: "missing_clock",
			mutate: func(agent *sreAgent) {
				agent.now = nil
			},
		},
		{
			name: "missing_holder",
			mutate: func(agent *sreAgent) {
				agent.config.IncidentClaimHolderID = ""
			},
		}, {
			name: "blank_holder",
			mutate: func(agent *sreAgent) {
				agent.config.IncidentClaimHolderID = "  \t"
			},
		},
		{
			name: "zero_lease",
			mutate: func(agent *sreAgent) {
				agent.config.IncidentClaimLeaseDuration = 0
			},
		},
		{
			name: "negative_lease",
			mutate: func(agent *sreAgent) {
				agent.config.IncidentClaimLeaseDuration = -time.Second
			},
		},
		{
			name: "lease_shorter_than_execution_timeout",
			mutate: func(agent *sreAgent) {
				agent.config.IncidentClaimLeaseDuration = 14 * time.Second
				agent.config.KubernetesRequestTimeout = 15 * time.Second
			},
		},
		{
			name: "lease_equal_to_execution_timeout",
			mutate: func(agent *sreAgent) {
				agent.config.IncidentClaimLeaseDuration = 15 * time.Second
				agent.config.KubernetesRequestTimeout = 15 * time.Second
			},
		},
		{
			name: "missing_executor",
			mutate: func(agent *sreAgent) {
				agent.verificationExecutor = nil
			},
		},
		{
			name: "zero_execution_timeout",
			mutate: func(agent *sreAgent) {
				agent.config.KubernetesRequestTimeout = 0
			},
		},
		{
			name: "negative_execution_timeout",
			mutate: func(agent *sreAgent) {
				agent.config.KubernetesRequestTimeout = -time.Second
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			agent, _ := newVerificationWorkerTestAgent()
			test.mutate(agent)

			incident, advanced, err :=
				agent.runVerificationWorkerOnce(
					context.Background(),
				)

			if !errors.Is(err, errVerificationWorkerConfiguration) {
				t.Fatalf(
					"runVerificationWorkerOnce() error = %v; "+
						"want errVerificationWorkerConfiguration",
					err,
				)
			}
			if advanced {
				t.Fatal(
					"runVerificationWorkerOnce() advanced = true; want false",
				)
			}
			if incident.ID != "" {
				t.Fatalf(
					"runVerificationWorkerOnce() Incident ID = %q; want empty",
					incident.ID,
				)
			}
		})
	}
}

func TestAgentVerificationWorkerRunRejectsInvalidPollIntervalWithoutPanic(
	t *testing.T,
) {
	t.Parallel()

	for _, interval := range []time.Duration{0, -time.Second} {
		interval := interval

		t.Run(interval.String(), func(t *testing.T) {
			t.Parallel()

			agent, logs := newVerificationWorkerTestAgent()
			agent.config.PollInterval = interval

			agent.runVerificationWorker(context.Background())

			logOutput := logs.String()
			for _, expected := range []string{
				`"msg":"agent_verification_worker_failed"`,
				`"component":"verification_worker"`,
				`"action_attempt_id":""`,
				`"action":"RUN_VERIFICATION_WORKER"`,
				`"error_code":"VERIFICATION_WORKER_CONFIGURATION_INVALID"`,
			} {
				if !strings.Contains(logOutput, expected) {
					t.Fatalf(
						"invalid interval log missing %q:\n%s",
						expected,
						logOutput,
					)
				}
			}
			if count := strings.Count(
				logOutput,
				`"msg":"agent_verification_worker_failed"`,
			); count != 1 {
				t.Fatalf(
					"configuration failure logs = %d; want 1\n%s",
					count,
					logOutput,
				)
			}
		})
	}
}

func TestAgentVerificationWorkerRunClassifiesConfigurationFailure(
	t *testing.T,
) {
	t.Parallel()

	agent, logs := newVerificationWorkerTestAgent()
	agent.verificationWorker = nil

	ctx, cancel := context.WithCancel(context.Background())
	agent.waitVerificationWorker = func(
		context.Context,
		time.Duration,
	) error {
		cancel()
		return context.Canceled
	}

	agent.runVerificationWorker(ctx)

	logOutput := logs.String()
	for _, expected := range []string{
		`"msg":"agent_verification_worker_failed"`,
		`"action":"RUN_VERIFICATION_WORKER"`,
		`"error_code":"VERIFICATION_WORKER_CONFIGURATION_INVALID"`,
	} {
		if !strings.Contains(logOutput, expected) {
			t.Fatalf(
				"configuration failure log missing %q:\n%s",
				expected,
				logOutput,
			)
		}
	}
}
func TestAgentVerificationWorkerRunBacksOffConsecutiveFailuresWithinBound(
	t *testing.T,
) {
	agent, _ := newVerificationWorkerTestAgent()
	agent.verificationWorker = &verificationWorkerLifecycleAdapter{
		claimRecovered: func(
			context.Context,
			incidentdomain.ClaimRecoveredVerificationResolutionCommand,
		) (incidentdomain.RecoveredVerificationResolutionClaim, bool, error) {
			return incidentdomain.RecoveredVerificationResolutionClaim{},
				false,
				errors.New("scheduler unavailable")
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	delays := make([]time.Duration, 0, 5)
	agent.waitVerificationWorker = func(
		ctx context.Context,
		delay time.Duration,
	) error {
		delays = append(delays, delay)
		if len(delays) == 5 {
			cancel()
			return ctx.Err()
		}

		return nil
	}

	agent.runVerificationWorker(ctx)

	want := []time.Duration{
		5 * time.Millisecond,
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		40 * time.Millisecond,
	}
	assertVerificationWorkerDelays(t, delays, want)
}

func TestAgentVerificationWorkerRunResetsBackoffAfterSuccessfulIteration(
	t *testing.T,
) {
	agent, _ := newVerificationWorkerTestAgent()
	calls := 0
	agent.verificationWorker = &verificationWorkerLifecycleAdapter{
		claimRecovered: func(
			context.Context,
			incidentdomain.ClaimRecoveredVerificationResolutionCommand,
		) (incidentdomain.RecoveredVerificationResolutionClaim, bool, error) {
			calls++
			if calls == 1 || calls == 2 || calls == 4 {
				return incidentdomain.RecoveredVerificationResolutionClaim{},
					false,
					errors.New("scheduler unavailable")
			}

			return incidentdomain.RecoveredVerificationResolutionClaim{}, false, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	delays := make([]time.Duration, 0, 4)
	agent.waitVerificationWorker = func(
		ctx context.Context,
		delay time.Duration,
	) error {
		delays = append(delays, delay)
		if len(delays) == 4 {
			cancel()
			return ctx.Err()
		}

		return nil
	}

	agent.runVerificationWorker(ctx)

	want := []time.Duration{
		5 * time.Millisecond,
		10 * time.Millisecond,
		5 * time.Millisecond,
		5 * time.Millisecond,
	}
	assertVerificationWorkerDelays(t, delays, want)
}

func TestAgentVerificationWorkerRunWaitIsCancelable(
	t *testing.T,
) {
	agent, _ := newVerificationWorkerTestAgent()
	startedWaiting := make(chan struct{})
	agent.waitVerificationWorker = func(
		ctx context.Context,
		_ time.Duration,
	) error {
		close(startedWaiting)
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runVerificationWorker(ctx)
	}()

	select {
	case <-startedWaiting:
	case <-time.After(time.Second):
		t.Fatal("Verification Worker did not enter its wait")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Verification Worker did not stop after wait cancellation")
	}
}

func TestAgentVerificationWorkerRunDoesNotBusyLoopWithoutWork(
	t *testing.T,
) {
	agent, _ := newVerificationWorkerTestAgent()
	recoveredClaims := 0
	pendingClaims := 0
	agent.verificationWorker = &verificationWorkerLifecycleAdapter{
		claimRecovered: func(
			context.Context,
			incidentdomain.ClaimRecoveredVerificationResolutionCommand,
		) (incidentdomain.RecoveredVerificationResolutionClaim, bool, error) {
			recoveredClaims++
			return incidentdomain.RecoveredVerificationResolutionClaim{}, false, nil
		},
		claimPending: func(
			context.Context,
			incidentdomain.ClaimPendingVerificationCommand,
		) (incidentdomain.PendingVerificationClaim, bool, error) {
			pendingClaims++
			return incidentdomain.PendingVerificationClaim{}, false, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	agent.waitVerificationWorker = func(
		ctx context.Context,
		_ time.Duration,
	) error {
		waits++
		cancel()
		return ctx.Err()
	}

	agent.runVerificationWorker(ctx)

	if recoveredClaims != 1 || pendingClaims != 1 || waits != 1 {
		t.Fatalf(
			"no-work loop counts = recovered:%d pending:%d waits:%d; "+
				"want 1,1,1",
			recoveredClaims,
			pendingClaims,
			waits,
		)
	}
}

func assertVerificationWorkerDelays(
	t *testing.T,
	got []time.Duration,
	want []time.Duration,
) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("Verification Worker delays = %v; want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf(
				"Verification Worker delay[%d] = %s; want %s",
				index,
				got[index],
				want[index],
			)
		}
	}
}
