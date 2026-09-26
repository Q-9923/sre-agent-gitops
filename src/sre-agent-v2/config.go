package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type agentConfig struct {
	ClusterName                   string
	PrometheusURL                 string
	OllamaURL                     string
	OllamaModel                   string
	AllowedNamespaces             []string
	AllowedControllerKinds        []string
	RestartPodApproved            bool
	MinimumConfidence             float64
	PollInterval                  time.Duration
	ReadinessStaleAfter           time.Duration
	LivenessStaleAfter            time.Duration
	RestartCooldown               time.Duration
	AttemptWindow                 time.Duration
	MaximumAttempts               int
	PrometheusTimeout             time.Duration
	OllamaTimeout                 time.Duration
	KubernetesRequestTimeout      time.Duration
	IncidentStoreConnectTimeout   time.Duration
	IncidentStoreMigrationTimeout time.Duration
	IncidentStoreBackend          string
	IncidentStorePostgresDSN      string
	RemediationStateNamespace     string
	RemediationStateConfigMap     string
	HealthAddress                 string
}

func loadConfig() (agentConfig, error) {
	config := agentConfig{
		ClusterName:                   envOrDefault("SRE_CLUSTER_NAME", "dev"),
		PrometheusURL:                 envOrDefault("PROMETHEUS_URL", "http://prometheus-stack-kube-prom-prometheus.monitoring.svc:9090"),
		OllamaURL:                     envOrDefault("OLLAMA_URL", "http://ollama-service.ai-services.svc:11434"),
		OllamaModel:                   envOrDefault("OLLAMA_MODEL", "qwen2.5:1.5b"),
		AllowedNamespaces:             splitCSV(envOrDefault("ALLOWED_NAMESPACES", "default")),
		AllowedControllerKinds:        splitCSV(envOrDefault("ALLOWED_CONTROLLER_KINDS", "ReplicaSet")),
		RestartPodApproved:            false,
		MinimumConfidence:             0.90,
		PollInterval:                  30 * time.Second,
		ReadinessStaleAfter:           5 * time.Minute,
		LivenessStaleAfter:            10 * time.Minute,
		RestartCooldown:               10 * time.Minute,
		AttemptWindow:                 time.Hour,
		MaximumAttempts:               1,
		PrometheusTimeout:             10 * time.Second,
		OllamaTimeout:                 90 * time.Second,
		KubernetesRequestTimeout:      15 * time.Second,
		IncidentStoreConnectTimeout:   10 * time.Second,
		IncidentStoreMigrationTimeout: 30 * time.Second,
		IncidentStoreBackend:          envOrDefault("INCIDENT_STORE_BACKEND", "memory"),
		IncidentStorePostgresDSN:      envOrDefault("INCIDENT_STORE_POSTGRES_DSN", ""),
		RemediationStateNamespace:     envOrDefault("REMEDIATION_STATE_NAMESPACE", "sre-agent-system"),
		RemediationStateConfigMap:     envOrDefault("REMEDIATION_STATE_CONFIGMAP", "sre-agent-v2-state"),
		HealthAddress:                 envOrDefault("HEALTH_ADDRESS", ":8080"),
	}

	var err error
	if config.RestartPodApproved, err = envBool("RESTART_POD_APPROVED", config.RestartPodApproved); err != nil {
		return agentConfig{}, err
	}
	if config.MinimumConfidence, err = envFloat("MINIMUM_CONFIDENCE", config.MinimumConfidence); err != nil {
		return agentConfig{}, err
	}
	if config.MaximumAttempts, err = envInt("MAXIMUM_ATTEMPTS", config.MaximumAttempts); err != nil {
		return agentConfig{}, err
	}
	for name, destination := range map[string]*time.Duration{
		"POLL_INTERVAL":                    &config.PollInterval,
		"READINESS_STALE_AFTER":            &config.ReadinessStaleAfter,
		"LIVENESS_STALE_AFTER":             &config.LivenessStaleAfter,
		"RESTART_COOLDOWN":                 &config.RestartCooldown,
		"ATTEMPT_WINDOW":                   &config.AttemptWindow,
		"PROMETHEUS_TIMEOUT":               &config.PrometheusTimeout,
		"OLLAMA_TIMEOUT":                   &config.OllamaTimeout,
		"KUBERNETES_REQUEST_TIMEOUT":       &config.KubernetesRequestTimeout,
		"INCIDENT_STORE_CONNECT_TIMEOUT":   &config.IncidentStoreConnectTimeout,
		"INCIDENT_STORE_MIGRATION_TIMEOUT": &config.IncidentStoreMigrationTimeout,
	} {
		if *destination, err = envDuration(name, *destination); err != nil {
			return agentConfig{}, err
		}
	}

	if err := config.validate(); err != nil {
		return agentConfig{}, err
	}
	return config, nil
}

func (config agentConfig) validate() error {
	if strings.TrimSpace(config.ClusterName) == "" {
		return fmt.Errorf("SRE_CLUSTER_NAME must not be empty")
	}
	switch config.IncidentStoreBackend {
	case "memory", "postgres":
	default:
		return fmt.Errorf(
			"INCIDENT_STORE_BACKEND must be memory or postgres",
		)
	}
	if config.IncidentStoreBackend == "postgres" &&
		strings.TrimSpace(config.IncidentStorePostgresDSN) == "" {
		return fmt.Errorf(
			"INCIDENT_STORE_POSTGRES_DSN must not be empty " +
				"when INCIDENT_STORE_BACKEND is postgres",
		)
	}
	if strings.TrimSpace(config.OllamaModel) == "" {
		return fmt.Errorf("OLLAMA_MODEL must not be empty")
	}
	if strings.TrimSpace(config.RemediationStateNamespace) == "" {
		return fmt.Errorf(
			"REMEDIATION_STATE_NAMESPACE must not be empty",
		)
	}
	if strings.TrimSpace(config.RemediationStateConfigMap) == "" {
		return fmt.Errorf(
			"REMEDIATION_STATE_CONFIGMAP must not be empty",
		)
	}
	if _, _, err := net.SplitHostPort(config.HealthAddress); err != nil {
		return fmt.Errorf(
			"HEALTH_ADDRESS must use host:port format such as :8080: %w",
			err,
		)
	}
	for name, value := range map[string]string{
		"PROMETHEUS_URL": config.PrometheusURL,
		"OLLAMA_URL":     config.OllamaURL,
	} {
		parsed, err := url.ParseRequestURI(value)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("%s must be an absolute http(s) URL", name)
		}
	}
	if len(config.AllowedNamespaces) == 0 {
		return fmt.Errorf("ALLOWED_NAMESPACES must contain at least one namespace")
	}
	if len(config.AllowedControllerKinds) == 0 {
		return fmt.Errorf("ALLOWED_CONTROLLER_KINDS must contain at least one kind")
	}
	if config.MinimumConfidence < 0 || config.MinimumConfidence > 1 {
		return fmt.Errorf("MINIMUM_CONFIDENCE must be between 0 and 1")
	}
	if config.MaximumAttempts <= 0 {
		return fmt.Errorf("MAXIMUM_ATTEMPTS must be greater than zero")
	}
	for name, value := range map[string]time.Duration{
		"POLL_INTERVAL":                    config.PollInterval,
		"READINESS_STALE_AFTER":            config.ReadinessStaleAfter,
		"LIVENESS_STALE_AFTER":             config.LivenessStaleAfter,
		"RESTART_COOLDOWN":                 config.RestartCooldown,
		"ATTEMPT_WINDOW":                   config.AttemptWindow,
		"PROMETHEUS_TIMEOUT":               config.PrometheusTimeout,
		"OLLAMA_TIMEOUT":                   config.OllamaTimeout,
		"KUBERNETES_REQUEST_TIMEOUT":       config.KubernetesRequestTimeout,
		"INCIDENT_STORE_CONNECT_TIMEOUT":   config.IncidentStoreConnectTimeout,
		"INCIDENT_STORE_MIGRATION_TIMEOUT": config.IncidentStoreMigrationTimeout,
	} {
		if value <= 0 {
			return fmt.Errorf("%s must be greater than zero", name)
		}
	}
	if config.LivenessStaleAfter <= config.PollInterval {
		return fmt.Errorf(
			"LIVENESS_STALE_AFTER must be greater than POLL_INTERVAL",
		)
	}
	return nil
}

func envOrDefault(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return strings.TrimSpace(value)
	}
	return fallback
}

func envBool(name string, fallback bool) (bool, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}

func envFloat(name string, fallback float64) (float64, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number: %w", name, err)
	}
	return parsed, nil
}

func envInt(name string, fallback int) (int, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration such as 30s or 10m: %w", name, err)
	}
	return parsed, nil
}

func splitCSV(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}
