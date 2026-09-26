//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"errors"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"sre-agent/internal/incident"
)

const incidentRegistryPostgresTestImage = "postgres:17.11-bookworm@sha256:639ab7ceb90e13123085b741fb31ef493fba25463002f6da665352e7b534b652"

func TestOpenIncidentRegistryReturnsUsablePostgresRegistry(
	t *testing.T,
) {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Minute,
	)
	t.Cleanup(cancel)

	container, err := tcpostgres.Run(
		ctx,
		incidentRegistryPostgresTestImage,
		tcpostgres.WithDatabase("sre_agent_test"),
		tcpostgres.WithUsername("sre_agent"),
		tcpostgres.WithPassword("integration-test-only"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf(
			"start PostgreSQL container: %v",
			err,
		)
	}
	testcontainers.CleanupContainer(t, container)

	connectionString, err := container.ConnectionString(
		ctx,
		"sslmode=disable",
	)
	if err != nil {
		t.Fatalf(
			"get PostgreSQL connection string: %v",
			err,
		)
	}

	config := agentConfig{
		IncidentStoreBackend:          "postgres",
		IncidentStorePostgresDSN:      connectionString,
		IncidentStoreConnectTimeout:   10 * time.Second,
		IncidentStoreMigrationTimeout: 30 * time.Second,
	}

	handle, err := openIncidentRegistry(ctx, config)
	if err != nil {
		t.Fatalf(
			"openIncidentRegistry() error = %v; want nil",
			err,
		)
	}
	t.Cleanup(handle.Close)

	observation := incident.Observation{
		Source:    "prometheus",
		Cluster:   "dev",
		AlertName: "KubePodCrashLooping",
		Target: incident.Target{
			Kind:      "Pod",
			Namespace: "sre-agent-lab",
			Name:      "crash-app",
			UID:       "pod-uid-assembly",
		},
	}

	observed, created, err := handle.Registry.Observe(
		ctx,
		observation,
	)
	if err != nil {
		t.Fatalf(
			"Registry.Observe() error = %v; want nil",
			err,
		)
	}
	if !created {
		t.Fatal(
			"Registry.Observe() created = false; want true",
		)
	}
	if observed.ID == "" {
		t.Fatal(
			"Registry.Observe() returned an empty incident ID",
		)
	}
}
func TestOpenIncidentRegistryAppliesMigrationTimeout(
	t *testing.T,
) {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Minute,
	)
	t.Cleanup(cancel)

	container, err := tcpostgres.Run(
		ctx,
		incidentRegistryPostgresTestImage,
		tcpostgres.WithDatabase("sre_agent_test"),
		tcpostgres.WithUsername("sre_agent"),
		tcpostgres.WithPassword("integration-test-only"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf(
			"start PostgreSQL container: %v",
			err,
		)
	}
	testcontainers.CleanupContainer(t, container)

	connectionString, err := container.ConnectionString(
		ctx,
		"sslmode=disable",
	)
	if err != nil {
		t.Fatalf(
			"get PostgreSQL connection string: %v",
			err,
		)
	}

	handle, err := openIncidentRegistry(
		ctx,
		agentConfig{
			IncidentStoreBackend:          "postgres",
			IncidentStorePostgresDSN:      connectionString,
			IncidentStoreConnectTimeout:   10 * time.Second,
			IncidentStoreMigrationTimeout: time.Nanosecond,
		},
	)
	if err == nil {
		handle.Close()
		t.Fatal(
			"openIncidentRegistry() error = nil; " +
				"want a migration timeout",
		)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf(
			"openIncidentRegistry() error = %v; "+
				"want context.DeadlineExceeded",
			err,
		)
	}
	if ctx.Err() != nil {
		t.Fatal(
			"openIncidentRegistry() exhausted the parent context; " +
				"want the independent migration timeout",
		)
	}
}
