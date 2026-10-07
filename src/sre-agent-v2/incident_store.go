package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "sre-agent/internal/adapters/postgres"
	approvaldomain "sre-agent/internal/approval"
	"sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

type incidentRegistry interface {
	Claim(
		context.Context,
		incident.ClaimCommand,
	) (incident.Claim, error)
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
	Registry              incidentRegistry
	Approvals             approvaldomain.Store
	Plans                 approvaldomain.PlanStore
	ActionAttempts        remediationdomain.ActionAttemptStore
	Verifications         remediationdomain.VerificationStore
	VerificationLifecycle incidentVerificationLifecycle
	close                 func()
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
		verificationStore :=
			remediationdomain.NewMemoryVerificationStore()

		registry :=
			incident.NewRegistryWithVerificationLifecycleStore(
				verificationStore,
			)

		return incidentRegistryHandle{
			Registry:  registry,
			Approvals: approvaldomain.NewMemoryStore(),
			Plans:     approvaldomain.NewMemoryPlanStore(),
			ActionAttempts: remediationdomain.
				NewMemoryActionAttemptStore(),
			Verifications:         verificationStore,
			VerificationLifecycle: registry,
			close:                 func() {},
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

		verificationStore :=
			postgresadapter.NewVerificationStore(pool)

		registry := postgresadapter.NewRegistry(pool)

		return incidentRegistryHandle{
			Registry: registry,
			Approvals: postgresadapter.NewApprovalStore(
				pool,
			),
			Plans: postgresadapter.NewPlanStore(pool),
			ActionAttempts: postgresadapter.
				NewActionAttemptStore(pool),

			Verifications:         verificationStore,
			VerificationLifecycle: registry,
			close:                 pool.Close,
		}, nil

	default:
		return incidentRegistryHandle{}, fmt.Errorf(
			"open incident registry: backend %q is not implemented",
			config.IncidentStoreBackend,
		)
	}
}
