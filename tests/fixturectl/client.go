package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/tiller-router/tiller-router/internal/database"
)

// pinnedCatalogueFixture is a precomputed, production-compatible Argon2id client
// key for browser tests that need a client to exist without testing key
// creation itself. The secret/hash/fingerprint are test fixtures only and
// fixturectl is never shipped in the production image. The real router can
// authenticate this key through its normal Argon2id verifier because the hash
// is a genuine PHC string with production parameters (64 MiB / 3 / 4).
//
// secret + hash generated once via:
//
//	salt="fixturesalt12345" (16 bytes), argon2id(secret, salt, 3, 65536, 4, 32)
const (
	pinnedSecret      = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	pinnedHash        = "$argon2id$v=19$m=65536,t=3,p=4$Zml4dHVyZXNhbHQxMjM0NQ$YjXl7+XuQ453GdOkihgo4QTJj8p6Wc8f7paVfT4a4ek"
	pinnedFingerprint = "AAAAAAAA"
)

// fixtureOutput is the machine-readable JSON emitted by the client subcommand.
type fixtureOutput struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Secret      string `json:"secret"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
}

// fixtureSelector derives a deterministic, per-client 12-char base64url selector
// from the client name. The selector is the DB lookup key (UNIQUE), so sharing
// a single pinned selector across clients would collide; the secret+hash are
// the same pinned fixture for every client (they authenticate through the real
// Argon2id verifier regardless of selector). Determinism keeps re-runs
// idempotent and independent of clock/randomness.
func fixtureSelector(name string) string {
	sum := sha256.Sum256([]byte("fixture-selector:" + name))
	enc := base64.RawURLEncoding.EncodeToString(sum[:])[:12]
	return enc
}

// outputDest is the writer for fixturectl client output. Swappable in tests.
var outputDest io.Writer = os.Stdout

func runClient(args []string) error {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to the router SQLite database (required)")
	name := fs.String("name", "", "client name (required, must be unique)")
	clientType := fs.String("type", "catalogue", "client key type: catalogue or single")
	singleModel := fs.String("single-model", "main", "client-facing model name for single keys")
	targetKind := fs.String("target-kind", "real", "target kind for single keys: real or virtual")
	targetID := fs.String("target-id", "", "provider_model_id (real) or virtual_model_id (virtual) for single keys")
	group := fs.String("group", "default", "client key group")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" || *name == "" {
		return errors.New("--db and --name are required")
	}
	if *clientType != "catalogue" && *clientType != "single" {
		return fmt.Errorf("invalid --type %q (expected catalogue or single)", *clientType)
	}
	if *clientType == "single" && *targetID == "" {
		return errors.New("--target-id is required for single keys")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("open database %s: %w", *dbPath, err)
	}
	defer db.Close()

	id := "browser-" + *name
	now := database.Now()
	selector := fixtureSelector(*name)

	tx, err := db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM client_model_permissions WHERE client_key_id=?`, id); err != nil {
		return fmt.Errorf("clear prior permissions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM client_group_defaults WHERE client_key_id=?`, id); err != nil {
		return fmt.Errorf("clear prior group defaults: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM client_single_bindings WHERE client_key_id=?`, id); err != nil {
		return fmt.Errorf("clear prior single binding: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM client_keys WHERE id=?`, id); err != nil {
		return fmt.Errorf("clear prior client key: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM client_keys WHERE name=?`, *name); err != nil {
		return fmt.Errorf("clear prior client name: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO client_keys(id,name,description,selector,secret_hash,secret_fingerprint,enabled,logging_enabled,retention_days,key_type,key_group,created_at,rotated_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, *name, "browser fixture client", selector, pinnedHash, pinnedFingerprint,
		1, 1, 30, *clientType, *group, now, nil, now,
	)
	if err != nil {
		return fmt.Errorf("insert client_keys: %w", err)
	}

	if *clientType == "catalogue" {
		var firstModel *string
		row := db.SQL.QueryRowContext(ctx, `SELECT id FROM provider_models WHERE available=1 LIMIT 1`)
		var mid string
		if err := row.Scan(&mid); err == nil {
			firstModel = &mid
		}
		if firstModel != nil {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
				id, "real", *firstModel, 0, now, now,
			); err != nil {
				return fmt.Errorf("insert placeholder permission: %w", err)
			}
		}
	}

	if *clientType == "single" {
		var realID, virtID *string
		if *targetKind == "real" {
			realID = targetID
		} else {
			virtID = targetID
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO client_single_bindings(client_key_id,exposed_model_name,real_model_id,virtual_model_id,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
			id, *singleModel, realID, virtID, now, now,
		); err != nil {
			return fmt.Errorf("insert single binding: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	out := fixtureOutput{
		ID:          id,
		Name:        *name,
		Secret:      "sk-tr-" + selector + "." + pinnedSecret,
		Type:        *clientType,
		Fingerprint: pinnedFingerprint,
	}
	enc := json.NewEncoder(outputDest)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	return nil
}
