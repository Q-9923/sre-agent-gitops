package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func TestAgentUsesFencedVerificationLifecycleWithoutSplitWrites(
	t *testing.T,
) {
	t.Parallel()

	now := time.Date(
		2026,
		time.October,
		7,
		17,
		0,
		0,
		0,
		time.UTC,
	)

	registry := &agentVerificationIncidentRegistry{}
	directVerificationStore := newAgentVerificationStoreProbe()

	atomicStopError := errors.New(
		"injected fenced verification lifecycle stop",
	)
	lifecycle := &agentFencedVerificationLifecycleProbe{
		err: atomicStopError,
	}

	agent := newAgentVerificationTestAgent(
		now,
		registry,
		directVerificationStore,
	)
	agent.verificationLifecycle = lifecycle

	attempt := agentVerificationTerminalAttempt(now)
	currentIncident := incident.Incident{
		ID:      attempt.Key.IncidentID,
		State:   incident.StateDiagnosed,
		Version: attempt.Key.FencingToken - 1,
	}
	subject := remediationdomain.VerificationSubject{
		Cluster:   "dev",
		Namespace: "default",
		Kind:      "ReplicaSet",
		Name:      "crash-app-rs",
		UID:       "rs-uid-agent-verification-atomic",
	}

	_, started := agent.beginVerification(
		context.Background(),
		currentIncident,
		attempt,
		attempt.Key.FencingToken,
		subject,
		"RESTART_POD",
		"default/crash-app",
	)
	if started {
		t.Fatal(
			"beginVerification() started = true; " +
				"want false after injected atomic lifecycle failure",
		)
	}

	if lifecycle.calls != 1 {
		t.Fatalf(
			"BeginFencedVerification calls = %d; want 1",
			lifecycle.calls,
		)
	}
	if len(lifecycle.commands) != 1 {
		t.Fatalf(
			"BeginFencedVerification commands = %d; want 1",
			len(lifecycle.commands),
		)
	}

	command := lifecycle.commands[0]

	if command.IncidentID != currentIncident.ID {
		t.Fatalf(
			"BeginFencedVerification IncidentID = %q; want %q",
			command.IncidentID,
			currentIncident.ID,
		)
	}
	if command.ExpectedVersion != attempt.Key.FencingToken {
		t.Fatalf(
			"BeginFencedVerification ExpectedVersion = %d; "+
				"want fencing token %d",
			command.ExpectedVersion,
			attempt.Key.FencingToken,
		)
	}
	if command.HolderID !=
		agent.config.IncidentClaimHolderID {
		t.Fatalf(
			"BeginFencedVerification HolderID = %q; want %q",
			command.HolderID,
			agent.config.IncidentClaimHolderID,
		)
	}
	if command.ReasonCode != verificationTransitionReason {
		t.Fatalf(
			"BeginFencedVerification ReasonCode = %q; want %q",
			command.ReasonCode,
			verificationTransitionReason,
		)
	}

	if directVerificationStore.beginCalls != 0 {
		t.Fatalf(
			"direct Verification Begin calls = %d; want 0",
			directVerificationStore.beginCalls,
		)
	}
	if len(registry.transitions) != 0 {
		t.Fatalf(
			"direct Incident Transition calls = %d; want 0",
			len(registry.transitions),
		)
	}
}

type agentFencedVerificationLifecycleProbe struct {
	calls    int
	commands []incident.BeginFencedVerificationCommand
	err      error
}

func (
	probe *agentFencedVerificationLifecycleProbe,
) BeginFencedVerification(
	ctx context.Context,
	command incident.BeginFencedVerificationCommand,
) (incident.FencedVerificationResult, error) {
	if err := ctx.Err(); err != nil {
		return incident.FencedVerificationResult{}, err
	}

	probe.calls++
	probe.commands = append(
		probe.commands,
		command,
	)

	return incident.FencedVerificationResult{}, probe.err
}
