package main

import (
	"strings"
	"testing"
)

func TestConfigFromEnv(t *testing.T) {
	values := map[string]string{
		"WORKFLOW_DATABASE_DRIVER":     "sqlite",
		"WORKFLOW_DATABASE_CONNECTION": "workflow.db",
	}
	cfg, err := configFromEnv(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.address != ":8080" || cfg.runners != 1 || cfg.lease.String() != "30s" || cfg.poll.String() != "250ms" {
		t.Fatalf("default configuration = %+v", cfg)
	}
	values["WORKFLOW_RUNNER_COUNT"] = "3"
	values["WORKFLOW_RUNNER_LEASE"] = "2s"
	values["WORKFLOW_RUNNER_POLL"] = "10ms"
	values["WORKFLOW_HTTP_ADDRESS"] = "127.0.0.1:9000"
	cfg, err = configFromEnv(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.runners != 3 || cfg.lease.String() != "2s" || cfg.poll.String() != "10ms" || cfg.address != "127.0.0.1:9000" {
		t.Fatalf("configured values = %+v", cfg)
	}
}

func TestConfigFromEnvRejectsInvalidSettings(t *testing.T) {
	base := map[string]string{
		"WORKFLOW_DATABASE_DRIVER":     "sqlite",
		"WORKFLOW_DATABASE_CONNECTION": "workflow.db",
	}
	for key, value := range map[string]string{
		"WORKFLOW_DATABASE_DRIVER":     "mysql",
		"WORKFLOW_DATABASE_CONNECTION": "",
		"WORKFLOW_RUNNER_COUNT":        "0",
		"WORKFLOW_RUNNER_LEASE":        "0s",
		"WORKFLOW_RUNNER_POLL":         "nope",
	} {
		t.Run(key+"_"+value, func(t *testing.T) {
			values := make(map[string]string, len(base)+1)
			for key, value := range base {
				values[key] = value
			}
			values[key] = value
			_, err := configFromEnv(func(key string) string { return values[key] })
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("error = %v, want explicit %s configuration error", err, key)
			}
		})
	}
}
