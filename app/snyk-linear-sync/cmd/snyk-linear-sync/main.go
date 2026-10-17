package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tesslio/snyk-linear-sync/internal/cache"
	"github.com/tesslio/snyk-linear-sync/internal/config"
	linearclient "github.com/tesslio/snyk-linear-sync/internal/linear"
	"github.com/tesslio/snyk-linear-sync/internal/logx"
	snykclient "github.com/tesslio/snyk-linear-sync/internal/snyk"
	syncsvc "github.com/tesslio/snyk-linear-sync/internal/sync"
)

func main() {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	logger, closeLogger, err := newLogger(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer closeLogger()

	if err := run(cfg, logger); err != nil {
		logger.Error("sync failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(cfg config.Config, logger *slog.Logger) error {
	ctx := context.Background()

	snyk, err := snykclient.New(ctx, cfg, logger.With("service", "snyk"))
	if err != nil {
		return err
	}

	cacheStore, err := cache.Open(cfg.Cache.DBFile)
	if err != nil {
		return err
	}
	defer cacheStore.Close()
	snyk.SetCache(cacheStore)

	linear := linearclient.New(cfg.Linear, cfg.Sync.LinearConcurrency, logger.With("service", "linear"))
	service := syncsvc.New(cfg, logger.With("service", "sync"), snyk, linear, cacheStore)

	result, err := service.Run(ctx)
	if err != nil {
		return err
	}

	logger.Info("sync complete",
		slog.Bool("dry_run", cfg.DryRun),
		slog.Int("findings", result.Findings),
		slog.Int("existing_issues", result.ExistingIssues),
		slog.Int("active_projects", result.ActiveProjects),
		slog.Int("inactive_projects", result.InactiveProjects),
		slog.Int("cluster_lookup_failures", result.ClusterLookupFailures),
		slog.Int("conflicts", result.Conflicts),
		slog.Int64("rebound", result.Rebound),
		slog.Int64("reopened", result.Reopened),
		slog.Int64("deferred_creates", result.DeferredCreates),
		slog.Int64("planned_creates", result.PlannedCreates),
		slog.Int64("planned_updates", result.PlannedUpdates),
		slog.Int64("planned_resolves", result.PlannedResolves),
		slog.Int64("cancelled_duplicates", result.CancelledDuplicates),
		slog.Int64("failed_ops", result.FailedOps),
	)

	return nil
}

func newLogger(cfg config.Config) (*slog.Logger, func() error, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.Log.ErrorFile), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log directory: %w", err)
	}

	logFile, err := os.OpenFile(cfg.Log.ErrorFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open error log file %q: %w", cfg.Log.ErrorFile, err)
	}

	consoleHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	fileHandler := slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelError})
	logger := slog.New(logx.NewMultiHandler(consoleHandler, fileHandler))

	return logger, logFile.Close, nil
}
