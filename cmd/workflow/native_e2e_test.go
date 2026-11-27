//go:build e2e

package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	r1 "github.com/ssge/workflow/acceptance/release1"
	"github.com/ssge/workflow/internal/testdb"
)

func TestNativeE2E(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			connection := filepath.Join(t.TempDir(), "workflow.db")
			if backend == "postgres" {
				dsn := os.Getenv("WORKFLOW_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Fatal("PostgreSQL is required for native e2e; run make e2e")
				}
				connection = testdb.PostgresSchema(t, dsn)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			baseURL := "http://" + listener.Addr().String()
			t.Logf("native e2e server: %s", baseURL)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			done := make(chan error, 1)
			go func() {
				done <- serve(ctx, config{driver: backend, connection: connection, runners: 2,
					lease: 30 * time.Second, poll: 25 * time.Millisecond}, listener)
			}()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Errorf("workflow server: %v", err)
				}
			})
			client := &http.Client{Timeout: 10 * time.Second}
			driver, err := r1.NewNativeDriver(baseURL, client, backend, nil, r1.PollConfig{})
			if err != nil {
				t.Fatal(err)
			}
			for _, scenario := range r1.DefaultScenarios() {
				t.Run(scenario.Name, func(t *testing.T) {
					report := r1.RunScenarios(ctx, driver, []r1.Scenario{scenario})
					for _, outcome := range report.Outcomes {
						if outcome.Status != r1.StatusPass {
							t.Fatalf("%s: %s: %s (last state: %s, handles: %v)", outcome.Capability,
								outcome.Status, outcome.Detail, outcome.LastState, outcome.Handles)
						}
					}
				})
			}
		})
	}
}
