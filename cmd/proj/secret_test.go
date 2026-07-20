package main

import (
	"os"
	"strings"
	"testing"
)

func TestSecretStoreRoundtrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(managerDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	m, err := loadSecrets()
	if err != nil {
		t.Fatalf("load fresh: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("fresh store should be empty, got %v", m)
	}

	m["JIRA"] = "tok-123"
	m["GH"] = "ghp_secret"
	if err := saveSecrets(m); err != nil {
		t.Fatalf("save: %v", err)
	}

	// The on-disk store must be ciphertext: no plaintext value may appear.
	raw, err := os.ReadFile(secretsPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "tok-123") || strings.Contains(string(raw), "ghp_secret") {
		t.Fatal("plaintext secret leaked into the store")
	}

	got, err := loadSecrets()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got["JIRA"] != "tok-123" || got["GH"] != "ghp_secret" {
		t.Fatalf("roundtrip mismatch: %v", got)
	}

	// A different key must not decrypt the existing store.
	if err := os.Remove(secretKeyPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSecrets(); err == nil {
		t.Fatal("decrypt with a regenerated key should fail, not silently succeed")
	}
}
