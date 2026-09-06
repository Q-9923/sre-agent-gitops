package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxPrometheusResponseBytes = 4 << 20

type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"`
	ActiveAt    time.Time         `json:"activeAt"`
}

type prometheusAlertsResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType,omitempty"`
	Error     string `json:"error,omitempty"`
	Data      struct {
		Alerts []Alert `json:"alerts"`
	} `json:"data"`
}

type prometheusClient struct {
	baseURL    string
	httpClient *http.Client
}

func newPrometheusClient(baseURL string, httpClient *http.Client) *prometheusClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &prometheusClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
	}
}

func (client *prometheusClient) firingAlerts(ctx context.Context) ([]Alert, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+"/api/v1/alerts", nil)
	if err != nil {
		return nil, fmt.Errorf("create Prometheus request: %w", err)
	}

	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Prometheus: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxPrometheusResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Prometheus response: %w", err)
	}
	if len(body) > maxPrometheusResponseBytes {
		return nil, fmt.Errorf("Prometheus response exceeds %d bytes", maxPrometheusResponseBytes)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"Prometheus returned status %d: %s",
			response.StatusCode,
			truncateUTF8(strings.TrimSpace(string(body)), 2048),
		)
	}

	var alertResponse prometheusAlertsResponse
	if err := json.Unmarshal(body, &alertResponse); err != nil {
		return nil, fmt.Errorf("decode Prometheus response: %w", err)
	}
	if alertResponse.Status != "success" {
		return nil, fmt.Errorf(
			"Prometheus API error %s: %s",
			truncateUTF8(alertResponse.ErrorType, 128),
			truncateUTF8(alertResponse.Error, 2048),
		)
	}

	firing := make([]Alert, 0, len(alertResponse.Data.Alerts))
	for _, alert := range alertResponse.Data.Alerts {
		if alert.State == "firing" {
			firing = append(firing, alert)
		}
	}
	return firing, nil
}
