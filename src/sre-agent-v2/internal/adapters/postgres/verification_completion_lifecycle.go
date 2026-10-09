package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	incidentdomain "sre-agent/internal/incident"
)

func (registry *Registry) CompleteFencedVerification(
	ctx context.Context,
	command incidentdomain.CompleteFencedVerificationCommand,
) (incidentdomain.FencedVerificationCompletionResult, error) {
	if registry == nil || registry.pool == nil {
		return incidentdomain.
				FencedVerificationCompletionResult{}, fmt.Errorf(
				"%w: PostgreSQL registry is not configured",
				incidentdomain.ErrVerificationLifecycleUnavailable,
			)
	}

	if ctx == nil {
		return incidentdomain.
				FencedVerificationCompletionResult{}, fmt.Errorf(
				"%w: context is required",
				incidentdomain.ErrInvalidFencedVerification,
			)
	}

	if err := command.Validate(); err != nil {
		return incidentdomain.
			FencedVerificationCompletionResult{}, err
	}

	if err := ctx.Err(); err != nil {
		return incidentdomain.
			FencedVerificationCompletionResult{}, err
	}

	transaction, err := registry.pool.BeginTx(
		ctx,
		pgx.TxOptions{},
	)
	if err != nil {
		return incidentdomain.
				FencedVerificationCompletionResult{}, fmt.Errorf(
				"begin PostgreSQL fenced Verification completion transaction: %w",
				err,
			)
	}
	defer func() {
		_ = transaction.Rollback(context.Background())
	}()

	current, err := lockIncidentForFencedVerification(
		ctx,
		transaction,
		verificationFenceFromComplete(command),
	)
	if err != nil {
		return incidentdomain.
			FencedVerificationCompletionResult{}, err
	}

	if current.State != incidentdomain.StateVerifying {
		return incidentdomain.
				FencedVerificationCompletionResult{}, fmt.Errorf(
				"%w: incident %q is in state %q",
				incidentdomain.ErrInvalidVerificationIncidentState,
				command.IncidentID,
				current.State,
			)
	}

	verification, err := completeVerificationInTransaction(
		ctx,
		transaction,
		command.Verification,
	)
	if err != nil {
		return incidentdomain.
				FencedVerificationCompletionResult{}, fmt.Errorf(
				"complete PostgreSQL fenced Verification for incident %q: %w",
				command.IncidentID,
				err,
			)
	}

	if err := transaction.Commit(ctx); err != nil {
		return incidentdomain.
				FencedVerificationCompletionResult{}, fmt.Errorf(
				"commit PostgreSQL fenced Verification completion: %w",
				err,
			)
	}

	return incidentdomain.FencedVerificationCompletionResult{
		Incident:     current,
		Verification: verification,
	}, nil
}
