package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"kronwrk/internal/config"
	"kronwrk/internal/db"
	"kronwrk/internal/models"
)

// The session caches config and one shared pgx pool for the whole process.
// One-shot commands behave as before (open once, closed in main); the shell
// runs many commands over the same pool instead of reconnecting per line.
var (
	sessionCfg   *config.Config
	sessionLog   *slog.Logger
	sessionPool  *pgxpool.Pool
	sessionStore *db.Store
	sessionUser  *models.DBUser
)

// getConfig loads config and the logger once per process.
func getConfig() (config.Config, *slog.Logger, error) {
	if sessionCfg != nil {
		return *sessionCfg, sessionLog, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, nil, err
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	sessionCfg, sessionLog = &cfg, log
	return cfg, log, nil
}

// getStore opens the shared pool on first use.
func getStore(ctx context.Context) (*db.Store, error) {
	if sessionStore != nil {
		return sessionStore, nil
	}
	cfg, _, err := getConfig()
	if err != nil {
		return nil, err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	sessionPool, sessionStore = pool, db.NewStore(pool)
	return sessionStore, nil
}

// currentUser returns the connected database user, cached for the life of the
// session so the shell prompt, help dimming, and per-command authorization
// share one WhoAmI round-trip. Invalidated by closeSession (logout).
func currentUser(ctx context.Context) (models.DBUser, error) {
	if sessionUser != nil {
		return *sessionUser, nil
	}
	store, err := getStore(ctx)
	if err != nil {
		return models.DBUser{}, err
	}
	u, err := store.WhoAmI(ctx)
	if err != nil {
		return models.DBUser{}, err
	}
	sessionUser = &u
	return u, nil
}

// closeSession closes the shared pool; called once when the process exits.
func closeSession() {
	if sessionPool != nil {
		sessionPool.Close()
		sessionPool, sessionStore = nil, nil
	}
	sessionUser = nil
}
