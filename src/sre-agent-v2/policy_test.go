package main

import "testing"

func TestEvaluateDecisionAllowsApprovedRestartForMatchingManagedPod(t *testing.T) {
	decision := Decision{
		SchemaVersion: "v1",
		IncidentID:    "inc-policy-allow",
		Action:        ActionRestartPod,
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-test",
			UID:       "uid-policy-allow",
		},
		Reason:           "The current evidence supports one controlled restart.",
		Evidence:         []string{"container:last_exit_code=1"},
		Confidence:       0.95,
		Risk:             "medium",
		RequiresApproval: true,
	}
	policyContext := PolicyContext{
		IncidentID: "inc-policy-allow",
		CurrentTarget: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-test",
			UID:       "uid-policy-allow",
		},
		AllowedNamespaces:      []string{"default"},
		ControllerOwnerKind:    "ReplicaSet",
		AllowedControllerKinds: []string{"ReplicaSet"},
		ApprovalGranted:        true,
		MinimumConfidence:      0.90,
		AttemptsInWindow:       0,
		MaximumAttempts:        1,
	}

	result := EvaluateDecision(decision, policyContext)

	if !result.Allowed {
		t.Fatalf("EvaluateDecision() Allowed = false, code = %q; want an allowed restart", result.Code)
	}
	if result.Action != ActionRestartPod {
		t.Fatalf("EvaluateDecision() Action = %q; want %q", result.Action, ActionRestartPod)
	}
	if result.Code != PolicyCodeAllowed {
		t.Fatalf("EvaluateDecision() Code = %q; want %q", result.Code, PolicyCodeAllowed)
	}
}

func TestEvaluateDecisionRejectsNonExecutableDecisions(t *testing.T) {
	tests := []struct {
		name     string
		decision Decision
		wantCode string
	}{
		{
			name:     "invalid decision",
			decision: Decision{},
			wantCode: PolicyCodeInvalidDecision,
		},
		{
			name:     "ignore",
			decision: policyTestDecision(ActionIgnore),
			wantCode: PolicyCodeDecisionIgnore,
		},
		{
			name:     "escalate",
			decision: policyTestDecision(ActionEscalate),
			wantCode: PolicyCodeDecisionEscalate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EvaluateDecision(tt.decision, policyTestContext())

			if result.Allowed {
				t.Fatalf("EvaluateDecision() Allowed = true; want false for %s", tt.name)
			}
			if result.Code != tt.wantCode {
				t.Fatalf("EvaluateDecision() Code = %q; want %q", result.Code, tt.wantCode)
			}
		})
	}
}

func TestEvaluateDecisionRejectsStaleOrUnauthorizedTargets(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*PolicyContext)
		wantCode string
	}{
		{
			name: "decision belongs to another incident",
			mutate: func(policyContext *PolicyContext) {
				policyContext.IncidentID = "inc-another-alert"
			},
			wantCode: PolicyCodeIncidentMismatch,
		},
		{
			name: "stale pod uid",
			mutate: func(policyContext *PolicyContext) {
				policyContext.CurrentTarget.UID = "replacement-pod-uid"
			},
			wantCode: PolicyCodeTargetMismatch,
		},
		{
			name: "namespace outside allowlist",
			mutate: func(policyContext *PolicyContext) {
				policyContext.AllowedNamespaces = []string{"sandbox"}
			},
			wantCode: PolicyCodeNamespaceDenied,
		},
		{
			name: "owner kind outside allowlist",
			mutate: func(policyContext *PolicyContext) {
				policyContext.ControllerOwnerKind = "StatefulSet"
			},
			wantCode: PolicyCodeOwnerDenied,
		},
		{
			name: "approval not granted",
			mutate: func(policyContext *PolicyContext) {
				policyContext.ApprovalGranted = false
			},
			wantCode: PolicyCodeApprovalRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policyContext := policyTestContext()
			tt.mutate(&policyContext)

			result := EvaluateDecision(policyTestDecision(ActionRestartPod), policyContext)

			if result.Allowed {
				t.Fatalf("EvaluateDecision() Allowed = true; want false for %s", tt.name)
			}
			if result.Code != tt.wantCode {
				t.Fatalf("EvaluateDecision() Code = %q; want %q", result.Code, tt.wantCode)
			}
		})
	}
}

func TestEvaluateDecisionRejectsUnsafeExecutionConditions(t *testing.T) {
	tests := []struct {
		name           string
		mutateDecision func(*Decision)
		mutateContext  func(*PolicyContext)
		wantCode       string
	}{
		{
			name: "confidence below policy threshold",
			mutateDecision: func(decision *Decision) {
				decision.Confidence = 0.80
			},
			wantCode: PolicyCodeLowConfidence,
		},
		{
			name: "high risk action",
			mutateDecision: func(decision *Decision) {
				decision.Risk = "high"
			},
			wantCode: PolicyCodeRiskDenied,
		},
		{
			name: "duplicate incident",
			mutateContext: func(policyContext *PolicyContext) {
				policyContext.Duplicate = true
			},
			wantCode: PolicyCodeDuplicate,
		},
		{
			name: "cooldown active",
			mutateContext: func(policyContext *PolicyContext) {
				policyContext.InCooldown = true
			},
			wantCode: PolicyCodeCooldown,
		},
		{
			name: "attempt limit reached",
			mutateContext: func(policyContext *PolicyContext) {
				policyContext.AttemptsInWindow = policyContext.MaximumAttempts
			},
			wantCode: PolicyCodeAttemptLimit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := policyTestDecision(ActionRestartPod)
			policyContext := policyTestContext()
			if tt.mutateDecision != nil {
				tt.mutateDecision(&decision)
			}
			if tt.mutateContext != nil {
				tt.mutateContext(&policyContext)
			}

			result := EvaluateDecision(decision, policyContext)

			if result.Allowed {
				t.Fatalf("EvaluateDecision() Allowed = true; want false for %s", tt.name)
			}
			if result.Code != tt.wantCode {
				t.Fatalf("EvaluateDecision() Code = %q; want %q", result.Code, tt.wantCode)
			}
		})
	}
}

func policyTestDecision(action string) Decision {
	return Decision{
		SchemaVersion: "v1",
		IncidentID:    "inc-policy-test",
		Action:        action,
		Target: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-test",
			UID:       "uid-policy-test",
		},
		Reason:           "The current evidence supports this candidate decision.",
		Evidence:         []string{"container:last_exit_code=1"},
		Confidence:       0.95,
		Risk:             "medium",
		RequiresApproval: action == ActionRestartPod,
	}
}

func policyTestContext() PolicyContext {
	return PolicyContext{
		IncidentID: "inc-policy-test",
		CurrentTarget: DecisionTarget{
			Cluster:   "dev",
			Namespace: "default",
			Kind:      "Pod",
			Name:      "crash-app-test",
			UID:       "uid-policy-test",
		},
		AllowedNamespaces:      []string{"default"},
		ControllerOwnerKind:    "ReplicaSet",
		AllowedControllerKinds: []string{"ReplicaSet"},
		ApprovalGranted:        true,
		MinimumConfidence:      0.90,
		AttemptsInWindow:       0,
		MaximumAttempts:        1,
	}
}
