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
