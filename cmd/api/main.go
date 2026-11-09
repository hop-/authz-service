package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"authz-service/internal/app"
	"authz-service/internal/config"

	_ "authz-service/docs"

	"github.com/hop-/goconfig"
)

func main() {
	slog.SetDefault(app.NewLogger(os.Stdout, app.ServiceAPI))

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// ctx is cancelled on SIGTERM/SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := app.RunAPI(ctx, cfg); err != nil {
		slog.Error(app.ServiceAPI+" failed", "error", err)
		stop()
		os.Exit(1)
	}
}

func loadConfig() (*config.Config, error) {
	if err := goconfig.Load(); err != nil {
		return nil, err
	}
	var cfg config.Config
	if err := goconfig.Extract("", &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
