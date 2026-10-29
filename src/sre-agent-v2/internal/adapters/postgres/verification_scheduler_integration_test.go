//go:build integration

package postgres

import (
	"testing"
	"time"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

type postgresPendingVerificationClaimOutcome struct {
	result incidentdomain.PendingVerificationClaim
	found  bool
	err    error
}

func TestRegistryClaimPendingVerificationAllowsOneConcurrentSchedulerPostgreSQL(
	t *testing.T,
) {
	fixture := newPostgresFencedVerificationRollbackFixture(
		t,
		"pending-verification-scheduler",
		"crash-app-pending-verification-scheduler",
	)

	fenced, err := fixture.registry.BeginFencedVerification(
		fixture.ctx,
		fixture.command,
	)
	if err != nil {
		t.Fatalf(
			"BeginFencedVerification() error = %v",
			err,
		)
	}
	if !fenced.Created {
		t.Fatal(
			"BeginFencedVerification() Created = false; want true",
		)
	}
	if fenced.Incident.State != incidentdomain.StateVerifying {
		t.Fatalf(
			"Incident State = %q; want %q",
			fenced.Incident.State,
			incidentdomain.StateVerifying,
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

	schedulerNow := fixture.claim.ExpiresAt.Add(time.Second)
	leaseDuration := 30 * time.Second

	registries := []*Registry{
		NewRegistry(fixture.pool),
		NewRegistry(fixture.pool),
	}
	holderIDs := []string{
		"verification-scheduler-a",
		"verification-scheduler-b",
	}

	start := make(chan struct{})
	outcomes := make(
		chan postgresPendingVerificationClaimOutcome,
		len(registries),
	)

	for index := range registries {
		registry := registries[index]
		holderID := holderIDs[index]

		go func() {
			<-start

			result, found, claimErr :=
				registry.ClaimPendingVerification(
					fixture.ctx,
					incidentdomain.
						ClaimPendingVerificationCommand{
						HolderID:      holderID,
						Now:           schedulerNow,
						LeaseDuration: leaseDuration,
					},
				)

			outcomes <- postgresPendingVerificationClaimOutcome{
				result: result,
				found:  found,
				err:    claimErr,
			}
		}()
	}

	close(start)

	var claimed []postgresPendingVerificationClaimOutcome

	for range registries {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatalf(
				"ClaimPendingVerification() error = %v; want nil",
				outcome.err,
			)
		}
		if outcome.found {
			claimed = append(claimed, outcome)
		}
	}

	if len(claimed) != 1 {
		t.Fatalf(
			"successful ClaimPendingVerification calls = %d; want 1",
			len(claimed),
		)
	}

	winner := claimed[0].result

	if winner.Verification.ID != fenced.Verification.ID {
		t.Fatalf(
			"claimed Verification ID = %q; want %q",
			winner.Verification.ID,
			fenced.Verification.ID,
		)
	}
	if winner.Verification.ActionKey !=
		fenced.Verification.ActionKey {
		t.Fatalf(
			"claimed Verification ActionKey = %#v; want %#v",
			winner.Verification.ActionKey,
			fenced.Verification.ActionKey,
		)
	}
	if winner.Verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"claimed Verification Status = %q; want %q",
			winner.Verification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}
	if winner.Claim.Incident.ID != fenced.Incident.ID {
		t.Fatalf(
			"claimed Incident ID = %q; want %q",
			winner.Claim.Incident.ID,
			fenced.Incident.ID,
		)
	}
	if winner.Claim.Incident.State !=
		incidentdomain.StateVerifying {
		t.Fatalf(
			"claimed Incident State = %q; want %q",
			winner.Claim.Incident.State,
			incidentdomain.StateVerifying,
		)
	}

	expectedVersion := fenced.Incident.Version + 1
	if winner.Claim.Incident.Version != expectedVersion {
		t.Fatalf(
			"claimed Incident Version = %d; want %d",
			winner.Claim.Incident.Version,
			expectedVersion,
		)
	}
	if winner.Claim.HolderID != holderIDs[0] &&
		winner.Claim.HolderID != holderIDs[1] {
		t.Fatalf(
			"claimed HolderID = %q; want one of %q",
			winner.Claim.HolderID,
			holderIDs,
		)
	}

	expectedExpiry := schedulerNow.Add(leaseDuration)
	if !winner.Claim.ExpiresAt.Equal(expectedExpiry) {
		t.Fatalf(
			"claimed ExpiresAt = %s; want %s",
			winner.Claim.ExpiresAt,
			expectedExpiry,
		)
	}

	history, err := fixture.registry.ClaimHistory(
		fixture.ctx,
		fenced.Incident.ID,
	)
	if err != nil {
		t.Fatalf("ClaimHistory() error = %v", err)
	}

	var persisted bool

	for _, event := range history {
		if event.IncidentVersion !=
			winner.Claim.Incident.Version {
			continue
		}

		persisted = true

		if event.IncidentID != fenced.Incident.ID {
			t.Fatalf(
				"ClaimAudit IncidentID = %q; want %q",
				event.IncidentID,
				fenced.Incident.ID,
			)
		}
		if event.HolderID != winner.Claim.HolderID {
			t.Fatalf(
				"ClaimAudit HolderID = %q; want %q",
				event.HolderID,
				winner.Claim.HolderID,
			)
		}
		if !event.AcquiredAt.Equal(schedulerNow) {
			t.Fatalf(
				"ClaimAudit AcquiredAt = %s; want %s",
				event.AcquiredAt,
				schedulerNow,
			)
		}
		if !event.ExpiresAt.Equal(expectedExpiry) {
			t.Fatalf(
				"ClaimAudit ExpiresAt = %s; want %s",
				event.ExpiresAt,
				expectedExpiry,
			)
		}
	}

	if !persisted {
		t.Fatalf(
			"ClaimHistory() has no event for Incident Version %d",
			winner.Claim.Incident.Version,
		)
	}
}

func TestRegistryClaimPendingVerificationRollsBackWhenAuditInsertFailsPostgreSQL(
	t *testing.T,
) {
	fixture := newPostgresFencedVerificationRollbackFixture(
		t,
		"pending-verification-audit-rollback",
		"crash-app-pending-verification-audit-rollback",
	)

	fenced, err := fixture.registry.BeginFencedVerification(
		fixture.ctx,
		fixture.command,
	)
	if err != nil {
		t.Fatalf(
			"BeginFencedVerification() error = %v",
			err,
		)
	}
	if !fenced.Created {
		t.Fatal(
			"BeginFencedVerification() Created = false; want true",
		)
	}

	const rejectedHolderID = "verification-scheduler-audit-rejected"

	_, err = fixture.pool.Exec(
		fixture.ctx,
		`
ALTER TABLE incident_claims
ADD CONSTRAINT incident_claims_reject_scheduler_audit_test
CHECK (holder_id <> 'verification-scheduler-audit-rejected')
`,
	)
	if err != nil {
		t.Fatalf(
			"install failing Claim Audit constraint: %v",
			err,
		)
	}

	schedulerNow := fixture.claim.ExpiresAt.Add(time.Second)
	leaseDuration := 30 * time.Second

	rejected, found, err :=
		NewRegistry(fixture.pool).ClaimPendingVerification(
			fixture.ctx,
			incidentdomain.ClaimPendingVerificationCommand{
				HolderID:      rejectedHolderID,
				Now:           schedulerNow,
				LeaseDuration: leaseDuration,
			},
		)
	if err == nil {
		t.Fatal(
			"ClaimPendingVerification() error = nil; " +
				"want Claim Audit INSERT failure",
		)
	}
	if found {
		t.Fatal(
			"ClaimPendingVerification() found = true; " +
				"want false after Claim Audit failure",
		)
	}
	if rejected !=
		(incidentdomain.PendingVerificationClaim{}) {
		t.Fatalf(
			"failed PendingVerificationClaim = %#v; want zero value",
			rejected,
		)
	}

	const recoveryHolderID = "verification-scheduler-after-audit-rollback"

	recovered, found, err :=
		NewRegistry(fixture.pool).ClaimPendingVerification(
			fixture.ctx,
			incidentdomain.ClaimPendingVerificationCommand{
				HolderID:      recoveryHolderID,
				Now:           schedulerNow,
				LeaseDuration: leaseDuration,
			},
		)
	if err != nil {
		t.Fatalf(
			"ClaimPendingVerification() after rollback error = %v",
			err,
		)
	}
	if !found {
		t.Fatal(
			"ClaimPendingVerification() after rollback found = false; " +
				"want true",
		)
	}

	if recovered.Verification.ID != fenced.Verification.ID {
		t.Fatalf(
			"recovered Verification ID = %q; want %q",
			recovered.Verification.ID,
			fenced.Verification.ID,
		)
	}
	if recovered.Verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"recovered Verification Status = %q; want %q",
			recovered.Verification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}
	if recovered.Claim.HolderID != recoveryHolderID {
		t.Fatalf(
			"recovered Claim HolderID = %q; want %q",
			recovered.Claim.HolderID,
			recoveryHolderID,
		)
	}

	expectedVersion := fenced.Incident.Version + 1
	if recovered.Claim.Incident.Version != expectedVersion {
		t.Fatalf(
			"recovered Incident Version = %d; want %d; "+
				"failed transaction must not consume a version",
			recovered.Claim.Incident.Version,
			expectedVersion,
		)
	}

	expectedExpiry := schedulerNow.Add(leaseDuration)
	if !recovered.Claim.ExpiresAt.Equal(expectedExpiry) {
		t.Fatalf(
			"recovered Claim ExpiresAt = %s; want %s",
			recovered.Claim.ExpiresAt,
			expectedExpiry,
		)
	}

	history, err := fixture.registry.ClaimHistory(
		fixture.ctx,
		fenced.Incident.ID,
	)
	if err != nil {
		t.Fatalf("ClaimHistory() error = %v", err)
	}

	var (
		rejectedAuditFound bool
		recoveryAuditFound bool
	)

	for _, event := range history {
		switch event.HolderID {
		case rejectedHolderID:
			rejectedAuditFound = true

		case recoveryHolderID:
			if event.IncidentVersion != expectedVersion {
				t.Fatalf(
					"recovery ClaimAudit IncidentVersion = %d; "+
						"want %d",
					event.IncidentVersion,
					expectedVersion,
				)
			}
			if !event.AcquiredAt.Equal(schedulerNow) {
				t.Fatalf(
					"recovery ClaimAudit AcquiredAt = %s; want %s",
					event.AcquiredAt,
					schedulerNow,
				)
			}
			if !event.ExpiresAt.Equal(expectedExpiry) {
				t.Fatalf(
					"recovery ClaimAudit ExpiresAt = %s; want %s",
					event.ExpiresAt,
					expectedExpiry,
				)
			}

			recoveryAuditFound = true
		}
	}

	if rejectedAuditFound {
		t.Fatal(
			"ClaimHistory() contains rejected scheduler audit; " +
				"want failed transaction fully rolled back",
		)
	}
	if !recoveryAuditFound {
		t.Fatal(
			"ClaimHistory() has no successful recovery scheduler audit",
		)
	}
}

func TestRegistryClaimPendingVerificationSurvivesAdapterRestartAndLeaseExpiryPostgreSQL(
	t *testing.T,
) {
	fixture := newPostgresFencedVerificationRollbackFixture(
		t,
		"pending-verification-adapter-restart",
		"crash-app-pending-verification-adapter-restart",
	)

	fenced, err := fixture.registry.BeginFencedVerification(
		fixture.ctx,
		fixture.command,
	)
	if err != nil {
		t.Fatalf(
			"BeginFencedVerification() error = %v",
			err,
		)
	}
	if !fenced.Created {
		t.Fatal(
			"BeginFencedVerification() Created = false; want true",
		)
	}

	firstRegistry := NewRegistry(fixture.pool)
	firstNow := fixture.claim.ExpiresAt.Add(time.Second)
	leaseDuration := 30 * time.Second

	first, found, err := firstRegistry.ClaimPendingVerification(
		fixture.ctx,
		incidentdomain.ClaimPendingVerificationCommand{
			HolderID:      "verification-scheduler-before-restart",
			Now:           firstNow,
			LeaseDuration: leaseDuration,
		},
	)
	if err != nil {
		t.Fatalf(
			"first ClaimPendingVerification() error = %v",
			err,
		)
	}
	if !found {
		t.Fatal(
			"first ClaimPendingVerification() found = false; want true",
		)
	}

	if first.Verification.ID != fenced.Verification.ID {
		t.Fatalf(
			"first Verification ID = %q; want %q",
			first.Verification.ID,
			fenced.Verification.ID,
		)
	}
	if first.Verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"first Verification Status = %q; want %q",
			first.Verification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}

	expectedFirstVersion := fenced.Incident.Version + 1
	if first.Claim.Incident.Version != expectedFirstVersion {
		t.Fatalf(
			"first Incident Version = %d; want %d",
			first.Claim.Incident.Version,
			expectedFirstVersion,
		)
	}

	restartedRegistry := NewRegistry(fixture.pool)

	activeLeaseNow := firstNow.Add(time.Second)

	_, found, err = restartedRegistry.ClaimPendingVerification(
		fixture.ctx,
		incidentdomain.ClaimPendingVerificationCommand{
			HolderID:      "verification-scheduler-after-restart",
			Now:           activeLeaseNow,
			LeaseDuration: leaseDuration,
		},
	)
	if err != nil {
		t.Fatalf(
			"ClaimPendingVerification() during active Lease error = %v",
			err,
		)
	}
	if found {
		t.Fatal(
			"ClaimPendingVerification() during active Lease found = true; " +
				"want false",
		)
	}

	takeoverNow := first.Claim.ExpiresAt.Add(time.Second)
	takeoverLeaseDuration := 45 * time.Second

	takenOver, found, err :=
		restartedRegistry.ClaimPendingVerification(
			fixture.ctx,
			incidentdomain.ClaimPendingVerificationCommand{
				HolderID:      "verification-scheduler-after-restart",
				Now:           takeoverNow,
				LeaseDuration: takeoverLeaseDuration,
			},
		)
	if err != nil {
		t.Fatalf(
			"ClaimPendingVerification() after Lease expiry error = %v",
			err,
		)
	}
	if !found {
		t.Fatal(
			"ClaimPendingVerification() after Lease expiry found = false; " +
				"want true",
		)
	}

	if takenOver.Verification.ID != first.Verification.ID {
		t.Fatalf(
			"taken-over Verification ID = %q; want %q",
			takenOver.Verification.ID,
			first.Verification.ID,
		)
	}
	if takenOver.Verification.ActionKey !=
		first.Verification.ActionKey {
		t.Fatalf(
			"taken-over Verification ActionKey = %#v; want %#v",
			takenOver.Verification.ActionKey,
			first.Verification.ActionKey,
		)
	}
	if takenOver.Verification.Status !=
		remediationdomain.VerificationStatusPending {
		t.Fatalf(
			"taken-over Verification Status = %q; want %q",
			takenOver.Verification.Status,
			remediationdomain.VerificationStatusPending,
		)
	}

	expectedTakeoverVersion := first.Claim.Incident.Version + 1
	if takenOver.Claim.Incident.Version != expectedTakeoverVersion {
		t.Fatalf(
			"taken-over Incident Version = %d; want %d",
			takenOver.Claim.Incident.Version,
			expectedTakeoverVersion,
		)
	}
	if takenOver.Claim.Incident.State !=
		incidentdomain.StateVerifying {
		t.Fatalf(
			"taken-over Incident State = %q; want %q",
			takenOver.Claim.Incident.State,
			incidentdomain.StateVerifying,
		)
	}
	if takenOver.Claim.HolderID !=
		"verification-scheduler-after-restart" {
		t.Fatalf(
			"taken-over HolderID = %q; want %q",
			takenOver.Claim.HolderID,
			"verification-scheduler-after-restart",
		)
	}

	expectedTakeoverExpiry :=
		takeoverNow.Add(takeoverLeaseDuration)
	if !takenOver.Claim.ExpiresAt.Equal(expectedTakeoverExpiry) {
		t.Fatalf(
			"taken-over ExpiresAt = %s; want %s",
			takenOver.Claim.ExpiresAt,
			expectedTakeoverExpiry,
		)
	}

	history, err := restartedRegistry.ClaimHistory(
		fixture.ctx,
		fenced.Incident.ID,
	)
	if err != nil {
		t.Fatalf("ClaimHistory() error = %v", err)
	}

	var (
		firstAuditFound    bool
		takeoverAuditFound bool
	)

	for _, event := range history {
		switch event.HolderID {
		case "verification-scheduler-before-restart":
			if event.IncidentVersion != expectedFirstVersion {
				t.Fatalf(
					"first ClaimAudit IncidentVersion = %d; want %d",
					event.IncidentVersion,
					expectedFirstVersion,
				)
			}

			firstAuditFound = true

		case "verification-scheduler-after-restart":
			if event.IncidentVersion != expectedTakeoverVersion {
				t.Fatalf(
					"takeover ClaimAudit IncidentVersion = %d; "+
						"want %d",
					event.IncidentVersion,
					expectedTakeoverVersion,
				)
			}
			if !event.AcquiredAt.Equal(takeoverNow) {
				t.Fatalf(
					"takeover ClaimAudit AcquiredAt = %s; want %s",
					event.AcquiredAt,
					takeoverNow,
				)
			}
			if !event.ExpiresAt.Equal(expectedTakeoverExpiry) {
				t.Fatalf(
					"takeover ClaimAudit ExpiresAt = %s; want %s",
					event.ExpiresAt,
					expectedTakeoverExpiry,
				)
			}

			takeoverAuditFound = true
		}
	}

	if !firstAuditFound {
		t.Fatal(
			"ClaimHistory() has no pre-restart scheduler audit",
		)
	}
	if !takeoverAuditFound {
		t.Fatal(
			"ClaimHistory() has no post-expiry takeover audit",
		)
	}
}

func TestRegistryClaimRecoveredVerificationResolutionSurvivesAdapterRestartAndLeaseExpiryPostgreSQL(
	t *testing.T,
) {
	fixture := newPostgresFencedVerificationRollbackFixture(
		t,
		"recovered-resolution-adapter-restart",
		"crash-app-recovered-resolution-adapter-restart",
	)

	fenced, err := fixture.registry.BeginFencedVerification(
		fixture.ctx,
		fixture.command,
	)
	if err != nil {
		t.Fatalf(
			"BeginFencedVerification() error = %v",
			err,
		)
	}
	if !fenced.Created {
		t.Fatal(
			"BeginFencedVerification() Created = false; want true",
		)
	}

	schedulerNow := fixture.claim.ExpiresAt.Add(time.Second)
	schedulerLeaseDuration := 30 * time.Second
	schedulerHolderID := "verification-result-scheduler"

	schedulerRegistry := NewRegistry(fixture.pool)

	scheduled, found, err :=
		schedulerRegistry.ClaimPendingVerification(
			fixture.ctx,
			incidentdomain.ClaimPendingVerificationCommand{
				HolderID:      schedulerHolderID,
				Now:           schedulerNow,
				LeaseDuration: schedulerLeaseDuration,
			},
		)
	if err != nil {
		t.Fatalf(
			"ClaimPendingVerification() error = %v",
			err,
		)
	}
	if !found {
		t.Fatal(
			"ClaimPendingVerification() found = false; want true",
		)
	}

	completedAt := schedulerNow.Add(time.Second)

	completed, err :=
		schedulerRegistry.CompleteFencedVerification(
			fixture.ctx,
			incidentdomain.CompleteFencedVerificationCommand{
				IncidentID: scheduled.Claim.Incident.ID,
				ExpectedVersion: scheduled.Claim.Incident.
					Version,
				HolderID: schedulerHolderID,
				Now:      completedAt,
				Verification: remediationdomain.
					CompleteVerificationCommand{
					ActionKey: scheduled.Verification.ActionKey,
					ExpectedVersion: scheduled.Verification.
						Version,
					To: remediationdomain.
						VerificationStatusRecovered,
					FinishedAt: completedAt,
					EvidenceCode: "WORKLOAD_RECOVERED_" +
						"BEFORE_RESOLUTION_RESTART",
				},
			},
		)
	if err != nil {
		t.Fatalf(
			"CompleteFencedVerification() error = %v",
			err,
		)
	}
	if completed.Verification.Status !=
		remediationdomain.VerificationStatusRecovered {
		t.Fatalf(
			"completed Verification Status = %q; want %q",
			completed.Verification.Status,
			remediationdomain.VerificationStatusRecovered,
		)
	}
	if completed.Incident.State !=
		incidentdomain.StateVerifying {
		t.Fatalf(
			"completed Incident State = %q; want %q",
			completed.Incident.State,
			incidentdomain.StateVerifying,
		)
	}
	if completed.Incident.ResolutionVerificationID != "" {
		t.Fatalf(
			"completed Incident ResolutionVerificationID = %q; want empty",
			completed.Incident.ResolutionVerificationID,
		)
	}

	restartedRegistry := NewRegistry(fixture.pool)

	claimAt := scheduled.Claim.ExpiresAt.Add(time.Second)
	resolutionLeaseDuration := 45 * time.Second
	resolutionHolderID := "verification-resolution-scheduler"

	work, found, err :=
		restartedRegistry.ClaimRecoveredVerificationResolution(
			fixture.ctx,
			incidentdomain.
				ClaimRecoveredVerificationResolutionCommand{
				HolderID:      resolutionHolderID,
				Now:           claimAt,
				LeaseDuration: resolutionLeaseDuration,
			},
		)
	if err != nil {
		t.Fatalf(
			"ClaimRecoveredVerificationResolution() error = %v",
			err,
		)
	}
	if !found {
		t.Fatal(
			"ClaimRecoveredVerificationResolution() found = false; " +
				"want true",
		)
	}

	if work.Verification.ID != completed.Verification.ID {
		t.Fatalf(
			"claimed Verification ID = %q; want %q",
			work.Verification.ID,
			completed.Verification.ID,
		)
	}
	if work.Verification.ActionKey !=
		completed.Verification.ActionKey {
		t.Fatalf(
			"claimed Verification ActionKey = %#v; want %#v",
			work.Verification.ActionKey,
			completed.Verification.ActionKey,
		)
	}
	if work.Verification.Status !=
		remediationdomain.VerificationStatusRecovered {
		t.Fatalf(
			"claimed Verification Status = %q; want %q",
			work.Verification.Status,
			remediationdomain.VerificationStatusRecovered,
		)
	}

	if work.Claim.Incident.ID != completed.Incident.ID {
		t.Fatalf(
			"claimed Incident ID = %q; want %q",
			work.Claim.Incident.ID,
			completed.Incident.ID,
		)
	}
	if work.Claim.Incident.State !=
		incidentdomain.StateVerifying {
		t.Fatalf(
			"claimed Incident State = %q; want %q",
			work.Claim.Incident.State,
			incidentdomain.StateVerifying,
		)
	}

	expectedIncidentVersion := completed.Incident.Version + 1
	if work.Claim.Incident.Version != expectedIncidentVersion {
		t.Fatalf(
			"claimed Incident Version = %d; want %d",
			work.Claim.Incident.Version,
			expectedIncidentVersion,
		)
	}
	if work.Claim.Incident.ResolutionVerificationID != "" {
		t.Fatalf(
			"claimed Incident ResolutionVerificationID = %q; want empty",
			work.Claim.Incident.ResolutionVerificationID,
		)
	}
	if work.Claim.HolderID != resolutionHolderID {
		t.Fatalf(
			"claimed HolderID = %q; want %q",
			work.Claim.HolderID,
			resolutionHolderID,
		)
	}

	expectedExpiry := claimAt.Add(resolutionLeaseDuration)
	if !work.Claim.ExpiresAt.Equal(expectedExpiry) {
		t.Fatalf(
			"claimed ExpiresAt = %s; want %s",
			work.Claim.ExpiresAt,
			expectedExpiry,
		)
	}

	history, err := restartedRegistry.ClaimHistory(
		fixture.ctx,
		completed.Incident.ID,
	)
	if err != nil {
		t.Fatalf(
			"ClaimHistory() error = %v",
			err,
		)
	}

	var resolutionAuditFound bool

	for _, event := range history {
		if event.HolderID != resolutionHolderID {
			continue
		}

		if event.IncidentVersion != expectedIncidentVersion {
			t.Fatalf(
				"resolution ClaimAudit IncidentVersion = %d; want %d",
				event.IncidentVersion,
				expectedIncidentVersion,
			)
		}
		if !event.AcquiredAt.Equal(claimAt) {
			t.Fatalf(
				"resolution ClaimAudit AcquiredAt = %s; want %s",
				event.AcquiredAt,
				claimAt,
			)
		}
		if !event.ExpiresAt.Equal(expectedExpiry) {
			t.Fatalf(
				"resolution ClaimAudit ExpiresAt = %s; want %s",
				event.ExpiresAt,
				expectedExpiry,
			)
		}

		resolutionAuditFound = true
	}

	if !resolutionAuditFound {
		t.Fatal(
			"ClaimHistory() has no recovered-resolution scheduler audit",
		)
	}
}
