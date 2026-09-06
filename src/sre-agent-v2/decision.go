package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	ActionIgnore     = "IGNORE"
	ActionRestartPod = "RESTART_POD"
	ActionEscalate   = "ESCALATE"
)

// DecisionTarget identifies the Kubernetes object considered by the model.
type DecisionTarget struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// Decision is the structured result returned by the model.
type Decision struct {
	SchemaVersion    string         `json:"schema_version"`
	IncidentID       string         `json:"incident_id"`
	Action           string         `json:"action"`
	Target           DecisionTarget `json:"target"`
	Reason           string         `json:"reason"`
	Evidence         []string       `json:"evidence"`
	Confidence       float64        `json:"confidence"`
	Risk             string         `json:"risk"`
	RequiresApproval bool           `json:"requires_approval"`
}

// ParseDecision rejects unstructured LLM output. Operational actions must
// never be inferred from words appearing in natural-language explanations.
func ParseDecision(raw string) (Decision, error) {
	var decision Decision
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decision); err != nil {
		return Decision{}, fmt.Errorf("parse decision JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Decision{}, fmt.Errorf("parse decision JSON: trailing JSON value is not allowed")
		}
		return Decision{}, fmt.Errorf("parse decision JSON: trailing content: %w", err)
	}
	if err := decision.Validate(); err != nil {
		return Decision{}, fmt.Errorf("validate decision: %w", err)
	}
	return decision, nil
}

// Validate enforces the v1 decision contract before policy evaluation.
func (d Decision) Validate() error {
	if d.SchemaVersion != "v1" {
		return fmt.Errorf("unsupported schema_version %q", d.SchemaVersion)
	}
	if strings.TrimSpace(d.IncidentID) == "" {
		return fmt.Errorf("incident_id is required")
	}

	switch d.Action {
	case ActionIgnore, ActionRestartPod, ActionEscalate:
	default:
		return fmt.Errorf("unsupported action %q", d.Action)
	}

	if strings.TrimSpace(d.Target.Cluster) == "" {
		return fmt.Errorf("target.cluster is required")
	}
	if strings.TrimSpace(d.Target.Namespace) == "" {
		return fmt.Errorf("target.namespace is required")
	}
	if d.Target.Kind != "Pod" {
		return fmt.Errorf("unsupported target.kind %q", d.Target.Kind)
	}
	if strings.TrimSpace(d.Target.Name) == "" {
		return fmt.Errorf("target.name is required")
	}
	if strings.TrimSpace(d.Target.UID) == "" {
		return fmt.Errorf("target.uid is required")
	}

	if strings.TrimSpace(d.Reason) == "" {
		return fmt.Errorf("reason is required")
	}
	if len(d.Evidence) == 0 {
		return fmt.Errorf("at least one evidence item is required")
	}
	for i, evidence := range d.Evidence {
		if strings.TrimSpace(evidence) == "" {
			return fmt.Errorf("evidence[%d] must not be empty", i)
		}
	}

	if d.Confidence < 0 || d.Confidence > 1 {
		return fmt.Errorf("confidence must be between 0 and 1")
	}

	switch d.Risk {
	case "low", "medium", "high":
	default:
		return fmt.Errorf("unsupported risk %q", d.Risk)
	}

	if d.Action == ActionRestartPod && !d.RequiresApproval {
		return fmt.Errorf("RESTART_POD requires approval")
	}

	return nil
}
