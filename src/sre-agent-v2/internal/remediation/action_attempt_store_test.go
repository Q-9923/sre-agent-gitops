package remediation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestActionAttemptStoreBeginReturnsExistingAttemptForSameExecutionKey(
	t *testing.T,
) {
	store := NewMemoryActionAttemptStore()
	command := validBeginActionAttemptCommand()

	first, created, err := store.Begin(
		context.Background(),
		command,
	)
	if err != nil {
		t.Fatalf("first Begin() error = %v", err)
	}
	if !created {
		t.Fatal("first Begin() created = false; want true")
	}

	retry := command
	retry.StartedAt = command.StartedAt.Add(time.Hour)

	second, created, err := store.Begin(
		context.Background(),
		retry,
	)
	if err != nil {
		t.Fatalf("second Begin() error = %v", err)
	}
	if created {
		t.Fatal("second Begin() created = true; want false")
	}

	if second.ID != first.ID {
		t.Fatalf(
			"second attempt ID = %q; want %q",
			second.ID,
			first.ID,
		)
	}
	if second.Key != first.Key {
		t.Fatalf(
			"second execution key = %#v; want %#v",
			second.Key,
			first.Key,
		)
	}
	if !second.StartedAt.Equal(first.StartedAt) {
		t.Fatalf(
			"second StartedAt = %s; want original %s",
			second.StartedAt,
			first.StartedAt,
		)
	}
}

func TestActionAttemptStoreConcurrentBeginCreatesOneAttempt(
	t *testing.T,
) {
	store := NewMemoryActionAttemptStore()
	command := validBeginActionAttemptCommand()

	const workers = 32

	type result struct {
		attempt ActionAttempt
		created bool
		err     error
	}

	results := make(chan result, workers)
	start := make(chan struct{})

	var waitGroup sync.WaitGroup
	waitGroup.Add(workers)

	for range workers {
		go func() {
			defer waitGroup.Done()

			<-start

			attempt, created, err := store.Begin(
				context.Background(),
				command,
			)
			results <- result{
				attempt: attempt,
				created: created,
				err:     err,
			}
		}()
	}

	close(start)
	waitGroup.Wait()
	close(results)

	createdCount := 0
	attemptID := ""

	for result := range results {
		if result.err != nil {
			t.Fatalf("Begin() error = %v", result.err)
		}

		if result.created {
			createdCount++
		}

		if attemptID == "" {
			attemptID = result.attempt.ID
		}
		if result.attempt.ID != attemptID {
			t.Fatalf(
				"attempt ID = %q; want %q",
				result.attempt.ID,
				attemptID,
			)
		}
	}

	if createdCount != 1 {
		t.Fatalf(
			"created count = %d; want 1",
			createdCount,
		)
	}
}

func TestActionAttemptStoreRejectsInvalidBeginCommand(
	t *testing.T,
) {
	testCases := []struct {
		name     string
		mutate   func(*BeginActionAttemptCommand)
		expected error
	}{
		{
			name: "missing incident ID",
			mutate: func(command *BeginActionAttemptCommand) {
				command.Key.IncidentID = ""
			},
			expected: ErrInvalidExecutionKey,
		},
		{
			name: "missing plan hash",
			mutate: func(command *BeginActionAttemptCommand) {
				command.Key.PlanHash = ""
			},
			expected: ErrInvalidExecutionKey,
		},
		{
			name: "missing target UID",
			mutate: func(command *BeginActionAttemptCommand) {
				command.Key.TargetUID = ""
			},
			expected: ErrInvalidExecutionKey,
		},
		{
			name: "zero fencing token",
			mutate: func(command *BeginActionAttemptCommand) {
				command.Key.FencingToken = 0
			},
			expected: ErrInvalidExecutionKey,
		},
		{
			name: "zero start time",
			mutate: func(command *BeginActionAttemptCommand) {
				command.StartedAt = time.Time{}
			},
			expected: ErrInvalidActionAttempt,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			command := validBeginActionAttemptCommand()
			testCase.mutate(&command)

			_, _, err := NewMemoryActionAttemptStore().
				Begin(
					context.Background(),
					command,
				)
			if !errors.Is(err, testCase.expected) {
				t.Fatalf(
					"Begin() error = %v; want %v",
					err,
					testCase.expected,
				)
			}
		})
	}
}

func TestActionAttemptStoreHonorsCanceledContext(
	t *testing.T,
) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := NewMemoryActionAttemptStore().Begin(
		ctx,
		validBeginActionAttemptCommand(),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf(
			"Begin() error = %v; want context.Canceled",
			err,
		)
	}
}

func validBeginActionAttemptCommand() BeginActionAttemptCommand {
	return BeginActionAttemptCommand{
		Key: ExecutionKey{
			IncidentID:   "inc-0123456789abcdef01234567",
			PlanHash:     "plan-hash-action-attempt",
			TargetUID:    "pod-uid-action-attempt",
			FencingToken: 7,
		},
		StartedAt: time.Date(
			2026,
			10,
			2,
			12,
			0,
			0,
			0,
			time.UTC,
		),
	}
}
