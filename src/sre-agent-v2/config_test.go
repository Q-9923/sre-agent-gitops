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
