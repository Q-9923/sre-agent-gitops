package main

import (
	"testing"
	"time"
)

func TestLoadConfigReadsHealthAddress(t *testing.T) {
	t.Setenv("HEALTH_ADDRESS", "127.0.0.1:18080")

	config, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v; want nil", err)
	}

	const expected = "127.0.0.1:18080"
	if config.HealthAddress != expected {
		t.Fatalf(
			"HealthAddress = %q; want %q",
			config.HealthAddress,
			expected,
		)
	}
}

func TestLoadConfigRejectsInvalidHealthAddress(t *testing.T) {
	t.Setenv("HEALTH_ADDRESS", "not-an-address")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig() accepted an invalid HEALTH_ADDRESS; want an error")
	}
}

func TestLoadConfigReadsReadinessStaleAfter(t *testing.T) {
	t.Setenv("READINESS_STALE_AFTER", "2m")

	config, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v; want nil", err)
	}

	if config.ReadinessStaleAfter != 2*time.Minute {
		t.Fatalf(
			"ReadinessStaleAfter = %s; want %s",
			config.ReadinessStaleAfter,
			2*time.Minute,
		)
	}
}

func TestLoadConfigReadsLivenessStaleAfter(t *testing.T) {
	t.Setenv("LIVENESS_STALE_AFTER", "7m")

	config, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v; want nil", err)
	}

	if config.LivenessStaleAfter != 7*time.Minute {
		t.Fatalf(
			"LivenessStaleAfter = %s; want %s",
			config.LivenessStaleAfter,
			7*time.Minute,
		)
	}
}
func TestLoadConfigRejectsLivenessStaleAfterNotGreaterThanPollInterval(t *testing.T) {
	tests := []struct {
		name               string
		pollInterval       string
		livenessStaleAfter string
	}{
		{
			name:               "shorter",
			pollInterval:       "1h",
			livenessStaleAfter: "10s",
		},
		{
			name:               "equal",
			pollInterval:       "30s",
			livenessStaleAfter: "30s",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("POLL_INTERVAL", test.pollInterval)
			t.Setenv(
				"LIVENESS_STALE_AFTER",
				test.livenessStaleAfter,
			)

			_, err := loadConfig()
			if err == nil {
				t.Fatalf(
					"loadConfig() accepted POLL_INTERVAL=%s and "+
						"LIVENESS_STALE_AFTER=%s; want an error",
					test.pollInterval,
					test.livenessStaleAfter,
				)
			}
		})
	}
}
func TestLoadConfigReadsRemediationStateLocation(t *testing.T) {
	t.Setenv(
		"REMEDIATION_STATE_NAMESPACE",
		"custom-agent-system",
	)
	t.Setenv(
		"REMEDIATION_STATE_CONFIGMAP",
		"custom-remediation-state",
	)

	config, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v; want nil", err)
	}

	if config.RemediationStateNamespace != "custom-agent-system" {
		t.Fatalf(
			"RemediationStateNamespace = %q; want %q",
			config.RemediationStateNamespace,
			"custom-agent-system",
		)
	}
	if config.RemediationStateConfigMap != "custom-remediation-state" {
		t.Fatalf(
			"RemediationStateConfigMap = %q; want %q",
			config.RemediationStateConfigMap,
			"custom-remediation-state",
		)
	}
}
func TestLoadConfigRejectsEmptyRemediationStateLocation(t *testing.T) {
	tests := []struct {
		name            string
		environmentName string
	}{
		{
			name:            "namespace",
			environmentName: "REMEDIATION_STATE_NAMESPACE",
		},
		{
			name:            "ConfigMap name",
			environmentName: "REMEDIATION_STATE_CONFIGMAP",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.environmentName, " ")

			_, err := loadConfig()
			if err == nil {
				t.Fatalf(
					"loadConfig() accepted an empty %s; want an error",
					test.environmentName,
				)
			}
		})
	}
}
func TestLoadConfigDefaultsIncidentStoreBackendToMemory(t *testing.T) {
	config, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v; want nil", err)
	}

	const expected = "memory"
	if config.IncidentStoreBackend != expected {
		t.Fatalf(
			"IncidentStoreBackend = %q; want %q",
			config.IncidentStoreBackend,
			expected,
		)
	}
}
func TestLoadConfigReadsIncidentStoreBackend(t *testing.T) {
	t.Setenv("INCIDENT_STORE_BACKEND", "postgres")
	t.Setenv(
		"INCIDENT_STORE_POSTGRES_DSN",
		"postgres://db.example.invalid/sre_agent",
	)

	config, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v; want nil", err)
	}

	const expected = "postgres"
	if config.IncidentStoreBackend != expected {
		t.Fatalf(
			"IncidentStoreBackend = %q; want %q",
			config.IncidentStoreBackend,
			expected,
		)
	}
}
func TestLoadConfigRejectsUnsupportedIncidentStoreBackend(t *testing.T) {
	t.Setenv("INCIDENT_STORE_BACKEND", "sqlite")

	_, err := loadConfig()
	if err == nil {
		t.Fatal(
			"loadConfig() accepted unsupported INCIDENT_STORE_BACKEND; want an error",
		)
	}
}

func TestLoadConfigRejectsPostgresBackendWithoutDSN(t *testing.T) {
	t.Setenv("INCIDENT_STORE_BACKEND", "postgres")
	t.Setenv("INCIDENT_STORE_POSTGRES_DSN", "")

	_, err := loadConfig()
	if err == nil {
		t.Fatal(
			"loadConfig() accepted postgres backend without " +
				"INCIDENT_STORE_POSTGRES_DSN; want an error",
		)
	}
}
func TestLoadConfigReadsIncidentStoreConnectTimeout(t *testing.T) {
	t.Setenv(
		"INCIDENT_STORE_CONNECT_TIMEOUT",
		"3s",
	)

	config, err := loadConfig()
	if err != nil {
		t.Fatalf(
			"loadConfig() error = %v; want nil",
			err,
		)
	}

	const expected = 3 * time.Second
	if config.IncidentStoreConnectTimeout != expected {
		t.Fatalf(
			"IncidentStoreConnectTimeout = %s; want %s",
			config.IncidentStoreConnectTimeout,
			expected,
		)
	}
}
func TestLoadConfigReadsIncidentStoreMigrationTimeout(t *testing.T) {
	t.Setenv(
		"INCIDENT_STORE_MIGRATION_TIMEOUT",
		"45s",
	)

	config, err := loadConfig()
	if err != nil {
		t.Fatalf(
			"loadConfig() error = %v; want nil",
			err,
		)
	}

	const expected = 45 * time.Second
	if config.IncidentStoreMigrationTimeout != expected {
		t.Fatalf(
			"IncidentStoreMigrationTimeout = %s; want %s",
			config.IncidentStoreMigrationTimeout,
			expected,
		)
	}
}
