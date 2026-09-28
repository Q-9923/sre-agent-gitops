package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const planSchemaVersion = "v1"

var (
	ErrInvalidPlan         = errors.New("invalid plan")
	ErrInvalidApproval     = errors.New("invalid approval")
	ErrIncidentMismatch    = errors.New("approval incident mismatch")
	ErrTargetUIDMismatch   = errors.New("approval target UID mismatch")
	ErrPlanHashMismatch    = errors.New("approval plan hash mismatch")
	ErrApprovalNotYetValid = errors.New("approval not yet valid")
	ErrApprovalExpired     = errors.New("approval expired")
)

type Target struct {
	Cluster   string
	Namespace string
	Kind      string
	Name      string
	UID       string
}

type PlanCommand struct {
	IncidentID string
	Action     string
	Target     Target
}

type Plan struct {
	IncidentID string
	Action     string
	Target     Target
	Hash       string
}

type Approval struct {
	IncidentID string
	PlanHash   string
	TargetUID  string
	ApprovedBy string
	ApprovedAt time.Time
	ExpiresAt  time.Time
}

func NewPlan(command PlanCommand) (Plan, error) {
	if invalidIdentity(command.IncidentID) ||
		invalidIdentity(command.Action) ||
		invalidIdentity(command.Target.Cluster) ||
		invalidIdentity(command.Target.Namespace) ||
		invalidIdentity(command.Target.Kind) ||
		invalidIdentity(command.Target.Name) ||
		invalidIdentity(command.Target.UID) {
		return Plan{}, ErrInvalidPlan
	}

	canonical := strings.Join(
		[]string{
			planSchemaVersion,
			command.IncidentID,
			command.Action,
			command.Target.Cluster,
			command.Target.Namespace,
			command.Target.Kind,
			command.Target.Name,
			command.Target.UID,
		},
		"\x00",
	)

	digest := sha256.Sum256([]byte(canonical))

	return Plan{
		IncidentID: command.IncidentID,
		Action:     command.Action,
		Target:     command.Target,
		Hash:       "sha256:" + hex.EncodeToString(digest[:]),
	}, nil
}

func Validate(granted Approval, plan Plan, now time.Time) error {
	canonicalPlan, err := NewPlan(
		PlanCommand{
			IncidentID: plan.IncidentID,
			Action:     plan.Action,
			Target:     plan.Target,
		},
	)
	if err != nil || canonicalPlan.Hash != plan.Hash {
		return ErrInvalidPlan
	}

	if err := ValidateRecord(granted); err != nil {
		return err
	}
	if granted.IncidentID != plan.IncidentID {
		return ErrIncidentMismatch
	}

	if granted.TargetUID != plan.Target.UID {
		return ErrTargetUIDMismatch
	}

	if granted.PlanHash != plan.Hash {
		return ErrPlanHashMismatch
	}

	if now.Before(granted.ApprovedAt) {
		return ErrApprovalNotYetValid
	}

	if !now.Before(granted.ExpiresAt) {
		return ErrApprovalExpired
	}

	return nil
}

func ValidateRecord(granted Approval) error {
	if invalidIdentity(granted.IncidentID) ||
		invalidIdentity(granted.PlanHash) ||
		invalidIdentity(granted.TargetUID) ||
		invalidIdentity(granted.ApprovedBy) ||
		granted.ApprovedAt.IsZero() ||
		granted.ExpiresAt.IsZero() ||
		!granted.ExpiresAt.After(granted.ApprovedAt) {
		return ErrInvalidApproval
	}

	return nil
}

func invalidIdentity(value string) bool {
	return strings.TrimSpace(value) == "" ||
		strings.ContainsRune(value, '\x00')
}
