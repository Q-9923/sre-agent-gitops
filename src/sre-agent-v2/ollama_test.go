package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestOllamaClientDecideRequestsSchemaAndParsesDecision(t *testing.T) {
	wantDecision := policyTestDecision(ActionRestartPod)
	wantDecision.IncidentID = "inc-ollama-test"
	decisionJSON, err := json.Marshal(wantDecision)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/generate" {
			t.Errorf("request path = %q; want /api/generate", request.URL.Path)
		}

		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode Ollama request: %v", err)
		}
		if body["model"] != "qwen-test" {
			t.Errorf("model = %q; want qwen-test", body["model"])
		}
		if body["stream"] != false {
			t.Errorf("stream = %v; want false", body["stream"])
		}
		if body["think"] != false {
			t.Errorf("think = %v; want false", body["think"])
		}
		format, ok := body["format"].(map[string]any)
		if !ok || format["type"] != "object" || format["additionalProperties"] != false {
			t.Errorf("format = %#v; want a strict object JSON schema", body["format"])
		}
		prompt, _ := body["prompt"].(string)
		if !strings.Contains(prompt, "inc-ollama-test") || !strings.Contains(prompt, "uid-policy-test") {
			t.Errorf("prompt does not contain the current incident and pod UID: %q", prompt)
		}

		responseWriter.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(responseWriter).Encode(map[string]any{
			"response": string(decisionJSON),
			"done":     true,
		}); err != nil {
			t.Fatalf("encode Ollama response: %v", err)
		}
	}))
	defer server.Close()

	evidence := IncidentEvidence{
		IncidentID: "inc-ollama-test",
		Alert: AlertEvidence{
			Name:        "PodCrashLooping",
			Description: "Pod is in CrashLoopBackOff.",
		},
		Pod: PodEvidence{
			Target: wantDecision.Target,
		},
	}
	client := newOllamaClient(server.URL, "qwen-test", server.Client())

	decision, err := client.decide(context.Background(), evidence)
	if err != nil {
		t.Fatalf("decide() error = %v; want nil", err)
	}
	if !reflect.DeepEqual(decision, wantDecision) {
		t.Fatalf("decide() = %#v; want %#v", decision, wantDecision)
	}
}
