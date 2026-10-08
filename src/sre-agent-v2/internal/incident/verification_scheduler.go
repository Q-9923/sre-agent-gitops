package incident

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	remediationdomain "sre-agent/internal/remediation"
)

var (
	ErrInvalidPendingVerificationClaim = errors.New(
		"invalid pending verification claim",
	)
	ErrPendingVerificationSchedulerUnavailable = errors.New(
		"pending verification scheduler is unavailable",
	)
)

type ClaimPendingVerificationCommand struct {
	HolderID      string
	Now           time.Time
	LeaseDuration time.Duration
}

func (command ClaimPendingVerificationCommand) Validate() error {
	switch {
	case strings.TrimSpace(command.HolderID) == "":
		return fmt.Errorf(
			"%w: holder ID is required",
			ErrInvalidPendingVerificationClaim,
		)

	case command.Now.IsZero():
		return fmt.Errorf(
			"%w: current time is required",
			ErrInvalidPendingVerificationClaim,
		)

	case command.LeaseDuration <= 0:
		return fmt.Errorf(
			"%w: lease duration must be greater than zero",
			ErrInvalidPendingVerificationClaim,
		)

	default:
		return nil
	}
}

type PendingVerificationClaim struct {
	Verification remediationdomain.Verification
	Claim        Claim
}

type pendingVerificationSource interface {
	ListPending(
		context.Context,
	) ([]remediationdomain.Verification, error)
}

func (registry *Registry) ClaimPendingVerification(
	ctx context.Context,
	command ClaimPendingVerificationCommand,
) (PendingVerificationClaim, bool, error) {
	if registry == nil {
		return PendingVerificationClaim{},
			false,
			ErrPendingVerificationSchedulerUnavailable
	}

	if ctx == nil {
		return PendingVerificationClaim{},
			false,
			fmt.Errorf(
				"%w: context is required",
				ErrInvalidPendingVerificationClaim,
			)
	}

	if err := command.Validate(); err != nil {
		return PendingVerificationClaim{}, false, err
	}

	if err := ctx.Err(); err != nil {
		return PendingVerificationClaim{}, false, err
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return PendingVerificationClaim{}, false, err
	}

	source, available :=
		registry.verificationLifecycleStore.(pendingVerificationSource)
	if !available {
		return PendingVerificationClaim{},
			false,
			ErrPendingVerificationSchedulerUnavailable
	}

	pending, err := source.ListPending(ctx)
	if err != nil {
		return PendingVerificationClaim{},
			false,
			fmt.Errorf(
				"list pending verifications: %w",
				err,
			)
	}

	if err := ctx.Err(); err != nil {
		return PendingVerificationClaim{}, false, err
	}

	for _, verification := range pending {
		if verification.Status !=
			remediationdomain.VerificationStatusPending {
			continue
		}

		current, exists :=
			registry.incidentsByID[verification.ActionKey.IncidentID]
		if !exists ||
			current.State != StateVerifying {
			continue
		}

		currentClaim, claimed :=
			registry.claimsByIncidentID[current.ID]
		if claimed &&
			command.Now.Before(currentClaim.ExpiresAt) {
			continue
		}

		current.Version++

		claim := Claim{
			Incident: current,
			HolderID: command.HolderID,
			ExpiresAt: command.Now.Add(
				command.LeaseDuration,
			),
		}

		registry.incidentsByID[current.ID] = current
		registry.claimsByIncidentID[current.ID] = claim

		registry.claimHistoryByIncidentID[current.ID] =
			append(
				registry.claimHistoryByIncidentID[current.ID],
				ClaimAuditEvent{
					IncidentID:      current.ID,
					IncidentVersion: current.Version,
					HolderID:        claim.HolderID,
					AcquiredAt:      command.Now,
					ExpiresAt:       claim.ExpiresAt,
				},
			)

		return PendingVerificationClaim{
			Verification: verification,
			Claim:        claim,
		}, true, nil
	}

	return PendingVerificationClaim{}, false, nil
}
