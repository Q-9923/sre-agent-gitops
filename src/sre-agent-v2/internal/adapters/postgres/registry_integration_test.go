//go:build integration

package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"sre-agent/internal/incident"
)

const postgresTestImage = "postgres:17.11-bookworm@sha256:639ab7ceb90e13123085b741fb31ef493fba25463002f6da665352e7b534b652"

func TestRegistryObserveConcurrentlyCreatesOneActiveIncident(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	container, err := tcpostgres.Run(
		ctx,
		postgresTestImage,
		tcpostgres.WithDatabase("sre_agent_test"),
		tcpostgres.WithUsername("sre_agent"),
		tcpostgres.WithPassword("integration-test-only"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start PostgreSQL container: %v", err)
	}
	testcontainers.CleanupContainer(t, container)

	connectionString, err := container.ConnectionString(
		ctx,
		"sslmode=disable",
	)
	if err != nil {
		t.Fatalf("get PostgreSQL connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, connectionString)
	if err != nil {
		t.Fatalf("create PostgreSQL pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	if err := ApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply incident migrations: %v", err)
	}

	registry := NewRegistry(pool)

	observation := incident.Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "KubePodCrashLooping",
		Target: incident.Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app",
			UID:       "pod-uid-concurrent-observe",
		},
	}

	const workers = 32

	type observeResult struct {
		value   incident.Incident
		created bool
		err     error
	}

	start := make(chan struct{})
	results := make(chan observeResult, workers)

	var waitGroup sync.WaitGroup
	waitGroup.Add(workers)

	for range workers {
		go func() {
			defer waitGroup.Done()
			<-start

			value, created, observeErr := registry.Observe(
				ctx,
				observation,
			)
			results <- observeResult{
				value:   value,
				created: created,
				err:     observeErr,
			}
		}()
	}

	close(start)
	waitGroup.Wait()
	close(results)

	var incidentID string
	createdCount := 0

	for result := range results {
		if result.err != nil {
			t.Fatalf("Observe() error: %v", result.err)
		}

		if incidentID == "" {
			incidentID = result.value.ID
		}

		if result.value.ID != incidentID {
			t.Fatalf(
				"Observe() incident ID = %q; want %q",
				result.value.ID,
				incidentID,
			)
		}

		if result.created {
			createdCount++
		}
	}

	if createdCount != 1 {
		t.Fatalf(
			"created count = %d; want 1",
			createdCount,
		)
	}

	var activeCount int
	if err := pool.QueryRow(
		ctx,
		`SELECT count(*) FROM incidents WHERE resolved_at IS NULL`,
	).Scan(&activeCount); err != nil {
		t.Fatalf("count active incidents: %v", err)
	}

	if activeCount != 1 {
		t.Fatalf(
			"active incident count = %d; want 1",
			activeCount,
		)
	}
}
func TestRegistryTransitionPersistsAuditAndAllowsRecurrence(
	t *testing.T,
) {
	ctx, pool, registry := newPostgresTestRegistry(t)

	observation := incident.Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "KubePodCrashLooping",
		Target: incident.Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app",
			UID:       "pod-uid-transition-recurrence",
		},
	}

	first, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("first Observe() error: %v", err)
	}
	if !created {
		t.Fatal("first Observe() created = false; want true")
	}

	resolved, err := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      first.ID,
			ExpectedVersion: first.Version,
			To:              incident.StateResolved,
			Actor:           "integration-test",
			ReasonCode:      "ALERT_RESOLVED",
		},
	)
	if err != nil {
		t.Fatalf("Transition() error: %v", err)
	}

	if resolved.State != incident.StateResolved {
		t.Fatalf(
			"resolved state = %q; want %q",
			resolved.State,
			incident.StateResolved,
		)
	}

	if resolved.Version != first.Version+1 {
		t.Fatalf(
			"resolved version = %d; want %d",
			resolved.Version,
			first.Version+1,
		)
	}

	var (
		persistedState   string
		persistedVersion int64
		hasResolvedAt    bool
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
    state,
    version,
    resolved_at IS NOT NULL
FROM incidents
WHERE id = $1
`,
		first.ID,
	).Scan(
		&persistedState,
		&persistedVersion,
		&hasResolvedAt,
	)
	if err != nil {
		t.Fatalf("read resolved incident: %v", err)
	}

	if persistedState != string(incident.StateResolved) {
		t.Fatalf(
			"persisted state = %q; want %q",
			persistedState,
			incident.StateResolved,
		)
	}

	if persistedVersion != int64(first.Version+1) {
		t.Fatalf(
			"persisted version = %d; want %d",
			persistedVersion,
			first.Version+1,
		)
	}

	if !hasResolvedAt {
		t.Fatal("resolved_at is NULL; want a resolution timestamp")
	}

	var (
		auditActor      string
		auditReasonCode string
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT actor, reason_code
FROM incident_transitions
WHERE incident_id = $1
  AND version = $2
`,
		first.ID,
		resolved.Version,
	).Scan(
		&auditActor,
		&auditReasonCode,
	)
	if err != nil {
		t.Fatalf("read transition audit: %v", err)
	}

	if auditActor != "integration-test" {
		t.Fatalf(
			"audit actor = %q; want %q",
			auditActor,
			"integration-test",
		)
	}

	if auditReasonCode != "ALERT_RESOLVED" {
		t.Fatalf(
			"audit reason code = %q; want %q",
			auditReasonCode,
			"ALERT_RESOLVED",
		)
	}

	second, secondCreated, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("second Observe() error: %v", err)
	}
	if !secondCreated {
		t.Fatal("second Observe() created = false; want true")
	}
	if second.ID == first.ID {
		t.Fatalf(
			"second incident ID = %q; want a new occurrence",
			second.ID,
		)
	}

	var (
		totalCount  int
		activeCount int
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
    count(*),
    count(*) FILTER (WHERE resolved_at IS NULL)
FROM incidents
`,
	).Scan(
		&totalCount,
		&activeCount,
	)
	if err != nil {
		t.Fatalf("count persisted incidents: %v", err)
	}

	if totalCount != 2 {
		t.Fatalf(
			"total incident count = %d; want 2",
			totalCount,
		)
	}

	if activeCount != 1 {
		t.Fatalf(
			"active incident count = %d; want 1",
			activeCount,
		)
	}
}

func newPostgresTestRegistry(
	t *testing.T,
) (context.Context, *pgxpool.Pool, *Registry) {
	t.Helper()

	ctx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Minute,
	)
	t.Cleanup(cancel)

	container, err := tcpostgres.Run(
		ctx,
		postgresTestImage,
		tcpostgres.WithDatabase("sre_agent_test"),
		tcpostgres.WithUsername("sre_agent"),
		tcpostgres.WithPassword("integration-test-only"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start PostgreSQL container: %v", err)
	}
	testcontainers.CleanupContainer(t, container)

	connectionString, err := container.ConnectionString(
		ctx,
		"sslmode=disable",
	)
	if err != nil {
		t.Fatalf("get PostgreSQL connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, connectionString)
	if err != nil {
		t.Fatalf("create PostgreSQL pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	if err := ApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply incident migrations: %v", err)
	}

	if err := ApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply incident migrations: %v", err)
	}

	return ctx, pool, NewRegistry(pool)
}
func TestRegistryTransitionAllowsOneConcurrentVersionWinner(
	t *testing.T,
) {
	ctx, pool, registry := newPostgresTestRegistry(t)

	current, created, err := registry.Observe(
		ctx,
		incident.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "KubePodCrashLooping",
			Target: incident.Target{
				Kind:      "Pod",
				Namespace: "sre-agent-lab",
				Name:      "crash-app",
				UID:       "pod-uid-concurrent-transition",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error: %v", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	command := incident.TransitionCommand{
		IncidentID:      current.ID,
		ExpectedVersion: current.Version,
		To:              incident.StateResolved,
		Actor:           "integration-test",
		ReasonCode:      "ALERT_RESOLVED",
	}

	type transitionResult struct {
		value incident.Incident
		err   error
	}

	const workers = 2

	start := make(chan struct{})
	results := make(chan transitionResult, workers)

	var waitGroup sync.WaitGroup
	waitGroup.Add(workers)

	for range workers {
		go func() {
			defer waitGroup.Done()
			<-start

			value, transitionErr := registry.Transition(
				ctx,
				command,
			)
			results <- transitionResult{
				value: value,
				err:   transitionErr,
			}
		}()
	}

	close(start)
	waitGroup.Wait()
	close(results)

	successCount := 0
	versionConflictCount := 0

	for result := range results {
		switch {
		case result.err == nil:
			successCount++

			if result.value.State != incident.StateResolved {
				t.Fatalf(
					"successful state = %q; want %q",
					result.value.State,
					incident.StateResolved,
				)
			}

		case errors.Is(
			result.err,
			incident.ErrVersionConflict,
		):
			versionConflictCount++

		default:
			t.Fatalf(
				"unexpected Transition() error: %v",
				result.err,
			)
		}
	}

	if successCount != 1 {
		t.Fatalf(
			"successful transitions = %d; want 1",
			successCount,
		)
	}

	if versionConflictCount != 1 {
		t.Fatalf(
			"version conflicts = %d; want 1",
			versionConflictCount,
		)
	}

	var (
		persistedState   string
		persistedVersion int64
		auditCount       int
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
    incidents.state,
    incidents.version,
    count(incident_transitions.incident_id)
FROM incidents
LEFT JOIN incident_transitions
    ON incident_transitions.incident_id = incidents.id
WHERE incidents.id = $1
GROUP BY incidents.id
`,
		current.ID,
	).Scan(
		&persistedState,
		&persistedVersion,
		&auditCount,
	)
	if err != nil {
		t.Fatalf("read concurrent transition result: %v", err)
	}

	if persistedState != string(incident.StateResolved) {
		t.Fatalf(
			"persisted state = %q; want %q",
			persistedState,
			incident.StateResolved,
		)
	}

	if persistedVersion != 2 {
		t.Fatalf(
			"persisted version = %d; want 2",
			persistedVersion,
		)
	}

	if auditCount != 1 {
		t.Fatalf(
			"audit count = %d; want 1",
			auditCount,
		)
	}
}
func TestRegistryTransitionRollsBackWhenAuditInsertFails(
	t *testing.T,
) {
	ctx, pool, registry := newPostgresTestRegistry(t)

	current, created, err := registry.Observe(
		ctx,
		incident.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "KubePodCrashLooping",
			Target: incident.Target{
				Kind:      "Pod",
				Namespace: "sre-agent-lab",
				Name:      "crash-app",
				UID:       "pod-uid-audit-rollback",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error: %v", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	_, err = pool.Exec(
		ctx,
		`
ALTER TABLE incident_transitions
ADD CONSTRAINT reject_forced_audit_failure
CHECK (actor <> 'force-audit-failure')
`,
	)
	if err != nil {
		t.Fatalf("install audit failure constraint: %v", err)
	}

	_, transitionErr := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      current.ID,
			ExpectedVersion: current.Version,
			To:              incident.StateResolved,
			Actor:           "force-audit-failure",
			ReasonCode:      "TEST_TRANSACTION_ROLLBACK",
		},
	)
	if transitionErr == nil {
		t.Fatal("Transition() error = nil; want audit insert failure")
	}

	var (
		persistedState   string
		persistedVersion int64
		hasResolvedAt    bool
		auditCount       int
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
    incidents.state,
    incidents.version,
    incidents.resolved_at IS NOT NULL,
    count(incident_transitions.incident_id)
FROM incidents
LEFT JOIN incident_transitions
    ON incident_transitions.incident_id = incidents.id
WHERE incidents.id = $1
GROUP BY incidents.id
`,
		current.ID,
	).Scan(
		&persistedState,
		&persistedVersion,
		&hasResolvedAt,
		&auditCount,
	)
	if err != nil {
		t.Fatalf("read rolled back incident: %v", err)
	}

	if persistedState != string(incident.StateDetected) {
		t.Fatalf(
			"persisted state = %q; want %q",
			persistedState,
			incident.StateDetected,
		)
	}

	if persistedVersion != 1 {
		t.Fatalf(
			"persisted version = %d; want 1",
			persistedVersion,
		)
	}

	if hasResolvedAt {
		t.Fatal("resolved_at was persisted after audit failure")
	}

	if auditCount != 0 {
		t.Fatalf(
			"audit count = %d; want 0",
			auditCount,
		)
	}
}
