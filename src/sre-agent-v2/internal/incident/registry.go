package incident

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type State string

const (
	StateDetected        State = "DETECTED"
	StateDiagnosed       State = "DIAGNOSED"
	StateWaitingApproval State = "WAITING_APPROVAL"
	StateVerifying       State = "VERIFYING"
	StateResolved        State = "RESOLVED"
)

var (
	ErrInvalidObservation = errors.New("invalid incident observation")
	ErrInvalidTransition  = errors.New("invalid incident transition")
	ErrVersionConflict    = errors.New("incident version conflict")
	ErrLeaseHeld          = errors.New("incident lease is held")
	ErrInvalidClaim       = errors.New("invalid incident claim")
	ErrInvalidResolution  = errors.New(
		"invalid incident resolution",
	)
	ErrRecoveryEvidenceUnavailable = errors.New(
		"recovery evidence store unavailable",
	)
	ErrIncidentNotFound = errors.New("incident not found")
)

type Target struct {
	Kind      string
	Namespace string
	Name      string
	UID       string
}

type Observation struct {
	Source    string
	Cluster   string
	AlertName string
	Target    Target
}

type ApprovalBinding struct {
	PlanHash  string
	TargetUID string
}

type Incident struct {
	ID                       string
	State                    State
	Version                  uint64
	ApprovalBinding          ApprovalBinding
	ResolutionVerificationID string
	idempotencyKey           string
}

type TransitionCommand struct {
	IncidentID      string
	ExpectedVersion uint64
	To              State
	Actor           string
	ReasonCode      string
	ApprovalBinding ApprovalBinding
}

type ClaimCommand struct {
	IncidentID      string
	ExpectedVersion uint64
	HolderID        string
	Now             time.Time
	LeaseDuration   time.Duration
}

type RecoveryEvidenceStore interface {
	RequireRecovered(
		context.Context,
		string,
		string,
	) error
}

type ResolveCommand struct {
	IncidentID      string
	ExpectedVersion uint64
	VerificationID  string
	Actor           string
	ReasonCode      string
}

func (command ResolveCommand) Validate() error {
	switch {
	case strings.TrimSpace(command.IncidentID) == "":
		return fmt.Errorf(
			"%w: incident ID is required",
			ErrInvalidResolution,
		)
	case command.ExpectedVersion == 0:
		return fmt.Errorf(
			"%w: expected version must be positive",
			ErrInvalidResolution,
		)
	case strings.TrimSpace(command.VerificationID) == "":
		return fmt.Errorf(
			"%w: verification ID is required",
			ErrInvalidResolution,
		)
	case strings.TrimSpace(command.Actor) == "":
		return fmt.Errorf(
			"%w: actor is required",
			ErrInvalidResolution,
		)
	case strings.TrimSpace(command.ReasonCode) == "":
		return fmt.Errorf(
			"%w: reason code is required",
			ErrInvalidResolution,
		)
	default:
		return nil
	}
}

func (command ClaimCommand) Validate() error {
	if strings.TrimSpace(command.IncidentID) == "" {
		return fmt.Errorf(
			"%w: incident ID is required",
			ErrInvalidClaim,
		)
	}

	if command.ExpectedVersion == 0 {
		return fmt.Errorf(
			"%w: expected version must be greater than zero",
			ErrInvalidClaim,
		)
	}

	if strings.TrimSpace(command.HolderID) == "" {
		return fmt.Errorf(
			"%w: holder ID is required",
			ErrInvalidClaim,
		)
	}

	if command.Now.IsZero() {
		return fmt.Errorf(
			"%w: current time is required",
			ErrInvalidClaim,
		)
	}

	if command.LeaseDuration <= 0 {
		return fmt.Errorf(
			"%w: lease duration must be greater than zero",
			ErrInvalidClaim,
		)
	}

	return nil
}

type Claim struct {
	Incident  Incident
	HolderID  string
	ExpiresAt time.Time
}
type ClaimAuditEvent struct {
	IncidentID      string
	IncidentVersion uint64
	HolderID        string
	AcquiredAt      time.Time
	ExpiresAt       time.Time
}

type ClaimAuditReader interface {
	ClaimHistory(
		ctx context.Context,
		incidentID string,
	) ([]ClaimAuditEvent, error)
}

func (observation Observation) IdempotencyKey() (string, error) {
	if strings.TrimSpace(observation.Target.UID) == "" {
		return "", fmt.Errorf(
			"%w: target UID is required",
			ErrInvalidObservation,
		)
	}

	return idempotencyKeyFor(observation), nil
}

type Registry struct {
	mu                       sync.Mutex
	recoveryEvidenceStore    RecoveryEvidenceStore
	activeByKey              map[string]string
	incidentsByID            map[string]Incident
	claimsByIncidentID       map[string]Claim
	claimHistoryByIncidentID map[string][]ClaimAuditEvent
	nextSequence             uint64
}

func NewRegistryWithRecoveryEvidenceStore(
	store RecoveryEvidenceStore,
) *Registry {
	registry := NewMemoryRegistry()
	registry.recoveryEvidenceStore = store

	return registry
}

func NewMemoryRegistry() *Registry {
	return &Registry{
		activeByKey:        make(map[string]string),
		incidentsByID:      make(map[string]Incident),
		claimsByIncidentID: make(map[string]Claim),
		claimHistoryByIncidentID: make(
			map[string][]ClaimAuditEvent,
		),
	}
}

func (registry *Registry) Observe(
	ctx context.Context,
	observation Observation,
) (Incident, bool, error) {
	if err := ctx.Err(); err != nil {
		return Incident{}, false, err
	}

	key, err := observation.IdempotencyKey()
	if err != nil {
		return Incident{}, false, err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()

	if incidentID, exists := registry.activeByKey[key]; exists {
		return registry.incidentsByID[incidentID], false, nil
	}

	registry.nextSequence++

	incident := Incident{
		ID:             incidentIDFor(key, registry.nextSequence),
		State:          StateDetected,
		Version:        1,
		idempotencyKey: key,
	}

	registry.activeByKey[key] = incident.ID
	registry.incidentsByID[incident.ID] = incident

	return incident, true, nil
}

func (registry *Registry) Claim(
	ctx context.Context,
	command ClaimCommand,
) (Claim, error) {
	if err := ctx.Err(); err != nil {
		return Claim{}, err
	}
	if err := command.Validate(); err != nil {
		return Claim{}, err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()

	incident, exists := registry.incidentsByID[command.IncidentID]
	if !exists {
		return Claim{}, fmt.Errorf(
			"incident %q was not found",
			command.IncidentID,
		)
	}

	if incident.Version != command.ExpectedVersion {
		return Claim{}, fmt.Errorf(
			"%w: incident %q current=%d expected=%d",
			ErrVersionConflict,
			incident.ID,
			incident.Version,
			command.ExpectedVersion,
		)
	}

	currentClaim, claimed := registry.claimsByIncidentID[incident.ID]
	if claimed &&
		currentClaim.HolderID != command.HolderID &&
		command.Now.Before(currentClaim.ExpiresAt) {
		return Claim{}, fmt.Errorf(
			"%w: incident %q is held by %q until %s",
			ErrLeaseHeld,
			incident.ID,
			currentClaim.HolderID,
			currentClaim.ExpiresAt.UTC().Format(time.RFC3339Nano),
		)
	}

	incident.Version++

	claim := Claim{
		Incident:  incident,
		HolderID:  command.HolderID,
		ExpiresAt: command.Now.Add(command.LeaseDuration),
	}

	registry.incidentsByID[incident.ID] = incident
	registry.claimsByIncidentID[incident.ID] = claim

	registry.claimHistoryByIncidentID[incident.ID] = append(
		registry.claimHistoryByIncidentID[incident.ID],
		ClaimAuditEvent{
			IncidentID:      incident.ID,
			IncidentVersion: incident.Version,
			HolderID:        claim.HolderID,
			AcquiredAt:      command.Now,
			ExpiresAt:       claim.ExpiresAt,
		},
	)

	return claim, nil
}

func (registry *Registry) ClaimHistory(
	ctx context.Context,
	incidentID string,
) ([]ClaimAuditEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	history := registry.claimHistoryByIncidentID[incidentID]

	result := make([]ClaimAuditEvent, len(history))
	copy(result, history)

	return result, nil
}

func (registry *Registry) Transition(
	ctx context.Context,
	command TransitionCommand,
) (Incident, error) {
	if err := ctx.Err(); err != nil {
		return Incident{}, err
	}
	if err := command.Validate(); err != nil {
		return Incident{}, err
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	current, exists := registry.incidentsByID[command.IncidentID]
	if !exists {
		return Incident{}, fmt.Errorf(
			"incident %q was not found",
			command.IncidentID,
		)
	}

	if current.Version != command.ExpectedVersion {
		return Incident{}, fmt.Errorf(
			"%w: incident %q current=%d expected=%d",
			ErrVersionConflict,
			current.ID,
			current.Version,
			command.ExpectedVersion,
		)
	}

	validTransition := (current.State == StateDetected &&
		(command.To == StateDiagnosed ||
			command.To == StateResolved)) ||
		(current.State == StateDiagnosed &&
			(command.To == StateWaitingApproval ||
				command.To == StateVerifying ||
				command.To == StateResolved)) ||
		(current.State == StateWaitingApproval &&
			command.To == StateVerifying)
	if !validTransition {
		return Incident{}, fmt.Errorf(
			"%w: from %q to %q",
			ErrInvalidTransition,
			current.State,
			command.To,
		)
	}

	current.State = command.To
	current.Version++

	if command.To == StateWaitingApproval {
		current.ApprovalBinding = command.ApprovalBinding
	}

	registry.incidentsByID[current.ID] = current

	if current.State == StateResolved {
		delete(
			registry.activeByKey,
			current.idempotencyKey,
		)
	}

	return current, nil
}

func (registry *Registry) Resolve(
	ctx context.Context,
	command ResolveCommand,
) (Incident, error) {
	if err := ctx.Err(); err != nil {
		return Incident{}, err
	}

	if err := command.Validate(); err != nil {
		return Incident{}, err
	}

	if registry.recoveryEvidenceStore == nil {
		return Incident{}, fmt.Errorf(
			"%w: resolving incident %q requires persisted recovery evidence",
			ErrRecoveryEvidenceUnavailable,
			command.IncidentID,
		)
	}

	if err := registry.recoveryEvidenceStore.RequireRecovered(
		ctx,
		command.VerificationID,
		command.IncidentID,
	); err != nil {
		return Incident{}, fmt.Errorf(
			"validate recovery evidence for incident %q: %w",
			command.IncidentID,
			err,
		)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return Incident{}, err
	}

	current, exists := registry.incidentsByID[command.IncidentID]
	if !exists {
		return Incident{}, fmt.Errorf(
			"%w: incident %q",
			ErrIncidentNotFound,
			command.IncidentID,
		)
	}

	if current.Version != command.ExpectedVersion {
		return Incident{}, fmt.Errorf(
			"%w: incident %q current=%d expected=%d",
			ErrVersionConflict,
			command.IncidentID,
			current.Version,
			command.ExpectedVersion,
		)
	}

	if current.State != StateVerifying {
		return Incident{}, fmt.Errorf(
			"%w: from %q to %q",
			ErrInvalidTransition,
			current.State,
			StateResolved,
		)
	}

	current.State = StateResolved
	current.Version++
	current.ResolutionVerificationID = command.VerificationID
	current.ApprovalBinding = ApprovalBinding{}

	registry.incidentsByID[current.ID] = current
	delete(registry.activeByKey, current.idempotencyKey)

	return current, nil
}

func (command TransitionCommand) Validate() error {
	if strings.TrimSpace(command.Actor) == "" {
		return fmt.Errorf(
			"%w: actor is required",
			ErrInvalidTransition,
		)
	}

	if strings.TrimSpace(command.ReasonCode) == "" {
		return fmt.Errorf(
			"%w: reason code is required",
			ErrInvalidTransition,
		)
	}

	if command.To == StateWaitingApproval {
		if strings.TrimSpace(command.ApprovalBinding.PlanHash) == "" {
			return fmt.Errorf(
				"%w: approval plan hash is required",
				ErrInvalidTransition,
			)
		}

		if strings.TrimSpace(command.ApprovalBinding.TargetUID) == "" {
			return fmt.Errorf(
				"%w: approval target UID is required",
				ErrInvalidTransition,
			)
		}

		return nil
	}

	if command.ApprovalBinding != (ApprovalBinding{}) {
		return fmt.Errorf(
			"%w: approval binding is only allowed for WAITING_APPROVAL",
			ErrInvalidTransition,
		)
	}

	return nil
}

func idempotencyKeyFor(observation Observation) string {
	input := fmt.Sprintf(
		"%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s",
		observation.Source,
		observation.Cluster,
		observation.AlertName,
		observation.Target.Kind,
		observation.Target.Namespace,
		observation.Target.Name,
		observation.Target.UID,
	)

	digest := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%x", digest)
}

func incidentIDFor(key string, sequence uint64) string {
	input := fmt.Sprintf("%s\x00%d", key, sequence)
	digest := sha256.Sum256([]byte(input))

	return fmt.Sprintf("inc-%x", digest[:12])
}
