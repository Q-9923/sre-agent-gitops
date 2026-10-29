package incident_test

import (
	"testing"
	"time"

	incidentdomain "sre-agent/internal/incident"
	remediationdomain "sre-agent/internal/remediation"
)

func TestRegistryClaimRecoveredVerificationResolutionAfterLeaseExpiry(
	t *testing.T,
) {
	t.Parallel()

	fixture :=
		newFencedVerificationCompletionTakeoverFixture(t)

	completedAt := fixture.takeoverAt.Add(time.Second)
	completed, err := fixture.registry.
		CompleteFencedVerification(
			fixture.ctx,
			fixture.completionCommand(completedAt),
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

	claimAt := fixture.activeClaim.ExpiresAt
	leaseDuration := 30 * time.Second

	work, found, err := fixture.registry.
		ClaimRecoveredVerificationResolution(
			fixture.ctx,
			incidentdomain.
				ClaimRecoveredVerificationResolutionCommand{
				HolderID:      "verification-resolver",
				Now:           claimAt,
				LeaseDuration: leaseDuration,
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
	if work.Claim.Incident.Version !=
		completed.Incident.Version+1 {
		t.Fatalf(
			"claimed Incident Version = %d; want %d",
			work.Claim.Incident.Version,
			completed.Incident.Version+1,
		)
	}
	if work.Claim.Incident.ResolutionVerificationID != "" {
		t.Fatalf(
			"claimed Incident ResolutionVerificationID = %q; want empty",
			work.Claim.Incident.ResolutionVerificationID,
		)
	}
	if work.Claim.HolderID != "verification-resolver" {
		t.Fatalf(
			"claimed HolderID = %q; want %q",
			work.Claim.HolderID,
			"verification-resolver",
		)
	}
	if !work.Claim.ExpiresAt.Equal(
		claimAt.Add(leaseDuration),
	) {
		t.Fatalf(
			"claimed ExpiresAt = %s; want %s",
			work.Claim.ExpiresAt,
			claimAt.Add(leaseDuration),
		)
	}
}
