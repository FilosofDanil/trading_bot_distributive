package config

import (
	"os"
	"path/filepath"
	"testing"
)

func clearEnv(t *testing.T) {
	for _, k := range []string{"TELEGRAM_BOT_TOKEN", "ALLOWED_USER_IDS", "ALLOWED_USERNAMES", "BINANCE_API_KEY", "BINANCE_API_SECRET"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestEnvFileAndAllowlists(t *testing.T) {
	clearEnv(t)
	env := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(env, []byte("TELEGRAM_BOT_TOKEN=x\nBINANCE_API_KEY=abc123\nBINANCE_API_SECRET=def456\n"+
		"ALLOWED_USER_IDS=123456789, 42\nALLOWED_USERNAMES=@Dwarf_Strong, other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if c.PresetKey != "abc123" || c.PresetSecret != "def456" {
		t.Fatalf("keys: %q %q", c.PresetKey, c.PresetSecret)
	}
	if !c.AllowedUserIDs[123456789] || !c.AllowedUserIDs[42] || len(c.AllowedUserIDs) != 2 {
		t.Fatalf("user ids: %v", c.AllowedUserIDs)
	}
	if !c.AllowedUsernames["dwarf_strong"] || !c.AllowedUsernames["other"] || len(c.AllowedUsernames) != 2 {
		t.Fatalf("usernames: %v", c.AllowedUsernames)
	}
}

func TestMissingEnvFileUsesProcessEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "x")
	c, err := Load(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatal(err)
	}
	if c.BotToken != "x" || c.HasAllowlist() {
		t.Fatalf("token %q, allowlist %v", c.BotToken, c.HasAllowlist())
	}
}

func TestPresetKeysRequireAllowlist(t *testing.T) {
	clearEnv(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "x")
	t.Setenv("BINANCE_API_KEY", "k")
	t.Setenv("BINANCE_API_SECRET", "s")
	if _, err := Load(""); err == nil {
		t.Fatal("preset keys without an allowlist must be rejected")
	}
	t.Setenv("ALLOWED_USER_IDS", "123")
	if _, err := Load(""); err != nil {
		t.Fatal(err)
	}
}

func TestBadUserID(t *testing.T) {
	clearEnv(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "x")
	t.Setenv("ALLOWED_USER_IDS", "123,@me")
	if _, err := Load(""); err == nil {
		t.Fatal("non-numeric ALLOWED_USER_IDS must be rejected")
	}
}
