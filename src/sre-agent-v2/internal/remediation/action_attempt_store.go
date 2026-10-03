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
	ErrInvalidExecutionKey = errors.New(
		"invalid execution key",
	)
	ErrInvalidActionAttempt = errors.New(
		"invalid action attempt",
	)
	ErrActionAttemptNotFound = errors.New(
		"action attempt not found",
	)
	ErrActionAttemptVersionConflict = errors.New(
		"action attempt version conflict",
	)
	ErrActionAttemptFencingConflict = errors.New(
		"action attempt fencing conflict",
	)
	ErrInvalidActionAttemptTransition = errors.New(
		"invalid action attempt transition",
	)
)

type ActionKey struct {
	IncidentID string
	PlanHash   string
	TargetUID  string
}

func (key ActionKey) Validate() error {
	if strings.TrimSpace(key.IncidentID) == "" {
		return fmt.Errorf(
			"%w: incident ID is required",
			ErrInvalidExecutionKey,
		)
	}

	if strings.TrimSpace(key.PlanHash) == "" {
		return fmt.Errorf(
			"%w: plan hash is required",
			ErrInvalidExecutionKey,
		)
	}

	if strings.TrimSpace(key.TargetUID) == "" {
		return fmt.Errorf(
			"%w: target UID is required",
			ErrInvalidExecutionKey,
		)
	}

	return nil
}

type ExecutionKey struct {
	IncidentID   string
	PlanHash     string
	TargetUID    string
	FencingToken uint64
}

func (key ExecutionKey) ActionKey() ActionKey {
	return ActionKey{
		IncidentID: key.IncidentID,
		PlanHash:   key.PlanHash,
		TargetUID:  key.TargetUID,
	}
}

func (key ExecutionKey) Validate() error {
	if err := key.ActionKey().Validate(); err != nil {
		return err
	}

	if key.FencingToken == 0 {
		return fmt.Errorf(
			"%w: fencing token must be greater than zero",
			ErrInvalidExecutionKey,
		)
	}

	return nil
}

type ActionAttemptStatus string

const (
	ActionAttemptStatusStarted   ActionAttemptStatus = "STARTED"
	ActionAttemptStatusSucceeded ActionAttemptStatus = "SUCCEEDED"
	ActionAttemptStatusFailed    ActionAttemptStatus = "FAILED"
	ActionAttemptStatusUnknown   ActionAttemptStatus = "UNKNOWN"
)

type BeginActionAttemptCommand struct {
	Key       ExecutionKey
	StartedAt time.Time
}

func (command BeginActionAttemptCommand) Validate() error {
	if err := command.Key.Validate(); err != nil {
		return err
	}

	if command.StartedAt.IsZero() {
		return fmt.Errorf(
			"%w: start time is required",
			ErrInvalidActionAttempt,
		)
	}

	return nil
}

type CompleteActionAttemptCommand struct {
	Key             ExecutionKey
	ExpectedVersion uint64
	To              ActionAttemptStatus
	FinishedAt      time.Time
	ErrorCode       string
}

func (command CompleteActionAttemptCommand) Validate() error {
	if err := command.Key.Validate(); err != nil {
		return err
	}

	if command.ExpectedVersion == 0 {
		return fmt.Errorf(
			"%w: expected version must be greater than zero",
			ErrInvalidActionAttempt,
		)
	}

	switch command.To {
	case ActionAttemptStatusSucceeded:
		if strings.TrimSpace(command.ErrorCode) != "" {
			return fmt.Errorf(
				"%w: successful attempt cannot have an error code",
				ErrInvalidActionAttempt,
			)
		}
	case ActionAttemptStatusFailed:
		if strings.TrimSpace(command.ErrorCode) == "" {
			return fmt.Errorf(
				"%w: failed attempt requires an error code",
				ErrInvalidActionAttempt,
			)
		}
	default:
		return fmt.Errorf(
			"%w: unsupported terminal status %q",
			ErrInvalidActionAttemptTransition,
			command.To,
		)
	}

	if command.FinishedAt.IsZero() {
		return fmt.Errorf(
			"%w: finish time is required",
			ErrInvalidActionAttempt,
		)
	}

	return nil
}

type RecoverActionAttemptCommand struct {
	Key         ExecutionKey
	RecoveredAt time.Time
	ReasonCode  string
}

func (command RecoverActionAttemptCommand) Validate() error {
	if err := command.Key.Validate(); err != nil {
		return err
	}

	if command.RecoveredAt.IsZero() {
		return fmt.Errorf(
			"%w: recovery time is required",
			ErrInvalidActionAttempt,
		)
	}

	if strings.TrimSpace(command.ReasonCode) == "" {
		return fmt.Errorf(
			"%w: recovery reason code is required",
			ErrInvalidActionAttempt,
		)
	}

	return nil
}

type ActionAttempt struct {
	ID                      string
	Key                     ExecutionKey
	Status                  ActionAttemptStatus
	Version                 uint64
	StartedAt               time.Time
	FinishedAt              *time.Time
	ErrorCode               string
	RecoveredByFencingToken uint64
}

func NewActionAttempt(
	command BeginActionAttemptCommand,
) (ActionAttempt, error) {
	if err := command.Validate(); err != nil {
		return ActionAttempt{}, err
	}

	return ActionAttempt{
		ID:        actionAttemptIDFor(command.Key.ActionKey()),
		Key:       command.Key,
		Status:    ActionAttemptStatusStarted,
		Version:   1,
		StartedAt: command.StartedAt,
	}, nil
}

type ActionAttemptStore interface {
	Begin(
		context.Context,
		BeginActionAttemptCommand,
	) (ActionAttempt, bool, error)
	Complete(
		context.Context,
		CompleteActionAttemptCommand,
	) (ActionAttempt, error)
	Recover(
		context.Context,
		RecoverActionAttemptCommand,
	) (ActionAttempt, bool, error)
}

type MemoryActionAttemptStore struct {
	mu       sync.Mutex
	attempts map[ActionKey]ActionAttempt
}

var _ ActionAttemptStore = (*MemoryActionAttemptStore)(nil)

func NewMemoryActionAttemptStore() *MemoryActionAttemptStore {
	return &MemoryActionAttemptStore{
		attempts: make(map[ActionKey]ActionAttempt),
	}
}

func (store *MemoryActionAttemptStore) Begin(
	ctx context.Context,
	command BeginActionAttemptCommand,
) (ActionAttempt, bool, error) {
	if err := ctx.Err(); err != nil {
		return ActionAttempt{}, false, err
	}

	attempt, err := NewActionAttempt(command)
	if err != nil {
		return ActionAttempt{}, false, err
	}

	actionKey := command.Key.ActionKey()

	store.mu.Lock()
	defer store.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return ActionAttempt{}, false, err
	}

	if store.attempts == nil {
		store.attempts = make(
			map[ActionKey]ActionAttempt,
		)
	}

	existing, exists := store.attempts[actionKey]
	if exists {
		return cloneActionAttempt(existing), false, nil
	}

	store.attempts[actionKey] = attempt

	return cloneActionAttempt(attempt), true, nil
}

func (store *MemoryActionAttemptStore) Complete(
	ctx context.Context,
	command CompleteActionAttemptCommand,
) (ActionAttempt, error) {
	if err := ctx.Err(); err != nil {
		return ActionAttempt{}, err
	}

	if err := command.Validate(); err != nil {
		return ActionAttempt{}, err
	}

	actionKey := command.Key.ActionKey()

	store.mu.Lock()
	defer store.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return ActionAttempt{}, err
	}

	current, exists := store.attempts[actionKey]
	if !exists {
		return ActionAttempt{}, fmt.Errorf(
			"%w: incident=%q plan=%q target_uid=%q",
			ErrActionAttemptNotFound,
			actionKey.IncidentID,
			actionKey.PlanHash,
			actionKey.TargetUID,
		)
	}

	if current.Key.FencingToken !=
		command.Key.FencingToken {
		return ActionAttempt{}, fmt.Errorf(
			"%w: current=%d expected=%d",
			ErrActionAttemptFencingConflict,
			current.Key.FencingToken,
			command.Key.FencingToken,
		)
	}

	if completionMatches(current, command) {
		return cloneActionAttempt(current), nil
	}

	if current.Version != command.ExpectedVersion {
		return ActionAttempt{}, fmt.Errorf(
			"%w: current=%d expected=%d",
			ErrActionAttemptVersionConflict,
			current.Version,
			command.ExpectedVersion,
		)
	}

	if current.Status != ActionAttemptStatusStarted {
		return ActionAttempt{}, fmt.Errorf(
			"%w: current=%q requested=%q",
			ErrInvalidActionAttemptTransition,
			current.Status,
			command.To,
		)
	}

	if command.FinishedAt.Before(current.StartedAt) {
		return ActionAttempt{}, fmt.Errorf(
			"%w: finish time precedes start time",
			ErrInvalidActionAttempt,
		)
	}

	finishedAt := command.FinishedAt

	current.Status = command.To
	current.Version++
	current.FinishedAt = &finishedAt
	current.ErrorCode = command.ErrorCode
	current.RecoveredByFencingToken = 0

	store.attempts[actionKey] = current

	return cloneActionAttempt(current), nil
}

func (store *MemoryActionAttemptStore) Recover(
	ctx context.Context,
	command RecoverActionAttemptCommand,
) (ActionAttempt, bool, error) {
	if err := ctx.Err(); err != nil {
		return ActionAttempt{}, false, err
	}

	if err := command.Validate(); err != nil {
		return ActionAttempt{}, false, err
	}

	actionKey := command.Key.ActionKey()

	store.mu.Lock()
	defer store.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return ActionAttempt{}, false, err
	}

	current, exists := store.attempts[actionKey]
	if !exists {
		return ActionAttempt{}, false, fmt.Errorf(
			"%w: incident=%q plan=%q target_uid=%q",
			ErrActionAttemptNotFound,
			actionKey.IncidentID,
			actionKey.PlanHash,
			actionKey.TargetUID,
		)
	}

	if recoveryMatches(current, command) {
		return cloneActionAttempt(current), false, nil
	}

	if current.Status != ActionAttemptStatusStarted {
		return ActionAttempt{}, false, fmt.Errorf(
			"%w: current=%q requested=%q",
			ErrInvalidActionAttemptTransition,
			current.Status,
			ActionAttemptStatusUnknown,
		)
	}

	if command.Key.FencingToken <=
		current.Key.FencingToken {
		return ActionAttempt{}, false, fmt.Errorf(
			"%w: recovery token=%d must exceed owner token=%d",
			ErrActionAttemptFencingConflict,
			command.Key.FencingToken,
			current.Key.FencingToken,
		)
	}

	if command.RecoveredAt.Before(current.StartedAt) {
		return ActionAttempt{}, false, fmt.Errorf(
			"%w: recovery time precedes start time",
			ErrInvalidActionAttempt,
		)
	}

	recoveredAt := command.RecoveredAt

	current.Status = ActionAttemptStatusUnknown
	current.Version++
	current.FinishedAt = &recoveredAt
	current.ErrorCode = command.ReasonCode
	current.RecoveredByFencingToken =
		command.Key.FencingToken

	store.attempts[actionKey] = current

	return cloneActionAttempt(current), true, nil
}

func completionMatches(
	current ActionAttempt,
	command CompleteActionAttemptCommand,
) bool {
	return current.Status == command.To &&
		current.Key.FencingToken ==
			command.Key.FencingToken &&
		current.FinishedAt != nil &&
		current.FinishedAt.Equal(command.FinishedAt) &&
		current.ErrorCode == command.ErrorCode &&
		current.RecoveredByFencingToken == 0
}

func recoveryMatches(
	current ActionAttempt,
	command RecoverActionAttemptCommand,
) bool {
	return current.Status == ActionAttemptStatusUnknown &&
		current.FinishedAt != nil &&
		current.FinishedAt.Equal(command.RecoveredAt) &&
		current.ErrorCode == command.ReasonCode &&
		current.RecoveredByFencingToken ==
			command.Key.FencingToken
}

func cloneActionAttempt(
	attempt ActionAttempt,
) ActionAttempt {
	cloned := attempt

	if attempt.FinishedAt != nil {
		finishedAt := *attempt.FinishedAt
		cloned.FinishedAt = &finishedAt
	}

	return cloned
}

func actionAttemptIDFor(key ActionKey) string {
	input := fmt.Sprintf(
		"%s\x00%s\x00%s",
		key.IncidentID,
		key.PlanHash,
		key.TargetUID,
	)
	digest := sha256.Sum256([]byte(input))

	return fmt.Sprintf("att-%x", digest[:12])
}
