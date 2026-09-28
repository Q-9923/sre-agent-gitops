package approval

import (
	"errors"
	"testing"
	"time"
)

func TestValidateRejectsApprovalForDifferentTargetUID(t *testing.T) {
	t.Parallel()

	now := time.Date(
		2026,
		time.September,
		28,
		17,
		0,
		0,
		0,
		time.UTC,
	)

	plan := Plan{
		IncidentID: "inc-approval-target-uid",
		Hash:       "sha256:approved-plan",
		TargetUID:  "pod-uid-current",
	}

	granted := Approval{
		IncidentID: plan.IncidentID,
		PlanHash:   plan.Hash,
		TargetUID:  "pod-uid-replaced",
		ApprovedBy: "operator-a",
		ApprovedAt: now.Add(-time.Minute),
		ExpiresAt:  now.Add(time.Minute),
	}

	err := Validate(granted, plan, now)
	if !errors.Is(err, ErrTargetUIDMismatch) {
		t.Fatalf(
			"Validate() error = %v; want ErrTargetUIDMismatch",
			err,
		)
	}
}
func TestValidateRejectsApprovalForDifferentPlanHash(t *testing.T) {
	t.Parallel()

	now := time.Date(
		2026,
		time.September,
		28,
		17,
		5,
		0,
		0,
		time.UTC,
	)

	plan := Plan{
		IncidentID: "inc-approval-plan-hash",
		Hash:       "sha256:current-plan",
		TargetUID:  "pod-uid-current",
	}

	granted := Approval{
		IncidentID: plan.IncidentID,
		PlanHash:   "sha256:previous-plan",
		TargetUID:  plan.TargetUID,
		ApprovedBy: "operator-a",
		ApprovedAt: now.Add(-time.Minute),
		ExpiresAt:  now.Add(time.Minute),
	}

	err := Validate(granted, plan, now)
	if !errors.Is(err, ErrPlanHashMismatch) {
		t.Fatalf(
			"Validate() error = %v; want ErrPlanHashMismatch",
			err,
		)
	}
}
func TestValidateRejectsApprovalForDifferentIncidentID(t *testing.T) {
	t.Parallel()

	now := time.Date(
		2026,
		time.September,
		28,
		17,
		10,
		0,
		0,
		time.UTC,
	)

	plan := Plan{
		IncidentID: "inc-current",
		Hash:       "sha256:current-plan",
		TargetUID:  "pod-uid-current",
	}

	granted := Approval{
		IncidentID: "inc-previous",
		PlanHash:   plan.Hash,
		TargetUID:  plan.TargetUID,
		ApprovedBy: "operator-a",
		ApprovedAt: now.Add(-time.Minute),
		ExpiresAt:  now.Add(time.Minute),
	}

	err := Validate(granted, plan, now)
	if !errors.Is(err, ErrIncidentMismatch) {
		t.Fatalf(
			"Validate() error = %v; want ErrIncidentMismatch",
			err,
		)
	}
}
func TestValidateRejectsApprovalAtExpiry(t *testing.T) {
	t.Parallel()

	now := time.Date(
		2026,
		time.September,
		28,
		17,
		15,
		0,
		0,
		time.UTC,
	)

	plan := Plan{
		IncidentID: "inc-expired-approval",
		Hash:       "sha256:current-plan",
		TargetUID:  "pod-uid-current",
	}

	granted := Approval{
		IncidentID: plan.IncidentID,
		PlanHash:   plan.Hash,
		TargetUID:  plan.TargetUID,
		ApprovedBy: "operator-a",
		ApprovedAt: now.Add(-time.Minute),
		ExpiresAt:  now,
	}

	err := Validate(granted, plan, now)
	if !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf(
			"Validate() error = %v; want ErrApprovalExpired",
			err,
		)
	}
}

func TestValidateRejectsApprovalWithoutApprover(t *testing.T) {
	t.Parallel()

	now := time.Date(
		2026,
		time.September,
		28,
		17,
		20,
		0,
		0,
		time.UTC,
	)

	plan := Plan{
		IncidentID: "inc-missing-approver",
		Hash:       "sha256:current-plan",
		TargetUID:  "pod-uid-current",
	}

	granted := Approval{
		IncidentID: plan.IncidentID,
		PlanHash:   plan.Hash,
		TargetUID:  plan.TargetUID,
		ApprovedAt: now.Add(-time.Minute),
		ExpiresAt:  now.Add(time.Minute),
	}

	err := Validate(granted, plan, now)
	if !errors.Is(err, ErrInvalidApproval) {
		t.Fatalf(
			"Validate() error = %v; want ErrInvalidApproval",
			err,
		)
	}
}
func TestValidateRejectsInvalidApprovalWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(
		2026,
		time.September,
		28,
		17,
		25,
		0,
		0,
		time.UTC,
	)

	plan := Plan{
		IncidentID: "inc-invalid-approval-window",
		Hash:       "sha256:current-plan",
		TargetUID:  "pod-uid-current",
	}

	granted := Approval{
		IncidentID: plan.IncidentID,
		PlanHash:   plan.Hash,
		TargetUID:  plan.TargetUID,
		ApprovedBy: "operator-a",
		ApprovedAt: now.Add(2 * time.Minute),
		ExpiresAt:  now.Add(time.Minute),
	}

	err := Validate(granted, plan, now)
	if !errors.Is(err, ErrInvalidApproval) {
		t.Fatalf(
			"Validate() error = %v; want ErrInvalidApproval",
			err,
		)
	}
}
func TestValidateRejectsApprovalBeforeApprovedAt(t *testing.T) {
	t.Parallel()

	now := time.Date(
		2026,
		time.September,
		28,
		17,
		30,
		0,
		0,
		time.UTC,
	)

	plan := Plan{
		IncidentID: "inc-future-approval",
		Hash:       "sha256:current-plan",
		TargetUID:  "pod-uid-current",
	}

	granted := Approval{
		IncidentID: plan.IncidentID,
		PlanHash:   plan.Hash,
		TargetUID:  plan.TargetUID,
		ApprovedBy: "operator-a",
		ApprovedAt: now.Add(time.Minute),
		ExpiresAt:  now.Add(2 * time.Minute),
	}

	err := Validate(granted, plan, now)
	if !errors.Is(err, ErrApprovalNotYetValid) {
		t.Fatalf(
			"Validate() error = %v; want ErrApprovalNotYetValid",
			err,
		)
	}
}
func TestValidateAcceptsMatchingActiveApproval(t *testing.T) {
	t.Parallel()

	now := time.Date(
		2026,
		time.September,
		28,
		17,
		35,
		0,
		0,
		time.UTC,
	)

	plan := Plan{
		IncidentID: "inc-valid-approval",
		Hash:       "sha256:approved-plan",
		TargetUID:  "pod-uid-current",
	}

	granted := Approval{
		IncidentID: plan.IncidentID,
		PlanHash:   plan.Hash,
		TargetUID:  plan.TargetUID,
		ApprovedBy: "operator-a",
		ApprovedAt: now.Add(-time.Minute),
		ExpiresAt:  now.Add(time.Minute),
	}

	if err := Validate(granted, plan, now); err != nil {
		t.Fatalf("Validate() error = %v; want nil", err)
	}
}
