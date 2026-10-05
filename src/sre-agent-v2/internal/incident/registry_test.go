package incident

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRegistryObserveReturnsExistingActiveIncidentForSameIdempotencyKey(
	t *testing.T,
) {
	t.Parallel()

	registry := NewMemoryRegistry()

	observation := Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "PodCrashLooping",
		Target: Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app-abc",
			UID:       "pod-uid-a",
		},
	}

	first, firstCreated, err := registry.Observe(
		context.Background(),
		observation,
	)
	if err != nil {
		t.Fatalf("first Observe() error = %v", err)
	}
	if !firstCreated {
		t.Fatal("first Observe() created = false; want true")
	}
	if first.ID == "" {
		t.Fatal("first Incident ID is empty")
	}
	if first.State != StateDetected {
		t.Fatalf("first State = %q; want %q", first.State, StateDetected)
	}

	second, secondCreated, err := registry.Observe(
		context.Background(),
		observation,
	)
	if err != nil {
		t.Fatalf("second Observe() error = %v", err)
	}
	if secondCreated {
		t.Fatal("second Observe() created = true; want false")
	}
	if second.ID != first.ID {
		t.Fatalf(
			"second Incident ID = %q; want existing ID %q",
			second.ID,
			first.ID,
		)
	}
	if second.State != StateDetected {
		t.Fatalf("second State = %q; want %q", second.State, StateDetected)
	}
}
func TestRegistryObserveCreatesNewIncidentAfterPreviousIncidentIsResolved(
	t *testing.T,
) {
	t.Parallel()

	registry := NewMemoryRegistry()
	ctx := context.Background()

	observation := Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "PodCrashLooping",
		Target: Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app-abc",
			UID:       "pod-uid-a",
		},
	}

	first, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("first Observe() error = %v", err)
	}
	if !created {
		t.Fatal("first Observe() created = false; want true")
	}

	resolved, err := registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      first.ID,
			ExpectedVersion: first.Version,
			To:              StateResolved,
			Actor:           "test",
			ReasonCode:      "ALERT_RESOLVED",
		},
	)
	if err != nil {
		t.Fatalf("Transition(RESOLVED) error = %v", err)
	}
	if resolved.State != StateResolved {
		t.Fatalf(
			"resolved State = %q; want %q",
			resolved.State,
			StateResolved,
		)
	}

	second, secondCreated, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("second Observe() error = %v", err)
	}
	if !secondCreated {
		t.Fatal("second Observe() created = false; want true after resolution")
	}
	if second.ID == first.ID {
		t.Fatalf(
			"second Incident ID = %q; want a new Incident after resolution",
			second.ID,
		)
	}
	if second.State != StateDetected {
		t.Fatalf("second State = %q; want %q", second.State, StateDetected)
	}
}
func TestRegistryTransitionRejectsStaleExpectedVersion(t *testing.T) {
	t.Parallel()

	registry := NewMemoryRegistry()
	ctx := context.Background()

	observation := Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "PodCrashLooping",
		Target: Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app-abc",
			UID:       "pod-uid-a",
		},
	}

	current, _, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}

	command := TransitionCommand{
		IncidentID:      current.ID,
		ExpectedVersion: current.Version,
		To:              StateResolved,
		Actor:           "test",
		ReasonCode:      "ALERT_RESOLVED",
	}

	updated, err := registry.Transition(ctx, command)
	if err != nil {
		t.Fatalf("first Transition() error = %v", err)
	}
	if updated.Version != current.Version+1 {
		t.Fatalf(
			"updated Version = %d; want %d",
			updated.Version,
			current.Version+1,
		)
	}

	_, err = registry.Transition(ctx, command)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf(
			"stale Transition() error = %v; want ErrVersionConflict",
			err,
		)
	}
}
func TestRegistryObserveRejectsObservationWithoutTargetUID(t *testing.T) {
	t.Parallel()

	registry := NewMemoryRegistry()
	ctx := context.Background()

	observation := Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "PodCrashLooping",
		Target: Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app-abc",
		},
	}

	incident, created, err := registry.Observe(ctx, observation)
	if !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf(
			"Observe() error = %v; want ErrInvalidObservation",
			err,
		)
	}
	if created {
		t.Fatal("Observe() created = true; want false")
	}
	if incident.ID != "" {
		t.Fatalf("Observe() Incident ID = %q; want empty", incident.ID)
	}

	observation.Target.UID = "pod-uid-a"

	valid, validCreated, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("valid Observe() error = %v", err)
	}
	if !validCreated {
		t.Fatal("valid Observe() created = false; want true")
	}
	if valid.ID == "" {
		t.Fatal("valid Observe() Incident ID is empty")
	}
}
func TestObservationIdempotencyKeyIsStableAndTargetUIDSensitive(
	t *testing.T,
) {
	t.Parallel()

	observation := Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "KubePodCrashLooping",
		Target: Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app",
			UID:       "pod-uid-1",
		},
	}

	first, err := observation.IdempotencyKey()
	if err != nil {
		t.Fatalf("first IdempotencyKey() error = %v", err)
	}

	second, err := observation.IdempotencyKey()
	if err != nil {
		t.Fatalf("second IdempotencyKey() error = %v", err)
	}

	if first == "" {
		t.Fatal("IdempotencyKey() returned an empty key")
	}

	if second != first {
		t.Fatalf(
			"second key = %q; want %q",
			second,
			first,
		)
	}

	changed := observation
	changed.Target.UID = "pod-uid-2"

	changedKey, err := changed.IdempotencyKey()
	if err != nil {
		t.Fatalf("changed IdempotencyKey() error = %v", err)
	}

	if changedKey == first {
		t.Fatal("different target UIDs produced the same key")
	}

	invalid := observation
	invalid.Target.UID = " "

	if _, err := invalid.IdempotencyKey(); err == nil {
		t.Fatal("IdempotencyKey() accepted an empty target UID")
	}
}
func TestRegistryTransitionRequiresAuditIdentity(t *testing.T) {
	t.Parallel()

	registry := NewMemoryRegistry()
	ctx := context.Background()

	current, created, err := registry.Observe(
		ctx,
		Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "KubePodCrashLooping",
			Target: Target{
				Kind:      "Pod",
				Namespace: "sre-agent-lab",
				Name:      "crash-app",
				UID:       "pod-uid-audit-required",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	tests := []struct {
		name       string
		actor      string
		reasonCode string
	}{
		{
			name:       "missing actor",
			actor:      " ",
			reasonCode: "ALERT_RESOLVED",
		},
		{
			name:       "missing reason code",
			actor:      "sre-agent",
			reasonCode: " ",
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			_, transitionErr := registry.Transition(
				ctx,
				TransitionCommand{
					IncidentID:      current.ID,
					ExpectedVersion: current.Version,
					To:              StateResolved,
					Actor:           testCase.actor,
					ReasonCode:      testCase.reasonCode,
				},
			)

			if !errors.Is(
				transitionErr,
				ErrInvalidTransition,
			) {
				t.Fatalf(
					"Transition() error = %v; want ErrInvalidTransition",
					transitionErr,
				)
			}
		})
	}
}
func TestRegistryTransitionToDiagnosedKeepsIncidentActive(
	t *testing.T,
) {
	t.Parallel()

	registry := NewMemoryRegistry()
	ctx := context.Background()
	observation := Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "PodCrashLooping",
		Target: Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app-diagnosed",
			UID:       "pod-uid-diagnosed",
		},
	}

	detected, created, err := registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}
	if detected.State != StateDetected {
		t.Fatalf(
			"detected State = %q; want %q",
			detected.State,
			StateDetected,
		)
	}
	if detected.Version != 1 {
		t.Fatalf(
			"detected Version = %d; want 1",
			detected.Version,
		)
	}

	diagnosed, err := registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: 1,
			To:              StateDiagnosed,
			Actor:           "sre-agent",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf("Transition() error = %v; want nil", err)
	}
	if diagnosed.ID != detected.ID {
		t.Fatalf(
			"diagnosed Incident ID = %q; want %q",
			diagnosed.ID,
			detected.ID,
		)
	}
	if diagnosed.State != StateDiagnosed {
		t.Fatalf(
			"diagnosed State = %q; want %q",
			diagnosed.State,
			StateDiagnosed,
		)
	}
	if diagnosed.Version != 2 {
		t.Fatalf(
			"diagnosed Version = %d; want 2",
			diagnosed.Version,
		)
	}

	observedAgain, createdAgain, err := registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("second Observe() error = %v; want nil", err)
	}
	if createdAgain {
		t.Fatal(
			"second Observe() created = true; " +
				"want false while the incident remains active",
		)
	}
	if observedAgain.ID != detected.ID {
		t.Fatalf(
			"second Observe() Incident ID = %q; want %q",
			observedAgain.ID,
			detected.ID,
		)
	}
	if observedAgain.State != StateDiagnosed {
		t.Fatalf(
			"second Observe() State = %q; want %q",
			observedAgain.State,
			StateDiagnosed,
		)
	}
	if observedAgain.Version != 2 {
		t.Fatalf(
			"second Observe() Version = %d; want 2",
			observedAgain.Version,
		)
	}
}
func TestRegistryTransitionFromDiagnosedToResolvedReleasesIncident(
	t *testing.T,
) {
	t.Parallel()

	registry := NewMemoryRegistry()
	ctx := context.Background()
	observation := Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "PodCrashLooping",
		Target: Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app-recovered",
			UID:       "pod-uid-recovered",
		},
	}

	detected, created, err := registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: 1,
			To:              StateDiagnosed,
			Actor:           "sre-agent",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"transition to DIAGNOSED error = %v; want nil",
			err,
		)
	}
	if diagnosed.State != StateDiagnosed {
		t.Fatalf(
			"diagnosed State = %q; want %q",
			diagnosed.State,
			StateDiagnosed,
		)
	}
	if diagnosed.Version != 2 {
		t.Fatalf(
			"diagnosed Version = %d; want 2",
			diagnosed.Version,
		)
	}

	resolved, err := registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: 2,
			To:              StateResolved,
			Actor:           "sre-agent",
			ReasonCode:      "RECOVERY_VERIFIED",
		},
	)
	if err != nil {
		t.Fatalf(
			"transition to RESOLVED error = %v; want nil",
			err,
		)
	}
	if resolved.ID != detected.ID {
		t.Fatalf(
			"resolved Incident ID = %q; want %q",
			resolved.ID,
			detected.ID,
		)
	}
	if resolved.State != StateResolved {
		t.Fatalf(
			"resolved State = %q; want %q",
			resolved.State,
			StateResolved,
		)
	}
	if resolved.Version != 3 {
		t.Fatalf(
			"resolved Version = %d; want 3",
			resolved.Version,
		)
	}

	recurrent, recurrentCreated, err := registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("recurrent Observe() error = %v; want nil", err)
	}
	if !recurrentCreated {
		t.Fatal(
			"recurrent Observe() created = false; " +
				"want true after the previous incident was resolved",
		)
	}
	if recurrent.ID == resolved.ID {
		t.Fatalf(
			"recurrent Incident ID = %q; want a new ID",
			recurrent.ID,
		)
	}
	if recurrent.State != StateDetected {
		t.Fatalf(
			"recurrent State = %q; want %q",
			recurrent.State,
			StateDetected,
		)
	}
	if recurrent.Version != 1 {
		t.Fatalf(
			"recurrent Version = %d; want 1",
			recurrent.Version,
		)
	}
}
func TestRegistryClaimAllowsOneConcurrentVersionWinner(t *testing.T) {
	t.Parallel()

	registry := NewMemoryRegistry()
	ctx := context.Background()

	observed, created, err := registry.Observe(
		ctx,
		Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "PodCrashLooping",
			Target: Target{
				Kind:      "Pod",
				Namespace: "sre-agent-lab",
				Name:      "crash-app-claim",
				UID:       "pod-uid-claim",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	leaseDuration := 30 * time.Second
	holders := []string{"agent-a", "agent-b"}

	type claimResult struct {
		holderID string
		claim    Claim
		err      error
	}

	start := make(chan struct{})
	results := make(chan claimResult, len(holders))

	var ready sync.WaitGroup
	ready.Add(len(holders))

	for _, holderID := range holders {
		go func(holderID string) {
			ready.Done()
			<-start

			claim, claimErr := registry.Claim(
				ctx,
				ClaimCommand{
					IncidentID:      observed.ID,
					ExpectedVersion: observed.Version,
					HolderID:        holderID,
					Now:             now,
					LeaseDuration:   leaseDuration,
				},
			)

			results <- claimResult{
				holderID: holderID,
				claim:    claim,
				err:      claimErr,
			}
		}(holderID)
	}

	ready.Wait()
	close(start)

	successes := 0
	versionConflicts := 0

	for range holders {
		result := <-results

		if errors.Is(result.err, ErrVersionConflict) {
			versionConflicts++
			continue
		}
		if result.err != nil {
			t.Fatalf(
				"Claim(%q) error = %v; want nil or ErrVersionConflict",
				result.holderID,
				result.err,
			)
		}

		successes++

		if result.claim.HolderID != result.holderID {
			t.Fatalf(
				"Claim HolderID = %q; want %q",
				result.claim.HolderID,
				result.holderID,
			)
		}
		if result.claim.Incident.ID != observed.ID {
			t.Fatalf(
				"Claim Incident ID = %q; want %q",
				result.claim.Incident.ID,
				observed.ID,
			)
		}
		if result.claim.Incident.State != StateDetected {
			t.Fatalf(
				"Claim Incident State = %q; want %q",
				result.claim.Incident.State,
				StateDetected,
			)
		}
		if result.claim.Incident.Version != 2 {
			t.Fatalf(
				"Claim Incident Version = %d; want 2",
				result.claim.Incident.Version,
			)
		}

		expectedExpiry := now.Add(leaseDuration)
		if !result.claim.ExpiresAt.Equal(expectedExpiry) {
			t.Fatalf(
				"Claim ExpiresAt = %s; want %s",
				result.claim.ExpiresAt,
				expectedExpiry,
			)
		}
	}

	if successes != 1 {
		t.Fatalf("successful Claims = %d; want 1", successes)
	}
	if versionConflicts != 1 {
		t.Fatalf(
			"Claim version conflicts = %d; want 1",
			versionConflicts,
		)
	}
}
func TestRegistryClaimRejectsDifferentHolderWhileLeaseIsActive(
	t *testing.T,
) {
	t.Parallel()

	registry := NewMemoryRegistry()
	ctx := context.Background()

	incident, created, err := registry.Observe(
		ctx,
		Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "PodCrashLooping",
			Target: Target{
				Kind:      "Pod",
				Namespace: "sre-agent-lab",
				Name:      "crash-app-active-lease",
				UID:       "pod-uid-active-lease",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

	first, err := registry.Claim(
		ctx,
		ClaimCommand{
			IncidentID:      incident.ID,
			ExpectedVersion: incident.Version,
			HolderID:        "agent-a",
			Now:             now,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("first Claim() error = %v; want nil", err)
	}

	_, err = registry.Claim(
		ctx,
		ClaimCommand{
			IncidentID:      incident.ID,
			ExpectedVersion: first.Incident.Version,
			HolderID:        "agent-b",
			Now:             now.Add(10 * time.Second),
			LeaseDuration:   30 * time.Second,
		},
	)
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf(
			"second Claim() error = %v; want ErrLeaseHeld",
			err,
		)
	}
}
func TestRegistryClaimAllowsDifferentHolderAfterLeaseExpires(
	t *testing.T,
) {
	t.Parallel()

	registry := NewMemoryRegistry()
	ctx := context.Background()

	incident, created, err := registry.Observe(
		ctx,
		Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "PodCrashLooping",
			Target: Target{
				Kind:      "Pod",
				Namespace: "sre-agent-lab",
				Name:      "crash-app-expired-lease",
				UID:       "pod-uid-expired-lease",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	now := time.Date(2026, 9, 28, 13, 0, 0, 0, time.UTC)

	first, err := registry.Claim(
		ctx,
		ClaimCommand{
			IncidentID:      incident.ID,
			ExpectedVersion: 1,
			HolderID:        "agent-a",
			Now:             now,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("first Claim() error = %v; want nil", err)
	}

	takeover, err := registry.Claim(
		ctx,
		ClaimCommand{
			IncidentID:      incident.ID,
			ExpectedVersion: first.Incident.Version,
			HolderID:        "agent-b",
			Now:             first.ExpiresAt,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("takeover Claim() error = %v; want nil", err)
	}
	if takeover.HolderID != "agent-b" {
		t.Fatalf(
			"takeover HolderID = %q; want %q",
			takeover.HolderID,
			"agent-b",
		)
	}
	if takeover.Incident.State != StateDetected {
		t.Fatalf(
			"takeover Incident State = %q; want %q",
			takeover.Incident.State,
			StateDetected,
		)
	}
	if takeover.Incident.Version != 3 {
		t.Fatalf(
			"takeover Incident Version = %d; want 3",
			takeover.Incident.Version,
		)
	}

	expectedExpiry := now.Add(60 * time.Second)
	if !takeover.ExpiresAt.Equal(expectedExpiry) {
		t.Fatalf(
			"takeover ExpiresAt = %s; want %s",
			takeover.ExpiresAt,
			expectedExpiry,
		)
	}
}
func TestRegistryClaimRejectsInvalidCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*ClaimCommand)
	}{
		{
			name: "missing incident ID",
			mutate: func(command *ClaimCommand) {
				command.IncidentID = " "
			},
		},
		{
			name: "zero expected version",
			mutate: func(command *ClaimCommand) {
				command.ExpectedVersion = 0
			},
		},
		{
			name: "missing holder ID",
			mutate: func(command *ClaimCommand) {
				command.HolderID = " "
			},
		},
		{
			name: "zero current time",
			mutate: func(command *ClaimCommand) {
				command.Now = time.Time{}
			},
		},
		{
			name: "zero lease duration",
			mutate: func(command *ClaimCommand) {
				command.LeaseDuration = 0
			},
		},
		{
			name: "negative lease duration",
			mutate: func(command *ClaimCommand) {
				command.LeaseDuration = -time.Second
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := NewMemoryRegistry()
			ctx := context.Background()

			incident, created, err := registry.Observe(
				ctx,
				Observation{
					Source:    "prometheus",
					Cluster:   "dev",
					AlertName: "PodCrashLooping",
					Target: Target{
						Kind:      "Pod",
						Namespace: "sre-agent-lab",
						Name:      "crash-app-invalid-claim",
						UID:       "pod-uid-invalid-claim",
					},
				},
			)
			if err != nil {
				t.Fatalf("Observe() error = %v; want nil", err)
			}
			if !created {
				t.Fatal("Observe() created = false; want true")
			}

			command := ClaimCommand{
				IncidentID:      incident.ID,
				ExpectedVersion: incident.Version,
				HolderID:        "agent-a",
				Now: time.Date(
					2026,
					9,
					28,
					14,
					0,
					0,
					0,
					time.UTC,
				),
				LeaseDuration: 30 * time.Second,
			}
			test.mutate(&command)

			_, err = registry.Claim(ctx, command)
			if !errors.Is(err, ErrInvalidClaim) {
				t.Fatalf(
					"Claim() error = %v; want ErrInvalidClaim",
					err,
				)
			}
		})
	}
}
func TestRegistryClaimHistoryRecordsSuccessfulClaimsInVersionOrder(
	t *testing.T,
) {
	t.Parallel()

	registry := NewMemoryRegistry()
	ctx := context.Background()

	observed, created, err := registry.Observe(
		ctx,
		Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "KubePodCrashLooping",
			Target: Target{
				Kind:      "Pod",
				Namespace: "sre-agent-lab",
				Name:      "crash-app-claim-history",
				UID:       "pod-uid-claim-history",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	acquiredAt := time.Date(
		2026,
		9,
		28,
		17,
		30,
		0,
		0,
		time.UTC,
	)

	firstClaim, err := registry.Claim(
		ctx,
		ClaimCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: observed.Version,
			HolderID:        "agent-a",
			Now:             acquiredAt,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("first Claim() error = %v; want nil", err)
	}

	_, err = registry.Claim(
		ctx,
		ClaimCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: firstClaim.Incident.Version,
			HolderID:        "agent-b",
			Now:             firstClaim.ExpiresAt,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("expired Lease takeover error = %v; want nil", err)
	}

	var auditReader ClaimAuditReader = registry

	history, err := auditReader.ClaimHistory(ctx, observed.ID)
	if err != nil {
		t.Fatalf("ClaimHistory() error = %v; want nil", err)
	}
	if len(history) != 2 {
		t.Fatalf(
			"ClaimHistory() length = %d; want 2",
			len(history),
		)
	}

	expected := []ClaimAuditEvent{
		{
			IncidentID:      observed.ID,
			IncidentVersion: 2,
			HolderID:        "agent-a",
			AcquiredAt:      acquiredAt,
			ExpiresAt:       acquiredAt.Add(30 * time.Second),
		},
		{
			IncidentID:      observed.ID,
			IncidentVersion: 3,
			HolderID:        "agent-b",
			AcquiredAt:      acquiredAt.Add(30 * time.Second),
			ExpiresAt:       acquiredAt.Add(60 * time.Second),
		},
	}

	for index := range expected {
		actualEvent := history[index]
		expectedEvent := expected[index]

		if actualEvent != expectedEvent {
			t.Fatalf(
				"ClaimHistory()[%d] = %#v; want %#v",
				index,
				actualEvent,
				expectedEvent,
			)
		}
	}
}
func TestRegistryTransitionRejectsStaleClaimVersionAfterLeaseTakeover(
	t *testing.T,
) {
	t.Parallel()

	registry := NewMemoryRegistry()
	ctx := context.Background()

	observed, created, err := registry.Observe(
		ctx,
		Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "KubePodCrashLooping",
			Target: Target{
				Kind:      "Pod",
				Namespace: "sre-agent-lab",
				Name:      "crash-app-fencing",
				UID:       "pod-uid-fencing",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	now := time.Date(
		2026,
		9,
		28,
		18,
		0,
		0,
		0,
		time.UTC,
	)

	staleClaim, err := registry.Claim(
		ctx,
		ClaimCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: observed.Version,
			HolderID:        "agent-a",
			Now:             now,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("agent-a Claim() error = %v; want nil", err)
	}

	currentClaim, err := registry.Claim(
		ctx,
		ClaimCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: staleClaim.Incident.Version,
			HolderID:        "agent-b",
			Now:             staleClaim.ExpiresAt,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("agent-b takeover Claim() error = %v; want nil", err)
	}

	_, err = registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: staleClaim.Incident.Version,
			To:              StateDiagnosed,
			Actor:           "agent-a",
			ReasonCode:      "STALE_LEASE_HOLDER",
		},
	)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf(
			"stale holder Transition() error = %v; want ErrVersionConflict",
			err,
		)
	}

	diagnosed, err := registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: currentClaim.Incident.Version,
			To:              StateDiagnosed,
			Actor:           "agent-b",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"current holder Transition() error = %v; want nil",
			err,
		)
	}
	if diagnosed.State != StateDiagnosed {
		t.Fatalf(
			"diagnosed State = %q; want %q",
			diagnosed.State,
			StateDiagnosed,
		)
	}
	if diagnosed.Version != 4 {
		t.Fatalf(
			"diagnosed Version = %d; want 4",
			diagnosed.Version,
		)
	}
}
func TestRegistryTransitionBindsApprovalAndKeepsIncidentActive(
	t *testing.T,
) {
	t.Parallel()

	registry := NewMemoryRegistry()
	ctx := context.Background()
	observation := waitingApprovalTestObservation(
		"pod-uid-waiting-approval",
	)

	diagnosed := observeDiagnosedIncidentForApprovalTest(
		t,
		registry,
		observation,
	)

	const (
		planHash  = "a2f50b614b76121f9547762fe28deba170246779442073099f18b25ca9cf87c5"
		targetUID = "pod-uid-waiting-approval"
	)

	waiting, err := registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              StateWaitingApproval,
			Actor:           "sre-agent-v2",
			ReasonCode:      "APPROVAL_REQUIRED",
			ApprovalBinding: ApprovalBinding{
				PlanHash:  planHash,
				TargetUID: targetUID,
			},
		},
	)
	if err != nil {
		t.Fatalf("Transition(WAITING_APPROVAL) error = %v", err)
	}

	if waiting.State != StateWaitingApproval {
		t.Fatalf(
			"waiting State = %q; want %q",
			waiting.State,
			StateWaitingApproval,
		)
	}
	if waiting.Version != 3 {
		t.Fatalf(
			"waiting Version = %d; want 3",
			waiting.Version,
		)
	}
	if waiting.ApprovalBinding.PlanHash != planHash {
		t.Fatalf(
			"waiting PlanHash = %q; want %q",
			waiting.ApprovalBinding.PlanHash,
			planHash,
		)
	}
	if waiting.ApprovalBinding.TargetUID != targetUID {
		t.Fatalf(
			"waiting TargetUID = %q; want %q",
			waiting.ApprovalBinding.TargetUID,
			targetUID,
		)
	}

	observedAgain, createdAgain, err := registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("second Observe() error = %v", err)
	}
	if createdAgain {
		t.Fatal(
			"second Observe() created = true; want false while approval is pending",
		)
	}
	if observedAgain.ID != waiting.ID {
		t.Fatalf(
			"second Observe() ID = %q; want %q",
			observedAgain.ID,
			waiting.ID,
		)
	}
	if observedAgain.State != StateWaitingApproval {
		t.Fatalf(
			"second Observe() State = %q; want %q",
			observedAgain.State,
			StateWaitingApproval,
		)
	}
	if observedAgain.ApprovalBinding != waiting.ApprovalBinding {
		t.Fatalf(
			"second Observe() ApprovalBinding = %#v; want %#v",
			observedAgain.ApprovalBinding,
			waiting.ApprovalBinding,
		)
	}
}

func TestRegistryTransitionRejectsIncompleteApprovalBinding(
	t *testing.T,
) {
	t.Parallel()

	testCases := []struct {
		name    string
		binding ApprovalBinding
	}{
		{
			name: "missing plan hash",
			binding: ApprovalBinding{
				TargetUID: "pod-uid-missing-plan",
			},
		},
		{
			name: "missing target UID",
			binding: ApprovalBinding{
				PlanHash: "a2f50b614b76121f9547762fe28deba170246779442073099f18b25ca9cf87c5",
			},
		},
		{
			name:    "empty binding",
			binding: ApprovalBinding{},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			registry := NewMemoryRegistry()
			observation := waitingApprovalTestObservation(
				"pod-uid-" + testCase.name,
			)
			diagnosed := observeDiagnosedIncidentForApprovalTest(
				t,
				registry,
				observation,
			)

			_, err := registry.Transition(
				context.Background(),
				TransitionCommand{
					IncidentID:      diagnosed.ID,
					ExpectedVersion: diagnosed.Version,
					To:              StateWaitingApproval,
					Actor:           "sre-agent-v2",
					ReasonCode:      "APPROVAL_REQUIRED",
					ApprovalBinding: testCase.binding,
				},
			)
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf(
					"Transition(WAITING_APPROVAL) error = %v; want ErrInvalidTransition",
					err,
				)
			}
		})
	}
}

func TestRegistryTransitionRejectsWaitingApprovalBeforeDiagnosis(
	t *testing.T,
) {
	t.Parallel()

	registry := NewMemoryRegistry()
	ctx := context.Background()
	observation := waitingApprovalTestObservation(
		"pod-uid-not-diagnosed",
	)

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	_, err = registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              StateWaitingApproval,
			Actor:           "sre-agent-v2",
			ReasonCode:      "APPROVAL_REQUIRED",
			ApprovalBinding: ApprovalBinding{
				PlanHash:  "a2f50b614b76121f9547762fe28deba170246779442073099f18b25ca9cf87c5",
				TargetUID: observation.Target.UID,
			},
		},
	)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf(
			"DETECTED -> WAITING_APPROVAL error = %v; want ErrInvalidTransition",
			err,
		)
	}
}

func TestRegistryTransitionRejectsApprovalBindingOutsideWaitingApproval(
	t *testing.T,
) {
	t.Parallel()

	registry := NewMemoryRegistry()
	observation := waitingApprovalTestObservation(
		"pod-uid-binding-on-resolution",
	)
	diagnosed := observeDiagnosedIncidentForApprovalTest(
		t,
		registry,
		observation,
	)

	_, err := registry.Transition(
		context.Background(),
		TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              StateResolved,
			Actor:           "sre-agent-v2",
			ReasonCode:      "INCIDENT_RESOLVED",
			ApprovalBinding: ApprovalBinding{
				PlanHash:  "a2f50b614b76121f9547762fe28deba170246779442073099f18b25ca9cf87c5",
				TargetUID: observation.Target.UID,
			},
		},
	)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf(
			"Transition(RESOLVED) with approval binding error = %v; want ErrInvalidTransition",
			err,
		)
	}
}

func waitingApprovalTestObservation(targetUID string) Observation {
	return Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "KubePodCrashLooping",
		Target: Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app-waiting-approval",
			UID:       targetUID,
		},
	}
}

func observeDiagnosedIncidentForApprovalTest(
	t *testing.T,
	registry *Registry,
	observation Observation,
) Incident {
	t.Helper()

	ctx := context.Background()

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              StateDiagnosed,
			Actor:           "sre-agent-v2",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf("Transition(DIAGNOSED) error = %v", err)
	}

	return diagnosed
}
func TestRegistryTransitionFromDiagnosedToVerifyingKeepsIncidentActive(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	registry := NewMemoryRegistry()
	observation := Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "KubePodCrashLooping",
		Target: Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app-verifying",
			UID:       "pod-uid-verifying",
		},
	}

	detected, created, err := registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              StateDiagnosed,
			Actor:           "sre-agent",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v; want nil",
			err,
		)
	}
	if diagnosed.State != StateDiagnosed {
		t.Fatalf(
			"diagnosed State = %q; want %q",
			diagnosed.State,
			StateDiagnosed,
		)
	}

	verifying, err := registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              StateVerifying,
			Actor:           "sre-agent",
			ReasonCode:      "VERIFICATION_STARTED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DIAGNOSED -> VERIFYING) error = %v; want nil",
			err,
		)
	}
	if verifying.ID != diagnosed.ID {
		t.Fatalf(
			"verifying ID = %q; want %q",
			verifying.ID,
			diagnosed.ID,
		)
	}
	if verifying.State != StateVerifying {
		t.Fatalf(
			"verifying State = %q; want %q",
			verifying.State,
			StateVerifying,
		)
	}
	if verifying.Version != diagnosed.Version+1 {
		t.Fatalf(
			"verifying Version = %d; want %d",
			verifying.Version,
			diagnosed.Version+1,
		)
	}
	if verifying.ApprovalBinding != (ApprovalBinding{}) {
		t.Fatalf(
			"verifying ApprovalBinding = %#v; want empty",
			verifying.ApprovalBinding,
		)
	}

	observedAgain, created, err := registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("second Observe() error = %v; want nil", err)
	}
	if created {
		t.Fatal(
			"second Observe() created = true; " +
				"want existing active VERIFYING incident",
		)
	}
	if observedAgain.ID != verifying.ID {
		t.Fatalf(
			"second Observe() ID = %q; want %q",
			observedAgain.ID,
			verifying.ID,
		)
	}
	if observedAgain.State != StateVerifying {
		t.Fatalf(
			"second Observe() State = %q; want %q",
			observedAgain.State,
			StateVerifying,
		)
	}
	if observedAgain.Version != verifying.Version {
		t.Fatalf(
			"second Observe() Version = %d; want %d",
			observedAgain.Version,
			verifying.Version,
		)
	}
}
func TestRegistryTransitionFromWaitingApprovalToVerifyingKeepsBindingAndIncidentActive(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	registry := NewMemoryRegistry()

	observation := Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "KubePodCrashLooping",
		Target: Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app-waiting-verification",
			UID:       "pod-uid-waiting-verification",
		},
	}

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              StateDiagnosed,
			Actor:           "sre-agent",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v; want nil",
			err,
		)
	}

	binding := ApprovalBinding{
		PlanHash:  "sha256:verification-approved-plan",
		TargetUID: observation.Target.UID,
	}

	waiting, err := registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              StateWaitingApproval,
			Actor:           "sre-agent",
			ReasonCode:      "APPROVAL_REQUIRED",
			ApprovalBinding: binding,
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DIAGNOSED -> WAITING_APPROVAL) error = %v; want nil",
			err,
		)
	}
	if waiting.State != StateWaitingApproval {
		t.Fatalf(
			"waiting State = %q; want %q",
			waiting.State,
			StateWaitingApproval,
		)
	}
	if waiting.ApprovalBinding != binding {
		t.Fatalf(
			"waiting ApprovalBinding = %#v; want %#v",
			waiting.ApprovalBinding,
			binding,
		)
	}

	verifying, err := registry.Transition(
		ctx,
		TransitionCommand{
			IncidentID:      waiting.ID,
			ExpectedVersion: waiting.Version,
			To:              StateVerifying,
			Actor:           "sre-agent",
			ReasonCode:      "APPROVAL_VALIDATED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(WAITING_APPROVAL -> VERIFYING) error = %v; want nil",
			err,
		)
	}
	if verifying.State != StateVerifying {
		t.Fatalf(
			"verifying State = %q; want %q",
			verifying.State,
			StateVerifying,
		)
	}
	if verifying.Version != waiting.Version+1 {
		t.Fatalf(
			"verifying Version = %d; want %d",
			verifying.Version,
			waiting.Version+1,
		)
	}
	if verifying.ApprovalBinding != binding {
		t.Fatalf(
			"verifying ApprovalBinding = %#v; want retained %#v",
			verifying.ApprovalBinding,
			binding,
		)
	}

	observedAgain, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("second Observe() error = %v; want nil", err)
	}
	if created {
		t.Fatal(
			"second Observe() created = true; " +
				"want existing active VERIFYING incident",
		)
	}
	if observedAgain.ID != verifying.ID {
		t.Fatalf(
			"second Observe() ID = %q; want %q",
			observedAgain.ID,
			verifying.ID,
		)
	}
	if observedAgain.State != StateVerifying {
		t.Fatalf(
			"second Observe() State = %q; want %q",
			observedAgain.State,
			StateVerifying,
		)
	}
	if observedAgain.Version != verifying.Version {
		t.Fatalf(
			"second Observe() Version = %d; want %d",
			observedAgain.Version,
			verifying.Version,
		)
	}
	if observedAgain.ApprovalBinding != binding {
		t.Fatalf(
			"second Observe() ApprovalBinding = %#v; want %#v",
			observedAgain.ApprovalBinding,
			binding,
		)
	}
}
