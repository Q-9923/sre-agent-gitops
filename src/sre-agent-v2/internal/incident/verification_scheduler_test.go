package incident_test

import (
	"context"
	"testing"
	"time"

	"errors"
	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func TestRegistryClaimPendingVerificationAllowsOneConcurrentScheduler(
	t *testing.T,
) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(
		2026,
		time.October,
		7,
		18,
		0,
		0,
		0,
		time.UTC,
	)

	verificationStore :=
		remediationdomain.NewMemoryVerificationStore()

	registry :=
		incidentdomain.NewRegistryWithVerificationLifecycleStore(
			verificationStore,
		)

	observed, created, err := registry.Observe(
		ctx,
		incidentdomain.Observation{
			Source:    "prometheus",
			Cluster:   "dev",
			AlertName: "PodCrashLooping",
			Target: incidentdomain.Target{
				Kind:      "Pod",
				Namespace: "default",
				Name:      "pending-verification-app",
				UID:       "pod-uid-pending-verification",
			},
		},
	)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if !created {
		t.Fatal("Observe() created = false; want true")
	}

	diagnosed, err := registry.Transition(
		ctx,
		incidentdomain.TransitionCommand{
			IncidentID:      observed.ID,
			ExpectedVersion: observed.Version,
			To:              incidentdomain.StateDiagnosed,
			Actor:           "agent-a",
			ReasonCode:      "DIAGNOSIS_COMPLETED",
		},
	)
	if err != nil {
		t.Fatalf(
			"Transition(DETECTED -> DIAGNOSED) error = %v",
			err,
		)
	}

	actionClaim, err := registry.Claim(
		ctx,
		incidentdomain.ClaimCommand{
			IncidentID:      diagnosed.ID,
			ExpectedVersion: diagnosed.Version,
			HolderID:        "agent-a",
			Now:             now,
			LeaseDuration:   30 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("Claim(agent-a) error = %v", err)
	}

	actionFinishedAt := now.Add(-time.Second)
	actionAttempt := remediationdomain.ActionAttempt{
		ID: "att-pending-verification-scheduler",
		Key: remediationdomain.ExecutionKey{
			IncidentID: actionClaim.Incident.ID,
			PlanHash: "sha256:" +
				"pending-verification-scheduler-plan",
			TargetUID: "pod-uid-pending-verification",
			FencingToken: actionClaim.Incident.
				Version,
		},
		Status: remediationdomain.
			ActionAttemptStatusSucceeded,
		Version:    2,
		StartedAt:  now.Add(-2 * time.Second),
		FinishedAt: &actionFinishedAt,
	}

	fenced, err := registry.BeginFencedVerification(
		ctx,
		incidentdomain.BeginFencedVerificationCommand{
			IncidentID: actionClaim.Incident.ID,
			ExpectedVersion: actionClaim.Incident.
				Version,
			HolderID:   "agent-a",
			Now:        now.Add(time.Second),
			ReasonCode: "ACTION_ATTEMPT_TERMINAL",
			Verification: remediationdomain.
				BeginVerificationCommand{
				ActionAttempt: actionAttempt,
				Subject: remediationdomain.
					VerificationSubject{
					Cluster:   "dev",
					Namespace: "default",
					Kind:      "ReplicaSet",
					Name: "pending-verification-" +
						"rs",
					UID: "rs-uid-pending-" +
						"verification",
				},
				StartedAt: now,
			},
		},
	)
	if err != nil {
		t.Fatalf(
			"BeginFencedVerification() error = %v",
			err,
		)
	}
	if fenced.Verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"Verification Status = %q; want %q",
			fenced.Verification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}

	schedulingAt := actionClaim.ExpiresAt
	schedulingLease := 30 * time.Second

	start := make(chan struct{})
	outcomes := make(
		chan pendingVerificationScheduleOutcome,
		2,
	)

	for _, holderID := range []string{
		"agent-b",
		"agent-c",
	} {
		holderID := holderID

		go func() {
			<-start

			scheduled, found, claimErr :=
				registry.ClaimPendingVerification(
					ctx,
					incidentdomain.
						ClaimPendingVerificationCommand{
						HolderID:      holderID,
						Now:           schedulingAt,
						LeaseDuration: schedulingLease,
					},
				)

			outcome := pendingVerificationScheduleOutcome{
				found: found,
				err:   claimErr,
			}
			if found {
				outcome.verification =
					scheduled.Verification
				outcome.claim = scheduled.Claim
			}

			outcomes <- outcome
		}()
	}

	close(start)

	results := []pendingVerificationScheduleOutcome{
		<-outcomes,
		<-outcomes,
	}

	successful := make(
		[]pendingVerificationScheduleOutcome,
		0,
		1,
	)
	notFound := 0

	for _, result := range results {
		if result.err != nil {
			t.Fatalf(
				"ClaimPendingVerification() error = %v; "+
					"want nil",
				result.err,
			)
		}

		if result.found {
			successful = append(successful, result)
			continue
		}

		notFound++
	}

	if len(successful) != 1 {
		t.Fatalf(
			"successful schedules = %d; want 1",
			len(successful),
		)
	}
	if notFound != 1 {
		t.Fatalf(
			"not-found schedules = %d; want 1",
			notFound,
		)
	}

	winner := successful[0]

	if winner.verification.ID != fenced.Verification.ID {
		t.Fatalf(
			"scheduled Verification ID = %q; want %q",
			winner.verification.ID,
			fenced.Verification.ID,
		)
	}
	if winner.verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"scheduled Verification Status = %q; want %q",
			winner.verification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}
	if winner.claim.HolderID != "agent-b" &&
		winner.claim.HolderID != "agent-c" {
		t.Fatalf(
			"scheduled Claim HolderID = %q; "+
				"want agent-b or agent-c",
			winner.claim.HolderID,
		)
	}
	if winner.claim.Incident.ID != fenced.Incident.ID {
		t.Fatalf(
			"scheduled Incident ID = %q; want %q",
			winner.claim.Incident.ID,
			fenced.Incident.ID,
		)
	}
	if winner.claim.Incident.State !=
		incidentdomain.StateVerifying {
		t.Fatalf(
			"scheduled Incident State = %q; want %q",
			winner.claim.Incident.State,
			incidentdomain.StateVerifying,
		)
	}
	if winner.claim.Incident.Version !=
		fenced.Incident.Version+1 {
		t.Fatalf(
			"scheduled Incident Version = %d; want %d",
			winner.claim.Incident.Version,
			fenced.Incident.Version+1,
		)
	}

	wantExpiresAt := schedulingAt.Add(schedulingLease)
	if !winner.claim.ExpiresAt.Equal(wantExpiresAt) {
		t.Fatalf(
			"scheduled Claim ExpiresAt = %s; want %s",
			winner.claim.ExpiresAt,
			wantExpiresAt,
		)
	}

	takeoverAt := winner.claim.ExpiresAt
	takeoverLease := 45 * time.Second

	takeover, found, err :=
		registry.ClaimPendingVerification(
			ctx,
			incidentdomain.
				ClaimPendingVerificationCommand{
				HolderID:      "agent-d",
				Now:           takeoverAt,
				LeaseDuration: takeoverLease,
			},
		)
	if err != nil {
		t.Fatalf(
			"expired lease takeover error = %v; want nil",
			err,
		)
	}
	if !found {
		t.Fatal(
			"expired lease takeover found = false; want true",
		)
	}
	if takeover.Verification.ID !=
		fenced.Verification.ID {
		t.Fatalf(
			"takeover Verification ID = %q; want %q",
			takeover.Verification.ID,
			fenced.Verification.ID,
		)
	}
	if takeover.Claim.HolderID != "agent-d" {
		t.Fatalf(
			"takeover Claim HolderID = %q; want agent-d",
			takeover.Claim.HolderID,
		)
	}
	if takeover.Claim.Incident.Version !=
		winner.claim.Incident.Version+1 {
		t.Fatalf(
			"takeover Incident Version = %d; want %d",
			takeover.Claim.Incident.Version,
			winner.claim.Incident.Version+1,
		)
	}

	wantTakeoverExpiresAt :=
		takeoverAt.Add(takeoverLease)
	if !takeover.Claim.ExpiresAt.Equal(
		wantTakeoverExpiresAt,
	) {
		t.Fatalf(
			"takeover Claim ExpiresAt = %s; want %s",
			takeover.Claim.ExpiresAt,
			wantTakeoverExpiresAt,
		)
	}
}

type pendingVerificationScheduleOutcome struct {
	verification remediationdomain.Verification
	claim        incidentdomain.Claim
	found        bool
	err          error
}

func TestRegistryClaimPendingVerificationRejectsInvalidCommand(
	t *testing.T,
) {
	t.Parallel()

	now := time.Date(
		2026,
		time.October,
		8,
		10,
		0,
		0,
		0,
		time.UTC,
	)

	tests := []struct {
		name    string
		command incidentdomain.ClaimPendingVerificationCommand
	}{
		{
			name: "missing holder",
			command: incidentdomain.
				ClaimPendingVerificationCommand{
				Now:           now,
				LeaseDuration: 30 * time.Second,
			},
		},
		{
			name: "missing current time",
			command: incidentdomain.
				ClaimPendingVerificationCommand{
				HolderID:      "agent-a",
				LeaseDuration: 30 * time.Second,
			},
		},
		{
			name: "missing lease duration",
			command: incidentdomain.
				ClaimPendingVerificationCommand{
				HolderID: "agent-a",
				Now:      now,
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			registry :=
				incidentdomain.NewMemoryRegistry()

			_, found, err :=
				registry.ClaimPendingVerification(
					context.Background(),
					testCase.command,
				)

			if !errors.Is(
				err,
				incidentdomain.
					ErrInvalidPendingVerificationClaim,
			) {
				t.Fatalf(
					"ClaimPendingVerification() error = %v; "+
						"want ErrInvalidPendingVerificationClaim",
					err,
				)
			}
			if found {
				t.Fatal(
					"ClaimPendingVerification() found = true; " +
						"want false",
				)
			}
		})
	}
}

func TestRegistryClaimPendingVerificationFailsClosedWithoutPendingSource(
	t *testing.T,
) {
	t.Parallel()

	registry := incidentdomain.NewMemoryRegistry()

	_, found, err := registry.ClaimPendingVerification(
		context.Background(),
		incidentdomain.ClaimPendingVerificationCommand{
			HolderID: "agent-a",
			Now: time.Date(
				2026,
				time.October,
				8,
				11,
				0,
				0,
				0,
				time.UTC,
			),
			LeaseDuration: 30 * time.Second,
		},
	)

	if !errors.Is(
		err,
		incidentdomain.
			ErrPendingVerificationSchedulerUnavailable,
	) {
		t.Fatalf(
			"ClaimPendingVerification() error = %v; "+
				"want ErrPendingVerificationSchedulerUnavailable",
			err,
		)
	}
	if found {
		t.Fatal(
			"ClaimPendingVerification() found = true; want false",
		)
	}
}
