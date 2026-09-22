package incident

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
)

type State string

const (
	StateDetected State = "DETECTED"
	StateResolved State = "RESOLVED"
)

var (
	ErrInvalidObservation = errors.New("invalid incident observation")
	ErrVersionConflict    = errors.New("incident version conflict")
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
}

type Registry struct {
	mu sync.Mutex

	activeByKey   map[string]string
	incidentsByID map[string]Incident
	nextSequence  uint64
}

func NewMemoryRegistry() *Registry {
	return &Registry{
		activeByKey:   make(map[string]string),
		incidentsByID: make(map[string]Incident),
	}
}

func (registry *Registry) Observe(
	ctx context.Context,
	observation Observation,
) (Incident, bool, error) {
	if err := ctx.Err(); err != nil {
		return Incident{}, false, err
	}

	if strings.TrimSpace(observation.Target.UID) == "" {
		return Incident{}, false, fmt.Errorf(
			"%w: target UID is required",
			ErrInvalidObservation,
		)
	}
	key := idempotencyKeyFor(observation)

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

func (registry *Registry) Transition(
	ctx context.Context,
	command TransitionCommand,
) (Incident, error) {
	if err := ctx.Err(); err != nil {
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

	if incident.State != StateDetected || command.To != StateResolved {
		return Incident{}, fmt.Errorf(
			"invalid incident transition from %q to %q",
			incident.State,
			command.To,
		)
	}

	incident.State = StateResolved
	incident.Version++

	registry.incidentsByID[incident.ID] = incident
	delete(registry.activeByKey, incident.idempotencyKey)

	return incident, nil
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
