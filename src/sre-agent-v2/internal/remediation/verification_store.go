package remediation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidVerification           = errors.New("invalid verification")
	ErrVerificationNotFound          = errors.New("verification not found")
	ErrVerificationVersionConflict   = errors.New("verification version conflict")
	ErrInvalidVerificationTransition = errors.New(
		"invalid verification transition",
	)
)

type VerificationStatus string

const (
	VerificationStatusPending VerificationStatus = "PENDING"

	VerificationStatusRecovered VerificationStatus = "RECOVERED"

	VerificationStatusNotRecovered VerificationStatus = "NOT_RECOVERED"

	VerificationStatusInconclusive VerificationStatus = "INCONCLUSIVE"
)

type Verification struct {
	ID              string
	ActionAttemptID string
	ActionKey       ActionKey
	Status          VerificationStatus
	Version         int64
	StartedAt       time.Time
	FinishedAt      *time.Time
	EvidenceCode    string
}

type BeginVerificationCommand struct {
	ActionAttempt ActionAttempt
	StartedAt     time.Time
}

func (command BeginVerificationCommand) Validate() error {
	actionKey := command.ActionAttempt.Key.ActionKey()

	if invalidVerificationActionKey(actionKey) ||
		strings.TrimSpace(command.ActionAttempt.ID) == "" ||
		command.ActionAttempt.Key.FencingToken <= 0 ||
		command.ActionAttempt.Version <= 0 ||
		command.ActionAttempt.FinishedAt == nil ||
		command.StartedAt.IsZero() {
		return ErrInvalidVerification
	}

	switch command.ActionAttempt.Status {
	case ActionAttemptStatusSucceeded,
		ActionAttemptStatusFailed,
		ActionAttemptStatusUnknown:
	default:
		return ErrInvalidVerification
	}

	if command.StartedAt.Before(
		*command.ActionAttempt.FinishedAt,
	) {
		return ErrInvalidVerification
	}

	return nil
}

func NewVerification(
	command BeginVerificationCommand,
) (Verification, error) {
	if err := command.Validate(); err != nil {
		return Verification{}, err
	}

	actionKey := command.ActionAttempt.Key.ActionKey()

	return Verification{
		ID: verificationIDFor(
			command.ActionAttempt.ID,
		),
		ActionAttemptID: command.ActionAttempt.ID,
		ActionKey:       actionKey,
		Status:          VerificationStatusPending,
		Version:         1,
		StartedAt:       command.StartedAt,
	}, nil
}

type CompleteVerificationCommand struct {
	ActionKey       ActionKey
	ExpectedVersion int64
	To              VerificationStatus
	FinishedAt      time.Time
	EvidenceCode    string
}

func (command CompleteVerificationCommand) Validate() error {
	if invalidVerificationActionKey(command.ActionKey) ||
		command.ExpectedVersion <= 0 ||
		command.FinishedAt.IsZero() ||
		strings.TrimSpace(command.EvidenceCode) == "" {
		return ErrInvalidVerification
	}

	switch command.To {
	case VerificationStatusRecovered,
		VerificationStatusNotRecovered,
		VerificationStatusInconclusive:
		return nil
	default:
		return ErrInvalidVerification
	}
}

type VerificationStore interface {
	Begin(
		context.Context,
		BeginVerificationCommand,
	) (Verification, bool, error)
	Complete(
		context.Context,
		CompleteVerificationCommand,
	) (Verification, error)
}

type MemoryVerificationStore struct {
	mu            sync.Mutex
	verifications map[ActionKey]Verification
}

var _ VerificationStore = (*MemoryVerificationStore)(nil)

func NewMemoryVerificationStore() *MemoryVerificationStore {
	return &MemoryVerificationStore{
		verifications: make(map[ActionKey]Verification),
	}
}

func (store *MemoryVerificationStore) Begin(
	ctx context.Context,
	command BeginVerificationCommand,
) (Verification, bool, error) {
	if err := ctx.Err(); err != nil {
		return Verification{}, false, err
	}

	verification, err := NewVerification(command)
	if err != nil {
		return Verification{}, false, err
	}

	actionKey := verification.ActionKey

	store.mu.Lock()
	defer store.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return Verification{}, false, err
	}

	if store.verifications == nil {
		store.verifications = make(
			map[ActionKey]Verification,
		)
	}

	existing, exists := store.verifications[actionKey]
	if exists {
		return cloneVerification(existing), false, nil
	}

	store.verifications[actionKey] = verification

	return cloneVerification(verification), true, nil
}

func (store *MemoryVerificationStore) Complete(
	ctx context.Context,
	command CompleteVerificationCommand,
) (Verification, error) {
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}

	if err := command.Validate(); err != nil {
		return Verification{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}

	current, exists := store.verifications[command.ActionKey]
	if !exists {
		return Verification{}, fmt.Errorf(
			"%w: incident=%q plan=%q target_uid=%q",
			ErrVerificationNotFound,
			command.ActionKey.IncidentID,
			command.ActionKey.PlanHash,
			command.ActionKey.TargetUID,
		)
	}

	if verificationCompletionMatches(current, command) {
		return cloneVerification(current), nil
	}

	if current.Version != command.ExpectedVersion {
		return Verification{}, fmt.Errorf(
			"%w: current=%d expected=%d",
			ErrVerificationVersionConflict,
			current.Version,
			command.ExpectedVersion,
		)
	}

	if current.Status != VerificationStatusPending {
		return Verification{}, fmt.Errorf(
			"%w: current=%q requested=%q",
			ErrInvalidVerificationTransition,
			current.Status,
			command.To,
		)
	}

	if command.FinishedAt.Before(current.StartedAt) {
		return Verification{}, fmt.Errorf(
			"%w: finish time precedes start time",
			ErrInvalidVerification,
		)
	}

	finishedAt := command.FinishedAt

	current.Status = command.To
	current.Version++
	current.FinishedAt = &finishedAt
	current.EvidenceCode = command.EvidenceCode

	store.verifications[command.ActionKey] = current

	return cloneVerification(current), nil
}

func verificationCompletionMatches(
	current Verification,
	command CompleteVerificationCommand,
) bool {
	return current.Status == command.To &&
		current.Version == command.ExpectedVersion+1 &&
		current.FinishedAt != nil &&
		current.FinishedAt.Equal(command.FinishedAt) &&
		current.EvidenceCode == command.EvidenceCode
}

func invalidVerificationActionKey(key ActionKey) bool {
	return strings.TrimSpace(key.IncidentID) == "" ||
		strings.TrimSpace(key.PlanHash) == "" ||
		strings.TrimSpace(key.TargetUID) == ""
}

func cloneVerification(
	verification Verification,
) Verification {
	cloned := verification

	if verification.FinishedAt != nil {
		finishedAt := *verification.FinishedAt
		cloned.FinishedAt = &finishedAt
	}

	return cloned
}

func verificationIDFor(actionAttemptID string) string {
	digest := sha256.Sum256(
		[]byte(actionAttemptID),
	)

	return fmt.Sprintf(
		"ver-%x",
		digest[:12],
	)
}
