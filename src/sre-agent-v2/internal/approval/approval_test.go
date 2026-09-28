package approval

import (
	"errors"
	"testing"
	"time"
)

var approvalTestNow = time.Date(
	2026,
	time.September,
	28,
	17,
	35,
	0,
	0,
	time.UTC,
)

func TestNewPlanBuildsCanonicalHash(t *testing.T) {
	t.Parallel()

	plan := mustNewPlan(t, validPlanCommand())

	const wantHash = "sha256:05be5048e1d75d42d1c293a8798e94afcdf02a82f1dee536f114c542ffed2999"
	if plan.Hash != wantHash {
		t.Fatalf(
			"NewPlan() Hash = %q; want %q",
			plan.Hash,
			wantHash,
		)
	}
}

func TestNewPlanHashChangesWhenTargetUIDChanges(t *testing.T) {
	t.Parallel()

	firstCommand := validPlanCommand()
	first := mustNewPlan(t, firstCommand)

	secondCommand := validPlanCommand()
	secondCommand.Target.UID = "pod-uid-456"
	second := mustNewPlan(t, secondCommand)

	if first.Hash == second.Hash {
		t.Fatalf(
			"NewPlan() generated the same Hash %q for different target UIDs",
			first.Hash,
		)
	}
}

func TestNewPlanRejectsInvalidCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*PlanCommand)
	}{
		{
			name: "missing incident ID",
			mutate: func(command *PlanCommand) {
				command.IncidentID = ""
			},
		},
		{
			name: "missing action",
			mutate: func(command *PlanCommand) {
				command.Action = ""
			},
		},
		{
			name: "missing cluster",
			mutate: func(command *PlanCommand) {
				command.Target.Cluster = ""
			},
		},
		{
			name: "missing namespace",
			mutate: func(command *PlanCommand) {
				command.Target.Namespace = ""
			},
		},
		{
			name: "missing kind",
			mutate: func(command *PlanCommand) {
				command.Target.Kind = ""
			},
		},
		{
			name: "missing name",
			mutate: func(command *PlanCommand) {
				command.Target.Name = ""
			},
		},
		{
			name: "missing UID",
			mutate: func(command *PlanCommand) {
				command.Target.UID = ""
			},
		},
		{
			name: "canonical delimiter in field",
			mutate: func(command *PlanCommand) {
				command.Action = "RESTART\x00POD"
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(
			test.name,
			func(t *testing.T) {
				t.Parallel()

				command := validPlanCommand()
				test.mutate(&command)

				_, err := NewPlan(command)
				if !errors.Is(err, ErrInvalidPlan) {
					t.Fatalf(
						"NewPlan() error = %v; want ErrInvalidPlan",
						err,
					)
				}
			},
		)
	}
}

func TestValidateRejectsMismatchedIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Approval)
		want   error
	}{
		{
			name: "incident ID",
			mutate: func(granted *Approval) {
				granted.IncidentID = "inc-previous"
			},
			want: ErrIncidentMismatch,
		},
		{
			name: "plan hash",
			mutate: func(granted *Approval) {
				granted.PlanHash = "sha256:previous-plan"
			},
			want: ErrPlanHashMismatch,
		},
		{
			name: "target UID",
			mutate: func(granted *Approval) {
				granted.TargetUID = "pod-uid-replaced"
			},
			want: ErrTargetUIDMismatch,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(
			test.name,
			func(t *testing.T) {
				t.Parallel()

				plan := mustNewPlan(t, validPlanCommand())
				granted := validApproval(plan)
				test.mutate(&granted)

				err := Validate(granted, plan, approvalTestNow)
				if !errors.Is(err, test.want) {
					t.Fatalf(
						"Validate() error = %v; want %v",
						err,
						test.want,
					)
				}
			},
		)
	}
}

func TestValidateRejectsInvalidApproval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Approval)
	}{
		{
			name: "missing approver",
			mutate: func(granted *Approval) {
				granted.ApprovedBy = ""
			},
		},
		{
			name: "missing approved time",
			mutate: func(granted *Approval) {
				granted.ApprovedAt = time.Time{}
			},
		},
		{
			name: "missing expiry time",
			mutate: func(granted *Approval) {
				granted.ExpiresAt = time.Time{}
			},
		},
		{
			name: "zero length validity window",
			mutate: func(granted *Approval) {
				granted.ExpiresAt = granted.ApprovedAt
			},
		},
		{
			name: "reversed validity window",
			mutate: func(granted *Approval) {
				granted.ExpiresAt = granted.ApprovedAt.Add(-time.Second)
			},
		},
		{
			name: "canonical delimiter in approver",
			mutate: func(granted *Approval) {
				granted.ApprovedBy = "operator\x00a"
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(
			test.name,
			func(t *testing.T) {
				t.Parallel()

				plan := mustNewPlan(t, validPlanCommand())
				granted := validApproval(plan)
				test.mutate(&granted)

				err := Validate(granted, plan, approvalTestNow)
				if !errors.Is(err, ErrInvalidApproval) {
					t.Fatalf(
						"Validate() error = %v; want ErrInvalidApproval",
						err,
					)
				}
			},
		)
	}
}

func TestValidateRejectsInactiveApproval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		now  time.Time
		want error
	}{
		{
			name: "before approved time",
			now:  approvalTestNow.Add(-2 * time.Minute),
			want: ErrApprovalNotYetValid,
		},
		{
			name: "at expiry",
			now:  approvalTestNow.Add(time.Minute),
			want: ErrApprovalExpired,
		},
		{
			name: "after expiry",
			now:  approvalTestNow.Add(2 * time.Minute),
			want: ErrApprovalExpired,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(
			test.name,
			func(t *testing.T) {
				t.Parallel()

				plan := mustNewPlan(t, validPlanCommand())
				granted := validApproval(plan)

				err := Validate(granted, plan, test.now)
				if !errors.Is(err, test.want) {
					t.Fatalf(
						"Validate() error = %v; want %v",
						err,
						test.want,
					)
				}
			},
		)
	}
}

func TestValidateRejectsTamperedPlanHash(t *testing.T) {
	t.Parallel()

	plan := mustNewPlan(t, validPlanCommand())
	plan.Hash = "sha256:tampered"

	granted := validApproval(plan)

	err := Validate(granted, plan, approvalTestNow)
	if !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf(
			"Validate() error = %v; want ErrInvalidPlan",
			err,
		)
	}
}

func TestValidateAcceptsMatchingActiveApproval(t *testing.T) {
	t.Parallel()

	plan := mustNewPlan(t, validPlanCommand())
	granted := validApproval(plan)

	if err := Validate(granted, plan, approvalTestNow); err != nil {
		t.Fatalf("Validate() error = %v; want nil", err)
	}
}

func validPlanCommand() PlanCommand {
	return PlanCommand{
		IncidentID: "inc-canonical-plan",
		Action:     "RESTART_POD",
		Target: Target{
			Cluster:   "dev",
			Namespace: "sre-agent-lab",
			Kind:      "Pod",
			Name:      "crash-app-abc",
			UID:       "pod-uid-123",
		},
	}
}

func mustNewPlan(t *testing.T, command PlanCommand) Plan {
	t.Helper()

	plan, err := NewPlan(command)
	if err != nil {
		t.Fatalf("NewPlan() error = %v; want nil", err)
	}

	return plan
}

func validApproval(plan Plan) Approval {
	return Approval{
		IncidentID: plan.IncidentID,
		PlanHash:   plan.Hash,
		TargetUID:  plan.Target.UID,
		ApprovedBy: "operator-a",
		ApprovedAt: approvalTestNow.Add(-time.Minute),
		ExpiresAt:  approvalTestNow.Add(time.Minute),
	}
}
