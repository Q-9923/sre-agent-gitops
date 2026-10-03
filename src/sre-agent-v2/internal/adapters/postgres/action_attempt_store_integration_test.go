//go:build integration

package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	approvaldomain "sre-agent/internal/approval"
	"sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

const actionAttemptPostgresTestImage = "postgres:17.11-bookworm@sha256:639ab7ceb90e13123085b741fb31ef493fba25463002f6da665352e7b534b652"

func TestActionAttemptStoreBeginReturnsExistingAttemptForSameExecutionKeyPostgreSQL(
	t *testing.T,
) {
	pool, command := newActionAttemptPostgresFixture(
		t,
		"adapter-restart",
	)

	firstStore := NewActionAttemptStore(pool)
	first, created, err := firstStore.Begin(
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

	restartedStore := NewActionAttemptStore(pool)
	second, created, err := restartedStore.Begin(
		context.Background(),
		retry,
	)
	if err != nil {
		t.Fatalf("second Begin() error = %v", err)
	}
	if created {
		t.Fatal("second Begin() created = true; want false")
	}

	assertSameActionAttempt(t, second, first)
}

func TestActionAttemptStoreConcurrentBeginCreatesOnePostgreSQLAttempt(
	t *testing.T,
) {
	pool, command := newActionAttemptPostgresFixture(
		t,
		"concurrent",
	)
	store := NewActionAttemptStore(pool)

	const workers = 16

	type result struct {
		attempt remediationdomain.ActionAttempt
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

	var storedCount int
	err := pool.QueryRow(
		context.Background(),
		`
SELECT count(*)
FROM action_attempts
WHERE incident_id = $1
  AND plan_hash = $2
  AND target_uid = $3
  AND fencing_token = $4
`,
		command.Key.IncidentID,
		command.Key.PlanHash,
		command.Key.TargetUID,
		command.Key.FencingToken,
	).Scan(&storedCount)
	if err != nil {
		t.Fatalf("count action attempts error = %v", err)
	}
	if storedCount != 1 {
		t.Fatalf(
			"stored action attempt count = %d; want 1",
			storedCount,
		)
	}
}

func newActionAttemptPostgresFixture(
	t *testing.T,
	suffix string,
) (
	*pgxpool.Pool,
	remediationdomain.BeginActionAttemptCommand,
) {
	t.Helper()

	setupContext, cancelSetup := context.WithTimeout(
		context.Background(),
		2*time.Minute,
	)
	defer cancelSetup()

	container, err := tcpostgres.Run(
		setupContext,
		actionAttemptPostgresTestImage,
		tcpostgres.WithDatabase("sre_agent_test"),
		tcpostgres.WithUsername("sre_agent"),
		tcpostgres.WithPassword("integration-test-only"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start PostgreSQL container error = %v", err)
	}

	t.Cleanup(func() {
		cleanupContext, cancelCleanup := context.WithTimeout(
			context.Background(),
			30*time.Second,
		)
		defer cancelCleanup()

		if err := container.Terminate(cleanupContext); err != nil {
			t.Errorf(
				"terminate PostgreSQL container error = %v",
				err,
			)
		}
	})

	connectionString, err := container.ConnectionString(
		setupContext,
		"sslmode=disable",
	)
	if err != nil {
		t.Fatalf(
			"PostgreSQL connection string error = %v",
			err,
		)
	}

	pool, err := pgxpool.New(
		setupContext,
		connectionString,
	)
	if err != nil {
		t.Fatalf("create PostgreSQL pool error = %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(setupContext); err != nil {
		t.Fatalf("ping PostgreSQL error = %v", err)
	}
	if err := ApplyMigrations(setupContext, pool); err != nil {
		t.Fatalf("ApplyMigrations() error = %v", err)
	}

	observation := incident.Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "PodCrashLooping",
		Target: incident.Target{
			Kind:      "Pod",
			Namespace: "default",
			Name:      "crash-app-" + suffix,
			UID:       "pod-uid-" + suffix,
		},
	}

	registry := NewRegistry(pool)
	observed, created, err := registry.Observe(
		setupContext,
		observation,
	)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	claimTime := time.Date(
		2026,
		10,
		2,
		12,
		0,
		0,
		0,
		time.UTC,
	)
	claim, err := registry.Claim(
		setupContext,
		incident.ClaimCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: observed.Version,
			HolderID:        "agent-action-attempt",
			Now:             claimTime,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}

	plan, err := approvaldomain.NewPlan(
		approvaldomain.PlanCommand{
			IncidentID: observed.ID,
			Action:     "RESTART_POD",
			Target: approvaldomain.Target{
				Cluster:   observation.Cluster,
				Namespace: observation.Target.Namespace,
				Kind:      observation.Target.Kind,
				Name:      observation.Target.Name,
				UID:       observation.Target.UID,
			},
		},
	)
	if err != nil {
		t.Fatalf("NewPlan() error = %v", err)
	}

	if err := NewPlanStore(pool).Publish(
		setupContext,
		plan,
	); err != nil {
		t.Fatalf("PlanStore.Publish() error = %v", err)
	}

	return pool, remediationdomain.BeginActionAttemptCommand{
		Key: remediationdomain.ExecutionKey{
			IncidentID:   observed.ID,
			PlanHash:     plan.Hash,
			TargetUID:    plan.Target.UID,
			FencingToken: claim.Incident.Version,
		},
		StartedAt: claimTime.Add(time.Second),
	}
}

func assertSameActionAttempt(
	t *testing.T,
	actual remediationdomain.ActionAttempt,
	expected remediationdomain.ActionAttempt,
) {
	t.Helper()

	if actual.ID != expected.ID {
		t.Fatalf(
			"ActionAttempt ID = %q; want %q",
			actual.ID,
			expected.ID,
		)
	}
	if actual.Key != expected.Key {
		t.Fatalf(
			"ExecutionKey = %#v; want %#v",
			actual.Key,
			expected.Key,
		)
	}
	if !actual.StartedAt.Equal(expected.StartedAt) {
		t.Fatalf(
			"StartedAt = %s; want %s",
			actual.StartedAt,
			expected.StartedAt,
		)
	}
}
func TestActionAttemptStoreBeginReturnsExistingAcrossFencingTokensPostgreSQL(
	t *testing.T,
) {
	pool, firstCommand := newActionAttemptPostgresFixture(
		t,
		"cross-fencing-recovery",
	)

	firstStore := NewActionAttemptStore(pool)
	first, created, err := firstStore.Begin(
		context.Background(),
		firstCommand,
	)
	if err != nil {
		t.Fatalf("first Begin() error = %v", err)
	}
	if !created {
		t.Fatal("first Begin() created = false; want true")
	}

	registry := NewRegistry(pool)

	takeoverClaim, err := registry.Claim(
		context.Background(),
		incident.ClaimCommand{
			IncidentID: firstCommand.Key.IncidentID,
			ExpectedVersion: firstCommand.
				Key.FencingToken,
			HolderID: "agent-action-attempt-recovery",
			Now: firstCommand.StartedAt.Add(
				time.Minute,
			),
			LeaseDuration: 30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("takeover Claim() error = %v", err)
	}

	takeoverCommand := firstCommand
	takeoverCommand.Key.FencingToken =
		takeoverClaim.Incident.Version
	takeoverCommand.StartedAt =
		firstCommand.StartedAt.Add(time.Minute)

	restartedStore := NewActionAttemptStore(pool)
	recovered, created, err := restartedStore.Begin(
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

	var storedCount int

	err = pool.QueryRow(
		context.Background(),
		`
SELECT count(*)
FROM action_attempts
WHERE incident_id = $1
  AND plan_hash = $2
  AND target_uid = $3
`,
		firstCommand.Key.IncidentID,
		firstCommand.Key.PlanHash,
		firstCommand.Key.TargetUID,
	).Scan(&storedCount)
	if err != nil {
		t.Fatalf("count action attempts error = %v", err)
	}
	if storedCount != 1 {
		t.Fatalf(
			"stored action attempt count = %d; want 1",
			storedCount,
		)
	}
}
func TestActionAttemptStorePersistsCompletionAcrossAdapterRestartPostgreSQL(
	t *testing.T,
) {
	tests := []struct {
		name      string
		status    remediationdomain.ActionAttemptStatus
		errorCode string
	}{
		{
			name:   "succeeded",
			status: remediationdomain.ActionAttemptStatusSucceeded,
		},
		{
			name:      "failed",
			status:    remediationdomain.ActionAttemptStatusFailed,
			errorCode: "KUBERNETES_ACTION_REJECTED",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			pool, beginCommand :=
				newActionAttemptPostgresFixture(
					t,
					"completion-"+testCase.name,
				)

			store := NewActionAttemptStore(pool)

			started, created, err := store.Begin(
				context.Background(),
				beginCommand,
			)
			if err != nil {
				t.Fatalf("Begin() error = %v", err)
			}
			if !created {
				t.Fatal(
					"Begin() created = false; want true",
				)
			}

			finishedAt := beginCommand.StartedAt.Add(
				5 * time.Second,
			)

			completed, err := store.Complete(
				context.Background(),
				remediationdomain.
					CompleteActionAttemptCommand{
					Key: beginCommand.Key,
					ExpectedVersion: started.
						Version,
					To:         testCase.status,
					FinishedAt: finishedAt,
					ErrorCode:  testCase.errorCode,
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
			if completed.Status != testCase.status {
				t.Fatalf(
					"completed Status = %q; want %q",
					completed.Status,
					testCase.status,
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
				testCase.errorCode {
				t.Fatalf(
					"completed ErrorCode = %q; want %q",
					completed.ErrorCode,
					testCase.errorCode,
				)
			}
			if completed.RecoveredByFencingToken != 0 {
				t.Fatalf(
					"completed RecoveredByFencingToken = %d; want 0",
					completed.
						RecoveredByFencingToken,
				)
			}

			restartedStore := NewActionAttemptStore(pool)

			retryCommand := beginCommand
			retryCommand.StartedAt =
				beginCommand.StartedAt.Add(time.Hour)

			restored, created, err :=
				restartedStore.Begin(
					context.Background(),
					retryCommand,
				)
			if err != nil {
				t.Fatalf(
					"restarted Begin() error = %v",
					err,
				)
			}
			if created {
				t.Fatal(
					"restarted Begin() created = true; want false",
				)
			}

			if restored.Status != testCase.status {
				t.Fatalf(
					"restored Status = %q; want %q",
					restored.Status,
					testCase.status,
				)
			}
			if restored.Version != completed.Version {
				t.Fatalf(
					"restored Version = %d; want %d",
					restored.Version,
					completed.Version,
				)
			}
			if restored.FinishedAt == nil ||
				!restored.FinishedAt.Equal(finishedAt) {
				t.Fatalf(
					"restored FinishedAt = %v; want %s",
					restored.FinishedAt,
					finishedAt,
				)
			}
			if restored.ErrorCode !=
				testCase.errorCode {
				t.Fatalf(
					"restored ErrorCode = %q; want %q",
					restored.ErrorCode,
					testCase.errorCode,
				)
			}
			if restored.RecoveredByFencingToken != 0 {
				t.Fatalf(
					"restored RecoveredByFencingToken = %d; want 0",
					restored.
						RecoveredByFencingToken,
				)
			}
		})
	}
}

func TestActionAttemptStoreRecoversAbandonedStartedAttemptPostgreSQL(
	t *testing.T,
) {
	pool, beginCommand := newActionAttemptPostgresFixture(
		t,
		"abandoned-recovery",
	)

	firstStore := NewActionAttemptStore(pool)

	started, created, err := firstStore.Begin(
		context.Background(),
		beginCommand,
	)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if !created {
		t.Fatal("Begin() created = false; want true")
	}

	registry := NewRegistry(pool)

	takeoverTime := beginCommand.StartedAt.Add(time.Minute)

	takeover, err := registry.Claim(
		context.Background(),
		incident.ClaimCommand{
			IncidentID: beginCommand.Key.IncidentID,
			ExpectedVersion: beginCommand.
				Key.FencingToken,
			HolderID:      "agent-recovery",
			Now:           takeoverTime,
			LeaseDuration: 30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("takeover Claim() error = %v", err)
	}

	recoveryKey := beginCommand.Key
	recoveryKey.FencingToken =
		takeover.Incident.Version

	restartedStore := NewActionAttemptStore(pool)

	recovered, changed, err := restartedStore.Recover(
		context.Background(),
		remediationdomain.RecoverActionAttemptCommand{
			Key:         recoveryKey,
			RecoveredAt: takeoverTime,
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
	if recovered.Status !=
		remediationdomain.ActionAttemptStatusUnknown {
		t.Fatalf(
			"recovered Status = %q; want %q",
			recovered.Status,
			remediationdomain.
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
		!recovered.FinishedAt.Equal(takeoverTime) {
		t.Fatalf(
			"recovered FinishedAt = %v; want %s",
			recovered.FinishedAt,
			takeoverTime,
		)
	}
	if recovered.ErrorCode !=
		"PREVIOUS_EXECUTOR_LOST" {
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

	secondRestart := NewActionAttemptStore(pool)

	retry, changed, err := secondRestart.Recover(
		context.Background(),
		remediationdomain.RecoverActionAttemptCommand{
			Key:         recoveryKey,
			RecoveredAt: takeoverTime,
			ReasonCode:  "PREVIOUS_EXECUTOR_LOST",
		},
	)
	if err != nil {
		t.Fatalf("retry Recover() error = %v", err)
	}
	if changed {
		t.Fatal("retry Recover() changed = true; want false")
	}
	if retry.Status !=
		remediationdomain.ActionAttemptStatusUnknown {
		t.Fatalf(
			"retry Status = %q; want %q",
			retry.Status,
			remediationdomain.
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

	retryBegin := beginCommand
	retryBegin.Key = recoveryKey
	retryBegin.StartedAt = takeoverTime.Add(time.Second)

	restored, created, err := secondRestart.Begin(
		context.Background(),
		retryBegin,
	)
	if err != nil {
		t.Fatalf("restarted Begin() error = %v", err)
	}
	if created {
		t.Fatal(
			"restarted Begin() created = true; want false",
		)
	}
	if restored.Status !=
		remediationdomain.ActionAttemptStatusUnknown {
		t.Fatalf(
			"restored Status = %q; want %q",
			restored.Status,
			remediationdomain.
				ActionAttemptStatusUnknown,
		)
	}
	if restored.RecoveredByFencingToken !=
		recoveryKey.FencingToken {
		t.Fatalf(
			"restored RecoveredByFencingToken = %d; want %d",
			restored.RecoveredByFencingToken,
			recoveryKey.FencingToken,
		)
	}
}
