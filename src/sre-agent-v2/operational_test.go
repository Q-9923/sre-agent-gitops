package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOperationalHandlerServesMetrics(t *testing.T) {
	const contentType = "text/plain; version=0.0.4; charset=utf-8"
	const metricsBody = "# TYPE sre_agent_test_metric gauge\n" +
		"sre_agent_test_metric 1\n"

	metricsHandler := http.HandlerFunc(func(
		response http.ResponseWriter,
		_ *http.Request,
	) {
		response.Header().Set("Content-Type", contentType)
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(metricsBody))
	})

	handler := newOperationalHandler(
		func() bool {
			return true
		},
		func() bool {
			return true
		},
		metricsHandler,
	)

	request := httptest.NewRequest(
		http.MethodGet,
		"/metrics",
		nil,
	)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf(
			"status = %d; want %d",
			response.Code,
			http.StatusOK,
		)
	}
	if contentTypeValue := response.Header().Get("Content-Type"); contentTypeValue != contentType {
		t.Fatalf(
			"Content-Type = %q; want %q",
			contentTypeValue,
			contentType,
		)
	}
	if body := response.Body.String(); body != metricsBody {
		t.Fatalf(
			"body = %q; want %q",
			body,
			metricsBody,
		)
	}
}
