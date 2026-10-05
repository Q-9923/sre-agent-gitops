package incident_test

import (
	"context"
	"testing"
	"time"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func TestRegistryResolveVerifyingIncidentWithPersistedRecoveredVerification(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	actionAttempts := remediationdomain.NewMemoryActionAttemptStore()
	verifications := remediationdomain.NewMemoryVerificationStore()

	registry := incidentdomain.NewRegistryWithRecoveryEvidenceStore(
		verifications,
	)

	observation := incidentdomain.Observation{
		Source:    "prometheus",
		Cluster:   "cluster-a",
		AlertName: "KubePodCrashLooping",
		Target: incidentdomain.Target{
			Kind:      "Pod",
			Namespace: "default",
			Name:      "crash-app",
			UID:       "pod-uid-resolution-evidence",
		},
	}

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed := transitionIncident(
		t,
		ctx,
		registry,
		detected,
		incidentdomain.StateDiagnosed,
		"DIAGNOSIS_COMPLETED",
	)

	verifying := transitionIncident(
		t,
		ctx,
		registry,
		diagnosed,
		incidentdomain.StateVerifying,
		"VERIFICATION_STARTED",
	)

	actionStartedAt := time.Date(
		2026,
		time.October,
		5,
		12,
		0,
		0,
		0,
		time.UTC,
	)
	actionFinishedAt := actionStartedAt.Add(5 * time.Second)

	executionKey := remediationdomain.ExecutionKey{
		IncidentID:   verifying.ID,
		PlanHash:     "sha256:47d7c89d25c02591b866f56234c8a4af4a82742e5c73ec68b910e9ce10a48472",
		TargetUID:    observation.Target.UID,
		FencingToken: 7,
	}

	startedAttempt, created, err := actionAttempts.Begin(
		ctx,
		remediationdomain.BeginActionAttemptCommand{
			Key:       executionKey,
			StartedAt: actionStartedAt,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !created {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             executionKey,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: actionFinishedAt,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	verificationStartedAt := actionFinishedAt.Add(time.Second)

	pendingVerification, created, err := verifications.Begin(
		ctx,
		remediationdomain.BeginVerificationCommand{
			ActionAttempt: succeededAttempt,
			Subject: remediationdomain.VerificationSubject{
				Cluster:   observation.Cluster,
				Namespace: observation.Target.Namespace,
				Kind:      "Deployment",
				Name:      "crash-app",
				UID:       "deployment-uid-resolution-evidence",
			},
			StartedAt: verificationStartedAt,
		},
	)
	if err != nil {
		t.Fatalf("Verification Begin() error = %v", err)
	}
	if !created {
		t.Fatal("Verification Begin() created = false; want true")
	}

	recoveredVerification, err := verifications.Complete(
		ctx,
		remediationdomain.CompleteVerificationCommand{
			ActionKey:       executionKey.ActionKey(),
			ExpectedVersion: pendingVerification.Version,
			To: remediationdomain.
				VerificationStatusRecovered,
			FinishedAt: verificationStartedAt.Add(
				30 * time.Second,
			),
			EvidenceCode: "WORKLOAD_AVAILABLE",
		},
	)
	if err != nil {
		t.Fatalf("Verification Complete() error = %v", err)
	}

	resolved, err := registry.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      verifying.ID,
			ExpectedVersion: verifying.Version,
			VerificationID:  recoveredVerification.ID,
			Actor:           "verification-controller",
			ReasonCode:      "RECOVERY_VERIFIED",
		},
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v; want nil", err)
	}

	if resolved.ID != verifying.ID {
		t.Fatalf(
			"resolved Incident ID = %q; want %q",
			resolved.ID,
			verifying.ID,
		)
	}
	if resolved.State != incidentdomain.StateResolved {
		t.Fatalf(
			"resolved State = %q; want %q",
			resolved.State,
			incidentdomain.StateResolved,
		)
	}
	if resolved.Version != verifying.Version+1 {
		t.Fatalf(
			"resolved Version = %d; want %d",
			resolved.Version,
			verifying.Version+1,
		)
	}
	if resolved.ResolutionVerificationID != recoveredVerification.ID {
		t.Fatalf(
			"ResolutionVerificationID = %q; want %q",
			resolved.ResolutionVerificationID,
			recoveredVerification.ID,
		)
	}
}

func transitionIncident(
	t *testing.T,
	ctx context.Context,
	registry *incidentdomain.Registry,
	current incidentdomain.Incident,
	target incidentdomain.State,
	reasonCode string,
) incidentdomain.Incident {
	t.Helper()

	updated, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      current.ID,
			ExpectedVersion: current.Version,
			To:              target,
			Actor:           "sre-agent",
			ReasonCode:      reasonCode,
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(%s -> %s) error = %v",
			current.State,
			target,
			err,
		)
	}

	return updated
}
