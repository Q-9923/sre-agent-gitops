package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "sre-agent/internal/adapters/postgres"
	"sre-agent/internal/incident"
)

type incidentRegistry interface {
	Observe(
		context.Context,
		incident.Observation,
	) (incident.Incident, bool, error)
	Transition(
		context.Context,
		incident.TransitionCommand,
	) (incident.Incident, error)
}

type incidentRegistryHandle struct {
	Registry incidentRegistry
	close    func()
}

func (handle incidentRegistryHandle) Close() {
	if handle.close != nil {
		handle.close()
	}
}

func openIncidentRegistry(
	ctx context.Context,
	config agentConfig,
) (incidentRegistryHandle, error) {
	if err := ctx.Err(); err != nil {
		return incidentRegistryHandle{}, err
	}

	switch config.IncidentStoreBackend {
	case "memory":
		return incidentRegistryHandle{
			Registry: incident.NewMemoryRegistry(),
			close:    func() {},
		}, nil

	case "postgres":
		poolConfig, err := pgxpool.ParseConfig(
			config.IncidentStorePostgresDSN,
		)
		if err != nil {
			return incidentRegistryHandle{}, errors.New(
				"parse PostgreSQL incident configuration",
			)
		}

		connectCtx, cancelConnect := context.WithTimeout(
			ctx,
			config.IncidentStoreConnectTimeout,
		)

		pool, err := pgxpool.NewWithConfig(
			connectCtx,
			poolConfig,
		)
		if err != nil {
			cancelConnect()
			return incidentRegistryHandle{}, fmt.Errorf(
				"create PostgreSQL incident pool: %w",
				err,
			)
		}

		if err := pool.Ping(connectCtx); err != nil {
			cancelConnect()
			pool.Close()
			return incidentRegistryHandle{}, fmt.Errorf(
				"ping PostgreSQL incident store: %w",
				err,
			)
		}
		cancelConnect()

		migrationCtx, cancelMigration := context.WithTimeout(
			ctx,
			config.IncidentStoreMigrationTimeout,
		)
		migrationErr := postgresadapter.ApplyMigrations(
			migrationCtx,
			pool,
		)
		cancelMigration()

		if migrationErr != nil {
			pool.Close()
			return incidentRegistryHandle{}, fmt.Errorf(
				"apply PostgreSQL incident migrations: %w",
				migrationErr,
			)
		}

		return incidentRegistryHandle{
			Registry: postgresadapter.NewRegistry(pool),
			close:    pool.Close,
		}, nil

	default:
		return incidentRegistryHandle{}, fmt.Errorf(
			"open incident registry: backend %q is not implemented",
			config.IncidentStoreBackend,
		)
	}
}
