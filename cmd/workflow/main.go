package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/ssge/workflow/db"
	"github.com/ssge/workflow/internal/clock"
	"github.com/ssge/workflow/internal/engine"
	"github.com/ssge/workflow/internal/httpapi"
)

type config struct {
	driver, connection, address string
	runners                     int
	lease, poll                 time.Duration
}

func main() {
	cfg, err := configFromEnv(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listener, err := net.Listen("tcp", cfg.address)
	if err != nil {
		log.Fatal(err)
	}
	if err := serve(ctx, cfg, listener); err != nil {
		log.Fatal(err)
	}
}

func configFromEnv(getenv func(string) string) (config, error) {
	cfg := config{
		driver:     getenv("WORKFLOW_DATABASE_DRIVER"),
		connection: getenv("WORKFLOW_DATABASE_CONNECTION"),
		address:    getenv("WORKFLOW_HTTP_ADDRESS"),
		runners:    1,
		lease:      30 * time.Second,
		poll:       250 * time.Millisecond,
	}
	if cfg.driver != "sqlite" && cfg.driver != "postgres" {
		return config{}, errors.New("WORKFLOW_DATABASE_DRIVER must be sqlite or postgres")
	}
	if cfg.connection == "" {
		return config{}, errors.New("WORKFLOW_DATABASE_CONNECTION is required")
	}
	if cfg.address == "" {
		cfg.address = ":8080"
	}
	if value := getenv("WORKFLOW_RUNNER_COUNT"); value != "" {
		count, err := strconv.Atoi(value)
		if err != nil || count < 1 {
			return config{}, errors.New("WORKFLOW_RUNNER_COUNT must be a positive integer")
		}
		cfg.runners = count
	}
	if value := getenv("WORKFLOW_RUNNER_LEASE"); value != "" {
		lease, err := time.ParseDuration(value)
		if err != nil || lease < time.Millisecond {
			return config{}, errors.New("WORKFLOW_RUNNER_LEASE must be at least 1ms")
		}
		cfg.lease = lease
	}
	if value := getenv("WORKFLOW_RUNNER_POLL"); value != "" {
		poll, err := time.ParseDuration(value)
		if err != nil || poll <= 0 {
			return config{}, errors.New("WORKFLOW_RUNNER_POLL must be positive")
		}
		cfg.poll = poll
	}
	return cfg, nil
}

func serve(ctx context.Context, cfg config, listener net.Listener) (result error) {
	defer listener.Close()
	database, err := db.Open(cfg.driver, cfg.connection)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	pool, err := database.DB()
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close database pool: %w", err))
		}
	}()
	if err := db.Migrate(ctx, database); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("create runner identity: %w", err)
	}
	workflowClock := clock.Real{}
	runtimeStore := db.NewRuntimeStore(database, func() int64 { return workflowClock.Now().UnixMilli() })
	workflowEngine := engine.New(runtimeStore, workflowClock)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	runnerErrors := make(chan error, cfg.runners)
	var runners sync.WaitGroup
	for i := 0; i < cfg.runners; i++ {
		runner, err := workflowEngine.NewRunner(fmt.Sprintf("%x-%d", nonce, i), cfg.lease)
		if err != nil {
			return fmt.Errorf("configure runner: %w", err)
		}
		runners.Add(1)
		go func() {
			defer runners.Done()
			if err := runner.Run(runCtx, cfg.poll); err != nil && !errors.Is(err, context.Canceled) {
				runnerErrors <- err
			}
		}()
	}

	server := &http.Server{
		Handler:           httpapi.NewHandler(workflowEngine),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()

	select {
	case <-ctx.Done():
	case err := <-serveErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			result = fmt.Errorf("serve HTTP: %w", err)
		}
	case err := <-runnerErrors:
		result = fmt.Errorf("runner stopped: %w", err)
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		result = errors.Join(result, fmt.Errorf("shut down HTTP server: %w", err))
	}
	runners.Wait()
	return result
}
