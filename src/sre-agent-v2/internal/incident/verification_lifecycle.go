package incident

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	remediationdomain "sre-agent/internal/remediation"
)

var (
	ErrInvalidFencedVerification = errors.New(
		"invalid fenced verification",
	)
	ErrVerificationFenceConflict = errors.New(
		"verification fence conflict",
	)
	ErrVerificationLifecycleUnavailable = errors.New(
		"verification lifecycle store is unavailable",
	)
	ErrVerificationIncidentNotFound = errors.New(
		"verification incident was not found",
	)
	ErrInvalidVerificationIncidentState = errors.New(
		"invalid incident state for verification",
	)
)

type VerificationLifecycleStore interface {
	remediationdomain.VerificationStore
	RecoveryEvidenceStore
}

type BeginFencedVerificationCommand struct {
	IncidentID      string
	ExpectedVersion uint64
	HolderID        string
	Now             time.Time
	ReasonCode      string
	Verification    remediationdomain.BeginVerificationCommand
}

func (command BeginFencedVerificationCommand) Validate() error {
	switch {
	case strings.TrimSpace(command.IncidentID) == "":
		return fmt.Errorf(
			"%w: incident ID is required",
			ErrInvalidFencedVerification,
		)
	case command.ExpectedVersion == 0:
		return fmt.Errorf(
			"%w: expected incident version is required",
			ErrInvalidFencedVerification,
		)
	case strings.TrimSpace(command.HolderID) == "":
		return fmt.Errorf(
			"%w: claim holder ID is required",
			ErrInvalidFencedVerification,
		)
	case command.Now.IsZero():
		return fmt.Errorf(
			"%w: current time is required",
			ErrInvalidFencedVerification,
		)
	case strings.TrimSpace(command.ReasonCode) == "":
		return fmt.Errorf(
			"%w: reason code is required",
			ErrInvalidFencedVerification,
		)
	}

	if err := command.Verification.Validate(); err != nil {
		return fmt.Errorf(
			"%w: verification command: %v",
			ErrInvalidFencedVerification,
			err,
		)
	}

	actionAttempt := command.Verification.ActionAttempt
	if actionAttempt.Key.IncidentID != command.IncidentID {
		return fmt.Errorf(
			"%w: action attempt incident %q does not match incident %q",
			ErrInvalidFencedVerification,
			actionAttempt.Key.IncidentID,
			command.IncidentID,
		)
	}

	return nil
}

type FencedVerificationResult struct {
	Incident     Incident
	Verification remediationdomain.Verification
	Created      bool
}

func NewRegistryWithVerificationLifecycleStore(
	store VerificationLifecycleStore,
) *Registry {
	registry := NewMemoryRegistry()
	registry.verificationLifecycleStore = store
	registry.recoveryEvidenceStore = store

	return registry
}

func (registry *Registry) BeginFencedVerification(
	ctx context.Context,
	command BeginFencedVerificationCommand,
) (FencedVerificationResult, error) {
	if registry == nil {
		return FencedVerificationResult{}, fmt.Errorf(
			"%w: incident registry is nil",
			ErrVerificationLifecycleUnavailable,
		)
	}

	if ctx == nil {
		return FencedVerificationResult{}, fmt.Errorf(
			"%w: context is required",
			ErrInvalidFencedVerification,
		)
	}

	if err := command.Validate(); err != nil {
		return FencedVerificationResult{}, err
	}

	if err := ctx.Err(); err != nil {
		return FencedVerificationResult{}, err
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return FencedVerificationResult{}, err
	}

	if registry.verificationLifecycleStore == nil {
		return FencedVerificationResult{}, ErrVerificationLifecycleUnavailable
	}

	current, exists := registry.incidentsByID[command.IncidentID]
	if !exists {
		return FencedVerificationResult{}, fmt.Errorf(
			"%w: incident %q",
			ErrVerificationIncidentNotFound,
			command.IncidentID,
		)
	}

	if current.Version != command.ExpectedVersion {
		return FencedVerificationResult{}, fmt.Errorf(
			"%w: incident %q current=%d expected=%d",
			ErrVersionConflict,
			command.IncidentID,
			current.Version,
			command.ExpectedVersion,
		)
	}

	claim, claimed := registry.claimsByIncidentID[command.IncidentID]
	if !claimed {
		return FencedVerificationResult{}, fmt.Errorf(
			"%w: incident %q has no active claim",
			ErrVerificationFenceConflict,
			command.IncidentID,
		)
	}

	if claim.HolderID != command.HolderID {
		return FencedVerificationResult{}, fmt.Errorf(
			"%w: incident %q is held by %q, not %q",
			ErrVerificationFenceConflict,
			command.IncidentID,
			claim.HolderID,
			command.HolderID,
		)
	}

	if claim.Incident.Version != command.ExpectedVersion {
		return FencedVerificationResult{}, fmt.Errorf(
			"%w: claim for incident %q has version %d, expected %d",
			ErrVerificationFenceConflict,
			command.IncidentID,
			claim.Incident.Version,
			command.ExpectedVersion,
		)
	}

	if !command.Now.Before(claim.ExpiresAt) {
		return FencedVerificationResult{}, fmt.Errorf(
			"%w: claim for incident %q expired at %s",
			ErrVerificationFenceConflict,
			command.IncidentID,
			claim.ExpiresAt,
		)
	}

	next := current

	switch current.State {
	case StateDiagnosed, StateWaitingApproval:
		next.State = StateVerifying
		next.Version++

	case StateVerifying:
		// The Incident is already fenced in VERIFYING. Reusing the unique
		// pending Verification must not advance the Incident version again.

	default:
		return FencedVerificationResult{}, fmt.Errorf(
			"%w: incident %q is in state %q",
			ErrInvalidVerificationIncidentState,
			command.IncidentID,
			current.State,
		)
	}

	verification, created, err :=
		registry.verificationLifecycleStore.Begin(
			ctx,
			command.Verification,
		)
	if err != nil {
		return FencedVerificationResult{}, fmt.Errorf(
			"begin fenced verification for incident %q: %w",
			command.IncidentID,
			err,
		)
	}

	if verification.Status != remediationdomain.VerificationStatusPending {
		return FencedVerificationResult{}, fmt.Errorf(
			"%w: verification %q has status %q",
			ErrInvalidFencedVerification,
			verification.ID,
			verification.Status,
		)
	}

	/*
		Do not publish the Incident mutation before Verification Begin succeeds.

		While this lock is held, Claim takeover cannot advance the Incident
		version. Once Begin succeeds, committing the in-memory Incident update
		cannot fail.
	*/
	registry.incidentsByID[next.ID] = next

	return FencedVerificationResult{
		Incident:     next,
		Verification: verification,
		Created:      created,
	}, nil
}
