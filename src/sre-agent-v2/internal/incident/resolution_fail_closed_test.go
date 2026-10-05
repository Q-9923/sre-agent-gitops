package incident_test

import (
	"context"
	"errors"
	"testing"
	"time"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func TestRegistryResolveRejectsPersistedPendingVerification(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(
		2026,
		time.October,
		5,
		14,
		0,
		0,
		0,
		time.UTC,
	)

	verifications := remediationdomain.NewMemoryVerificationStore()
	registry := incidentdomain.NewRegistryWithRecoveryEvidenceStore(
		verifications,
	)

	observation := incidentdomain.Observation{
		Source:    "prometheus",
		Cluster:   "development",
		AlertName: "PodCrashLooping",
		Target: incidentdomain.Target{
			Kind:      "Pod",
			Namespace: "default",
			Name:      "pending-verification-pod",
			UID:       "pod-uid-pending-verification",
		},
	}

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Incident Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Incident Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "test-suite",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v",
			err,
		)
	}

	verifying, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              incidentdomain.StateVerifying,
			Actor:           "test-suite",
			ReasonCode:      "REMEDIATION_SUBMITTED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DIAGNOSED -> VERIFYING) error = %v",
			err,
		)
	}

	actionAttempts := remediationdomain.NewMemoryActionAttemptStore()
	executionKey := remediationdomain.ExecutionKey{
		IncidentID: verifying.ID,
		PlanHash: "sha256:" +
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetUID:    observation.Target.UID,
		FencingToken: 1,
	}

	startedAttempt, actionCreated, err := actionAttempts.Begin(
		ctx,
		remediationdomain.BeginActionAttemptCommand{
			Key:       executionKey,
			StartedAt: now,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !actionCreated {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             executionKey,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: now.Add(5 * time.Second),
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	pendingVerification, verificationCreated, err := verifications.Begin(
		ctx,
		remediationdomain.BeginVerificationCommand{
			ActionAttempt: succeededAttempt,
			Subject: remediationdomain.VerificationSubject{
				Cluster:   observation.Cluster,
				Namespace: observation.Target.Namespace,
				Kind:      observation.Target.Kind,
				Name:      observation.Target.Name,
				UID:       observation.Target.UID,
			},
			StartedAt: now.Add(6 * time.Second),
		},
	)
	if err != nil {
		t.Fatalf("Verification Begin() error = %v", err)
	}
	if !verificationCreated {
		t.Fatal("Verification Begin() created = false; want true")
	}
	if pendingVerification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"Verification Status = %q; want %q",
			pendingVerification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}

	_, err = registry.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      verifying.ID,
			ExpectedVersion: verifying.Version,
			VerificationID:  pendingVerification.ID,
			Actor:           "test-suite",
			ReasonCode:      "RECOVERY_VERIFIED",
		},
	)
	if !errors.Is(
		err,
		remediationdomain.ErrVerificationNotRecovered,
	) {
		t.Fatalf(
			"Resolve() error = %v; want ErrVerificationNotRecovered",
			err,
		)
	}

	current, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("second Incident Observe() error = %v", err)
	}
	if created {
		t.Fatal(
			"second Incident Observe() created = true; " +
				"want existing active Incident",
		)
	}
	if current.ID != verifying.ID {
		t.Fatalf(
			"Incident ID = %q; want %q",
			current.ID,
			verifying.ID,
		)
	}
	if current.State != incidentdomain.StateVerifying {
		t.Fatalf(
			"Incident State = %q; want %q",
			current.State,
			incidentdomain.StateVerifying,
		)
	}
	if current.Version != verifying.Version {
		t.Fatalf(
			"Incident Version = %d; want unchanged %d",
			current.Version,
			verifying.Version,
		)
	}
	if current.ResolutionVerificationID != "" {
		t.Fatalf(
			"ResolutionVerificationID = %q; want empty",
			current.ResolutionVerificationID,
		)
	}
}
func TestRegistryResolveRejectsMissingVerification(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	verifications := remediationdomain.NewMemoryVerificationStore()
	registry := incidentdomain.NewRegistryWithRecoveryEvidenceStore(
		verifications,
	)

	observation := incidentdomain.Observation{
		Source:    "prometheus",
		Cluster:   "development",
		AlertName: "PodCrashLooping",
		Target: incidentdomain.Target{
			Kind:      "Pod",
			Namespace: "default",
			Name:      "missing-verification-pod",
			UID:       "pod-uid-missing-verification",
		},
	}

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Incident Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Incident Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "test-suite",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v",
			err,
		)
	}

	verifying, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              incidentdomain.StateVerifying,
			Actor:           "test-suite",
			ReasonCode:      "REMEDIATION_SUBMITTED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DIAGNOSED -> VERIFYING) error = %v",
			err,
		)
	}

	_, err = registry.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      verifying.ID,
			ExpectedVersion: verifying.Version,
			VerificationID:  "ver-missing",
			Actor:           "test-suite",
			ReasonCode:      "RECOVERY_VERIFIED",
		},
	)
	if !errors.Is(
		err,
		remediationdomain.ErrVerificationNotFound,
	) {
		t.Fatalf(
			"Resolve() error = %v; want ErrVerificationNotFound",
			err,
		)
	}

	current, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("second Incident Observe() error = %v", err)
	}
	if created {
		t.Fatal(
			"second Incident Observe() created = true; " +
				"want existing active Incident",
		)
	}
	if current.ID != verifying.ID {
		t.Fatalf(
			"Incident ID = %q; want %q",
			current.ID,
			verifying.ID,
		)
	}
	if current.State != incidentdomain.StateVerifying {
		t.Fatalf(
			"Incident State = %q; want %q",
			current.State,
			incidentdomain.StateVerifying,
		)
	}
	if current.Version != verifying.Version {
		t.Fatalf(
			"Incident Version = %d; want unchanged %d",
			current.Version,
			verifying.Version,
		)
	}
	if current.ResolutionVerificationID != "" {
		t.Fatalf(
			"ResolutionVerificationID = %q; want empty",
			current.ResolutionVerificationID,
		)
	}
}

func TestRegistryResolveRejectsRecoveredVerificationFromAnotherIncident(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(
		2026,
		time.October,
		5,
		15,
		0,
		0,
		0,
		time.UTC,
	)

	verifications := remediationdomain.NewMemoryVerificationStore()
	registry := incidentdomain.NewRegistryWithRecoveryEvidenceStore(
		verifications,
	)

	currentObservation := incidentdomain.Observation{
		Source:    "prometheus",
		Cluster:   "development",
		AlertName: "PodCrashLooping",
		Target: incidentdomain.Target{
			Kind:      "Pod",
			Namespace: "default",
			Name:      "current-incident-pod",
			UID:       "pod-uid-current-incident",
		},
	}

	detected, created, err := registry.Observe(
		ctx,
		currentObservation,
	)
	if err != nil {
		t.Fatalf("current Incident Observe() error = %v", err)
	}
	if !created {
		t.Fatal("current Incident Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "test-suite",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v",
			err,
		)
	}

	verifying, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              incidentdomain.StateVerifying,
			Actor:           "test-suite",
			ReasonCode:      "REMEDIATION_SUBMITTED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DIAGNOSED -> VERIFYING) error = %v",
			err,
		)
	}

	otherObservation := incidentdomain.Observation{
		Source:    "prometheus",
		Cluster:   "development",
		AlertName: "PodCrashLooping",
		Target: incidentdomain.Target{
			Kind:      "Pod",
			Namespace: "default",
			Name:      "other-incident-pod",
			UID:       "pod-uid-other-incident",
		},
	}

	otherIncident, created, err := registry.Observe(
		ctx,
		otherObservation,
	)
	if err != nil {
		t.Fatalf("other Incident Observe() error = %v", err)
	}
	if !created {
		t.Fatal("other Incident Observe() created = false; want true")
	}

	actionAttempts := remediationdomain.NewMemoryActionAttemptStore()
	executionKey := remediationdomain.ExecutionKey{
		IncidentID: otherIncident.ID,
		PlanHash: "sha256:" +
			"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" +
			"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TargetUID:    otherObservation.Target.UID,
		FencingToken: 1,
	}

	startedAttempt, actionCreated, err := actionAttempts.Begin(
		ctx,
		remediationdomain.BeginActionAttemptCommand{
			Key:       executionKey,
			StartedAt: now,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !actionCreated {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             executionKey,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: now.Add(5 * time.Second),
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	pendingVerification, verificationCreated, err := verifications.Begin(
		ctx,
		remediationdomain.BeginVerificationCommand{
			ActionAttempt: succeededAttempt,
			Subject: remediationdomain.VerificationSubject{
				Cluster:   otherObservation.Cluster,
				Namespace: otherObservation.Target.Namespace,
				Kind:      otherObservation.Target.Kind,
				Name:      otherObservation.Target.Name,
				UID:       otherObservation.Target.UID,
			},
			StartedAt: now.Add(6 * time.Second),
		},
	)
	if err != nil {
		t.Fatalf("Verification Begin() error = %v", err)
	}
	if !verificationCreated {
		t.Fatal("Verification Begin() created = false; want true")
	}

	recoveredVerification, err := verifications.Complete(
		ctx,
		remediationdomain.CompleteVerificationCommand{
			ActionKey:       pendingVerification.ActionKey,
			ExpectedVersion: pendingVerification.Version,
			To: remediationdomain.
				VerificationStatusRecovered,
			FinishedAt:   now.Add(10 * time.Second),
			EvidenceCode: "WORKLOAD_READY",
		},
	)
	if err != nil {
		t.Fatalf("Verification Complete() error = %v", err)
	}

	_, err = registry.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      verifying.ID,
			ExpectedVersion: verifying.Version,
			VerificationID:  recoveredVerification.ID,
			Actor:           "test-suite",
			ReasonCode:      "RECOVERY_VERIFIED",
		},
	)
	if !errors.Is(
		err,
		remediationdomain.ErrVerificationNotFound,
	) {
		t.Fatalf(
			"Resolve() error = %v; want ErrVerificationNotFound",
			err,
		)
	}

	current, created, err := registry.Observe(
		ctx,
		currentObservation,
	)
	if err != nil {
		t.Fatalf("current Incident re-observe error = %v", err)
	}
	if created {
		t.Fatal(
			"current Incident re-observe created = true; " +
				"want existing active Incident",
		)
	}
	if current.ID != verifying.ID {
		t.Fatalf(
			"Incident ID = %q; want %q",
			current.ID,
			verifying.ID,
		)
	}
	if current.State != incidentdomain.StateVerifying {
		t.Fatalf(
			"Incident State = %q; want %q",
			current.State,
			incidentdomain.StateVerifying,
		)
	}
	if current.Version != verifying.Version {
		t.Fatalf(
			"Incident Version = %d; want unchanged %d",
			current.Version,
			verifying.Version,
		)
	}
	if current.ResolutionVerificationID != "" {
		t.Fatalf(
			"ResolutionVerificationID = %q; want empty",
			current.ResolutionVerificationID,
		)
	}
}
func TestRegistryResolveFailsClosedWithoutRecoveryEvidenceStore(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	registry := incidentdomain.NewMemoryRegistry()

	observation := incidentdomain.Observation{
		Source:    "prometheus",
		Cluster:   "development",
		AlertName: "PodCrashLooping",
		Target: incidentdomain.Target{
			Kind:      "Pod",
			Namespace: "default",
			Name:      "unavailable-evidence-store-pod",
			UID:       "pod-uid-unavailable-evidence-store",
		},
	}

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Incident Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Incident Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "test-suite",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v",
			err,
		)
	}

	verifying, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              incidentdomain.StateVerifying,
			Actor:           "test-suite",
			ReasonCode:      "REMEDIATION_SUBMITTED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DIAGNOSED -> VERIFYING) error = %v",
			err,
		)
	}

	_, err = registry.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      verifying.ID,
			ExpectedVersion: verifying.Version,
			VerificationID:  "ver-store-unavailable",
			Actor:           "test-suite",
			ReasonCode:      "RECOVERY_VERIFIED",
		},
	)
	if !errors.Is(
		err,
		incidentdomain.ErrRecoveryEvidenceUnavailable,
	) {
		t.Fatalf(
			"Resolve() error = %v; "+
				"want ErrRecoveryEvidenceUnavailable",
			err,
		)
	}

	current, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("second Incident Observe() error = %v", err)
	}
	if created {
		t.Fatal(
			"second Incident Observe() created = true; " +
				"want existing active Incident",
		)
	}
	if current.ID != verifying.ID {
		t.Fatalf(
			"Incident ID = %q; want %q",
			current.ID,
			verifying.ID,
		)
	}
	if current.State != incidentdomain.StateVerifying {
		t.Fatalf(
			"Incident State = %q; want %q",
			current.State,
			incidentdomain.StateVerifying,
		)
	}
	if current.Version != verifying.Version {
		t.Fatalf(
			"Incident Version = %d; want unchanged %d",
			current.Version,
			verifying.Version,
		)
	}
	if current.ResolutionVerificationID != "" {
		t.Fatalf(
			"ResolutionVerificationID = %q; want empty",
			current.ResolutionVerificationID,
		)
	}
}
func TestRegistryResolveRejectsIncidentThatIsNotVerifying(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(
		2026,
		time.October,
		5,
		16,
		0,
		0,
		0,
		time.UTC,
	)

	verifications := remediationdomain.NewMemoryVerificationStore()
	registry := incidentdomain.NewRegistryWithRecoveryEvidenceStore(
		verifications,
	)

	observation := incidentdomain.Observation{
		Source:    "prometheus",
		Cluster:   "development",
		AlertName: "PodCrashLooping",
		Target: incidentdomain.Target{
			Kind:      "Pod",
			Namespace: "default",
			Name:      "diagnosed-incident-pod",
			UID:       "pod-uid-diagnosed-incident",
		},
	}

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Incident Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Incident Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "test-suite",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v",
			err,
		)
	}

	actionAttempts := remediationdomain.NewMemoryActionAttemptStore()
	executionKey := remediationdomain.ExecutionKey{
		IncidentID: diagnosed.ID,
		PlanHash: "sha256:" +
			"cccccccccccccccccccccccccccccccc" +
			"cccccccccccccccccccccccccccccccc",
		TargetUID:    observation.Target.UID,
		FencingToken: 1,
	}

	startedAttempt, actionCreated, err := actionAttempts.Begin(
		ctx,
		remediationdomain.BeginActionAttemptCommand{
			Key:       executionKey,
			StartedAt: now,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !actionCreated {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             executionKey,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: now.Add(5 * time.Second),
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	pendingVerification, verificationCreated, err := verifications.Begin(
		ctx,
		remediationdomain.BeginVerificationCommand{
			ActionAttempt: succeededAttempt,
			Subject: remediationdomain.VerificationSubject{
				Cluster:   observation.Cluster,
				Namespace: observation.Target.Namespace,
				Kind:      observation.Target.Kind,
				Name:      observation.Target.Name,
				UID:       observation.Target.UID,
			},
			StartedAt: now.Add(6 * time.Second),
		},
	)
	if err != nil {
		t.Fatalf("Verification Begin() error = %v", err)
	}
	if !verificationCreated {
		t.Fatal("Verification Begin() created = false; want true")
	}

	recoveredVerification, err := verifications.Complete(
		ctx,
		remediationdomain.CompleteVerificationCommand{
			ActionKey:       pendingVerification.ActionKey,
			ExpectedVersion: pendingVerification.Version,
			To: remediationdomain.
				VerificationStatusRecovered,
			FinishedAt:   now.Add(10 * time.Second),
			EvidenceCode: "WORKLOAD_READY",
		},
	)
	if err != nil {
		t.Fatalf("Verification Complete() error = %v", err)
	}

	_, err = registry.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			VerificationID:  recoveredVerification.ID,
			Actor:           "test-suite",
			ReasonCode:      "RECOVERY_VERIFIED",
		},
	)
	if !errors.Is(err, incidentdomain.ErrInvalidTransition) {
		t.Fatalf(
			"Resolve() error = %v; want ErrInvalidTransition",
			err,
		)
	}

	current, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("second Incident Observe() error = %v", err)
	}
	if created {
		t.Fatal(
			"second Incident Observe() created = true; " +
				"want existing active Incident",
		)
	}
	if current.ID != diagnosed.ID {
		t.Fatalf(
			"Incident ID = %q; want %q",
			current.ID,
			diagnosed.ID,
		)
	}
	if current.State != incidentdomain.StateDiagnosed {
		t.Fatalf(
			"Incident State = %q; want %q",
			current.State,
			incidentdomain.StateDiagnosed,
		)
	}
	if current.Version != diagnosed.Version {
		t.Fatalf(
			"Incident Version = %d; want unchanged %d",
			current.Version,
			diagnosed.Version,
		)
	}
	if current.ResolutionVerificationID != "" {
		t.Fatalf(
			"ResolutionVerificationID = %q; want empty",
			current.ResolutionVerificationID,
		)
	}
}
func TestRegistryResolveRejectsStaleIncidentVersion(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(
		2026,
		time.October,
		5,
		17,
		0,
		0,
		0,
		time.UTC,
	)

	verifications := remediationdomain.NewMemoryVerificationStore()
	registry := incidentdomain.NewRegistryWithRecoveryEvidenceStore(
		verifications,
	)

	observation := incidentdomain.Observation{
		Source:    "prometheus",
		Cluster:   "development",
		AlertName: "PodCrashLooping",
		Target: incidentdomain.Target{
			Kind:      "Pod",
			Namespace: "default",
			Name:      "stale-resolution-pod",
			UID:       "pod-uid-stale-resolution",
		},
	}

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Incident Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Incident Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "test-suite",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v",
			err,
		)
	}

	verifying, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              incidentdomain.StateVerifying,
			Actor:           "test-suite",
			ReasonCode:      "REMEDIATION_SUBMITTED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DIAGNOSED -> VERIFYING) error = %v",
			err,
		)
	}

	actionAttempts := remediationdomain.NewMemoryActionAttemptStore()
	executionKey := remediationdomain.ExecutionKey{
		IncidentID: verifying.ID,
		PlanHash: "sha256:" +
			"dddddddddddddddddddddddddddddddd" +
			"dddddddddddddddddddddddddddddddd",
		TargetUID:    observation.Target.UID,
		FencingToken: 1,
	}

	startedAttempt, actionCreated, err := actionAttempts.Begin(
		ctx,
		remediationdomain.BeginActionAttemptCommand{
			Key:       executionKey,
			StartedAt: now,
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Begin() error = %v", err)
	}
	if !actionCreated {
		t.Fatal("ActionAttempt Begin() created = false; want true")
	}

	succeededAttempt, err := actionAttempts.Complete(
		ctx,
		remediationdomain.CompleteActionAttemptCommand{
			Key:             executionKey,
			ExpectedVersion: startedAttempt.Version,
			To: remediationdomain.
				ActionAttemptStatusSucceeded,
			FinishedAt: now.Add(5 * time.Second),
		},
	)
	if err != nil {
		t.Fatalf("ActionAttempt Complete() error = %v", err)
	}

	pendingVerification, verificationCreated, err := verifications.Begin(
		ctx,
		remediationdomain.BeginVerificationCommand{
			ActionAttempt: succeededAttempt,
			Subject: remediationdomain.VerificationSubject{
				Cluster:   observation.Cluster,
				Namespace: observation.Target.Namespace,
				Kind:      observation.Target.Kind,
				Name:      observation.Target.Name,
				UID:       observation.Target.UID,
			},
			StartedAt: now.Add(6 * time.Second),
		},
	)
	if err != nil {
		t.Fatalf("Verification Begin() error = %v", err)
	}
	if !verificationCreated {
		t.Fatal("Verification Begin() created = false; want true")
	}

	recoveredVerification, err := verifications.Complete(
		ctx,
		remediationdomain.CompleteVerificationCommand{
			ActionKey:       pendingVerification.ActionKey,
			ExpectedVersion: pendingVerification.Version,
			To: remediationdomain.
				VerificationStatusRecovered,
			FinishedAt:   now.Add(10 * time.Second),
			EvidenceCode: "WORKLOAD_READY",
		},
	)
	if err != nil {
		t.Fatalf("Verification Complete() error = %v", err)
	}

	_, err = registry.Resolve(
		ctx,
		incidentdomain.ResolveCommand{
			IncidentID:      verifying.ID,
			ExpectedVersion: diagnosed.Version,
			VerificationID:  recoveredVerification.ID,
			Actor:           "test-suite",
			ReasonCode:      "RECOVERY_VERIFIED",
		},
	)
	if !errors.Is(err, incidentdomain.ErrVersionConflict) {
		t.Fatalf(
			"Resolve() error = %v; want ErrVersionConflict",
			err,
		)
	}

	current, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("second Incident Observe() error = %v", err)
	}
	if created {
		t.Fatal(
			"second Incident Observe() created = true; " +
				"want existing active Incident",
		)
	}
	if current.ID != verifying.ID {
		t.Fatalf(
			"Incident ID = %q; want %q",
			current.ID,
			verifying.ID,
		)
	}
	if current.State != incidentdomain.StateVerifying {
		t.Fatalf(
			"Incident State = %q; want %q",
			current.State,
			incidentdomain.StateVerifying,
		)
	}
	if current.Version != verifying.Version {
		t.Fatalf(
			"Incident Version = %d; want unchanged %d",
			current.Version,
			verifying.Version,
		)
	}
	if current.ResolutionVerificationID != "" {
		t.Fatalf(
			"ResolutionVerificationID = %q; want empty",
			current.ResolutionVerificationID,
		)
	}
}
