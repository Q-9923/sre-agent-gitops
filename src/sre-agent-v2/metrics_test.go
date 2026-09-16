package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentMetricsHandlerExposesGoRuntimeMetrics(t *testing.T) {
	metrics := newAgentMetrics()

	request := httptest.NewRequest(
		http.MethodGet,
		"/metrics",
		nil,
	)
	response := httptest.NewRecorder()

	metrics.handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf(
			"status = %d; want %d",
			response.Code,
			http.StatusOK,
		)
	}

	contentType := response.Header().Get("Content-Type")
	if !strings.HasPrefix(
		contentType,
		"text/plain; version=0.0.4",
	) {
		t.Fatalf(
			"Content-Type = %q; want Prometheus text format",
			contentType,
		)
	}

	body := response.Body.String()

	if !strings.Contains(body, "# HELP go_goroutines ") {
		t.Fatal(
			"metrics response does not contain go_goroutines HELP metadata",
		)
	}

	if !strings.Contains(body, "\ngo_goroutines ") {
		t.Fatal(
			"metrics response does not contain go_goroutines sample",
		)
	}
}

func TestAgentMetricsRecordsCyclesByResult(t *testing.T) {
	metrics := newAgentMetrics()

	metrics.recordCycle("NO_ACTION")
	metrics.recordCycle("NO_ACTION")
	metrics.recordCycle("ERROR")

	request := httptest.NewRequest(
		http.MethodGet,
		"/metrics",
		nil,
	)
	response := httptest.NewRecorder()

	metrics.handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf(
			"status = %d; want %d",
			response.Code,
			http.StatusOK,
		)
	}

	body := response.Body.String()

	expectedSamples := []string{
		`sre_agent_cycles_total{result="NO_ACTION"} 2`,
		`sre_agent_cycles_total{result="ERROR"} 1`,
	}

	for _, expected := range expectedSamples {
		if !strings.Contains(body, expected) {
			t.Fatalf(
				"metrics response does not contain %q:\n%s",
				expected,
				body,
			)
		}
	}
}

func TestAgentMetricsMapsUnknownCycleResultsToBoundedLabel(t *testing.T) {
	metrics := newAgentMetrics()

	metrics.recordCycle("INCIDENT_001")
	metrics.recordCycle("POD_CRASH_002")

	request := httptest.NewRequest(
		http.MethodGet,
		"/metrics",
		nil,
	)
	response := httptest.NewRecorder()

	metrics.handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf(
			"status = %d; want %d",
			response.Code,
			http.StatusOK,
		)
	}

	body := response.Body.String()

	unexpectedLabels := []string{
		`result="INCIDENT_001"`,
		`result="POD_CRASH_002"`,
	}

	for _, unexpected := range unexpectedLabels {
		if strings.Contains(body, unexpected) {
			t.Fatalf(
				"metrics response exposed an unbounded label %q:\n%s",
				unexpected,
				body,
			)
		}
	}

	expected := `sre_agent_cycles_total{result="UNKNOWN"} 2`
	if !strings.Contains(body, expected) {
		t.Fatalf(
			"metrics response does not contain %q:\n%s",
			expected,
			body,
		)
	}
}
