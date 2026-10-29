package incident

import (
	"context"
	"errors"
	"testing"
	"time"
)

type recoveredResolutionEvidenceStub struct{}

func (recoveredResolutionEvidenceStub) RequireRecovered(
	context.Context,
	string,
	string,
) error {
	return nil
}

func TestRegistryResolveRequiresActiveAuditedClaim(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 14, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		holderID   string
		resolvedAt time.Time
		withAudit  bool
	}{
		{
			name:       "wrong_holder",
			holderID:   "worker-old",
			resolvedAt: now,
			withAudit:  true,
		},
		{
			name:       "expired_lease",
			holderID:   "worker-current",
			resolvedAt: now.Add(time.Minute),
			withAudit:  true,
		},
		{
			name:       "missing_claim_audit",
			holderID:   "worker-current",
			resolvedAt: now,
			withAudit:  false,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			current := Incident{
				ID:      "inc-resolution-fence-" + test.name,
				State:   StateVerifying,
				Version: 7,
			}
			claim := Claim{
				Incident:  current,
				HolderID:  "worker-current",
				ExpiresAt: now.Add(time.Minute),
			}
			registry := NewMemoryRegistry()
			registry.recoveryEvidenceStore =
				recoveredResolutionEvidenceStub{}
			registry.incidentsByID[current.ID] = current
			registry.claimsByIncidentID[current.ID] = claim

			if test.withAudit {
				registry.claimHistoryByIncidentID[current.ID] =
					[]ClaimAuditEvent{
						{
							IncidentID:      current.ID,
							IncidentVersion: current.Version,
							HolderID:        claim.HolderID,
							AcquiredAt:      now.Add(-time.Minute),
							ExpiresAt:       claim.ExpiresAt,
						},
					}
			}

			_, err := registry.Resolve(
				context.Background(),
				ResolveCommand{
					IncidentID:      current.ID,
					ExpectedVersion: current.Version,
					VerificationID:  "ver-resolution-fence",
					HolderID:        test.holderID,
					Now:             test.resolvedAt,
					Actor:           test.holderID,
					ReasonCode:      "RECOVERY_VERIFIED",
				},
			)
			if !errors.Is(err, ErrVerificationFenceConflict) {
				t.Fatalf(
					"Resolve() error = %v; want ErrVerificationFenceConflict",
					err,
				)
			}

			if got := registry.incidentsByID[current.ID]; got != current {
				t.Fatalf(
					"Resolve() changed Incident = %#v; want %#v",
					got,
					current,
				)
			}
		})
	}
}
