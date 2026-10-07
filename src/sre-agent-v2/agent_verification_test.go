package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func TestAgentBeginsPendingVerificationAndTransitionsIncidentToVerifying(
	t *testing.T,
) {
	t.Parallel()

	now := time.Date(
		2026,
		time.October,
		6,
		10,
		0,
		0,
		0,
		time.UTC,
	)

	registry := &agentVerificationIncidentRegistry{}
	store := newAgentVerificationStoreProbe()

	agent := newAgentVerificationTestAgent(
		now,
		registry,
		store,
	)

	currentIncident := incident.Incident{
		ID:      "inc-agent-verification",
		State:   incident.StateDiagnosed,
		Version: 7,
	}
	attempt := agentVerificationTerminalAttempt(now)
	subject := remediationdomain.VerificationSubject{
		Cluster:   "dev",
		Namespace: "default",
		Kind:      "ReplicaSet",
		Name:      "crash-app-rs",
		UID:       "rs-uid-agent-verification",
	}

	verification, started := agent.beginVerification(
		context.Background(),
		currentIncident,
		attempt,
		attempt.Key.FencingToken,
		subject,
		"RESTART_POD",
		"default/crash-app",
	)
	if !started {
		t.Fatal("beginVerification() started = false; want true")
	}

	if verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"Verification Status = %q; want %q",
			verification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}
	if verification.ActionAttemptID != attempt.ID {
		t.Fatalf(
			"Verification ActionAttemptID = %q; want %q",
			verification.ActionAttemptID,
			attempt.ID,
		)
	}
	if verification.Subject != subject {
		t.Fatalf(
			"Verification Subject = %#v; want %#v",
			verification.Subject,
			subject,
		)
	}

	if store.beginCalls != 1 {
		t.Fatalf(
			"Verification Begin calls = %d; want 1",
			store.beginCalls,
		)
	}
	if len(store.created) != 1 || !store.created[0] {
		t.Fatalf(
			"Verification created results = %#v; want []bool{true}",
			store.created,
		)
	}

	if len(registry.transitions) != 1 {
		t.Fatalf(
			"Incident Transition calls = %d; want 1",
			len(registry.transitions),
		)
	}

	command := registry.transitions[0]
	if command.IncidentID != currentIncident.ID {
		t.Fatalf(
			"Transition IncidentID = %q; want %q",
			command.IncidentID,
			currentIncident.ID,
		)
	}
	if command.ExpectedVersion != attempt.Key.FencingToken {
		t.Fatalf(
			"Transition ExpectedVersion = %d; want fencing token %d",
			command.ExpectedVersion,
			attempt.Key.FencingToken,
		)
	}
	if command.To != incident.StateVerifying {
		t.Fatalf(
			"Transition To = %q; want %q",
			command.To,
			incident.StateVerifying,
		)
	}
	if command.To == incident.StateResolved {
		t.Fatal("Agent resolved Incident before independent verification")
	}
}

func TestAgentDoesNotPersistVerificationBeforeIncidentFence(
	t *testing.T,
) {
	t.Parallel()

	now := time.Date(
		2026,
		time.October,
		6,
		10,
		5,
		0,
		0,
		time.UTC,
	)

	registry := &agentVerificationIncidentRegistry{
		transitionErrors: []error{
			errors.New("injected incident transition failure"),
			nil,
		},
	}
	store := newAgentVerificationStoreProbe()

	agent := newAgentVerificationTestAgent(
		now,
		registry,
		store,
	)

	currentIncident := incident.Incident{
		ID:      "inc-agent-verification-fence",
		State:   incident.StateDiagnosed,
		Version: 11,
	}
	attempt := agentVerificationTerminalAttempt(now)
	attempt.Key.IncidentID = currentIncident.ID
	attempt.Key.FencingToken = 12

	subject := remediationdomain.VerificationSubject{
		Cluster:   "dev",
		Namespace: "default",
		Kind:      "ReplicaSet",
		Name:      "retry-app-rs",
		UID:       "rs-uid-agent-verification-fence",
	}

	first, firstStarted := agent.beginVerification(
		context.Background(),
		currentIncident,
		attempt,
		attempt.Key.FencingToken,
		subject,
		"RESTART_POD",
		"default/retry-app",
	)
	if firstStarted {
		t.Fatal(
			"first beginVerification() started = true; " +
				"want false after fencing failure",
		)
	}
	if first.ID != "" {
		t.Fatalf(
			"first Verification ID = %q; "+
				"want empty before successful fencing",
			first.ID,
		)
	}
	if store.beginCalls != 0 {
		t.Fatalf(
			"Verification Begin calls after fencing failure = %d; "+
				"want 0",
			store.beginCalls,
		)
	}

	second, secondStarted := agent.beginVerification(
		context.Background(),
		currentIncident,
		attempt,
		attempt.Key.FencingToken,
		subject,
		"RESTART_POD",
		"default/retry-app",
	)
	if !secondStarted {
		t.Fatal(
			"second beginVerification() started = false; want true",
		)
	}
	if second.ID == "" {
		t.Fatal("second Verification ID is empty")
	}

	if store.beginCalls != 1 {
		t.Fatalf(
			"Verification Begin calls = %d; want 1",
			store.beginCalls,
		)
	}
	if len(store.created) != 1 || !store.created[0] {
		t.Fatalf(
			"Verification created results = %#v; "+
				"want []bool{true}",
			store.created,
		)
	}
	if len(registry.transitions) != 2 {
		t.Fatalf(
			"Incident Transition calls = %d; want 2",
			len(registry.transitions),
		)
	}
}

func TestAgentRejectsStaleFencingTokenForVerifyingIncident(
	t *testing.T,
) {
	t.Parallel()

	now := time.Date(
		2026,
		time.October,
		6,
		10,
		20,
		0,
		0,
		time.UTC,
	)

	registry := &agentVerificationIncidentRegistry{}
	store := newAgentVerificationStoreProbe()
	logs := &bytes.Buffer{}

	agent := newAgentVerificationTestAgent(
		now,
		registry,
		store,
	)
	agent.verificationLifecycle =
		&agentFencedVerificationLifecycleProbe{
			err: incident.ErrVerificationFenceConflict,
		}
	agent.logger = slog.New(
		slog.NewJSONHandler(logs, nil),
	)

	attempt := agentVerificationTerminalAttempt(now)
	currentIncident := incident.Incident{
		ID:      attempt.Key.IncidentID,
		State:   incident.StateVerifying,
		Version: 20,
	}

	_, started := agent.beginVerification(
		context.Background(),
		currentIncident,
		attempt,
		21,
		remediationdomain.VerificationSubject{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "ReplicaSet",
			Name:      "crash-app-rs",
			UID:       "rs-uid-agent-verification",
		},
		"RESTART_POD",
		"default/crash-app",
	)
	if started {
		t.Fatal(
			"beginVerification() started = true; " +
				"want false for stale fencing token",
		)
	}
	if store.beginCalls != 0 {
		t.Fatalf(
			"Verification Begin calls = %d; "+
				"want 0 for stale fencing token",
			store.beginCalls,
		)
	}
	if len(registry.transitions) != 0 {
		t.Fatalf(
			"Incident Transition calls = %d; "+
				"want 0 for already VERIFYING Incident",
			len(registry.transitions),
		)
	}
	if !bytes.Contains(
		logs.Bytes(),
		[]byte(`"error_code":"INCIDENT_VERIFYING_FENCE_CONFLICT"`),
	) {
		t.Fatalf(
			"missing INCIDENT_VERIFYING_FENCE_CONFLICT log: %s",
			logs.String(),
		)
	}
}

func TestVerificationSubjectForPodIsStableAcrossPodReplacement(
	t *testing.T,
) {
	t.Parallel()

	first := PodEvidence{
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-old",
			UID:       "pod-uid-old",
		},
		Owner: OwnerEvidence{
			Kind: "ReplicaSet",
			Name: "crash-app-rs",
			UID:  "rs-uid-stable",
		},
	}
	replacement := first
	replacement.Target.Name = "crash-app-new"
	replacement.Target.UID = "pod-uid-new"

	firstSubject := verificationSubjectForPod(first)
	replacementSubject := verificationSubjectForPod(replacement)

	if firstSubject != replacementSubject {
		t.Fatalf(
			"replacement Pod Subject = %#v; want stable %#v",
			replacementSubject,
			firstSubject,
		)
	}
	if firstSubject.UID != first.Owner.UID {
		t.Fatalf(
			"Verification Subject UID = %q; want owner UID %q",
			firstSubject.UID,
			first.Owner.UID,
		)
	}
	if firstSubject.UID == first.Target.UID {
		t.Fatal(
			"Verification Subject incorrectly uses replaceable Pod UID",
		)
	}
}

func TestAgentFailsClosedWithoutVerificationStore(
	t *testing.T,
) {
	t.Parallel()

	now := time.Date(
		2026,
		time.October,
		6,
		10,
		10,
		0,
		0,
		time.UTC,
	)

	registry := &agentVerificationIncidentRegistry{}
	logs := &bytes.Buffer{}

	agent := &sreAgent{
		config: agentConfig{
			IncidentClaimHolderID:    "agent-verifier",
			KubernetesRequestTimeout: time.Second,
		},
		incidents: registry,
		logger: slog.New(
			slog.NewJSONHandler(logs, nil),
		),
		now: func() time.Time {
			return now
		},
	}

	attempt := agentVerificationTerminalAttempt(now)

	_, started := agent.beginVerification(
		context.Background(),
		incident.Incident{
			ID:      attempt.Key.IncidentID,
			State:   incident.StateDiagnosed,
			Version: attempt.Key.FencingToken - 1,
		},
		attempt,
		attempt.Key.FencingToken,
		remediationdomain.VerificationSubject{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "ReplicaSet",
			Name:      "crash-app-rs",
			UID:       "rs-uid-agent-verification",
		},
		"RESTART_POD",
		"default/crash-app",
	)
	if started {
		t.Fatal("beginVerification() started = true; want false")
	}

	if len(registry.transitions) != 0 {
		t.Fatalf(
			"Incident Transition calls = %d; want 0",
			len(registry.transitions),
		)
	}

	if !bytes.Contains(
		logs.Bytes(),
		[]byte(`"error_code":"VERIFICATION_STORE_UNAVAILABLE"`),
	) {
		t.Fatalf(
			"missing VERIFICATION_STORE_UNAVAILABLE log: %s",
			logs.String(),
		)
	}
}

func TestAgentReusesPendingVerificationForIncidentAlreadyVerifying(
	t *testing.T,
) {
	t.Parallel()

	now := time.Date(
		2026,
		time.October,
		6,
		10,
		15,
		0,
		0,
		time.UTC,
	)

	registry := &agentVerificationIncidentRegistry{}
	store := newAgentVerificationStoreProbe()

	agent := newAgentVerificationTestAgent(
		now,
		registry,
		store,
	)

	agent.verificationLifecycle =
		&agentVerificationLifecycleAdapter{
			registry:                 registry,
			store:                    store,
			incidentAlreadyVerifying: true,
		}
	attempt := agentVerificationTerminalAttempt(now)
	currentIncident := incident.Incident{
		ID:      attempt.Key.IncidentID,
		State:   incident.StateVerifying,
		Version: attempt.Key.FencingToken,
	}
	subject := remediationdomain.VerificationSubject{
		Cluster:   "dev",
		Namespace: "default",
		Kind:      "ReplicaSet",
		Name:      "crash-app-rs",
		UID:       "rs-uid-agent-verification",
	}

	first, started := agent.beginVerification(
		context.Background(),
		currentIncident,
		attempt,
		attempt.Key.FencingToken,
		subject,
		"RESTART_POD",
		"default/crash-app",
	)
	if !started {
		t.Fatal("first beginVerification() started = false; want true")
	}

	second, started := agent.beginVerification(
		context.Background(),
		currentIncident,
		attempt,
		attempt.Key.FencingToken,
		subject,
		"RESTART_POD",
		"default/crash-app",
	)
	if !started {
		t.Fatal("second beginVerification() started = false; want true")
	}
	if second.ID != first.ID {
		t.Fatalf(
			"second Verification ID = %q; want %q",
			second.ID,
			first.ID,
		)
	}

	if len(registry.transitions) != 0 {
		t.Fatalf(
			"Incident Transition calls = %d; want 0 "+
				"when Incident is already VERIFYING",
			len(registry.transitions),
		)
	}
}

type agentVerificationIncidentRegistry struct {
	transitions      []incident.TransitionCommand
	transitionErrors []error
}

func (registry *agentVerificationIncidentRegistry) Claim(
	context.Context,
	incident.ClaimCommand,
) (incident.Claim, error) {
	return incident.Claim{},
		errors.New("unexpected Claim call")
}

func (registry *agentVerificationIncidentRegistry) Observe(
	context.Context,
	incident.Observation,
) (incident.Incident, bool, error) {
	return incident.Incident{},
		false,
		errors.New("unexpected Observe call")
}

func (registry *agentVerificationIncidentRegistry) Transition(
	ctx context.Context,
	command incident.TransitionCommand,
) (incident.Incident, error) {
	if err := ctx.Err(); err != nil {
		return incident.Incident{}, err
	}

	registry.transitions = append(
		registry.transitions,
		command,
	)

	if len(registry.transitionErrors) > 0 {
		transitionErr := registry.transitionErrors[0]
		registry.transitionErrors =
			registry.transitionErrors[1:]

		if transitionErr != nil {
			return incident.Incident{}, transitionErr
		}
	}

	return incident.Incident{
		ID:      command.IncidentID,
		State:   command.To,
		Version: command.ExpectedVersion + 1,
	}, nil
}

type agentVerificationStoreProbe struct {
	inner      remediationdomain.VerificationStore
	beginCalls int
	created    []bool
}

func newAgentVerificationStoreProbe() *agentVerificationStoreProbe {
	return &agentVerificationStoreProbe{
		inner: remediationdomain.NewMemoryVerificationStore(),
	}
}

func (store *agentVerificationStoreProbe) Begin(
	ctx context.Context,
	command remediationdomain.BeginVerificationCommand,
) (remediationdomain.Verification, bool, error) {
	store.beginCalls++

	verification, created, err := store.inner.Begin(
		ctx,
		command,
	)
	if err == nil {
		store.created = append(store.created, created)
	}

	return verification, created, err
}

func (store *agentVerificationStoreProbe) Complete(
	ctx context.Context,
	command remediationdomain.CompleteVerificationCommand,
) (remediationdomain.Verification, error) {
	return store.inner.Complete(ctx, command)
}

type agentVerificationLifecycleAdapter struct {
	registry                 incidentRegistry
	store                    remediationdomain.VerificationStore
	incidentAlreadyVerifying bool
}

func (
	adapter *agentVerificationLifecycleAdapter,
) BeginFencedVerification(
	ctx context.Context,
	command incident.BeginFencedVerificationCommand,
) (incident.FencedVerificationResult, error) {
	if err := ctx.Err(); err != nil {
		return incident.FencedVerificationResult{}, err
	}

	var verifyingIncident incident.Incident

	if adapter.incidentAlreadyVerifying {
		verifyingIncident = incident.Incident{
			ID:      command.IncidentID,
			State:   incident.StateVerifying,
			Version: command.ExpectedVersion,
		}
	} else {
		transitioned, err := adapter.registry.Transition(
			ctx,
			incident.TransitionCommand{
				IncidentID:      command.IncidentID,
				ExpectedVersion: command.ExpectedVersion,
				To:              incident.StateVerifying,
				Actor:           command.HolderID,
				ReasonCode:      command.ReasonCode,
			},
		)
		if err != nil {
			return incident.FencedVerificationResult{}, err
		}

		verifyingIncident = transitioned
	}

	verification, created, err := adapter.store.Begin(
		ctx,
		command.Verification,
	)
	if err != nil {
		return incident.FencedVerificationResult{}, err
	}

	return incident.FencedVerificationResult{
		Incident:     verifyingIncident,
		Verification: verification,
		Created:      created,
	}, nil
}
func newAgentVerificationTestAgent(
	now time.Time,
	registry incidentRegistry,
	store remediationdomain.VerificationStore,
) *sreAgent {
	agent := &sreAgent{
		config: agentConfig{
			IncidentClaimHolderID:    "agent-verifier",
			KubernetesRequestTimeout: time.Second,
		},
		incidents:     registry,
		verifications: store,
		logger: slog.New(
			slog.NewJSONHandler(&bytes.Buffer{}, nil),
		),
		now: func() time.Time {
			return now
		},
	}

	agent.verificationLifecycle =
		&agentVerificationLifecycleAdapter{
			registry: registry,
			store:    store,
		}

	return agent
}
func agentVerificationTerminalAttempt(
	now time.Time,
) remediationdomain.ActionAttempt {
	finishedAt := now.Add(-time.Second)

	return remediationdomain.ActionAttempt{
		ID: "att-agent-verification",
		Key: remediationdomain.ExecutionKey{
			IncidentID:   "inc-agent-verification",
			PlanHash:     "sha256:agent-verification-plan",
			TargetUID:    "pod-uid-agent-verification",
			FencingToken: 8,
		},
		Status:     remediationdomain.ActionAttemptStatusSucceeded,
		Version:    2,
		StartedAt:  now.Add(-2 * time.Second),
		FinishedAt: &finishedAt,
	}
}
