package main

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"time"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

type verificationWorkerLogCapture struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	records chan struct{}
}

func newVerificationWorkerLogCapture() *verificationWorkerLogCapture {
	return &verificationWorkerLogCapture{
		records: make(chan struct{}, 16),
	}
}

func (capture *verificationWorkerLogCapture) Write(
	payload []byte,
) (int, error) {
	capture.mu.Lock()
	written, err := capture.buffer.Write(payload)
	capture.mu.Unlock()

	select {
	case capture.records <- struct{}{}:
	default:
	}

	return written, err
}

func (capture *verificationWorkerLogCapture) String() string {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	return capture.buffer.String()
}

type agentVerificationWorkerRunLifecycleProbe struct {
	work                 incidentdomain.PendingVerificationClaim
	recoveredWork        incidentdomain.RecoveredVerificationResolutionClaim
	completion           incidentdomain.FencedVerificationCompletionResult
	resolvedIncident     incidentdomain.Incident
	resolvedEvents       chan incidentdomain.Incident
	claimedEvents        chan struct{}
	recoveredClaimEvents chan struct{}
	claimErr             error
	resolveErr           error
	recoveredClaimErr    error
	completionErr        error
	recoveredClaimed     bool
	pendingNotFound      bool
	recoveredClaimCalls  int
	claimCalls           int
	completeCalls        int
	resolveCalls         int
	completionCommands   []incidentdomain.CompleteFencedVerificationCommand
	resolveCommands      []incidentdomain.ResolveCommand
}

func (
	probe *agentVerificationWorkerRunLifecycleProbe,
) ClaimRecoveredVerificationResolution(
	ctx context.Context,
	_ incidentdomain.ClaimRecoveredVerificationResolutionCommand,
) (
	incidentdomain.RecoveredVerificationResolutionClaim,
	bool,
	error,
) {
	if err := ctx.Err(); err != nil {
		return incidentdomain.RecoveredVerificationResolutionClaim{},
			false,
			err
	}

	probe.recoveredClaimCalls++

	if probe.recoveredClaimEvents != nil {
		select {
		case probe.recoveredClaimEvents <- struct{}{}:
		default:
		}
	}
	if probe.recoveredClaimErr != nil {
		return incidentdomain.RecoveredVerificationResolutionClaim{},
			false,
			probe.recoveredClaimErr
	}

	return probe.recoveredWork, probe.recoveredClaimed, nil
}

func (
	probe *agentVerificationWorkerRunLifecycleProbe,
) ClaimPendingVerification(
	ctx context.Context,
	_ incidentdomain.ClaimPendingVerificationCommand,
) (incidentdomain.PendingVerificationClaim, bool, error) {
	if err := ctx.Err(); err != nil {
		return incidentdomain.PendingVerificationClaim{}, false, err
	}

	probe.claimCalls++

	if probe.claimErr != nil {
		return incidentdomain.PendingVerificationClaim{},
			false,
			probe.claimErr
	}
	if probe.pendingNotFound {
		return incidentdomain.PendingVerificationClaim{}, false, nil
	}

	if probe.claimedEvents != nil {
		select {
		case probe.claimedEvents <- struct{}{}:
		default:
		}
	}

	return probe.work, true, nil
}

func (
	probe *agentVerificationWorkerRunLifecycleProbe,
) CompleteFencedVerification(
	ctx context.Context,
	command incidentdomain.CompleteFencedVerificationCommand,
) (incidentdomain.FencedVerificationCompletionResult, error) {
	if err := ctx.Err(); err != nil {
		return incidentdomain.FencedVerificationCompletionResult{}, err
	}

	probe.completeCalls++
	probe.completionCommands = append(probe.completionCommands, command)
	if probe.completionErr == nil {
		probe.pendingNotFound = true
	}

	return probe.completion, probe.completionErr
}

func (
	probe *agentVerificationWorkerRunLifecycleProbe,
) Resolve(
	ctx context.Context,
	command incidentdomain.ResolveCommand,
) (incidentdomain.Incident, error) {
	if err := ctx.Err(); err != nil {
		return incidentdomain.Incident{}, err
	}

	probe.resolveCalls++
	probe.resolveCommands = append(probe.resolveCommands, command)

	if probe.resolveErr != nil {
		return incidentdomain.Incident{}, probe.resolveErr
	}

	probe.recoveredClaimed = false

	select {
	case probe.resolvedEvents <- probe.resolvedIncident:
	default:
	}

	return probe.resolvedIncident, nil
}

type verificationWorkerLifecycleAdapter struct {
	claimRecovered func(
		context.Context,
		incidentdomain.ClaimRecoveredVerificationResolutionCommand,
	) (incidentdomain.RecoveredVerificationResolutionClaim, bool, error)
	claimPending func(
		context.Context,
		incidentdomain.ClaimPendingVerificationCommand,
	) (incidentdomain.PendingVerificationClaim, bool, error)
	complete func(
		context.Context,
		incidentdomain.CompleteFencedVerificationCommand,
	) (incidentdomain.FencedVerificationCompletionResult, error)
	resolve func(
		context.Context,
		incidentdomain.ResolveCommand,
	) (incidentdomain.Incident, error)
}

func (
	adapter *verificationWorkerLifecycleAdapter,
) ClaimRecoveredVerificationResolution(
	ctx context.Context,
	command incidentdomain.ClaimRecoveredVerificationResolutionCommand,
) (incidentdomain.RecoveredVerificationResolutionClaim, bool, error) {
	if adapter.claimRecovered == nil {
		return incidentdomain.RecoveredVerificationResolutionClaim{}, false, nil
	}

	return adapter.claimRecovered(ctx, command)
}

func (
	adapter *verificationWorkerLifecycleAdapter,
) ClaimPendingVerification(
	ctx context.Context,
	command incidentdomain.ClaimPendingVerificationCommand,
) (incidentdomain.PendingVerificationClaim, bool, error) {
	if adapter.claimPending == nil {
		return incidentdomain.PendingVerificationClaim{}, false, nil
	}

	return adapter.claimPending(ctx, command)
}

func (
	adapter *verificationWorkerLifecycleAdapter,
) CompleteFencedVerification(
	ctx context.Context,
	command incidentdomain.CompleteFencedVerificationCommand,
) (incidentdomain.FencedVerificationCompletionResult, error) {
	if adapter.complete == nil {
		return incidentdomain.FencedVerificationCompletionResult{}, nil
	}

	return adapter.complete(ctx, command)
}

func (
	adapter *verificationWorkerLifecycleAdapter,
) Resolve(
	ctx context.Context,
	command incidentdomain.ResolveCommand,
) (incidentdomain.Incident, error) {
	if adapter.resolve == nil {
		return incidentdomain.Incident{}, nil
	}

	return adapter.resolve(ctx, command)
}

func newVerificationWorkerTestAgent() (*sreAgent, *bytes.Buffer) {
	workerAt := time.Date(
		2026,
		time.October,
		13,
		12,
		0,
		0,
		0,
		time.UTC,
	)
	logs := &bytes.Buffer{}

	return &sreAgent{
		config: agentConfig{
			PollInterval:               5 * time.Millisecond,
			IncidentClaimHolderID:      "verification-worker",
			IncidentClaimLeaseDuration: 30 * time.Second,
			KubernetesRequestTimeout:   time.Second,
		},
		verificationWorker: &verificationWorkerLifecycleAdapter{},
		verificationExecutor: &agentVerificationExecutorProbe{
			result: verificationExecutionResult{
				Status:       remediationdomain.VerificationStatusNotRecovered,
				EvidenceCode: "WORKLOAD_NOT_RECOVERED",
			},
		},
		logger: slog.New(slog.NewJSONHandler(logs, nil)),
		now: func() time.Time {
			return workerAt
		},
	}, logs
}
