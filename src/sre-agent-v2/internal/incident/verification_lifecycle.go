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

type CompleteFencedVerificationCommand struct {
	IncidentID      string
	ExpectedVersion uint64
	HolderID        string
	Now             time.Time
	Verification    remediationdomain.CompleteVerificationCommand
}

type verificationFence struct {
	IncidentID      string
	ExpectedVersion uint64
	HolderID        string
	Now             time.Time
}

func (command BeginFencedVerificationCommand) fence() verificationFence {
	return verificationFence{
		IncidentID:      command.IncidentID,
		ExpectedVersion: command.ExpectedVersion,
		HolderID:        command.HolderID,
		Now:             command.Now,
	}
}

func (command CompleteFencedVerificationCommand) fence() verificationFence {
	return verificationFence{
		IncidentID:      command.IncidentID,
		ExpectedVersion: command.ExpectedVersion,
		HolderID:        command.HolderID,
		Now:             command.Now,
	}
}

func (command CompleteFencedVerificationCommand) Validate() error {
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
	}

	if err := command.Verification.Validate(); err != nil {
		return fmt.Errorf(
			"%w: verification command: %v",
			ErrInvalidFencedVerification,
			err,
		)
	}

	if command.Verification.ActionKey.IncidentID !=
		command.IncidentID {
		return fmt.Errorf(
			"%w: verification incident %q does not match incident %q",
			ErrInvalidFencedVerification,
			command.Verification.ActionKey.IncidentID,
			command.IncidentID,
		)
	}

	return nil
}

type FencedVerificationCompletionResult struct {
	Incident     Incident
	Verification remediationdomain.Verification
}

func (registry *Registry) CompleteFencedVerification(
	ctx context.Context,
	command CompleteFencedVerificationCommand,
) (FencedVerificationCompletionResult, error) {
	if registry == nil {
		return FencedVerificationCompletionResult{}, fmt.Errorf(
			"%w: incident registry is nil",
			ErrVerificationLifecycleUnavailable,
		)
	}

	if ctx == nil {
		return FencedVerificationCompletionResult{}, fmt.Errorf(
			"%w: context is required",
			ErrInvalidFencedVerification,
		)
	}

	if err := command.Validate(); err != nil {
		return FencedVerificationCompletionResult{}, err
	}

	if err := ctx.Err(); err != nil {
		return FencedVerificationCompletionResult{}, err
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return FencedVerificationCompletionResult{}, err
	}

	if registry.verificationLifecycleStore == nil {
		return FencedVerificationCompletionResult{},
			ErrVerificationLifecycleUnavailable
	}

	current, err := registry.validateVerificationFenceLocked(
		command.fence(),
	)
	if err != nil {
		return FencedVerificationCompletionResult{}, err
	}
	if current.State != StateVerifying {
		return FencedVerificationCompletionResult{}, fmt.Errorf(
			"%w: incident %q is in state %q",
			ErrInvalidVerificationIncidentState,
			command.IncidentID,
			current.State,
		)
	}

	verification, err :=
		registry.verificationLifecycleStore.Complete(
			ctx,
			command.Verification,
		)
	if err != nil {
		return FencedVerificationCompletionResult{}, fmt.Errorf(
			"complete fenced Verification for incident %q: %w",
			command.IncidentID,
			err,
		)
	}

	return FencedVerificationCompletionResult{
		Incident:     current,
		Verification: verification,
	}, nil
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

func (registry *Registry) validateVerificationFenceLocked(
	fence verificationFence,
) (Incident, error) {
	current, exists := registry.incidentsByID[fence.IncidentID]
	if !exists {
		return Incident{}, fmt.Errorf(
			"%w: incident %q",
			ErrVerificationIncidentNotFound,
			fence.IncidentID,
		)
	}

	if current.Version != fence.ExpectedVersion {
		return Incident{}, fmt.Errorf(
			"%w: incident %q current=%d expected=%d",
			ErrVersionConflict,
			fence.IncidentID,
			current.Version,
			fence.ExpectedVersion,
		)
	}

	claim, claimed := registry.claimsByIncidentID[fence.IncidentID]
	if !claimed {
		return Incident{}, fmt.Errorf(
			"%w: incident %q has no active claim",
			ErrVerificationFenceConflict,
			fence.IncidentID,
		)
	}

	if claim.HolderID != fence.HolderID {
		return Incident{}, fmt.Errorf(
			"%w: incident %q is held by %q, not %q",
			ErrVerificationFenceConflict,
			fence.IncidentID,
			claim.HolderID,
			fence.HolderID,
		)
	}

	if claim.Incident.Version != fence.ExpectedVersion {
		return Incident{}, fmt.Errorf(
			"%w: claim for incident %q has version %d, expected %d",
			ErrVerificationFenceConflict,
			fence.IncidentID,
			claim.Incident.Version,
			fence.ExpectedVersion,
		)
	}

	currentClaimAudited := false

	for _, event := range registry.claimHistoryByIncidentID[fence.IncidentID] {
		if event.IncidentID == fence.IncidentID &&
			event.IncidentVersion == fence.ExpectedVersion &&
			event.HolderID == fence.HolderID &&
			event.ExpiresAt.Equal(claim.ExpiresAt) {
			currentClaimAudited = true
			break
		}
	}

	if !currentClaimAudited {
		return Incident{}, fmt.Errorf(
			"%w: incident %q version %d has no matching claim audit",
			ErrVerificationFenceConflict,
			fence.IncidentID,
			fence.ExpectedVersion,
		)
	}

	if !fence.Now.Before(claim.ExpiresAt) {
		return Incident{}, fmt.Errorf(
			"%w: claim for incident %q expired at %s",
			ErrVerificationFenceConflict,
			fence.IncidentID,
			claim.ExpiresAt,
		)
	}

	return current, nil
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

	current, err := registry.validateVerificationFenceLocked(
		command.fence(),
	)
	if err != nil {
		return FencedVerificationResult{}, err
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
