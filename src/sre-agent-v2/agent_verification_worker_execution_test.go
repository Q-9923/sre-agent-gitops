package main

import (
	"bytes"
	"context"
	"log/slog"
	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
	"testing"
	"time"
)

func TestAgentVerificationWorkerClaimsExecutesCompletesAndResolvesRecovered(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(
		2026,
		time.October,
		9,
		10,
		0,
		0,
		0,
		time.UTC,
	)

	store := remediationdomain.NewMemoryVerificationStore()
	registry :=
		incidentdomain.NewRegistryWithVerificationLifecycleStore(
			store,
		)

	observation := incidentdomain.Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "PodCrashLooping",
		Target: incidentdomain.Target{
			Kind:      "Pod",
			Namespace: "default",
			Name:      "worker-app",
			UID:       "pod-uid-verification-worker",
		},
	}

	observed, created, err := registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: observed.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "action-agent",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v",
			err,
		)
	}

	actionClaim, err := registry.Claim(
		ctx,
		incidentdomain.ClaimCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			HolderID:        "action-agent",
			Now:             now,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("Claim(action-agent) error = %v", err)
	}

	actionFinishedAt := now.Add(-time.Second)
	actionAttempt := remediationdomain.ActionAttempt{
		ID: "att-verification-worker",
		Key: remediationdomain.ExecutionKey{
			IncidentID:   actionClaim.Incident.ID,
			PlanHash:     "sha256:verification-worker-plan",
			TargetUID:    observation.Target.UID,
			FencingToken: actionClaim.Incident.Version,
		},
		Status:  remediationdomain.ActionAttemptStatusSucceeded,
		Version: 2,
		StartedAt: now.Add(
			-2 * time.Second,
		),
		FinishedAt: &actionFinishedAt,
	}
	subject := remediationdomain.VerificationSubject{
		Cluster:   observation.Cluster,
		Namespace: observation.Target.Namespace,
		Kind:      "ReplicaSet",
		Name:      "worker-app-rs",
		UID:       "rs-uid-verification-worker",
	}
	beginCommand := remediationdomain.BeginVerificationCommand{
		ActionAttempt: actionAttempt,
		Subject:       subject,
		StartedAt:     now,
	}

	fenced, err := registry.BeginFencedVerification(
		ctx,
		incidentdomain.BeginFencedVerificationCommand{
			IncidentID: actionClaim.Incident.ID,
			ExpectedVersion: actionClaim.Incident.
				Version,
			HolderID:     actionClaim.HolderID,
			Now:          now.Add(time.Second),
			ReasonCode:   verificationTransitionReason,
			Verification: beginCommand,
		},
	)
	if err != nil {
		t.Fatalf("BeginFencedVerification() error = %v", err)
	}
	if fenced.Verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"Verification Status = %q; want %q",
			fenced.Verification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}
	if fenced.Incident.State != incidentdomain.StateVerifying {
		t.Fatalf(
			"Incident State = %q; want %q",
			fenced.Incident.State,
			incidentdomain.StateVerifying,
		)
	}

	workerAt := actionClaim.ExpiresAt
	executor := &agentVerificationExecutorProbe{
		result: verificationExecutionResult{
			Status: remediationdomain.
				VerificationStatusRecovered,
			EvidenceCode: "WORKLOAD_RECOVERED",
		},
	}

	var logs bytes.Buffer
	agent := &sreAgent{
		config: agentConfig{
			IncidentClaimHolderID:      "verification-worker",
			IncidentClaimLeaseDuration: 30 * time.Second,
			KubernetesRequestTimeout:   time.Second,
		},
		verificationLifecycle: registry,
		verificationWorker:    registry,
		verificationExecutor:  executor,
		logger: slog.New(
			slog.NewJSONHandler(&logs, nil),
		),
		now: func() time.Time {
			return workerAt
		},
	}
	resolved, advanced, err :=
		agent.runVerificationWorkerOnce(ctx)
	if err != nil {
		t.Fatalf(
			"runVerificationWorkerOnce() error = %v",
			err,
		)
	}
	if !advanced {
		t.Fatalf(
			"runVerificationWorkerOnce() advanced = false; want true\nlogs:\n%s",
			logs.String(),
		)
	}
	if resolved.ID != observed.ID {
		t.Fatalf(
			"resolved Incident ID = %q; want %q",
			resolved.ID,
			observed.ID,
		)
	}
	if resolved.State != incidentdomain.StateResolved {
		t.Fatalf(
			"resolved Incident State = %q; want %q",
			resolved.State,
			incidentdomain.StateResolved,
		)
	}
	if resolved.ResolutionVerificationID !=
		fenced.Verification.ID {
		t.Fatalf(
			"ResolutionVerificationID = %q; want %q",
			resolved.ResolutionVerificationID,
			fenced.Verification.ID,
		)
	}

	if executor.calls != 1 {
		t.Fatalf(
			"Verification executor calls = %d; want 1",
			executor.calls,
		)
	}
	if len(executor.subjects) != 1 {
		t.Fatalf(
			"Verification executor subjects = %d; want 1",
			len(executor.subjects),
		)
	}
	if executor.subjects[0] != subject {
		t.Fatalf(
			"Verification executor Subject = %#v; want %#v",
			executor.subjects[0],
			subject,
		)
	}

	persisted, created, err := store.Begin(
		ctx,
		beginCommand,
	)
	if err != nil {
		t.Fatalf(
			"reload Verification through Begin() error = %v",
			err,
		)
	}
	if created {
		t.Fatal(
			"reload Verification through Begin() created = true; " +
				"want existing Verification",
		)
	}
	if persisted.Status !=
		remediationdomain.VerificationStatusRecovered {
		t.Fatalf(
			"persisted Verification Status = %q; want %q",
			persisted.Status,
			remediationdomain.VerificationStatusRecovered,
		)
	}
	if persisted.EvidenceCode != "WORKLOAD_RECOVERED" {
		t.Fatalf(
			"persisted Verification EvidenceCode = %q; want %q",
			persisted.EvidenceCode,
			"WORKLOAD_RECOVERED",
		)
	}
	if persisted.FinishedAt == nil ||
		!persisted.FinishedAt.Equal(workerAt) {
		t.Fatalf(
			"persisted Verification FinishedAt = %v; want %s",
			persisted.FinishedAt,
			workerAt,
		)
	}
}

func TestAgentVerificationWorkerPrioritizesRecoveredResolutionWithoutExecutor(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(
		2026,
		time.October,
		9,
		13,
		0,
		0,
		0,
		time.UTC,
	)

	actionKey := remediationdomain.ActionKey{
		IncidentID: "inc-recovered-resolution-worker",
		PlanHash:   "sha256:recovered-resolution-worker-plan",
		TargetUID:  "pod-uid-recovered-resolution-worker",
	}
	finishedAt := now.Add(-time.Minute)
	recovered := remediationdomain.Verification{
		ID:              "ver-recovered-resolution-worker",
		ActionAttemptID: "att-recovered-resolution-worker",
		ActionKey:       actionKey,
		Subject: remediationdomain.VerificationSubject{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "ReplicaSet",
			Name:      "recovered-resolution-worker-rs",
			UID:       "rs-uid-recovered-resolution-worker",
		},
		Status: remediationdomain.
			VerificationStatusRecovered,
		Version:      2,
		StartedAt:    now.Add(-2 * time.Minute),
		FinishedAt:   &finishedAt,
		EvidenceCode: "WORKLOAD_RECOVERED",
	}
	verifyingIncident := incidentdomain.Incident{
		ID:      actionKey.IncidentID,
		State:   incidentdomain.StateVerifying,
		Version: 7,
	}
	claim := incidentdomain.Claim{
		Incident:  verifyingIncident,
		HolderID:  "verification-resolution-worker",
		ExpiresAt: now.Add(30 * time.Second),
	}

	resolvedIncident := verifyingIncident
	resolvedIncident.State = incidentdomain.StateResolved
	resolvedIncident.Version++
	resolvedIncident.ResolutionVerificationID = recovered.ID

	lifecycle := &agentVerificationWorkerRunLifecycleProbe{
		recoveredWork: incidentdomain.
			RecoveredVerificationResolutionClaim{
			Verification: recovered,
			Claim:        claim,
		},
		recoveredClaimed: true,
		resolvedIncident: resolvedIncident,
		resolvedEvents: make(
			chan incidentdomain.Incident,
			1,
		),
	}

	agent := &sreAgent{
		config: agentConfig{
			IncidentClaimHolderID: claim.HolderID,
			IncidentClaimLeaseDuration: 30 *
				time.Second,
		},
		verificationWorker: lifecycle,
		now: func() time.Time {
			return now
		},
	}
	resolved, advanced, err :=
		agent.runVerificationWorkerOnce(ctx)
	if err != nil {
		t.Fatalf(
			"runVerificationWorkerOnce() error = %v",
			err,
		)
	}
	if !advanced {
		t.Fatal(
			"runVerificationWorkerOnce() advanced = false; want true",
		)
	}
	if resolved.ID != resolvedIncident.ID {
		t.Fatalf(
			"resolved Incident ID = %q; want %q",
			resolved.ID,
			resolvedIncident.ID,
		)
	}
	if resolved.State != incidentdomain.StateResolved {
		t.Fatalf(
			"resolved Incident State = %q; want %q",
			resolved.State,
			incidentdomain.StateResolved,
		)
	}
	if resolved.ResolutionVerificationID != recovered.ID {
		t.Fatalf(
			"ResolutionVerificationID = %q; want %q",
			resolved.ResolutionVerificationID,
			recovered.ID,
		)
	}

	if lifecycle.recoveredClaimCalls != 1 {
		t.Fatalf(
			"ClaimRecoveredVerificationResolution calls = %d; want 1",
			lifecycle.recoveredClaimCalls,
		)
	}
	if lifecycle.claimCalls != 0 {
		t.Fatalf(
			"ClaimPendingVerification calls = %d; want 0",
			lifecycle.claimCalls,
		)
	}
	if lifecycle.completeCalls != 0 {
		t.Fatalf(
			"CompleteFencedVerification calls = %d; want 0",
			lifecycle.completeCalls,
		)
	}
	if lifecycle.resolveCalls != 1 {
		t.Fatalf(
			"Resolve calls = %d; want 1",
			lifecycle.resolveCalls,
		)
	}
	if len(lifecycle.resolveCommands) != 1 {
		t.Fatalf(
			"Resolve commands = %d; want 1",
			len(lifecycle.resolveCommands),
		)
	}

	resolveCommand := lifecycle.resolveCommands[0]

	if resolveCommand.IncidentID != verifyingIncident.ID {
		t.Fatalf(
			"Resolve IncidentID = %q; want %q",
			resolveCommand.IncidentID,
			verifyingIncident.ID,
		)
	}
	if resolveCommand.ExpectedVersion !=
		verifyingIncident.Version {
		t.Fatalf(
			"Resolve ExpectedVersion = %d; want %d",
			resolveCommand.ExpectedVersion,
			verifyingIncident.Version,
		)
	}
	if resolveCommand.VerificationID != recovered.ID {
		t.Fatalf(
			"Resolve VerificationID = %q; want %q",
			resolveCommand.VerificationID,
			recovered.ID,
		)
	}
	if resolveCommand.Actor != claim.HolderID {
		t.Fatalf(
			"Resolve Actor = %q; want %q",
			resolveCommand.Actor,
			claim.HolderID,
		)
	}
	if resolveCommand.ReasonCode !=
		verificationRecoveredReasonCode {
		t.Fatalf(
			"Resolve ReasonCode = %q; want %q",
			resolveCommand.ReasonCode,
			verificationRecoveredReasonCode,
		)
	}
}

func TestAgentVerificationWorkerCompletesNonRecoveredOutcomesWithoutResolvingIncident(
	t *testing.T,
) {
	t.Parallel()

	testCases := []struct {
		name         string
		status       remediationdomain.VerificationStatus
		evidenceCode string
	}{
		{
			name: "not_recovered",
			status: remediationdomain.
				VerificationStatusNotRecovered,
			evidenceCode: "WORKLOAD_NOT_RECOVERED",
		},
		{
			name: "inconclusive",
			status: remediationdomain.
				VerificationStatusInconclusive,
			evidenceCode: "RECOVERY_EVIDENCE_INCONCLUSIVE",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			workerAt := time.Date(
				2026,
				time.October,
				12,
				18,
				30,
				0,
				0,
				time.UTC,
			)

			actionKey := remediationdomain.ActionKey{
				IncidentID: "inc-worker-" + testCase.name,
				PlanHash: "sha256:worker-" +
					testCase.name,
				TargetUID: "pod-uid-worker-" +
					testCase.name,
			}
			subject := remediationdomain.VerificationSubject{
				Cluster:   "dev",
				Namespace: "default",
				Kind:      "ReplicaSet",
				Name: "worker-" +
					testCase.name + "-rs",
				UID: "rs-uid-worker-" +
					testCase.name,
			}
			pending := remediationdomain.Verification{
				ID: "ver-worker-" +
					testCase.name,
				ActionAttemptID: "att-worker-" +
					testCase.name,
				ActionKey: actionKey,
				Subject:   subject,
				Status: remediationdomain.
					VerificationStatusPending,
				Version:   1,
				StartedAt: workerAt.Add(-time.Minute),
			}
			verifyingIncident := incidentdomain.Incident{
				ID:      actionKey.IncidentID,
				State:   incidentdomain.StateVerifying,
				Version: 12,
			}
			claim := incidentdomain.Claim{
				Incident: verifyingIncident,
				HolderID: "verification-worker",
				ExpiresAt: workerAt.Add(
					30 * time.Second,
				),
			}

			finishedAt := workerAt
			completedVerification := pending
			completedVerification.Status = testCase.status
			completedVerification.Version++
			completedVerification.FinishedAt = &finishedAt
			completedVerification.EvidenceCode =
				testCase.evidenceCode

			lifecycle := &agentVerificationWorkerRunLifecycleProbe{
				work: incidentdomain.PendingVerificationClaim{
					Verification: pending,
					Claim:        claim,
				},
				completion: incidentdomain.
					FencedVerificationCompletionResult{
					Incident:     verifyingIncident,
					Verification: completedVerification,
				},
			}
			executor := &agentVerificationExecutorProbe{
				result: verificationExecutionResult{
					Status:       testCase.status,
					EvidenceCode: testCase.evidenceCode,
				},
			}

			agent := &sreAgent{
				config: agentConfig{
					IncidentClaimHolderID: claim.HolderID,
					IncidentClaimLeaseDuration: 30 *
						time.Second,
					KubernetesRequestTimeout: time.Second,
				},
				verificationWorker:   lifecycle,
				verificationExecutor: executor,
				now: func() time.Time {
					return workerAt
				},
			}

			incidentResult, advanced, err :=
				agent.runVerificationWorkerOnce(ctx)
			if err != nil {
				t.Fatalf(
					"runVerificationWorkerOnce() error = %v",
					err,
				)
			}
			if !advanced {
				t.Fatal(
					"runVerificationWorkerOnce() advanced = false; " +
						"want true",
				)
			}
			if incidentResult.ID != verifyingIncident.ID {
				t.Fatalf(
					"Incident ID = %q; want %q",
					incidentResult.ID,
					verifyingIncident.ID,
				)
			}
			if incidentResult.State !=
				incidentdomain.StateVerifying {
				t.Fatalf(
					"Incident State = %q; want %q",
					incidentResult.State,
					incidentdomain.StateVerifying,
				)
			}
			if incidentResult.ResolutionVerificationID != "" {
				t.Fatalf(
					"ResolutionVerificationID = %q; want empty",
					incidentResult.ResolutionVerificationID,
				)
			}

			if executor.calls != 1 {
				t.Fatalf(
					"Verification executor calls = %d; want 1",
					executor.calls,
				)
			}
			if lifecycle.recoveredClaimCalls != 1 {
				t.Fatalf(
					"ClaimRecoveredVerificationResolution calls = %d; "+
						"want 1",
					lifecycle.recoveredClaimCalls,
				)
			}
			if lifecycle.claimCalls != 1 {
				t.Fatalf(
					"ClaimPendingVerification calls = %d; want 1",
					lifecycle.claimCalls,
				)
			}
			if lifecycle.completeCalls != 1 {
				t.Fatalf(
					"CompleteFencedVerification calls = %d; want 1",
					lifecycle.completeCalls,
				)
			}
			if lifecycle.resolveCalls != 0 {
				t.Fatalf(
					"Resolve calls = %d; want 0",
					lifecycle.resolveCalls,
				)
			}
			if len(lifecycle.completionCommands) != 1 {
				t.Fatalf(
					"completion commands = %d; want 1",
					len(lifecycle.completionCommands),
				)
			}

			command := lifecycle.completionCommands[0]

			if command.IncidentID != verifyingIncident.ID {
				t.Fatalf(
					"completion IncidentID = %q; want %q",
					command.IncidentID,
					verifyingIncident.ID,
				)
			}
			if command.ExpectedVersion !=
				verifyingIncident.Version {
				t.Fatalf(
					"completion ExpectedVersion = %d; want %d",
					command.ExpectedVersion,
					verifyingIncident.Version,
				)
			}
			if command.HolderID != claim.HolderID {
				t.Fatalf(
					"completion HolderID = %q; want %q",
					command.HolderID,
					claim.HolderID,
				)
			}
			if !command.Now.Equal(workerAt) {
				t.Fatalf(
					"completion Now = %s; want %s",
					command.Now,
					workerAt,
				)
			}
			if command.Verification.ActionKey != actionKey {
				t.Fatalf(
					"completion ActionKey = %#v; want %#v",
					command.Verification.ActionKey,
					actionKey,
				)
			}
			if command.Verification.ExpectedVersion !=
				pending.Version {
				t.Fatalf(
					"completion Verification ExpectedVersion = %d; "+
						"want %d",
					command.Verification.ExpectedVersion,
					pending.Version,
				)
			}
			if command.Verification.To != testCase.status {
				t.Fatalf(
					"completion status = %q; want %q",
					command.Verification.To,
					testCase.status,
				)
			}
			if command.Verification.EvidenceCode !=
				testCase.evidenceCode {
				t.Fatalf(
					"completion EvidenceCode = %q; want %q",
					command.Verification.EvidenceCode,
					testCase.evidenceCode,
				)
			}
			if !command.Verification.FinishedAt.Equal(
				workerAt,
			) {
				t.Fatalf(
					"completion FinishedAt = %s; want %s",
					command.Verification.FinishedAt,
					workerAt,
				)
			}
		})
	}
}
