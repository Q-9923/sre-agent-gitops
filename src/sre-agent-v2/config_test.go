package main

import "testing"

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
