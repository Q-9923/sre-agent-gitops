package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/tern/v2/migrate"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func ApplyMigrations(
	ctx context.Context,
	pool *pgxpool.Pool,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	connection, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf(
			"acquire PostgreSQL migration connection: %w",
			err,
		)
	}
	defer connection.Release()

	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf(
			"open embedded incident migrations: %w",
			err,
		)
	}

	migrator, err := migrate.NewMigrator(
		ctx,
		connection.Conn(),
		"incident_schema_version",
	)
	if err != nil {
		return fmt.Errorf(
			"create incident migrator: %w",
			err,
		)
	}

	if err := migrator.LoadMigrations(migrations); err != nil {
		return fmt.Errorf(
			"load incident migrations: %w",
			err,
		)
	}

	if err := migrator.Migrate(ctx); err != nil {
		return fmt.Errorf(
			"apply incident migrations: %w",
			err,
		)
	}

	return nil
}
