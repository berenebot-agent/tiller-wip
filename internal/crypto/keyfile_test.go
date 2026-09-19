package crypto

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseKey(t *testing.T) {
	key := testKey(7)
	encoded := EncodeKey(key)
	got, err := ParseKey(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(key) {
		t.Fatal("ParseKey did not round-trip the key")
	}
	// Bare base64 (no prefix) and surrounding whitespace are accepted.
	raw := encoded[len("base64:"):]
	if _, err := ParseKey("  " + raw + "\n"); err != nil {
		t.Fatalf("bare base64 rejected: %v", err)
	}
	for _, bad := range []string{"", "base64:notbase64", "base64:" + raw[:10]} {
		if _, err := ParseKey(bad); err == nil {
			t.Fatalf("ParseKey(%q) succeeded, want error", bad)
		}
	}
}

func TestResolveGeneratesAndReloads(t *testing.T) {
	dir := t.TempDir()
	key, source, err := Resolve(dir, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if source != SourceGenerated {
		t.Fatalf("source = %q, want generated", source)
	}
	info, err := os.Stat(filepath.Join(dir, MasterKeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %o, want 600", perm)
	}
	reloaded, source, err := Resolve(dir, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if source != SourceDataFile {
		t.Fatalf("source = %q, want data_file", source)
	}
	if string(reloaded) != string(key) {
		t.Fatal("reloaded key differs from generated key")
	}
}

func TestResolvePrecedenceAndLocked(t *testing.T) {
	dir := t.TempDir()
	envKey := testKey(3)
	fileKey := testKey(4)
	keyFile := filepath.Join(dir, "secret.key")
	if err := os.WriteFile(keyFile, []byte(EncodeKey(fileKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	// File source takes precedence over the inline env value.
	got, source, err := Resolve(dir, EncodeKey(envKey), keyFile, false)
	if err != nil {
		t.Fatal(err)
	}
	if source != SourceEnvFile || string(got) != string(fileKey) {
		t.Fatalf("file precedence failed: source=%q", source)
	}
	// Inline env is used when no file is supplied.
	got, source, err = Resolve(dir, EncodeKey(envKey), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if source != SourceEnv || string(got) != string(envKey) {
		t.Fatalf("env source failed: source=%q", source)
	}
	// Encrypted rows with no key must yield a nil key (locked), not a new key.
	empty := t.TempDir()
	got, source, err = Resolve(empty, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil || source != SourceNone {
		t.Fatalf("locked resolve = (%v, %q), want (nil, none)", got, source)
	}
	if _, err := os.Stat(filepath.Join(empty, MasterKeyFileName)); !os.IsNotExist(err) {
		t.Fatal("Resolve minted a key while encrypted rows existed")
	}
}
