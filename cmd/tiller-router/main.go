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
	"strings"
	"syscall"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	cryptosecret "github.com/tiller-router/tiller-router/internal/crypto"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/privdrop"
	"github.com/tiller-router/tiller-router/internal/server"
	"github.com/tiller-router/tiller-router/internal/store"
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
	db, err := database.Open(ctx, filepath.Join(cfg.DataDir, "tiller-router.db"), database.WithBackupDir(cfg.BackupDir))
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
	case "rotate-master-key":
		return rotateMasterKey(ctx, cfg, db, logger)
	case "serve":
	default:
		return fmt.Errorf("unknown command %q (expected serve, migrate, rotate-master-key, or healthcheck)", command)
	}
	cipher, err := prepareSecrets(ctx, cfg, db, logger)
	if err != nil {
		return err
	}
	logger.Info("tiller-router starting", "version", buildversion.Version, "commit", buildversion.Commit)
	app, err := server.New(cfg, db, logger, server.WithSecretCipher(cipher))
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

// prepareSecrets resolves the master key, encrypts any remaining plaintext
// credentials in place, and returns the cipher to inject into the store. It
// returns a locked cipher when existing ciphertext cannot be decrypted, so the
// service starts in the locked state rather than exposing or overwriting
// credentials.
func prepareSecrets(ctx context.Context, cfg config.Config, db *database.DB, logger *slog.Logger) (store.SecretCipher, error) {
	hasEncrypted, err := store.HasEncryptedSecrets(ctx, db.SQL)
	if err != nil {
		return nil, fmt.Errorf("inspect stored secrets: %w", err)
	}
	key, source, err := cryptosecret.Resolve(cfg.DataDir, cfg.MasterKey, cfg.MasterKeyFile, hasEncrypted)
	if err != nil {
		return nil, err
	}
	var cipher store.SecretCipher = cryptosecret.Locked()
	if key != nil {
		c, err := cryptosecret.New(key)
		if err != nil {
			return nil, err
		}
		cipher = c
	}
	migrated, locked, err := store.MigrateSecrets(ctx, db.SQL, cipher)
	if err != nil {
		return nil, fmt.Errorf("migrate provider credentials: %w", err)
	}
	if locked {
		cipher = cryptosecret.Locked()
	}
	logger.Info("provider credential encryption ready",
		"state", store.SecretsState(cipher),
		"key_source", string(source),
		"migrated", migrated,
	)
	return cipher, nil
}

// rotateMasterKey re-encrypts every recoverable secret from the active key to a
// new key supplied via TILLER_MASTER_KEY_NEW or TILLER_MASTER_KEY_NEW_FILE.
// The new key is written to <DataDir>/master.key. It must be run with the
// service stopped.
func rotateMasterKey(ctx context.Context, cfg config.Config, db *database.DB, logger *slog.Logger) error {
	hasEncrypted, err := store.HasEncryptedSecrets(ctx, db.SQL)
	if err != nil {
		return fmt.Errorf("inspect stored secrets: %w", err)
	}
	if !hasEncrypted {
		return errors.New("no encrypted credentials found; nothing to rotate")
	}
	oldKey, source, err := cryptosecret.Resolve(cfg.DataDir, cfg.MasterKey, cfg.MasterKeyFile, true)
	if err != nil {
		return err
	}
	if oldKey == nil {
		return errors.New("current master key is unavailable; cannot rotate")
	}
	newKey, err := resolveNewMasterKey()
	if err != nil {
		return err
	}
	oldCipher, err := cryptosecret.New(oldKey)
	if err != nil {
		return err
	}
	newCipher, err := cryptosecret.New(newKey)
	if err != nil {
		return err
	}
	// When the active key is the data-directory file, stage the new key file
	// before rotating so a rotation failure can restore it. When the active key
	// comes from env/file, that source shadows the data file, so only warn.
	keyPath := filepath.Join(cfg.DataDir, cryptosecret.MasterKeyFileName)
	writeDataKey := source == cryptosecret.SourceDataFile || source == cryptosecret.SourceGenerated
	var previousKeyFile []byte
	hadPreviousKeyFile := false
	if writeDataKey {
		if raw, rerr := os.ReadFile(keyPath); rerr == nil {
			previousKeyFile, hadPreviousKeyFile = raw, true
		}
		if err := cryptosecret.WriteKeyFile(keyPath, newKey); err != nil {
			return fmt.Errorf("write new master key: %w", err)
		}
	}
	rotated, err := store.RotateSecrets(ctx, db.SQL, oldCipher, newCipher)
	if err != nil {
		if writeDataKey {
			if hadPreviousKeyFile {
				_ = os.WriteFile(keyPath, previousKeyFile, 0o600)
			} else {
				_ = os.Remove(keyPath)
			}
		}
		return fmt.Errorf("rotate credentials: %w", err)
	}
	logger.Info("master key rotated", "rotated", rotated, "previous_source", string(source))
	if writeDataKey {
		logger.Info("new master key written", "key_file", keyPath)
	}
	if strings.TrimSpace(cfg.MasterKey) != "" {
		logger.Warn("TILLER_MASTER_KEY is set and takes precedence over the data-directory key file; update it to the new key before restarting")
	}
	if strings.TrimSpace(cfg.MasterKeyFile) != "" {
		logger.Warn("TILLER_MASTER_KEY_FILE is set and takes precedence over the data-directory key file; update that secret to the new key before restarting")
	}
	return nil
}

// resolveNewMasterKey reads the rotation target key only. It never falls back
// to the active key, so a missing new key cannot silently re-encrypt with the
// old one.
func resolveNewMasterKey() ([]byte, error) {
	if f := strings.TrimSpace(os.Getenv("TILLER_MASTER_KEY_NEW_FILE")); f != "" {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read TILLER_MASTER_KEY_NEW_FILE: %w", err)
		}
		return cryptosecret.ParseKey(string(raw))
	}
	if raw := os.Getenv("TILLER_MASTER_KEY_NEW"); strings.TrimSpace(raw) != "" {
		return cryptosecret.ParseKey(raw)
	}
	return nil, errors.New("set TILLER_MASTER_KEY_NEW or TILLER_MASTER_KEY_NEW_FILE to the new master key")
}
