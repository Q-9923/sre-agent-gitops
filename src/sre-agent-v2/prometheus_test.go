package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPrometheusClientReturnsOnlyFiringAlerts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/alerts" {
			t.Errorf("request path = %q; want /api/v1/alerts", request.URL.Path)
		}
		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = responseWriter.Write([]byte(`{
			"status":"success",
			"data":{"alerts":[
				{"labels":{"alertname":"PodCrashLooping","namespace":"default","pod":"crash-a"},"annotations":{},"state":"firing","activeAt":"2026-09-02T10:00:00Z"},
				{"labels":{"alertname":"PodCrashLooping","namespace":"default","pod":"crash-b"},"annotations":{},"state":"pending","activeAt":"2026-09-02T10:00:00Z"}
			]}
		}`))
	}))
	defer server.Close()

	client := newPrometheusClient(server.URL, server.Client())
	alerts, err := client.firingAlerts(context.Background())
	if err != nil {
		t.Fatalf("firingAlerts() error = %v; want nil", err)
	}
	if len(alerts) != 1 || alerts[0].Labels["pod"] != "crash-a" {
		t.Fatalf("firingAlerts() = %#v; want only crash-a", alerts)
	}
}
