package incident

import (
	"context"
	"errors"
	"testing"
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
