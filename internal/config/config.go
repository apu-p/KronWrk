// Package config loads Kronwrk settings from environment variables.
//
// Per the design (env-based config, no Viper for v1), every value has a sane
// default so the binary runs locally with only DATABASE_URL set.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime settings for the scheduler, worker, and CLI.
type Config struct {
	DatabaseURL       string
	PollInterval      time.Duration
	WorkerConcurrency int
	HeartbeatInterval time.Duration
	JobTimeoutDefault time.Duration
	LogLevel          slog.Level
}

// Load reads configuration from the environment, applying defaults.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:       getenv("DATABASE_URL", "postgres://localhost:5432/kronwrk?sslmode=disable"),
		PollInterval:      getenvDuration("POLL_INTERVAL", 5*time.Second),
		WorkerConcurrency: getenvInt("WORKER_CONCURRENCY", 5),
		HeartbeatInterval: getenvDuration("HEARTBEAT_INTERVAL", 10*time.Second),
		JobTimeoutDefault: getenvDuration("JOB_TIMEOUT_DEFAULT", time.Hour),
		LogLevel:          getenvLevel("LOG_LEVEL", slog.LevelInfo),
	}
	// Each unit of concurrency is a goroutine holding a pooled DB connection, so
	// an absurd value is a self-inflicted resource-exhaustion / connection-storm
	// footgun rather than useful configuration. Bound it.
	const maxWorkerConcurrency = 512
	if cfg.WorkerConcurrency < 1 || cfg.WorkerConcurrency > maxWorkerConcurrency {
		return Config{}, fmt.Errorf("WORKER_CONCURRENCY must be between 1 and %d, got %d", maxWorkerConcurrency, cfg.WorkerConcurrency)
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func getenvLevel(key string, def slog.Level) slog.Level {
	switch os.Getenv(key) {
	case "debug", "DEBUG":
		return slog.LevelDebug
	case "info", "INFO":
		return slog.LevelInfo
	case "warn", "WARN":
		return slog.LevelWarn
	case "error", "ERROR":
		return slog.LevelError
	default:
		return def
	}
}
