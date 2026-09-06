package main

import (
	"encoding/json"
	"testing"
)

func TestParseDecisionRejectsNaturalLanguageContainingRestart(t *testing.T) {
	_, err := ParseDecision("You should not restart this pod.")
	if err == nil {
		t.Fatal("ParseDecision() accepted natural language containing restart; want a safe parse error")
	}
}

func TestParseDecisionAcceptsValidIgnoreJSON(t *testing.T) {
	raw := `{
		"schema_version":"v1",
		"incident_id":"inc-test-001",
		"action":"IGNORE",
		"target":{
			"cluster":"dev",
			"namespace":"default",
			"kind":"Pod",
			"name":"crash-app-test",
			"uid":"uid-test-001"
		},
		"reason":"The current evidence does not justify an automated action.",
		"evidence":["container:last_exit_code=1"],
		"confidence":0.90,
		"risk":"low",
		"requires_approval":false
	}`

	decision, err := ParseDecision(raw)
	if err != nil {
		t.Fatalf("ParseDecision() error = %v; want nil", err)
	}
	if decision.SchemaVersion != "v1" {
		t.Fatalf("SchemaVersion = %q; want v1", decision.SchemaVersion)
	}
	if decision.Action != "IGNORE" {
		t.Fatalf("Action = %q; want IGNORE", decision.Action)
	}
	if decision.Target.UID != "uid-test-001" {
		t.Fatalf("Target.UID = %q; want uid-test-001", decision.Target.UID)
	}
}

func TestParseDecisionRejectsUnknownFields(t *testing.T) {
	raw := `{
		"schema_version":"v1",
		"incident_id":"inc-test-002",
		"action":"IGNORE",
		"target":{
			"cluster":"dev",
			"namespace":"default",
			"kind":"Pod",
			"name":"crash-app-test",
			"uid":"uid-test-002"
		},
		"reason":"The current evidence does not justify an automated action.",
		"evidence":["container:last_exit_code=1"],
		"confidence":0.90,
		"risk":"low",
		"requires_approval":false,
		"unexpected_action":"RESTART_POD"
	}`

	_, err := ParseDecision(raw)
	if err == nil {
		t.Fatal("ParseDecision() accepted an unknown field; want a safe parse error")
	}
}

func TestParseDecisionRejectsTrailingJSON(t *testing.T) {
	valid, err := json.Marshal(validDecisionFixture())
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	raw := string(valid) + ` {"action":"RESTART_POD"}`
	if _, err := ParseDecision(raw); err == nil {
		t.Fatal("ParseDecision() accepted trailing JSON; want a safe parse error")
	}
}

func TestParseDecisionRejectsUnsafeDecisions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "unsupported schema version",
			mutate: func(decision map[string]any) {
				decision["schema_version"] = "v2"
			},
		},
		{
			name: "missing incident id",
			mutate: func(decision map[string]any) {
				delete(decision, "incident_id")
			},
		},
		{
			name: "unsupported action",
			mutate: func(decision map[string]any) {
				decision["action"] = "DELETE_EVERYTHING"
			},
		},
		{
			name: "missing target uid",
			mutate: func(decision map[string]any) {
				decision["target"].(map[string]any)["uid"] = ""
			},
		},
		{
			name: "empty reason",
			mutate: func(decision map[string]any) {
				decision["reason"] = ""
			},
		},
		{
			name: "empty evidence",
			mutate: func(decision map[string]any) {
				decision["evidence"] = []string{}
			},
		},
		{
			name: "confidence above one",
			mutate: func(decision map[string]any) {
				decision["confidence"] = 1.01
			},
		},
		{
			name: "unsupported risk",
			mutate: func(decision map[string]any) {
				decision["risk"] = "extreme"
			},
		},
		{
			name: "restart without approval",
			mutate: func(decision map[string]any) {
				decision["action"] = "RESTART_POD"
				decision["risk"] = "medium"
				decision["requires_approval"] = false
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := validDecisionFixture()
			tt.mutate(fixture)

			raw, err := json.Marshal(fixture)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}

			if _, err := ParseDecision(string(raw)); err == nil {
				t.Fatal("ParseDecision() accepted an unsafe decision; want a validation error")
			}
		})
	}
}

func validDecisionFixture() map[string]any {
	return map[string]any{
		"schema_version": "v1",
		"incident_id":    "inc-test-safe",
		"action":         "IGNORE",
		"target": map[string]any{
			"cluster":   "dev",
			"namespace": "default",
			"kind":      "Pod",
			"name":      "crash-app-test",
			"uid":       "uid-test-safe",
		},
		"reason":            "The current evidence does not justify an automated action.",
		"evidence":          []string{"container:last_exit_code=1"},
		"confidence":        0.90,
		"risk":              "low",
		"requires_approval": false,
	}
}
