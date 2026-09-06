package main

const (
	PolicyCodeAllowed          = "ALLOWED"
	PolicyCodeInvalidDecision  = "INVALID_DECISION"
	PolicyCodeDecisionIgnore   = "DECISION_IGNORE"
	PolicyCodeDecisionEscalate = "DECISION_ESCALATE"
	PolicyCodeIncidentMismatch = "INCIDENT_MISMATCH"
	PolicyCodeTargetMismatch   = "TARGET_MISMATCH"
	PolicyCodeNamespaceDenied  = "NAMESPACE_DENIED"
	PolicyCodeOwnerDenied      = "OWNER_DENIED"
	PolicyCodeApprovalRequired = "APPROVAL_REQUIRED"
	PolicyCodeLowConfidence    = "LOW_CONFIDENCE"
	PolicyCodeRiskDenied       = "RISK_DENIED"
	PolicyCodeDuplicate        = "DUPLICATE_INCIDENT"
	PolicyCodeCooldown         = "COOLDOWN_ACTIVE"
	PolicyCodeAttemptLimit     = "ATTEMPT_LIMIT_REACHED"
)

// PolicyContext contains the observed Kubernetes state and policy controls
// used to authorize a model-proposed decision.
type PolicyContext struct {
	IncidentID             string
	CurrentTarget          DecisionTarget
	AllowedNamespaces      []string
	ControllerOwnerKind    string
	AllowedControllerKinds []string
	ApprovalGranted        bool
	MinimumConfidence      float64
	Duplicate              bool
	InCooldown             bool
	AttemptsInWindow       int
	MaximumAttempts        int
}

// PolicyResult is the side-effect-free authorization result consumed by the
// remediation layer.
type PolicyResult struct {
	Allowed bool
	Action  string
	Code    string
}

// EvaluateDecision authorizes a candidate decision without changing cluster
// state. Execution remains the caller's responsibility.
func EvaluateDecision(decision Decision, policyContext PolicyContext) PolicyResult {
	if err := decision.Validate(); err != nil {
		return deniedPolicyResult(decision.Action, PolicyCodeInvalidDecision)
	}

	switch decision.Action {
	case ActionIgnore:
		return deniedPolicyResult(decision.Action, PolicyCodeDecisionIgnore)
	case ActionEscalate:
		return deniedPolicyResult(decision.Action, PolicyCodeDecisionEscalate)
	}

	if decision.IncidentID != policyContext.IncidentID {
		return deniedPolicyResult(decision.Action, PolicyCodeIncidentMismatch)
	}
	if decision.Target != policyContext.CurrentTarget {
		return deniedPolicyResult(decision.Action, PolicyCodeTargetMismatch)
	}
	if !containsString(policyContext.AllowedNamespaces, decision.Target.Namespace) {
		return deniedPolicyResult(decision.Action, PolicyCodeNamespaceDenied)
	}
	if !containsString(policyContext.AllowedControllerKinds, policyContext.ControllerOwnerKind) {
		return deniedPolicyResult(decision.Action, PolicyCodeOwnerDenied)
	}
	if !policyContext.ApprovalGranted {
		return deniedPolicyResult(decision.Action, PolicyCodeApprovalRequired)
	}
	if decision.Confidence < policyContext.MinimumConfidence {
		return deniedPolicyResult(decision.Action, PolicyCodeLowConfidence)
	}
	if decision.Risk == "high" {
		return deniedPolicyResult(decision.Action, PolicyCodeRiskDenied)
	}
	if policyContext.Duplicate {
		return deniedPolicyResult(decision.Action, PolicyCodeDuplicate)
	}
	if policyContext.InCooldown {
		return deniedPolicyResult(decision.Action, PolicyCodeCooldown)
	}
	if policyContext.MaximumAttempts <= 0 || policyContext.AttemptsInWindow >= policyContext.MaximumAttempts {
		return deniedPolicyResult(decision.Action, PolicyCodeAttemptLimit)
	}

	return PolicyResult{
		Allowed: true,
		Action:  decision.Action,
		Code:    PolicyCodeAllowed,
	}
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func deniedPolicyResult(action, code string) PolicyResult {
	return PolicyResult{
		Allowed: false,
		Action:  action,
		Code:    code,
	}
}
