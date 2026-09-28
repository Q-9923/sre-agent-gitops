package approval

import (
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidApproval   = errors.New("invalid approval")
	ErrIncidentMismatch  = errors.New("approval incident mismatch")
	ErrTargetUIDMismatch = errors.New("approval target UID mismatch")
	ErrPlanHashMismatch  = errors.New("approval plan hash mismatch")
	ErrApprovalExpired   = errors.New("approval expired")
	ErrApprovalNotYetValid = errors.New("approval not yet valid")
)

type Plan struct {
	IncidentID string
	Hash       string
	TargetUID  string
}

type Approval struct {
	IncidentID string
	PlanHash   string
	TargetUID  string
	ApprovedBy string
	ApprovedAt time.Time
	ExpiresAt  time.Time
}

func Validate(granted Approval, plan Plan, now time.Time) error {
	if strings.TrimSpace(granted.ApprovedBy) == "" {
		return ErrInvalidApproval
	}
	if !granted.ExpiresAt.After(granted.ApprovedAt) {
		return ErrInvalidApproval
	}

	if now.Before(granted.ApprovedAt) {
		return ErrApprovalNotYetValid
	}

	if granted.IncidentID != plan.IncidentID {
		return ErrIncidentMismatch
	}

	if granted.TargetUID != plan.TargetUID {
		return ErrTargetUIDMismatch
	}

	if granted.PlanHash != plan.Hash {
		return ErrPlanHashMismatch
	}
	if !now.Before(granted.ExpiresAt) {
		return ErrApprovalExpired
	}

	return nil
}
