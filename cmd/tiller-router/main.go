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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	cryptosecret "github.com/tiller-router/tiller-router/internal/crypto"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/dnsfallback"
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
	for _, notice := range cfg.Deprecations {
		// Two channels on purpose: the structured WARN lands in the JSON log
		// stream even when the operator only grep's for it, while the stderr
		// banner stays visible at TILLER_LOG_LEVEL=error and during a
		// `docker compose up` attach where nobody reads the JSON.
		logger.Warn("deprecated configuration", "detail", notice)
		fmt.Fprintf(os.Stderr, "WARNING: %s\n", notice)
	}
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
	db, err := database.Open(ctx, filepath.Join(cfg.DataDir, "tiller-router.db"), database.WithBackupDir(cfg.BackupDir), database.WithHostedMode(cfg.Mode == config.ModeHosted))
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
	// Install before any outbound clients or background goroutines capture the
	// default resolver. Hosted transports still validate every resolved address.
	net.DefaultResolver = dnsfallback.New(cfg.DNSFallbackServers)
	logger.Info("DNS fallback configured", "servers", cfg.DNSFallbackServers)
	app, err := server.New(cfg, db, logger, server.WithSecretCipher(cipher))
	if err != nil {
		return err
	}
	runCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	app.StartBackground(runCtx)
	newHTTPServer := func(addr string, handler http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20}
	}
	httpServer := newHTTPServer(cfg.ListenAddr, app.Handler())
	servers := []*http.Server{httpServer}
	errCh := make(chan error, 2)
	go func() {
		logger.Info("tiller-router listening", "addr", cfg.ListenAddr)
		errCh <- httpServer.ListenAndServe()
	}()
	if cfg.Mode == config.ModeHosted {
		if cfg.PlatformPort == 0 {
			logger.Warn("platform console is exposed on the main listener: set TILLER_PLATFORM_PORT to serve it on a separate, network-restricted port (for example a VPN-only port)")
		} else {
			platformAddr := ":" + strconv.Itoa(cfg.PlatformPort)
			platformServer := newHTTPServer(platformAddr, app.PlatformHandler())
			servers = append(servers, platformServer)
			go func() {
				logger.Info("platform console listening", "addr", platformAddr, "note", "not served on the main listener; restrict who can reach this port")
				errCh <- platformServer.ListenAndServe()
			}()
		}
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-runCtx.Done():
		shutdownCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		// Attempt both shutdown steps even if the HTTP drain errors, so the
		// Activity writer is always flushed (StopBackground) and the database is
		// always closed. Return the first error after both are attempted.
		var shutdownErr error
		for _, srv := range servers {
			if err := srv.Shutdown(shutdownCtx); err != nil && shutdownErr == nil {
				shutdownErr = err
			}
		}
		// A long stream can use the whole HTTP drain budget. The Activity flush
		// needs its own, or it returns at once and the database closes under it.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		stopErr := app.StopBackground(stopCtx)
		closeErr := db.Close()
		if shutdownErr != nil {
			return shutdownErr
		}
		if stopErr != nil {
			return stopErr
		}
		if closeErr != nil {
			return closeErr
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
	// A retained pre-rotation key sidecar means a previous `rotate-master-key`
	// ran. It is kept by default so old ciphertext and backups stay
	// recoverable; surface it so an operator knows it is there and how to
	// recover from an interrupted rotation. Never log key material.
	sidecar := filepath.Join(cfg.DataDir, cryptosecret.PreviousMasterKeyFileName)
	if _, statErr := os.Stat(sidecar); statErr == nil {
		logger.Warn("retained pre-rotation master key found",
			"sidecar", sidecar,
			"note", "this is the key used before the last rotation; it is retained so old ciphertext and backups remain recoverable",
			"recover", fmt.Sprintf("if a rotation was interrupted, copy %s over %s and restart", sidecar, filepath.Join(cfg.DataDir, cryptosecret.MasterKeyFileName)),
			"remove", "delete it deliberately once no stored ciphertext or backup still needs the old key",
		)
	}
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
	// When the active key is the data-directory file, durably preserve the old
	// key before overwriting it, then write the new key atomically, then
	// rotate. Preserving the old key as a sidecar means a crash or kill after
	// the overwrite (but before RotateSecrets commits) no longer loses the only
	// copy of the old key: the operator can copy master.key.previous back over
	// master.key. When the active key comes from env/file, that source shadows
	// the data file, so only warn.
	keyPath := filepath.Join(cfg.DataDir, cryptosecret.MasterKeyFileName)
	writeDataKey := source == cryptosecret.SourceDataFile || source == cryptosecret.SourceGenerated
	var previousKey []byte
	hadPreviousKey := false
	if writeDataKey {
		if raw, rerr := os.ReadFile(keyPath); rerr == nil {
			parsed, perr := cryptosecret.ParseKey(string(raw))
			if perr != nil {
				return fmt.Errorf("parse current master key: %w", perr)
			}
			previousKey, hadPreviousKey = parsed, true
		}
		if hadPreviousKey {
			// Durable, atomic preservation of the old key before the new one
			// replaces it. Reuses WriteKeyFile so the 0600 + temp+rename
			// guarantees match the active key file.
			sidecar := filepath.Join(cfg.DataDir, cryptosecret.PreviousMasterKeyFileName)
			if err := cryptosecret.WriteKeyFile(sidecar, previousKey); err != nil {
				return fmt.Errorf("preserve previous master key: %w", err)
			}
		}
		if err := cryptosecret.WriteKeyFile(keyPath, newKey); err != nil {
			return fmt.Errorf("write new master key: %w", err)
		}
	}
	rotated, err := store.RotateSecrets(ctx, db.SQL, oldCipher, newCipher)
	if err != nil {
		if writeDataKey {
			if hadPreviousKey {
				// Restore in place; the sidecar also survives so a failed
				// in-process restore is still recoverable after a crash.
				if werr := cryptosecret.WriteKeyFile(keyPath, previousKey); werr != nil {
					logger.Error("failed to restore previous master key after rotation error; recover from the sidecar", "sidecar", filepath.Join(cfg.DataDir, cryptosecret.PreviousMasterKeyFileName), "error", werr.Error())
				}
			} else {
				_ = os.Remove(keyPath)
			}
		}
		return fmt.Errorf("rotate credentials: %w", err)
	}
	logger.Info("master key rotated", "rotated", rotated, "previous_source", string(source))
	if writeDataKey {
		logger.Info("new master key written", "key_file", keyPath)
		logger.Info("previous master key retained for recovery", "sidecar", filepath.Join(cfg.DataDir, cryptosecret.PreviousMasterKeyFileName), "note", "kept by default so old ciphertext and backups remain recoverable; remove it deliberately once it is no longer needed")
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
