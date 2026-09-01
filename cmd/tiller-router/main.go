package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/privdrop"
	"github.com/tiller-router/tiller-router/internal/server"
	buildversion "github.com/tiller-router/tiller-router/internal/version"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("tiller-router stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	// Started as root (e.g. `user: "0:0"` so a fresh bind-mounted data
	// directory can be fixed up without host-side chown), hand the data
	// directory to the runtime user and drop privileges before touching the
	// database. Already non-root — the normal case — is a no-op. The
	// healthcheck subcommand never opens the database, so skip the walk.
	if command != "healthcheck" {
		dropped, err := privdrop.DropToRuntimeUser(cfg.DataDir)
		if err != nil {
			return err
		}
		if dropped {
			logger.Info("dropped privileges to runtime user", "uid", privdrop.DefaultUID, "gid", privdrop.DefaultGID)
		}
	}
	db, err := database.Open(ctx, filepath.Join(cfg.DataDir, "tiller-router.db"))
	if err != nil {
		if errors.Is(err, database.ErrDataDirUnwritable) {
			logger.Error(
				"data directory is not writable by the runtime user — the container runs as uid "+
					"65532 by default, but the bind-mounted directory is owned by someone else (a fresh "+
					"rootful-Docker bind mount is created as root). Fix ownership once, then up again.",
				"dir", cfg.DataDir,
				"uid", os.Getuid(),
				"fix", "sudo chown -R 65532:65532 ./data",
				"alt", "set TILLER_UID and TILLER_GID in .env to the uid:gid that owns ./data",
				"see", "README 'Create the data directory'",
			)
		}
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	switch command {
	case "migrate":
		return nil
	case "healthcheck":
		_, port, splitErr := net.SplitHostPort(cfg.ListenAddr)
		if splitErr != nil || port == "" {
			return fmt.Errorf("invalid listen address %q", cfg.ListenAddr)
		}
		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:" + port + "/health/ready")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("readiness returned %d", resp.StatusCode)
		}
		return nil
	case "serve":
	default:
		return fmt.Errorf("unknown command %q (expected serve, migrate, or healthcheck)", command)
	}
	logger.Info("tiller-router starting", "version", buildversion.Version, "commit", buildversion.Commit)
	app, err := server.New(cfg, db, logger)
	if err != nil {
		return err
	}
	runCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	app.StartBackground(runCtx)
	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("tiller-router listening", "addr", cfg.ListenAddr)
		errCh <- httpServer.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-runCtx.Done():
		shutdownCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
	}
	return nil
}
