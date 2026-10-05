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
func TestRegistryTransitionToDiagnosedKeepsIncidentActive(
	t *testing.T,
) {
	ctx, _, registry := newPostgresTestRegistry(t)
	observation := incident.Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "KubePodCrashLooping",
		Target: incident.Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app-diagnosed",
			UID:       "pod-uid-diagnosed",
		},
	}

	detected, created, err := registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}
	if detected.State != incident.StateDetected {
		t.Fatalf(
			"detected State = %q; want %q",
			detected.State,
			incident.StateDetected,
		)
	}
	if detected.Version != 1 {
		t.Fatalf(
			"detected Version = %d; want 1",
			detected.Version,
		)
	}

	diagnosed, err := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: 1,
			To:              incident.StateDiagnosed,
			Actor:           "sre-agent",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf("Transition() error = %v; want nil", err)
	}
	if diagnosed.ID != detected.ID {
		t.Fatalf(
			"diagnosed Incident ID = %q; want %q",
			diagnosed.ID,
			detected.ID,
		)
	}
	if diagnosed.State != incident.StateDiagnosed {
		t.Fatalf(
			"diagnosed State = %q; want %q",
			diagnosed.State,
			incident.StateDiagnosed,
		)
	}
	if diagnosed.Version != 2 {
		t.Fatalf(
			"diagnosed Version = %d; want 2",
			diagnosed.Version,
		)
	}

	observedAgain, createdAgain, err := registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("second Observe() error = %v; want nil", err)
	}
	if createdAgain {
		t.Fatal(
			"second Observe() created = true; " +
				"want false while the incident remains active",
		)
	}
	if observedAgain.ID != detected.ID {
		t.Fatalf(
			"second Observe() Incident ID = %q; want %q",
			observedAgain.ID,
			detected.ID,
		)
	}
	if observedAgain.State != incident.StateDiagnosed {
		t.Fatalf(
			"second Observe() State = %q; want %q",
			observedAgain.State,
			incident.StateDiagnosed,
		)
	}
	if observedAgain.Version != 2 {
		t.Fatalf(
			"second Observe() Version = %d; want 2",
			observedAgain.Version,
		)
	}
}
func TestRegistryTransitionFromDiagnosedToResolvedReleasesIncident(
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
			Name:      "crash-app-recovered",
			UID:       "pod-uid-recovered",
		},
	}

	detected, created, err := registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: 1,
			To:              incident.StateDiagnosed,
			Actor:           "sre-agent",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"transition to DIAGNOSED error = %v; want nil",
			err,
		)
	}
	if diagnosed.State != incident.StateDiagnosed {
		t.Fatalf(
			"diagnosed State = %q; want %q",
			diagnosed.State,
			incident.StateDiagnosed,
		)
	}
	if diagnosed.Version != 2 {
		t.Fatalf(
			"diagnosed Version = %d; want 2",
			diagnosed.Version,
		)
	}

	resolved, err := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: 2,
			To:              incident.StateResolved,
			Actor:           "sre-agent",
			ReasonCode:      "RECOVERY_VERIFIED",
		},
	)
	if err != nil {
		t.Fatalf(
			"transition to RESOLVED error = %v; want nil",
			err,
		)
	}
	if resolved.ID != detected.ID {
		t.Fatalf(
			"resolved Incident ID = %q; want %q",
			resolved.ID,
			detected.ID,
		)
	}
	if resolved.State != incident.StateResolved {
		t.Fatalf(
			"resolved State = %q; want %q",
			resolved.State,
			incident.StateResolved,
		)
	}
	if resolved.Version != 3 {
		t.Fatalf(
			"resolved Version = %d; want 3",
			resolved.Version,
		)
	}
	var (
		persistedState   string
		persistedVersion int64
		resolvedAt       time.Time
		auditFromState   string
		auditToState     string
		auditActor       string
		auditReasonCode  string
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
    incidents.state,
    incidents.version,
    incidents.resolved_at,
    transitions.from_state,
    transitions.to_state,
    transitions.actor,
    transitions.reason_code
FROM incidents
JOIN incident_transitions AS transitions
  ON transitions.incident_id = incidents.id
 AND transitions.version = incidents.version
WHERE incidents.id = $1
`,
		resolved.ID,
	).Scan(
		&persistedState,
		&persistedVersion,
		&resolvedAt,
		&auditFromState,
		&auditToState,
		&auditActor,
		&auditReasonCode,
	)
	if err != nil {
		t.Fatalf(
			"query resolved incident and audit error = %v; want nil",
			err,
		)
	}
	if persistedState != string(incident.StateResolved) {
		t.Fatalf(
			"persisted State = %q; want %q",
			persistedState,
			incident.StateResolved,
		)
	}
	if persistedVersion != 3 {
		t.Fatalf(
			"persisted Version = %d; want 3",
			persistedVersion,
		)
	}
	if resolvedAt.IsZero() {
		t.Fatal(
			"persisted resolved_at is zero; want resolution timestamp",
		)
	}
	if auditFromState != string(incident.StateDiagnosed) {
		t.Fatalf(
			"audit from_state = %q; want %q",
			auditFromState,
			incident.StateDiagnosed,
		)
	}
	if auditToState != string(incident.StateResolved) {
		t.Fatalf(
			"audit to_state = %q; want %q",
			auditToState,
			incident.StateResolved,
		)
	}
	if auditActor != "sre-agent" {
		t.Fatalf(
			"audit actor = %q; want %q",
			auditActor,
			"sre-agent",
		)
	}
	if auditReasonCode != "RECOVERY_VERIFIED" {
		t.Fatalf(
			"audit reason_code = %q; want %q",
			auditReasonCode,
			"RECOVERY_VERIFIED",
		)
	}
	recurrent, recurrentCreated, err := registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("recurrent Observe() error = %v; want nil", err)
	}
	if !recurrentCreated {
		t.Fatal(
			"recurrent Observe() created = false; " +
				"want true after the previous incident was resolved",
		)
	}
	if recurrent.ID == resolved.ID {
		t.Fatalf(
			"recurrent Incident ID = %q; want a new ID",
			recurrent.ID,
		)
	}
	if recurrent.State != incident.StateDetected {
		t.Fatalf(
			"recurrent State = %q; want %q",
			recurrent.State,
			incident.StateDetected,
		)
	}
	if recurrent.Version != 1 {
		t.Fatalf(
			"recurrent Version = %d; want 1",
			recurrent.Version,
		)
	}
}
func TestRegistryClaimAllowsOneConcurrentVersionWinner(t *testing.T) {
	ctx, _, registry := newPostgresTestRegistry(t)

	observed, created, err := registry.Observe(
		ctx,
		incident.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "KubePodCrashLooping",
			Target: incident.Target{
				Kind:      "Pod",
				Namespace: "sre-agent-lab",
				Name:      "crash-app-claim",
				UID:       "pod-uid-postgres-claim",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	now := time.Date(2026, 9, 28, 15, 0, 0, 0, time.UTC)
	holders := []string{"agent-a", "agent-b"}

	type claimResult struct {
		holderID string
		claim    incident.Claim
		err      error
	}

	start := make(chan struct{})
	results := make(chan claimResult, len(holders))

	var waitGroup sync.WaitGroup
	waitGroup.Add(len(holders))

	for _, holderID := range holders {
		go func(holderID string) {
			defer waitGroup.Done()
			<-start

			claim, claimErr := registry.Claim(
				ctx,
				incident.ClaimCommand{
					IncidentID:      observed.ID,
					ExpectedVersion: observed.Version,
					HolderID:        holderID,
					Now:             now,
					LeaseDuration:   30 * time.Second,
				},
			)

			results <- claimResult{
				holderID: holderID,
				claim:    claim,
				err:      claimErr,
			}
		}(holderID)
	}

	close(start)
	waitGroup.Wait()
	close(results)

	successes := 0
	versionConflicts := 0

	for result := range results {
		if errors.Is(result.err, incident.ErrVersionConflict) {
			versionConflicts++
			continue
		}
		if result.err != nil {
			t.Fatalf(
				"Claim(%q) error = %v; want nil or ErrVersionConflict",
				result.holderID,
				result.err,
			)
		}

		successes++

		if result.claim.HolderID != result.holderID {
			t.Fatalf(
				"Claim HolderID = %q; want %q",
				result.claim.HolderID,
				result.holderID,
			)
		}
		if result.claim.Incident.ID != observed.ID {
			t.Fatalf(
				"Claim Incident ID = %q; want %q",
				result.claim.Incident.ID,
				observed.ID,
			)
		}
		if result.claim.Incident.State != incident.StateDetected {
			t.Fatalf(
				"Claim Incident State = %q; want %q",
				result.claim.Incident.State,
				incident.StateDetected,
			)
		}
		if result.claim.Incident.Version != 2 {
			t.Fatalf(
				"Claim Incident Version = %d; want 2",
				result.claim.Incident.Version,
			)
		}
		if !result.claim.ExpiresAt.Equal(now.Add(30 * time.Second)) {
			t.Fatalf(
				"Claim ExpiresAt = %s; want %s",
				result.claim.ExpiresAt,
				now.Add(30*time.Second),
			)
		}
	}

	if successes != 1 {
		t.Fatalf("successful Claims = %d; want 1", successes)
	}
	if versionConflicts != 1 {
		t.Fatalf(
			"Claim version conflicts = %d; want 1",
			versionConflicts,
		)
	}
}
func TestRegistryClaimLeaseSurvivesAdapterRestartAndExpires(
	t *testing.T,
) {
	ctx, pool, firstRegistry := newPostgresTestRegistry(t)

	observed, created, err := firstRegistry.Observe(
		ctx,
		incident.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "KubePodCrashLooping",
			Target: incident.Target{
				Kind:      "Pod",
				Namespace: "sre-agent-lab",
				Name:      "crash-app-persisted-lease",
				UID:       "pod-uid-persisted-lease",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	now := time.Date(2026, 9, 28, 16, 0, 0, 0, time.UTC)

	firstClaim, err := firstRegistry.Claim(
		ctx,
		incident.ClaimCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: observed.Version,
			HolderID:        "agent-a",
			Now:             now,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("first Claim() error = %v; want nil", err)
	}

	restartedRegistry := NewRegistry(pool)

	_, err = restartedRegistry.Claim(
		ctx,
		incident.ClaimCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: firstClaim.Incident.Version,
			HolderID:        "agent-b",
			Now:             now.Add(10 * time.Second),
			LeaseDuration:   30 * time.Second,
		},
	)
	if !errors.Is(err, incident.ErrLeaseHeld) {
		t.Fatalf(
			"active Lease Claim() error = %v; want ErrLeaseHeld",
			err,
		)
	}

	takeover, err := restartedRegistry.Claim(
		ctx,
		incident.ClaimCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: firstClaim.Incident.Version,
			HolderID:        "agent-b",
			Now:             firstClaim.ExpiresAt,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("expired Lease takeover error = %v; want nil", err)
	}
	if takeover.HolderID != "agent-b" {
		t.Fatalf(
			"takeover HolderID = %q; want %q",
			takeover.HolderID,
			"agent-b",
		)
	}
	if takeover.Incident.State != incident.StateDetected {
		t.Fatalf(
			"takeover State = %q; want %q",
			takeover.Incident.State,
			incident.StateDetected,
		)
	}
	if takeover.Incident.Version != 3 {
		t.Fatalf(
			"takeover Version = %d; want 3",
			takeover.Incident.Version,
		)
	}

	expectedExpiry := now.Add(60 * time.Second)
	if !takeover.ExpiresAt.Equal(expectedExpiry) {
		t.Fatalf(
			"takeover ExpiresAt = %s; want %s",
			takeover.ExpiresAt,
			expectedExpiry,
		)
	}
}
func TestRegistryClaimHistorySurvivesAdapterRestart(t *testing.T) {
	ctx, pool, firstRegistry := newPostgresTestRegistry(t)

	observed, created, err := firstRegistry.Observe(
		ctx,
		incident.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "KubePodCrashLooping",
			Target: incident.Target{
				Kind:      "Pod",
				Namespace: "sre-agent-lab",
				Name:      "crash-app-claim-audit",
				UID:       "pod-uid-claim-audit",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	acquiredAt := time.Date(
		2026,
		9,
		28,
		17,
		0,
		0,
		0,
		time.UTC,
	)

	_, err = firstRegistry.Claim(
		ctx,
		incident.ClaimCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: observed.Version,
			HolderID:        "agent-a",
			Now:             acquiredAt,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("Claim() error = %v; want nil", err)
	}

	restartedRegistry := NewRegistry(pool)

	var auditReader incident.ClaimAuditReader = restartedRegistry

	history, err := auditReader.ClaimHistory(ctx, observed.ID)
	if err != nil {
		t.Fatalf("ClaimHistory() error = %v; want nil", err)
	}
	if len(history) != 1 {
		t.Fatalf(
			"ClaimHistory() length = %d; want 1",
			len(history),
		)
	}

	event := history[0]

	if event.IncidentID != observed.ID {
		t.Fatalf(
			"event IncidentID = %q; want %q",
			event.IncidentID,
			observed.ID,
		)
	}
	if event.IncidentVersion != 2 {
		t.Fatalf(
			"event IncidentVersion = %d; want 2",
			event.IncidentVersion,
		)
	}
	if event.HolderID != "agent-a" {
		t.Fatalf(
			"event HolderID = %q; want %q",
			event.HolderID,
			"agent-a",
		)
	}
	if !event.AcquiredAt.Equal(acquiredAt) {
		t.Fatalf(
			"event AcquiredAt = %s; want %s",
			event.AcquiredAt,
			acquiredAt,
		)
	}

	expectedExpiry := acquiredAt.Add(30 * time.Second)
	if !event.ExpiresAt.Equal(expectedExpiry) {
		t.Fatalf(
			"event ExpiresAt = %s; want %s",
			event.ExpiresAt,
			expectedExpiry,
		)
	}
}
func TestRegistryTransitionRejectsStaleClaimVersionAfterLeaseTakeover(
	t *testing.T,
) {
	ctx, _, registry := newPostgresTestRegistry(t)

	observed, created, err := registry.Observe(
		ctx,
		incident.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "KubePodCrashLooping",
			Target: incident.Target{
				Kind:      "Pod",
				Namespace: "sre-agent-lab",
				Name:      "crash-app-postgres-fencing",
				UID:       "pod-uid-postgres-fencing",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	now := time.Date(
		2026,
		9,
		28,
		18,
		30,
		0,
		0,
		time.UTC,
	)

	staleClaim, err := registry.Claim(
		ctx,
		incident.ClaimCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: observed.Version,
			HolderID:        "agent-a",
			Now:             now,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("agent-a Claim() error = %v; want nil", err)
	}

	currentClaim, err := registry.Claim(
		ctx,
		incident.ClaimCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: staleClaim.Incident.Version,
			HolderID:        "agent-b",
			Now:             staleClaim.ExpiresAt,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("agent-b takeover Claim() error = %v; want nil", err)
	}

	_, err = registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: staleClaim.Incident.Version,
			To:              incident.StateDiagnosed,
			Actor:           "agent-a",
			ReasonCode:      "STALE_LEASE_HOLDER",
		},
	)
	if !errors.Is(err, incident.ErrVersionConflict) {
		t.Fatalf(
			"stale holder Transition() error = %v; want ErrVersionConflict",
			err,
		)
	}

	diagnosed, err := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: currentClaim.Incident.Version,
			To:              incident.StateDiagnosed,
			Actor:           "agent-b",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"current holder Transition() error = %v; want nil",
			err,
		)
	}
	if diagnosed.State != incident.StateDiagnosed {
		t.Fatalf(
			"diagnosed State = %q; want %q",
			diagnosed.State,
			incident.StateDiagnosed,
		)
	}
	if diagnosed.Version != 4 {
		t.Fatalf(
			"diagnosed Version = %d; want 4",
			diagnosed.Version,
		)
	}
}

func TestRegistryTransitionPersistsWaitingApprovalAcrossAdapterRestart(
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
			Name:      "crash-app-waiting-approval",
			UID:       "pod-uid-postgres-waiting-approval",
		},
	}

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              incident.StateDiagnosed,
			Actor:           "sre-agent-v2",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf("Transition(DIAGNOSED) error = %v; want nil", err)
	}

	const (
		planHash  = "a2f50b614b76121f9547762fe28deba170246779442073099f18b25ca9cf87c5"
		targetUID = "pod-uid-postgres-waiting-approval"
	)

	waiting, err := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              incident.StateWaitingApproval,
			Actor:           "sre-agent-v2",
			ReasonCode:      "APPROVAL_REQUIRED",
			ApprovalBinding: incident.ApprovalBinding{
				PlanHash:  planHash,
				TargetUID: targetUID,
			},
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(WAITING_APPROVAL) error = %v; want nil",
			err,
		)
	}

	if waiting.State != incident.StateWaitingApproval {
		t.Fatalf(
			"waiting State = %q; want %q",
			waiting.State,
			incident.StateWaitingApproval,
		)
	}
	if waiting.Version != diagnosed.Version+1 {
		t.Fatalf(
			"waiting Version = %d; want %d",
			waiting.Version,
			diagnosed.Version+1,
		)
	}
	if waiting.ApprovalBinding.PlanHash != planHash {
		t.Fatalf(
			"waiting PlanHash = %q; want %q",
			waiting.ApprovalBinding.PlanHash,
			planHash,
		)
	}
	if waiting.ApprovalBinding.TargetUID != targetUID {
		t.Fatalf(
			"waiting TargetUID = %q; want %q",
			waiting.ApprovalBinding.TargetUID,
			targetUID,
		)
	}

	var (
		persistedState     string
		persistedVersion   int64
		persistedPlanHash  string
		persistedTargetUID string
		resolvedAtIsNull   bool
		auditFromState     string
		auditToState       string
		auditActor         string
		auditReasonCode    string
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
    incidents.state,
    incidents.version,
    incidents.approval_plan_hash,
    incidents.approval_target_uid,
    incidents.resolved_at IS NULL,
    transitions.from_state,
    transitions.to_state,
    transitions.actor,
    transitions.reason_code
FROM incidents
JOIN incident_transitions AS transitions
  ON transitions.incident_id = incidents.id
 AND transitions.version = incidents.version
WHERE incidents.id = $1
`,
		waiting.ID,
	).Scan(
		&persistedState,
		&persistedVersion,
		&persistedPlanHash,
		&persistedTargetUID,
		&resolvedAtIsNull,
		&auditFromState,
		&auditToState,
		&auditActor,
		&auditReasonCode,
	)
	if err != nil {
		t.Fatalf(
			"query persisted waiting approval incident error = %v",
			err,
		)
	}

	if persistedState != string(incident.StateWaitingApproval) {
		t.Fatalf(
			"persisted State = %q; want %q",
			persistedState,
			incident.StateWaitingApproval,
		)
	}
	if persistedVersion != int64(waiting.Version) {
		t.Fatalf(
			"persisted Version = %d; want %d",
			persistedVersion,
			waiting.Version,
		)
	}
	if persistedPlanHash != planHash {
		t.Fatalf(
			"persisted PlanHash = %q; want %q",
			persistedPlanHash,
			planHash,
		)
	}
	if persistedTargetUID != targetUID {
		t.Fatalf(
			"persisted TargetUID = %q; want %q",
			persistedTargetUID,
			targetUID,
		)
	}
	if !resolvedAtIsNull {
		t.Fatal(
			"persisted resolved_at is not NULL while approval is pending",
		)
	}
	if auditFromState != string(incident.StateDiagnosed) {
		t.Fatalf(
			"audit from_state = %q; want %q",
			auditFromState,
			incident.StateDiagnosed,
		)
	}
	if auditToState != string(incident.StateWaitingApproval) {
		t.Fatalf(
			"audit to_state = %q; want %q",
			auditToState,
			incident.StateWaitingApproval,
		)
	}
	if auditActor != "sre-agent-v2" {
		t.Fatalf(
			"audit actor = %q; want %q",
			auditActor,
			"sre-agent-v2",
		)
	}
	if auditReasonCode != "APPROVAL_REQUIRED" {
		t.Fatalf(
			"audit reason_code = %q; want %q",
			auditReasonCode,
			"APPROVAL_REQUIRED",
		)
	}

	restartedRegistry := NewRegistry(pool)

	recovered, recoveredCreated, err := restartedRegistry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf(
			"Observe() after adapter restart error = %v",
			err,
		)
	}
	if recoveredCreated {
		t.Fatal(
			"Observe() after adapter restart created a duplicate Incident",
		)
	}
	if recovered.ID != waiting.ID {
		t.Fatalf(
			"recovered Incident ID = %q; want %q",
			recovered.ID,
			waiting.ID,
		)
	}
	if recovered.State != incident.StateWaitingApproval {
		t.Fatalf(
			"recovered State = %q; want %q",
			recovered.State,
			incident.StateWaitingApproval,
		)
	}
	if recovered.Version != waiting.Version {
		t.Fatalf(
			"recovered Version = %d; want %d",
			recovered.Version,
			waiting.Version,
		)
	}
	if recovered.ApprovalBinding != waiting.ApprovalBinding {
		t.Fatalf(
			"recovered ApprovalBinding = %#v; want %#v",
			recovered.ApprovalBinding,
			waiting.ApprovalBinding,
		)
	}
}

func TestRegistryTransitionWaitingApprovalRollsBackWhenAuditInsertFails(
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
			Name:      "crash-app-waiting-approval-rollback",
			UID:       "pod-uid-waiting-approval-rollback",
		},
	}

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              incident.StateDiagnosed,
			Actor:           "sre-agent-v2",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf("Transition(DIAGNOSED) error = %v; want nil", err)
	}

	_, err = pool.Exec(
		ctx,
		`
ALTER TABLE incident_transitions
ADD CONSTRAINT incident_transitions_reject_waiting_approval_test
CHECK (to_state <> 'WAITING_APPROVAL')
`,
	)
	if err != nil {
		t.Fatalf("install failing audit constraint: %v", err)
	}

	_, err = registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              incident.StateWaitingApproval,
			Actor:           "sre-agent-v2",
			ReasonCode:      "APPROVAL_REQUIRED",
			ApprovalBinding: incident.ApprovalBinding{
				PlanHash:  "f8c1e13645e14ce2754097dc585f649823f6592763ba03a2f7d4fa3ed82148bc",
				TargetUID: observation.Target.UID,
			},
		},
	)
	if err == nil {
		t.Fatal(
			"Transition(WAITING_APPROVAL) error = nil; want audit failure",
		)
	}

	var (
		persistedState    string
		persistedVersion  int64
		planHashIsNull    bool
		targetUIDIsNull   bool
		waitingAuditCount int
	)

	err = pool.QueryRow(
		ctx,
		`
SELECT
    state,
    version,
    approval_plan_hash IS NULL,
    approval_target_uid IS NULL
FROM incidents
WHERE id = $1
`,
		diagnosed.ID,
	).Scan(
		&persistedState,
		&persistedVersion,
		&planHashIsNull,
		&targetUIDIsNull,
	)
	if err != nil {
		t.Fatalf("query rolled back Incident error = %v", err)
	}

	if persistedState != string(incident.StateDiagnosed) {
		t.Fatalf(
			"persisted State = %q; want %q after rollback",
			persistedState,
			incident.StateDiagnosed,
		)
	}
	if persistedVersion != int64(diagnosed.Version) {
		t.Fatalf(
			"persisted Version = %d; want %d after rollback",
			persistedVersion,
			diagnosed.Version,
		)
	}
	if !planHashIsNull {
		t.Fatal(
			"approval_plan_hash is not NULL after transaction rollback",
		)
	}
	if !targetUIDIsNull {
		t.Fatal(
			"approval_target_uid is not NULL after transaction rollback",
		)
	}

	err = pool.QueryRow(
		ctx,
		`
SELECT count(*)
FROM incident_transitions
WHERE incident_id = $1
  AND to_state = 'WAITING_APPROVAL'
`,
		diagnosed.ID,
	).Scan(&waitingAuditCount)
	if err != nil {
		t.Fatalf("count waiting approval audit records: %v", err)
	}
	if waitingAuditCount != 0 {
		t.Fatalf(
			"waiting approval audit count = %d; want 0",
			waitingAuditCount,
		)
	}

	recovered, recoveredCreated, err := NewRegistry(pool).Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf("Observe() after rollback error = %v", err)
	}
	if recoveredCreated {
		t.Fatal(
			"Observe() after rollback created a duplicate Incident",
		)
	}
	if recovered.State != incident.StateDiagnosed {
		t.Fatalf(
			"recovered State = %q; want %q",
			recovered.State,
			incident.StateDiagnosed,
		)
	}
	if recovered.Version != diagnosed.Version {
		t.Fatalf(
			"recovered Version = %d; want %d",
			recovered.Version,
			diagnosed.Version,
		)
	}
	if recovered.ApprovalBinding != (incident.ApprovalBinding{}) {
		t.Fatalf(
			"recovered ApprovalBinding = %#v; want empty",
			recovered.ApprovalBinding,
		)
	}
}

func TestRegistryTransitionPersistsVerifyingAcrossAdapterRestart(
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
			Name:      "crash-app-verifying-restart",
			UID:       "pod-uid-verifying-restart",
		},
	}

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              incident.StateDiagnosed,
			Actor:           "sre-agent",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v; want nil",
			err,
		)
	}

	verifying, err := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              incident.StateVerifying,
			Actor:           "sre-agent",
			ReasonCode:      "VERIFICATION_STARTED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DIAGNOSED -> VERIFYING) error = %v; want nil",
			err,
		)
	}
	if verifying.State != incident.StateVerifying {
		t.Fatalf(
			"verifying State = %q; want %q",
			verifying.State,
			incident.StateVerifying,
		)
	}
	if verifying.Version != diagnosed.Version+1 {
		t.Fatalf(
			"verifying Version = %d; want %d",
			verifying.Version,
			diagnosed.Version+1,
		)
	}
	if verifying.ApprovalBinding != (incident.ApprovalBinding{}) {
		t.Fatalf(
			"verifying ApprovalBinding = %#v; want empty",
			verifying.ApprovalBinding,
		)
	}

	restartedRegistry := NewRegistry(pool)

	reloaded, created, err := restartedRegistry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf(
			"Observe() after adapter restart error = %v; want nil",
			err,
		)
	}
	if created {
		t.Fatal(
			"Observe() after adapter restart created = true; " +
				"want existing active VERIFYING incident",
		)
	}
	if reloaded.ID != verifying.ID {
		t.Fatalf(
			"reloaded ID = %q; want %q",
			reloaded.ID,
			verifying.ID,
		)
	}
	if reloaded.State != incident.StateVerifying {
		t.Fatalf(
			"reloaded State = %q; want %q",
			reloaded.State,
			incident.StateVerifying,
		)
	}
	if reloaded.Version != verifying.Version {
		t.Fatalf(
			"reloaded Version = %d; want %d",
			reloaded.Version,
			verifying.Version,
		)
	}
	if reloaded.ApprovalBinding != (incident.ApprovalBinding{}) {
		t.Fatalf(
			"reloaded ApprovalBinding = %#v; want empty",
			reloaded.ApprovalBinding,
		)
	}
}

func TestRegistryTransitionFromWaitingApprovalToVerifyingPersistsBindingAcrossAdapterRestart(
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
			Name:      "crash-app-approved-verification",
			UID:       "pod-uid-approved-verification",
		},
	}

	detected, created, err := registry.Observe(ctx, observation)
	if err != nil {
		t.Fatalf("Observe() error = %v; want nil", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      detected.ID,
			ExpectedVersion: detected.Version,
			To:              incident.StateDiagnosed,
			Actor:           "sre-agent",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v; want nil",
			err,
		)
	}

	binding := incident.ApprovalBinding{
		PlanHash:  "sha256:postgres-approved-verification-plan",
		TargetUID: observation.Target.UID,
	}

	waiting, err := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			To:              incident.StateWaitingApproval,
			Actor:           "sre-agent",
			ReasonCode:      "APPROVAL_REQUIRED",
			ApprovalBinding: binding,
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DIAGNOSED -> WAITING_APPROVAL) error = %v; want nil",
			err,
		)
	}
	if waiting.ApprovalBinding != binding {
		t.Fatalf(
			"waiting ApprovalBinding = %#v; want %#v",
			waiting.ApprovalBinding,
			binding,
		)
	}

	verifying, err := registry.Transition(
		ctx,
		incident.TransitionCommand{
			IncidentID:      waiting.ID,
			ExpectedVersion: waiting.Version,
			To:              incident.StateVerifying,
			Actor:           "sre-agent",
			ReasonCode:      "APPROVAL_VALIDATED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(WAITING_APPROVAL -> VERIFYING) error = %v; want nil",
			err,
		)
	}
	if verifying.State != incident.StateVerifying {
		t.Fatalf(
			"verifying State = %q; want %q",
			verifying.State,
			incident.StateVerifying,
		)
	}
	if verifying.Version != waiting.Version+1 {
		t.Fatalf(
			"verifying Version = %d; want %d",
			verifying.Version,
			waiting.Version+1,
		)
	}
	if verifying.ApprovalBinding != binding {
		t.Fatalf(
			"verifying ApprovalBinding = %#v; want retained %#v",
			verifying.ApprovalBinding,
			binding,
		)
	}

	restartedRegistry := NewRegistry(pool)

	reloaded, created, err := restartedRegistry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf(
			"Observe() after adapter restart error = %v; want nil",
			err,
		)
	}
	if created {
		t.Fatal(
			"Observe() after adapter restart created = true; " +
				"want existing active VERIFYING incident",
		)
	}
	if reloaded.ID != verifying.ID {
		t.Fatalf(
			"reloaded ID = %q; want %q",
			reloaded.ID,
			verifying.ID,
		)
	}
	if reloaded.State != incident.StateVerifying {
		t.Fatalf(
			"reloaded State = %q; want %q",
			reloaded.State,
			incident.StateVerifying,
		)
	}
	if reloaded.Version != verifying.Version {
		t.Fatalf(
			"reloaded Version = %d; want %d",
			reloaded.Version,
			verifying.Version,
		)
	}
	if reloaded.ApprovalBinding != binding {
		t.Fatalf(
			"reloaded ApprovalBinding = %#v; want %#v",
			reloaded.ApprovalBinding,
			binding,
		)
	}
}
