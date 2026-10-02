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
	ErrInvalidExecutionKey  = errors.New("invalid execution key")
	ErrInvalidActionAttempt = errors.New(
		"invalid action attempt",
	)
)

type ExecutionKey struct {
	IncidentID   string
	PlanHash     string
	TargetUID    string
	FencingToken uint64
}

func (key ExecutionKey) Validate() error {
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

	if key.FencingToken == 0 {
		return fmt.Errorf(
			"%w: fencing token must be greater than zero",
			ErrInvalidExecutionKey,
		)
	}

	return nil
}

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

type ActionAttempt struct {
	ID        string
	Key       ExecutionKey
	StartedAt time.Time
}

func NewActionAttempt(
	command BeginActionAttemptCommand,
) (ActionAttempt, error) {
	if err := command.Validate(); err != nil {
		return ActionAttempt{}, err
	}

	return ActionAttempt{
		ID:        actionAttemptIDFor(command.Key),
		Key:       command.Key,
		StartedAt: command.StartedAt,
	}, nil
}

type ActionAttemptStore interface {
	Begin(
		context.Context,
		BeginActionAttemptCommand,
	) (ActionAttempt, bool, error)
}

type MemoryActionAttemptStore struct {
	mu       sync.Mutex
	attempts map[ExecutionKey]ActionAttempt
}

var _ ActionAttemptStore = (*MemoryActionAttemptStore)(nil)

func NewMemoryActionAttemptStore() *MemoryActionAttemptStore {
	return &MemoryActionAttemptStore{
		attempts: make(map[ExecutionKey]ActionAttempt),
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

	store.mu.Lock()
	defer store.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return ActionAttempt{}, false, err
	}

	if store.attempts == nil {
		store.attempts = make(
			map[ExecutionKey]ActionAttempt,
		)
	}

	existing, exists := store.attempts[command.Key]
	if exists {
		return existing, false, nil
	}

	store.attempts[command.Key] = attempt

	return attempt, true, nil
}

func actionAttemptIDFor(key ExecutionKey) string {
	input := fmt.Sprintf(
		"%s\x00%s\x00%s\x00%d",
		key.IncidentID,
		key.PlanHash,
		key.TargetUID,
		key.FencingToken,
	)
	digest := sha256.Sum256([]byte(input))

	return fmt.Sprintf("att-%x", digest[:12])
}
