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
	cfg, err := config.Load()
	if err != nil {
		// Config validation precedes logging setup, so render the error with
		// a default-level handler rather than depending on the configured
		// level (which may itself be what failed to load).
		logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
		logger.Error("tiller-router stopped", "error", err.Error())
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(cfg.LogLevel)}))
	if err := run(cfg, logger); err != nil {
		logger.Error("tiller-router stopped", "error", err.Error())
		os.Exit(1)
	}
}

// parseLogLevel maps a config load-level string to a slog.Level. The string
// is validated in config.Load, so any value reaching here is one of
// debug/info/warn/error; a bogus value still degrades to warn for safety.
func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "info":
		return slog.LevelInfo
	default:
		return slog.LevelWarn
	}
}

func run(cfg config.Config, logger *slog.Logger) error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	ctx := context.Background()
	// Resolve the runtime identity up front so even the no-op (already
	// non-root) path can log and remediate with the correct UID/GID.
	runUID, runGID, err := privdrop.ResolvedIdentity()
	if err != nil {
		return err
	}
	// Started as root (e.g. `user: "0:0"` so a fresh bind-mounted data
	// directory can be fixed up without host-side chown), hand the data
	// directory to the runtime user and drop privileges before touching the
	// database. Already non-root — the normal case — is a no-op. The
	// healthcheck subcommand never opens the database, so skip the walk.
	if command != "healthcheck" {
		dropped, appliedUID, appliedGID, err := privdrop.DropToRuntimeUser(cfg.DataDir)
		if err != nil {
			return err
		}
		if dropped {
			logger.Info("dropped privileges to runtime user", "uid", appliedUID, "gid", appliedGID)
		}
		runUID, runGID = appliedUID, appliedGID
	}
	if command == "healthcheck" {
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
	}
	db, err := database.Open(ctx, filepath.Join(cfg.DataDir, "tiller-router.db"))
	if err != nil {
		if errors.Is(err, database.ErrDataDirUnwritable) {
			logger.Error(
				"data directory is not writable by the runtime user — the bind-mounted directory is "+
					"owned by someone other than the runtime user (a fresh rootful-Docker bind mount "+
					"is created as root). Fix ownership once, then up again.",
				"dir", cfg.DataDir,
				"uid", runUID,
				"gid", runGID,
				"fix", fmt.Sprintf("sudo chown -R %d:%d ./data", runUID, runGID),
				"alt", "set TILLER_RUN_UID and TILLER_RUN_GID in .env to the uid:gid that owns ./data",
				"see", "README 'Create the data directory'",
			)
		}
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	switch command {
	case "migrate":
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
