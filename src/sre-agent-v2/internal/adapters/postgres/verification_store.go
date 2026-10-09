package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	remediationdomain "sre-agent/internal/remediation"
)

type VerificationStore struct {
	pool *pgxpool.Pool
}

var _ remediationdomain.VerificationStore = (*VerificationStore)(nil)

func NewVerificationStore(pool *pgxpool.Pool) *VerificationStore {
	return &VerificationStore{
		pool: pool,
	}
}

func (store *VerificationStore) Begin(
	ctx context.Context,
	command remediationdomain.BeginVerificationCommand,
) (remediationdomain.Verification, bool, error) {
	return beginVerificationWithQuerier(
		ctx,
		store.pool,
		command,
	)
}

func (store *VerificationStore) Complete(
	ctx context.Context,
	command remediationdomain.CompleteVerificationCommand,
) (remediationdomain.Verification, error) {
	if err := ctx.Err(); err != nil {
		return remediationdomain.Verification{}, err
	}

	if err := validateVerificationCompletionCommand(command); err != nil {
		return remediationdomain.Verification{}, err
	}

	transaction, err := store.pool.BeginTx(
		ctx,
		pgx.TxOptions{},
	)
	if err != nil {
		return remediationdomain.Verification{}, fmt.Errorf(
			"begin PostgreSQL verification completion transaction: %w",
			err,
		)
	}
	defer func() {
		_ = transaction.Rollback(ctx)
	}()

	verification, err := completeVerificationInTransaction(
		ctx,
		transaction,
		command,
	)
	if err != nil {
		return remediationdomain.Verification{}, err
	}

	if err := transaction.Commit(ctx); err != nil {
		return remediationdomain.Verification{}, fmt.Errorf(
			"commit PostgreSQL verification completion: %w",
			err,
		)
	}

	return verification, nil
}

func completeVerificationInTransaction(
	ctx context.Context,
	transaction pgx.Tx,
	command remediationdomain.CompleteVerificationCommand,
) (remediationdomain.Verification, error) {
	current, err := lookupVerificationByActionKeyForUpdate(
		ctx,
		transaction,
		command.ActionKey,
	)
	if err != nil {
		return remediationdomain.Verification{}, err
	}

	if verificationCompletionMatches(current, command) {
		return current, nil
	}

	if current.Version != command.ExpectedVersion {
		return remediationdomain.Verification{}, fmt.Errorf(
			"%w: current=%d expected=%d",
			remediationdomain.ErrVerificationVersionConflict,
			current.Version,
			command.ExpectedVersion,
		)
	}

	if current.Status !=
		remediationdomain.VerificationStatusPending {
		return remediationdomain.Verification{}, fmt.Errorf(
			"%w: current=%q target=%q",
			remediationdomain.ErrInvalidVerificationTransition,
			current.Status,
			command.To,
		)
	}

	if command.FinishedAt.Before(current.StartedAt) {
		return remediationdomain.Verification{}, fmt.Errorf(
			"%w: finished_at precedes started_at",
			remediationdomain.ErrInvalidVerification,
		)
	}

	result, err := transaction.Exec(
		ctx,
		`
UPDATE verifications
SET
	status = $1,
	version = version + 1,
	finished_at = $2,
	evidence_code = $3
WHERE
	id = $4
	AND version = $5
	AND status = $6
`,
		command.To,
		command.FinishedAt,
		command.EvidenceCode,
		current.ID,
		command.ExpectedVersion,
		remediationdomain.VerificationStatusPending,
	)
	if err != nil {
		return remediationdomain.Verification{}, fmt.Errorf(
			"complete PostgreSQL verification: %w",
			err,
		)
	}

	if result.RowsAffected() != 1 {
		return remediationdomain.Verification{}, fmt.Errorf(
			"%w: verification %q changed concurrently",
			remediationdomain.ErrVerificationVersionConflict,
			current.ID,
		)
	}

	finishedAt := command.FinishedAt

	current.Status = command.To
	current.Version++
	current.FinishedAt = &finishedAt
	current.EvidenceCode = command.EvidenceCode

	return current, nil
}

func (store *VerificationStore) lookupByActionAttemptID(
	ctx context.Context,
	actionAttemptID string,
) (remediationdomain.Verification, error) {
	return lookupVerificationByActionAttemptIDWithQuerier(
		ctx,
		store.pool,
		actionAttemptID,
	)
}

func lookupVerificationByActionKeyForUpdate(
	ctx context.Context,
	tx pgx.Tx,
	actionKey remediationdomain.ActionKey,
) (remediationdomain.Verification, error) {
	row := tx.QueryRow(
		ctx,
		`
			SELECT
				verification.id,
				verification.action_attempt_id,
				action_attempt.incident_id,
				action_attempt.plan_hash,
				action_attempt.target_uid,
				verification.subject_cluster,
				verification.subject_namespace,
				verification.subject_kind,
				verification.subject_name,
				verification.subject_uid,
				verification.status,
				verification.version,
				verification.started_at,
				verification.finished_at,
				COALESCE(verification.evidence_code, '')
			FROM verifications AS verification
			INNER JOIN action_attempts AS action_attempt
				ON action_attempt.id = verification.action_attempt_id
			WHERE
				action_attempt.incident_id = $1
				AND action_attempt.plan_hash = $2
				AND action_attempt.target_uid = $3
			FOR UPDATE OF verification
		`,
		actionKey.IncidentID,
		actionKey.PlanHash,
		actionKey.TargetUID,
	)

	verification, err := scanVerification(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return remediationdomain.Verification{}, fmt.Errorf(
			"%w: incident=%q plan_hash=%q target_uid=%q",
			remediationdomain.ErrVerificationNotFound,
			actionKey.IncidentID,
			actionKey.PlanHash,
			actionKey.TargetUID,
		)
	}
	if err != nil {
		return remediationdomain.Verification{}, fmt.Errorf(
			"lock PostgreSQL verification: %w",
			err,
		)
	}

	return verification, nil
}

type verificationRowScanner interface {
	Scan(dest ...any) error
}

func scanVerification(
	row verificationRowScanner,
) (remediationdomain.Verification, error) {
	var verification remediationdomain.Verification
	var status string
	var startedAt pgtype.Timestamptz
	var finishedAt pgtype.Timestamptz

	err := row.Scan(
		&verification.ID,
		&verification.ActionAttemptID,
		&verification.ActionKey.IncidentID,
		&verification.ActionKey.PlanHash,
		&verification.ActionKey.TargetUID,
		&verification.Subject.Cluster,
		&verification.Subject.Namespace,
		&verification.Subject.Kind,
		&verification.Subject.Name,
		&verification.Subject.UID,
		&status,
		&verification.Version,
		&startedAt,
		&finishedAt,
		&verification.EvidenceCode,
	)
	if err != nil {
		return remediationdomain.Verification{}, err
	}

	verification.Status = remediationdomain.VerificationStatus(status)
	verification.StartedAt = startedAt.Time.UTC()

	if finishedAt.Valid {
		value := finishedAt.Time.UTC()
		verification.FinishedAt = &value
	}

	return verification, nil
}
func validateVerificationCompletionCommand(
	command remediationdomain.CompleteVerificationCommand,
) error {
	if strings.TrimSpace(command.ActionKey.IncidentID) == "" ||
		strings.TrimSpace(command.ActionKey.PlanHash) == "" ||
		strings.TrimSpace(command.ActionKey.TargetUID) == "" ||
		command.ExpectedVersion <= 0 ||
		command.FinishedAt.IsZero() ||
		strings.TrimSpace(command.EvidenceCode) == "" ||
		!isTerminalVerificationStatus(command.To) {
		return remediationdomain.ErrInvalidVerification
	}

	return nil
}

func isTerminalVerificationStatus(
	status remediationdomain.VerificationStatus,
) bool {
	switch status {
	case remediationdomain.VerificationStatusRecovered,
		remediationdomain.VerificationStatusNotRecovered,
		remediationdomain.VerificationStatusInconclusive:
		return true
	default:
		return false
	}
}

func verificationCompletionMatches(
	current remediationdomain.Verification,
	command remediationdomain.CompleteVerificationCommand,
) bool {
	return current.Status == command.To &&
		current.Version == command.ExpectedVersion+1 &&
		current.FinishedAt != nil &&
		current.FinishedAt.Equal(command.FinishedAt) &&
		current.EvidenceCode == command.EvidenceCode
}
