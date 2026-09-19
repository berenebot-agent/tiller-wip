package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MasterKeyFileName is the auto-generated key file inside the data directory.
// It lives beside the database, not inside it, so a database dump never
// contains the key; a full data-directory archive does and is treated as
// secret (see docs/backup_restore_runbook.md).
const MasterKeyFileName = "master.key"

// KeySource names where the active master key came from. It is safe to log.
type KeySource string

const (
	SourceNone      KeySource = "none"
	SourceEnv       KeySource = "env"
	SourceEnvFile   KeySource = "env_file"
	SourceDataFile  KeySource = "data_file"
	SourceGenerated KeySource = "generated"
)

// ParseKey parses a base64-encoded master key. A leading "base64:" is
// optional; surrounding whitespace is ignored. The decoded key must be exactly
// KeySize bytes.
func ParseKey(raw string) ([]byte, error) {
	v := strings.TrimSpace(raw)
	v = strings.TrimPrefix(v, "base64:")
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, errors.New("crypto: empty master key")
	}
	key, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("crypto: master key is not valid base64: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: master key must decode to %d bytes, got %d", KeySize, len(key))
	}
	return key, nil
}

// EncodeKey renders a raw key in the canonical "base64:<value>" form.
func EncodeKey(key []byte) string {
	return "base64:" + base64.StdEncoding.EncodeToString(key)
}

// GenerateKey returns a new random 32-byte master key.
func GenerateKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// Resolve determines the active master key. Precedence is
// TILLER_MASTER_KEY_FILE > TILLER_MASTER_KEY > <dataDir>/master.key. When no
// key is available and no encrypted rows exist, a new key is generated and
// persisted. When encrypted rows exist but no key is available, it returns a
// nil key so the caller starts in the locked state rather than silently
// orphaning the existing ciphertext.
func Resolve(dataDir, envKey, envKeyFile string, hasEncryptedRows bool) (key []byte, source KeySource, err error) {
	if f := strings.TrimSpace(envKeyFile); f != "" {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, SourceNone, fmt.Errorf("read TILLER_MASTER_KEY_FILE: %w", err)
		}
		k, err := ParseKey(string(raw))
		if err != nil {
			return nil, SourceNone, fmt.Errorf("TILLER_MASTER_KEY_FILE: %w", err)
		}
		return k, SourceEnvFile, nil
	}
	if strings.TrimSpace(envKey) != "" {
		k, err := ParseKey(envKey)
		if err != nil {
			return nil, SourceNone, fmt.Errorf("TILLER_MASTER_KEY: %w", err)
		}
		return k, SourceEnv, nil
	}
	path := filepath.Join(dataDir, MasterKeyFileName)
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		k, perr := ParseKey(string(raw))
		if perr != nil {
			return nil, SourceNone, fmt.Errorf("%s: %w", path, perr)
		}
		return k, SourceDataFile, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, SourceNone, fmt.Errorf("read %s: %w", path, err)
	}
	if hasEncryptedRows {
		return nil, SourceNone, nil
	}
	k, err := GenerateKey()
	if err != nil {
		return nil, SourceNone, err
	}
	if err := WriteKeyFile(path, k); err != nil {
		return nil, SourceNone, err
	}
	return k, SourceGenerated, nil
}

// WriteKeyFile writes a raw key to path as "base64:<value>\n" with 0600
// permissions, atomically (write to a temp file in the same directory, then
// rename).
func WriteKeyFile(path string, key []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".master-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(EncodeKey(key) + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
