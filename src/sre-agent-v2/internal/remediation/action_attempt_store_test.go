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

func TestActionAttemptStoreBeginReturnsExistingAttemptAcrossFencingTokens(
	t *testing.T,
) {
	store := NewMemoryActionAttemptStore()
	firstCommand := validBeginActionAttemptCommand()

	first, created, err := store.Begin(
		context.Background(),
		firstCommand,
	)
	if err != nil {
		t.Fatalf("first Begin() error = %v", err)
	}
	if !created {
		t.Fatal("first Begin() created = false; want true")
	}

	takeoverCommand := firstCommand
	takeoverCommand.Key.FencingToken++
	takeoverCommand.StartedAt = firstCommand.StartedAt.Add(time.Minute)

	recovered, created, err := store.Begin(
		context.Background(),
		takeoverCommand,
	)
	if err != nil {
		t.Fatalf("takeover Begin() error = %v", err)
	}
	if created {
		t.Fatal(
			"takeover Begin() created = true; " +
				"want existing attempt across fencing tokens",
		)
	}

	if recovered.ID != first.ID {
		t.Fatalf(
			"recovered attempt ID = %q; want %q",
			recovered.ID,
			first.ID,
		)
	}
	if recovered.Key.FencingToken !=
		firstCommand.Key.FencingToken {
		t.Fatalf(
			"recovered FencingToken = %d; want original %d",
			recovered.Key.FencingToken,
			firstCommand.Key.FencingToken,
		)
	}
	if !recovered.StartedAt.Equal(first.StartedAt) {
		t.Fatalf(
			"recovered StartedAt = %s; want original %s",
			recovered.StartedAt,
			first.StartedAt,
		)
	}
}

func TestActionAttemptStoreCompletesStartedAttemptSuccessfully(
	t *testing.T,
) {
	store := NewMemoryActionAttemptStore()
	beginCommand := validBeginActionAttemptCommand()

	started, created, err := store.Begin(
		context.Background(),
		beginCommand,
	)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if !created {
		t.Fatal("Begin() created = false; want true")
	}
	if started.Status != ActionAttemptStatusStarted {
		t.Fatalf(
			"started Status = %q; want %q",
			started.Status,
			ActionAttemptStatusStarted,
		)
	}
	if started.Version != 1 {
		t.Fatalf(
			"started Version = %d; want 1",
			started.Version,
		)
	}
	if started.FinishedAt != nil {
		t.Fatalf(
			"started FinishedAt = %s; want nil",
			started.FinishedAt,
		)
	}

	finishedAt := beginCommand.StartedAt.Add(5 * time.Second)

	completed, err := store.Complete(
		context.Background(),
		CompleteActionAttemptCommand{
			Key:             beginCommand.Key,
			ExpectedVersion: started.Version,
			To:              ActionAttemptStatusSucceeded,
			FinishedAt:      finishedAt,
		},
	)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	if completed.ID != started.ID {
		t.Fatalf(
			"completed ID = %q; want %q",
			completed.ID,
			started.ID,
		)
	}
	if completed.Status != ActionAttemptStatusSucceeded {
		t.Fatalf(
			"completed Status = %q; want %q",
			completed.Status,
			ActionAttemptStatusSucceeded,
		)
	}
	if completed.Version != started.Version+1 {
		t.Fatalf(
			"completed Version = %d; want %d",
			completed.Version,
			started.Version+1,
		)
	}
	if completed.FinishedAt == nil ||
		!completed.FinishedAt.Equal(finishedAt) {
		t.Fatalf(
			"completed FinishedAt = %v; want %s",
			completed.FinishedAt,
			finishedAt,
		)
	}
	if completed.ErrorCode != "" {
		t.Fatalf(
			"completed ErrorCode = %q; want empty",
			completed.ErrorCode,
		)
	}

	recoveryCommand := beginCommand
	recoveryCommand.Key.FencingToken++
	recoveryCommand.StartedAt =
		beginCommand.StartedAt.Add(time.Minute)

	recovered, created, err := store.Begin(
		context.Background(),
		recoveryCommand,
	)
	if err != nil {
		t.Fatalf("recovery Begin() error = %v", err)
	}
	if created {
		t.Fatal(
			"recovery Begin() created = true; " +
				"want completed attempt",
		)
	}
	if recovered.Status != ActionAttemptStatusSucceeded {
		t.Fatalf(
			"recovered Status = %q; want %q",
			recovered.Status,
			ActionAttemptStatusSucceeded,
		)
	}
	if recovered.Version != completed.Version {
		t.Fatalf(
			"recovered Version = %d; want %d",
			recovered.Version,
			completed.Version,
		)
	}
}

func TestActionAttemptStoreCompletesStartedAttemptWithFailure(
	t *testing.T,
) {
	store := NewMemoryActionAttemptStore()
	beginCommand := validBeginActionAttemptCommand()

	started, created, err := store.Begin(
		context.Background(),
		beginCommand,
	)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if !created {
		t.Fatal("Begin() created = false; want true")
	}

	finishedAt := beginCommand.StartedAt.Add(5 * time.Second)

	completed, err := store.Complete(
		context.Background(),
		CompleteActionAttemptCommand{
			Key:             beginCommand.Key,
			ExpectedVersion: started.Version,
			To:              ActionAttemptStatusFailed,
			FinishedAt:      finishedAt,
			ErrorCode:       "KUBERNETES_ACTION_REJECTED",
		},
	)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	if completed.Status != ActionAttemptStatusFailed {
		t.Fatalf(
			"completed Status = %q; want %q",
			completed.Status,
			ActionAttemptStatusFailed,
		)
	}
	if completed.Version != started.Version+1 {
		t.Fatalf(
			"completed Version = %d; want %d",
			completed.Version,
			started.Version+1,
		)
	}
	if completed.FinishedAt == nil ||
		!completed.FinishedAt.Equal(finishedAt) {
		t.Fatalf(
			"completed FinishedAt = %v; want %s",
			completed.FinishedAt,
			finishedAt,
		)
	}
	if completed.ErrorCode !=
		"KUBERNETES_ACTION_REJECTED" {
		t.Fatalf(
			"completed ErrorCode = %q; want %q",
			completed.ErrorCode,
			"KUBERNETES_ACTION_REJECTED",
		)
	}
	if completed.RecoveredByFencingToken != 0 {
		t.Fatalf(
			"completed RecoveredByFencingToken = %d; want 0",
			completed.RecoveredByFencingToken,
		)
	}
}

func TestActionAttemptStoreRecoversAbandonedStartedAttemptAsUnknown(
	t *testing.T,
) {
	store := NewMemoryActionAttemptStore()
	beginCommand := validBeginActionAttemptCommand()

	started, created, err := store.Begin(
		context.Background(),
		beginCommand,
	)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if !created {
		t.Fatal("Begin() created = false; want true")
	}

	recoveryKey := beginCommand.Key
	recoveryKey.FencingToken++

	recoveredAt := beginCommand.StartedAt.Add(time.Minute)

	recovered, changed, err := store.Recover(
		context.Background(),
		RecoverActionAttemptCommand{
			Key:         recoveryKey,
			RecoveredAt: recoveredAt,
			ReasonCode:  "PREVIOUS_EXECUTOR_LOST",
		},
	)
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !changed {
		t.Fatal("Recover() changed = false; want true")
	}

	if recovered.ID != started.ID {
		t.Fatalf(
			"recovered ID = %q; want %q",
			recovered.ID,
			started.ID,
		)
	}
	if recovered.Status != ActionAttemptStatusUnknown {
		t.Fatalf(
			"recovered Status = %q; want %q",
			recovered.Status,
			ActionAttemptStatusUnknown,
		)
	}
	if recovered.Version != started.Version+1 {
		t.Fatalf(
			"recovered Version = %d; want %d",
			recovered.Version,
			started.Version+1,
		)
	}
	if recovered.FinishedAt == nil ||
		!recovered.FinishedAt.Equal(recoveredAt) {
		t.Fatalf(
			"recovered FinishedAt = %v; want %s",
			recovered.FinishedAt,
			recoveredAt,
		)
	}
	if recovered.ErrorCode != "PREVIOUS_EXECUTOR_LOST" {
		t.Fatalf(
			"recovered ErrorCode = %q; want %q",
			recovered.ErrorCode,
			"PREVIOUS_EXECUTOR_LOST",
		)
	}
	if recovered.Key.FencingToken !=
		beginCommand.Key.FencingToken {
		t.Fatalf(
			"attempt FencingToken = %d; want original %d",
			recovered.Key.FencingToken,
			beginCommand.Key.FencingToken,
		)
	}
	if recovered.RecoveredByFencingToken !=
		recoveryKey.FencingToken {
		t.Fatalf(
			"RecoveredByFencingToken = %d; want %d",
			recovered.RecoveredByFencingToken,
			recoveryKey.FencingToken,
		)
	}

	retry, changed, err := store.Recover(
		context.Background(),
		RecoverActionAttemptCommand{
			Key:         recoveryKey,
			RecoveredAt: recoveredAt,
			ReasonCode:  "PREVIOUS_EXECUTOR_LOST",
		},
	)
	if err != nil {
		t.Fatalf("retry Recover() error = %v", err)
	}
	if changed {
		t.Fatal("retry Recover() changed = true; want false")
	}
	if retry.Status != ActionAttemptStatusUnknown {
		t.Fatalf(
			"retry Status = %q; want %q",
			retry.Status,
			ActionAttemptStatusUnknown,
		)
	}
	if retry.Version != recovered.Version {
		t.Fatalf(
			"retry Version = %d; want %d",
			retry.Version,
			recovered.Version,
		)
	}
}
