package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	release1 "github.com/ssge/workflow/acceptance/release1"
)

type backendReport struct {
	Backend string          `json:"backend"`
	Report  release1.Report `json:"report"`
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	scenarios := release1.DefaultScenarios()
	reports := make([]backendReport, 0, 2)

	nativeURL := strings.TrimSpace(os.Getenv("WORKFLOW_NATIVE_URL"))
	if nativeURL == "" {
		reports = append(reports, backendReport{
			Backend: "native",
			Report:  release1.NotConfiguredReport("native", "unconfigured", scenarios, "WORKFLOW_NATIVE_URL is not set"),
		})
	} else if strings.ToLower(strings.TrimSpace(os.Getenv("WORKFLOW_NATIVE_ISOLATED"))) != "true" {
		reports = append(reports, backendReport{
			Backend: "native",
			Report: release1.NotConfiguredReport("native", os.Getenv("WORKFLOW_NATIVE_VERSION"), scenarios,
				"WORKFLOW_NATIVE_ISOLATED=true is required for the fixed-key fixture"),
		})
	} else {
		version := os.Getenv("WORKFLOW_NATIVE_VERSION")
		interval, err := positiveDurationEnv("WORKFLOW_NATIVE_POLL_INTERVAL", 250*time.Millisecond)
		check(err)
		timeout, err := positiveDurationEnv("WORKFLOW_NATIVE_QUERY_TIMEOUT", 75*time.Second)
		check(err)
		queryLag, err := positiveDurationEnv("WORKFLOW_NATIVE_QUERY_LAG", 250*time.Millisecond)
		check(err)
		driver, err := release1.NewNativeDriver(nativeURL, &http.Client{Timeout: 30 * time.Second}, version, nil,
			release1.PollConfig{Interval: interval, Timeout: timeout, QueryLag: queryLag})
		check(err)
		reports = append(reports, backendReport{Backend: "native", Report: release1.RunScenarios(ctx, driver, scenarios)})
	}

	reference, err := release1.RunReferenceFromEnv(ctx, scenarios)
	check(err)
	reports = append(reports, backendReport{Backend: "camunda8", Report: reference})

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	check(encoder.Encode(reports))
	for _, report := range reports {
		for _, outcome := range report.Report.Outcomes {
			if outcome.Status != release1.StatusPass {
				os.Exit(1)
			}
		}
	}
}

func positiveDurationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s duration %q: %w", name, value, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return duration, nil
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
