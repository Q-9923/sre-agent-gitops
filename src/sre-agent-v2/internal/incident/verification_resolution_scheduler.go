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
	ErrInvalidRecoveredVerificationResolutionClaim = errors.New(
		"invalid recovered verification resolution claim",
	)
	ErrRecoveredVerificationResolutionSchedulerUnavailable = errors.New(
		"recovered verification resolution scheduler is unavailable",
	)
)

type ClaimRecoveredVerificationResolutionCommand struct {
	HolderID      string
	Now           time.Time
	LeaseDuration time.Duration
}

func (
	command ClaimRecoveredVerificationResolutionCommand,
) Validate() error {
	switch {
	case strings.TrimSpace(command.HolderID) == "":
		return fmt.Errorf(
			"%w: holder ID is required",
			ErrInvalidRecoveredVerificationResolutionClaim,
		)

	case command.Now.IsZero():
		return fmt.Errorf(
			"%w: current time is required",
			ErrInvalidRecoveredVerificationResolutionClaim,
		)

	case command.LeaseDuration <= 0:
		return fmt.Errorf(
			"%w: lease duration must be greater than zero",
			ErrInvalidRecoveredVerificationResolutionClaim,
		)

	default:
		return nil
	}
}

type RecoveredVerificationResolutionClaim struct {
	Verification remediationdomain.Verification
	Claim        Claim
}

type recoveredVerificationResolutionSource interface {
	ListRecovered(
		context.Context,
	) ([]remediationdomain.Verification, error)
}

func (registry *Registry) ClaimRecoveredVerificationResolution(
	ctx context.Context,
	command ClaimRecoveredVerificationResolutionCommand,
) (RecoveredVerificationResolutionClaim, bool, error) {
	if registry == nil {
		return RecoveredVerificationResolutionClaim{},
			false,
			ErrRecoveredVerificationResolutionSchedulerUnavailable
	}

	if ctx == nil {
		return RecoveredVerificationResolutionClaim{},
			false,
			fmt.Errorf(
				"%w: context is required",
				ErrInvalidRecoveredVerificationResolutionClaim,
			)
	}

	if err := command.Validate(); err != nil {
		return RecoveredVerificationResolutionClaim{},
			false,
			err
	}

	if err := ctx.Err(); err != nil {
		return RecoveredVerificationResolutionClaim{},
			false,
			err
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return RecoveredVerificationResolutionClaim{},
			false,
			err
	}

	source, available :=
		registry.verificationLifecycleStore.(recoveredVerificationResolutionSource)
	if !available {
		return RecoveredVerificationResolutionClaim{},
			false,
			ErrRecoveredVerificationResolutionSchedulerUnavailable
	}

	recovered, err := source.ListRecovered(ctx)
	if err != nil {
		return RecoveredVerificationResolutionClaim{},
			false,
			fmt.Errorf(
				"list recovered verifications: %w",
				err,
			)
	}

	if err := ctx.Err(); err != nil {
		return RecoveredVerificationResolutionClaim{},
			false,
			err
	}

	for _, verification := range recovered {
		if verification.Status !=
			remediationdomain.VerificationStatusRecovered {
			continue
		}

		current, exists :=
			registry.incidentsByID[verification.ActionKey.IncidentID]
		if !exists ||
			current.State != StateVerifying ||
			current.ResolutionVerificationID != "" {
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

		return RecoveredVerificationResolutionClaim{
			Verification: verification,
			Claim:        claim,
		}, true, nil
	}

	return RecoveredVerificationResolutionClaim{}, false, nil
}
