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
	StateDetected  State = "DETECTED"
	StateDiagnosed State = "DIAGNOSED"
	StateResolved  State = "RESOLVED"
)

var (
	ErrInvalidObservation = errors.New("invalid incident observation")
	ErrInvalidTransition  = errors.New("invalid incident transition")
	ErrVersionConflict    = errors.New("incident version conflict")
	ErrLeaseHeld          = errors.New("incident lease is held")
	ErrInvalidClaim       = errors.New("invalid incident claim")
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

type Incident struct {
	ID      string
	State   State
	Version uint64

	idempotencyKey string
}

type TransitionCommand struct {
	IncidentID      string
	ExpectedVersion uint64
	To              State
	Actor           string
	ReasonCode      string
}

type ClaimCommand struct {
	IncidentID      string
	ExpectedVersion uint64
	HolderID        string
	Now             time.Time
	LeaseDuration   time.Duration
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
	mu sync.Mutex

	activeByKey        map[string]string
	incidentsByID      map[string]Incident
	claimsByIncidentID map[string]Claim
	nextSequence       uint64
}

func NewMemoryRegistry() *Registry {
	return &Registry{
		activeByKey:        make(map[string]string),
		incidentsByID:      make(map[string]Incident),
		claimsByIncidentID: make(map[string]Claim),
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

	return claim, nil
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

	incident, exists := registry.incidentsByID[command.IncidentID]
	if !exists {
		return Incident{}, fmt.Errorf(
			"incident %q was not found",
			command.IncidentID,
		)
	}

	if incident.Version != command.ExpectedVersion {
		return Incident{}, fmt.Errorf(
			"%w: incident %q current=%d expected=%d",
			ErrVersionConflict,
			incident.ID,
			incident.Version,
			command.ExpectedVersion,
		)
	}
	validTransition := (incident.State == StateDetected &&
		(command.To == StateDiagnosed ||
			command.To == StateResolved)) ||
		(incident.State == StateDiagnosed &&
			command.To == StateResolved)
	if !validTransition {
		return Incident{}, fmt.Errorf(
			"%w: from %q to %q",
			ErrInvalidTransition,
			incident.State,
			command.To,
		)
	}
	incident.State = command.To
	incident.Version++

	registry.incidentsByID[incident.ID] = incident
	if incident.State == StateResolved {
		delete(
			registry.activeByKey,
			incident.idempotencyKey,
		)
	}

	return incident, nil
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
